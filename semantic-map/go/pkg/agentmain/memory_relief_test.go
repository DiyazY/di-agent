package agentmain

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/DiyazY/di-agent/internal/minimal"
	"github.com/DiyazY/di-agent/pkg/actuators"
	"github.com/DiyazY/di-agent/pkg/policy"
	"github.com/DiyazY/di-agent/pkg/runtime"
	"github.com/DiyazY/di-agent/pkg/runtime/kubernetes"
	"github.com/DiyazY/di-agent/pkg/spec"
	"github.com/DiyazY/di-agent/pkg/statemap"
)

// TestBothLeversBuiltByDefault: relief enabled with the default levers runs one
// loop for CPU and one for memory, each with its default thresholds.
func TestBothLeversBuiltByDefault(t *testing.T) {
	s := spec.Default()
	s.Node.ID = "n1"
	s.Loops.Relief.Enabled = true
	rt, err := runtime.Build("dry-run", runtime.Env{NodeID: "n1", Spec: s.Runtime})
	if err != nil {
		t.Fatal(err)
	}
	acting, _, err := buildActing(s, rt, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	pol, err := policy.New(s.Policy)
	if err != nil {
		t.Fatal(err)
	}
	loops, err := buildRelief(s, rt, acting, pol, statemap.New(statemap.Config{Owner: "n1"}, nil), false, nil, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	want := []spec.Lever{{
		Action: "cpu.limit", Enter: 0.25, Exit: 0.10, EnterFor: time.Minute, ExitFor: 3 * time.Minute,
		Step: 0.25, MinEffect: 0.03, VictimShare: 0.05, Settle: 2 * time.Minute, Expiry: time.Hour,
	}, {
		Action: "memory.high", Enter: 0.08, Exit: 0.03, EnterFor: time.Minute, ExitFor: 3 * time.Minute,
		Step: 0.25, MinEffect: 0.01, VictimShare: 0.05, Settle: 2 * time.Minute, Expiry: time.Hour,
	}}
	if len(loops) != len(want) {
		t.Fatalf("%d loops; want %d", len(loops), len(want))
	}
	for i, l := range loops {
		if got := l.Lever(); !reflect.DeepEqual(got, want[i]) || l.Status().Action != want[i].Action {
			t.Errorf("loop %d: lever %+v, action %s; want %+v", i, got, l.Status().Action, want[i])
		}
	}
}

// TestReliefRefusesUnwritableLedger: on a runtime that changes the system, a
// ledger the agent cannot write refuses start, naming the file.
func TestReliefRefusesUnwritableLedger(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes a read-only directory")
	}
	dry, err := runtime.Build("dry-run", runtime.Env{NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	s := spec.Default()
	s.Node.ID, s.Runtime.Kind = "n1", "kubernetes"
	s.Loops.Relief.Enabled = true
	s.Actuation.Ledger = filepath.Join(dir, "actions.json")
	if _, _, err := buildActing(s, named{dry}, t.Logf); err == nil || !strings.Contains(err.Error(), s.Actuation.Ledger) {
		t.Fatalf("err %v; want a refusal naming %s", err, s.Actuation.Ledger)
	}
}

// TestActuatorsOnlyWithRelief: with relief off no actuator is built, so one that
// could not be (no cgroup v2 here) does not stop an agent that changes nothing.
// With relief on it refuses start, naming the lever.
func TestActuatorsOnlyWithRelief(t *testing.T) {
	dry, err := runtime.Build("dry-run", runtime.Env{NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	root, ledger := t.TempDir(), filepath.Join(t.TempDir(), "actions.json")
	s, err := spec.Parse([]byte("runtime: {kind: kubernetes}\n" +
		"sources: {collectors: [{name: cgroup, type: cgroup, config: {root: " + root + "}}]}\n" +
		"actuation: {ledger: " + ledger + "}\n"))
	if err != nil {
		t.Fatal(err)
	}
	s.Node.ID = "n1"
	acting, _, err := buildActing(s, named{dry}, t.Logf)
	if err != nil {
		t.Fatalf("relief off: %v", err)
	}
	if d := acting.Descriptors(); len(d) != 0 {
		t.Errorf("relief off built %v; want no actuator", d)
	}
	s.Loops.Relief.Enabled = true
	_, _, err = buildActing(s, named{dry}, t.Logf)
	if err == nil || !strings.Contains(err.Error(), "loops.relief.levers[1] memory.high") || !strings.Contains(err.Error(), "cgroup v2") {
		t.Fatalf("relief on: err %v; want memory.high's lever named, needing cgroup v2", err)
	}
}

// TestAPodTheAgentCappedIsNotTheKubelets: memory.high is built with the ledger's
// entries, so a pod the agent itself capped before a restart is not taken for
// the kubelet managing memory.high.
func TestAPodTheAgentCappedIsNotTheKubelets(t *testing.T) {
	dry, err := runtime.Build("dry-run", runtime.Env{NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	const uid = "1234abcd-0000-1111-2222-333344445555"
	pod := filepath.Join(root, "kubepods", "burstable", "pod"+uid)
	if err := os.MkdirAll(pod, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{
		filepath.Join(root, "cgroup.controllers"): "cpu memory\n",
		filepath.Join(pod, "memory.high"):         "500000000\n",
	} {
		if err := os.WriteFile(name, []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ledger := filepath.Join(t.TempDir(), "actions.json")
	s, err := spec.Parse([]byte("runtime: {kind: kubernetes}\nloops: {relief: {enabled: true}}\n" +
		"sources: {collectors: [{name: cgroup, type: cgroup, config: {root: " + root + "}}]}\n" +
		"actuation: {ledger: " + ledger + "}\n"))
	if err != nil {
		t.Fatal(err)
	}
	s.Node.ID = "n1"
	if _, _, err := buildActing(s, named{dry}, t.Logf); err == nil || !strings.Contains(err.Error(), "MemoryQoS") {
		t.Fatalf("err %v; want a memory.high nobody recorded taken for the kubelet's", err)
	}
	l, err := actuators.OpenLedger(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Put(actuators.Entry{Action: "memory.high", Subject: "pod:" + uid, Applied: 500000000, NodeID: "n1"}); err != nil {
		t.Fatal(err)
	}
	acting, _, err := buildActing(s, named{dry}, t.Logf)
	if err != nil {
		t.Fatalf("err %v; want the agent's own cap recognised from the ledger", err)
	}
	if _, ok := acting.Actuator("memory.high"); !ok {
		t.Error("no memory.high actuator built")
	}
}

// listing is a runtime whose listing is set by the test.
type listing struct {
	ws  []runtime.Workload
	err error
}

func (listing) Name() string { return "test" }
func (l *listing) Workloads(context.Context) ([]runtime.Workload, error) {
	return l.ws, l.err
}

// TestRequestsRefreshedFromRuntime: the collector's memory requests are the
// runtime's pods' requests by UID; a pod with none, and a workload that is not a
// pod, are not in it, and a failed listing keeps the last one, said once.
func TestRequestsRefreshedFromRuntime(t *testing.T) {
	rt, err := runtime.Build("dry-run", runtime.Env{NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	dr := rt.(*runtime.DryRun)
	dr.Put(runtime.Workload{Subject: "pod:a", UID: "uid-a",
		Resources: map[string]runtime.Resource{"memory": {Request: f64(1 << 30)}}})
	dr.Put(runtime.Workload{Subject: "pod:b", UID: "uid-b",
		Resources: map[string]runtime.Resource{"cpu": {Request: f64(0.1)}}})
	dr.Put(runtime.Workload{Subject: "unit:x", UID: "uid-x",
		Resources: map[string]runtime.Resource{"memory": {Request: f64(1)}}})
	req := &minimal.Requests{}
	var logged []string
	r := &requestsRefresher{rt: rt, req: req, logf: func(f string, a ...any) { logged = append(logged, f) }}
	r.refresh(context.Background())
	got, ok := req.Load()
	if !ok || !reflect.DeepEqual(got, map[string]float64{"uid-a": 1 << 30}) {
		t.Fatalf("requests %v (stored %v); want uid-a's only", got, ok)
	}

	failing := &listing{err: errors.New("apiserver down")}
	r.rt = failing
	r.refresh(context.Background())
	r.refresh(context.Background())
	if got, _ := req.Load(); !reflect.DeepEqual(got, map[string]float64{"uid-a": 1 << 30}) {
		t.Errorf("after a failed listing requests %v; want the last kept", got)
	}
	if len(logged) != 1 {
		t.Errorf("logged %v; want one line for the streak", logged)
	}
	failing.err, failing.ws = nil, []runtime.Workload{{Subject: "pod:c", UID: "uid-c",
		Resources: map[string]runtime.Resource{"memory": {Request: f64(2)}}}}
	r.refresh(context.Background())
	failing.err = errors.New("down again")
	r.refresh(context.Background())
	if len(logged) != 3 {
		t.Errorf("logged %v; want a recovery and the new streak said", logged)
	}
}

// TestNoRequestsOnTheDryRun: the dry run lists no requests, so the refresh is
// not started and the collector drops the request condition.
func TestNoRequestsOnTheDryRun(t *testing.T) {
	rt, err := runtime.Build("dry-run", runtime.Env{NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	req := &minimal.Requests{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if startRequestsLoop(ctx, "dry-run", rt, req, t.Logf) {
		t.Fatal("started on the dry run")
	}
	if _, ok := req.Load(); ok {
		t.Error("requests stored on the dry run")
	}
	l := &listing{ws: []runtime.Workload{{Subject: "pod:a", UID: "u",
		Resources: map[string]runtime.Resource{"memory": {Request: f64(5)}}}}}
	if !startRequestsLoop(ctx, "kubernetes", l, req, t.Logf) {
		t.Fatal("not started on kubernetes")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got, ok := req.Load(); ok {
			if got["u"] != 5 {
				t.Fatalf("requests %v", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no refresh at start")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// memTree is a cgroup v2 root with one burstable pod whose memory.high reads
// high, and a spec on kubernetes with relief on over it, the ledger at ledger.
func memTree(t *testing.T, high, extra string) (s spec.Spec, root, podDir, ledger string) {
	t.Helper()
	root = t.TempDir()
	const uid = "1234abcd-0000-1111-2222-333344445555"
	podDir = filepath.Join(root, "kubepods", "burstable", "pod"+uid)
	if err := os.MkdirAll(podDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{
		filepath.Join(root, "cgroup.controllers"): "cpu memory\n",
		filepath.Join(podDir, "memory.high"):      high + "\n",
	} {
		if err := os.WriteFile(name, []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ledger = filepath.Join(t.TempDir(), "actions.json")
	s, err := spec.Parse([]byte("runtime: {kind: kubernetes}\nloops: {relief: {enabled: true" + extra + "}}\n" +
		"sources: {collectors: [{name: cgroup, type: cgroup, config: {root: " + root + "}}]}\n" +
		"actuation: {ledger: " + ledger + "}\n"))
	if err != nil {
		t.Fatal(err)
	}
	s.Node.ID = "n1"
	return s, root, podDir, ledger
}

// TestADryRunOnAReadOnlyTreeStarts: dry_run on kubernetes never writes, so a
// read-only cgroup tree, as the DaemonSet's /host, does not refuse start; a real
// run on it still does.
func TestADryRunOnAReadOnlyTreeStarts(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through file modes")
	}
	dry, err := runtime.Build("dry-run", runtime.Env{NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	s, _, pod, _ := memTree(t, "max", ", dry_run: true")
	if err := os.Chmod(filepath.Join(pod, "memory.high"), 0o444); err != nil {
		t.Fatal(err)
	}
	if _, _, err := buildActing(s, named{dry}, t.Logf); err != nil {
		t.Fatalf("dry run refused on a read-only tree: %v", err)
	}
	s.Loops.Relief.DryRun = nil
	if _, _, err := buildActing(s, named{dry}, t.Logf); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("real run: err %v; want the read-only refusal", err)
	}
}

// TestASidecarsRequestIsPartOfThePodsPromise: the memory contention signal
// protects a pod within its request; the request the collector is given is the
// whole pod's, sidecar included, so a victim using its main container's request
// plus its sidecar's is still counted as protected.
func TestASidecarsRequestIsPartOfThePodsPromise(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	mem := func(q string) corev1.ResourceList {
		return corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(q)}
	}
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "victim", Namespace: "default", UID: "uid-v"},
		Spec: corev1.PodSpec{NodeName: "n1",
			Containers:     []corev1.Container{{Name: "app", Resources: corev1.ResourceRequirements{Requests: mem("256Mi")}}},
			InitContainers: []corev1.Container{{Name: "proxy", RestartPolicy: &always, Resources: corev1.ResourceRequirements{Requests: mem("64Mi")}}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, QOSClass: corev1.PodQOSBurstable},
	}
	rt := kubernetes.New(runtime.Env{NodeID: "n1", Spec: spec.RuntimeSpec{Kind: "kubernetes"}}, fake.NewSimpleClientset(p))
	req := &minimal.Requests{}
	(&requestsRefresher{rt: rt, req: req, logf: t.Logf}).refresh(context.Background())
	got, ok := req.Load()
	if !ok || got["uid-v"] != 320<<20 {
		t.Fatalf("requests %v; want the victim's 320Mi, sidecar included", got)
	}
}
