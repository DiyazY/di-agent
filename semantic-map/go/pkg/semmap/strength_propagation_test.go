package semmap

import (
	"math"
	"testing"

	"github.com/DiyazY/di-agent/internal/minimal"
	"github.com/DiyazY/di-agent/pkg/types"
)

// These tests pin the write-through invariant: an operator recalibration must
// reach the storage EdgeDescriptor the Reasoner reads, not just the ontology.
// SetPropositionStrength and Tune both wrote only the ontology at one point, so a
// recalibration moved the audit log and changed no decision.
//
// The per-KD split is what makes this subtle. prior_init seeds edges from
// distribution_edge_weights while the ontology carries the global propositions
// table, so the two legitimately disagree on a seeded daemon. A tune must anchor
// its delta to the edge value the Reasoner reads, not to the global figure.
//
// Proposition IDs come from the loaded spec rather than being named literally, so
// these tests survive a change to the graph's scope.

// newSplitMap seeds storage edges with values that deliberately differ from the
// ontology's proposition strengths, reproducing the post-prior_init state of a
// k0s-seeded daemon.
func newSplitMap(t *testing.T, edgePriors map[string]float64) *SemanticMap {
	t.Helper()
	storage := minimal.NewInMemoryStorage()
	ontology := minimal.NewOntologyFromSpec(mustSpec())
	updater := minimal.NewEMAUpdater(storage, 0.2, 500)
	reasoner := minimal.NewRuleEngineReasoner(storage, ontology, 0.5, nil, nil)

	props, err := ontology.Propositions()
	if err != nil {
		t.Fatalf("Propositions: %v", err)
	}
	for _, p := range props {
		prior := p.PriorStrength
		if v, ok := edgePriors[p.PropositionID]; ok {
			prior = v
		}
		if err := storage.PutEdge(&types.EdgeDescriptor{
			FromID:        p.FromConstruct,
			ToID:          p.ToConstruct,
			PropositionID: p.PropositionID,
			Direction:     p.Direction,
			PriorWeight:   prior,
			EMAWeight:     prior,
		}); err != nil {
			t.Fatalf("PutEdge: %v", err)
		}
	}
	return New(storage, ontology, updater, reasoner,
		minimal.NewDisabledProposer(), minimal.NewRuleBasedTunerFromSpec(mustSpec()))
}

func edgePrior(t *testing.T, m *SemanticMap, propID string) float64 {
	t.Helper()
	edges, err := m.AllEdges()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range edges {
		if e.PropositionID == propID {
			return e.PriorWeight
		}
	}
	t.Fatalf("no edge for %s", propID)
	return 0
}

func TestSetPropositionStrengthReachesStorage(t *testing.T) {
	// k0s-like split: edge prior well below the global proposition strength.
	m := newSplitMap(t, map[string]float64{specProp(t): 0.2138})

	if got := edgePrior(t, m, specProp(t)); math.Abs(got-0.2138) > 1e-9 {
		t.Fatalf("setup: edge prior = %v, want 0.2138", got)
	}

	if err := m.SetPropositionStrength(specProp(t), 0.55); err != nil {
		t.Fatalf("SetPropositionStrength: %v", err)
	}

	if got := edgePrior(t, m, specProp(t)); math.Abs(got-0.55) > 1e-9 {
		t.Errorf("edge PriorWeight = %v, want 0.55 — an ontology-only write leaves "+
			"the Reasoner computing from the stale magnitude", got)
	}

	props, err := m.Propositions()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range props {
		if p.PropositionID == specProp(t) && math.Abs(p.PriorStrength-0.55) > 1e-9 {
			t.Errorf("proposition strength = %v, want 0.55", p.PriorStrength)
		}
	}
}

func TestSetPropositionStrengthPreservesEvidence(t *testing.T) {
	m := newSplitMap(t, map[string]float64{specProp(t): 0.2138})

	// Accumulate evidence, then recalibrate the prior. The EMA is observation
	// history and must survive a prior revision untouched.
	if err := m.Ingest(pairFrom(t), pairTo(t), 0.9, "evt-1"); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	before := func() (float64, int) {
		edges, _ := m.AllEdges()
		for _, e := range edges {
			if e.PropositionID == specProp(t) {
				return e.EMAWeight, e.NObservations
			}
		}
		return 0, 0
	}
	emaBefore, obsBefore := before()
	if obsBefore == 0 {
		t.Fatal("setup: expected at least one observation on the edge")
	}

	if err := m.SetPropositionStrength(specProp(t), 0.55); err != nil {
		t.Fatalf("SetPropositionStrength: %v", err)
	}

	emaAfter, obsAfter := before()
	if math.Abs(emaAfter-emaBefore) > 1e-12 {
		t.Errorf("EMAWeight changed %v -> %v; recalibrating a prior must not "+
			"rewrite accumulated evidence", emaBefore, emaAfter)
	}
	if obsAfter != obsBefore {
		t.Errorf("NObservations changed %d -> %d", obsBefore, obsAfter)
	}
}

// TestSetPropositionStrengthTracksEMAOnUnobservedEdge is removed. It covered an
// edge with zero observations, where EMAWeight is a seed value rather than
// evidence and must follow a recalibrated prior. Under a telemetry-only scope
// every edge in the graph has both endpoints routed, so no such edge exists to
// test. The behaviour it guarded is still implemented in SetPropositionStrength
// (the NObservations == 0 branch) and would matter again if a construct were
// admitted before a metric was routed to it.

func TestTuneAnchorsDeltaToEdgePrior(t *testing.T) {
	// Take the first intent from the spec and the propositions it names, so the
	// test exercises the shipped vocabulary rather than a remembered one.
	spec := mustSpec()
	if len(spec.IntentRules) == 0 {
		t.Skip("spec declares no intent rules")
	}
	rule := spec.IntentRules[0]

	// Seed the named edges well below their ontology strengths, reproducing the
	// post-prior_init split a real daemon has.
	const seeded = 0.2138
	priors := map[string]float64{}
	for pid := range rule.Deltas {
		priors[pid] = seeded
	}
	m := newSplitMap(t, priors)

	adjustments, err := m.Tune("prioritize "+rule.Keywords[0], "test-operator")
	if err != nil {
		t.Fatalf("Tune: %v", err)
	}
	if len(adjustments) == 0 {
		t.Fatal("no adjustments applied; is the tuner wired to the spec?")
	}

	for _, a := range adjustments {
		delta, named := rule.Deltas[a.PropositionID]
		if !named {
			continue // another intent's keyword also matched
		}
		if math.Abs(a.OldStrength-seeded) > 1e-9 {
			t.Errorf("%s old = %v, want the edge prior %v (not the ontology strength)",
				a.PropositionID, a.OldStrength, seeded)
		}
		want := seeded + delta
		if floor := spec.FloorFor(a.PropositionID); want < floor {
			want = floor
		}
		if math.Abs(a.NewStrength-want) > 1e-9 {
			t.Errorf("%s new = %v, want %v", a.PropositionID, a.NewStrength, want)
		}
		// And it must land in storage, or the Reasoner never sees it.
		if got := edgePrior(t, m, a.PropositionID); math.Abs(got-a.NewStrength) > 1e-9 {
			t.Errorf("%s edge prior after tune = %v, want %v", a.PropositionID, got, a.NewStrength)
		}
	}
}
