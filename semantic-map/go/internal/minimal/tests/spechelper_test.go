package minimal_test

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

// firstSpecProp, specPair and firstSpecKeyword name elements that exist in the
// loaded spec, so scenario narration does not hardcode a graph scope.
func firstSpecProp() string { return mustSpec().Propositions[0].PropositionID }

func specPair() (string, string) {
	p := mustSpec().Propositions[0]
	return p.FromConstruct, p.ToConstruct
}

func firstSpecKeyword() string {
	s := mustSpec()
	if len(s.IntentRules) == 0 || len(s.IntentRules[0].Keywords) == 0 {
		return "performance"
	}
	return s.IntentRules[0].Keywords[0]
}
