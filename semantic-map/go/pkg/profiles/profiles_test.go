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

// TestBuildWiresCgroupSubjectOptions checks that Config's CgroupSubjects/CgroupMaxSubjects
// fields actually reach the collector Build constructs, rather than only being accepted
// and silently dropped: a synthetic pod cgroup under CgroupRoot must show up as a
// pod:<uid> subject in the samples the built collector produces.
func TestBuildWiresCgroupSubjectOptions(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{root, filepath.Join(root, "kubepods.slice", "kubepods-pod8f3c1234_aaaa_bbbb_cccc_1234567890ab.slice")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		// memory.max carries a concrete limit rather than "max" (no limit) so the
		// subject's memory sample resolves from the cgroup files alone on the very
		// first Collect() call; falling back to "max" would make this test depend on
		// the host's /proc/meminfo (MemTotalBytes isn't wired through Config), which
		// is unset on non-Linux dev machines and would leave the ratio undefined.
		for name, content := range map[string]string{"cpu.stat": "usage_usec 1\nnr_periods 0\nnr_throttled 0\n", "memory.current": "1024\n", "memory.max": "1073741824\n"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	_, coll, err := Build("edge-minimal", Config{
		DomainSpec: mustSpec(), NodeID: "n1", CgroupRoot: root,
		CgroupSubjects: true, CgroupMaxSubjects: 4, EMAAlpha: 0.2, ConvergenceThreshold: 10, MinTrustScore: 0.5,
	})
	if err != nil {
		t.Fatal(err)
	}
	samples, _ := coll.Collect()
	var scoped bool
	for _, s := range samples {
		if s.Subject == "pod:8f3c1234-aaaa-bbbb-cccc-1234567890ab" {
			scoped = true
		}
	}
	if !scoped {
		t.Error("Build did not pass the subject options to the cgroup collector")
	}
}
