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

// allSpecProps names every proposition the loaded specification declares, so a test
// that wants "the graph's relationships" tracks the shipped model instead of a
// remembered list. Hardcoded IDs here went stale silently when the graph was scoped
// down: the propositions vanished from the spec, these tests kept asking for them, and
// Go's test cache reported ok because the specification is read at runtime from a path
// outside the package directory.
func allSpecProps() []string {
	s := mustSpec()
	out := make([]string, 0, len(s.Propositions))
	for _, p := range s.Propositions {
		out = append(out, p.PropositionID)
	}
	return out
}

// conflictPair returns two propositions the specification declares over the same
// endpoints with opposite signs, and false when it declares none. A conflict pair is a
// property of a specification, not of the architecture, and the shipped one has no such
// pair: the two that used to form one were the same claim written twice against
// outcome measures of opposite polarity.
func conflictPair() (string, string, bool) {
	s := mustSpec()
	for i, a := range s.Propositions {
		for _, b := range s.Propositions[i+1:] {
			if a.FromConstruct == b.FromConstruct && a.ToConstruct == b.ToConstruct &&
				a.Direction != b.Direction {
				return a.PropositionID, b.PropositionID, true
			}
		}
	}
	return "", "", false
}
