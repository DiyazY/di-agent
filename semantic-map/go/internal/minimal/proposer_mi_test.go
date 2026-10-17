package minimal_test

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/compliance"
	"github.com/DiyazY/di-agent/internal/minimal"
	"github.com/DiyazY/di-agent/pkg/contracts"
	"github.com/DiyazY/di-agent/pkg/statemap"
	"github.com/DiyazY/di-agent/pkg/types"
)

// ── Compliance ────────────────────────────────────────────────────────────────

func TestMICorrelationProposerCompliance(t *testing.T) {
	compliance.RunProposerCompliance(t, func(t *testing.T) contracts.ProposerContract {
		// Pair the proposer with a real ontology so the backbone coverage
		// check has real propositions to consult. The compliance suite feeds
		// PS→RC observations; P10 (PS→RC −) is in the bootstrap, but the
		// suite's data has positive correlation, so the proposer emits a
		// positive PS→RC candidate (conflict-pair sibling — multigraph-legal).
		ontology := minimal.NewOntologyFromSpec(mustSpec())
		return minimal.NewMICorrelationProposer(minimal.LookupOntology(ontology), 0.8, 30, 100, 0)
	})
}

// ── Strongly correlated input → emits a candidate ─────────────────────────────

func TestMICorrelationProposer_StronglyCorrelatedEmits(t *testing.T) {
	ontology := minimal.NewOntologyFromSpec(mustSpec())
	// Use a free pair (MU↛PS), threshold 0.8, minPairs 30, bufSize 200.
	p := minimal.NewMICorrelationProposer(minimal.LookupOntology(ontology), 0.8, 30, 200, 0)

	for i := 0; i < 100; i++ {
		x := float64(i) / 100.0
		y := 0.95*x + 0.01
		if err := p.Observe("MU", "PS", x, y); err != nil {
			t.Fatal(err)
		}
	}

	cs, err := p.GetCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 {
		t.Fatalf("expected exactly 1 candidate after strong correlation; got %d", len(cs))
	}
	c := cs[0]
	if c.CandidateID != "MU->PS" {
		t.Errorf("unexpected candidate id: %s", c.CandidateID)
	}
	if c.Direction != types.Positive {
		t.Errorf("expected positive direction; got %v", c.Direction)
	}
	if c.MIScore < 0.95 {
		t.Errorf("expected MIScore close to 1.0; got %v", c.MIScore)
	}
}

// ── Uncorrelated input → no candidate ─────────────────────────────────────────

func TestMICorrelationProposer_UncorrelatedQuiet(t *testing.T) {
	ontology := minimal.NewOntologyFromSpec(mustSpec())
	p := minimal.NewMICorrelationProposer(minimal.LookupOntology(ontology), 0.8, 30, 200, 0)
	rng := rand.New(rand.NewSource(7))

	for i := 0; i < 200; i++ {
		x := rng.Float64()
		y := rng.Float64() // independent
		if err := p.Observe("MU", "PS", x, y); err != nil {
			t.Fatal(err)
		}
	}

	cs, _ := p.GetCandidates()
	if len(cs) != 0 {
		t.Errorf("expected no candidates for uncorrelated input; got %d (first: %+v)", len(cs), cs[0])
	}
}

// ── Confirm: candidate becomes a real proposition ────────────────────────────

func TestMICorrelationProposer_ConfirmAddsProposition(t *testing.T) {
	ontology := minimal.NewOntologyFromSpec(mustSpec())
	p := minimal.NewMICorrelationProposer(minimal.LookupOntology(ontology), 0.8, 30, 200, 0)

	for i := 0; i < 60; i++ {
		x := float64(i) / 100.0
		y := 0.9 * x
		_ = p.Observe("MU", "PS", x, y)
	}

	before, _ := ontology.Propositions()
	beforeCount := len(before)

	cs, _ := p.GetCandidates()
	if len(cs) != 1 {
		t.Fatalf("expected 1 candidate; got %d", len(cs))
	}
	// Confirm returns the proposition rather than adding it: the caller applies it
	// through the facade, which is the only path that reaches the state model too.
	prop, err := p.Confirm(cs[0].CandidateID, "", types.CandidateReview{})
	if err != nil {
		t.Fatalf("Confirm error: %v", err)
	}
	if prop == nil {
		t.Fatal("Confirm returned no proposition for a pending candidate")
	}
	if prop.FromConstruct != cs[0].FromID || prop.ToConstruct != cs[0].ToID {
		t.Errorf("returned proposition %s→%s does not carry the candidate's endpoints %s→%s",
			prop.FromConstruct, prop.ToConstruct, cs[0].FromID, cs[0].ToID)
	}
	if err := ontology.AddValidatedProposition(prop); err != nil {
		t.Fatalf("applying the returned proposition: %v", err)
	}

	after, _ := ontology.Propositions()
	if len(after) != beforeCount+1 {
		t.Errorf("applying the confirmation should add exactly 1 proposition; before=%d after=%d",
			beforeCount, len(after))
	}

	// Confirmed candidate must no longer appear in pending.
	cs, _ = p.GetCandidates()
	if len(cs) != 0 {
		t.Errorf("expected 0 pending candidates after Confirm; got %d", len(cs))
	}

	// History records the candidate with its final status (Confirmed).
	hist, _ := p.GetHistory()
	if len(hist) != 1 {
		t.Fatalf("expected 1 history entry; got %d", len(hist))
	}
	if hist[0].Status != types.Confirmed {
		t.Errorf("history entry should reflect Confirmed status; got %v", hist[0].Status)
	}

	// New proposition is visible via Propositions().
	var foundConfirmed bool
	for _, prop := range after {
		if prop.FromConstruct == "MU" && prop.ToConstruct == "PS" &&
			len(prop.EvidenceSources) == 1 && prop.EvidenceSources[0] == "proposer-mi" {
			foundConfirmed = true
			break
		}
	}
	if !foundConfirmed {
		t.Error("could not locate the confirmed MU→PS proposition in ontology")
	}
}

// ── Reject suppresses future re-emission ──────────────────────────────────────

func TestMICorrelationProposer_RejectSuppressesReemission(t *testing.T) {
	ontology := minimal.NewOntologyFromSpec(mustSpec())
	p := minimal.NewMICorrelationProposer(minimal.LookupOntology(ontology), 0.8, 30, 200, 0)

	feed := func() {
		for i := 0; i < 60; i++ {
			x := float64(i) / 100.0
			y := 0.9 * x
			_ = p.Observe("MU", "PS", x, y)
		}
	}

	feed()
	cs, _ := p.GetCandidates()
	if len(cs) != 1 {
		t.Fatalf("expected 1 pending candidate; got %d", len(cs))
	}
	cid := cs[0].CandidateID
	if err := p.Reject(cid, types.CandidateReview{}); err != nil {
		t.Fatal(err)
	}

	// Continue feeding correlated data — must not re-emit the rejected pair.
	feed()
	cs, _ = p.GetCandidates()
	for _, c := range cs {
		if c.CandidateID == cid {
			t.Errorf("rejected candidate %q re-emitted as pending after further observations", cid)
		}
	}
}

// ── Re-emission idempotency: many Observes → one CandidateID ──────────────────

func TestMICorrelationProposer_NoDuplicateCandidate(t *testing.T) {
	ontology := minimal.NewOntologyFromSpec(mustSpec())
	p := minimal.NewMICorrelationProposer(minimal.LookupOntology(ontology), 0.8, 30, 200, 0)

	for i := 0; i < 500; i++ {
		x := float64(i%100) / 100.0
		y := 0.9 * x
		_ = p.Observe("MU", "PS", x, y)
	}
	cs, _ := p.GetCandidates()
	if len(cs) != 1 {
		t.Errorf("expected exactly 1 candidate; got %d", len(cs))
	}
	// History should accumulate but never have two distinct CandidateIDs for the same pair.
	hist, _ := p.GetHistory()
	seen := make(map[string]bool)
	for _, h := range hist {
		seen[h.CandidateID] = true
	}
	if len(seen) != 1 {
		t.Errorf("history shows %d distinct CandidateIDs for same pair; expected 1", len(seen))
	}
}

// ── Coverage check: existing same-direction proposition blocks emission ───────

func TestMICorrelationProposer_RespectsExistingDirection(t *testing.T) {
	// The spec's first proposition already occupies its (from, to) pair in one
	// direction. Same-direction proposals on that pair must be blocked;
	// opposite-direction (conflict-pair sibling) proposals are permitted
	// (multigraph behavior).
	from, to, sign := specPair()
	if from == "" {
		t.Skip("spec covers every pair in both directions; no free direction to test")
	}
	{
		ontology := minimal.NewOntologyFromSpec(mustSpec())
		p := minimal.NewMICorrelationProposer(minimal.LookupOntology(ontology), 0.8, 30, 200, 0)
		for i := 0; i < 100; i++ {
			x := float64(i) / 100.0
			y := sameDirectionSeries(sign, x)
			_ = p.Observe(from, to, x, y)
		}
		cs, _ := p.GetCandidates()
		if len(cs) != 0 {
			t.Errorf("expected no candidate (pair already in the backbone); got %d", len(cs))
		}
	}
	{
		ontology := minimal.NewOntologyFromSpec(mustSpec())
		p := minimal.NewMICorrelationProposer(minimal.LookupOntology(ontology), 0.8, 30, 200, 0)
		for i := 0; i < 100; i++ {
			x := float64(i) / 100.0
			y := sameDirectionSeries(flipDir(sign), x)
			_ = p.Observe(from, to, x, y)
		}
		cs, _ := p.GetCandidates()
		if len(cs) != 1 {
			t.Fatalf("expected 1 candidate on the free direction of %s→%s; got %d", from, to, len(cs))
		}
		if wantDir := oppositeOf(sign); cs[0].Direction != wantDir {
			t.Errorf("expected %v direction; got %v", wantDir, cs[0].Direction)
		}
	}
}

// ── Pearson sanity ────────────────────────────────────────────────────────────

