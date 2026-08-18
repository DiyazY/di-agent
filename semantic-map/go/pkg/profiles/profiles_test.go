package profiles

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// The two seeding tests that used to open this file inspected a storage graph that no
// longer exists: they called applyPriorWeights and seedFromOntology directly and
// checked the EdgeDescriptors that came out. Per-KD and global prior seeding are now
// verified where they matter, through Build and against the state model the agent
// reasons from — see TestBuildAppliesPerKDPriors and TestBuildWithoutKDUsesGlobalPriors
// in build_priors_test.go. Re-adding a direct seeder test would check the seeder
// against itself.

// TestValidateKD checks that unknown distribution names are rejected.
func TestValidateKD(t *testing.T) {
	pwPath := findPriorWeightsFile(t)
	raw, _ := os.ReadFile(pwPath)
	var pw priorWeightsFile
	_ = json.Unmarshal(raw, &pw)

	if err := validateKD(&pw, "k0s"); err != nil {
		t.Errorf("k0s should be valid; got %v", err)
	}
	if err := validateKD(&pw, ""); err != nil {
		t.Errorf("empty KD should be valid (skip per-KD seeding); got %v", err)
	}
	if err := validateKD(&pw, "nonexistent-distro"); err == nil {
		t.Error("expected error for unknown KD")
	}
	if err := validateKD(nil, "k0s"); err != nil {
		t.Errorf("nil priorWeights should make KD a no-op; got %v", err)
	}
}

// findPriorWeightsFile walks up from this package to locate prior_weights.json
// in the semantic-map directory. Keeps the test independent of the test
// runner's working directory.
func findPriorWeightsFile(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// Walk up at most 6 levels looking for semantic-map/prior_weights.json.
	dir := wd
	for i := 0; i < 6; i++ {
		candidate := filepath.Join(dir, "prior_weights.json")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		candidate = filepath.Join(dir, "semantic-map", "prior_weights.json")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Skipf("prior_weights.json not found from %q — skipping numerical verification", wd)
	return ""
}

func almostEqual(a, b, eps float64) bool {
	return math.Abs(a-b) <= eps
}
