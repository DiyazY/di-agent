package minimal_test

import (
	"math"
	"math/rand"
	"testing"

	"github.com/DiyazY/di-agent/compliance"
	"github.com/DiyazY/di-agent/internal/minimal"
	"github.com/DiyazY/di-agent/pkg/contracts"
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
		return minimal.NewMICorrelationProposer(ontology, 0.8, 30, 100)
	})
}

// ── Strongly correlated input → emits a candidate ─────────────────────────────

func TestMICorrelationProposer_StronglyCorrelatedEmits(t *testing.T) {
	ontology := minimal.NewOntologyFromSpec(mustSpec())
	// Use a free pair (MU↛PS), threshold 0.8, minPairs 30, bufSize 200.
	p := minimal.NewMICorrelationProposer(ontology, 0.8, 30, 200)

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
	if c.CandidateID != "P-prop-MU-PS" {
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
	p := minimal.NewMICorrelationProposer(ontology, 0.8, 30, 200)
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
	p := minimal.NewMICorrelationProposer(ontology, 0.8, 30, 200)

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
	if err := p.Confirm(cs[0].CandidateID); err != nil {
		t.Fatalf("Confirm error: %v", err)
	}

	after, _ := ontology.Propositions()
	if len(after) != beforeCount+1 {
		t.Errorf("Confirm should add exactly 1 proposition; before=%d after=%d", beforeCount, len(after))
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
	p := minimal.NewMICorrelationProposer(ontology, 0.8, 30, 200)

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
	p := minimal.NewMICorrelationProposer(ontology, 0.8, 30, 200)

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
		p := minimal.NewMICorrelationProposer(ontology, 0.8, 30, 200)
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
		p := minimal.NewMICorrelationProposer(ontology, 0.8, 30, 200)
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
	p := minimal.NewMICorrelationProposer(ontology, 0.5, 10, 50)
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
