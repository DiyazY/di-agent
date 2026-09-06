package profiles

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
		// The subject is proven through its CPU sample, which needs two Collect()
		// calls and no node memory capacity: MemTotalBytes is not wired through
		// Config, /proc/meminfo is absent on non-Linux dev machines, and without a
		// known capacity the collector deliberately emits no memory sample at all.
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
	if _, err := coll.Collect(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	pod := filepath.Join(root, "kubepods.slice", "kubepods-pod8f3c1234_aaaa_bbbb_cccc_1234567890ab.slice")
	if err := os.WriteFile(filepath.Join(pod, "cpu.stat"), []byte("usage_usec 5001\nnr_periods 0\nnr_throttled 0\n"), 0o644); err != nil {
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

// TestBuildWithScriptPathUsesTheSyntheticSystem checks that Config.ScriptPath makes
// Build wire the synthetic system in place of any real collector: the returned
// CollectorContract must be the scripted.SystemScript for the given scenario, and its
// first Collect() must emit a sample scoped to the scenario's subject.
func TestBuildWithScriptPathUsesTheSyntheticSystem(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(p, []byte(`{"name":"t","seed":1,"tick_seconds":10,"duration_seconds":600,
	  "node":{"node_cpu":{"coupling":"sum","base":0.1,"of":"cpu_utilization"}},
	  "subjects":[{"id":"pod:a","arrive":0,"properties":{"cpu_utilization":{"pattern":"constant","value":0.3}}}],
	  "expect":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, coll, err := Build("edge-minimal", Config{DomainSpec: mustSpec(), NodeID: "sim", ScriptPath: p,
		CgroupRoot: t.TempDir(), EMAAlpha: 0.2, ConvergenceThreshold: 10, MinTrustScore: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if coll == nil || !strings.HasPrefix(coll.SourceID(), "system-script:") {
		t.Fatalf("collector %v; want the synthetic system to replace the others when -script is set", coll)
	}
	samples, _ := coll.Collect()
	var scoped bool
	for _, s := range samples {
		scoped = scoped || s.Subject == "pod:a"
	}
	if !scoped {
		t.Error("the synthetic system emitted no scoped sample")
	}
}

// TestBuildRefusesAMissingScript: a bad -script path used to log and hand back a
// daemon with no collector, which then reported "collection loop disabled" — the
// message for a deliberately disabled loop — and served an empty map indefinitely.
func TestBuildRefusesAMissingScript(t *testing.T) {
	_, _, err := Build("edge-minimal", Config{DomainSpec: mustSpec(), NodeID: "sim",
		ScriptPath: filepath.Join(t.TempDir(), "missing.json"), CgroupRoot: t.TempDir(),
		EMAAlpha: 0.2, ConvergenceThreshold: 10, MinTrustScore: 0.5})
	if err == nil {
		t.Fatal("a missing -script produced a daemon with no collector instead of an error")
	}
}

// TestBuildRefusesACollectIntervalThatDisagreesWithTheTick: the script stamps
// simulated time one tick per Collect while the map sweeps on the wall clock. With
// -collect-interval above the tick every property goes stale and then retires while
// the script is still emitting; below it nothing ever goes stale. That is not a drift
// to warn about; it is a configuration the daemon must not start with.
func TestBuildRefusesACollectIntervalThatDisagreesWithTheTick(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(p, []byte(`{"name":"t","seed":1,"tick_seconds":10,"duration_seconds":600,
	  "node":{"node_cpu":{"coupling":"sum","base":0.1,"of":"cpu_utilization"}},
	  "subjects":[{"id":"pod:a","arrive":0,"properties":{"cpu_utilization":{"pattern":"constant","value":0.3}}}],
	  "expect":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	base := Config{DomainSpec: mustSpec(), NodeID: "sim", ScriptPath: p, CgroupRoot: t.TempDir(),
		EMAAlpha: 0.2, ConvergenceThreshold: 10, MinTrustScore: 0.5}
	mismatch := base
	mismatch.CollectInterval = 1 * time.Second
	if _, _, err := Build("edge-minimal", mismatch); err == nil || !strings.Contains(err.Error(), "collect-interval") {
		t.Fatalf("mismatched -collect-interval was accepted (err=%v); want a refusal that names the flag", err)
	}
	match := base
	match.CollectInterval = 10 * time.Second
	if _, coll, err := Build("edge-minimal", match); err != nil || coll == nil {
		t.Fatalf("matching interval: err=%v coll=%v; want the synthetic system", err, coll)
	}
}

// TestBuildRefusesAMalformedUnitGlob: a `-cgroup-units` pattern path.Match cannot
// parse matched nothing on every call and said nothing about it.
func TestBuildRefusesAMalformedUnitGlob(t *testing.T) {
	_, _, err := Build("edge-minimal", Config{DomainSpec: mustSpec(), NodeID: "n1", CgroupRoot: t.TempDir(),
		CgroupSubjects: true, CgroupUnitGlobs: []string{"[k0s"}, EMAAlpha: 0.2, ConvergenceThreshold: 10, MinTrustScore: 0.5})
	if err == nil || !strings.Contains(err.Error(), "[k0s") {
		t.Fatalf("err=%v; want a refusal naming the pattern", err)
	}
}
