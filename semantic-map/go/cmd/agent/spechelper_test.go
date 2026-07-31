package main

import (
	"testing"

	"github.com/DiyazY/di-agent/pkg/domain"
)

// mustSpec loads the committed domain specification so tests boot the agent from
// the same data artifact the daemon uses. A change to the model's scope therefore
// surfaces in these tests rather than in a divergent fixture.
func mustSpec(t *testing.T) *domain.Spec {
	t.Helper()
	s, err := domain.LoadFound()
	if err != nil {
		t.Fatalf("test setup: %v", err)
	}
	return s
}

// firstProp returns a proposition ID that exists in the loaded spec, for tests
// that need to name one without caring which.
func firstProp(t *testing.T) string {
	t.Helper()
	s := mustSpec(t)
	if len(s.Propositions) == 0 {
		t.Fatal("domain spec declares no propositions")
	}
	return s.Propositions[0].PropositionID
}

// pairFrom and pairTo name an endpoint pair that carries at least one edge in the
// loaded spec, for tests that need a real pair without depending on which.
func pairFrom(t *testing.T) string {
	t.Helper()
	return mustSpec(t).Propositions[0].FromConstruct
}

func pairTo(t *testing.T) string {
	t.Helper()
	return mustSpec(t).Propositions[0].ToConstruct
}
