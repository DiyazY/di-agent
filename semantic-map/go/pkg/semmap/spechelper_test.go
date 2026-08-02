package semmap

import (
	"testing"

	"github.com/DiyazY/di-agent/internal/minimal"
	"github.com/DiyazY/di-agent/pkg/contracts"
	"github.com/DiyazY/di-agent/pkg/domain"
	"github.com/DiyazY/di-agent/pkg/types"
)

// mustSpec loads the committed domain specification. Tests build their ontology
// from the same data artifact the daemon uses, so a change to the model's scope
// surfaces here rather than in a divergent test fixture.
func mustSpec() *domain.Spec {
	s, err := domain.LoadFound()
	if err != nil {
		panic("test setup: " + err.Error())
	}
	return s
}

var _ = testing.Short

// specProp names a proposition that exists in the loaded spec.
func specProp(t *testing.T) string {
	t.Helper()
	s := mustSpec()
	if len(s.Propositions) == 0 {
		t.Fatal("domain spec declares no propositions")
	}
	return s.Propositions[0].PropositionID
}

func pairFrom(t *testing.T) string {
	t.Helper()
	return mustSpec().Propositions[0].FromConstruct
}

func pairTo(t *testing.T) string {
	t.Helper()
	return mustSpec().Propositions[0].ToConstruct
}

// newIdentityMap builds a minimal endpoint-mode map with storage seeded from the
// spec, for tests that care about ingestion behaviour rather than reasoning.
func newIdentityMap(t *testing.T) (*SemanticMap, *minimal.InMemoryStorage) {
	t.Helper()
	return buildMap(t, false)
}

// newRelationalMap is the same with the paired updater.
func newRelationalMap(t *testing.T) (*SemanticMap, *minimal.InMemoryStorage) {
	t.Helper()
	return buildMap(t, true)
}

func buildMap(t *testing.T, relational bool) (*SemanticMap, *minimal.InMemoryStorage) {
	t.Helper()
	spec := mustSpec()
	storage := minimal.NewInMemoryStorage()
	ontology := minimal.NewOntologyFromSpec(spec)

	for _, c := range spec.Constructs {
		if err := storage.PutNode(&types.NodeDescriptor{
			NodeID: c.ConstructID, ConstructType: c.Name,
			PriorValue: 0.5, EMAValue: 0.5,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range spec.Propositions {
		dir := types.Positive
		if p.Direction == "negative" {
			dir = types.Negative
		}
		if err := storage.PutEdge(&types.EdgeDescriptor{
			FromID: p.FromConstruct, ToID: p.ToConstruct, PropositionID: p.PropositionID,
			Direction: dir, PriorWeight: 0.5, EMAWeight: 0.5,
		}); err != nil {
			t.Fatal(err)
		}
	}

	var updater contracts.UpdaterContract = minimal.NewEMAUpdater(storage, 0.2, 500)
	if relational {
		updater = minimal.NewRelationalEMAUpdater(storage, 0.2, 500, 8, 60)
	}
	sm := New(storage, ontology, updater,
		minimal.NewRuleEngineReasoner(mustSpec(), 0.5, nil, nil),
		minimal.NewDisabledProposer(), minimal.NewDisabledTuner())
	return sm, storage
}
