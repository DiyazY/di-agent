package minimal

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DiyazY/di-agent/pkg/contracts"
	"github.com/DiyazY/di-agent/pkg/stats"
	"github.com/DiyazY/di-agent/pkg/types"
)

// MICorrelationProposer is a production-grade ProposerContract implementation.
//
// It maintains a fixed-size ring buffer of (valueA, valueB) observations per
// pair. Once `minPairs` samples accumulate, it computes the Pearson
// correlation coefficient over the window. If |r| > threshold AND no existing
// relationship between the pair exists (per lookup) AND the candidate was not
// previously rejected or deferred, it emits a CandidateEdge.
//
// Pearson correlation stands in for mutual information here — it captures
// linear dependence cheaply and deterministically. The name "MI" is kept for
// continuity with the literature on automatic graph extension; a richer
// edge-standard or cloud-full profile may swap in true mutual-information
// estimation without changing the interface.
//
// p-values are computed via the Fisher z-transform (two-tailed).
//
// ObserveProperty, the property path with the scope rule, is the entry point
// from ingestion: a scoped property (non-empty subject) pairs only with
// unscoped ones, direction scoped -> unscoped, inside a time tolerance.
// ObserveConstruct is the explicit construct-pairing path kept for callers
// that already know the pair and supply no timestamps; Observe is the
// lowest-level explicit path either builds on.
type MICorrelationProposer struct {
	lookup contracts.RelationshipLookup

	mu         sync.Mutex
	buffers    map[string]*stats.PairWindow    // key: fromID + "→" + toID
	candidates map[string]*types.CandidateEdge // key: CandidateID — holds the LATEST status
	order      []string                        // insertion order of CandidateIDs, for stable history iteration (and, since the cap must be deterministic, stable cap eviction)

	latestConstructs map[string]float64     // explicit construct path: id → last value
	latestProps      map[string]latestValue // property path: id → last value, time, scope

	threshold  float64
	minPairs   int
	bufSize    int
	pairWindow time.Duration
	maxPending int
	seq        uint64 // identity for pairs that carry no timestamps
}

type latestValue struct {
	value  float64
	at     time.Time
	scoped bool
}

// NewMICorrelationProposer builds the proposer. lookup answers "already known";
// pairWindow is the time tolerance for the property path (0 = 15s).
func NewMICorrelationProposer(lookup contracts.RelationshipLookup, threshold float64,
	minPairs, bufSize int, pairWindow time.Duration) *MICorrelationProposer {
	if minPairs < 3 {
		minPairs = 3
	}
	if bufSize < minPairs {
		bufSize = minPairs
	}
	if pairWindow <= 0 {
		pairWindow = 15 * time.Second
	}
	return &MICorrelationProposer{
		lookup:           lookup,
		buffers:          make(map[string]*stats.PairWindow),
		candidates:       make(map[string]*types.CandidateEdge),
		latestConstructs: make(map[string]float64),
		latestProps:      make(map[string]latestValue),
		threshold:        threshold,
		minPairs:         minPairs,
		bufSize:          bufSize,
		pairWindow:       pairWindow,
		maxPending:       64,
	}
}

// SetMaxPending bounds the number of Pending candidates; the excess is deferred,
// weakest (|r|·n) first, and stays visible in history.
func (p *MICorrelationProposer) SetMaxPending(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n > 0 {
		p.maxPending = n
	}
}

// Observe is the explicit path: the caller supplies the pair directly. There is no
// scope rule and no time tolerance; identity for deduplication comes from the
// sequence counter, not from timestamps.
func (p *MICorrelationProposer) Observe(fromID, toID string, valueA, valueB float64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	return p.observeLocked(fromID, toID, valueA, valueB, strconv.FormatUint(p.seq, 10))
}

