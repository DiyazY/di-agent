package semmap

import (
	"math"
	"testing"

	"github.com/DiyazY/di-agent/internal/minimal"
	"github.com/DiyazY/di-agent/pkg/types"
)

// These tests pin the behaviour paper §5.2 depends on: the six edges with no
// telemetry analog "remain at the structural prior until one of three
// operator-driven recalibration paths is invoked" — the Tuner, a direct
// SetPropositionStrength call, or Deprecate. Two of those three wrote only the
// ontology, so the Reasoner (which reads storage edges) never saw the change and
// a recalibration was silently cosmetic.
//
// The per-KD split is what makes this subtle. prior_init seeds edges from
// `distribution_edge_weights` while the ontology carries the global
// `propositions` table, so the two legitimately disagree: on k0s, P1's edge
// prior is 0.2138 against a proposition strength of 0.620. A tune must anchor
// its delta to the edge value the Reasoner reads.

// newSplitMap seeds storage edges with values that deliberately differ from the
// ontology's proposition strengths, reproducing the post-prior_init state of a
// k0s-seeded daemon.
func newSplitMap(t *testing.T, edgePriors map[string]float64) *SemanticMap {
	t.Helper()
	storage := minimal.NewInMemoryStorage()
	ontology := minimal.NewStaticDiSelectOntology()
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
		minimal.NewDisabledProposer(), minimal.NewRuleBasedTuner())
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
	m := newSplitMap(t, map[string]float64{"P1": 0.2138})

	if got := edgePrior(t, m, "P1"); math.Abs(got-0.2138) > 1e-9 {
		t.Fatalf("setup: edge prior = %v, want 0.2138", got)
	}

	if err := m.SetPropositionStrength("P1", 0.55); err != nil {
		t.Fatalf("SetPropositionStrength: %v", err)
	}

	if got := edgePrior(t, m, "P1"); math.Abs(got-0.55) > 1e-9 {
		t.Errorf("edge PriorWeight = %v, want 0.55 — an ontology-only write leaves "+
			"the Reasoner computing from the stale magnitude", got)
	}

	props, err := m.Propositions()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range props {
		if p.PropositionID == "P1" && math.Abs(p.PriorStrength-0.55) > 1e-9 {
			t.Errorf("proposition strength = %v, want 0.55", p.PriorStrength)
		}
	}
}

func TestSetPropositionStrengthPreservesEvidence(t *testing.T) {
	m := newSplitMap(t, map[string]float64{"P1": 0.2138})

	// Accumulate evidence, then recalibrate the prior. The EMA is observation
	// history and must survive a prior revision untouched.
	if err := m.Ingest("SC", "RC", 0.9, "evt-1"); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	before := func() (float64, int) {
		edges, _ := m.AllEdges()
		for _, e := range edges {
			if e.PropositionID == "P1" {
				return e.EMAWeight, e.NObservations
			}
		}
		return 0, 0
	}
	emaBefore, obsBefore := before()
	if obsBefore == 0 {
		t.Fatal("setup: expected at least one observation on P1")
	}

	if err := m.SetPropositionStrength("P1", 0.55); err != nil {
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

func TestSetPropositionStrengthTracksEMAOnUnobservedEdge(t *testing.T) {
	// P4 (SC→RR) is one of the six edges with no telemetry analog, so it carries
	// zero observations for the whole deployment — and it is exactly the kind of
	// edge the recalibration path in paper §5.2 exists to serve (e.g. a fresh
	// kube-bench scan revising an SC score). With no observations, EMAWeight is
	// the seed value rather than evidence, so it must follow the new prior;
	// otherwise a later first observation would start from a superseded
	// magnitude.
	m := newSplitMap(t, map[string]float64{"P4": 0.3084})

	if err := m.SetPropositionStrength("P4", 0.71); err != nil {
		t.Fatalf("SetPropositionStrength: %v", err)
	}

	edges, err := m.AllEdges()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range edges {
		if e.PropositionID != "P4" {
			continue
		}
		if e.NObservations != 0 {
			t.Fatalf("setup: expected zero observations, got %d", e.NObservations)
		}
		if math.Abs(e.PriorWeight-0.71) > 1e-9 {
			t.Errorf("PriorWeight = %v, want 0.71", e.PriorWeight)
		}
		if math.Abs(e.EMAWeight-0.71) > 1e-9 {
			t.Errorf("EMAWeight = %v, want 0.71 — on an edge with no observations "+
				"the EMA is the seed value, not evidence, and must follow the "+
				"recalibrated prior", e.EMAWeight)
		}
	}
}

func TestTuneAnchorsDeltaToEdgePrior(t *testing.T) {
	// The real k0s calibration for the two propositions the "security" keyword
	// group targets. Reproduces paper Table 6.
	m := newSplitMap(t, map[string]float64{"P1": 0.2138, "P11": 0.0089})

	adjustments, err := m.Tune("prioritize security", "test-operator")
	if err != nil {
		t.Fatalf("Tune: %v", err)
	}
	if len(adjustments) == 0 {
		t.Fatal("no adjustments applied; is the tuner wired?")
	}

	got := map[string][2]float64{}
	for _, a := range adjustments {
		got[a.PropositionID] = [2]float64{a.OldStrength, a.NewStrength}
	}

	// P1: 0.2138 + 0.12 = 0.3338, above the 0.30 SC floor so unclamped.
	if v, ok := got["P1"]; !ok {
		t.Error("P1 not adjusted")
	} else {
		if math.Abs(v[0]-0.2138) > 1e-9 {
			t.Errorf("P1 old = %v, want the edge prior 0.2138 (not the global 0.620)", v[0])
		}
		if math.Abs(v[1]-0.3338) > 1e-9 {
			t.Errorf("P1 new = %v, want 0.3338", v[1])
		}
	}

	// P11: 0.0089 + 0.12 = 0.1289, below the 0.30 SC-adjacent floor, so clamped.
	// This is the case that exercises the floor at all — anchored to the global
	// strength of 0.480 it could never be reached.
	if v, ok := got["P11"]; !ok {
		t.Error("P11 not adjusted")
	} else {
		if math.Abs(v[0]-0.0089) > 1e-9 {
			t.Errorf("P11 old = %v, want the edge prior 0.0089", v[0])
		}
		if math.Abs(v[1]-0.30) > 1e-9 {
			t.Errorf("P11 new = %v, want 0.30 (SC-adjacent floor clamp)", v[1])
		}
	}

	// And the tune must land in storage, or the Reasoner never sees it.
	if p := edgePrior(t, m, "P1"); math.Abs(p-0.3338) > 1e-9 {
		t.Errorf("P1 edge prior after tune = %v, want 0.3338", p)
	}
	if p := edgePrior(t, m, "P11"); math.Abs(p-0.30) > 1e-9 {
		t.Errorf("P11 edge prior after tune = %v, want 0.30", p)
	}
}