func TestMICorrelationProposer_PerfectCorrelation(t *testing.T) {
	ontology := minimal.NewOntologyFromSpec(mustSpec())
	p := minimal.NewMICorrelationProposer(minimal.LookupOntology(ontology), 0.5, 10, 50, 0)
	// y = x exactly → r should be ≈ 1.0.
	for i := 0; i < 20; i++ {
		x := float64(i)
		_ = p.Observe("MU", "PS", x, x)
	}
	cs, _ := p.GetCandidates()
	if len(cs) != 1 {
		t.Fatalf("expected 1 candidate; got %d", len(cs))
	}
	if math.Abs(cs[0].MIScore-1.0) > 1e-6 {
		t.Errorf("expected MIScore=1.0 for perfect correlation; got %v", cs[0].MIScore)
	}
}

// ── LookupOntology ─────────────────────────────────────────────────────────────

func TestLookupOntologyCoversDeclaredPropositions(t *testing.T) {
	ontology := minimal.NewOntologyFromSpec(mustSpec())
	first := mustSpec().Propositions[0]
	sign := 1
	if first.Direction == "negative" {
		sign = -1
	}
	l := minimal.LookupOntology(ontology)
	if !l.Covered(first.FromConstruct, first.ToConstruct, sign) {
		t.Errorf("proposition %s should cover (%s,%s,%+d)", first.PropositionID, first.FromConstruct, first.ToConstruct, sign)
	}
	if l.Covered(first.FromConstruct, first.ToConstruct, -sign) {
		t.Error("the opposite sign is not covered by the same proposition")
	}
}

// ── Property-level pairing with the scope rule ────────────────────────────────

// coveredNone is a lookup that covers nothing, for tests about pairing alone.
type coveredNone struct{}

func (coveredNone) Covered(_, _ string, _ int) bool { return false }

// subjectPairsKnown counts every pair of two subjects' properties as already a
// relationship, so a test about another mechanism sees only pairs with the node.
type subjectPairsKnown struct{}

func (subjectPairsKnown) Covered(from, to string, _ int) bool {
	return strings.Contains(from, "@") && strings.Contains(to, "@")
}

func feedScoped(p *minimal.MICorrelationProposer, n int, gap time.Duration) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		at := t0.Add(time.Duration(i) * 10 * time.Second)
		x := float64(i%20) / 20
		_ = p.ObserveProperty(types.PropertyReading{ID: "cpu_utilization@pod:a", Subject: "pod:a", Value: x, At: at})
		_ = p.ObserveProperty(types.PropertyReading{ID: "cpu_utilization@pod:b", Subject: "pod:b", Value: 1 - x, At: at})
		_ = p.ObserveProperty(types.PropertyReading{ID: "cpu_pressure_ratio", Subject: "", Value: 0.9*x + 0.05, At: at.Add(gap)})
	}
}

func mustCandidates(t *testing.T, p *minimal.MICorrelationProposer) []*types.CandidateEdge {
	t.Helper()
	cs, err := p.GetCandidates()
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func TestObserveProperty_PairsSubjectsWithTheNodeAndWithEachOther(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 60, 15*time.Second)
	feedScoped(p, 40, 2*time.Second)
	byID := map[string]*types.CandidateEdge{}
	for _, c := range mustCandidates(t, p) {
		byID[c.CandidateID] = c
		if c.FromID == "cpu_pressure_ratio" {
			t.Errorf("direction must be scoped -> unscoped; got %s -> %s", c.FromID, c.ToID)
		}
	}
	for _, id := range []string{"cpu_utilization@pod:a->cpu_pressure_ratio", "cpu_utilization@pod:b->cpu_pressure_ratio"} {
		if c := byID[id]; c == nil || c.Undirected {
			t.Errorf("expected directed %s; got %+v", id, c)
		}
	}
	// The two pods move in opposite directions: an undirected, negative, same-metric pair.
	c := byID["cpu_utilization@pod:a<->cpu_utilization@pod:b"]
	if c == nil {
		t.Fatalf("expected the two pods to be paired with each other; got %v", byID)
	}
	if !c.Undirected || c.Direction != types.Negative || c.Kind != types.CandidateKindSameMetric {
		t.Errorf("pod pair: undirected=%v direction=%v kind=%q; want undirected, negative, same-metric", c.Undirected, c.Direction, c.Kind)
	}
}

func TestObserveProperty_RespectsTimeTolerance(t *testing.T) {
	p := minimal.NewMICorrelationProposer(subjectPairsKnown{}, 0.8, 10, 60, 15*time.Second)
	feedScoped(p, 40, 10*time.Minute) // every node reading is minutes away from every pod reading, so no pair can ever fall inside the 15s window
	if cs := mustCandidates(t, p); len(cs) != 0 {
		t.Errorf("readings outside the pair window formed %d candidates; want none", len(cs))
	}
}

func TestObserveProperty_SkipsCoveredPairs(t *testing.T) {
	m := statemap.New(statemap.Config{AdmitUnknown: true}, statemap.NewJournal(0))
	_ = m.Record(statemap.Observation{ID: "cpu_utilization@pod:a", Value: .1, At: time.Now(), Subject: "pod:a"})
	_ = m.Observe("cpu_pressure_ratio", .1, time.Now())
	_ = m.DeclareRelationship(statemap.Relationship{From: "cpu_utilization@pod:a", To: "cpu_pressure_ratio", Sign: 1, Label: "discovered"})
	p := minimal.NewMICorrelationProposer(m, 0.8, 10, 60, 15*time.Second)
	feedScoped(p, 40, 2*time.Second)
	ids := map[string]bool{}
	for _, c := range mustCandidates(t, p) {
		ids[c.CandidateID] = true
		if c.FromID == "cpu_utilization@pod:a" && !c.Undirected {
			t.Error("a pair the state map already holds must not be re-proposed")
		}
	}
	if !ids["cpu_utilization@pod:b->cpu_pressure_ratio"] {
		t.Error("expected the uncovered pod:b candidate to still be proposed")
	}
}

func TestForgetDropsEverythingAboutAProperty(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 60, 15*time.Second)
	feedScoped(p, 40, 2*time.Second)
	if err := p.Forget("cpu_utilization@pod:a"); err != nil {
		t.Fatal(err)
	}
	var sawB bool
	for _, c := range mustCandidates(t, p) {
		if c.FromID == "cpu_utilization@pod:a" {
			t.Error("candidate for a forgotten property survived")
		}
		if c.FromID == "cpu_utilization@pod:b" {
			sawB = true
		}
	}
	if !sawB {
		t.Error("expected pod:b's candidate to survive the forget of pod:a")
	}
	h, _ := p.GetHistory()
	for _, c := range h {
		if c.FromID == "cpu_utilization@pod:a" {
			t.Error("history for a forgotten property survived")
		}
	}
}

// feedPendingCap feeds three pod series against a shared "node" series. pod:1
// and pod:2 are exact affine transforms of node (r = 1.0); pod:3 carries a
// periodic deviation so its |r| is clearly below the other two while staying
// above the 0.5 threshold — it is the genuinely weakest of the three.
func feedPendingCap(p *minimal.MICorrelationProposer) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 30; i++ {
		at := t0.Add(time.Duration(i) * 10 * time.Second)
		x := float64(i%10) / 10
		_ = p.ObserveProperty(types.PropertyReading{ID: "m@pod:1", Subject: "pod:1", Value: x, At: at})
		_ = p.ObserveProperty(types.PropertyReading{ID: "m@pod:2", Subject: "pod:2", Value: x * 0.9, At: at})
		_ = p.ObserveProperty(types.PropertyReading{ID: "m@pod:3", Subject: "pod:3", Value: x*0.7 + 0.1 + 0.35*math.Sin(float64(i)), At: at})
		_ = p.ObserveProperty(types.PropertyReading{ID: "node", Subject: "", Value: x, At: at})
	}
}

func TestPendingCapDefersTheWeakest(t *testing.T) {
	p := minimal.NewMICorrelationProposer(subjectPairsKnown{}, 0.5, 10, 60, 15*time.Second)
	p.SetMaxPending(2)
	feedPendingCap(p)

	cs := mustCandidates(t, p)
	if len(cs) > 2 {
		t.Errorf("pending=%d exceeds the cap of 2", len(cs))
	}
	pendingFrom := map[string]bool{}
	for _, c := range cs {
		pendingFrom[c.FromID] = true
	}
	if !pendingFrom["m@pod:1"] || !pendingFrom["m@pod:2"] {
		t.Errorf("expected the two strong candidates (m@pod:1, m@pod:2) to remain pending; got %v", pendingFrom)
	}

	h, _ := p.GetHistory()
	var deferredCount int
	var deferredFrom string
	for _, c := range h {
		if c.Status == types.Deferred {
			deferredCount++
			deferredFrom = c.FromID
		}
	}
	if deferredCount != 1 {
		t.Fatalf("expected exactly 1 deferred candidate; got %d", deferredCount)
	}
	if deferredFrom != "m@pod:3" {
		t.Errorf("expected the weaker m@pod:3 candidate to be deferred; got FromID=%q", deferredFrom)
	}
}

// buildCapScenario feeds three pod series and one node series at a shared
// cadence with maxPending=2, so the third candidate forces one deferral.
//
// With affine=true the pods are distinct affine transforms of node. Pearson is
// affine-invariant, so their |r| are equal in exact arithmetic — but not in
// float64: at the moment the cap fires the three values sit one ULP apart, and
// which one rounds lowest depends on the architecture's arithmetic (arm64 fuses
// the multiply-add in the covariance sums; amd64 at GOAMD64=v1 does not). On
// arm64 pod:3 rounds lowest, on amd64 pod:2 does. The CandidateID tie-break
// never engages for such series, so no test may assert which of them is
// deferred.
//
// With affine=false the three pods report the bit-identical series, which is
// the only way to reach the tie-break: equal bits on every platform, broken by
// the lexicographically larger CandidateID.
func buildCapScenario(affine bool) *minimal.MICorrelationProposer {
	p := minimal.NewMICorrelationProposer(subjectPairsKnown{}, 0.5, 10, 60, 15*time.Second)
	p.SetMaxPending(2)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 30; i++ {
		at := t0.Add(time.Duration(i) * 10 * time.Second)
		x := float64(i%10) / 10
		x2, x3 := x, x
		if affine {
			x2, x3 = x*0.9, x*0.7+0.1
		}
		_ = p.ObserveProperty(types.PropertyReading{ID: "m@pod:1", Subject: "pod:1", Value: x, At: at})
		_ = p.ObserveProperty(types.PropertyReading{ID: "m@pod:2", Subject: "pod:2", Value: x2, At: at})
		_ = p.ObserveProperty(types.PropertyReading{ID: "m@pod:3", Subject: "pod:3", Value: x3, At: at})
		_ = p.ObserveProperty(types.PropertyReading{ID: "node", Subject: "", Value: x, At: at})
	}
	return p
}

