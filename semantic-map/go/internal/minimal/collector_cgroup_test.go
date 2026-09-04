package minimal

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/types"
)

const testUID = "8f3c1234-aaaa-bbbb-cccc-1234567890ab"

func writeCgroup(t *testing.T, dir string, usageUsec, periods, throttled uint64, memCurrent string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cpu := "usage_usec " + strconv.FormatUint(usageUsec, 10) + "\nnr_periods " + strconv.FormatUint(periods, 10) +
		"\nnr_throttled " + strconv.FormatUint(throttled, 10) + "\n"
	for name, content := range map[string]string{"cpu.stat": cpu, "memory.current": memCurrent + "\n", "memory.max": "max\n", "cgroup.procs": "4242\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// fakeTree builds a root with one systemd pod (plus a container scope under it), one
// cgroupfs pod, and one systemd unit.
func fakeTree(t *testing.T) (root, podA, podB, unit string) {
	t.Helper()
	root = t.TempDir()
	writeCgroup(t, root, 10_000_000, 0, 0, "2147483648")
	podA = filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice", "kubepods-burstable-pod8f3c1234_aaaa_bbbb_cccc_1234567890ab.slice")
	writeCgroup(t, podA, 1_000_000, 100, 10, "268435456")
	writeCgroup(t, filepath.Join(podA, "cri-containerd-abc.scope"), 900_000, 100, 10, "200000000")
	podB = filepath.Join(root, "kubepods", "besteffort", "pod"+"11111111-2222-3333-4444-555555555555")
	writeCgroup(t, podB, 500_000, 0, 0, "134217728")
	unit = filepath.Join(root, "system.slice", "k0sworker.service")
	writeCgroup(t, unit, 2_000_000, 0, 0, "67108864")
	return
}

func advance(t *testing.T, dir string, usageUsec, periods, throttled uint64) {
	t.Helper()
	cpu := "usage_usec " + strconv.FormatUint(usageUsec, 10) + "\nnr_periods " + strconv.FormatUint(periods, 10) +
		"\nnr_throttled " + strconv.FormatUint(throttled, 10) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "cpu.stat"), []byte(cpu), 0o644); err != nil {
		t.Fatal(err)
	}
}

func bySubject(samples []*types.MetricSample) map[string]map[types.MetricType]*types.MetricSample {
	out := map[string]map[types.MetricType]*types.MetricSample{}
	for _, s := range samples {
		if out[s.Subject] == nil {
			out[s.Subject] = map[types.MetricType]*types.MetricSample{}
		}
		out[s.Subject][s.MetricType] = s
	}
	return out
}

func TestCollectWalksPodsAsSubjects(t *testing.T) {
	root, podA, podB, _ := fakeTree(t)
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, MaxSubjects: 256, MemTotalBytes: 4 << 30})
	c.numCPU = 4
	if _, err := c.Collect(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	advance(t, podA, 1_000_000+40_000, 110, 15) // +40ms cpu over ~20ms wall on 4 cpus ≈ 0.5 share
	advance(t, podB, 500_000, 0, 0)
	samples, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	got := bySubject(samples)
	a := got["pod:"+testUID]
	if a == nil {
		t.Fatalf("pod A missing; subjects=%v", keys(got))
	}
	if a[types.MemoryUtilization] == nil || a[types.MemoryUtilization].Value != float64(268435456)/float64(4<<30) {
		t.Errorf("pod A memory share %v; want memory.current / MemTotal", a[types.MemoryUtilization])
	}
	if s := a[types.CPUUtilization]; s == nil || s.Value <= 0 || s.Value > 1 || s.Unit != "share-of-node-capacity" || s.Range == nil || s.Labels["qos"] != "burstable" {
		t.Errorf("pod A cpu sample %+v; want a share in (0,1] with declared unit/range and labels", s)
	}
	if s := a[types.CPUThrottleRatio]; s == nil || s.Value != 0.5 {
		t.Errorf("pod A throttle %v; want 5/10 = 0.5", s)
	}
	if got["pod:11111111-2222-3333-4444-555555555555"][types.CPUThrottleRatio] != nil {
		t.Error("a pod whose periods never advance must not get a throttle property")
	}
	if _, ok := got[""]; !ok {
		t.Error("the node-level (root) samples must still be emitted")
	}
	for subj := range got {
		if subj != "" && subj[:4] != "pod:" {
			t.Errorf("unexpected subject %q: units are off by default and container scopes are never subjects", subj)
		}
	}
}

func TestCollectUnitsAllowlistAndCap(t *testing.T) {
	root, _, _, _ := fakeTree(t)
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, UnitGlobs: []string{"k0s*.service"}, MaxSubjects: 1, MemTotalBytes: 4 << 30})
	samples, _ := c.Collect()
	got := bySubject(samples)
	delete(got, "")
	if len(got) != 1 {
		t.Errorf("cap of 1 subject not enforced: %v", keys(got))
	}
	c2 := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, UnitGlobs: []string{"k0s*.service"}, MaxSubjects: 256, MemTotalBytes: 4 << 30})
	samples, _ = c2.Collect()
	if bySubject(samples)["unit:k0sworker.service"] == nil {
		t.Error("allowlisted unit was not a subject")
	}
}

func TestVanishedSubjectDropsItsSnapshotAndEmitsNothing(t *testing.T) {
	root, podA, _, _ := fakeTree(t)
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, MaxSubjects: 256, MemTotalBytes: 4 << 30})
	c.Collect()
	if err := os.RemoveAll(podA); err != nil {
		t.Fatal(err)
	}
	samples, _ := c.Collect()
	if bySubject(samples)["pod:"+testUID] != nil {
		t.Error("a vanished pod produced samples")
	}
	c.mu.Lock()
	_, still := c.prev["pod:"+testUID]
	c.mu.Unlock()
	if still {
		t.Error("the vanished subject's snapshot was kept")
	}
}

func TestRootMemoryUsesMemTotalWhenKnown(t *testing.T) {
	root, _, _, _ := fakeTree(t)
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{MemTotalBytes: 4 << 30})
	samples, _ := c.Collect()
	if s := bySubject(samples)[""][types.MemoryUtilization]; s == nil || s.Value != float64(2147483648)/float64(4<<30) {
		t.Errorf("root memory %v; want memory.current / MemTotal even though memory.max is 'max'", s)
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
