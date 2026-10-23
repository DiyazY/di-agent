package minimal

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/types"
)

// TestMemoryContentionExcludes: a pod sets the node's memory contention only when it is
// not limiting itself and, with requests known, is within its request. Each excluded pod
// waits far more than the one counted, so letting any of them through shows.
func TestMemoryContentionExcludes(t *testing.T) {
	within := podMem{uid: "ok", current: 100, psi: 0.2}
	req := map[string]float64{"ok": 200, "limited": 200, "over": 50}
	for _, tc := range []struct {
		name string
		pod  podMem
	}{
		{"selfLimited", podMem{uid: "limited", current: 100, psi: 0.9, selfLimited: true}},
		{"over its request", podMem{uid: "over", current: 100, psi: 0.9}},
		{"no request while requests are known", podMem{uid: "unknown", current: 100, psi: 0.9}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := memoryContention([]podMem{within, tc.pod}, req, true); got != 0.2 {
				t.Fatalf("memoryContention = %v; want the counted pod's 0.2, the excluded pod's 0.9 left out", got)
			}
		})
	}
	t.Run("within its request is counted", func(t *testing.T) {
		at := podMem{uid: "ok", current: 200, psi: 0.4} // current == request is within
		if got := memoryContention([]podMem{at}, req, true); got != 0.4 {
			t.Fatalf("memoryContention = %v; want 0.4", got)
		}
	})
}

// TestMemoryContentionNoProvider: without requests the request condition is dropped, so a
// pod with none, or using more than one would say, still counts; self-limiting still
// excludes.
func TestMemoryContentionNoProvider(t *testing.T) {
	pods := []podMem{
		{uid: "a", current: 1 << 40, psi: 0.7},
		{uid: "b", current: 10, psi: 0.9, selfLimited: true},
	}
	if got := memoryContention(pods, nil, false); got != 0.7 {
		t.Fatalf("memoryContention = %v; want 0.7", got)
	}
}

func TestMemoryContentionNoneIsZero(t *testing.T) {
	if got := memoryContention(nil, nil, false); got != 0 {
		t.Fatalf("no pods: %v, want 0", got)
	}
	if got := memoryContention([]podMem{{uid: "a", psi: 0.5, selfLimited: true}}, nil, false); got != 0 {
		t.Fatalf("no counted pod: %v, want 0", got)
	}
}

func TestRequestsLoadBeforeStoreAndCopies(t *testing.T) {
	var r Requests
	if m, ok := r.Load(); ok || m != nil {
		t.Fatalf("Load before Store = %v, %v; want nil, false", m, ok)
	}
	in := map[string]float64{"a": 1}
	r.Store(in)
	in["a"] = 2
	in["b"] = 3
	m, ok := r.Load()
	if !ok || len(m) != 1 || m["a"] != 1 {
		t.Fatalf("Load = %v, %v; want the map as stored, untouched by later writes to the argument", m, ok)
	}
	r.Store(nil)
	if m, ok := r.Load(); !ok || len(m) != 0 {
		t.Fatalf("Load after Store(nil) = %v, %v; want an empty map, true", m, ok)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) { defer wg.Done(); r.Store(map[string]float64{"x": float64(i)}) }(i)
		go func() { defer wg.Done(); r.Load() }()
	}
	wg.Wait()
}