func singleDeferredID(t *testing.T, p *minimal.MICorrelationProposer) string {
	t.Helper()
	h, _ := p.GetHistory()
	var ids []string
	for _, c := range h {
		if c.Status == types.Deferred {
			ids = append(ids, c.CandidateID)
		}
	}
	if len(ids) != 1 {
		t.Fatalf("expected exactly 1 deferred candidate; got %d (%v)", len(ids), ids)
	}
	return ids[0]
}

// TestPendingCapIsDeterministic rebuilds the affine cap scenario twice and
// checks that the same candidate is deferred both times. It deliberately does
// not say which: that is decided by one ULP of rounding and differs between
// architectures (see buildCapScenario). What it guards is the original defect
// — the weakest candidate chosen by ranging over a map, which varied run to run.
func TestPendingCapIsDeterministic(t *testing.T) {
	d1 := singleDeferredID(t, buildCapScenario(true))
	d2 := singleDeferredID(t, buildCapScenario(true))
	if d1 != d2 {
		t.Errorf("two identical runs deferred different candidates: %q vs %q", d1, d2)
	}
}

// TestPendingCapTieBreaksOnCandidateID reaches the tie-break with bit-identical
// pod series, so the |r|·n scores are equal on every platform and the
// lexicographically larger CandidateID must be the one deferred.
func TestPendingCapTieBreaksOnCandidateID(t *testing.T) {
	d := singleDeferredID(t, buildCapScenario(false))
	if d != "m@pod:3->node" {
		t.Errorf("expected the tie-break to defer %q; got %q", "m@pod:3->node", d)
	}
}

// TestConfirmedCandidateIsNeverReemitted covers finding 1: a Confirmed candidate
// must never be overwritten by a fresh Pending entry, however the correlation
// evolves afterward (including a sign flip — coverage is sign-specific, so a
// flipped correlation is exactly the case that reaches the switch again).
func TestConfirmedCandidateIsNeverReemitted(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 60, 15*time.Second)
	feedScoped(p, 40, 2*time.Second)

	var cid string
	for _, c := range mustCandidates(t, p) {
		if c.FromID == "cpu_utilization@pod:a" && !c.Undirected {
			cid = c.CandidateID
		}
	}
	if cid == "" {
		t.Fatal("expected a candidate for cpu_utilization@pod:a before confirming")
	}
	if _, err := p.Confirm(cid, "", types.CandidateReview{}); err != nil {
		t.Fatalf("Confirm: %v", err)
	}

	// Keep feeding a strongly correlated stream for the confirmed pair, using
	// fresh timestamps so the folds are not deduped away.
	t1 := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 40; i++ {
		at := t1.Add(time.Duration(i) * 10 * time.Second)
		x := float64(i%20) / 20
		_ = p.ObserveProperty(types.PropertyReading{ID: "cpu_utilization@pod:a", Subject: "pod:a", Value: x, At: at})
		_ = p.ObserveProperty(types.PropertyReading{ID: "cpu_pressure_ratio", Subject: "", Value: 0.9*x + 0.05, At: at.Add(2 * time.Second)})
	}
	// Then a sign-flipped stream for the same pair.
	t2 := time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 40; i++ {
		at := t2.Add(time.Duration(i) * 10 * time.Second)
		x := float64(i%20) / 20
		_ = p.ObserveProperty(types.PropertyReading{ID: "cpu_utilization@pod:a", Subject: "pod:a", Value: x, At: at})
		_ = p.ObserveProperty(types.PropertyReading{ID: "cpu_pressure_ratio", Subject: "", Value: -(0.9*x + 0.05), At: at.Add(2 * time.Second)})
	}

	hist, _ := p.GetHistory()
	var occurrences int
	var status types.CandidateStatus
	for _, c := range hist {
		if c.CandidateID == cid {
			occurrences++
			status = c.Status
		}
	}
	if occurrences != 1 {
		t.Errorf("expected exactly 1 history entry for %q; got %d", cid, occurrences)
	}
	if status != types.Confirmed {
		t.Errorf("expected status Confirmed; got %v", status)
	}
	for _, c := range mustCandidates(t, p) {
		if c.CandidateID == cid {
			t.Errorf("confirmed candidate %q reappeared among pending candidates", cid)
		}
	}
}

// TestPendingCandidateDirectionFollowsTheEvidence: a Pending candidate is refreshed on
// every fold. Its direction is evidence like its |r| and n, so once the window has
// turned over to the opposite sign the candidate must say so — otherwise Confirm hands
// the map a sign the machine contradicts, and the map refuses to reverse it later.
func TestPendingCandidateDirectionFollowsTheEvidence(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 6, 8, 15*time.Second)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	feed := func(start time.Time, n int, slope float64) {
		for i := 0; i < n; i++ {
			at := start.Add(time.Duration(i) * 10 * time.Second)
			x := float64(i%10) / 10
			_ = p.ObserveProperty(types.PropertyReading{ID: "cpu@pod:a", Subject: "pod:a", Value: x, At: at})
			_ = p.ObserveProperty(types.PropertyReading{ID: "pressure", Subject: "", Value: 0.5 + slope*x, At: at})
		}
	}
	feed(t0, 12, 0.4)
	cs := mustCandidates(t, p)
	if len(cs) != 1 || cs[0].Direction != types.Positive {
		t.Fatalf("after a positive stream: %d candidates, first %+v; want one Positive", len(cs), cs)
	}
	feed(t0.Add(time.Hour), 24, -0.4)
	cs = mustCandidates(t, p)
	if len(cs) != 1 {
		t.Fatalf("after the sign flipped: %d candidates; want the same one", len(cs))
	}
	if cs[0].Direction != types.Negative {
		t.Errorf("direction %v with |r|=%.3f over n=%d after the window turned negative; the pending candidate froze its first direction",
			cs[0].Direction, cs[0].MIScore, cs[0].NObservations)
	}
}

// TestEachObservationPairsOnce: at a shared cadence each tick used to fold two pairs —
// (pod_i, node_{i-1}) when the pod arrived and (pod_i, node_i) when the node did — so
// n counted every reading twice, min-pairs was reached in half the advertised ticks
// and the Fisher p was computed over doubled, autocorrelated support. An observation
// takes part in at most one pair per counterpart.
func TestEachObservationPairsOnce(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.5, 5, 60, 15*time.Second)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		at := t0.Add(time.Duration(i) * 10 * time.Second)
		x := float64(i) / 10
		_ = p.ObserveProperty(types.PropertyReading{ID: "cpu@pod:a", Subject: "pod:a", Value: x, At: at})
		_ = p.ObserveProperty(types.PropertyReading{ID: "pressure", Subject: "", Value: x, At: at})
	}
	cs := mustCandidates(t, p)
	if len(cs) != 1 {
		t.Fatalf("%d candidates; want 1", len(cs))
	}
	if cs[0].NObservations != 10 {
		t.Errorf("n=%d after 10 ticks of one pod and one node reading each; want 10, one pair per tick", cs[0].NObservations)
	}
}

// TestCapDeferralIsRecordedAndReentersWhenItOutranksThePending: a cap deferral is the
// proposer's choice, not an operator's, so history says so and a log line names it;
// and it is not permanent — when the deferred pair later outranks a pending one, the
// two swap, so the cap bounds what is shown rather than what can ever be seen.
func TestCapDeferralIsRecordedAndReentersWhenItOutranksThePending(t *testing.T) {
	p := minimal.NewMICorrelationProposer(subjectPairsKnown{}, 0.5, 5, 60, 15*time.Second)
	p.SetMaxPending(1)
	var logged []string
	p.SetLogger(func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) })
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tick := func(i int, pods ...string) {
		at := t0.Add(time.Duration(i) * 10 * time.Second)
		x := float64(i%10) / 10
		for _, pod := range pods {
			_ = p.ObserveProperty(types.PropertyReading{ID: "m@" + pod, Subject: pod, Value: x, At: at})
		}
		_ = p.ObserveProperty(types.PropertyReading{ID: "node", Subject: "", Value: x, At: at})
	}
	byID := func() map[string]*types.CandidateEdge {
		h, _ := p.GetHistory()
		out := map[string]*types.CandidateEdge{}
		for _, c := range h {
			out[c.CandidateID] = c
		}
		return out
	}
	for i := 0; i < 6; i++ {
		tick(i, "pod:a", "pod:b")
	}
	m := byID()
	a, b := m["m@pod:a->node"], m["m@pod:b->node"]
	if a == nil || b == nil || a.Status != types.Pending || b.Status != types.Deferred {
		t.Fatalf("after the cap: a=%+v b=%+v; want a pending and b deferred by the tie-break", a, b)
	}
	if b.Reason != "cap" {
		t.Errorf("deferred candidate carries reason %q; want \"cap\" so history can tell it from an operator's Defer", b.Reason)
	}
	if len(logged) == 0 || !strings.Contains(logged[0], "m@pod:b->node") {
		t.Errorf("the cap deferral was not logged: %v", logged)
	}
	for i := 6; i < 20; i++ {
		tick(i, "pod:b") // pod:a falls silent; pod:b keeps reporting and outgrows it
	}
	m = byID()
	a, b = m["m@pod:a->node"], m["m@pod:b->node"]
	if b.Status != types.Pending {
		t.Errorf("pod:b stayed %v with |r|·n=%.1f against pod:a's %.1f; a cap deferral must re-enter when it outranks the pending",
			b.Status, b.MIScore*float64(b.NObservations), a.MIScore*float64(a.NObservations))
	}
	if a.Status != types.Deferred || a.Reason != "cap" {
		t.Errorf("pod:a is %v/%q; want cap-deferred in pod:b's place", a.Status, a.Reason)
	}
}

