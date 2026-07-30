package explain_test

import (
	"testing"

	"github.com/DiyazY/di-agent/pkg/domain"
)

// mustSpec loads the committed domain specification so tests build from the same
// data artifact the daemon uses.
func mustSpec(t *testing.T) *domain.Spec {
	t.Helper()
	s, err := domain.LoadFound()
	if err != nil {
		t.Fatalf("test setup: %v", err)
	}
	return s
}
