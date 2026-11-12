package minimal

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

func writeMemoryStat(t *testing.T, dir string, refault, file, shmem uint64) {
	t.Helper()
	content := "workingset_refault_file " + strconv.FormatUint(refault, 10) +
		"\nfile " + strconv.FormatUint(file, 10) +
		"\nshmem " + strconv.FormatUint(shmem, 10) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "memory.stat"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// nestedPodUID is a pod-shaped cgroup created INSIDE podA's directory, so the walk
// would recognise it if (and only if) it wrongly descended into an already-recognised
// subject. It must never appear as a subject.
const nestedPodUID = "22222222-3333-4444-5555-666666666666"

// Fake /proc/meminfo values for the tree below: a node with 8 GiB of which 6 GiB is
// available, so the node memory share is (8192000 − 6144000) / 8192000 = 0.25.
const (
	fakeMemTotalKB     = 8192000
	fakeMemAvailableKB = 6144000
	fakeMemShare       = float64(fakeMemTotalKB-fakeMemAvailableKB) / float64(fakeMemTotalKB)
)

// writeRootCgroup writes a cgroup v2 hierarchy ROOT: cpu.stat, and deliberately no
// memory.current / memory.max. The kernel does not create memory files at the root of
// a v2 hierarchy, so a fixture that writes them there certifies a filesystem shape no
// real host can present.
func writeRootCgroup(t *testing.T, dir string, usageUsec, periods, throttled uint64) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cpu := "usage_usec " + strconv.FormatUint(usageUsec, 10) + "\nnr_periods " + strconv.FormatUint(periods, 10) +
		"\nnr_throttled " + strconv.FormatUint(throttled, 10) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "cpu.stat"), []byte(cpu), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeFakeProc writes a /proc tree holding just the meminfo the root's memory share
// is computed from.
func writeFakeProc(t *testing.T) string {
	t.Helper()
	procRoot := t.TempDir()
	meminfo := "MemTotal:       " + strconv.Itoa(fakeMemTotalKB) + " kB\n" +
		"MemFree:          123456 kB\n" +
		"MemAvailable:   " + strconv.Itoa(fakeMemAvailableKB) + " kB\n"
	if err := os.WriteFile(filepath.Join(procRoot, "meminfo"), []byte(meminfo), 0o644); err != nil {
		t.Fatal(err)
	}
	return procRoot
}

// fakeTree builds a root with one systemd pod (plus a container scope AND a nested
// pod-shaped cgroup under it), one cgroupfs pod, and one systemd unit, alongside a
// fake /proc the root's memory share comes from.
func fakeTree(t *testing.T) (root, podA, podB, unit, procRoot string) {
	t.Helper()
	root = t.TempDir()
	procRoot = writeFakeProc(t)
	writeRootCgroup(t, root, 10_000_000, 0, 0)
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
	root, podA, podB, _, procRoot := fakeTree(t)
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, MaxSubjects: 256, MemTotalBytes: 4 << 30, ProcRoot: procRoot})
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
	if s := a[types.CPUUtilization]; s == nil || s.Value <= 0 || s.Value > 1 || s.Unit != "fraction" || s.Range == nil || s.Labels["qos"] != "burstable" {
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
	root, _, _, _, procRoot := fakeTree(t)
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, UnitGlobs: []string{"k0s*.service"}, MaxSubjects: 1, MemTotalBytes: 4 << 30, ProcRoot: procRoot})
	samples, _ := c.Collect()
	got := bySubject(samples)
	delete(got, "")
	if len(got) != 1 {
		t.Errorf("cap of 1 subject not enforced: %v", keys(got))
	}
	c2 := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, UnitGlobs: []string{"k0s*.service"}, MaxSubjects: 256, MemTotalBytes: 4 << 30, ProcRoot: procRoot})
	samples, _ = c2.Collect()
	if bySubject(samples)["unit:k0sworker.service"] == nil {
		t.Error("allowlisted unit was not a subject")
	}
}

func TestVanishedSubjectDropsItsSnapshotAndEmitsNothing(t *testing.T) {
	root, podA, _, _, procRoot := fakeTree(t)
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, MaxSubjects: 256, MemTotalBytes: 4 << 30, ProcRoot: procRoot})
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

// TestRootMemoryComesFromMeminfoOnCgroupV2 pins where the node-level memory share
// comes from. The root of a cgroup v2 hierarchy has no memory.current / memory.max, so
// reading them there fails and the node would have no memory property at all — the one
// number an operator asking "how much memory is this machine using" expects. /proc
// answers it instead: (MemTotal − MemAvailable) / MemTotal.
func TestRootMemoryComesFromMeminfoOnCgroupV2(t *testing.T) {
	root, _, _, _, procRoot := fakeTree(t)
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{MemTotalBytes: 4 << 30, ProcRoot: procRoot})
	samples, _ := c.Collect()
	s := bySubject(samples)[""][types.MemoryUtilization]
	if s == nil {
		t.Fatal("the root emitted no memory sample; a cgroup v2 root has no memory files, so /proc/meminfo must answer")
	}
	if s.Value != fakeMemShare {
		t.Errorf("root memory share %v; want (MemTotal − MemAvailable) / MemTotal = %v", s.Value, fakeMemShare)
	}
	if s.Unit != "fraction" || s.Range == nil {
		t.Errorf("root memory sample %+v; want the unit the spec declares for the node's memory (fraction) and a range", s)
	}
}

