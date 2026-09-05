package statemap

import (
	"math"
	"strconv"
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

// TestEstimateIsReproducible: two identical estimates against an unchanged map must
// produce byte-identical rationale and caveats. The unmatched-assumption loop used to
// range over a Go map, so ordering was not guaranteed across calls.
func TestEstimateIsReproducible(t *testing.T) {
	m, c := estimateFixture(t)
	unit := [2]float64{0, 1}
	// Two more pod:a-scoped properties that do NOT relate to pressure, so `without
	// pod:a` produces several unmatched assumptions whose order is at stake.
	_ = m.Record(Observation{ID: "mem@pod:a", Value: 0.4, At: c.now(), Subject: "pod:a", Range: &unit})
	_ = m.Record(Observation{ID: "io@pod:a", Value: 0.6, At: c.now(), Subject: "pod:a", Range: &unit})

	res1 := m.Estimate(EstimateRequest{ID: "r1", Target: "pressure", Without: []string{"pod:a"}})
	res2 := m.Estimate(EstimateRequest{ID: "r2", Target: "pressure", Without: []string{"pod:a"}})

	if res1.Rationale != res2.Rationale {
		t.Errorf("rationale differs across identical estimates:\n%s\nvs\n%s", res1.Rationale, res2.Rationale)
	}
	if len(res1.Caveats) != len(res2.Caveats) {
		t.Fatalf("caveats length differs: %v vs %v", res1.Caveats, res2.Caveats)
	}
	for i := range res1.Caveats {
		if res1.Caveats[i] != res2.Caveats[i] {
			t.Errorf("caveat %d differs: %q vs %q", i, res1.Caveats[i], res2.Caveats[i])
		}
	}
	if len(res1.Assumptions) != 3 {
		t.Errorf("assumptions %v; want three entries (queue@pod:a, mem@pod:a, io@pod:a)", res1.Assumptions)
	}
}

// TestEstimateDoesNotDuplicateSourceCaveats: a source with two incoming relationships
// into the target must contribute its arithmetic per edge, but its source-scoped
// caveats and its assumption rationale line only once.
func TestEstimateDoesNotDuplicateSourceCaveats(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	_ = m.Observe("x@pod:1", 0.5, c.now()) // no declared range
	_ = m.Observe("y", 0.5, c.now())
	_ = m.DeclareRelationship(Relationship{From: "x@pod:1", To: "y", Sign: 1, Label: "a"})
	_ = m.DeclareRelationship(Relationship{From: "x@pod:1", To: "y", Sign: 1, Label: "b"})
	_ = m.AssertRelationshipStrength(RelationshipID("x@pod:1", "y", "a"), 0.5, "op", "t")
	_ = m.AssertRelationshipStrength(RelationshipID("x@pod:1", "y", "b"), 0.3, "op", "t")

	res := m.Estimate(EstimateRequest{Target: "y", Assume: map[string]float64{"x@pod:1": 0.9}})

	var rangeCaveats int
	for _, cv := range res.Caveats {
		if strings.Contains(cv, "was assumed, not declared") {
			rangeCaveats++
		}
	}
	if rangeCaveats != 1 {
		t.Errorf("caveats %v; want exactly one \"was assumed, not declared\" caveat, got %d", res.Caveats, rangeCaveats)
	}
	if n := strings.Count(res.Rationale, "assumed x@pod:1 = "); n != 1 {
		t.Errorf("rationale %q; want exactly one \"assumed x@pod:1 = \" occurrence, got %d", res.Rationale, n)
	}
	if len(res.Influences) != 2 {
		t.Errorf("influences %v; want two, one per relationship", res.Influences)
	}
}

// TestEstimateResultDoesNotAliasTheDecision: EstimateResult.Assumptions/Excluded must
// be independent copies. A caller mutating the result must not corrupt the journaled
// Decision, which is the audit record.
func TestEstimateResultDoesNotAliasTheDecision(t *testing.T) {
	m, _ := estimateFixture(t)
	res := m.Estimate(EstimateRequest{ID: "d-alias", Target: "pressure",
		Assume: map[string]float64{"cpu@pod:b": 0.9}, Without: []string{"pod:a"}})

	res.Assumptions["injected"] = 999
	if len(res.Excluded) > 0 {
		res.Excluded[0] = "tampered"
	}
	res.Excluded = append(res.Excluded, "extra")

	var d *Decision
	for _, e := range m.Journal().Events(0, 0) {
		if e.Decision != nil && e.Decision.ID == "d-alias" {
			d = e.Decision
		}
	}
	if d == nil {
		t.Fatal("decision d-alias not found in the journal")
	}
	if _, ok := d.Assumptions["injected"]; ok {
		t.Errorf("decision assumptions mutated via the result: %v", d.Assumptions)
	}
	if len(d.Excluded) != 1 || d.Excluded[0] != "pod:a" {
		t.Errorf("decision excluded mutated via the result: %v", d.Excluded)
	}
}

// TestEstimateDefaultIdDistinguishesCounterfactuals pins the default decision id apart
// for a baseline and for the counterfactuals asked at the same revision. Neither Decide
// nor Commit advances the revision, so "est-<rev>-<target>" alone would be the same
// string for all three and the journal's last writer would silently replace the others'
// record — a replay would then answer with assumptions the caller never made.
func TestEstimateDefaultIdDistinguishesCounterfactuals(t *testing.T) {
	m, _ := estimateFixture(t)
	rev := m.Revision()

	plain := m.Estimate(EstimateRequest{Target: "pressure"})
	assumed := m.Estimate(EstimateRequest{Target: "pressure", Assume: map[string]float64{"queue@pod:a": 100}})
	without := m.Estimate(EstimateRequest{Target: "pressure", Without: []string{"pod:b"}})

	if m.Revision() != rev {
		t.Fatalf("revision moved from %d to %d; the collision this test guards needs all three at one revision", rev, m.Revision())
	}
	ids := map[string]string{"plain": plain.DecisionID, "assume": assumed.DecisionID, "without": without.DecisionID}
	seen := map[string]string{}
	for name, id := range ids {
		if id == "" {
			t.Fatalf("%s estimate produced no decision id", name)
		}
		if other, dup := seen[id]; dup {
			t.Errorf("%s and %s share decision id %q; each answer needs its own record", other, name, id)
		}
		seen[id] = name
	}
	if want := "est-" + strconv.FormatUint(rev, 10) + "-pressure"; plain.DecisionID != want {
		t.Errorf("plain estimate id %q; want the unchanged shape %q", plain.DecisionID, want)
	}

	// Each id must replay as its own answer, not as whichever was recorded last.
	for name, want := range map[string]map[string]float64{
		"plain":   {},
		"assume":  {"queue@pod:a": 100},
		"without": {"cpu@pod:b": 0},
	} {
		d, ok := m.Journal().Decision(ids[name])
		if !ok {
			t.Errorf("%s decision %q is not in the journal", name, ids[name])
			continue
		}
		if len(d.Assumptions) != len(want) {
			t.Errorf("%s decision %q replays with assumptions %v; want %v", name, ids[name], d.Assumptions, want)
			continue
		}
		for k, v := range want {
			if got, ok := d.Assumptions[k]; !ok || got != v {
				t.Errorf("%s decision %q replays with assumptions %v; want %v", name, ids[name], d.Assumptions, want)
			}
		}
	}
}

// TestEstimateRefusesAnEmptyWithout: Query.Subject "" means no restriction, so an
// empty exclusion passed through would take every property in the map — the target
// included — to its floor and report a confident projection of nothing.
func TestEstimateRefusesAnEmptyWithout(t *testing.T) {
	m, _ := estimateFixture(t)
	res := m.Estimate(EstimateRequest{Target: "pressure", Without: []string{""}})
	if res.Err == "" {
		t.Fatalf("empty without was accepted: assumptions=%v excluded=%v hypothetical=%+v",
			res.Assumptions, res.Excluded, res.Hypothetical)
	}
	if len(res.Assumptions) != 0 || res.Hypothetical != nil {
		t.Errorf("empty without floored properties anyway: %v", res.Assumptions)
	}
}
