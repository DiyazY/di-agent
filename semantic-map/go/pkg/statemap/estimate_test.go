package statemap

import (
	"encoding/json"
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
	_ = m.Record(Observation{ID: "x@pod:1", Value: 0.5, At: c.now(), Subject: "pod:1"}) // no declared range
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
	_ = m.Record(Observation{ID: "x@pod:1", Value: 0.5, At: c.now(), Subject: "pod:1"}) // no declared range
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

func anyCaveat(caveats []string, substr string) bool {
	for _, c := range caveats {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

// withUnmeasuredEdge adds a third source whose relationship to pressure is declared
// but has no strength yet.
func withUnmeasuredEdge(t *testing.T) *Map {
	t.Helper()
	m, c := estimateFixture(t)
	unit := [2]float64{0, 1}
	_ = m.Record(Observation{ID: "io@pod:c", Value: 0.4, At: c.now(), Subject: "pod:c", Range: &unit})
	if err := m.DeclareRelationship(Relationship{From: "io@pod:c", To: "pressure", Sign: 1, Label: "d"}); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestEstimateCaveatsUnmeasuredInfluencesEvenWithoutAssumptions: an influence with no
// strength is left out of the sensitivity and the contributions. Leaving it out of the
// caveats too, whenever no assumption was made, reported "3 influences" computed from
// two with nothing to say so.
func TestEstimateCaveatsUnmeasuredInfluencesEvenWithoutAssumptions(t *testing.T) {
	m := withUnmeasuredEdge(t)
	res := m.Estimate(EstimateRequest{Target: "pressure"})
	if !anyCaveat(res.Caveats, "no strength yet") {
		t.Errorf("baseline estimate omitted an unmeasured influence from the arithmetic without saying so: %v", res.Caveats)
	}
}

// TestEstimateAssumptionOnAnUnmeasuredEdgeIsNotCalledUnrelated: the source relates to
// the target; what is missing is the strength. Saying it "does not influence" the
// target is the wrong claim.
func TestEstimateAssumptionOnAnUnmeasuredEdgeIsNotCalledUnrelated(t *testing.T) {
	m := withUnmeasuredEdge(t)
	res := m.Estimate(EstimateRequest{Target: "pressure", Assume: map[string]float64{"io@pod:c": 0.9}})
	if anyCaveat(res.Caveats, "does not influence") {
		t.Errorf("an assumption on a source with an unmeasured edge was reported as unrelated: %v", res.Caveats)
	}
	if !anyCaveat(res.Caveats, "io@pod:c") || !anyCaveat(res.Caveats, "no strength yet") {
		t.Errorf("the caveat must name the source and say the edge has no strength yet: %v", res.Caveats)
	}
}

// TestEstimateRefusesAReusedDecisionID: the journal keeps one record per id, so a
// second estimate under an id already used would silently replace the first.
func TestEstimateRefusesAReusedDecisionID(t *testing.T) {
	m, _ := estimateFixture(t)
	if first := m.Estimate(EstimateRequest{ID: "ask-1", Target: "pressure"}); first.Err != "" {
		t.Fatal(first.Err)
	}
	second := m.Estimate(EstimateRequest{ID: "ask-1", Target: "pressure", Assume: map[string]float64{"cpu@pod:b": 0.9}})
	if second.Err == "" {
		t.Fatal("a reused decision id was accepted")
	}
	d, ok := m.Journal().Decision("ask-1")
	if !ok || len(d.Assumptions) != 0 {
		t.Errorf("the first decision was replaced: ok=%v assumptions=%v", ok, d.Assumptions)
	}
}

// TestEstimateRefusesNonFiniteAssumptions: Record refuses NaN and Inf readings; an
// assumption is a reading the caller supposes, held to the same rule.
func TestEstimateRefusesNonFiniteAssumptions(t *testing.T) {
	m, _ := estimateFixture(t)
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if res := m.Estimate(EstimateRequest{Target: "pressure", Assume: map[string]float64{"cpu@pod:b": v}}); res.Err == "" {
			t.Errorf("assumption %v accepted; delta=%v", v, res.Hypothetical)
		}
	}
}

// TestEstimateWithoutASingleProperty: `without` may name one property rather than a
// whole subject; only that property goes to its floor.
func TestEstimateWithoutASingleProperty(t *testing.T) {
	m, _ := estimateFixture(t)
	res := m.Estimate(EstimateRequest{Target: "pressure", Without: []string{"queue@pod:a"}})
	if res.Err != "" || res.Hypothetical == nil || !near(res.Hypothetical.Delta, -0.4) {
		t.Fatalf("err=%q hypothetical=%+v; want delta -0.4 from queue alone", res.Err, res.Hypothetical)
	}
	if len(res.Excluded) != 1 || res.Excluded[0] != "queue@pod:a" {
		t.Errorf("excluded %v; want [queue@pod:a]", res.Excluded)
	}
	if _, floored := res.Assumptions["cpu@pod:b"]; floored {
		t.Error("excluding one property floored another subject's property")
	}
}

// TestEstimateWithoutNothingKnownIsACaveatNotAnExclusion: a name that matches
// neither a property nor a subject changes nothing and says so.
func TestEstimateWithoutNothingKnownIsACaveatNotAnExclusion(t *testing.T) {
	m, _ := estimateFixture(t)
	res := m.Estimate(EstimateRequest{Target: "pressure", Without: []string{"pod:zzz"}})
	if res.Err != "" || res.Hypothetical != nil || len(res.Excluded) != 0 {
		t.Errorf("err=%q hypothetical=%+v excluded=%v; want a plain baseline", res.Err, res.Hypothetical, res.Excluded)
	}
	if !anyCaveat(res.Caveats, "pod:zzz names nothing") {
		t.Errorf("caveats %v; want one saying pod:zzz names nothing in the map", res.Caveats)
	}
}

// TestEstimateAssumingADerivedSourceSaysSo: a derived property assumed directly
// leaves its members where they are, which the answer must state.
func TestEstimateAssumingADerivedSourceSaysSo(t *testing.T) {
	m, _ := estimateFixture(t)
	if err := m.DeclareProperty(Property{ID: "agg", Kind: Derived, Members: []string{"cpu@pod:b"}}); err != nil {
		t.Fatal(err)
	}
	if err := m.DeclareRelationship(Relationship{From: "agg", To: "pressure", Sign: 1, Label: "d"}); err != nil {
		t.Fatal(err)
	}
	_ = m.AssertRelationshipStrength(RelationshipID("agg", "pressure", "d"), 0.5, "op", "fixture")
	res := m.Estimate(EstimateRequest{Target: "pressure", Assume: map[string]float64{"agg": 0.9}})
	if res.Err != "" || !anyCaveat(res.Caveats, "agg is derived and was assumed directly") {
		t.Errorf("err=%q caveats %v; want the derived-assumed caveat", res.Err, res.Caveats)
	}
}

// TestEstimateProjectionClampsToTheDeclaredRange: a projection past the target's
// ceiling is reported at the ceiling, not beyond it.
func TestEstimateProjectionClampsToTheDeclaredRange(t *testing.T) {
	m, _ := estimateFixture(t)
	// queue 50→100 adds +0.4, cpu 0.2→1.0 adds +0.4: 0.3 + 0.8 would be 1.1 on [0,1].
	res := m.Estimate(EstimateRequest{Target: "pressure", Assume: map[string]float64{"queue@pod:a": 100, "cpu@pod:b": 1.0}})
	if res.Err != "" || res.Hypothetical == nil {
		t.Fatalf("err=%q hypothetical=%v", res.Err, res.Hypothetical)
	}
	if !near(res.Hypothetical.Delta, 0.8) || res.Hypothetical.ProjectedLevel != 1 {
		t.Errorf("delta %.3f projected %.3f; want delta 0.8 and the projection clamped to 1", res.Hypothetical.Delta, res.Hypothetical.ProjectedLevel)
	}
}

// TestEstimateUnmatchedAssumptionIsRecordedAndNamed: an assumption on a property
// that relates to the target through nothing is still part of the question and is
// said to be idle, by name.
func TestEstimateUnmatchedAssumptionIsRecordedAndNamed(t *testing.T) {
	m, _ := estimateFixture(t)
	res := m.Estimate(EstimateRequest{Target: "pressure", Assume: map[string]float64{"idle@pod:z": 0.5}})
	if !anyCaveat(res.Caveats, "assumption on idle@pod:z does not influence pressure") {
		t.Errorf("caveats %v; want the unmatched assumption named", res.Caveats)
	}
	if v, ok := res.Assumptions["idle@pod:z"]; !ok || v != 0.5 {
		t.Errorf("assumptions %v; want the unmatched assumption recorded as asked", res.Assumptions)
	}
}

// TestJournalDecisionIsACopy: the journal hands out its audit record; a caller
// writing into it must not change what the journal remembers.
func TestJournalDecisionIsACopy(t *testing.T) {
	m, _ := estimateFixture(t)
	res := m.Estimate(EstimateRequest{ID: "ask-copy", Target: "pressure", Assume: map[string]float64{"cpu@pod:b": 0.9}})
	if res.Err != "" {
		t.Fatal(res.Err)
	}
	d, _ := m.Journal().Decision("ask-copy")
	d.Assumptions["cpu@pod:b"] = 42
	d.Excluded = append(d.Excluded, "tampered")
	if len(d.PropertiesRead) > 0 {
		d.PropertiesRead[0].Labels = map[string]string{"tampered": "yes"}
	}
	again, _ := m.Journal().Decision("ask-copy")
	if again.Assumptions["cpu@pod:b"] != 0.9 || len(again.Excluded) != 0 {
		t.Errorf("the journal's record changed through a returned copy: %+v", again)
	}
	if len(again.PropertiesRead) > 0 && again.PropertiesRead[0].Labels["tampered"] != "" {
		t.Error("the journal's recorded properties changed through a returned copy")
	}
}

// TestInfluenceJSONKeepsAZeroStrength: a learned zero and "no strength yet" are
// different facts; omitempty dropped the field for the first and made it look like
// the second.
func TestInfluenceJSONKeepsAZeroStrength(t *testing.T) {
	b, err := json.Marshal(Influence{Relationship: "r", Source: "s", Known: true, Strength: 0, Contribution: 0})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"effective_strength":0`, `"contribution":0`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("%s lacks %s", b, want)
		}
	}
}
