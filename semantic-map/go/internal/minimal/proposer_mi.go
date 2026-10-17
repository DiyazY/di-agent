package minimal

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"math"
	"slices"
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
// correlation coefficient over the window. If |r| >= threshold AND the pair is
// not already covered by a relationship with that sign (per lookup) AND the
// candidate is not settled — confirmed, rejected, or deferred by an operator —
// it emits a CandidateEdge; a cap-deferred candidate is refreshed and re-enters
// when it outranks a pending one.
//
// Pearson correlation stands in for mutual information here — it captures
// linear dependence cheaply and deterministically. The name "MI" is kept for
// continuity with the literature on automatic graph extension; a richer
// edge-standard or cloud-full profile may swap in true mutual-information
// estimation without changing the interface.
//
// p-values are computed via the Fisher z-transform (two-tailed).
//
// ObserveProperty, the property path, is the entry point from ingestion. Inside a
// time tolerance any two properties may pair: a scoped property (non-empty subject)
// with an unscoped one directed scoped -> unscoped, and two unscoped or two scoped
// properties as an undirected pair. Properties of one chart and one subject that stay
// at |r| >= groupR are an arithmetic group, and only its representative pairs outside
// it (see regroupLocked). How many pairs are tested at once is the pair budget
// (SetMaxPairs); see rationLocked for how it is shared out. A candidate's p-value must
// also clear a bar corrected for how many pairs are tested (SetFDR, barLocked).
// Observe is the explicit path for a caller that already knows the pair and
// supplies both values; ObserveConstruct is the explicit construct path that
// pairs each construct value with the latest of every other construct,
// lexicographically. Ingestion uses neither.
type MICorrelationProposer struct {
	lookup contracts.RelationshipLookup

	mu      sync.Mutex
	buffers map[string]*stats.PairWindow // key: fromID + "→" + toID
	// lastPair remembers, per buffer key, the timestamps of the last pair folded, so
	// a reading takes part in at most one pair per counterpart: without it each tick
	// at a shared cadence folded (x_i, y_{i-1}) on x's arrival and (x_i, y_i) on
	// y's, counting every reading twice. When the later arrival is the closer
	// partner it replaces the pair just folded rather than being refused: after a
	// missed reading the first arrival of a tick folds the other's previous value,
	// and refusing the same-tick reading would keep the window one interval out of
	// step from then on.
	lastPair   map[string][2]int64
	candidates map[string]*types.CandidateEdge // key: CandidateID — holds the LATEST status
	order      []string                        // insertion order of CandidateIDs, for stable history iteration (and, since the cap must be deterministic, stable cap eviction)

	latestConstructs map[string]float64     // explicit construct path: id → last value
	latestProps      map[string]latestValue // property path: id → last value, time, scope
	recent           map[string]*valueRing  // property path: id → its last bufSize values, for its spread

	// The pair budget. slots holds every pair the property path tests, keyed like
	// buffers; partners indexes them by property, so a reading folds into its own
	// pairs without walking every other property.
	maxPairs  int // 0 = no limit
	slots     map[string]*pairSlot
	partners  map[string]map[string]struct{}
	displaced uint64
	lastPass  time.Time // reading time of the last rationing pass
	latestAt  time.Time // the latest reading time seen

	// Arithmetic groups. links holds, by pair key, the tested pairs of one chart and
	// one subject at |r| >= groupR; repOf maps each member of a group to its
	// representative, and is recomputed when a link forms or breaks.
	links map[string]struct{}
	repOf map[string]string

	// The corrected bar. pvals holds every pair's p-value at its last fold with enough
	// pairs to give one; bar is the Benjamini–Hochberg bar over them at rate fdr,
	// recomputed once per round of folds (folds counts them, barFolds marks the last).
	fdr             float64
	pvals           map[string]float64
	bar             float64
	barValid        bool
	folds, barFolds uint64
	barTests        int // the m the bar was last computed over

	threshold  float64
	minSpread  float64 // the variation gate: least spread, as a share of the declared range
	minPairs   int
	bufSize    int
	pairWindow time.Duration
	maxPending int
	logf       func(format string, args ...any)
	seq        uint64 // identity for pairs that carry no timestamps
}

// pairSlot is one tested pair of the property path. score is |r|·n at the last fold
// that had enough pairs to compute it, which is what displacement ranks by.
type pairSlot struct {
	from, to string
	class    string
	n        int
	r        float64
}

// valueRing is a property's recent values, kept to measure how far it moves.
type valueRing struct {
	vs   []float64
	next int
}

func (v *valueRing) push(x float64, capacity int) {
	if len(v.vs) < capacity {
		v.vs = append(v.vs, x)
		return
	}
	v.vs[v.next] = x
	v.next = (v.next + 1) % capacity
}

type latestValue struct {
	value   float64
	at      time.Time
	subject string
	chart   string
	partOf  string      // the node-level property the latest reading said this is a part of
	rng     *[2]float64 // declared range; nil when the producer declared none
}

func (v latestValue) scoped() bool { return v.subject != "" }

// pairMeta is what the property path knows about a pair beyond its values. The
// explicit paths pass the zero value: a caller-supplied pair is directed, and its
// caller has declared no ranges to check movement against.
type pairMeta struct {
	property   bool // formed by ObserveProperty: the variation gate and caveats apply
	undirected bool
	kind       string
	replace    bool // overwrite the last folded pair: this one shares a reading with it and is closer in time
	sameChart  bool // both ends are of one chart and one subject: the pair can link an arithmetic group
	xRange     *[2]float64
	yRange     *[2]float64
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
		recent:           make(map[string]*valueRing),
		slots:            make(map[string]*pairSlot),
		partners:         make(map[string]map[string]struct{}),
		links:            make(map[string]struct{}),
		repOf:            make(map[string]string),
		pvals:            make(map[string]float64),
		fdr:              DefaultFDR,
		threshold:        threshold,
		minSpread:        DefaultMinSpread,
		minPairs:         minPairs,
		bufSize:          bufSize,
		pairWindow:       pairWindow,
		maxPending:       64,
	}
}

