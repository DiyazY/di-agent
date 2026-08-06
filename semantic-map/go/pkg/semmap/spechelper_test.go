package semmap_test

import (
	"github.com/DiyazY/di-agent/pkg/semmap"
	"testing"

	"github.com/DiyazY/di-agent/internal/minimal"
	"github.com/DiyazY/di-agent/pkg/contracts"
	"github.com/DiyazY/di-agent/pkg/domain"
	"github.com/DiyazY/di-agent/pkg/profiles"
	"github.com/DiyazY/di-agent/pkg/statemap"
)

// mustSpec loads the committed domain specification. Tests build their model from the
// same data artifact the daemon uses, so a change to the model's scope surfaces here
// rather than in a divergent test fixture.
func mustSpec() *domain.Spec {
	s, err := domain.LoadFound()
	if err != nil {
		panic("test setup: " + err.Error())
	}
	return s
}

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

// newMap builds a facade over a state model seeded from the spec — the wiring a daemon
// has. It returns the model too, because that is where the assertions belong: a test
// that checked a second structure was checking the copy, which is the arrangement these
// fixtures used to have.
func newMap(t *testing.T) (*semmap.SemanticMap, *statemap.Map) {
	t.Helper()
	return newMapWith(t, minimal.NewDisabledTuner())
}

// newMapWith is the same with a caller-chosen tuner, for the tests that drive operator
// intent through the facade.
func newMapWith(t *testing.T, tuner contracts.TunerContract) (*semmap.SemanticMap, *statemap.Map) {
	t.Helper()
	spec := mustSpec()
	state := statemap.New(statemap.Config{
		Owner:                   "test-node",
		ConvergenceObservations: 10,
		Alpha:                   0.5,
		AdmitUnknown:            true,
		Learn:                   true,
		LearnConfig:             statemap.LearnConfig{PairWindowSeconds: 15, MinSupport: 4, Window: 30},
	}, statemap.NewJournal(0))
	if _, err := profiles.SeedStateMap(state, spec, "", ""); err != nil {
		t.Fatalf("seeding the state model: %v", err)
	}

	sm := semmap.New(minimal.NewOntologyFromSpec(spec),
		minimal.NewRuleEngineReasoner(spec, 0.5, nil, nil),
		minimal.NewDisabledProposer(), tuner)
	sm.AttachState(state)
	return sm, state
}

// relationshipFor finds the state-model relationship carrying a proposition's ID.
func relationshipFor(t *testing.T, state *statemap.Map, propID string) statemap.Relationship {
	t.Helper()
	for _, r := range state.Relationships("", "") {
		if r.Label == propID {
			return r
		}
	}
	t.Fatalf("no relationship carries proposition %s", propID)
	return statemap.Relationship{}
}