// TestRootMemoryIsSilentWhenMeminfoIsUnreadable: no memory files at the root and no
// readable meminfo means the collector has no basis for a node memory share, and
// inventing one would be worse than not answering.
func TestRootMemoryIsSilentWhenMeminfoIsUnreadable(t *testing.T) {
	root, _, _, _, _ := fakeTree(t)
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{MemTotalBytes: 4 << 30, ProcRoot: t.TempDir()})
	samples, _ := c.Collect()
	if s := bySubject(samples)[""][types.MemoryUtilization]; s != nil {
		t.Errorf("root memory sample %+v; want none when neither the cgroup nor meminfo can say", s)
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

func TestMemoryAdmissionHeadroomReservesUnusedRequests(t *testing.T) {
	root, _, _, _, procRoot := fakeTree(t)
	podA := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice", "kubepods-burstable-pod8f3c1234_aaaa_bbbb_cccc_1234567890ab.slice")
	writeMemoryStat(t, podA, 10, 128<<20, 32<<20)
	req := &Requests{}
	req.Store(map[string]float64{testUID: float64(1 << 30), "pending-pod": float64(1 << 30)})
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{
		Subjects: true, MaxSubjects: 256, MemTotalBytes: uint64(fakeMemTotalKB) * 1024,
		ProcRoot: procRoot, Requests: req,
	})
	samples, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	s := bySubject(samples)[""][types.MemoryAdmissionHeadroomRatio]
	if s == nil {
		t.Fatal("no memory admission headroom sample")
	}
	available := float64(fakeMemAvailableKB) * 1024
	nonReclaimable := float64(268435456 - (128 << 20) + (32 << 20))
	reserved := (1<<30 - nonReclaimable) + (1 << 30)
	want := (available - float64(reserved)) / float64(fakeMemTotalKB*1024)
	if math.Abs(s.Value-want) > 1e-9 || s.Unit != "fraction" || s.Range == nil || *s.Range != [2]float64{0, 1} {
		t.Fatalf("headroom sample %+v; want share %.6f", s, want)
	}
}

func TestMemoryAdmissionHeadroomKeepsPageCacheReserved(t *testing.T) {
	root, podA, _, _, procRoot := fakeTree(t)
	const request = uint64(1 << 30)
	if err := os.WriteFile(filepath.Join(podA, "memory.current"), []byte(strconv.FormatUint(request, 10)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeMemoryStat(t, podA, 0, request, 0)
	req := &Requests{}
	req.Store(map[string]float64{testUID: float64(request)})
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{
		Subjects: true, MaxSubjects: 256, MemTotalBytes: uint64(fakeMemTotalKB) * 1024,
		ProcRoot: procRoot, Requests: req,
	})
	samples, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	s := bySubject(samples)[""][types.MemoryAdmissionHeadroomRatio]
	if s == nil {
		t.Fatal("no memory admission headroom sample")
	}
	want := (float64(fakeMemAvailableKB)*1024 - float64(request)) / float64(fakeMemTotalKB*1024)
	if math.Abs(s.Value-want) > 1e-9 {
		t.Fatalf("headroom sample %.6f; want %.6f with cache still reserved", s.Value, want)
	}
}

func TestMemoryAdmissionHeadroomReservesFullRequestWithoutReliableMemoryStat(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{name: "missing", setup: func(*testing.T, string) {}},
		{name: "inconsistent", setup: func(t *testing.T, pod string) {
			content := "workingset_refault_file 0\nfile 999999999\nshmem 1000000000\n"
			if err := os.WriteFile(filepath.Join(pod, "memory.stat"), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, podA, _, _, procRoot := fakeTree(t)
			tc.setup(t, podA)
			req := &Requests{}
			req.Store(map[string]float64{testUID: float64(1 << 30)})
			c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{
				Subjects: true, MaxSubjects: 256, MemTotalBytes: uint64(fakeMemTotalKB) * 1024,
				ProcRoot: procRoot, Requests: req,
			})
			samples, err := c.Collect()
			if err != nil {
				t.Fatal(err)
			}
			s := bySubject(samples)[""][types.MemoryAdmissionHeadroomRatio]
			if s == nil {
				t.Fatal("no memory admission headroom sample")
			}
			want := (float64(fakeMemAvailableKB)*1024 - float64(1<<30)) / float64(fakeMemTotalKB*1024)
			if math.Abs(s.Value-want) > 1e-9 {
				t.Fatalf("headroom sample %.6f; want %.6f with full request reserved", s.Value, want)
			}
		})
	}
}

func TestMemoryAdmissionHeadroomRequiresFreshRequests(t *testing.T) {
	root, _, _, _, procRoot := fakeTree(t)
	req := &Requests{}
	req.Store(map[string]float64{"pending-pod": float64(1 << 30)})
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{
		Subjects: true, MaxSubjects: 256, MemTotalBytes: uint64(fakeMemTotalKB) * 1024,
		ProcRoot: procRoot, Requests: req,
	})
	if samples, err := c.Collect(); err != nil {
		t.Fatal(err)
	} else if bySubject(samples)[""][types.MemoryAdmissionHeadroomRatio] == nil {
		t.Fatal("fresh runtime requests did not produce admission headroom")
	}
	req.MarkStale()
	samples, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if bySubject(samples)[""][types.MemoryAdmissionHeadroomRatio] != nil {
		t.Fatal("stale runtime requests produced a current admission headroom bound")
	}
}

func TestMemoryAdmissionHeadroomReservesFullRequestWhenPodIsUnread(t *testing.T) {
	root, podA, _, _, procRoot := fakeTree(t)
	req := &Requests{}
	req.Store(map[string]float64{testUID: float64(1 << 30)})
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{
		Subjects: true, MaxSubjects: 256, MemTotalBytes: uint64(fakeMemTotalKB) * 1024,
		ProcRoot: procRoot, Requests: req,
	})
	if _, err := c.Collect(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(podA, "cpu.stat")); err != nil {
		t.Fatal(err)
	}
	samples, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	s := bySubject(samples)[""][types.MemoryAdmissionHeadroomRatio]
	if s == nil {
		t.Fatal("no memory admission headroom sample")
	}
	available := float64(fakeMemAvailableKB) * 1024
	want := (available - (1 << 30)) / float64(fakeMemTotalKB*1024)
	if math.Abs(s.Value-want) > 1e-9 {
		t.Fatalf("headroom sample %v; want %.6f after reserving the full request", s.Value, want)
	}
}

