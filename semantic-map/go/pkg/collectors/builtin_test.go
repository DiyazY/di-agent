package collectors

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/internal/minimal"
	"github.com/DiyazY/di-agent/pkg/spec"
	"github.com/DiyazY/di-agent/pkg/types"
	"go.yaml.in/yaml/v3"
)

func node(t *testing.T, src string) yaml.Node {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(src), &n); err != nil {
		t.Fatal(err)
	}
	if n.Kind == yaml.DocumentNode {
		return *n.Content[0]
	}
	return n
}

func TestBuiltinConfigStrict(t *testing.T) {
	_, err := Build(Env{NodeID: "n"}, []spec.CollectorEntry{{Name: "cg", Type: "cgroup", Config: node(t, "roots: /x")}})
	if err == nil || !strings.Contains(err.Error(), "roots") || !strings.Contains(err.Error(), "sources.collectors[0] cg") {
		t.Fatalf("err = %v", err)
	}
}

func TestBuiltinCgroupDefaults(t *testing.T) {
	root := newFakeCgroupRoot(t)
	s, err := Build(Env{NodeID: "n", Interval: time.Second}, []spec.CollectorEntry{
		{Name: "cg", Type: "cgroup", Config: node(t, "root: "+root)},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.members[0].m.C.Collect() //nolint:errcheck // the first call only stores the baseline
	time.Sleep(2 * time.Millisecond)
	got, err := s.members[0].m.C.Collect()
	if err != nil || len(got) == 0 {
		t.Fatalf("Collect = %d samples, %v", len(got), err)
	}
}

func TestBuiltinCgroupBadUnits(t *testing.T) {
	_, err := Build(Env{NodeID: "n"}, []spec.CollectorEntry{{Name: "cg", Type: "cgroup", Config: node(t, `units: ["["]`)}})
	if err == nil || !strings.Contains(err.Error(), "sources.collectors[0] cg") {
		t.Fatalf("err = %v", err)
	}
}

func TestBuiltinNetdataRequiresURL(t *testing.T) {
	_, err := Build(Env{NodeID: "n"}, []spec.CollectorEntry{{Name: "nd", Type: "netdata"}})
	if err == nil || !strings.Contains(err.Error(), "url") {
		t.Fatalf("err = %v", err)
	}
	if _, err := Build(Env{NodeID: "n"}, []spec.CollectorEntry{{Name: "nd", Type: "netdata", Config: node(t, "url: http://localhost:19999")}}); err != nil {
		t.Fatalf("with url: %v", err)
	}
}

func TestBuiltinScriptIntervalMismatchRefused(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	sc := `{"name":"t","seed":1,"tick_seconds":10,"duration_seconds":600,"noise":0.01,
"node":{"node_cpu":{"coupling":"sum","base":0.1,"of":"cpu_utilization"}},
"subjects":[{"id":"pod:a","arrive":0,"properties":{"cpu_utilization":{"pattern":"sine","min":0.1,"max":0.6,"period":120}}}],
"expect":{"admitted_within_ticks":1}}`
	if err := os.WriteFile(p, []byte(sc), 0o644); err != nil {
		t.Fatal(err)
	}
	entry := spec.CollectorEntry{Name: "sc", Type: "script", Config: node(t, "path: "+p)}
	_, err := Build(Env{NodeID: "n", Interval: 5 * time.Second}, []spec.CollectorEntry{entry})
	if err == nil || !strings.Contains(err.Error(), "sources.interval") {
		t.Fatalf("err = %v", err)
	}
	if _, err := Build(Env{NodeID: "n", Interval: 10 * time.Second}, []spec.CollectorEntry{entry}); err != nil {
		t.Fatalf("matching interval: %v", err)
	}
}

// TestCgroupRoot pins where the agent's groups read cgroups from: the root of the
// first cgroup entry, its default when the entry sets none, and the default when
// no entry is a cgroup one.
func TestCgroupRoot(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []spec.CollectorEntry
		want    string
	}{
		{"none", nil, "/sys/fs/cgroup"},
		{"no cgroup entry", []spec.CollectorEntry{{Name: "nd", Type: "netdata", Config: node(t, "url: http://x")}}, "/sys/fs/cgroup"},
		{"default root", []spec.CollectorEntry{{Name: "cg", Type: "cgroup"}}, "/sys/fs/cgroup"},
		{"first cgroup wins", []spec.CollectorEntry{
			{Name: "nd", Type: "netdata", Config: node(t, "url: http://x")},
			{Name: "cg", Type: "cgroup", Config: node(t, "root: /host/cg")},
			{Name: "cg2", Type: "cgroup", Config: node(t, "root: /other")},
		}, "/host/cg"},
	} {
		if got := CgroupRoot(tc.entries); got != tc.want {
			t.Errorf("%s: CgroupRoot = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestBuiltinCgroupWiresSubjectOptions checks that the entry's subject options reach
// the collector rather than being accepted and dropped: a synthetic pod cgroup under
// root must show up as a pod:<uid> subject. Moved here from pkg/profiles, which no
// longer builds collectors.
func TestBuiltinCgroupWiresSubjectOptions(t *testing.T) {
	root := t.TempDir()
	pod := filepath.Join(root, "kubepods.slice", "kubepods-pod8f3c1234_aaaa_bbbb_cccc_1234567890ab.slice")
	for _, dir := range []string{root, pod} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		// The subject is proven through its CPU sample, which needs two Collect()
		// calls and no node memory capacity.
		for name, content := range map[string]string{"cpu.stat": "usage_usec 1\nnr_periods 0\nnr_throttled 0\n", "memory.current": "1024\n", "memory.max": "1073741824\n"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	s, err := Build(Env{NodeID: "n1"}, []spec.CollectorEntry{
		{Name: "cg", Type: "cgroup", Config: node(t, "{root: "+root+", subjects: true, max_subjects: 4}")},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := s.members[0].m.C
	if _, err := c.Collect(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(pod, "cpu.stat"), []byte("usage_usec 5001\nnr_periods 0\nnr_throttled 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	samples, _ := c.Collect()
	var scoped bool
	for _, smp := range samples {
		scoped = scoped || smp.Subject == "pod:8f3c1234-aaaa-bbbb-cccc-1234567890ab"
	}
	if !scoped {
		t.Error("the subject options did not reach the cgroup collector")
	}
}

// TestBuiltinScriptMissingRefused: a bad script path must refuse the daemon, not hand
// back one with no collector, which reports "collection loop disabled" — the message
// for a deliberate choice — and serves an empty map indefinitely.
func TestBuiltinScriptMissingRefused(t *testing.T) {
	_, err := Build(Env{NodeID: "n", Interval: 10 * time.Second}, []spec.CollectorEntry{
		{Name: "sc", Type: "script", Config: node(t, "path: "+filepath.Join(t.TempDir(), "missing.json"))},
	})
	if err == nil {
		t.Fatal("a missing script path was accepted")
	}
}

// TestBuiltinCgroupCountsAgainstEnvRequests: the cgroup collector built from an entry
// reads the pods' memory requests from Env.Requests. A pod the stored requests do not
// name is not counted in memory_contention_ratio, however much it stalls.
func TestBuiltinCgroupCountsAgainstEnvRequests(t *testing.T) {
	root := newFakeCgroupRoot(t)
	pod := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice", "kubepods-burstable-pod8f3c1234_aaaa_bbbb_cccc_1234567890ab.slice")
	if err := os.MkdirAll(pod, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(pod, "cpu.stat"), "usage_usec 500000\nnr_periods 10\nnr_throttled 1\n")
	mustWrite(t, filepath.Join(pod, "memory.current"), "268435456\n")
	psi := func(total int) {
		mustWrite(t, filepath.Join(pod, "memory.pressure"), "some avg10=0 avg60=0 avg300=0 total="+strconv.Itoa(total)+"\n")
	}
	for _, tc := range []struct {
		name   string
		stored bool
		want   float64
	}{{"no requests stored", false, 1}, {"pod has no request", true, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			reqs := &minimal.Requests{}
			if tc.stored {
				reqs.Store(map[string]float64{})
			}
			s, err := Build(Env{NodeID: "n", Interval: time.Second, Requests: reqs}, []spec.CollectorEntry{
				{Name: "cg", Type: "cgroup", Config: node(t, "root: "+root)},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			psi(0)
			s.members[0].m.C.Collect() //nolint:errcheck // the first call only stores the baseline
			time.Sleep(2 * time.Millisecond)
			psi(1_000_000_000) // far longer than the window: the whole window stalled
			got, err := s.members[0].m.C.Collect()
			if err != nil {
				t.Fatal(err)
			}
			for _, smp := range got {
				if smp.MetricType == types.MemoryContentionRatio && smp.Subject == "" {
					if smp.Value != tc.want {
						t.Fatalf("memory_contention_ratio = %v, want %v", smp.Value, tc.want)
					}
					return
				}
			}
			t.Fatal("no node memory_contention_ratio")
		})
	}
}