// TestOperatorDeferralDoesNotReenter: an operator's Defer is a decision the proposer
// does not overturn, however the evidence grows.
func TestOperatorDeferralDoesNotReenter(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 60, 15*time.Second)
	feedScoped(p, 20, 2*time.Second)
	cs := mustCandidates(t, p)
	if len(cs) == 0 {
		t.Fatal("no candidate to defer")
	}
	id := cs[0].CandidateID
	if err := p.Defer(id, types.CandidateReview{}); err != nil {
		t.Fatal(err)
	}
	feedScoped(p, 40, 2*time.Second)
	h, _ := p.GetHistory()
	for _, c := range h {
		if c.CandidateID == id && (c.Status != types.Deferred || c.Reason != "operator") {
			t.Errorf("operator-deferred candidate is %v/%q after more evidence; want Deferred/operator", c.Status, c.Reason)
		}
	}
}

// TestSettledCandidatesCannotBeRejectedOrDeferred: a Confirmed candidate has become a
// relationship in the map; letting history later say "rejected" about it would
// contradict the map.
func TestSettledCandidatesCannotBeRejectedOrDeferred(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 60, 15*time.Second)
	feedScoped(p, 40, 2*time.Second)
	cs := mustCandidates(t, p)
	if len(cs) == 0 {
		t.Fatal("no candidate to confirm")
	}
	id := cs[0].CandidateID
	if _, err := p.Confirm(id, "", types.CandidateReview{}); err != nil {
		t.Fatal(err)
	}
	if err := p.Reject(id, types.CandidateReview{}); err == nil {
		t.Error("a Confirmed candidate was rejected")
	}
	if err := p.Defer(id, types.CandidateReview{}); err == nil {
		t.Error("a Confirmed candidate was deferred")
	}
	h, _ := p.GetHistory()
	for _, c := range h {
		if c.CandidateID == id && c.Status != types.Confirmed {
			t.Errorf("status %v after the refused transitions; want Confirmed", c.Status)
		}
	}
}

// TestObserveProperty_PairsNodePropertiesUndirected: two node-level properties that
// correlate become one undirected candidate, whichever arrives first, and a subject
// still pairs with each of them, directed.
func TestObserveProperty_PairsNodePropertiesUndirected(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.5, 5, 60, 15*time.Second)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 20; i++ {
		at := t0.Add(time.Duration(i) * 10 * time.Second)
		x := float64(i%10) / 10
		_ = p.ObserveProperty(types.PropertyReading{ID: "cpu@pod:a", Subject: "pod:a", Value: x, At: at})
		// Alternate arrival order: the pair must still fold into one buffer.
		if i%2 == 0 {
			_ = p.ObserveProperty(types.PropertyReading{ID: "pressure", Value: x, At: at})
			_ = p.ObserveProperty(types.PropertyReading{ID: "load", Value: 0.5 * x, At: at})
		} else {
			_ = p.ObserveProperty(types.PropertyReading{ID: "load", Value: 0.5 * x, At: at})
			_ = p.ObserveProperty(types.PropertyReading{ID: "pressure", Value: x, At: at})
		}
	}
	h, _ := p.GetHistory()
	var scoped, undirected int
	for _, c := range h {
		switch {
		case c.Undirected:
			undirected++
			if c.CandidateID != "load<->pressure" || c.FromID != "load" || c.ToID != "pressure" {
				t.Errorf("undirected candidate %s (%s, %s); want load<->pressure in lexicographic order", c.CandidateID, c.FromID, c.ToID)
			}
			if c.NObservations != 20 {
				t.Errorf("n=%d; both arrival orders must fold into one buffer of 20 pairs", c.NObservations)
			}
		case strings.Contains(c.FromID, "@"):
			scoped++
		}
	}
	if undirected != 1 {
		t.Errorf("%d undirected candidates; want exactly one for the node pair", undirected)
	}
	if scoped != 2 {
		t.Errorf("%d scoped candidates; want pod:a paired with both node properties", scoped)
	}
}

// coveredOneWay covers exactly from -> to with the given sign.
type coveredOneWay struct {
	from, to string
	sign     int
}

func (c coveredOneWay) Covered(from, to string, sign int) bool {
	return from == c.from && to == c.to && sign == c.sign
}

var unit = [2]float64{0, 1}

// feedNodePair drives two node-level properties with the given series.
func feedNodePair(p *minimal.MICorrelationProposer, n int, a, b func(i int) float64, rngA, rngB *[2]float64) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		at := t0.Add(time.Duration(i) * 10 * time.Second)
		_ = p.ObserveProperty(types.PropertyReading{ID: "a", Value: a(i), At: at, Range: rngA})
		_ = p.ObserveProperty(types.PropertyReading{ID: "b", Value: b(i), At: at, Range: rngB})
	}
}

// TestVariationGate: correlation is scale-free, so a series that moves 0.05% of its
// declared range still correlates perfectly. The gate holds such a pair back, lets a
// pair that moves through it, and states that it could not check an end with no
// declared range rather than passing it silently.
func TestVariationGate(t *testing.T) {
	big := func(i int) float64 { return float64(i%10) / 10 }       // moves 90% of [0,1]
	tiny := func(i int) float64 { return 0.5 + float64(i%10)/2e4 } // moves 0.045% of [0,1]

	for _, tc := range []struct {
		name       string
		a, b       func(int) float64
		rngA, rngB *[2]float64
		want       bool
		caveats    int
	}{
		{"both move", big, big, &unit, &unit, true, 0},
		{"one barely moves", big, tiny, &unit, &unit, false, 0},
		{"range undeclared: not checked, and said so", big, tiny, &unit, nil, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := minimal.NewMICorrelationProposer(coveredNone{}, 0.5, 5, 60, 15*time.Second)
			feedNodePair(p, 20, tc.a, tc.b, tc.rngA, tc.rngB)
			cs, _ := p.GetCandidates()
			if got := len(cs) == 1; got != tc.want {
				t.Fatalf("candidates=%d; want proposed=%v", len(cs), tc.want)
			}
			if tc.want && len(cs[0].Caveats) != tc.caveats {
				t.Errorf("caveats %q; want %d", cs[0].Caveats, tc.caveats)
			}
		})
	}

	t.Run("0 disables the gate", func(t *testing.T) {
		p := minimal.NewMICorrelationProposer(coveredNone{}, 0.5, 5, 60, 15*time.Second)
		p.SetMinSpread(0)
		feedNodePair(p, 20, big, tiny, &unit, &unit)
		if cs, _ := p.GetCandidates(); len(cs) != 1 {
			t.Errorf("candidates=%d with the gate off; want 1", len(cs))
		}
	})
}

// TestCompositionFlag: a subject's metric against the node total of the same metric
// is part against whole. It is still proposed, marked as such, and a different metric
// is not marked.
// Readings of one tick arrive one after another while the previous tick's are still
// inside the pair window. After a missed reading the first arrival of the next tick
// finds the other's previous value unpaired and folds it; the same-tick reading that
// follows must replace that pair, or the window stays one interval out of step for
// good — and on an alternating series a one-interval lag reads as r = -1.
func TestPairsReadingsOfTheSameTickAfterAGap(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.5, 5, 60, 15*time.Second)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 20; i++ {
		at := t0.Add(time.Duration(i) * 10 * time.Second)
		x := float64(i % 2)
		_ = p.ObserveProperty(types.PropertyReading{ID: "cpu@pod:a", Subject: "pod:a", Value: x, At: at})
		if i != 5 { // one failed read
			_ = p.ObserveProperty(types.PropertyReading{ID: "memory", Value: x, At: at})
		}
		_ = p.ObserveProperty(types.PropertyReading{ID: "cpu", Value: x, At: at})
	}
	h, _ := p.GetHistory()
	if len(h) == 0 {
		t.Fatal("no candidates")
	}
	for _, c := range h {
		if c.Direction != types.Positive || c.MIScore < 0.99 {
			t.Errorf("%s: direction %v |r|=%.3f; same-tick readings must pair, giving r = +1", c.CandidateID, c.Direction, c.MIScore)
		}
		if c.NObservations < 19 {
			t.Errorf("%s: n=%d; want one pair per tick with both readings", c.CandidateID, c.NObservations)
		}
	}
}

func TestCompositionFlag(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.5, 5, 60, 15*time.Second)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 20; i++ {
		at := t0.Add(time.Duration(i) * 10 * time.Second)
		x := float64(i%10) / 10
		_ = p.ObserveProperty(types.PropertyReading{ID: "cpu_utilization@pod:a", Subject: "pod:a", Value: x, At: at, Range: &unit})
		_ = p.ObserveProperty(types.PropertyReading{ID: "cpu_utilization", Value: 0.2 + 0.5*x, At: at, Range: &unit})
		_ = p.ObserveProperty(types.PropertyReading{ID: "cpu_pressure_ratio", Value: 0.1 + 0.3*x, At: at, Range: &unit})
	}
	cs, _ := p.GetCandidates()
	kinds := map[string]string{}
	for _, c := range cs {
		kinds[c.CandidateID] = c.Kind
	}
	if k, ok := kinds["cpu_utilization@pod:a->cpu_utilization"]; !ok || k != types.CandidateKindComposition {
		t.Errorf("pod CPU -> node CPU: kind %q (proposed=%v); want %q", k, ok, types.CandidateKindComposition)
	}
	if k, ok := kinds["cpu_utilization@pod:a->cpu_pressure_ratio"]; !ok || k != "" {
		t.Errorf("pod CPU -> node pressure: kind %q (proposed=%v); want an ordinary candidate", k, ok)
	}
	prop, err := p.Confirm("cpu_utilization@pod:a->cpu_utilization", "", types.CandidateReview{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prop.Description, "composition") {
		t.Errorf("confirmed composition's description %q does not say so", prop.Description)
	}
}

// TestUndirectedPairCoveredEitherWay: a relationship declared b -> a covers the a<->b
// candidate, since the candidate claims no direction; a directed candidate is still
// covered only by its own direction.
func TestUndirectedPairCoveredEitherWay(t *testing.T) {
	x := func(i int) float64 { return float64(i%10) / 10 }
	p := minimal.NewMICorrelationProposer(coveredOneWay{"b", "a", 1}, 0.5, 5, 60, 15*time.Second)
	feedNodePair(p, 20, x, x, &unit, &unit)
	if cs, _ := p.GetCandidates(); len(cs) != 0 {
		t.Errorf("b -> a is declared; the undirected a<->b was still proposed: %v", cs[0].CandidateID)
	}
}