// TestSubjectMemoryIsSilentWhenNodeCapacityIsUnknown: without MemTotal there is no
// share of node capacity to report. memory.current / memory.max is a share of the
// subject's own limit — a different quantity — and stamping it with this collector's
// unit would hand the estimator a number that is not what it says it is.
func TestSubjectMemoryIsSilentWhenNodeCapacityIsUnknown(t *testing.T) {
	root := t.TempDir()
	writeRootCgroup(t, root, 10_000_000, 0, 0)
	pod := filepath.Join(root, "kubepods", "besteffort", "pod11111111-2222-3333-4444-555555555555")
	writeCgroup(t, pod, 500_000, 0, 0, "67108864")
	if err := os.WriteFile(filepath.Join(pod, "memory.max"), []byte("134217728\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// No meminfo under ProcRoot and no MemTotalBytes: node capacity is unknown.
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, MaxSubjects: 8, ProcRoot: t.TempDir()})
	samples, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range samples {
		if s.Subject != "" && s.MetricType == types.MemoryUtilization {
			t.Errorf("subject %s emitted memory %v as %q with node capacity unknown; want no sample rather than a share of the pod's own limit",
				s.Subject, s.Value, s.Unit)
		}
	}
}

// TestThrottleRatioDeclaresItsOwnUnit: throttled periods over elapsed periods is a
// share of the subject's CFS periods, not a share of node capacity, and the estimator
// normalises by what the sample declares.
func TestThrottleRatioDeclaresItsOwnUnit(t *testing.T) {
	root, podA, _, _, procRoot := fakeTree(t)
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, MaxSubjects: 8, MemTotalBytes: 4 << 30, ProcRoot: procRoot})
	if _, err := c.Collect(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	advance(t, podA, 1_040_000, 110, 15)
	samples, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	var throttle *types.MetricSample
	for _, s := range samples {
		if s.Subject != "" && s.MetricType == types.CPUThrottleRatio {
			throttle = s
		}
	}
	if throttle == nil {
		t.Fatal("no throttle sample for the pod after two collects")
	}
	if throttle.Unit != "share-of-periods" || throttle.Range == nil || *throttle.Range != [2]float64{0, 1} {
		t.Errorf("throttle sample unit %q range %v; want share-of-periods on [0,1]", throttle.Unit, throttle.Range)
	}
}

// TestThrottleDeltaSurvivesACounterReset: usage is guarded against a counter that
// went backwards; the throttle delta was not. A cgroup recreated under the same
// subject between two ticks restarts both counters, and if nr_periods has already
// passed the old value while nr_throttled has not, the unsigned subtraction wraps to
// a maximal, plausible-looking cpu_throttle_ratio of 1.0.
func TestThrottleDeltaSurvivesACounterReset(t *testing.T) {
	root, podA, _, _, procRoot := fakeTree(t)
	advance(t, podA, 1_000_000, 1000, 900)
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, MaxSubjects: 8, MemTotalBytes: 4 << 30, ProcRoot: procRoot})
	if _, err := c.Collect(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	advance(t, podA, 1_040_000, 1200, 10) // recreated: periods ran past, throttled restarted
	samples, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range samples {
		if s.Subject == "pod:"+testUID && s.MetricType == types.CPUThrottleRatio {
			t.Errorf("throttle ratio %v emitted across a counter reset; want no sample rather than a wrapped delta", s.Value)
		}
	}
}

func captureLogs(opts CgroupOptions) (CgroupOptions, *[]string) {
	var lines []string
	opts.Logf = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	return opts, &lines
}

// TestUnreadableRootIsLoggedOnce: a cgroup v1 host, or a container whose
// /sys/fs/cgroup is not the host's, leaves the root's cpu.stat unreadable. The
// collector then produces no node properties forever, and nothing said so.
func TestUnreadableRootIsLoggedOnce(t *testing.T) {
	root := t.TempDir() // no cpu.stat at all
	opts, lines := captureLogs(CgroupOptions{MemTotalBytes: 4 << 30, ProcRoot: t.TempDir()})
	c := NewCgroupCollectorWithOptions("n1", root, opts)
	for i := 0; i < 3; i++ {
		if _, err := c.Collect(); err != nil {
			t.Fatal(err)
		}
	}
	if len(*lines) != 1 || !strings.Contains((*lines)[0], "cpu.stat") {
		t.Errorf("unreadable root logged %d times (%v); want exactly one line naming cpu.stat", len(*lines), *lines)
	}
}

// TestRootWithoutPodCgroupsIsLoggedOnce: subjects on and no kubepods directory under
// the root means the agent is probably reading its own container's cgroup as the
// node — the one deployment mistake that yields plausible numbers about the wrong
// thing. It must be said once.
func TestRootWithoutPodCgroupsIsLoggedOnce(t *testing.T) {
	root := t.TempDir()
	writeRootCgroup(t, root, 10_000_000, 0, 0)
	opts, lines := captureLogs(CgroupOptions{Subjects: true, MaxSubjects: 8, MemTotalBytes: 4 << 30, ProcRoot: t.TempDir()})
	c := NewCgroupCollectorWithOptions("n1", root, opts)
	for i := 0; i < 3; i++ {
		if _, err := c.Collect(); err != nil {
			t.Fatal(err)
		}
	}
	if len(*lines) != 1 || !strings.Contains((*lines)[0], "kubepods") {
		t.Errorf("missing pod cgroups logged %d times (%v); want exactly one line naming kubepods", len(*lines), *lines)
	}
}

// TestSubjectCapTruncationIsVisible: the cap used to log once per process and the
// census then under-counted with no trace. The skipped count is logged whenever it
// changes, so a growing node keeps saying how much it is not showing.
func TestSubjectCapTruncationIsVisible(t *testing.T) {
	root, _, _, _, procRoot := fakeTree(t) // two pods
	opts, lines := captureLogs(CgroupOptions{Subjects: true, MaxSubjects: 1, MemTotalBytes: 4 << 30, ProcRoot: procRoot})
	c := NewCgroupCollectorWithOptions("n1", root, opts)
	_, _ = c.Collect()
	_, _ = c.Collect()
	if len(*lines) != 1 || !strings.Contains((*lines)[0], "1 ") {
		t.Fatalf("cap truncation logged %d times (%v); want one line while 1 subject is skipped", len(*lines), *lines)
	}
	writeCgroup(t, filepath.Join(root, "kubepods", "besteffort", "pod99999999-2222-3333-4444-555555555555"), 500_000, 0, 0, "1024")
	_, _ = c.Collect()
	if len(*lines) != 2 || !strings.Contains((*lines)[1], "2 ") {
		t.Errorf("after a third pod appeared the skipped count changed to 2 and was not logged: %v", *lines)
	}
}