func writeMemEvents(t *testing.T, dir string, high, max, oomKill uint64) {
	t.Helper()
	body := fmt.Sprintf("low 0\nhigh %d\nmax %d\noom 0\noom_kill %d\noom_group_kill 0\n", high, max, oomKill)
	if err := os.WriteFile(filepath.Join(dir, "memory.events"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCollectorEmitsMemoryEvents: memory.events' high, max and oom_kill become counts per
// window for each subject from its second collect; the node root has no memory.events
// and gets none; a counter that went backwards (the cgroup was recreated) gives no
// readings that window and the next window counts from the new values.
func TestCollectorEmitsMemoryEvents(t *testing.T) {
	root, podA, _, unit, procRoot := fakeTree(t)
	writeMemEvents(t, podA, 5, 1, 0)
	writeMemEvents(t, unit, 0, 0, 0)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, MaxSubjects: 8,
		UnitGlobs: []string{"k0s*"}, MemTotalBytes: 4 << 30, ProcRoot: procRoot, Now: func() time.Time { return clock }})
	events := []types.MetricType{types.MemoryEventsHigh, types.MemoryEventsMax, types.MemoryOOMKills}
	collect := func() map[string]map[types.MetricType]*types.MetricSample {
		t.Helper()
		samples, err := c.Collect()
		if err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(10 * time.Second)
		return bySubject(samples)
	}
	pod, unitSub := "pod:"+testUID, "unit:k0sworker.service"
	got := collect()
	for _, mt := range events {
		if s := got[pod][mt]; s != nil {
			t.Fatalf("first collect emitted %s = %v; a count per window needs a window", mt, s.Value)
		}
	}
	writeMemEvents(t, podA, 8, 1, 2)
	writeMemEvents(t, unit, 0, 4, 0)
	got = collect()
	for _, tc := range []struct {
		subject string
		mt      types.MetricType
		want    float64
	}{
		{pod, types.MemoryEventsHigh, 3}, {pod, types.MemoryEventsMax, 0}, {pod, types.MemoryOOMKills, 2},
		{unitSub, types.MemoryEventsMax, 4},
	} {
		s := got[tc.subject][tc.mt]
		if s == nil {
			t.Errorf("%s: no %s on the second collect", tc.subject, tc.mt)
			continue
		}
		if s.Value != tc.want {
			t.Errorf("%s: %s = %v, want %v", tc.subject, tc.mt, s.Value, tc.want)
		}
		if s.Unit != "count" || s.Range == nil || *s.Range != [2]float64{0, 1e9} {
			t.Errorf("%s: %s unit %q range %v, want count on [0,1e9]", tc.subject, tc.mt, s.Unit, s.Range)
		}
	}
	for _, mt := range events {
		if s := got[""][mt]; s != nil {
			t.Errorf("node emitted %s = %v; the v2 root has no memory.events", mt, s.Value)
		}
	}
	writeMemEvents(t, podA, 0, 0, 0) // recreated
	got = collect()
	for _, mt := range events {
		if s := got[pod][mt]; s != nil {
			t.Errorf("after a counter reset %s = %v; want no reading that window", mt, s.Value)
		}
	}
	writeMemEvents(t, podA, 1, 0, 0)
	got = collect()
	if s := got[pod][types.MemoryEventsHigh]; s == nil || s.Value != 1 {
		t.Fatalf("window after the reset: memory_events_high %v; want 1, counted from the new values", s)
	}
}

// TestCollectorEmitsMemoryContention: the node's memory contention is the worst memory
// stall of a promised pod within its request and not limiting itself. A BestEffort pod
// never sets it; before the requests are first stored the request condition is dropped.
func TestCollectorEmitsMemoryContention(t *testing.T) {
	root, podA, podB, _, procRoot := fakeTree(t) // podA burstable 256 MiB, podB besteffort
	const uidC = "33333333-4444-5555-6666-777777777777"
	podC := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice", "kubepods-burstable-pod33333333_4444_5555_6666_777777777777.slice")
	writeCgroup(t, podC, 1_000_000, 0, 0, "104857600") // 100 MiB
	writePSI(t, root, "memory.pressure", 0)
	for _, d := range []string{podA, podB, podC} {
		writePSI(t, d, "memory.pressure", 0)
		writeMemEvents(t, d, 0, 0, 0)
	}
	reqs := &Requests{}
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, MaxSubjects: 8,
		MemTotalBytes: 4 << 30, ProcRoot: procRoot, Requests: reqs, Now: func() time.Time { return clock }})
	var stall = map[string]uint64{root: 0, podA: 0, podB: 0, podC: 0}
	collect := func(psi map[string]uint64) *types.MetricSample {
		t.Helper()
		for d, add := range psi {
			stall[d] += add
			writePSI(t, d, "memory.pressure", stall[d])
		}
		samples, err := c.Collect()
		if err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(10 * time.Second)
		return bySubject(samples)[""][types.MemoryContentionRatio]
	}
	if s := collect(nil); s != nil {
		t.Fatalf("first collect emitted memory_contention_ratio %v; there is no window yet", s.Value)
	}
	window := map[string]uint64{root: 9_000_000, podA: 3_000_000, podB: 9_000_000, podC: 5_000_000} // 0.3, 0.9, 0.5 of 10 s
	if s := collect(window); s == nil || s.Value != 0.5 {
		t.Fatalf("before requests are stored: %v; want 0.5, the worst promised pod with the request condition dropped", s)
	}
	reqs.Store(map[string]float64{testUID: 512 << 20, uidC: 50 << 20})
	s := collect(window)
	if s == nil || s.Value != 0.3 {
		t.Fatalf("with requests: %v; want 0.3 — podC is over its request, podB BestEffort", s)
	}
	if s.Unit != "fraction" || s.Range == nil || *s.Range != [2]float64{0, 1} {
		t.Fatalf("memory_contention_ratio unit %q range %v; want fraction on [0,1]", s.Unit, s.Range)
	}
	writeMemEvents(t, podA, 1, 0, 0) // podA hits its memory.high: limiting itself
	if s := collect(window); s == nil || s.Value != 0 {
		t.Fatalf("with podA self-limited: %v; want 0, no counted pod", s)
	}
}

