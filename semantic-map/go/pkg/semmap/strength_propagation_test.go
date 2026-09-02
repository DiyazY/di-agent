package semmap_test

import (
	"math"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/internal/minimal"
	"github.com/DiyazY/di-agent/pkg/semmap"
	"github.com/DiyazY/di-agent/pkg/statemap"
)

// These tests pin the write-through invariant: an operator recalibration must reach the
// model every answer is read from, not just the declaration layer. SetPropositionStrength
// and Tune each wrote only the ontology at one point, so a recalibration moved the audit
// log and changed no decision.
//
// The subtle part is the anchoring. prior_init calibrates per cluster, so the state
// model's relationship priors legitimately differ from the global propositions table. A
// tune has to anchor its delta to the value in force — anchoring to the global figure
// would turn a nudge into a jump and discard the per-cluster calibration in one action.

// newSplitMap seeds relationship priors that deliberately differ from the ontology's
// proposition strengths, reproducing the post-prior_init state of a calibrated daemon.
func newSplitMap(t *testing.T, priors map[string]float64) (*semmap.SemanticMap, *statemap.Map) {
	t.Helper()
	m, state := newMapWith(t, minimal.NewRuleBasedTunerFromSpec(mustSpec()))

	for propID, prior := range priors {
		rel := relationshipFor(t, state, propID)
		if err := state.AssertRelationshipStrength(rel.ID, prior, "prior_init",
			"per-cluster calibration"); err != nil {
			t.Fatal(err)
		}
	}
	return m, state
}

// relPrior reads what the relationship currently stands at. The operator path writes
// an assertion rather than a prior now, so this reads the effective value — which is
// the assertion when one has been made, and is what every answer is computed from.
func relPrior(t *testing.T, state *statemap.Map, propID string) float64 {
	t.Helper()
	rel := relationshipFor(t, state, propID)
	v, known := rel.Effective()
	if !known {
		t.Fatalf("relationship for %s has no strength", propID)
	}
	return v
}

func TestSetPropositionStrengthReachesTheModel(t *testing.T) {
	// A calibrated split: the relationship's prior sits well below the global
	// proposition strength.
	m, state := newSplitMap(t, map[string]float64{specProp(t): 0.2138})

	if got := relPrior(t, state, specProp(t)); math.Abs(got-0.2138) > 1e-9 {
		t.Fatalf("setup: relationship prior = %v, want 0.2138", got)
	}

	if err := m.SetPropositionStrength(specProp(t), 0.55); err != nil {
		t.Fatalf("SetPropositionStrength: %v", err)
	}

	if got := relPrior(t, state, specProp(t)); math.Abs(got-0.55) > 1e-9 {
		t.Errorf("relationship prior = %v, want 0.55 — a declaration-only write leaves "+
			"every answer computed from the stale magnitude", got)
	}

	// The declaration layer moves too, so /propositions does not report a number the
	// agent stopped using.
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
	m, state := newSplitMap(t, map[string]float64{specProp(t): 0.2138})
	rel := relationshipFor(t, state, specProp(t))

	// Give the relationship evidence of its own, then recalibrate the prior. What was
	// learned from this system is observation history and must survive a prior revision
	// untouched — which is exactly why the two numbers are separate fields.
	if err := state.ObserveRelationship(rel.ID, 0.9, time.Now()); err != nil {
		t.Fatalf("ObserveRelationship: %v", err)
	}
	before := relationshipFor(t, state, specProp(t))
	if before.NObservations == 0 {
		t.Fatal("setup: expected at least one observation on the relationship")
	}

	if err := m.SetPropositionStrength(specProp(t), 0.55); err != nil {
		t.Fatalf("SetPropositionStrength: %v", err)
	}

	after := relationshipFor(t, state, specProp(t))
	if math.Abs(after.Strength-before.Strength) > 1e-12 {
		t.Errorf("learned strength changed %v -> %v; recalibrating a prior must not "+
			"rewrite accumulated evidence", before.Strength, after.Strength)
	}
	if after.NObservations != before.NObservations {
		t.Errorf("observation count changed %d -> %d", before.NObservations, after.NObservations)
	}
	if after.Assertion == nil || math.Abs(*after.Assertion-0.55) > 1e-9 {
		t.Errorf("assertion = %v, want the asserted 0.55", after.Assertion)
	}
}

func TestTuneAnchorsDeltaToTheCalibratedPrior(t *testing.T) {
	// Take the first intent from the spec and the propositions it names, so the test
	// exercises the shipped vocabulary rather than a remembered one.
	spec := mustSpec()
	if len(spec.IntentRules) == 0 {
		t.Skip("spec declares no intent rules")
	}
	rule := spec.IntentRules[0]

	// Seed the named relationships well below their ontology strengths, reproducing the
	// post-prior_init split a real daemon has.
	const seeded = 0.2138
	priors := map[string]float64{}
	for pid := range rule.Deltas {
		priors[pid] = seeded
	}
	m, state := newSplitMap(t, priors)

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
			t.Errorf("%s old = %v, want the calibrated prior %v (not the global strength)",
				a.PropositionID, a.OldStrength, seeded)
		}
		want := seeded + delta
		if floor := spec.FloorFor(a.PropositionID); want < floor {
			want = floor
		}
		if math.Abs(a.NewStrength-want) > 1e-9 {
			t.Errorf("%s new = %v, want %v", a.PropositionID, a.NewStrength, want)
		}
		// And it must land in the state model, or no answer ever sees it.
		if got := relPrior(t, state, a.PropositionID); math.Abs(got-a.NewStrength) > 1e-9 {
			t.Errorf("%s prior after tune = %v, want %v", a.PropositionID, got, a.NewStrength)
		}
	}
}