// DefaultMinSpread is the variation gate's default: over the window, each end of a
// pair must have moved by at least 1% of its declared range.
const DefaultMinSpread = 0.01

// SetMinSpread sets the variation gate: a candidate is emitted only when each end
// with a declared range moved, over the window, by at least this share of that
// range. 0 disables the gate; a negative value is ignored.
func (p *MICorrelationProposer) SetMinSpread(share float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if share >= 0 {
		p.minSpread = share
	}
}

// DefaultFDR is the false-discovery rate the candidate bar holds to by default: of
// the candidates the bar lets through, about 5% are expected to be chance.
const DefaultFDR = 0.05

// SetFDR sets the false-discovery rate of the candidate bar. A pair becomes a
// candidate only when its p-value is at or under the Benjamini–Hochberg bar at this
// rate over every pair being tested, so the more pairs are tested the more evidence a
// candidate needs. 0 disables the correction; a value outside [0, 1) is ignored.
func (p *MICorrelationProposer) SetFDR(q float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if q >= 0 && q < 1 {
		p.fdr = q
		p.barValid = false
	}
}

// groupR is the |r| at which two properties of one chart and one subject are taken to
// be arithmetic of each other: sums, complements or smoothings, which correlate
// near-perfectly by construction.
const groupR = 0.99

// SetMaxPairs sets the pair budget: how many pairs the property path tests at once,
// across every pair class. 0 or less removes the limit, and every eligible pair is
// tested. Lowering it below the pairs already tested takes effect as pairs are
// displaced or settle; nothing is dropped here.
func (p *MICorrelationProposer) SetMaxPairs(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n < 0 {
		n = 0
	}
	p.maxPairs = n
}

// matureAt is how many pairs a tested pair holds before it can be displaced: enough
// that a low |r| is evidence rather than a short window.
const matureAt = 30

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
	return p.observeLocked(fromID, toID, valueA, valueB, strconv.FormatUint(p.seq, 10), pairMeta{})
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
		if err := p.observeLocked(fromID, toID, valA, valB, strconv.FormatUint(p.seq, 10), pairMeta{}); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ObserveProperty is the entry point from ingestion. The pairing rule:
//
//   - a scoped property (non-empty subject) pairs with each unscoped one, directed
//     scoped -> unscoped: a subject is a component of the node, so the modelling
//     assumption is component influences whole;
//   - two unscoped properties pair as an undirected candidate, ordered
//     lexicographically, because nothing says which node-level quantity drives the
//     other and the operator chooses on confirm;
//   - two scoped properties pair the same way, undirected. Two subjects' readings of
//     one metric are marked same-metric: they often share a driver.
//
// Readings further apart than pairWindow do not form a pair. A reading no newer than
// its property's last is a repeat or arrives out of order, and is not paired: that
// one timestamp per property is what keeps a replayed batch from folding twice, so
// a tested pair carries no set of identities. A reading folds into
// the pairs already tested for its property. While the budget has room it also
// starts testing every pair it can form; once the budget is full, new pairs wait for
// the rationing pass, which runs at most once per pair window of reading time.
func (p *MICorrelationProposer) ObserveProperty(r types.PropertyReading) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	id, at := r.ID, r.At
	if at.IsZero() {
		at = time.Now()
	}
	if prev, ok := p.latestProps[id]; ok && !at.After(prev.at) {
		return nil
	}
	chart := r.Chart
	if chart == "" {
		// A producer that names no chart does not unname it: with two collectors
		// feeding one property, taking each reading's chart as given made the property
		// leave and rejoin its arithmetic group every tick.
		chart = p.latestProps[id].chart
	}
	// Unlike the chart, a reading without part_of withdraws it: a producer states
	// part_of on every reading it applies to, so its absence is how a wrong
	// declaration, or a device that stopped counting into the node, is corrected.
	p.latestProps[id] = latestValue{value: r.Value, at: at, subject: r.Subject, chart: chart, partOf: r.PartOf, rng: r.Range}
	ring := p.recent[id]
	if ring == nil {
		ring = &valueRing{}
		p.recent[id] = ring
	}
	ring.push(r.Value, p.bufSize)
	if at.After(p.latestAt) {
		p.latestAt = at
	}
	for otherID := range p.partners[id] {
		p.foldLocked(id, otherID)
	}
	if p.roomLocked() {
		p.admitAllLocked(id, at)
	} else if at.Sub(p.lastPass) >= p.pairWindow {
		p.lastPass = at
		p.rationLocked(at)
	}
	return nil
}

func (p *MICorrelationProposer) roomLocked() bool {
	return p.maxPairs <= 0 || len(p.slots) < p.maxPairs
}

// orient puts a pair the way its buffer and candidate are keyed: the scoped end
// first when exactly one end is scoped (directed), otherwise lexicographic
// (undirected).
func (p *MICorrelationProposer) orient(a, b string) (from, to string, undirected bool, class string) {
	sa, sb := p.latestProps[a].scoped(), p.latestProps[b].scoped()
	switch {
	case sa && !sb:
		return a, b, false, types.PairSubjectNode
	case sb && !sa:
		return b, a, false, types.PairSubjectNode
	}
	class = types.PairNodeNode
	if sa {
		class = types.PairSubjectSubject
	}
	if b < a {
		a, b = b, a
	}
	return a, b, true, class
}

