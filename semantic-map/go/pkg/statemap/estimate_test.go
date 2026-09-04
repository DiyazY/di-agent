package statemap

import (
	"math"
	"strings"
	"testing"
)

// estimateFixture: two subjects feed one node-level target with asserted strengths, so
// the arithmetic is checkable without the estimator in the way.
func estimateFixture(t *testing.T) (*Map, *clock) {
	t.Helper()
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	rng := [2]float64{0, 100}
	unit := [2]float64{0, 1}
	_ = m.Record(Observation{ID: "queue@pod:a", Value: 50, At: c.now(), Subject: "pod:a", Unit: "items", Range: &rng})
	_ = m.Record(Observation{ID: "cpu@pod:b", Value: 0.2, At: c.now(), Subject: "pod:b", Range: &unit})
	_ = m.Record(Observation{ID: "pressure", Value: 0.3, At: c.now(), Range: &unit})
	for _, r := range []Relationship{
		{From: "queue@pod:a", To: "pressure", Sign: 1, Label: "d"},
		{From: "cpu@pod:b", To: "pressure", Sign: 1, Label: "d"},
	} {
		if err := m.DeclareRelationship(r); err != nil {
			t.Fatal(err)
		}
	}
	_ = m.AssertRelationshipStrength(RelationshipID("queue@pod:a", "pressure", "d"), 0.8, "op", "fixture")
	_ = m.AssertRelationshipStrength(RelationshipID("cpu@pod:b", "pressure", "d"), 0.5, "op", "fixture")
	return m, c
}

func TestEstimateBaselineNormalisesByRange(t *testing.T) {
	m, _ := estimateFixture(t)
	res := m.Estimate(EstimateRequest{Target: "pressure"})
	if res.Err != "" {
		t.Fatal(res.Err)
	}
	// queue 50/100 → 0.5·0.8 = 0.40 ; cpu 0.2 → 0.2·0.5 = 0.10
	if !near(res.Answer.Contributions, 0.5) || !near(res.Answer.Sensitivity, 1.3) {
		t.Errorf("contributions=%.3f sensitivity=%.3f; want 0.5 and 1.3", res.Answer.Contributions, res.Answer.Sensitivity)
	}
	if res.Hypothetical != nil {
		t.Error("no assumptions were made; there must be no hypothetical block")
	}
}

func TestEstimateAssumeSubstitutesAndProjects(t *testing.T) {
	m, _ := estimateFixture(t)
	res := m.Estimate(EstimateRequest{Target: "pressure", Assume: map[string]float64{"queue@pod:a": 100}})
	if res.Hypothetical == nil {
		t.Fatal("assume produced no hypothetical")
	}
	// queue 100 → 1.0·0.8 = 0.8, delta = +0.4, projected = 0.3 + 0.4·(1−0) = 0.7
	if !near(res.Hypothetical.Delta, 0.4) || !near(res.Hypothetical.ProjectedLevel, 0.7) {
		t.Errorf("delta=%.3f projected=%.3f; want 0.4 and 0.7", res.Hypothetical.Delta, res.Hypothetical.ProjectedLevel)
	}
	var slope bool
	for _, cv := range res.Caveats {
		if strings.Contains(cv, "not a fitted slope") {
			slope = true
		}
	}
	if !slope {
		t.Errorf("caveats %v; the standing slope caveat is missing", res.Caveats)
	}
	if res.Assumptions["queue@pod:a"] != 100 {
		t.Errorf("assumptions %v; want the substitution recorded", res.Assumptions)
	}
}

func TestEstimateWithoutSubjectTakesItsPropertiesToTheFloor(t *testing.T) {
	m, _ := estimateFixture(t)
	res := m.Estimate(EstimateRequest{Target: "pressure", Without: []string{"pod:a"}})
	if res.Hypothetical == nil || !near(res.Hypothetical.Delta, -0.4) {
		t.Fatalf("without pod:a: %+v; want delta −0.4 (queue to its floor)", res.Hypothetical)
	}
	if len(res.Excluded) != 1 || res.Excluded[0] != "pod:a" || res.Assumptions["queue@pod:a"] != 0 {
		t.Errorf("excluded=%v assumptions=%v; want pod:a recorded and queue assumed at its floor", res.Excluded, res.Assumptions)
	}
	if !near(res.Hypothetical.ProjectedLevel, math.Max(0, 0.3-0.4)) {
		t.Errorf("projected=%.3f; want clamped to the target's floor", res.Hypothetical.ProjectedLevel)
	}
}

func TestEstimateRecordsAssumptionsInTheDecision(t *testing.T) {
	m, _ := estimateFixture(t)
	m.Estimate(EstimateRequest{ID: "d-1", Target: "pressure", Assume: map[string]float64{"cpu@pod:b": 0.9}})
	var d *Decision
	for _, e := range m.Journal().Events(0, 0) {
		if e.Decision != nil && e.Decision.ID == "d-1" {
			d = e.Decision
		}
	}
	if d == nil || d.Assumptions["cpu@pod:b"] != 0.9 || !strings.Contains(d.Question, "assuming cpu@pod:b=0.9") {
		t.Fatalf("decision %+v; want assumptions and a question naming them", d)
	}
}

func TestEstimateCaveatsAssumedRangeAndOutOfRange(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	_ = m.Observe("x@pod:1", 0.5, c.now()) // no declared range
	_ = m.Observe("y", 0.5, c.now())
	_ = m.DeclareRelationship(Relationship{From: "x@pod:1", To: "y", Sign: 1})
	_ = m.AssertRelationshipStrength(RelationshipID("x@pod:1", "y", ""), 0.5, "op", "t")
	res := m.Estimate(EstimateRequest{Target: "y", Assume: map[string]float64{"x@pod:1": 3}})
	var assumedRange, outOfRange bool
	for _, cv := range res.Caveats {
		assumedRange = assumedRange || strings.Contains(cv, "was assumed, not declared")
		outOfRange = outOfRange || strings.Contains(cv, "outside its declared range")
	}
	if !assumedRange || !outOfRange {
		t.Errorf("caveats %v; want both the assumed-range and out-of-range caveats", res.Caveats)
	}
}