// TestConfirmOrientation: a directed candidate is confirmed as proposed and refuses
// its target as the source; an undirected one runs from whichever end is named.
func TestConfirmOrientation(t *testing.T) {
	x := func(i int) float64 { return float64(i%10) / 10 }
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.5, 5, 60, 15*time.Second)
	feedNodePair(p, 20, x, x, &unit, &unit)
	if _, err := p.Confirm("a<->b", "b", types.CandidateReview{}); err != nil {
		t.Fatalf("undirected confirm from b: %v", err)
	}

	q := minimal.NewMICorrelationProposer(coveredNone{}, 0.5, 5, 60, 15*time.Second)
	feedScoped(q, 40, 0)
	cs, _ := q.GetCandidates()
	if len(cs) == 0 {
		t.Fatal("no scoped candidate to confirm")
	}
	if _, err := q.Confirm(cs[0].CandidateID, cs[0].ToID, types.CandidateReview{}); !errors.Is(err, contracts.ErrBadConfirmation) {
		t.Errorf("directed candidate confirmed from its target: %v; want ErrBadConfirmation", err)
	}
	if _, err := q.Confirm(cs[0].CandidateID, cs[0].FromID, types.CandidateReview{}); err != nil {
		t.Errorf("directed candidate confirmed from its source: %v", err)
	}
}

// feedRationing drives five node-level properties: two that never move, one that
// moves without following anything, and two that move together. Their names put the
// quiet ones first, so while the budget has room they take it.
func feedRationing(p *minimal.MICorrelationProposer, from, to int) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rng := &[2]float64{0, 1}
	for i := from; i < to; i++ {
		at := t0.Add(time.Duration(i) * 10 * time.Second)
		x := float64(i%10) / 10
		for _, r := range []types.PropertyReading{
			{ID: "a_quiet", Value: 0.5},
			{ID: "b_quiet", Value: 0.5},
			{ID: "c_noise", Value: 0.5 + 0.4*math.Sin(float64(i)*1.7)},
			{ID: "x", Value: x},
			{ID: "y", Value: 0.2 + 0.7*x},
		} {
			r.At, r.Range = at, rng
			_ = p.ObserveProperty(r)
		}
	}
}

// TestBudgetRationsPairsToTheMostVariable: a budget of three is filled first by the
// quiet properties, which arrive first. Once those pairs have held matureAt pairs
// below the threshold they are displaced, and the moving pair is found.
func TestBudgetRationsPairsToTheMostVariable(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 300, 15*time.Second)
	p.SetMaxPairs(3)
	feedRationing(p, 0, 120)

	d, _ := p.Discovery()
	if d.Budget != 3 || d.TestedPairs > 3 {
		t.Errorf("budget %d, tested %d; the budget must hold", d.Budget, d.TestedPairs)
	}
	if d.Properties != 5 || d.EligiblePairs != 10 {
		t.Errorf("properties %d, eligible %d; want 5 and 10", d.Properties, d.EligiblePairs)
	}
	if d.Displaced == 0 {
		t.Error("nothing displaced: the quiet pairs kept the budget")
	}
	h, _ := p.GetHistory()
	var found *types.CandidateEdge
	for _, c := range h {
		if strings.Contains(c.CandidateID, "quiet") {
			t.Errorf("a property that never moved reached a candidate: %s", c.CandidateID)
		}
		if c.CandidateID == "x<->y" {
			found = c
		}
	}
	if found == nil {
		t.Fatalf("x<->y never proposed; history %d", len(h))
	}

	// A pair at or above the threshold keeps its slot: it goes on accumulating.
	n := found.NObservations
	feedRationing(p, 120, 200)
	for _, c := range mustCandidates(t, p) {
		if c.CandidateID == "x<->y" && c.NObservations <= n {
			t.Errorf("x<->y stayed at n=%d after 80 more ticks; a candidate's pair must not be displaced", c.NObservations)
		}
	}
}

// TestSettledPairStopsBeingTested: rejecting a candidate frees its slot at the next
// rationing pass, and the pair is not admitted again.
func TestSettledPairStopsBeingTested(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 60, 15*time.Second)
	p.SetMaxPairs(1)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tick := func(i int) {
		at := t0.Add(time.Duration(i) * 10 * time.Second)
		x := float64(i%10) / 10
		_ = p.ObserveProperty(types.PropertyReading{ID: "x", Value: x, At: at})
		_ = p.ObserveProperty(types.PropertyReading{ID: "y", Value: x, At: at})
	}
	for i := 0; i < 20; i++ {
		tick(i)
	}
	if err := p.Reject("x<->y", types.CandidateReview{}); err != nil {
		t.Fatal(err)
	}
	for i := 20; i < 25; i++ {
		tick(i)
	}
	if d, _ := p.Discovery(); d.TestedPairs != 0 {
		t.Errorf("tested %d pairs after the only pair was rejected; want 0", d.TestedPairs)
	}
}

// TestForgetFreesTheBudget: a departed property's pairs leave the budget.
func TestForgetFreesTheBudget(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 60, 15*time.Second)
	feedScoped(p, 5, 2*time.Second)
	before, _ := p.Discovery()
	_ = p.Forget("cpu_utilization@pod:a")
	after, _ := p.Discovery()
	if before.TestedPairs != 3 || after.TestedPairs != 1 {
		t.Errorf("tested %d before and %d after forgetting pod:a; want 3 and 1", before.TestedPairs, after.TestedPairs)
	}
}

// TestReplayedReadingIsNotFoldedTwice: the property path keeps no identities per
// pair; a reading no newer than its property's last is what it refuses instead.
func TestReplayedReadingIsNotFoldedTwice(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.5, 5, 60, 15*time.Second)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var batch []types.PropertyReading
	for i := 0; i < 10; i++ {
		at := t0.Add(time.Duration(i) * 10 * time.Second)
		x := float64(i % 2)
		batch = append(batch, types.PropertyReading{ID: "x", Value: x, At: at}, types.PropertyReading{ID: "y", Value: x, At: at})
	}
	for range 2 { // the whole batch, then the same batch again
		for _, r := range batch {
			_ = p.ObserveProperty(r)
		}
	}
	h, _ := p.GetHistory()
	if len(h) != 1 || h[0].NObservations != 10 {
		t.Errorf("history %d candidates, n=%v; want one with n=10", len(h), h)
	}
}

func TestDerivePairBudget(t *testing.T) {
	const gib = 1 << 30
	pi := minimal.DerivePairBudget(8*gib, 4, 10*time.Second, 120)
	vm := minimal.DerivePairBudget(2*gib, 2, 10*time.Second, 120)
	if pi <= vm {
		t.Errorf("an 8 GiB, 4-core machine got %d pairs and a 2 GiB, 2-core one %d; more machine must allow more", pi, vm)
	}
	if want := int(0.01 * 2 * gib / float64(minimal.PairBytes(120))); vm != want {
		t.Errorf("2 GiB: %d pairs; memory allows %d and should bind", vm, want)
	}
	if fast := minimal.DerivePairBudget(64*gib, 1, 100*time.Millisecond, 120); fast != 200 {
		t.Errorf("one core at a 100ms tick: %d pairs; CPU allows 200 and should bind", fast)
	}
	if got := minimal.DerivePairBudget(0, 4, 10*time.Second, 120); got != minimal.FallbackPairBudget {
		t.Errorf("unreadable memory: %d; want the fallback", got)
	}
}

// feedUncorrelated drives node-level properties that each move with their own
// amplitude and frequency, so no pair correlates: every tested pair matures weak.
func feedUncorrelated(p *minimal.MICorrelationProposer, amp map[string]float64, from, to int) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rng := &[2]float64{0, 1}
	ids := make([]string, 0, len(amp))
	for id := range amp {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for i := from; i < to; i++ {
		at := t0.Add(time.Duration(i) * 10 * time.Second)
		for k, id := range ids {
			v := 0.5 + amp[id]*math.Sin(float64(i)*(1.3+0.71*float64(k)))
			_ = p.ObserveProperty(types.PropertyReading{ID: id, Value: v, At: at, Range: rng})
		}
	}
}

// TestDisplacementRespectsRank: a weak pair of the most variable properties is not
// displaced by a pair that varies less, so the budget settles rather than rotating.
func TestDisplacementRespectsRank(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 60, 15*time.Second)
	p.SetMaxPairs(1)
	// "a_lo"/"b_lo" sort first, so the low pair takes the only slot; the high pair
	// displaces it once it matures, and nothing displaces the high pair after.
	feedUncorrelated(p, map[string]float64{"a_lo": 0.05, "b_lo": 0.05, "c_hi": 0.45, "d_hi": 0.45}, 0, 400)
	d, _ := p.Discovery()
	if d.Displaced != 1 {
		t.Errorf("displaced %d times over 400 ticks; want once, the low pair making way for the high one", d.Displaced)
	}
}

// TestQuietPairIsDisplacedAndItsCandidateDeferred: a pair whose property stopped
// reporting holds no claim on the budget, mature or not, and a pending candidate it
// backed leaves the queue as untested rather than standing with frozen numbers.
func TestQuietPairIsDisplacedAndItsCandidateDeferred(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 60, 15*time.Second)
	p.SetMaxPairs(1)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rng := &[2]float64{0, 1}
	for i := 0; i < 40; i++ {
		at := t0.Add(time.Duration(i) * 10 * time.Second)
		x := float64(i%10) / 10
		_ = p.ObserveProperty(types.PropertyReading{ID: "x", Value: x, At: at, Range: rng})
		if i < 20 { // y stops reporting
			_ = p.ObserveProperty(types.PropertyReading{ID: "y", Value: x, At: at, Range: rng})
		}
		_ = p.ObserveProperty(types.PropertyReading{ID: "z", Value: 1 - x, At: at, Range: rng})
	}
	h, _ := p.GetHistory()
	byID := map[string]*types.CandidateEdge{}
	for _, c := range h {
		byID[c.CandidateID] = c
	}
	if c := byID["x<->y"]; c == nil || c.Status != types.Deferred || c.Reason != "untested" {
		t.Errorf("x<->y after y fell silent: %+v; want deferred as untested", c)
	}
	if c := byID["x<->z"]; c == nil || c.Status != types.Pending {
		t.Errorf("x<->z: %+v; the freed slot should have found it", c)
	}
	d, _ := p.Discovery()
	if d.TestedPairs > d.EligiblePairs || d.TestedPairs+d.IdlePairs > d.Budget {
		t.Errorf("discovery %+v: tested above eligible, or above the budget", d)
	}
}