func candidateID(from, to string, undirected bool) string {
	if undirected {
		return from + "<->" + to
	}
	return from + "->" + to
}

// worthTestingLocked reports whether a pair could still produce anything: not
// barred by an arithmetic group, not settled by an operator, and not already a
// relationship either sign or way round.
func (p *MICorrelationProposer) worthTestingLocked(from, to string, undirected bool) bool {
	if p.barredLocked(from, to) {
		return false
	}
	if c, ok := p.candidates[candidateID(from, to, undirected)]; ok && settled(c) {
		return false
	}
	if p.lookup == nil {
		return true
	}
	for _, sign := range []int{1, -1} {
		if p.lookup.Covered(from, to, sign) || (undirected && p.lookup.Covered(to, from, sign)) {
			return false
		}
	}
	return true
}

// settled is a candidate evidence cannot change: confirmed, or rejected or deferred
// by an operator. The proposer's own deferrals (cap, untested) can still re-enter, and
// so can an agent's rejection or deferral once the evidence has moved.
func settled(c *types.CandidateEdge) bool {
	return c.Status == types.Confirmed ||
		((c.Status == types.Rejected || c.Status == types.Deferred) && c.Reason == byOperator)
}

// admitAllLocked starts testing every pair id can form now, while the budget has
// room. When there is less room than pairs, they are taken in id order so the choice
// does not follow map iteration.
func (p *MICorrelationProposer) admitAllLocked(id string, at time.Time) {
	var others []string
	for otherID, other := range p.latestProps {
		if otherID == id || p.paired(id, otherID) || absDur(at.Sub(other.at)) > p.pairWindow {
			continue
		}
		others = append(others, otherID)
	}
	if p.maxPairs > 0 && p.maxPairs-len(p.slots) < len(others) {
		sort.Strings(others)
	}
	for _, otherID := range others {
		if !p.roomLocked() {
			return
		}
		if p.admitLocked(id, otherID) {
			p.foldLocked(id, otherID)
		}
	}
}

// sameChart reports whether two properties are of one chart and one subject, which is
// where an arithmetic group can form.
func (p *MICorrelationProposer) sameChart(a, b string) bool {
	va, vb := p.latestProps[a], p.latestProps[b]
	return va.chart != "" && va.chart == vb.chart && va.subject == vb.subject
}

// barredLocked reports whether a pair would pair a group member other than its
// representative with a property outside its chart: the representative stands for
// the group there. Pairs inside a chart are never barred; they are what links a group
// and what would show it breaking.
func (p *MICorrelationProposer) barredLocked(a, b string) bool {
	if p.sameChart(a, b) {
		return false
	}
	ra, ga := p.repOf[a]
	rb, gb := p.repOf[b]
	return (ga && ra != a) || (gb && rb != b)
}

func (p *MICorrelationProposer) paired(a, b string) bool {
	_, ok := p.partners[a][b]
	return ok
}

// admitLocked starts testing a pair, if it is worth testing.
func (p *MICorrelationProposer) admitLocked(a, b string) bool {
	from, to, undirected, class := p.orient(a, b)
	if !p.worthTestingLocked(from, to, undirected) {
		return false
	}
	p.slots[from+"→"+to] = &pairSlot{from: from, to: to, class: class}
	for _, e := range [][2]string{{a, b}, {b, a}} {
		if p.partners[e[0]] == nil {
			p.partners[e[0]] = make(map[string]struct{})
		}
		p.partners[e[0]][e[1]] = struct{}{}
	}
	return true
}

// dropLocked stops testing a pair and forgets its window.
func (p *MICorrelationProposer) dropLocked(key string) {
	sl, ok := p.slots[key]
	if !ok {
		return
	}
	delete(p.slots, key)
	delete(p.buffers, key)
	delete(p.lastPair, key)
	delete(p.pvals, key)
	delete(p.partners[sl.from], sl.to)
	delete(p.partners[sl.to], sl.from)
	if _, linked := p.links[key]; linked {
		// An untested link can neither hold nor break a group: the group goes with it,
		// and its members compete like any other property until it forms again.
		delete(p.links, key)
		p.regroupLocked()
	}
}

// foldLocked folds id's latest reading with otherID's, if they are close enough in
// time and neither reading already took part in this pair as closely.
func (p *MICorrelationProposer) foldLocked(id, otherID string) {
	me, other := p.latestProps[id], p.latestProps[otherID]
	if absDur(me.at.Sub(other.at)) > p.pairWindow {
		return
	}
	from, to, undirected, _ := p.orient(id, otherID)
	fv, tv := p.latestProps[from], p.latestProps[to]
	xAt, yAt := fv.at.UnixNano(), tv.at.UnixNano()
	key := from + "→" + to
	replace := false
	if lp, used := p.lastPair[key]; used && (lp[0] == xAt || lp[1] == yAt) {
		if absNano(xAt-yAt) >= absNano(lp[0]-lp[1]) {
			return // that reading already took part in a pair with this counterpart, as closely
		}
		replace = true // a closer partner arrived for a reading already paired: swap it in
	}
	meta := pairMeta{property: true, undirected: undirected, replace: replace, sameChart: p.sameChart(from, to),
		xRange: fv.rng, yRange: tv.rng}
	switch {
	case !undirected && (metricOf(from) == to || fv.partOf == to):
		meta.kind = types.CandidateKindComposition
	case fv.scoped() && tv.scoped() && metricOf(from) == metricOf(to):
		meta.kind = types.CandidateKindSameMetric
	case fv.scoped() && fv.subject == tv.subject:
		meta.kind = types.CandidateKindSameSubject
	}
	// No identity: ObserveProperty refuses a reading that is not newer than its
	// property's last, which is the dedup this path needs.
	_ = p.observeLocked(from, to, fv.value, tv.value, "", meta) // never non-nil; the contract keeps the error return
	p.lastPair[key] = [2]int64{xAt, yAt}
}

