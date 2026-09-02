package semmap_test

import (
	"testing"

	"github.com/DiyazY/di-agent/pkg/statemap"
	"github.com/DiyazY/di-agent/pkg/types"
)

// A declaration and the model are two records of the same graph, and every mutation of
// the declaration has to reach the model — because the model is what every answer is
// read from.
//
// This is the invariant these tests have always guarded; what counted as "the model"
// changed. It used to be a storage graph, and AddValidatedProposition and AddConstruct
// wrote only the ontology, so a confirmed candidate appeared in Propositions() and in
// no traversal: a propose-then-confirm flow that appeared to succeed and changed
// nothing. The same failure is available today one layer over, so the assertions now
// look at the state model.

func TestAddValidatedPropositionReachesTheModel(t *testing.T) {
	m, state := newMap(t)

	before, err := m.AllEdges()
	if err != nil {
		t.Fatal(err)
	}

	// A self-loop is refused by the state model, so relate two distinct constructs —
	// which is also what a confirmed candidate always is.
	spec := mustSpec()
	if len(spec.Constructs) < 2 {
		t.Skip("spec declares fewer than two constructs")
	}
	p := &types.Proposition{
		PropositionID: "P90",
		FromConstruct: spec.Constructs[0].ConstructID,
		ToConstruct:   spec.Constructs[1].ConstructID,
		Direction:     types.Positive,
		PriorStrength: 0.42,
	}
	if err := m.AddValidatedProposition(p); err != nil {
		t.Fatalf("AddValidatedProposition: %v", err)
	}

	after, err := m.AllEdges()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("relation count %d -> %d, want +1: the proposition was added to the "+
			"declaration but no relationship reached the state model, so no answer can "+
			"traverse it", len(before), len(after))
	}

	rel := relationshipFor(t, state, "P90")
	// Cold-start invariant: a freshly declared relationship carries no evidence and
	// therefore no strength. The strength a declaration used to carry is gone — adding a
	// proposition asserts that two properties relate, not what the relation is worth.
	if rel.Confidence != 0 || rel.NObservations != 0 {
		t.Errorf("new relationship should carry no evidence, got confidence=%v n=%d",
			rel.Confidence, rel.NObservations)
	}
	if v, known := rel.Effective(); known {
		t.Errorf("effective strength %v on a freshly declared relationship; want unknown", v)
	}
	if rel.Basis() != "unknown" {
		t.Errorf("basis %q at cold start, want unknown", rel.Basis())
	}
	if rel.Provenance != statemap.Seeded {
		t.Errorf("provenance %s on a relationship nothing has observed, want seeded",
			rel.Provenance)
	}
}

func TestAddConstructReachesTheModel(t *testing.T) {
	m, state := newMap(t)

	c := &types.Construct{ConstructID: "AU", Name: "AuditConstruct"}
	if err := m.AddConstruct(c); err != nil {
		t.Fatalf("AddConstruct: %v", err)
	}

	p, ok := state.Property("AU")
	if !ok {
		t.Fatal("construct added to the declaration but no property in the state model, " +
			"so nothing can be said about it")
	}
	// Observed, not derived: nothing routes to a construct added at runtime, and a
	// derived property with no members would be a summary of nothing.
	if p.Kind != statemap.Observed {
		t.Errorf("construct property is %s, want observed while nothing routes to it", p.Kind)
	}
	if p.Confidence != 0 || p.NObservations != 0 {
		t.Errorf("a construct nothing routes to yet reports confidence %v from %d "+
			"observations, want zero of both", p.Confidence, p.NObservations)
	}
}

func TestDeprecateRetiresInTheModel(t *testing.T) {
	m, state := newMap(t)

	if err := m.Deprecate(specProp(t), "retired for this test"); err != nil {
		t.Fatalf("Deprecate: %v", err)
	}

	rel := relationshipFor(t, state, specProp(t))
	if rel.Status != statemap.Retired {
		t.Errorf("relationship status %s after deprecation; a claim that stays active "+
			"keeps contributing to every answer", rel.Status)
	}
	if rel.RetiredReason == "" {
		t.Error("retirement recorded no reason, so the withdrawal cannot be audited")
	}

	// Soft delete: it stays retrievable and shows on the graph surfaces as deprecated,
	// because a decision taken before the retirement must remain reconstructible.
	edges, err := m.AllEdges()
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, e := range edges {
		if e.PropositionID != specProp(t) {
			continue
		}
		seen = true
		if !e.Deprecated {
			t.Error("the graph surface does not report the relation as deprecated")
		}
		if e.DeprecatedReason == "" {
			t.Error("the graph surface reports no reason for the deprecation")
		}
	}
	if !seen {
		t.Error("the relation vanished from the graph surface; deprecation is a soft delete")
	}
}