// TestDiscoveryCountsIdlePairsApart: pairs whose ends went quiet are reported as
// idle, so tested pairs never exceed the eligible ones.
func TestDiscoveryCountsIdlePairsApart(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 60, 15*time.Second)
	feedUncorrelated(p, map[string]float64{"a": 0.3, "b": 0.3, "c": 0.3, "d": 0.3}, 0, 5)
	feedUncorrelated(p, map[string]float64{"a": 0.3}, 10, 12) // only a keeps reporting
	d, _ := p.Discovery()
	if d.Properties != 1 || d.EligiblePairs != 0 || d.TestedPairs != 0 || d.IdlePairs != 6 {
		t.Errorf("discovery %+v; want 1 property, 0 eligible, 0 tested, 6 idle", d)
	}
}

// feedChart feeds two properties of one chart (busy, and busy_avg moving with it at
// half the amplitude, as a smoothing would) and an unrelated load that follows busy
// with noise. coupled reports, per tick, whether busy_avg still tracks busy.
func feedChart(p *minimal.MICorrelationProposer, chart string, from, to int, coupled func(i int) bool) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rng := &[2]float64{0, 1}
	for i := from; i < to; i++ {
		at := t0.Add(time.Duration(i) * 10 * time.Second)
		x := float64(i%10) / 10
		avg := 0.25 + 0.5*x
		if !coupled(i) {
			avg = 0.5 + 0.3*math.Sin(float64(i)*2.9)
		}
		for _, r := range []types.PropertyReading{
			{ID: "busy", Chart: chart, Value: x},
			{ID: "busy_avg", Chart: chart, Value: avg},
			{ID: "load", Value: 0.1 + 0.6*x + 0.05*math.Sin(float64(i)*1.3)},
		} {
			r.At, r.Range = at, rng
			_ = p.ObserveProperty(r)
		}
	}
}

func always(int) bool { return true }

// TestChartSurvivesAReadingWithoutOne: a property fed by two producers, one naming its
// chart and one not, keeps the chart. Taking each reading's chart as given made the
// property leave and rejoin its group every tick.
func TestChartSurvivesAReadingWithoutOne(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 120, 15*time.Second)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 60; i++ {
		feedChart(p, "system.cpu", i, i+1, always)
		// The second producer's reading of busy, a second later and with no chart.
		x := float64(i%10) / 10
		at := t0.Add(time.Duration(i)*10*time.Second + time.Second)
		_ = p.ObserveProperty(types.PropertyReading{ID: "busy", Value: x, At: at, Range: &[2]float64{0, 1}})
	}
	if d, _ := p.Discovery(); len(d.Groups) != 1 || d.Groups[0].Chart != "system.cpu" {
		t.Errorf("groups %+v with busy also read without a chart; want the system.cpu group to stand", d.Groups)
	}
	for _, c := range mustCandidates(t, p) {
		if c.CandidateID == "busy<->busy_avg" {
			t.Error("busy<->busy_avg proposed: a fold after the chartless reading took the group's link for an ordinary pair")
		}
	}
}

// TestArithmeticGroupIsReportedOnceAndRepresented: two properties of one chart that
// move as one are a group, not a candidate. The more variable of them stands for the
// group: it is paired with load, and the other is not. Without a chart nothing says
// they are arithmetic, and their pair is proposed like any other.
func TestArithmeticGroupIsReportedOnceAndRepresented(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 120, 15*time.Second)
	feedChart(p, "system.cpu", 0, 60, always)

	d, _ := p.Discovery()
	if len(d.Groups) != 1 {
		t.Fatalf("groups %+v; want one", d.Groups)
	}
	g := d.Groups[0]
	if g.Chart != "system.cpu" || g.Representative != "busy" || !slices.Equal(g.Members, []string{"busy", "busy_avg"}) {
		t.Errorf("group %+v; want system.cpu {busy, busy_avg} represented by busy, the more variable", g)
	}
	if d.TestedPairs != 2 {
		t.Errorf("%d pairs tested; want 2: busy<->busy_avg links the group, busy<->load represents it", d.TestedPairs)
	}
	pending := map[string]bool{}
	for _, c := range mustCandidates(t, p) {
		pending[c.CandidateID] = true
	}
	if !pending["busy<->load"] {
		t.Errorf("busy<->load not proposed; pending %v", pending)
	}
	for _, id := range []string{"busy<->busy_avg", "busy_avg<->load"} {
		if pending[id] {
			t.Errorf("%s is pending; the group is reported once, through its representative", id)
		}
	}

	q := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 120, 15*time.Second)
	feedChart(q, "", 0, 60, always)
	if d, _ := q.Discovery(); len(d.Groups) != 0 {
		t.Errorf("groups %+v; series of no chart are never grouped", d.Groups)
	}
	var found bool
	for _, c := range mustCandidates(t, q) {
		found = found || c.CandidateID == "busy<->busy_avg"
	}
	if !found {
		t.Error("without a chart, busy<->busy_avg not proposed")
	}
}

// TestGroupBreaksWhenItsMembersPartWays: once busy_avg stops tracking busy, the link
// breaks after the window has turned over, and busy_avg is paired with load again.
func TestGroupBreaksWhenItsMembersPartWays(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 30, 15*time.Second)
	feedChart(p, "system.cpu", 0, 40, always)
	if d, _ := p.Discovery(); len(d.Groups) != 1 {
		t.Fatalf("groups %+v before parting; want one", d.Groups)
	}
	feedChart(p, "system.cpu", 40, 100, func(int) bool { return false })
	d, _ := p.Discovery()
	if len(d.Groups) != 0 {
		t.Errorf("groups %+v after busy_avg parted from busy; want none", d.Groups)
	}
	if d.TestedPairs != 3 {
		t.Errorf("%d pairs tested after the group broke; want all 3", d.TestedPairs)
	}
}

// TestGroupDefersTheCandidatesItTakesOver: a pending candidate from a member that
// stops representing its group is deferred as grouped, not left pending with numbers
// nothing updates.
func TestGroupDefersTheCandidatesItTakesOver(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 30, 15*time.Second)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rng := &[2]float64{0, 1}
	for i := 0; i < 60; i++ {
		at := t0.Add(time.Duration(i) * 10 * time.Second)
		x := float64(i%10) / 10
		// busy_avg tracks load from the start; busy joins its chart only after 20
		// ticks of noise, so busy_avg<->load is proposed before the group forms.
		busy := 0.5 + 0.4*math.Sin(float64(i)*2.9)
		if i >= 20 {
			busy = x
		}
		for _, r := range []types.PropertyReading{
			{ID: "busy", Chart: "system.cpu", Value: busy},
			{ID: "busy_avg", Chart: "system.cpu", Value: 0.25 + 0.5*x},
			{ID: "load", Value: 0.1 + 0.6*x + 0.05*math.Sin(float64(i)*1.3)},
		} {
			r.At, r.Range = at, rng
			_ = p.ObserveProperty(r)
		}
		if i == 19 {
			var found bool
			for _, c := range mustCandidates(t, p) {
				found = found || c.CandidateID == "busy_avg<->load"
			}
			if !found {
				t.Fatal("busy_avg<->load not proposed before the group formed")
			}
		}
	}
	d, _ := p.Discovery()
	if len(d.Groups) != 1 || d.Groups[0].Representative != "busy" {
		t.Fatalf("groups %+v; want one represented by busy", d.Groups)
	}
	h, _ := p.GetHistory()
	for _, c := range h {
		if c.CandidateID == "busy_avg<->load" && (c.Status != types.Deferred || c.Reason != "grouped") {
			t.Errorf("busy_avg<->load is %v (%q); want deferred as grouped", c.Status, c.Reason)
		}
	}
}

// TestBarCorrectsForThePairsTested: a pair that clears |r| on few readings is a
// candidate when it is the only pair tested, and not when hundreds of pairs that
// show nothing are tested beside it. A candidate that clears the bar reports it.
func TestBarCorrectsForThePairsTested(t *testing.T) {
	feed := func(p *minimal.MICorrelationProposer, noise int) {
		rnd := rand.New(rand.NewSource(7))
		t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		rng := &[2]float64{0, 1}
		for i := 0; i < 12; i++ {
			at := t0.Add(time.Duration(i) * 10 * time.Second)
			x := float64(i%10) / 10
			rs := []types.PropertyReading{
				{ID: "x", Value: x},
				{ID: "y", Value: 0.3 + 0.4*x + 0.16*math.Sin(float64(i)*2.3)},
			}
			for k := 0; k < noise; k++ {
				rs = append(rs, types.PropertyReading{ID: fmt.Sprintf("n%02d", k), Value: rnd.Float64()})
			}
			for _, r := range rs {
				r.At, r.Range = at, rng
				_ = p.ObserveProperty(r)
			}
		}
	}
	xy := func(p *minimal.MICorrelationProposer) *types.CandidateEdge {
		for _, c := range mustCandidates(t, p) {
			if c.CandidateID == "x<->y" {
				return c
			}
		}
		return nil
	}

	alone := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 120, 15*time.Second)
	feed(alone, 0)
	c := xy(alone)
	if c == nil {
		t.Fatal("x<->y not proposed when it is the only pair tested")
	}
	if c.PBar == 0 || c.PValue > c.PBar {
		t.Errorf("x<->y p=%.3g bar=%.3g; a candidate reports the bar it cleared", c.PValue, c.PBar)
	}

	crowded := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 120, 15*time.Second)
	feed(crowded, 30)
	d, _ := crowded.Discovery()
	if c := xy(crowded); c != nil {
		t.Errorf("x<->y proposed at p=%.3g among %d p-values; the bar is %.3g", c.PValue, d.PValuesTested, d.PBar)
	}
	if d.FDR != minimal.DefaultFDR || d.PValuesTested < 400 {
		t.Errorf("discovery fdr=%.2f over %d p-values; want the default rate over every pair", d.FDR, d.PValuesTested)
	}

	uncorrected := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 120, 15*time.Second)
	uncorrected.SetFDR(0)
	feed(uncorrected, 30)
	if c := xy(uncorrected); c == nil || c.PBar != 0 {
		t.Errorf("x<->y with the correction off: %+v; want proposed with no bar", c)
	}
}