// rationLocked shares a full budget out. It runs when the budget is full, at most
// once per pair window of reading time:
//
//   - pairs whose candidate an operator settled, or that became a relationship, stop
//     being tested: nothing they show can change anything;
//   - properties compete only if they were observed within the pair window and
//     moved, over their recent values, by at least minSpread of their declared range
//     (of their largest magnitude when they declare none), and an arithmetic group
//     only through its representative;
//   - the most variable properties get pairs first, whatever the pair's class: a
//     pair ranks by its less variable end, because the variation gate needs both;
//   - a new pair takes a free slot, or displaces a tested pair that ranks below it:
//     first one with an end gone quiet (not observed within the pair window), then
//     one that has held matureAt pairs and stays below the candidate threshold,
//     weakest |r|·n first. A live pair at or above the threshold is never
//     displaced, and a pair is never displaced by one that ranks no higher, so the
//     budget settles instead of rotating.
func (p *MICorrelationProposer) rationLocked(now time.Time) {
	for key, sl := range p.slots {
		if !p.worthTestingLocked(sl.from, sl.to, sl.class != types.PairSubjectNode) {
			p.dropLocked(key)
		}
	}

	type competitor struct {
		id string
		v  float64
	}
	var comp []competitor
	rank := map[string]float64{} // a competing property's variability; absent ranks 0
	fresh := map[string]bool{}
	for id, lv := range p.latestProps {
		if absDur(now.Sub(lv.at)) > p.pairWindow {
			continue
		}
		fresh[id] = true
		if rep, ok := p.repOf[id]; ok && rep != id {
			continue // its group competes through its representative
		}
		if v := p.variability(id, lv); v > 0 && v >= p.minSpread {
			comp = append(comp, competitor{id, v})
			rank[id] = v
		}
	}
	sort.Slice(comp, func(i, j int) bool {
		if comp[i].v != comp[j].v {
			return comp[i].v > comp[j].v
		}
		return comp[i].id < comp[j].id
	})

	mature := matureAt
	if mature > p.bufSize {
		mature = p.bufSize
	}
	type victim struct {
		key   string
		quiet bool
		score float64
		rank  float64
	}
	var victims []victim
	for key, sl := range p.slots {
		quiet := !fresh[sl.from] || !fresh[sl.to]
		if quiet || (sl.n >= mature && math.Abs(sl.r) < p.threshold) {
			victims = append(victims, victim{key, quiet, math.Abs(sl.r) * float64(sl.n), math.Min(rank[sl.from], rank[sl.to])})
		}
	}
	sort.Slice(victims, func(i, j int) bool {
		a, b := victims[i], victims[j]
		if a.quiet != b.quiet {
			return a.quiet
		}
		if a.score != b.score {
			return a.score < b.score
		}
		return a.key > b.key
	})

	// Pair (i, j) with i < j ranks by comp[j].v, so walking j upward visits pairs in
	// descending rank. A pair that finds no victim ranked below it ends the pass: every
	// later pair ranks no higher.
	for j := 1; j < len(comp); j++ {
		for i := 0; i < j; i++ {
			a, b := comp[i].id, comp[j].id
			if p.paired(a, b) {
				continue
			}
			if from, to, undirected, _ := p.orient(a, b); !p.worthTestingLocked(from, to, undirected) {
				continue
			}
			if !p.roomLocked() {
				k := slices.IndexFunc(victims, func(v victim) bool { return v.rank < comp[j].v })
				if k < 0 {
					return
				}
				p.displaceLocked(victims[k].key)
				victims = slices.Delete(victims, k, k+1)
			}
			p.admitLocked(a, b)
		}
	}
}

// displaceLocked stops testing a pair to make room for another. A pending candidate
// it backed is deferred as untested: nothing updates its numbers any more, so it
// should not ask an operator to confirm them, nor hold a place under the pending cap.
// It re-enters if the pair is tested again and its evidence still holds.
func (p *MICorrelationProposer) displaceLocked(key string) {
	sl := p.slots[key]
	if sl == nil {
		return
	}
	if c := p.candidates[candidateID(sl.from, sl.to, sl.class != types.PairSubjectNode)]; c != nil && c.Status == types.Pending {
		c.Status, c.Reason = types.Deferred, deferredUntested
	}
	p.dropLocked(key)
	p.displaced++
}

// variability is how far a property moved over its recent values: spread as a share
// of its declared range, or of its largest magnitude when it declares none.
func (p *MICorrelationProposer) variability(id string, lv latestValue) float64 {
	ring := p.recent[id]
	if ring == nil || len(ring.vs) < 2 {
		return 0
	}
	lo, hi, mag := ring.vs[0], ring.vs[0], 0.0
	for _, x := range ring.vs {
		lo, hi, mag = math.Min(lo, x), math.Max(hi, x), math.Max(mag, math.Abs(x))
	}
	scale := mag
	if lv.rng != nil {
		scale = lv.rng[1] - lv.rng[0]
	}
	if scale <= 0 {
		return 0
	}
	return (hi - lo) / scale
}

