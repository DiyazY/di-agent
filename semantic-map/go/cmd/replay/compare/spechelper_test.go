package compare

import (
	"testing"

	"github.com/DiyazY/di-agent/pkg/domain"
)

// mustSpec loads the committed domain specification so the comparison harness
// tests build the same model the daemon does.
func mustSpec(t *testing.T) *domain.Spec {
	t.Helper()
	s, err := domain.LoadFound()
	if err != nil {
		t.Fatalf("test setup: %v", err)
	}
	return s
}
