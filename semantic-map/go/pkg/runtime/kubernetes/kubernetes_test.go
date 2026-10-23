package kubernetes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/DiyazY/di-agent/pkg/actuators"
	"github.com/DiyazY/di-agent/pkg/runtime"
	"github.com/DiyazY/di-agent/pkg/spec"
)

type podOpt func(*corev1.Pod)

func rl(cpu string) corev1.ResourceList {
	if cpu == "" {
		return nil
	}
	return corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}
}

// burstable is a running pod on node with a request and a limit.
func burstable(name, node, uid, request, limit string, opts ...podOpt) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(uid)},
		Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{
			Name:      "c",
			Resources: corev1.ResourceRequirements{Requests: rl(request), Limits: rl(limit)},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, QOSClass: corev1.PodQOSBurstable},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func newRT(node string, pods ...*corev1.Pod) (*Runtime, *fake.Clientset) {
	cs := fake.NewSimpleClientset()
	for _, p := range pods {
		if _, err := cs.CoreV1().Pods(p.Namespace).Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
			panic(err)
		}
	}
	return New(runtime.Env{NodeID: node, Spec: spec.RuntimeSpec{Kind: "kubernetes"}}, cs), cs
}

func getPod(t *testing.T, cs *fake.Clientset, name string) *corev1.Pod {
	t.Helper()
	p, err := cs.CoreV1().Pods("default").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func workload(t *testing.T, r *Runtime, subject string) runtime.Workload {
	t.Helper()
	ws, err := r.Workloads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range ws {
		if w.Subject == subject {
			return w
		}
	}
	t.Fatalf("no workload %q in %+v", subject, ws)
	return runtime.Workload{}
}

func TestOnlyThisNodesPodsAreWorkloads(t *testing.T) {
	r, _ := newRT("n1", burstable("a", "n1", "ua", "100m", "1"), burstable("b", "n2", "ub", "100m", "1"))
	ws, err := r.Workloads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 || ws[0].Subject != "pod:ua" || ws[0].Name != "a" {
		t.Fatalf("workloads %+v; want only pod:ua named a", ws)
	}
}

func TestTheSpecsNodeOverridesTheNodeID(t *testing.T) {
	cs := fake.NewSimpleClientset(burstable("a", "k8s-name", "ua", "100m", "1"))
	r := New(runtime.Env{NodeID: "n1", Spec: spec.RuntimeSpec{Node: "k8s-name"}}, cs)
	if ws, _ := r.Workloads(context.Background()); len(ws) != 1 {
		t.Fatalf("workloads %+v", ws)
	}
}

func TestRuntimeAttributesAreLabels(t *testing.T) {
	p := burstable("a", "n1", "ua", "100m", "1", func(p *corev1.Pod) {
		p.Labels = map[string]string{"app": "web"}
		p.Spec.PriorityClassName = "system-node-critical"
		p.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "x"}, {Kind: "Other"}}
	})
	r, _ := newRT("n1", p)
	l := workload(t, r, "pod:ua").Labels
	want := map[string]string{
		"app": "web", "k8s/namespace": "default", "k8s/priority-class": "system-node-critical",
		"k8s/qos": "Burstable", "k8s/owner-kind": "DaemonSet",
	}
	for k, v := range want {
		if l[k] != v {
			t.Errorf("label %s = %q, want %q (labels %v)", k, l[k], v, l)
		}
	}
}

func TestCPUSumsContainers(t *testing.T) {
	p := burstable("a", "n1", "ua", "100m", "500m")
	p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "d", Resources: corev1.ResourceRequirements{Requests: rl("200m"), Limits: rl("1")}})
	r, _ := newRT("n1", p)
	w := workload(t, r, "pod:ua")
	if c := w.Resources["cpu"]; c.Request == nil || *c.Request != 0.3 || c.Limit == nil || *c.Limit != 1.5 {
		t.Fatalf("cpu %+v", c)
	}
	p2 := burstable("b", "n1", "ub", "100m", "500m")
	p2.Spec.Containers = append(p2.Spec.Containers, corev1.Container{Name: "d"})
	r, _ = newRT("n1", p2)
	if l := workload(t, r, "pod:ub").Resources["cpu"].Limit; l != nil {
		t.Fatalf("limit %v; a container without one makes the pod's nil", *l)
	}
}

// set is the cpu.limit actuator's Set of v cores on subject.
func set(r *Runtime, subject string, v float64) error {
	return NewCPULimit(r).Set(context.Background(), runtime.Workload{Subject: subject}, v, true)
}

