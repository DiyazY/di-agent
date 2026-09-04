package minimal_test

import (
	"math"
	"math/rand"
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
	prop, err := p.Confirm(cs[0].CandidateID)
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
	if err := p.Reject(cid); err != nil {
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

func feedScoped(p *minimal.MICorrelationProposer, n int, gap time.Duration) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		at := t0.Add(time.Duration(i) * 10 * time.Second)
		x := float64(i%20) / 20
		_ = p.ObserveProperty("cpu_utilization@pod:a", "pod:a", x, at)
		_ = p.ObserveProperty("cpu_utilization@pod:b", "pod:b", 1-x, at)
		_ = p.ObserveProperty("cpu_pressure_ratio", "", 0.9*x+0.05, at.Add(gap))
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

func TestObserveProperty_PairsScopedWithUnscopedOnly(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 60, 15*time.Second)
	feedScoped(p, 40, 2*time.Second)
	ids := map[string]bool{}
	for _, c := range mustCandidates(t, p) {
		ids[c.CandidateID] = true
		if c.FromID == "cpu_pressure_ratio" {
			t.Errorf("direction must be scoped -> unscoped; got %s -> %s", c.FromID, c.ToID)
		}
	}
	if !ids["cpu_utilization@pod:a->cpu_pressure_ratio"] || !ids["cpu_utilization@pod:b->cpu_pressure_ratio"] {
		t.Errorf("expected both pods to be proposed against node pressure; got %v", ids)
	}
	if ids["cpu_utilization@pod:a->cpu_utilization@pod:b"] || ids["cpu_utilization@pod:b->cpu_utilization@pod:a"] {
		t.Error("two scoped properties must never be paired, however correlated")
	}
}

func TestObserveProperty_RespectsTimeTolerance(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 60, 15*time.Second)
	feedScoped(p, 40, 10*time.Minute) // every node reading is minutes away from every pod reading, so no pair can ever fall inside the 15s window
	if cs := mustCandidates(t, p); len(cs) != 0 {
		t.Errorf("readings outside the pair window formed %d candidates; want none", len(cs))
	}
}

func TestObserveProperty_SkipsCoveredPairs(t *testing.T) {
	m := statemap.New(statemap.Config{AdmitUnknown: true}, statemap.NewJournal(0))
	_ = m.Observe("cpu_utilization@pod:a", .1, time.Now())
	_ = m.Observe("cpu_pressure_ratio", .1, time.Now())
	_ = m.DeclareRelationship(statemap.Relationship{From: "cpu_utilization@pod:a", To: "cpu_pressure_ratio", Sign: 1, Label: "discovered"})
	p := minimal.NewMICorrelationProposer(m, 0.8, 10, 60, 15*time.Second)
	feedScoped(p, 40, 2*time.Second)
	for _, c := range mustCandidates(t, p) {
		if c.FromID == "cpu_utilization@pod:a" {
			t.Error("a pair the state map already holds must not be re-proposed")
		}
	}
}

func TestForgetDropsEverythingAboutAProperty(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.8, 10, 60, 15*time.Second)
	feedScoped(p, 40, 2*time.Second)
	if err := p.Forget("cpu_utilization@pod:a"); err != nil {
		t.Fatal(err)
	}
	for _, c := range mustCandidates(t, p) {
		if c.FromID == "cpu_utilization@pod:a" {
			t.Error("candidate for a forgotten property survived")
		}
	}
	h, _ := p.GetHistory()
	for _, c := range h {
		if c.FromID == "cpu_utilization@pod:a" {
			t.Error("history for a forgotten property survived")
		}
	}
}

func TestPendingCapDefersTheWeakest(t *testing.T) {
	p := minimal.NewMICorrelationProposer(coveredNone{}, 0.5, 10, 60, 15*time.Second)
	p.SetMaxPending(2)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 30; i++ {
		at := t0.Add(time.Duration(i) * 10 * time.Second)
		x := float64(i%10) / 10
		_ = p.ObserveProperty("m@pod:1", "pod:1", x, at)
		_ = p.ObserveProperty("m@pod:2", "pod:2", x*0.9, at)
		_ = p.ObserveProperty("m@pod:3", "pod:3", x*0.7+0.1, at)
		_ = p.ObserveProperty("node", "", x, at)
	}
	if cs := mustCandidates(t, p); len(cs) > 2 {
		t.Errorf("pending=%d exceeds the cap of 2", len(cs))
	}
	var deferred int
	h, _ := p.GetHistory()
	for _, c := range h {
		if c.Status == types.Deferred {
			deferred++
		}
	}
	if deferred == 0 {
		t.Error("the excess candidate should be visible in history as deferred, not silently dropped")
	}
}
