package semmap

import (
	"testing"

	"github.com/DiyazY/di-agent/internal/minimal"
	"github.com/DiyazY/di-agent/pkg/types"
)

// The ontology and storage are two records of the same graph. seedFromOntology
// syncs them at startup; every runtime mutation must keep them in sync too.
// Before these tests, only Deprecate did: AddValidatedProposition and
// AddConstruct wrote the ontology alone, so a confirmed candidate edge existed
// in Propositions() but not in AllEdges() — invisible to the Reasoner, which
// iterates edges. The result was a propose-then-confirm flow that appeared to
// succeed and changed nothing.

// newTestMap wires an edge-minimal SemanticMap and seeds storage from the
// ontology, mirroring what profiles.Build does at startup. Seeding matters here:
// without it AllEdges() is empty and a test asserting an edge survived
// deprecation would pass or fail for the wrong reason.
func newTestMap(t *testing.T) *SemanticMap {
	t.Helper()
	storage := minimal.NewInMemoryStorage()
	ontology := minimal.NewStaticDiSelectOntology()
	updater := minimal.NewEMAUpdater(storage, 0.2, 500)
	reasoner := minimal.NewRuleEngineReasoner(storage, ontology, 0.5, nil, nil)

	propositions, err := ontology.Propositions()
	if err != nil {
		t.Fatalf("Propositions: %v", err)
	}
	for _, p := range propositions {
		if err := storage.PutEdge(&types.EdgeDescriptor{
			FromID:        p.FromConstruct,
			ToID:          p.ToConstruct,
			PropositionID: p.PropositionID,
			Direction:     p.Direction,
			PriorWeight:   p.PriorStrength,
			EMAWeight:     p.PriorStrength,
		}); err != nil {
			t.Fatalf("PutEdge: %v", err)
		}
	}

	return New(storage, ontology, updater, reasoner,
		minimal.NewDisabledProposer(), minimal.NewRuleBasedTuner())
}

func TestAddValidatedPropositionMaterializesEdge(t *testing.T) {
	m := newTestMap(t)

	before, err := m.AllEdges()
	if err != nil {
		t.Fatal(err)
	}

	p := &types.Proposition{
		PropositionID: "P90",
		FromConstruct: "MU",
		ToConstruct:   "PS",
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
		t.Fatalf("edge count %d -> %d, want +1: the proposition was added to the "+
			"ontology but no EdgeDescriptor reached storage, so the Reasoner "+
			"cannot traverse it", len(before), len(after))
	}

	var found *types.EdgeDescriptor
	for _, e := range after {
		if e.PropositionID == "P90" {
			found = e
		}
	}
	if found == nil {
		t.Fatal("P90 edge absent from storage")
	}
	if found.PriorWeight != 0.42 {
		t.Errorf("PriorWeight = %v, want 0.42", found.PriorWeight)
	}
	// Cold-start invariant: a freshly seeded edge has EMA == prior and no
	// evidence, so effective(e) == prior until telemetry arrives.
	if found.EMAWeight != found.PriorWeight {
		t.Errorf("EMAWeight = %v, want == PriorWeight %v (cold start)",
			found.EMAWeight, found.PriorWeight)
	}
	if found.Confidence != 0 || found.NObservations != 0 {
		t.Errorf("new edge should carry no evidence, got confidence=%v n_obs=%d",
			found.Confidence, found.NObservations)
	}
}

func TestAddConstructMaterializesNode(t *testing.T) {
	m := newTestMap(t)

	c := &types.Construct{ConstructID: "AU", Name: "AuditConstruct"}
	if err := m.AddConstruct(c); err != nil {
		t.Fatalf("AddConstruct: %v", err)
	}

	node, err := m.storage.GetNode("AU")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if node == nil {
		t.Fatal("construct added to ontology but no NodeDescriptor in storage")
	}
	if node.ConstructType != "AuditConstruct" {
		t.Errorf("ConstructType = %q, want %q", node.ConstructType, "AuditConstruct")
	}
}

func TestDeprecateSyncsFlagToStorage(t *testing.T) {
	m := newTestMap(t)

	if err := m.Deprecate("P4", "no resilience telemetry"); err != nil {
		t.Fatalf("Deprecate: %v", err)
	}
	edges, err := m.AllEdges()
	if err != nil {
		t.Fatal(err)
	}
	// Soft delete: the edge stays, flagged. Retirement must not destroy the
	// evidence record accumulated before the operator retired the claim.
	seen := false
	for _, e := range edges {
		if e.PropositionID != "P4" {
			continue
		}
		seen = true
		if !e.Deprecated {
			t.Error("P4 edge not flagged deprecated in storage")
		}
		if e.DeprecatedReason == "" {
			t.Error("P4 edge missing DeprecatedReason")
		}
	}
	if !seen {
		t.Error("P4 edge removed from storage; deprecation must be a soft delete")
	}
}