// fakeClock returns a clock the tests advance instead of sleeping.
func fakeClock(start time.Time) (func() time.Time, func(d time.Duration)) {
	now := start
	return func() time.Time { return now }, func(d time.Duration) { now = now.Add(d) }
}

// TestCPUStatWithoutUsageIsNotAMeasurement: a cpu.stat with no usage_usec key
// parsed to 0 and, two ticks later, emitted cpu_utilization = 0 as a reading.
func TestCPUStatWithoutUsageIsNotAMeasurement(t *testing.T) {
	root, podA, podB, _, procRoot := fakeTree(t)
	if err := os.WriteFile(filepath.Join(podB, "cpu.stat"), []byte("nr_periods 100\nnr_throttled 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	now, advanceClock := fakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, MaxSubjects: 8, MemTotalBytes: 4 << 30, ProcRoot: procRoot, Now: now})
	if _, err := c.Collect(); err != nil {
		t.Fatal(err)
	}
	advanceClock(10 * time.Second)
	advance(t, podA, 1_040_000, 110, 15)
	samples, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	got := bySubject(samples)
	if got["pod:"+testUID][types.CPUUtilization] == nil {
		t.Error("the pod with usage_usec emitted no cpu_utilization after 10 s on the injected clock")
	}
	for subject, m := range got {
		if subject != "pod:"+testUID && subject != "" && m[types.CPUUtilization] != nil {
			t.Errorf("subject %s emitted cpu_utilization %v from a cpu.stat with no usage_usec", subject, m[types.CPUUtilization].Value)
		}
	}
}

// TestDeepPodShapedDirectoryIsNotASubject: the walk is depth-bounded; a pod-shaped
// name four levels down is not a subject however it is named.
func TestDeepPodShapedDirectoryIsNotASubject(t *testing.T) {
	root, _, _, _, procRoot := fakeTree(t)
	deep := filepath.Join(root, "kubepods.slice", "x.slice", "y.slice", "z.slice", "kubepods-pod33333333_4444_5555_6666_777777777777.slice")
	writeCgroup(t, deep, 100_000, 0, 0, "1024")
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, MaxSubjects: 8, MemTotalBytes: 4 << 30, ProcRoot: procRoot})
	samples, _ := c.Collect()
	if bySubject(samples)["pod:33333333-4444-5555-6666-777777777777"] != nil {
		t.Error("a pod-shaped directory beyond the depth bound became a subject")
	}
}

// TestSubjectWithUnreadableCPUStatIsAbsentThisTick: unreadable files are transient
// per contract — the subject is simply absent from the batch, with no error.
func TestSubjectWithUnreadableCPUStatIsAbsentThisTick(t *testing.T) {
	root, _, podB, _, procRoot := fakeTree(t)
	if err := os.Remove(filepath.Join(podB, "cpu.stat")); err != nil {
		t.Fatal(err)
	}
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, MaxSubjects: 8, MemTotalBytes: 4 << 30, ProcRoot: procRoot})
	samples, err := c.Collect()
	if err != nil {
		t.Fatalf("an unreadable subject must not be an error: %v", err)
	}
	if bySubject(samples)["pod:11111111-2222-3333-4444-555555555555"] != nil {
		t.Error("a subject whose cpu.stat is unreadable emitted samples")
	}
	if bySubject(samples)["pod:"+testUID] == nil {
		t.Error("the readable subject beside it went missing too")
	}
}

func writePressure(t *testing.T, dir string, someTotalUsec uint64) {
	t.Helper()
	writePSI(t, dir, "cpu.pressure", someTotalUsec)
}

// TestCPUPressureIsTheStallShareOfTheWindow: cpu_pressure_ratio is the delta of
// PSI's cumulative "some" stall time over the elapsed window, for the node and for
// each subject, declared as "fraction" on [0,1]. A cgroup without cpu.pressure
// (a kernel booted with psi=0) yields no sample rather than a zero.
func TestCPUPressureIsTheStallShareOfTheWindow(t *testing.T) {
	root, podA, podB, _, procRoot := fakeTree(t)
	writePressure(t, root, 5_000_000)
	writePressure(t, podA, 1_000_000)
	// podB deliberately has no cpu.pressure.
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, MaxSubjects: 8,
		MemTotalBytes: 4 << 30, ProcRoot: procRoot, Now: func() time.Time { return clock }})
	if _, err := c.Collect(); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(10 * time.Second)
	writePressure(t, root, 5_000_000+2_500_000) // stalled 2.5 s of 10 s
	writePressure(t, podA, 1_000_000+500_000)   // stalled 0.5 s of 10 s
	advance(t, podB, 600_000, 0, 0)
	samples, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	got := bySubject(samples)
	for subject, want := range map[string]float64{"": 0.25, "pod:" + testUID: 0.05} {
		s := got[subject][types.CPUPressureRatio]
		if s == nil {
			t.Errorf("subject %q: no cpu_pressure_ratio after two collects", subject)
			continue
		}
		if d := s.Value - want; d > 1e-9 || d < -1e-9 {
			t.Errorf("subject %q: cpu_pressure_ratio %v, want %v", subject, s.Value, want)
		}
		if s.Unit != "fraction" || s.Range == nil || *s.Range != [2]float64{0, 1} {
			t.Errorf("subject %q: unit %q range %v, want fraction on [0,1]: the spec declares cpu_pressure_ratio so",
				subject, s.Unit, s.Range)
		}
	}
	if s := got["pod:11111111-2222-3333-4444-555555555555"][types.CPUPressureRatio]; s != nil {
		t.Errorf("pod without cpu.pressure reported %v; want no sample rather than a zero", s.Value)
	}

	// A counter that went backwards (the cgroup was recreated) yields no sample.
	clock = clock.Add(10 * time.Second)
	writePressure(t, podA, 10)
	samples, err = c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if s := bySubject(samples)["pod:"+testUID][types.CPUPressureRatio]; s != nil {
		t.Errorf("cpu_pressure_ratio %v across a counter reset; want no sample", s.Value)
	}
}

