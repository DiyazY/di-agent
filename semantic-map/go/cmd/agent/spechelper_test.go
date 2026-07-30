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