func TestTheLimitIsRoundedDownToMillicores(t *testing.T) {
	r, cs := newRT("n1", burstable("a", "n1", "ua", "", "1"))
	if err := set(r, "pod:ua", 0.2999); err != nil {
		t.Fatal(err)
	}
	if got := getPod(t, cs, "a").Spec.Containers[0].Resources.Limits.Cpu().String(); got != "299m" {
		t.Fatalf("limit %s; want 299m", got)
	}
	if v, _, err := NewCPULimit(r).Read(context.Background(), runtime.Workload{Subject: "pod:ua"}); err != nil || v != 0.299 {
		t.Fatalf("read %v err %v; want the value in force, 0.299", v, err)
	}
	if err := set(r, "pod:ua", 0.0001); err != nil {
		t.Fatal(err)
	}
	if got := getPod(t, cs, "a").Spec.Containers[0].Resources.Limits.Cpu().String(); got != "1m" {
		t.Fatalf("limit %s; want never below 1m", got)
	}
}

func TestANotFoundAboutSomethingElseIsNotGone(t *testing.T) {
	r, cs := newRT("n1", burstable("a", "n1", "ua", "100m", "1"))
	failResize(cs, apierrors.NewNotFound(schema.GroupResource{Resource: "pods/resize"}, "other"))
	if err := set(r, "pod:ua", 0.3); err == nil || errors.Is(err, actuators.ErrGone) {
		t.Fatalf("err %v; a 404 not about this pod is not ErrGone", err)
	}
}

func TestResizeLeavesMemoryAndRequestsAlone(t *testing.T) {
	p := burstable("a", "n1", "ua", "100m", "1")
	p.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("64Mi")
	p.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("128Mi")
	r, cs := newRT("n1", p)
	if err := set(r, "pod:ua", 0.3); err != nil {
		t.Fatal(err)
	}
	res := getPod(t, cs, "a").Spec.Containers[0].Resources
	if res.Limits.Memory().String() != "128Mi" || res.Requests.Memory().String() != "64Mi" || res.Requests.Cpu().String() != "100m" {
		t.Fatalf("resources %+v", res)
	}
}

// TestAFinishedPodIsGone: a pod that has finished has no limit to read or give
// back, so Read and Set say it is gone, as for a deleted one.
func TestAFinishedPodIsGone(t *testing.T) {
	for _, phase := range []corev1.PodPhase{corev1.PodSucceeded, corev1.PodFailed} {
		r, _ := newRT("n1", burstable("a", "n1", "ua", "100m", "1", func(p *corev1.Pod) { p.Status.Phase = phase }))
		a, w := NewCPULimit(r), runtime.Workload{Subject: "pod:ua"}
		if _, _, err := a.Read(context.Background(), w); !errors.Is(err, actuators.ErrGone) {
			t.Fatalf("%s: read err %v; want ErrGone", phase, err)
		}
		if err := a.Set(context.Background(), w, 1, true); !errors.Is(err, actuators.ErrGone) {
			t.Fatalf("%s: set err %v; want ErrGone", phase, err)
		}
	}
}

func TestAGonePodIsErrGone(t *testing.T) {
	r, cs := newRT("n1", burstable("a", "n1", "ua", "100m", "1"))
	ctx := context.Background()
	if err := cs.CoreV1().Pods("default").Delete(ctx, "a", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := set(r, "pod:ua", 0.3); !errors.Is(err, actuators.ErrGone) {
		t.Fatalf("set err %v; want ErrGone", err)
	}
}

func TestAPodThatRestartedUnderANewUIDIsErrGone(t *testing.T) {
	r, cs := newRT("n1", burstable("a", "n1", "ua", "100m", "1"))
	p := getPod(t, cs, "a")
	p.UID = "ua2"
	if _, err := cs.CoreV1().Pods("default").Update(context.Background(), p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := set(r, "pod:ua", 0.3); !errors.Is(err, actuators.ErrGone) {
		t.Fatalf("set err %v; want ErrGone", err)
	}
}

// TestAPodThatCannotTakeALimitIsNotApplied: Acting asks the actuator's Check
// before it changes anything.
func TestAPodThatCannotTakeALimitIsNotApplied(t *testing.T) {
	r, cs := newRT("n1", burstable("a", "n1", "ua", "100m", "1", func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending }))
	w := workload(t, r, "pod:ua")
	acting := actuators.NewActing("n1", actuators.MemoryLedger(), NewCPULimit(r))
	if _, err := acting.Apply(context.Background(), w, "cpu.limit", 0.3, time.Time{}, "d"); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("err %v; want the Check reason", err)
	}
	if got := getPod(t, cs, "a").Spec.Containers[0].Resources.Limits.Cpu().String(); got != "1" {
		t.Fatalf("limit %s; a refused apply must not touch the pod", got)
	}
}