// ObserveConstruct is the explicit construct-pairing path: it pairs the value with
// the latest value of every other construct fed this way, in lexicographic order and
// without a time tolerance, because its callers supply no timestamps.
func (p *MICorrelationProposer) ObserveConstruct(constructID string, value float64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.latestConstructs[constructID] = value
	var firstErr error
	for otherID, otherVal := range p.latestConstructs {
		if otherID == constructID {
			continue
		}
		fromID, toID, valA, valB := constructID, otherID, value, otherVal
		if otherID < constructID {
			fromID, toID, valA, valB = otherID, constructID, otherVal, value
		}
		p.seq++
		if err := p.observeLocked(fromID, toID, valA, valB, strconv.FormatUint(p.seq, 10)); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ObserveProperty is the entry point from ingestion. The scope rule: a scoped
// property (non-empty subject) pairs only with unscoped ones, never with another
// scoped one, and the direction is scoped -> unscoped — a subject is a component of
// the node, so the modelling assumption is component influences whole. Readings
// further apart than pairWindow do not form a pair.
func (p *MICorrelationProposer) ObserveProperty(id, subject string, value float64, at time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if at.IsZero() {
		at = time.Now()
	}
	me := latestValue{value: value, at: at, scoped: subject != ""}
	p.latestProps[id] = me
	var firstErr error
	for otherID, other := range p.latestProps {
		if otherID == id || other.scoped == me.scoped {
			continue
		}
		gap := at.Sub(other.at)
		if gap < 0 {
			gap = -gap
		}
		if gap > p.pairWindow {
			continue
		}
		from, to, x, y := id, otherID, value, other.value
		if !me.scoped {
			from, to, x, y = otherID, id, other.value, value
		}
		identity := from + "|" + to + "|" + strconv.FormatInt(at.Unix(), 10) + "|" + strconv.FormatInt(other.at.Unix(), 10)
		if err := p.observeLocked(from, to, x, y, identity); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Forget drops the buffers, latest values and candidates involving a property.
func (p *MICorrelationProposer) Forget(propertyID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.latestProps, propertyID)
	delete(p.latestConstructs, propertyID)
	for key := range p.buffers {
		from, to, _ := strings.Cut(key, "→")
		if from == propertyID || to == propertyID {
			delete(p.buffers, key)
		}
	}
	kept := p.order[:0]
	for _, cid := range p.order {
		c := p.candidates[cid]
		if c != nil && (c.FromID == propertyID || c.ToID == propertyID) {
			delete(p.candidates, cid)
			continue
		}
		kept = append(kept, cid)
	}
	p.order = kept
	return nil
}

func (p *MICorrelationProposer) observeLocked(fromID, toID string, valueA, valueB float64, identity string) error {
	key := fromID + "→" + toID
	buf, ok := p.buffers[key]
	if !ok {
		buf = stats.NewPairWindow()
		p.buffers[key] = buf
	}
	if !buf.Fold(identity, valueA, valueB, p.bufSize) {
		return nil
	}
	if buf.Len() < p.minPairs {
		return nil
	}
	r, ok := buf.Pearson()
	if !ok || math.Abs(r) < p.threshold {
		return nil
	}
	direction, sign := types.Positive, 1
	if r < 0 {
		direction, sign = types.Negative, -1
	}
	if p.lookup != nil && p.lookup.Covered(fromID, toID, sign) {
		return nil
	}
	candID := fromID + "->" + toID
	if existing, ok := p.candidates[candID]; ok {
		switch existing.Status {
		case types.Confirmed:
			return nil // a confirmed pair is settled; never re-emitted
		case types.Rejected, types.Deferred:
			return nil // permanent suppression within the session; deferred stays out
		case types.Pending:
			existing.MIScore = math.Abs(r)
			existing.PValue = stats.FisherPValue(r, buf.Len())
			existing.NObservations = buf.Len()
			return nil
		}
	}
	p.candidates[candID] = &types.CandidateEdge{
		CandidateID: candID, FromID: fromID, ToID: toID, Direction: direction,
		MIScore: math.Abs(r), PValue: stats.FisherPValue(r, buf.Len()),
		NObservations: buf.Len(), Status: types.Pending,
	}
	p.order = append(p.order, candID)
	p.enforceCapLocked()
	return nil
}

// enforceCapLocked defers the weakest pending candidates until at most maxPending
// remain. Deferred rather than deleted: an operator can see in history that the
// proposer had more to say than the cap allowed.
//
// Candidates are collected by walking p.order (insertion order), not by ranging
// over the p.candidates map — Go's map iteration order is randomized, and ranging
// over it would make the choice of "weakest" nondeterministic across runs. The
// comparison itself is made total by tie-breaking on CandidateID: among equal
// |r|·n scores the lexicographically larger id is treated as weaker. Equal means
// bit-equal, which series with identical readings produce; affine-related series
// do not — Pearson is affine-invariant in exact arithmetic, but in float64 their
// |r| land one ULP apart and which rounds lowest depends on the platform's
// arithmetic (fused multiply-add on arm64, not on amd64 v1). The choice is
// therefore reproducible run to run on one machine, not across architectures. A
// deferral is session-permanent, so an arbitrary choice here would be a silent,
// unreproducible loss.
func (p *MICorrelationProposer) enforceCapLocked() {
	var pending []*types.CandidateEdge
	for _, cid := range p.order {
		c := p.candidates[cid]
		if c != nil && c.Status == types.Pending {
			pending = append(pending, c)
		}
	}
	for len(pending) > p.maxPending {
		weakest := 0
		for i, c := range pending {
			wc := pending[weakest]
			score, weakestScore := c.MIScore*float64(c.NObservations), wc.MIScore*float64(wc.NObservations)
			if score < weakestScore || (score == weakestScore && c.CandidateID > wc.CandidateID) {
				weakest = i
			}
		}
		pending[weakest].Status = types.Deferred
		pending = append(pending[:weakest], pending[weakest+1:]...)
	}
}

// GetCandidates returns Pending candidates sorted by CandidateID for stable
// output.
func (p *MICorrelationProposer) GetCandidates() ([]*types.CandidateEdge, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*types.CandidateEdge, 0, len(p.candidates))
	for _, c := range p.candidates {
		if c.Status == types.Pending {
			out = append(out, copyCandidate(c))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CandidateID < out[j].CandidateID })
	return out, nil
}

// Confirm marks a Pending candidate accepted and returns the proposition it represents.
// The synthesized PropositionID is
//
//	"P-" + hex(sha256(CandidateID))[:8]
//
// so the same candidate always lands the same proposition ID across replays.
//
// It adds nothing itself. It used to call AddValidatedProposition on the ontology, which
// meant a confirmed candidate reached the declaration layer and not the state model — so
// it appeared in Propositions() and in no traversal, influencing no answer. That is the
// exact failure propose-then-confirm exists to prevent, one layer over. The facade adds
// the returned proposition through its own path, which declares both halves.
//
// The candidate is only marked Confirmed on the way out, so a caller that fails to apply
// the proposition can retry: an unapplied confirmation must not leave the candidate in a
// state where it can never be confirmed again.
func (p *MICorrelationProposer) Confirm(candidateID string) (*types.Proposition, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.candidates[candidateID]
	if !ok {
		return nil, fmt.Errorf("candidate %q not found", candidateID)
	}
	if c.Status != types.Pending {
		return nil, fmt.Errorf("candidate %q is not Pending (status=%v)", candidateID, c.Status)
	}

	prop := &types.Proposition{
		PropositionID:   "P-" + synthesizePropSuffix(candidateID),
		FromConstruct:   c.FromID,
		ToConstruct:     c.ToID,
		Direction:       c.Direction,
		PriorStrength:   c.MIScore,
		Description:     fmt.Sprintf("Auto-proposed by MICorrelationProposer (|r|=%.3f, n=%d)", c.MIScore, c.NObservations),
		EvidenceSources: []string{"proposer-mi"},
	}
	c.Status = types.Confirmed
	return prop, nil
}

// Reject marks a candidate as permanently suppressed for this session.
// The candidate stays in the map so subsequent Observe calls on the same
// pair are short-circuited; a history entry is appended.
func (p *MICorrelationProposer) Reject(candidateID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.candidates[candidateID]
	if !ok {
		return fmt.Errorf("candidate %q not found", candidateID)
	}
	c.Status = types.Rejected
	return nil
}

// Defer marks a candidate as Deferred — it moves out of GetCandidates but
// remains in the candidates map. In this v1 implementation Defer behaves
// like a weaker form of Reject: re-Observe on the same pair will not
// re-emit while the deferred entry is present. A richer profile may
// re-promote deferred candidates after a fresh evidence cycle; not yet.
func (p *MICorrelationProposer) Defer(candidateID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.candidates[candidateID]
	if !ok {
		return fmt.Errorf("candidate %q not found", candidateID)
	}
	c.Status = types.Deferred
	return nil
}

// GetHistory returns one entry per candidate the proposer has ever emitted,
// reflecting the candidate's current status (Pending / Confirmed / Rejected /
// Deferred). Order matches insertion (first-seen first). This is the audit
// surface — every candidate that has existed appears here exactly once with
// its lifecycle endpoint.
func (p *MICorrelationProposer) GetHistory() ([]*types.CandidateEdge, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*types.CandidateEdge, 0, len(p.order))
	for _, id := range p.order {
		if c, ok := p.candidates[id]; ok {
			out = append(out, copyCandidate(c))
		}
	}
	return out, nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

func synthesizePropSuffix(candidateID string) string {
	h := sha256.Sum256([]byte(candidateID))
	return hex.EncodeToString(h[:4]) // 8 hex chars
}

func copyCandidate(c *types.CandidateEdge) *types.CandidateEdge {
	cp := *c
	return &cp
}

// LookupOntology adapts the declaration layer to RelationshipLookup: a pair is covered
// when a non-deprecated proposition runs from -> to with the same direction. Used by
// callers that pair constructs explicitly; the daemon passes the state map instead.
func LookupOntology(o contracts.OntologyContract) contracts.RelationshipLookup {
	return ontologyLookup{o: o}
}

type ontologyLookup struct{ o contracts.OntologyContract }

func (l ontologyLookup) Covered(from, to string, sign int) bool {
	props, err := l.o.Propositions()
	if err != nil {
		return false
	}
	dir := types.Positive
	if sign < 0 {
		dir = types.Negative
	}
	for _, prop := range props {
		if !prop.Deprecated && prop.FromConstruct == from && prop.ToConstruct == to && prop.Direction == dir {
			return true
		}
	}
	return false
}