// linkLocked records whether a pair of one chart and one subject links an arithmetic
// group, regrouping when that changes, and reports whether it does.
func (p *MICorrelationProposer) linkLocked(key string, linked bool) bool {
	_, was := p.links[key]
	switch {
	case linked && !was:
		p.links[key] = struct{}{}
		p.regroupLocked()
	case !linked && was:
		delete(p.links, key)
		p.regroupLocked()
	}
	return linked
}

// linkFloorLocked is the |r| a pair of one chart needs to link a group: groupR to
// form the link, and groupR less one standard error of Fisher's z to keep it, so a pair
// hovering at the cut-off does not form and break its group at every crossing. The
// band is the one weakenedBelow gives a pending candidate (0.988 at 120 pairs).
func (p *MICorrelationProposer) linkFloorLocked(key string, n int) float64 {
	if _, linked := p.links[key]; linked {
		return weakenedBelow(groupR, n)
	}
	return groupR
}

// regroupLocked recomputes the arithmetic groups from their links: the members a
// chain of links connects are one group. Its representative is the most variable
// member, except that a member already representing part of the group keeps the
// role, so one link forming elsewhere does not move every group's pairs. A pair that
// regrouping bars stops being tested, and a pending candidate it backed is deferred
// as grouped; it re-enters if the group breaks and the pair still holds.
func (p *MICorrelationProposer) regroupLocked() {
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if parent[x] == x {
			return x
		}
		parent[x] = find(parent[x])
		return parent[x]
	}
	for key := range p.links {
		a, b, _ := strings.Cut(key, "→")
		for _, x := range []string{a, b} {
			if _, ok := parent[x]; !ok {
				parent[x] = x
			}
		}
		if ra, rb := find(a), find(b); ra != rb {
			parent[ra] = rb
		}
	}
	groups := map[string][]string{}
	for x := range parent {
		groups[find(x)] = append(groups[find(x)], x)
	}
	prev := p.repOf
	p.repOf = make(map[string]string, len(parent))
	for _, members := range groups {
		sort.Strings(members)
		pick := func(eligible func(string) bool) string {
			best, bestV := "", -1.0
			for _, m := range members {
				if !eligible(m) {
					continue
				}
				if v := p.variability(m, p.latestProps[m]); v > bestV {
					best, bestV = m, v
				}
			}
			return best
		}
		rep := pick(func(m string) bool { return prev[m] == m })
		if rep == "" {
			rep = pick(func(string) bool { return true })
		}
		for _, m := range members {
			p.repOf[m] = rep
		}
	}
	for key, sl := range p.slots {
		if !p.barredLocked(sl.from, sl.to) {
			continue
		}
		if c := p.candidates[candidateID(sl.from, sl.to, sl.class != types.PairSubjectNode)]; c != nil && c.Status == types.Pending {
			c.Status, c.Reason = types.Deferred, deferredGrouped
		}
		p.dropLocked(key)
	}
}

// groupsLocked lists the arithmetic groups, ordered by representative.
func (p *MICorrelationProposer) groupsLocked() []types.ArithmeticGroup {
	byRep := map[string][]string{}
	for m, rep := range p.repOf {
		byRep[rep] = append(byRep[rep], m)
	}
	out := make([]types.ArithmeticGroup, 0, len(byRep))
	for rep, members := range byRep {
		sort.Strings(members)
		lv := p.latestProps[rep]
		out = append(out, types.ArithmeticGroup{Chart: lv.chart, Subject: lv.subject, Representative: rep, Members: members})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Representative < out[j].Representative })
	return out
}