func TestCanReasons(t *testing.T) {
	multi := burstable("m", "n1", "um", "100m", "1")
	multi.Spec.Containers = append(multi.Spec.Containers, corev1.Container{Name: "d", Resources: corev1.ResourceRequirements{Limits: rl("1")}})
	cases := []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{"possible", burstable("p", "n1", "u", "100m", "1"), ""},
		{"BestEffort", burstable("p", "n1", "u", "", "", func(p *corev1.Pod) { p.Status.QOSClass = corev1.PodQOSBestEffort }),
			"BestEffort: a limit would change the pod's QoS class"},
		{"no limit", burstable("p", "n1", "u", "100m", ""),
			"no CPU limit: a limit the agent added could never be removed"},
		{"Guaranteed", burstable("p", "n1", "u", "1", "1", func(p *corev1.Pod) { p.Status.QOSClass = corev1.PodQOSGuaranteed }),
			"Guaranteed: changing the limit alone would change the pod's QoS class"},
		{"multi", multi, "more than one container: the limit cannot be split by the agent"},
		{"deleting", burstable("p", "n1", "u", "100m", "1", func(p *corev1.Pod) { n := metav1.Now(); p.DeletionTimestamp = &n }), "being deleted"},
		{"sidecar", burstable("p", "n1", "u", "100m", "1", func(p *corev1.Pod) {
			a := corev1.ContainerRestartPolicyAlways
			p.Spec.InitContainers = []corev1.Container{{Name: "s", RestartPolicy: &a}}
		}), "pod-level resources or sidecars: the agent cannot account for them"},
		{"pod-level resources", burstable("p", "n1", "u", "100m", "1", func(p *corev1.Pod) { p.Spec.Resources = &corev1.ResourceRequirements{Limits: rl("2")} }),
			"pod-level resources or sidecars: the agent cannot account for them"},
		{"pending", burstable("p", "n1", "u", "100m", "1", func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending }), "not running"},
		{"restarts on resize", burstable("p", "n1", "u", "100m", "1", func(p *corev1.Pod) {
			p.Spec.Containers[0].ResizePolicy = []corev1.ContainerResizePolicy{
				{ResourceName: corev1.ResourceMemory, RestartPolicy: corev1.NotRequired},
				{ResourceName: corev1.ResourceCPU, RestartPolicy: corev1.RestartContainer},
			}
		}), "resizePolicy restarts the container for a CPU change"},
		{"resize without restart", burstable("p", "n1", "u", "100m", "1", func(p *corev1.Pod) {
			p.Spec.Containers[0].ResizePolicy = []corev1.ContainerResizePolicy{
				{ResourceName: corev1.ResourceCPU, RestartPolicy: corev1.NotRequired},
			}
		}), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, _ := newRT("n1", c.pod)
			w := workload(t, r, "pod:"+string(c.pod.UID))
			if got := NewCPULimit(r).Check(w); got != c.want {
				t.Fatalf("Check = %q, want %q", got, c.want)
			}
		})
	}
}

func TestMultiContainerPodsCannotBeResized(t *testing.T) {
	p := burstable("m", "n1", "um", "100m", "1")
	p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "d", Resources: corev1.ResourceRequirements{Limits: rl("1")}})
	r, _ := newRT("n1", p)
	if err := set(r, "pod:um", 0.3); err == nil {
		t.Fatal("set on a two-container pod")
	}
}

func TestPodsNotRunningAreListed(t *testing.T) {
	r, _ := newRT("n1", burstable("a", "n1", "ua", "100m", "1", func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }))
	if got := NewCPULimit(r).Check(workload(t, r, "pod:ua")); got != "not running" {
		t.Fatalf("check %q", got)
	}
}