// writePSI writes a cgroup v2 PSI file of the given name with a "some" total.
func writePSI(t *testing.T, dir, file string, someTotalUsec uint64) {
	t.Helper()
	body := fmt.Sprintf("some avg10=0.00 avg60=0.00 avg300=0.00 total=%d\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n", someTotalUsec)
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeProcStat writes /proc/stat's aggregate cpu line with the given steal and a
// total that the eight counted columns add up to.
func writeProcStat(t *testing.T, procRoot string, steal, total uint64) {
	t.Helper()
	// user carries the rest; guest (counted inside user) is set to show it is not
	// added a second time.
	user := total - steal - 400
	body := fmt.Sprintf("cpu  %d 0 100 200 50 25 25 %d 77 0\ncpu0 1 2 3 4 5 6 7 8 9 10\n", user, steal)
	if err := os.WriteFile(filepath.Join(procRoot, "stat"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestIOAndMemoryPressureAndSteal: io.pressure and memory.pressure are read like
// cpu.pressure, for the node and for each subject; steal is the node's share of CPU
// time the hypervisor spent elsewhere, read from /proc/stat, and no subject gets one.
func TestIOAndMemoryPressureAndSteal(t *testing.T) {
	root, podA, _, _, procRoot := fakeTree(t)
	writePSI(t, root, "io.pressure", 1_000_000)
	writePSI(t, root, "memory.pressure", 0)
	writePSI(t, podA, "io.pressure", 0)
	writeProcStat(t, procRoot, 1_000, 100_000)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, MaxSubjects: 8,
		MemTotalBytes: 4 << 30, ProcRoot: procRoot, Now: func() time.Time { return clock }})
	if _, err := c.Collect(); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(10 * time.Second)
	writePSI(t, root, "io.pressure", 1_000_000+4_000_000) // 4 s of 10 s
	writePSI(t, root, "memory.pressure", 1_000_000)       // 1 s of 10 s
	writePSI(t, podA, "io.pressure", 2_000_000)           // 2 s of 10 s
	writeProcStat(t, procRoot, 1_000+150, 100_000+1_000)  // 150 of 1 000 ticks stolen
	samples, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	got := bySubject(samples)
	pod := "pod:" + testUID
	for _, tc := range []struct {
		subject string
		metric  types.MetricType
		want    float64
	}{
		{"", types.IOPressureRatio, 0.4},
		{"", types.MemoryPressureRatio, 0.1},
		{"", types.CPUStealRatio, 0.15},
		{pod, types.IOPressureRatio, 0.2},
	} {
		s := got[tc.subject][tc.metric]
		if s == nil {
			t.Errorf("subject %q: no %s after two collects", tc.subject, tc.metric)
			continue
		}
		if d := s.Value - tc.want; d > 1e-9 || d < -1e-9 {
			t.Errorf("subject %q: %s = %v, want %v", tc.subject, tc.metric, s.Value, tc.want)
		}
		if s.Unit != "fraction" || s.Range == nil || *s.Range != [2]float64{0, 1} {
			t.Errorf("subject %q: %s unit %q range %v, want fraction on [0,1]", tc.subject, tc.metric, s.Unit, s.Range)
		}
	}
	if s := got[pod][types.MemoryPressureRatio]; s != nil {
		t.Errorf("pod without memory.pressure reported %v; want no sample rather than a zero", s.Value)
	}
	if s := got[pod][types.CPUStealRatio]; s != nil {
		t.Errorf("a subject reported cpu_steal_ratio %v; steal is the node's alone", s.Value)
	}
}

// TestPodsAreNamedFromTheKubeletsLogDirectory: a pod's subject is its UID, which is
// stable and unique but says nothing to an operator. The kubelet keeps one directory
// per pod under /var/log/pods named <namespace>_<name>_<uid>, so the collector labels
// each pod with its namespace and name from there; a pod created after the first
// tick is named on its first tick, and one with no directory is left unnamed.
func TestPodsAreNamedFromTheKubeletsLogDirectory(t *testing.T) {
	root, _, _, _, procRoot := fakeTree(t)
	logs := t.TempDir()
	for _, d := range []string{"default_cycler_" + testUID, "junk", "kube-system_coredns-54f859f7dc-4tjdt_99999999-9999-9999-9999-999999999999"} {
		if err := os.Mkdir(filepath.Join(logs, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, MemTotalBytes: 4 << 30, ProcRoot: procRoot, PodLogRoot: logs})
	const uidB = "11111111-2222-3333-4444-555555555555"
	labels := func() map[string]map[string]string {
		samples, err := c.Collect()
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]map[string]string{}
		for s, ms := range bySubject(samples) {
			for _, m := range ms {
				out[s] = m.Labels
			}
		}
		return out
	}
	got := labels()
	if a := got["pod:"+testUID]; a["namespace"] != "default" || a["pod"] != "cycler" {
		t.Errorf("pod A labels %v; want namespace=default pod=cycler", a)
	}
	if b := got["pod:"+uidB]; b["pod"] != "" || b["namespace"] != "" {
		t.Errorf("pod B labels %v; a pod with no log directory has no name", b)
	}
	if err := os.Mkdir(filepath.Join(logs, "web_api-7d4b9_"+uidB), 0o755); err != nil {
		t.Fatal(err)
	}
	if b := labels()["pod:"+uidB]; b["namespace"] != "web" || b["pod"] != "api-7d4b9" {
		t.Errorf("pod B labels %v after its directory appeared; want namespace=web pod=api-7d4b9", b)
	}
}

// writeThrottledUsec appends cpu.stat's cumulative throttled time to the file the
// last writeCgroup or advance left.
func writeThrottledUsec(t *testing.T, dir string, usec uint64) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, "cpu.stat"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "throttled_usec %d\n", usec); err != nil {
		t.Fatal(err)
	}
}