// feedNoise feeds x and y, related, beside noise properties of pure noise, for ticks
// [from, to). y follows x loosely enough that ten readings make it marginal.
func feedNoise(p *minimal.MICorrelationProposer, seed int64, noise, from, to int, extra func(i int) []types.PropertyReading) {
	rnd := rand.New(rand.NewSource(seed))
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rng := &[2]float64{0, 1}
	for i := 0; i < to; i++ {
		x := float64(i%10) / 10
		rs := []types.PropertyReading{
			{ID: "x", Value: x},
			// The noise repeats every ten ticks, as x does, so every window of ten
			// readings holds the same |r|: what changes is only the bar.
			{ID: "y", Value: 0.3 + 0.4*x + 0.11*math.Sin(float64(i%10)*2.3)},
		}
		for k := 0; k < noise; k++ {
			rs = append(rs, types.PropertyReading{ID: fmt.Sprintf("n%02d", k), Value: rnd.Float64()})
		}
		if extra != nil {
			rs = append(rs, extra(i)...)
		}
		if i < from {
			continue // the noise generator still advances, so a later window continues the series
		}
		at := t0.Add(time.Duration(i) * 10 * time.Second)
		for _, r := range rs {
			r.At, r.Range = at, rng
			_ = p.ObserveProperty(r)
		}
	}
}

// TestBarCountsEveryPairFromTheFirstRound: in the first round of folds most pairs
// have no p-value yet. The bar must still count them, or the first pairs to reach
// enough readings are judged as if they were the only tests, and noise gets through.
func TestBarCountsEveryPairFromTheFirstRound(t *testing.T) {
	var noisy int
	for seed := int64(1); seed <= 20; seed++ {
		p := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 120, 15*time.Second)
		feedNoise(p, seed, 30, 0, 10, nil)
		for _, c := range mustCandidates(t, p) {
			if strings.HasPrefix(c.FromID, "n") || strings.HasPrefix(c.ToID, "n") {
				noisy++
				t.Logf("seed %d: %s pending at p=%.3g, bar %.3g", seed, c.CandidateID, c.PValue, c.PBar)
			}
		}
		if d, _ := p.Discovery(); d.PValuesTested != 496 || d.PBar > 0.001 {
			t.Errorf("seed %d: bar %.3g over %d pairs; want every one of 496 counted", seed, d.PBar, d.PValuesTested)
		}
	}
	if noisy > 1 {
		t.Errorf("%d noise pairs pending over 20 runs; at a 5%% rate about one is expected", noisy)
	}
}

// TestPendingCandidateLeavesWhenTheBarTightens: x<->y clears the bar while it is the
// only pair tested. Once thirty noise properties are tested beside it, on the same ten
// readings, it no longer does, and is deferred rather than left pending on numbers
// that stopped being true.
func TestPendingCandidateLeavesWhenTheBarTightens(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 10, 15*time.Second)
	feedNoise(p, 7, 0, 0, 12, nil)
	if len(mustCandidates(t, p)) != 1 {
		t.Fatal("x<->y not proposed alone")
	}
	feedNoise(p, 7, 30, 12, 40, nil)
	h, _ := p.GetHistory()
	for _, c := range h {
		if c.CandidateID == "x<->y" && (c.Status != types.Deferred || c.Reason != "bar") {
			t.Errorf("x<->y is %v (%q) at p=%.3g, bar %.3g; want deferred by the bar", c.Status, c.Reason, c.PValue, c.PBar)
		}
	}
}

// TestArithmeticDoesNotLoosenTheBar: forty-five links inside one chart are p ≈ 0 by
// construction. Counted as discoveries they would raise the bar for every real pair.
func TestArithmeticDoesNotLoosenTheBar(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 120, 15*time.Second)
	charted := func(i int) []types.PropertyReading {
		x := float64(i%7) / 7
		var rs []types.PropertyReading
		for k := 0; k < 10; k++ {
			rs = append(rs, types.PropertyReading{ID: fmt.Sprintf("c%d", k), Chart: "c", Value: 0.05 + float64(k)*0.01 + 0.5*x})
		}
		return rs
	}
	feedNoise(p, 3, 30, 0, 12, charted)
	d, _ := p.Discovery()
	if len(d.Groups) != 1 || d.PBar > 0.001 {
		t.Errorf("groups %d, bar %.3g over %d; the chart's links must not count as discoveries", len(d.Groups), d.PBar, d.PValuesTested)
	}
}

// TestGroupBreaksWhenItsLinkIsDisplaced: under a full budget a member gone quiet loses
// its link to displacement. The group must break with it; otherwise the member comes
// back still barred from pairing outside its chart, with nothing left to test that
// would ever break the group.
func TestGroupBreaksWhenItsLinkIsDisplaced(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 120, 15*time.Second)
	p.SetMaxPairs(2)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rng := &[2]float64{0, 1}
	for i := 0; i < 80; i++ {
		at := t0.Add(time.Duration(i) * 10 * time.Second)
		x := float64(i%10) / 10
		rs := []types.PropertyReading{
			{ID: "busy", Chart: "cpu", Value: x},
			{ID: "load", Value: 0.1 + 0.6*x + 0.05*math.Sin(float64(i)*1.3)},
		}
		if i < 30 || i >= 60 {
			rs = append(rs, types.PropertyReading{ID: "busy_avg", Chart: "cpu", Value: 0.25 + 0.5*x})
		}
		if i >= 30 {
			rs = append(rs, types.PropertyReading{ID: "extra", Value: 0.5 + 0.4*math.Sin(float64(i)*2.9)})
		}
		for _, r := range rs {
			r.At, r.Range = at, rng
			_ = p.ObserveProperty(r)
		}
		if i == 29 {
			if d, _ := p.Discovery(); len(d.Groups) != 1 {
				t.Fatalf("groups %+v before busy_avg went quiet; want one", d.Groups)
			}
		}
		if i == 59 {
			if d, _ := p.Discovery(); len(d.Groups) != 0 {
				t.Errorf("groups %+v after busy_avg's link was displaced; want none", d.Groups)
			}
		}
	}
}

// TestPendingCandidateLeavesWhenItWeakens: a pending candidate whose correlation
// falls below the threshold leaves the queue with the numbers that put it out,
// rather than standing on the |r| it had when it was emitted. It re-enters when the
// pair holds again.
func TestPendingCandidateLeavesWhenItWeakens(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 20, 15*time.Second)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rng := &[2]float64{0, 1}
	feed := func(from, to int, coupled bool) {
		for i := from; i < to; i++ {
			at := t0.Add(time.Duration(i) * 10 * time.Second)
			x := float64(i%10) / 10
			y := 0.2 + 0.7*x
			if !coupled {
				y = 0.5 + 0.4*math.Sin(float64(i)*2.9)
			}
			_ = p.ObserveProperty(types.PropertyReading{ID: "x", Value: x, At: at, Range: rng})
			_ = p.ObserveProperty(types.PropertyReading{ID: "y", Value: y, At: at, Range: rng})
		}
	}
	status := func() (types.CandidateStatus, string, float64) {
		h, _ := p.GetHistory()
		for _, c := range h {
			if c.CandidateID == "x<->y" {
				return c.Status, c.Reason, c.MIScore
			}
		}
		t.Fatal("x<->y never proposed")
		return 0, "", 0
	}
	feed(0, 30, true)
	if s, _, _ := status(); s != types.Pending {
		t.Fatalf("x<->y is %v while coupled; want pending", s)
	}
	feed(30, 60, false)
	if s, why, r := status(); s != types.Deferred || why != "weakened" || r >= 0.8 {
		t.Errorf("x<->y is %v (%q) at |r|=%.3f after parting; want deferred as weakened, with the |r| that put it out", s, why, r)
	}
	feed(60, 90, true)
	if s, _, _ := status(); s != types.Pending {
		t.Errorf("x<->y is %v after coupling again; want pending", s)
	}
}

// TestPendingCandidateHoldsThroughAJitter: a pending pair whose |r| dips under the
// threshold by less than its own sampling error stays pending. Leaving at the
// threshold itself, a pair sitting on it flapped between pending and weakened with
// every few readings, each flip a change for an operator to look at. It leaves once
// the dip is larger than the window's sampling error.
func TestPendingCandidateHoldsThroughAJitter(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 120, 15*time.Second)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rng := &[2]float64{0, 1}
	feed := func(from, to int, noise float64) {
		for i := from; i < to; i++ {
			at := t0.Add(time.Duration(i) * 10 * time.Second)
			x := float64(i%10) / 10
			y := 0.3 + 0.4*x + noise*math.Sin(float64(i)*2.9)
			_ = p.ObserveProperty(types.PropertyReading{ID: "x", Value: x, At: at, Range: rng})
			_ = p.ObserveProperty(types.PropertyReading{ID: "y", Value: y, At: at, Range: rng})
		}
	}
	status := func() (types.CandidateStatus, string, float64) {
		h, _ := p.GetHistory()
		for _, c := range h {
			if c.CandidateID == "x<->y" {
				return c.Status, c.Reason, c.MIScore
			}
		}
		t.Fatal("x<->y never proposed")
		return 0, "", 0
	}
	feed(0, 120, 0.05)
	if s, _, _ := status(); s != types.Pending {
		t.Fatalf("x<->y is %v while coupled; want pending", s)
	}
	// One SE of Fisher's z at n=120 puts the floor at |r| ≈ 0.765 for a 0.8 threshold.
	feed(120, 240, 0.13)
	if s, why, r := status(); s != types.Pending || r >= 0.8 || r < 0.765 {
		t.Errorf("x<->y is %v (%q) at |r|=%.3f; want pending at an |r| just under the threshold", s, why, r)
	}
	feed(240, 360, 0.2)
	if s, why, r := status(); s != types.Deferred || why != "weakened" || r >= 0.765 {
		t.Errorf("x<->y is %v (%q) at |r|=%.3f; want deferred as weakened below the floor", s, why, r)
	}
}

