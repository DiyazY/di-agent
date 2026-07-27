package explain

import (
	"testing"

	"github.com/DiyazY/di-agent/pkg/peers"
	"github.com/DiyazY/di-agent/pkg/profiles"
	"github.com/DiyazY/di-agent/pkg/semmap"
)

// plannerTestReader adapts a real edge-minimal SemanticMap to
// SemanticMapReader for in-package tests that exercise unexported helpers.
//
// This lives in export_test.go (compiled only under `go test`) so the
// production package keeps no dependency on pkg/profiles or pkg/semmap —
// the SemanticMapReader interface remains the only coupling.
type plannerTestReader struct{ *semmap.SemanticMap }

func (r plannerTestReader) Peers() *peers.Registry { return r.SemanticMap.Peers() }

func newPlannerTestReader(t *testing.T) SemanticMapReader {
	t.Helper()
	sm, _, err := profiles.Build("edge-minimal", profiles.Config{})
	if err != nil {
		t.Fatalf("profiles.Build: %v", err)
	}
	return plannerTestReader{sm}
}