// TestCPUContentionIsStallNotExplainedByOwnThrottling: a cgroup's CPU "some" stall
// counts the time its own quota held it back, so a pod at its limit reports pressure
// with nothing else on the node waiting. cpu_contention_ratio is the stall share left
// after the cgroup's own throttled time, per subject. The node's is not the root's stall,
// which carries every subject's throttling: it is the waiting of a pod owed CPU it does not
// get (owedContention), and podA's 0.01 is under the waiting floor, so the node's is 0.
func TestCPUContentionIsStallNotExplainedByOwnThrottling(t *testing.T) {
	root, podA, podB, _, procRoot := fakeTree(t)
	podBSubject := "pod:11111111-2222-3333-4444-555555555555"
	writePressure(t, root, 0)
	writePressure(t, podA, 0)
	writePressure(t, podB, 0)
	writeThrottledUsec(t, podA, 0)
	writeThrottledUsec(t, podB, 0)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, MaxSubjects: 8,
		MemTotalBytes: 4 << 30, ProcRoot: procRoot, Now: func() time.Time { return clock }})
	if _, err := c.Collect(); err != nil {
		t.Fatal(err)
	}

	clock = clock.Add(10 * time.Second)
	writePressure(t, root, 6_000_000) // the root carries both pods' stall
	// podA sits at its own limit: stalled 3.0 s, of which its quota held it 2.9 s.
	advance(t, podA, 2_000_000, 200, 110)
	writeThrottledUsec(t, podA, 2_900_000)
	writePressure(t, podA, 3_000_000)
	// podB waited 4 s for CPU and was throttled for 1 s of it.
	advance(t, podB, 600_000, 100, 10)
	writeThrottledUsec(t, podB, 1_000_000)
	writePressure(t, podB, 4_000_000)
	samples, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	got := bySubject(samples)
	// podB is BestEffort, so only podA is promised CPU (TestABestEffortPodDoesNotSetNodeContention).
	for subject, want := range map[string]float64{"pod:" + testUID: 0.01, podBSubject: 0.3, "": 0} {
		s := got[subject][types.CPUContentionRatio]
		if s == nil {
			t.Errorf("subject %q: no cpu_contention_ratio after two collects", subject)
			continue
		}
		if d := s.Value - want; d > 1e-9 || d < -1e-9 {
			t.Errorf("subject %q: cpu_contention_ratio %v, want %v", subject, s.Value, want)
		}
		if s.Unit != "fraction" || s.Range == nil || *s.Range != [2]float64{0, 1} {
			t.Errorf("subject %q: unit %q range %v, want fraction on [0,1]", subject, s.Unit, s.Range)
		}
	}

	// Throttled longer than stalled (the two counters are sampled a moment apart):
	// contention is clamped at zero, never negative.
	clock = clock.Add(10 * time.Second)
	advance(t, podA, 3_000_000, 300, 220)
	writeThrottledUsec(t, podA, 2_900_000+3_100_000)
	writePressure(t, podA, 3_000_000+3_000_000)
	advance(t, podB, 700_000, 200, 10)
	writeThrottledUsec(t, podB, 1_000_000)
	writePressure(t, podB, 4_000_000)
	samples, err = c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	got = bySubject(samples)
	if s := got["pod:"+testUID][types.CPUContentionRatio]; s == nil || s.Value != 0 {
		t.Errorf("throttled beyond its stall: cpu_contention_ratio %v, want 0", s)
	}
	if s := got[""][types.CPUContentionRatio]; s == nil || s.Value != 0 {
		t.Errorf("node cpu_contention_ratio %v with no subject contended, want 0", s)
	}

	// A subject without cpu.pressure gives no contention, and a node with no subject
	// reading gives none either, rather than a zero that reads as "nobody waiting".
	clock = clock.Add(10 * time.Second)
	for _, d := range []string{podA, podB} {
		if err := os.Remove(filepath.Join(d, "cpu.pressure")); err != nil {
			t.Fatal(err)
		}
	}
	samples, err = c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	for subject, ms := range bySubject(samples) {
		if s := ms[types.CPUContentionRatio]; s != nil {
			t.Errorf("subject %q: cpu_contention_ratio %v without any cpu.pressure; want no sample", subject, s.Value)
		}
	}
}

// TestABestEffortPodDoesNotSetNodeContention: a BestEffort pod has no CPU request, so on
// a node that is merely busy it waits almost all the time — that is the share it was
// promised, not contention. The node's signal is the worst-off workload that was
// promised something; the BestEffort pod's own reading is still emitted.
func TestABestEffortPodDoesNotSetNodeContention(t *testing.T) {
	root, podA, podB, _, procRoot := fakeTree(t) // podB is under kubepods/besteffort
	podBSubject := "pod:11111111-2222-3333-4444-555555555555"
	for _, d := range []string{root, podA, podB} {
		writePressure(t, d, 0)
	}
	writeThrottledUsec(t, podA, 0)
	writeThrottledUsec(t, podB, 0)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, MaxSubjects: 8,
		MemTotalBytes: 4 << 30, ProcRoot: procRoot, Now: func() time.Time { return clock }})
	if _, err := c.Collect(); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(10 * time.Second)
	advance(t, podA, 2_000_000, 200, 0)
	writeThrottledUsec(t, podA, 0)
	writePressure(t, podA, 1_000_000) // waited 1 s of 10
	advance(t, podB, 600_000, 100, 0)
	writeThrottledUsec(t, podB, 0)
	writePressure(t, podB, 9_500_000) // waited 9.5 s of 10: BestEffort on a busy node
	samples, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	got := bySubject(samples)
	if s := got[""][types.CPUContentionRatio]; s == nil || s.Value != 0.1 {
		t.Fatalf("node cpu_contention_ratio %v; want the burstable pod's 0.1, not the BestEffort pod's 0.95", s)
	}
	if s := got[podBSubject][types.CPUContentionRatio]; s == nil || s.Value != 0.95 {
		t.Fatalf("BestEffort pod's own cpu_contention_ratio %v; want it still reported, 0.95", s)
	}
}

// TestNodeContentionIsAnchoredOnItsWindow: like every other windowed sample, the
// node's cpu_contention_ratio takes its event identity from the start of the window it
// measures, so a reading is identified by the window and not by when it was emitted.
func TestNodeContentionIsAnchoredOnItsWindow(t *testing.T) {
	root, podA, podB, _, procRoot := fakeTree(t)
	for _, d := range []string{root, podA, podB} {
		writePressure(t, d, 0)
	}
	writeThrottledUsec(t, podA, 0)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := start
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, MaxSubjects: 8,
		MemTotalBytes: 4 << 30, ProcRoot: procRoot, Now: func() time.Time { return clock }})
	if _, err := c.Collect(); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(10 * time.Second)
	advance(t, podA, 2_000_000, 200, 0)
	writeThrottledUsec(t, podA, 0)
	writePressure(t, podA, 1_000_000)
	samples, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	s := bySubject(samples)[""][types.CPUContentionRatio]
	if s == nil {
		t.Fatal("no node cpu_contention_ratio")
	}
	want := c.sample(types.CPUContentionRatio, unitNodeShare, s.Value, clock, start, "", nil).EventID
	if s.EventID != want {
		t.Fatalf("event id %s; want the one anchored on the window start %s", s.EventID, want)
	}
}

