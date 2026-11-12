package statemap

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestAssociationsNeverBecomeCausalInfluences(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	for _, id := range []string{"a", "b"} {
		if err := m.Record(Observation{ID: id, Value: 0.5, At: c.now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.DeclareRelationship(Relationship{From: "b", To: "a", Label: "association", Sign: 1,
		Association: true, Provenance: Discovered}); err != nil {
		t.Fatal(err)
	}
	id := RelationshipID("a", "b", "association")
	if err := m.ObserveRelationship(id, 1, c.now()); err != nil {
		t.Fatal(err)
	}
	r, ok := m.Relationship(id)
	if !ok || !r.Association || r.Basis() != "association" {
		t.Fatalf("association lost: %+v", r)
	}
	if _, known := r.Effective(); known {
		t.Fatal("association offers causal strength")
	}
	if m.Covered("a", "b", 1) || !m.Associated("a", "b", 1) || !m.Associated("b", "a", 1) {
		t.Fatal("association confused with directional coverage")
	}
	if err := m.AssertRelationshipStrength(id, 1, "operator", "test"); err == nil {
		t.Fatal("association accepted a directional strength assertion")
	}
	if err := m.DeclareRelationship(Relationship{From: "a", To: "b", Label: "association", Sign: 1}); err == nil {
		t.Fatal("re-declaration silently promoted an association to an influence")
	}
	// Even a snapshot containing evidence layers must not gain causal authority.
	strength := 1.0
	loaded := Relationship{Association: true, Assertion: &strength, Measured: &strength, Established: &strength}
	if _, known := loaded.Effective(); known {
		t.Fatal("association's persisted layers gained causal authority")
	}
	explanation, err := m.Explain("b")
	if err != nil || !strings.Contains(explanation, "associated with (not causal influences)") ||
		strings.Contains(explanation, "  influenced by") {
		t.Fatalf("association described as a causal edge: %s %v", explanation, err)
	}
	for _, target := range []string{"a", "b"} {
		d := m.Decide("test-"+target, "estimate", "check influence")
		if len(d.RelationshipsInto(target)) != 0 {
			t.Fatal("association entered a decision as a directional influence")
		}
		res := m.Estimate(EstimateRequest{Target: target, Assume: map[string]float64{"a": 1}})
		if res.Answer.Contributions != 0 || res.Answer.Sensitivity != 0 {
			t.Fatalf("association changed an estimate: %+v", res)
		}
	}
	for i := 0; i < 2; i++ {
		if _, _, err := m.RecordIntervention(InterventionOutcome{From: "a", To: "b", Agreed: true}); err != nil {
			t.Fatal(err)
		}
	}
	if !m.Covered("a", "b", 1) {
		t.Fatal("association prevented creation of a separately supported directional edge")
	}
	r, _ = m.Relationship(id)
	if !r.Association || r.Provenance != Discovered || r.Interventions != nil {
		t.Fatalf("intervention mutated association semantics: %+v", r)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}
	restored, _ := newTestMap(t, Config{AdmitUnknown: true})
	if ok, err := restored.Load(path); err != nil || !ok {
		t.Fatalf("restore: %v %v", ok, err)
	}
	r, _ = restored.Relationship(id)
	if !r.Association {
		t.Fatal("association became directed on restart")
	}
}
