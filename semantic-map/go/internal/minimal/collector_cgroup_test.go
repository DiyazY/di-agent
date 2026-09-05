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

// nestedPodUID is a pod-shaped cgroup created INSIDE podA's directory, so the walk
// would recognise it if (and only if) it wrongly descended into an already-recognised
// subject. It must never appear as a subject.
const nestedPodUID = "22222222-3333-4444-5555-666666666666"

// fakeTree builds a root with one systemd pod (plus a container scope AND a nested
// pod-shaped cgroup under it), one cgroupfs pod, and one systemd unit.
func fakeTree(t *testing.T) (root, podA, podB, unit string) {
	t.Helper()
	root = t.TempDir()
	writeCgroup(t, root, 10_000_000, 0, 0, "2147483648")
	podA = filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice", "kubepods-burstable-pod8f3c1234_aaaa_bbbb_cccc_1234567890ab.slice")
	writeCgroup(t, podA, 1_000_000, 100, 10, "268435456")
	writeCgroup(t, filepath.Join(podA, "cri-containerd-abc.scope"), 900_000, 100, 10, "200000000")
	// A directory the recogniser WOULD accept as a pod subject, nested inside podA.
	// The walk must not descend into podA (already a recognised subject) to find it.
	writeCgroup(t, filepath.Join(podA, "kubepods-burstable-pod22222222_3333_4444_5555_666666666666.slice"), 100_000, 0, 0, "10000000")
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
	if _, ok := got["pod:"+nestedPodUID]; ok {
		t.Error("the walk must not descend into a recognised directory: the nested pod-shaped cgroup under podA must not become its own subject")
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

// TestSubjectWithoutMemoryAccountingStillReportsCPU covers the case where a systemd
// unit's cgroup has no memory.current/memory.max (memory accounting disabled for that
// unit) but does have cpu.stat. Losing memory must not discard the CPU samples: only
// the memory sample should be skipped.
func TestSubjectWithoutMemoryAccountingStillReportsCPU(t *testing.T) {
	root := t.TempDir()
	writeCgroup(t, root, 10_000_000, 0, 0, "2147483648")
	unit := filepath.Join(root, "system.slice", "k0sworker.service")
	if err := os.MkdirAll(unit, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unit, "cgroup.procs"), []byte("4242\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Deliberately no memory.current / memory.max in this directory.
	advance(t, unit, 1_000_000, 0, 0)

	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, UnitGlobs: []string{"k0s*.service"}, MaxSubjects: 256, MemTotalBytes: 4 << 30})
	if _, err := c.Collect(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	advance(t, unit, 1_040_000, 0, 0)
	samples, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	got := bySubject(samples)
	u := got["unit:k0sworker.service"]
	if u == nil {
		t.Fatalf("unit subject missing entirely because memory accounting is absent; subjects=%v", keys(got))
	}
	if u[types.CPUUtilization] == nil {
		t.Error("unit subject should still report cpu_utilization when memory.current/memory.max are absent")
	}
	if u[types.MemoryUtilization] != nil {
		t.Error("unit subject should have no memory_utilization sample when memory.current/memory.max are absent")
	}
}

// TestReadMemTotalParsesMeminfoAndFallsBackToZero covers readMemTotal directly: a
// well-formed meminfo file, a missing file, and a malformed MemTotal value.
func TestReadMemTotalParsesMeminfoAndFallsBackToZero(t *testing.T) {
	dir := t.TempDir()

	good := filepath.Join(dir, "meminfo_good")
	if err := os.WriteFile(good, []byte("MemTotal:       16384000 kB\nMemFree:          123456 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := readMemTotal(good), uint64(16384000*1024); got != want {
		t.Errorf("readMemTotal(good) = %d; want %d", got, want)
	}

	missing := filepath.Join(dir, "does-not-exist")
	if got := readMemTotal(missing); got != 0 {
		t.Errorf("readMemTotal(missing) = %d; want 0", got)
	}

	malformed := filepath.Join(dir, "meminfo_bad")
	if err := os.WriteFile(malformed, []byte("MemTotal:       notanumber kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readMemTotal(malformed); got != 0 {
		t.Errorf("readMemTotal(malformed) = %d; want 0", got)
	}
}

// TestCmdLabelReadsArgv0AndIgnoresEmpty covers cmdLabel directly: a normal argv0, an
// empty cmdline (must not yield the filepath.Base("") == "." bug), and a cgroup.procs
// with no PID.
func TestCmdLabelReadsArgv0AndIgnoresEmpty(t *testing.T) {
	root := t.TempDir()
	procRoot := t.TempDir()
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, CmdLabel: true, ProcRoot: procRoot, MemTotalBytes: 4 << 30})

	// Case 1: argv0 is /usr/bin/ffmpeg -> cmd=ffmpeg.
	dir1 := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir1, "cgroup.procs"), []byte("111\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pidDir1 := filepath.Join(procRoot, "111")
	if err := os.MkdirAll(pidDir1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pidDir1, "cmdline"), []byte("/usr/bin/ffmpeg\x00-i\x00in.mp4\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := c.cmdLabel(dir1), "ffmpeg"; got != want {
		t.Errorf("cmdLabel with argv0=/usr/bin/ffmpeg = %q; want %q", got, want)
	}

	// Case 2: empty cmdline -> no cmd label ("").
	dir2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir2, "cgroup.procs"), []byte("222\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pidDir2 := filepath.Join(procRoot, "222")
	if err := os.MkdirAll(pidDir2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pidDir2, "cmdline"), []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := c.cmdLabel(dir2); got != "" {
		t.Errorf("cmdLabel with empty cmdline = %q; want \"\" (not \".\")", got)
	}

	// Case 3: cgroup.procs has no PID -> no label.
	dir3 := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir3, "cgroup.procs"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := c.cmdLabel(dir3); got != "" {
		t.Errorf("cmdLabel with no PID in cgroup.procs = %q; want \"\"", got)
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
