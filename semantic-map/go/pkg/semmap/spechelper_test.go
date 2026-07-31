package semmap

import (
	"testing"

	"github.com/DiyazY/di-agent/pkg/domain"
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