// TestALimitThatWouldMakeThePodGuaranteedIsRefused: a CPU limit equal to the
// request, on a pod whose memory request equals its limit, changes the QoS class.
func TestALimitThatWouldMakeThePodGuaranteedIsRefused(t *testing.T) {
	mem := resource.MustParse("64Mi")
	r, cs := newRT("n1", burstable("a", "n1", "ua", "500m", "1", func(p *corev1.Pod) {
		c := &p.Spec.Containers[0].Resources
		c.Requests[corev1.ResourceMemory], c.Limits[corev1.ResourceMemory] = mem, mem
	}))
	if err := set(r, "pod:ua", 0.5); err == nil || !strings.Contains(err.Error(), "Guaranteed") {
		t.Fatalf("err %v; want the QoS change refused", err)
	}
	if got := getPod(t, cs, "a").Spec.Containers[0].Resources.Limits.Cpu().String(); got != "1" {
		t.Fatalf("pod touched: limit %s", got)
	}
	if err := set(r, "pod:ua", 0.6); err != nil {
		t.Fatalf("a limit above the request: %v", err)
	}
}

// TestSetAtTheLimitInForceSendsNoResize: a pod already at the value, as after a
// resize that never took, needs no resize to be given back, so a resize the API
// keeps refusing cannot keep a change in force that is not there.
func TestSetAtTheLimitInForceSendsNoResize(t *testing.T) {
	r, cs := newRT("n1", burstable("a", "n1", "ua", "100m", "1"))
	failResize(cs, errors.New("timeout"))
	if err := set(r, "pod:ua", 1); err != nil {
		t.Fatalf("set: %v; want no resize for the limit in force", err)
	}
	if err := set(r, "pod:ua", 0.5); err == nil {
		t.Fatal("want the resize error for a new limit")
	}
}

func failResize(cs *fake.Clientset, err error) {
	cs.PrependReactor("patch", "pods", func(a k8stesting.Action) (bool, apiruntime.Object, error) {
		if a.GetSubresource() == "resize" {
			return true, nil, err
		}
		return false, nil, nil
	})
}

func TestTheAgentsOwnPodIsSelf(t *testing.T) {
	t.Setenv("POD_UID", "ua")
	r, _ := newRT("n1", burstable("a", "n1", "ua", "100m", "1"), burstable("b", "n1", "ub", "100m", "1"))
	if !workload(t, r, "pod:ua").Self || workload(t, r, "pod:ub").Self {
		t.Fatal("only pod ua is the agent's own")
	}
}

func TestNoPodUIDMeansNoSelf(t *testing.T) {
	t.Setenv("POD_UID", "")
	r, _ := newRT("n1", burstable("a", "n1", "", "100m", "1"))
	if workload(t, r, "pod:").Self {
		t.Fatal("an empty POD_UID must not match a pod with an empty UID")
	}
}

func TestTheFactoryNamesTheSpecPath(t *testing.T) {
	_, err := runtime.Build("kubernetes", runtime.Env{NodeID: "n1", Spec: spec.RuntimeSpec{Kind: "kubernetes", Kubeconfig: "/nonexistent/kubeconfig"}})
	if err == nil || !strings.Contains(err.Error(), "runtime.kubeconfig") {
		t.Fatalf("err %v; want it to name runtime.kubeconfig", err)
	}
}

// TestInClusterNeedsPodUID: without POD_UID the agent cannot recognise its own
// pod, and nothing else stops it capping itself, so the factory refuses.
func TestInClusterNeedsPodUID(t *testing.T) {
	t.Setenv("POD_UID", "")
	_, err := runtime.Build("kubernetes", runtime.Env{NodeID: "n1", Spec: spec.RuntimeSpec{Kind: "kubernetes"}})
	if err == nil || !strings.Contains(err.Error(), "POD_UID") || !strings.Contains(err.Error(), "metadata.uid") {
		t.Fatalf("err %v; want one naming POD_UID and the downward API's metadata.uid", err)
	}
}

// TestTheClientTimesOut: a request to an API server that does not answer ends.
func TestTheClientTimesOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	doc := `apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://127.0.0.1:1"}}]
users: [{name: u, user: {token: t}}]
contexts: [{name: x, context: {cluster: c, user: u}}]
current-context: x
`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := restConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timeout <= 0 || cfg.Timeout > time.Minute {
		t.Fatalf("timeout %s; want a bound on every request", cfg.Timeout)
	}
}

// TestThePodKindIsAPolicyKind: a rule matching kind pod must validate, or no rule
// could single out this runtime's workloads.
func TestThePodKindIsAPolicyKind(t *testing.T) {
	kind, _, _ := strings.Cut(subjectOf(*burstable("a", "n1", "ua", "100m", "1")), ":")
	if !slices.Contains(spec.WorkloadKinds, kind) {
		t.Fatalf("spec.WorkloadKinds %v lacks %q", spec.WorkloadKinds, kind)
	}
}
