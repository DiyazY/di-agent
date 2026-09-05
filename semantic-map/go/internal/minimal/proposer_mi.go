package minimal

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
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

	mu      sync.Mutex
	buffers map[string]*stats.PairWindow // key: fromID + "→" + toID
	// lastPair remembers, per buffer key, the timestamps of the last pair folded, so
	// a reading takes part in at most one pair per counterpart: without it each tick
	// at a shared cadence folded (x_i, y_{i-1}) on x's arrival and (x_i, y_i) on
	// y's, counting every reading twice.
	lastPair   map[string][2]int64
	candidates map[string]*types.CandidateEdge // key: CandidateID — holds the LATEST status
	order      []string                        // insertion order of CandidateIDs, for stable history iteration (and, since the cap must be deterministic, stable cap eviction)

	latestConstructs map[string]float64     // explicit construct path: id → last value
	latestProps      map[string]latestValue // property path: id → last value, time, scope

	threshold  float64
	minPairs   int
	bufSize    int
	pairWindow time.Duration
	maxPending int
	logf       func(format string, args ...any)
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
		lastPair:         make(map[string][2]int64),
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
		xAt, yAt := at.UnixNano(), other.at.UnixNano()
		if !me.scoped {
			from, to, x, y = otherID, id, other.value, value
			xAt, yAt = other.at.UnixNano(), at.UnixNano()
		}
		key := from + "→" + to
		if lp, used := p.lastPair[key]; used && (lp[0] == xAt || lp[1] == yAt) {
			continue // that reading already took part in a pair with this counterpart
		}
		identity := from + "|" + to + "|" + strconv.FormatInt(at.Unix(), 10) + "|" + strconv.FormatInt(other.at.Unix(), 10)
		if err := p.observeLocked(from, to, x, y, identity); err != nil && firstErr == nil {
			firstErr = err
		}
		p.lastPair[key] = [2]int64{xAt, yAt}
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
			delete(p.lastPair, key)
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
		case types.Rejected:
			return nil // permanent suppression within the session
		case types.Deferred:
			if existing.Reason != deferredByCap {
				return nil // an operator's deferral is not overturned by evidence
			}
			existing.Direction, existing.MIScore = direction, math.Abs(r)
			existing.PValue, existing.NObservations = stats.FisherPValue(r, buf.Len()), buf.Len()
			if p.outranksPendingLocked(existing) {
				existing.Status, existing.Reason = types.Pending, ""
				p.log("proposer: %s re-enters pending with |r|·n=%.1f; the cap defers the weakest instead", candID, score(existing))
				p.enforceCapLocked()
			}
			return nil
		case types.Pending:
			existing.Direction = direction
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
// SetLogger routes the proposer's own log lines (cap deferrals and re-entries)
// somewhere other than the standard logger; tests capture them.
func (p *MICorrelationProposer) SetLogger(f func(format string, args ...any)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.logf = f
}

func (p *MICorrelationProposer) log(format string, args ...any) {
	if p.logf != nil {
		p.logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// Reasons a Deferred candidate carries, so history can tell the proposer's choice
// from an operator's.
const (
	deferredByCap      = "cap"
	deferredByOperator = "operator"
)

func score(c *types.CandidateEdge) float64 { return c.MIScore * float64(c.NObservations) }

// weaker is the total order the cap uses: lower |r|·n, then the lexicographically
// larger id among bit-equal scores.
func weaker(a, b *types.CandidateEdge) bool {
	sa, sb := score(a), score(b)
	return sa < sb || (sa == sb && a.CandidateID > b.CandidateID)
}

func (p *MICorrelationProposer) pendingLocked() []*types.CandidateEdge {
	var pending []*types.CandidateEdge
	for _, cid := range p.order {
		c := p.candidates[cid]
		if c != nil && c.Status == types.Pending {
			pending = append(pending, c)
		}
	}
	return pending
}

// outranksPendingLocked reports whether a cap-deferred candidate would survive the
// cap now: there is room, or it is stronger than the weakest pending candidate.
func (p *MICorrelationProposer) outranksPendingLocked(c *types.CandidateEdge) bool {
	pending := p.pendingLocked()
	if len(pending) < p.maxPending {
		return true
	}
	weakest := pending[0]
	for _, w := range pending[1:] {
		if weaker(w, weakest) {
			weakest = w
		}
	}
	return weaker(weakest, c)
}

func (p *MICorrelationProposer) enforceCapLocked() {
	pending := p.pendingLocked()
	for len(pending) > p.maxPending {
		weakest := 0
		for i, c := range pending {
			if weaker(c, pending[weakest]) {
				weakest = i
			}
		}
		w := pending[weakest]
		w.Status, w.Reason = types.Deferred, deferredByCap
		p.log("proposer: %s deferred by the pending cap (%d): |r|·n=%.1f is the weakest; it re-enters if it outranks a pending candidate",
			w.CandidateID, p.maxPending, score(w))
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
// The candidate is marked Confirmed here, before the facade applies the proposition.
// If applying fails the facade calls Reopen, so the candidate returns to Pending rather
// than standing in history as confirmed with nothing behind it.
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

// Reopen returns a Confirmed candidate to Pending. The facade calls it when the
// relationship a confirmation stands for could not be declared, so the candidate can
// be retried or rejected instead of standing in history as confirmed with nothing
// behind it.
func (p *MICorrelationProposer) Reopen(candidateID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.candidates[candidateID]
	if !ok {
		return fmt.Errorf("candidate %q not found", candidateID)
	}
	if c.Status != types.Confirmed {
		return fmt.Errorf("candidate %q is not Confirmed (status=%v)", candidateID, c.Status)
	}
	c.Status, c.Reason = types.Pending, ""
	return nil
}

// Reject marks a candidate as permanently suppressed for this session. The candidate
// stays in the map so subsequent observations of the same pair are short-circuited,
// and its history entry reads Rejected. A Confirmed candidate cannot be rejected: the
// relationship it became is in the map, and retiring that is the operator's move.
func (p *MICorrelationProposer) Reject(candidateID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.candidates[candidateID]
	if !ok {
		return fmt.Errorf("candidate %q not found", candidateID)
	}
	if c.Status == types.Confirmed {
		return fmt.Errorf("candidate %q is Confirmed: the relationship it became is in the map; retire that instead", candidateID)
	}
	c.Status, c.Reason = types.Rejected, ""
	return nil
}

// Defer marks a candidate as Deferred by an operator — out of GetCandidates, still in
// history with Reason "operator", and not overturned by later evidence. A cap deferral
// (Reason "cap") is the proposer's own, provisional choice and re-enters when it
// outranks a pending candidate; an operator may Defer a cap-deferred candidate to
// make it stick. Confirmed and Rejected candidates cannot be deferred.
func (p *MICorrelationProposer) Defer(candidateID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.candidates[candidateID]
	if !ok {
		return fmt.Errorf("candidate %q not found", candidateID)
	}
	if c.Status == types.Confirmed || c.Status == types.Rejected {
		return fmt.Errorf("candidate %q is %v and cannot be deferred", candidateID, c.Status)
	}
	c.Status, c.Reason = types.Deferred, deferredByOperator
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