// TestNodeContentionMovesOnWhenTheRootIsUnreadable: node contention is the worst
// promised subject's, so it does not need the root's cpu.stat; a root that becomes
// unreadable after a good read must not pin every later node sample to the same
// window, which would give them one event id and the map would keep only the first.
func TestNodeContentionMovesOnWhenTheRootIsUnreadable(t *testing.T) {
	root, podA, podB, _, procRoot := fakeTree(t)
	for _, d := range []string{root, podA, podB} {
		writePressure(t, d, 0)
	}
	writeThrottledUsec(t, podA, 0)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, MaxSubjects: 8,
		MemTotalBytes: 4 << 30, ProcRoot: procRoot, Now: func() time.Time { return clock }})
	if _, err := c.Collect(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "cpu.stat")); err != nil {
		t.Fatal(err)
	}
	var ids []string
	stall := uint64(0)
	for i := 0; i < 2; i++ {
		clock = clock.Add(10 * time.Second)
		stall += 1_000_000
		advance(t, podA, 2_000_000, 200, 0)
		writeThrottledUsec(t, podA, 0)
		writePressure(t, podA, stall)
		samples, err := c.Collect()
		if err != nil {
			t.Fatal(err)
		}
		s := bySubject(samples)[""][types.CPUContentionRatio]
		if s == nil {
			t.Fatalf("collect %d: no node cpu_contention_ratio", i)
		}
		ids = append(ids, s.EventID)
	}
	if ids[0] == ids[1] {
		t.Fatalf("both node contention samples have event id %s; want each anchored on its own window", ids[0])
	}
}

// TestAPodsContainersAreItsPartsWhenItHasSeveral: a pod with more than one container is
// broken down into container:<uid>.<name> subjects, each naming its pod as parent, named
// from the kubelet's /var/log/containers links (<pod>_<ns>_<container>-<id>.log). A pod
// with one container is that container, so it gets no part that would only repeat it,
// and a cgroup with no log (the sandbox) is no container anyone wrote.
func TestAPodsContainersAreItsPartsWhenItHasSeveral(t *testing.T) {
	root, podA, _, _, procRoot := fakeTree(t)
	writeCgroup(t, filepath.Join(podA, "cri-containerd-def.scope"), 100_000, 0, 0, "50000000")
	writeCgroup(t, filepath.Join(podA, "cri-containerd-0sandbox.scope"), 1_000, 0, 0, "1000000")
	cl := t.TempDir()
	for _, n := range []string{"cycler_default_db-abc.log", "cycler_default_exporter-def.log"} {
		if err := os.Symlink("/dev/null", filepath.Join(cl, n)); err != nil {
			t.Fatal(err)
		}
	}
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, MaxSubjects: 16, MemTotalBytes: 4 << 30,
		ProcRoot: procRoot, ContainerLogRoot: cl})
	samples, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	by := bySubject(samples)
	for name, mem := range map[string]float64{"db": 200000000, "exporter": 50000000} {
		s := by["container:"+testUID+"."+name][types.MemoryUtilization]
		if s == nil {
			t.Fatalf("no container:%s.%s; subjects %v", testUID, name, keys(by))
		}
		if s.Labels["parent"] != "pod:"+testUID || s.Labels["container"] != name || s.Labels["kind"] != "container" {
			t.Errorf("%s labels %v; want its pod as parent and its name", name, s.Labels)
		}
		if want := mem / float64(4<<30); s.Value < want*0.99 || s.Value > want*1.01 {
			t.Errorf("%s memory %v; want %v of the node", name, s.Value, want)
		}
	}
	for s := range by {
		if strings.Contains(s, "sandbox") || strings.HasPrefix(s, "container:11111111") {
			t.Errorf("subject %s; want no part for the sandbox or for a single-container pod", s)
		}
	}
}

// TestAUnitDoesNotSetNodeContention: node contention is relief's trigger, and relief acts
// on the runtime's workloads. A unit's waiting is not theirs to relieve: the node's own
// decision model, alone on CPUs no pod can use, waited 0.6 of the time on its own runner
// threads, which held node contention above relief's exit level whatever was capped and
// made every verification read as no effect. The unit's own reading is still emitted.
func TestAUnitDoesNotSetNodeContention(t *testing.T) {
	root, podA, _, unit, procRoot := fakeTree(t)
	for _, d := range []string{root, podA, unit} {
		writePressure(t, d, 0)
	}
	writeThrottledUsec(t, podA, 0)
	writeThrottledUsec(t, unit, 0)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, MaxSubjects: 8, UnitGlobs: []string{"*"},
		MemTotalBytes: 4 << 30, ProcRoot: procRoot, Now: func() time.Time { return clock }})
	if _, err := c.Collect(); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(10 * time.Second)
	advance(t, podA, 2_000_000, 200, 0)
	writeThrottledUsec(t, podA, 0)
	writePressure(t, podA, 1_000_000) // 0.1
	advance(t, unit, 3_000_000, 0, 0)
	writeThrottledUsec(t, unit, 0)
	writePressure(t, unit, 9_000_000) // 0.9, on its own threads
	samples, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	got := bySubject(samples)
	if s := got[""][types.CPUContentionRatio]; s == nil || s.Value != 0.1 {
		t.Fatalf("node cpu_contention_ratio %v; want the pod's 0.1, not the unit's 0.9", s)
	}
	if s := got["unit:k0sworker.service"][types.CPUContentionRatio]; s == nil || s.Value != 0.9 {
		t.Fatalf("unit's own cpu_contention_ratio %v; want it still reported, 0.9", s)
	}
}