// TestMemoryContentionIsZeroWithoutAPromisedPod: with the node's memory stall read but no
// pod promised memory (only BestEffort, or a promised pod first seen this window), the
// node reports a fresh 0 — relief releases only on fresh readings, so silence here would
// hold a step it took for ever.
func TestMemoryContentionIsZeroWithoutAPromisedPod(t *testing.T) {
	root := t.TempDir()
	procRoot := writeFakeProc(t)
	writeRootCgroup(t, root, 10_000_000, 0, 0)
	writePSI(t, root, "memory.pressure", 0)
	podB := filepath.Join(root, "kubepods", "besteffort", "pod11111111-2222-3333-4444-555555555555")
	writeCgroup(t, podB, 500_000, 0, 0, "134217728")
	writePSI(t, podB, "memory.pressure", 0)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, MaxSubjects: 8,
		MemTotalBytes: 4 << 30, ProcRoot: procRoot, Now: func() time.Time { return clock }})
	if _, err := c.Collect(); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(10 * time.Second)
	writePSI(t, root, "memory.pressure", 5_000_000)
	writePSI(t, podB, "memory.pressure", 9_000_000)
	// A promised pod new this window has no stall reading yet and is not counted either.
	podA := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice", "kubepods-burstable-pod8f3c1234_aaaa_bbbb_cccc_1234567890ab.slice")
	writeCgroup(t, podA, 1_000_000, 0, 0, "268435456")
	writePSI(t, podA, "memory.pressure", 9_000_000)
	samples, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	s := bySubject(samples)[""][types.MemoryContentionRatio]
	if s == nil || s.Value != 0 {
		t.Fatalf("memory_contention_ratio %v; want a fresh 0 when no promised pod has a reading", s)
	}
}

// TestMemoryContentionAbsentWithoutNodePSI: a node whose root has no memory.pressure
// reports no memory contention at all, even with pods that stall: no PSI is not calm.
func TestMemoryContentionAbsentWithoutNodePSI(t *testing.T) {
	root, podA, _, _, procRoot := fakeTree(t) // the root has no memory.pressure
	writePSI(t, podA, "memory.pressure", 0)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, MaxSubjects: 8,
		MemTotalBytes: 4 << 30, ProcRoot: procRoot, Now: func() time.Time { return clock }})
	if _, err := c.Collect(); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(10 * time.Second)
	writePSI(t, podA, "memory.pressure", 3_000_000)
	samples, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if s := bySubject(samples)[""][types.MemoryContentionRatio]; s != nil {
		t.Fatalf("memory_contention_ratio %v without node memory PSI; want none", s.Value)
	}
}