// barLocked is the p-value bar a candidate must be at or under: Benjamini–Hochberg at
// rate fdr over the m pairs being tested, arithmetic links aside. A pair without
// enough readings for a p-value yet still counts, as a test that has found nothing:
// otherwise, in the first round, the first pairs to reach enough readings would be
// judged as if they were the only tests. With the p-values sorted ascending, the bar
// is k·fdr/m for the largest k whose p-value is at or under that, and fdr/m when none
// is — so a candidate needs more evidence the more pairs are tested. It is recomputed
// once per round of folds, or sooner when m has moved by a tenth, which keeps its
// cost about one sort per tick. 0 when fdr is 0.
func (p *MICorrelationProposer) barLocked() float64 {
	if p.fdr <= 0 {
		return 0
	}
	tests := len(p.buffers) - len(p.links)
	if p.barValid && p.folds-p.barFolds < uint64(len(p.pvals)) && absInt(tests-p.barTests)*10 < p.barTests {
		return p.bar
	}
	ps := make([]float64, 0, len(p.pvals))
	for _, v := range p.pvals {
		ps = append(ps, v)
	}
	sort.Float64s(ps)
	m, k := float64(max(tests, len(ps))), 1
	for i := len(ps) - 1; i >= 0; i-- {
		if ps[i] <= float64(i+1)*p.fdr/m {
			k = i + 1
			break
		}
	}
	p.bar = p.fdr
	if m > 0 {
		p.bar = float64(k) * p.fdr / m
	}
	p.barValid, p.barFolds, p.barTests = true, p.folds, tests
	return p.bar
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// Discovery reports the budget and how far it rationed the pairs.
func (p *MICorrelationProposer) Discovery() (*types.DiscoveryCounts, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	d := &types.DiscoveryCounts{
		Budget:        p.maxPairs,
		TestedByClass: map[string]int{types.PairSubjectNode: 0, types.PairNodeNode: 0, types.PairSubjectSubject: 0},
		Displaced:     p.displaced,
	}
	fresh := map[string]bool{}
	for id, lv := range p.latestProps {
		if absDur(p.latestAt.Sub(lv.at)) <= p.pairWindow {
			fresh[id] = true
		}
	}
	d.Properties = len(fresh)
	d.EligiblePairs = d.Properties * (d.Properties - 1) / 2
	d.Groups = p.groupsLocked()
	d.FDR, d.PBar = p.fdr, p.barLocked()
	d.PValuesTested = max(len(p.buffers)-len(p.links), len(p.pvals))
	for _, sl := range p.slots {
		if fresh[sl.from] && fresh[sl.to] {
			d.TestedPairs++
			d.TestedByClass[sl.class]++
		} else {
			d.IdlePairs++
		}
	}
	return d, nil
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func absNano(d int64) int64 {
	if d < 0 {
		return -d
	}
	return d
}

// metricOf is the metric a property id names: "cpu_utilization@pod:a" and
// "cpu_utilization" both name cpu_utilization.
func metricOf(id string) string {
	m, _, _ := strings.Cut(id, "@")
	return m
}

// Forget drops the buffers, latest values and candidates involving a property.
func (p *MICorrelationProposer) Forget(propertyID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.latestProps, propertyID)
	delete(p.latestConstructs, propertyID)
	delete(p.recent, propertyID)
	for key, sl := range p.slots {
		if sl.from == propertyID || sl.to == propertyID {
			p.dropLocked(key)
		}
	}
	delete(p.partners, propertyID)
	for key := range p.buffers {
		from, to, _ := strings.Cut(key, "→")
		if from == propertyID || to == propertyID {
			delete(p.buffers, key)
			delete(p.lastPair, key)
			delete(p.pvals, key)
		}
	}
	regroup := false
	if _, ok := p.repOf[propertyID]; ok {
		regroup = true
	}
	for key := range p.links {
		from, to, _ := strings.Cut(key, "→")
		if from == propertyID || to == propertyID {
			delete(p.links, key)
			regroup = true
		}
	}
	if regroup {
		p.regroupLocked()
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

func (p *MICorrelationProposer) observeLocked(fromID, toID string, valueA, valueB float64, identity string, meta pairMeta) error {
	key := fromID + "→" + toID
	buf, ok := p.buffers[key]
	if !ok {
		buf = stats.NewPairWindow(p.bufSize)
		p.buffers[key] = buf
	}
	folded := buf.Fold
	if meta.replace {
		folded = buf.ReplaceLast
	}
	if !folded(identity, valueA, valueB) {
		return nil
	}
	if buf.Len() < p.minPairs {
		return nil
	}
	r, ok := buf.Pearson()
	if ok && math.Abs(r) >= weakenedBelow(p.threshold, buf.Len()) {
		// A strong r over a window that is mostly one value is a few shared spikes:
		// idle interfaces that burst at the same moments reached r = 1 on a VM on a
		// handful of readings. Each end must hold minPairs readings' worth of evidence — its
		// readings off its commonest value, plus that value once — the least a pair
		// is tested on, or its r is as undefined as a constant series'. Checked only
		// where r could matter, since it costs a pass over the window.
		if dx, dy := buf.Distinct(); dx < p.minPairs || dy < p.minPairs {
			r, ok = 0, false
		}
	}
	if sl := p.slots[key]; sl != nil {
		sl.n, sl.r = buf.Len(), r // r is 0 when undefined, which ranks the pair weakest
	}
	pv := 1.0 // a pair whose r is undefined was tested and showed nothing
	if ok {
		pv = stats.FisherPValue(r, buf.Len())
	}
	p.folds++
	candID := candidateID(fromID, toID, meta.undirected)
	if meta.sameChart && p.linkLocked(key, ok && math.Abs(r) >= p.linkFloorLocked(key, buf.Len())) {
		// Arithmetic, not a finding: the group is reported in Discovery instead, and
		// the link's p ≈ 0 stays out of the bar, where it would count as a discovery.
		delete(p.pvals, key)
		if c := p.candidates[candID]; c != nil && c.Status == types.Pending {
			c.Status, c.Reason = types.Deferred, deferredGrouped
		}
		return nil
	}
	p.pvals[key] = pv
	if !ok || math.Abs(r) < p.threshold {
		// A pending candidate whose pair no longer holds leaves the queue with the
		// numbers that put it out, rather than asking an operator to confirm an |r|
		// it had once. It re-enters if the pair holds again. A dip smaller than the
		// window's sampling error is not a pair that stopped holding: leaving at the
		// threshold itself, a pair sitting on it flapped in and out of the queue.
		if c := p.candidates[candID]; c != nil && c.Status == types.Pending {
			c.MIScore, c.NObservations = math.Abs(r), buf.Len()
			c.PValue = pv
			if !ok || math.Abs(r) < weakenedBelow(p.threshold, buf.Len()) {
				c.Status, c.Reason = types.Deferred, deferredWeakened
			}
		}
		return nil
	}
	bar := p.barLocked()
	if bar > 0 && pv > bar {
		// Strong enough alone, not once the number of pairs tested is counted. A pending
		// candidate the bar has tightened past leaves the queue with the numbers that
		// put it out; it re-enters if it clears the bar again.
		if c := p.candidates[candID]; c != nil && c.Status == types.Pending {
			c.MIScore, c.PValue, c.PBar, c.NObservations = math.Abs(r), pv, bar, buf.Len()
			c.Status, c.Reason = types.Deferred, deferredByBar
		}
		return nil
	}
	direction, sign := types.Positive, 1
	if r < 0 {
		direction, sign = types.Negative, -1
	}
	if p.lookup != nil && (p.lookup.Covered(fromID, toID, sign) ||
		(meta.undirected && p.lookup.Covered(toID, fromID, sign))) {
		return nil // an undirected pair is covered by a relationship either way round
	}
	if existing, ok := p.candidates[candID]; ok {
		switch existing.Status {
		case types.Confirmed:
			return nil // a confirmed pair is settled; never re-emitted
		case types.Rejected, types.Deferred:
			if existing.Reason == byOperator {
				return nil // an operator's rejection or deferral is not overturned by evidence
			}
			existing.Direction, existing.MIScore = direction, math.Abs(r)
			existing.PValue, existing.PBar, existing.NObservations = pv, bar, buf.Len()
			existing.Kind = meta.kind
			if existing.Reason == byAgent {
				// An agent decided on evidence that may since have moved; until it has,
				// the decision stands, and once it has, the candidate is reviewed again.
				if existing.Review.Outdated(existing) {
					existing.Status, existing.Reason = types.Pending, ""
					p.log("proposer: %s returns to pending: its evidence moved since the agent's %s", candID, existing.Review.Action)
					p.enforceCapLocked()
				}
				return nil
			}
			if p.outranksPendingLocked(existing) {
				existing.Status, existing.Reason = types.Pending, ""
				p.log("proposer: %s re-enters pending with |r|·n=%.1f; the cap defers the weakest instead", candID, score(existing))
				p.enforceCapLocked()
			}
			return nil
		case types.Pending:
			existing.Direction = direction
			existing.MIScore = math.Abs(r)
			existing.PValue, existing.PBar = pv, bar
			existing.NObservations = buf.Len()
			// The kind follows the latest readings too: a part_of declared after the
			// candidate formed, or withdrawn since, changes what the candidate is.
			existing.Kind = meta.kind
			return nil
		}
	}
	var caveats []string
	if meta.property {
		var moved bool
		if moved, caveats = p.movedEnoughLocked(fromID, toID, buf, meta); !moved {
			return nil
		}
	}
	p.candidates[candID] = &types.CandidateEdge{
		CandidateID: candID, FromID: fromID, ToID: toID, Direction: direction,
		MIScore: math.Abs(r), PValue: pv, PBar: bar,
		NObservations: buf.Len(), Status: types.Pending,
		Undirected: meta.undirected, Kind: meta.kind, Caveats: caveats,
	}
	p.order = append(p.order, candID)
	p.enforceCapLocked()
	return nil
}

// movedEnoughLocked is the variation gate. Correlation is scale-free: a pod whose
// memory steps between 572 KB and 1 MB correlates with node memory at r = 0.995 while
// moving 0.01% of the node. So each end with a declared range must have moved, over
// the window, by at least minSpread of that range. An end without a declared range
// cannot be checked, and the candidate says so instead of passing silently.
func (p *MICorrelationProposer) movedEnoughLocked(fromID, toID string, buf *stats.PairWindow, meta pairMeta) (bool, []string) {
	xs, ys := buf.Spread()
	var caveats []string
	for _, end := range []struct {
		id     string
		spread float64
		rng    *[2]float64
	}{{fromID, xs, meta.xRange}, {toID, ys, meta.yRange}} {
		if end.rng == nil {
			caveats = append(caveats, fmt.Sprintf("%s declares no range, so whether it moved enough to matter was not checked", end.id))
			continue
		}
		if width := end.rng[1] - end.rng[0]; end.spread < p.minSpread*width {
			return false, nil
		}
	}
	return true, caveats
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
	deferredByCap = "cap"
	// byOperator and byAgent say who rejected or deferred a candidate. Only an
	// operator's decision is final for the session.
	byOperator = "operator"
	byAgent    = "agent"
	// deferredUntested is a pending candidate whose pair was displaced from the
	// budget: its numbers stopped being updated.
	deferredUntested = "untested"
	// deferredGrouped is a candidate an arithmetic group took over: its pair links the
	// group, or an end joined a group it does not represent.
	deferredGrouped = "grouped"
	// deferredByBar is a pending candidate whose p-value no longer clears the bar, as
	// more pairs came to be tested beside it.
	deferredByBar = "bar"
	// deferredWeakened is a pending candidate whose |r| fell below the candidate
	// threshold by more than its sampling error (weakenedBelow): the evidence it
	// was emitted on no longer holds.
	deferredWeakened = "weakened"
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
// from orients it: "" or the candidate's FromID for a directed candidate; either end
// for an undirected one, which has no orientation until an operator gives it.
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
func (p *MICorrelationProposer) Confirm(candidateID, from string, rv types.CandidateReview) (*types.Proposition, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.candidates[candidateID]
	if !ok {
		return nil, fmt.Errorf("candidate %q not found", candidateID)
	}
	if c.Status != types.Pending {
		return nil, fmt.Errorf("candidate %q is not Pending (status=%v)", candidateID, c.Status)
	}
	src, dst := c.FromID, c.ToID
	switch {
	case c.Undirected && from == "":
		return nil, fmt.Errorf("%w: %s is undirected; name the end it runs from (%s or %s)",
			contracts.ErrBadConfirmation, candidateID, c.FromID, c.ToID)
	case c.Undirected && from == c.ToID:
		src, dst = c.ToID, c.FromID
	case from != "" && from != c.FromID:
		if c.Undirected {
			return nil, fmt.Errorf("%w: %q is not an end of %s", contracts.ErrBadConfirmation, from, candidateID)
		}
		return nil, fmt.Errorf("%w: %s runs from %s; a directed candidate is confirmed as proposed",
			contracts.ErrBadConfirmation, candidateID, c.FromID)
	}

	prop := &types.Proposition{
		PropositionID:   "P-" + synthesizePropSuffix(candidateID),
		FromConstruct:   src,
		ToConstruct:     dst,
		Direction:       c.Direction,
		PriorStrength:   c.MIScore,
		Description:     fmt.Sprintf("Auto-proposed by MICorrelationProposer (|r|=%.3f, n=%d)", c.MIScore, c.NObservations),
		EvidenceSources: []string{"proposer-mi"},
	}
	if c.Undirected {
		prop.Description += "; direction chosen by the " + reviewerName(rv)
	}
	switch c.Kind {
	case types.CandidateKindComposition:
		prop.Description += "; composition: the source is part of the target"
	case types.CandidateKindSameMetric:
		prop.Description += "; same metric on two subjects: a shared driver is likely"
	case types.CandidateKindSameSubject:
		prop.Description += "; two metrics of one subject: they may hold together by construction"
	}
	c.Status = types.Confirmed
	rv.Applied = true
	c.Review = stamp(c, rv, types.ReviewConfirm)
	return prop, nil
}

// stamp completes a review with the candidate's evidence as it stands, so a later
// change in that evidence can be told from the one the decision was made on.
func stamp(c *types.CandidateEdge, rv types.CandidateReview, action string) *types.CandidateReview {
	if rv.By == "" {
		rv.By = types.ReviewerOperator
	}
	if rv.Action == "" {
		rv.Action = action
	}
	if rv.At.IsZero() {
		rv.At = time.Now()
	}
	rv.MIScore, rv.NObservations, rv.Direction, rv.Kind = c.MIScore, c.NObservations, c.Direction, c.Kind
	return &rv
}

// reviewerName is how a proposition's description names who oriented it.
func reviewerName(rv types.CandidateReview) string {
	switch {
	case rv.By == "" || rv.By == types.ReviewerOperator:
		return "operator"
	case rv.Model != "":
		return rv.By + " " + rv.Model
	}
	return rv.By
}

// reasonFor is who a rejection or deferral is attributed to.
func reasonFor(rv types.CandidateReview) string {
	if rv.By == "" || rv.By == types.ReviewerOperator {
		return byOperator
	}
	return byAgent
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
	c.Status, c.Reason, c.Review = types.Pending, "", nil // the confirmation never took effect
	return nil
}

// Reject marks a candidate Rejected. An operator's rejection is permanent for the
// session: the candidate stays in the map so later observations of the pair are
// short-circuited. An agent's (Reason "agent") is not: the pair is still tested, and
// the candidate returns to Pending once its evidence moves past what the review saw.
// A Confirmed candidate cannot be rejected: the relationship it became is in the map,
// and retiring that is the operator's move.
func (p *MICorrelationProposer) Reject(candidateID string, rv types.CandidateReview) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.candidates[candidateID]
	if !ok {
		return fmt.Errorf("candidate %q not found", candidateID)
	}
	if c.Status == types.Confirmed {
		return fmt.Errorf("candidate %q is Confirmed: the relationship it became is in the map; retire that instead", candidateID)
	}
	if settled(c) {
		return fmt.Errorf("candidate %q is %v by the operator", candidateID, c.Status)
	}
	c.Status, c.Reason = types.Rejected, reasonFor(rv)
	rv.Applied = true
	c.Review = stamp(c, rv, types.ReviewReject)
	return nil
}

// Defer marks a candidate Deferred — out of GetCandidates, still in history. An
// operator's deferral (Reason "operator") is not overturned by later evidence; an
// agent's (Reason "agent") returns to Pending once the evidence moves past what the
// review saw. A cap deferral (Reason "cap") is the proposer's own, provisional choice
// and re-enters when it outranks a pending candidate; an operator may Defer a
// cap-deferred candidate to make it stick. Confirmed candidates, and ones an operator
// settled, cannot be deferred.
func (p *MICorrelationProposer) Defer(candidateID string, rv types.CandidateReview) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.candidates[candidateID]
	if !ok {
		return fmt.Errorf("candidate %q not found", candidateID)
	}
	if settled(c) {
		return fmt.Errorf("candidate %q is %v and cannot be deferred", candidateID, c.Status)
	}
	c.Status, c.Reason = types.Deferred, reasonFor(rv)
	rv.Applied = true
	c.Review = stamp(c, rv, types.ReviewDefer)
	return nil
}

// Annotate records a review of a Pending candidate without acting on it: an
// escalation, or what a reviewer would have done had the autonomy level let it.
// The candidate stays Pending, in the operator's queue.
func (p *MICorrelationProposer) Annotate(candidateID string, rv types.CandidateReview) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.candidates[candidateID]
	if !ok {
		return fmt.Errorf("candidate %q not found", candidateID)
	}
	if c.Status != types.Pending {
		return fmt.Errorf("candidate %q is not Pending (status=%v)", candidateID, c.Status)
	}
	rv.Applied = false
	c.Review = stamp(c, rv, types.ReviewEscalate)
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
	cp.Caveats = append([]string(nil), c.Caveats...)
	if c.Review != nil {
		rv := *c.Review
		cp.Review = &rv
	}
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

// weakenedBelow is the |r| under which a pending candidate leaves the queue: the
// threshold less one standard error of Fisher's z over n pairs, 1/√(n−3). Entering
// takes the threshold itself, so a pair whose |r| jitters around it is proposed once
// rather than at every crossing; the band narrows as the window fills (0.82 for a
// 0.85 threshold at 120 pairs).
func weakenedBelow(threshold float64, n int) float64 {
	if n <= 3 || threshold >= 1 {
		return threshold // no band to give
	}
	return math.Tanh(math.Atanh(threshold) - 1/math.Sqrt(float64(n-3)))
}