// TestSharesAreOfTheMachinesCPUsNotTheAgents: every CPU fraction is a share of the node,
// so it divides by the machine's CPUs. runtime.NumCPU is the CPUs the agent's own process
// may run on: pinned to six of twelve, the agent reported its decision model at 0.998 of
// the node and the node at its ceiling (case-study run 2026-10-20i). /proc/stat lists one
// cpuN line per CPU the machine has.
func TestSharesAreOfTheMachinesCPUsNotTheAgents(t *testing.T) {
	root, _, _, _, procRoot := fakeTree(t)
	stat := "cpu  100 0 100 1000 0 0 0 0 0 0\n"
	for i := 0; i < 12; i++ {
		stat += "cpu" + strconv.Itoa(i) + " 10 0 10 100 0 0 0 0 0 0\n"
	}
	stat += "intr 0\nctxt 0\n"
	if err := os.WriteFile(filepath.Join(procRoot, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, MaxSubjects: 8, MemTotalBytes: 4 << 30, ProcRoot: procRoot})
	if c.numCPU != 12 {
		t.Fatalf("numCPU %v; want the machine's 12 from /proc/stat", c.numCPU)
	}
}

func writeSelfCgroup(t *testing.T, procRoot, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(procRoot, "self"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procRoot, "self", "cgroup"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func collectTwice(t *testing.T, c *CgroupCollector, bump func()) []*types.MetricSample {
	t.Helper()
	if _, err := c.Collect(); err != nil {
		t.Fatal(err)
	}
	bump()
	time.Sleep(20 * time.Millisecond)
	s, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSelfIsASubjectWithTheSelfLabel(t *testing.T) {
	root, _, _, _, procRoot := fakeTree(t)
	self := filepath.Join(root, "system.slice", "di-agent.service")
	writeCgroup(t, self, 1_000_000, 0, 0, "50000000")
	writeSelfCgroup(t, procRoot, "0::/system.slice/di-agent.service\n")
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, MaxSubjects: 256, MemTotalBytes: 4 << 30, ProcRoot: procRoot, UnitGlobs: []string{"di-agent.service"}})
	c.numCPU = 4
	got := bySubject(collectTwice(t, c, func() { advance(t, self, 1_040_000, 0, 0) }))
	s := got["unit:di-agent"]
	if s[types.CPUUtilization] == nil || s[types.MemoryUtilization] == nil {
		t.Fatalf("unit:di-agent lacks cpu/memory: %v", s)
	}
	l := s[types.CPUUtilization].Labels
	if l["self"] != "true" || l["unit"] != "di-agent.service" || l["kind"] != "unit" {
		t.Errorf("labels = %v", l)
	}
	if got["unit:di-agent.service"] != nil {
		t.Error("the self directory must not also be unit:di-agent.service")
	}
	if got[""][types.CPUUtilization] == nil {
		t.Error("node samples missing")
	}
}

func TestSelfAsAPodKeepsThePodAndSkipsItsOwnContainerPart(t *testing.T) {
	root, podA, _, _, procRoot := fakeTree(t)
	rel, _ := filepath.Rel(root, filepath.Join(podA, "cri-containerd-abc.scope"))
	writeSelfCgroup(t, procRoot, "0::/"+filepath.ToSlash(rel)+"\n")
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, MaxSubjects: 256, MemTotalBytes: 4 << 30, ProcRoot: procRoot})
	c.numCPU = 4
	got := bySubject(collectTwice(t, c, func() { advance(t, podA, 1_040_000, 110, 15) }))
	if got["pod:"+testUID] == nil || got["unit:di-agent"] == nil {
		t.Fatalf("want pod and unit:di-agent, got %v", got)
	}
	for sub := range got {
		if strings.HasPrefix(sub, "container:") {
			t.Errorf("self container part must not be emitted: %s", sub)
		}
	}
}

func TestSelfCgroupPicksTheUnifiedLineOnAHybridHierarchy(t *testing.T) {
	root, _, _, _, procRoot := fakeTree(t)
	self := filepath.Join(root, "system.slice", "di-agent.service")
	writeCgroup(t, self, 1_000_000, 0, 0, "50000000")
	writeSelfCgroup(t, procRoot, "12:cpu,cpuacct:/user.slice\n0::/system.slice/di-agent.service\n")
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, MemTotalBytes: 4 << 30, ProcRoot: procRoot})
	if bySubject(collectTwice(t, c, func() {}))["unit:di-agent"] == nil {
		t.Error("unit:di-agent absent on a hybrid file")
	}

	root2, _, _, _, procRoot2 := fakeTree(t)
	writeSelfCgroup(t, procRoot2, "12:cpu,cpuacct:/user.slice\n")
	opts, lines := captureLogs(CgroupOptions{Subjects: true, MemTotalBytes: 4 << 30, ProcRoot: procRoot2})
	c2 := NewCgroupCollectorWithOptions("n1", root2, opts)
	if bySubject(collectTwice(t, c2, func() {}))["unit:di-agent"] != nil {
		t.Error("unit:di-agent present without a 0:: line")
	}
	n := 0
	for _, l := range *lines {
		if strings.Contains(l, "self/cgroup") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("want one self/cgroup log line, got %d: %v", n, *lines)
	}
}

func TestSelfPathMissingUnderRootIsLoggedOnceAndSkipped(t *testing.T) {
	root, _, _, _, procRoot := fakeTree(t)
	writeSelfCgroup(t, procRoot, "0::/system.slice/nothere.service\n")
	opts, lines := captureLogs(CgroupOptions{Subjects: true, MemTotalBytes: 4 << 30, ProcRoot: procRoot})
	c := NewCgroupCollectorWithOptions("n1", root, opts)
	var last []*types.MetricSample
	for i := 0; i < 3; i++ {
		last, _ = c.Collect()
	}
	if bySubject(last)["unit:di-agent"] != nil {
		t.Error("missing self dir must not be collected")
	}
	if len(*lines) != 1 {
		t.Errorf("want one log line, got %v", *lines)
	}
}

func TestNoSelfCgroupFileIsSilent(t *testing.T) {
	root, _, _, _, procRoot := fakeTree(t)
	opts, lines := captureLogs(CgroupOptions{Subjects: true, MemTotalBytes: 4 << 30, ProcRoot: procRoot})
	c := NewCgroupCollectorWithOptions("n1", root, opts)
	got := bySubject(collectTwice(t, c, func() {}))
	if got["unit:di-agent"] != nil || len(*lines) != 0 {
		t.Errorf("want silent absence, got %v lines=%v", got["unit:di-agent"], *lines)
	}
}