// TestSameSubjectFlag: two metrics of one subject that move together, such as a
// capped pod's CPU and its throttling, are proposed and marked same-subject: one
// workload's own quantities hold together by construction more often than by a
// finding. Two subjects' readings stay unmarked unless they are of one metric.
func TestSameSubjectFlag(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 120, 15*time.Second)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rng := &[2]float64{0, 1}
	for i := 0; i < 30; i++ {
		at := t0.Add(time.Duration(i) * 10 * time.Second)
		x := float64(i%10) / 10
		for _, r := range []types.PropertyReading{
			{ID: "cpu_utilization@pod:a", Subject: "pod:a", Value: x},
			{ID: "cpu_throttle_ratio@pod:a", Subject: "pod:a", Value: 0.1 + 0.8*x},
			{ID: "io_pressure_ratio@pod:b", Subject: "pod:b", Value: 0.2 + 0.5*x},
		} {
			r.At, r.Range = at, rng
			_ = p.ObserveProperty(r)
		}
	}
	kinds := map[string]string{}
	for _, c := range mustCandidates(t, p) {
		kinds[c.CandidateID] = c.Kind
	}
	if k, ok := kinds["cpu_throttle_ratio@pod:a<->cpu_utilization@pod:a"]; !ok || k != types.CandidateKindSameSubject {
		t.Errorf("pod:a's own pair: kind %q (proposed %v); want same-subject", k, ok)
	}
	if k, ok := kinds["cpu_utilization@pod:a<->io_pressure_ratio@pod:b"]; !ok || k != "" {
		t.Errorf("a pair across two subjects: kind %q (proposed %v); want proposed and unmarked", k, ok)
	}
}

// TestGroupHoldsThroughAJitter: a link forms at groupR and holds until the pair falls
// below it by more than one standard error of Fisher's z, so a pair whose |r| hovers at
// the cut-off keeps its group rather than forming and breaking it at every crossing.
func TestGroupHoldsThroughAJitter(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 120, 15*time.Second)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rng := &[2]float64{0, 1}
	noise := rand.New(rand.NewSource(7))
	changes, grouped := 0, false
	feed := func(from, to int, sd float64) {
		for i := from; i < to; i++ {
			at := t0.Add(time.Duration(i) * 10 * time.Second)
			x := float64(i%10) / 10
			for _, r := range []types.PropertyReading{
				{ID: "used", Value: x, Chart: "system.ram"},
				{ID: "free", Value: 1 - x + sd*noise.NormFloat64(), Chart: "system.ram"},
			} {
				r.At, r.Range = at, rng
				_ = p.ObserveProperty(r)
			}
			d, _ := p.Discovery()
			if now := len(d.Groups) == 1; now != grouped {
				changes++
				grouped = now
			}
		}
	}
	feed(0, 120, 0.02)
	if !grouped {
		t.Fatal("used and free not grouped while they move as one")
	}
	changes = 0
	feed(120, 600, 0.04) // |r| hovers at the cut-off
	if changes != 0 || !grouped {
		t.Errorf("the group changed %d times while |r| hovered at the cut-off; want it held", changes)
	}
	feed(600, 840, 0.15)
	if grouped {
		t.Error("used and free still grouped after they parted")
	}
}

// TestSparseSpikesAreNotEvidence: two rates that sit at zero and spike together a few
// times correlate at r ≈ 1 over the window, but the window holds only those few
// readings' worth of evidence, as idle pods' interfaces bursting at the same moments
// showed. A pair is tested only when each end leaves its commonest value in at
// least minPairs readings; past that, spikes are as good as any other movement.
func TestSparseSpikesAreNotEvidence(t *testing.T) {
	run := func(every int) bool {
		p := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 30, 120, 15*time.Second)
		t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		for i := 0; i < 240; i++ {
			at := t0.Add(time.Duration(i) * 10 * time.Second)
			v := 0.0
			if i%every == 0 {
				v = 300 + float64(i%7)
			}
			for _, r := range []types.PropertyReading{
				{ID: "net.net.sent@net:veth1", Subject: "net:veth1", Value: v},
				{ID: "net.net.sent@net:veth2", Subject: "net:veth2", Value: v * 1.1},
			} {
				r.At = at
				_ = p.ObserveProperty(r)
			}
		}
		h, _ := p.GetHistory()
		return len(h) > 0
	}
	if run(40) {
		t.Error("two series at zero but for 3 shared spikes in 120 readings were proposed")
	}
	if !run(3) {
		t.Error("two series off zero in 40 of 120 readings, together, were not proposed")
	}
}

// TestDeclaredPartIsComposition: a subject's reading its producer declared part of a
// node-level property is composition against it, though the names differ: on a node
// with one disk the disk's writes are the node's I/O out, which is not a finding.
func TestDeclaredPartIsComposition(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 30, 15*time.Second)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 30; i++ {
		at := t0.Add(time.Duration(i) * 10 * time.Second)
		x := float64(i%10) / 10
		for _, r := range []types.PropertyReading{
			{ID: "disk.io.writes@disk:sda", Subject: "disk:sda", PartOf: "system.io.out", Value: x},
			{ID: "disk.await.writes@disk:sda", Subject: "disk:sda", Value: 0.2 + 0.5*x},
			{ID: "system.io.out", Value: x},
		} {
			r.At = at
			_ = p.ObserveProperty(r)
		}
	}
	kinds := map[string]string{}
	for _, c := range mustCandidates(t, p) {
		kinds[c.CandidateID] = c.Kind
	}
	if k, ok := kinds["disk.io.writes@disk:sda->system.io.out"]; !ok || k != types.CandidateKindComposition {
		t.Errorf("writes->system.io.out kind %q (proposed %v); want composition", k, ok)
	}
	if k := kinds["disk.await.writes@disk:sda->system.io.out"]; k != "" {
		t.Errorf("await->system.io.out kind %q; an undeclared part is no composition", k)
	}
}

func TestAPartOfFollowsTheLatestReading(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 30, 15*time.Second)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const id = "net.net.received@net:eth0->system.io.out"
	observe := func(from, to int, partOf string) {
		for i := from; i < to; i++ {
			at := t0.Add(time.Duration(i) * 10 * time.Second)
			x := float64(i%10) / 10
			for _, r := range []types.PropertyReading{
				{ID: "net.net.received@net:eth0", Subject: "net:eth0", PartOf: partOf, Value: x},
				{ID: "system.io.out", Value: x},
			} {
				r.At = at
				_ = p.ObserveProperty(r)
			}
		}
	}
	kind := func() (string, bool) {
		for _, c := range mustCandidates(t, p) {
			if c.CandidateID == id {
				return c.Kind, true
			}
		}
		return "", false
	}
	for _, step := range []struct {
		name, partOf, want string
		from, to           int
	}{
		{"undeclared", "", "", 0, 30},
		{"declared after the candidate formed", "system.io.out", types.CandidateKindComposition, 30, 31},
		{"withdrawn", "", "", 31, 32}, // a wrong declaration, corrected
	} {
		observe(step.from, step.to, step.partOf)
		if k, ok := kind(); !ok || k != step.want {
			t.Errorf("%s: kind %q (proposed %v); want %q", step.name, k, ok, step.want)
		}
	}
}

// reviewPair feeds a subject's metric and a node metric for readings [from, to): y
// follows x exactly, plus noise of the given amplitude.
func reviewPair(p *minimal.MICorrelationProposer, from, to int, noise float64) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := from; i < to; i++ {
		at := t0.Add(time.Duration(i) * 10 * time.Second)
		x := float64(i%10) / 10
		y := x + noise*float64((i*7)%5-2)/2
		for _, r := range []types.PropertyReading{
			{ID: "m@pod:a", Subject: "pod:a", Value: x},
			{ID: "node.m2", Value: y},
		} {
			r.At = at
			_ = p.ObserveProperty(r)
		}
	}
}

func historyOf(t *testing.T, p *minimal.MICorrelationProposer, id string) *types.CandidateEdge {
	t.Helper()
	h, err := p.GetHistory()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range h {
		if c.CandidateID == id {
			return c
		}
	}
	t.Fatalf("%s is not in history", id)
	return nil
}

func TestAnAgentRejectionReopensWhenTheEvidenceMoves(t *testing.T) {
	const id = "m@pod:a->node.m2"
	for _, tc := range []struct {
		by         string
		wantReason string
		wantAfter  types.CandidateStatus
	}{
		{types.ReviewerDecisionModel, "agent", types.Pending},
		{types.ReviewerOperator, "operator", types.Rejected},
	} {
		p := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 30, 15*time.Second)
		reviewPair(p, 0, 30, 0)
		if err := p.Reject(id, types.CandidateReview{By: tc.by, Model: "m", DecisionID: "rv-1"}); err != nil {
			t.Fatal(err)
		}
		c := historyOf(t, p, id)
		if c.Status != types.Rejected || c.Reason != tc.wantReason || c.Review == nil ||
			c.Review.By != tc.by || !c.Review.Applied || c.Review.MIScore != c.MIScore {
			t.Fatalf("%s: rejected %v reason %q review %+v; want the review stamped with the evidence", tc.by, c.Status, c.Reason, c.Review)
		}
		reviewPair(p, 30, 31, 0) // the same evidence: the rejection stands
		if c := historyOf(t, p, id); c.Status != types.Rejected {
			t.Errorf("%s: the same evidence reopened the candidate (%v)", tc.by, c.Status)
		}
		reviewPair(p, 31, 61, 0.3) // r falls from 1 to about 0.9, still a candidate
		if c := historyOf(t, p, id); c.Status != tc.wantAfter {
			t.Errorf("%s: after the evidence moved (|r| %.3f) status %v; want %v", tc.by, c.MIScore, c.Status, tc.wantAfter)
		}
	}
}

func TestAnnotateKeepsTheCandidateInTheQueue(t *testing.T) {
	const id = "m@pod:a->node.m2"
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 30, 15*time.Second)
	reviewPair(p, 0, 30, 0)
	if err := p.Annotate(id, types.CandidateReview{By: types.ReviewerReasoningAgent, Tier: 2, Applied: true}); err != nil {
		t.Fatal(err)
	}
	c := historyOf(t, p, id)
	if c.Status != types.Pending || c.Review == nil || c.Review.Action != types.ReviewEscalate || c.Review.Applied {
		t.Errorf("annotated: %v review %+v; want pending, escalated and not applied", c.Status, c.Review)
	}
}
