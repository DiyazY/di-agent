package kubernetes

import (
	"context"
	"errors"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/DiyazY/di-agent/pkg/actuators"
	"github.com/DiyazY/di-agent/pkg/runtime"
	"github.com/DiyazY/di-agent/pkg/spec"
)

func TestCPULimitDescriptor(t *testing.T) {
	r, _ := newRT("n1")
	d := NewCPULimit(r).Describe()
	if d.Action != "cpu.limit" || d.Resource != "cpu" || d.Unit != "cores" ||
		d.Capacity != "cpu_cores" || d.Relieves != "cpu_contention_ratio" || d.Usage != "cpu_utilization" || d.Min != 0 {
		t.Fatalf("descriptor %+v", d)
	}
}

func TestCPULimitCheckReasons(t *testing.T) {
	cases := []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{"ok", burstable("p", "n1", "u", "100m", "1"), ""},
		{"not running", burstable("p", "n1", "u", "100m", "1", func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending }), "not running"},
		{"two containers", burstable("p", "n1", "u", "100m", "1", func(p *corev1.Pod) {
			p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "d"})
		}), "more than one container: the limit cannot be split by the agent"},
		{"besteffort", burstable("p", "n1", "u", "", "", func(p *corev1.Pod) { p.Status.QOSClass = corev1.PodQOSBestEffort }),
			"BestEffort: a limit would change the pod's QoS class"},
		{"no limit", burstable("p", "n1", "u", "100m", ""), "no CPU limit: a limit the agent added could never be removed"},
		{"guaranteed", burstable("p", "n1", "u", "1", "1", func(p *corev1.Pod) { p.Status.QOSClass = corev1.PodQOSGuaranteed }),
			"Guaranteed: changing the limit alone would change the pod's QoS class"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rt, _ := newRT("n1", c.pod)
			got := NewCPULimit(rt).Check(workload(t, rt, "pod:u"))
			if got != c.want {
				t.Fatalf("check %q, want %q", got, c.want)
			}
		})
	}
	rt, _ := newRT("n1", burstable("p", "n1", "u", "100m", "1"))
	w := workload(t, rt, "pod:u")
	w.Self = true
	if got := NewCPULimit(rt).Check(w); got != "the agent itself" {
		t.Fatalf("self: %q", got)
	}
	if got := NewCPULimit(rt).Check(runtime.Workload{Subject: "pod:never"}); got != "not a workload the runtime lists" {
		t.Fatalf("unlisted: %q", got)
	}
}

// TestBuildPicksTheActuatorForTheRuntime: cpu.limit over a Kubernetes runtime is
// the real actuator; over any other it is the stand-in that touches nothing.
func TestBuildPicksTheActuatorForTheRuntime(t *testing.T) {
	r, _ := newRT("n1")
	a, err := actuators.Build("cpu.limit", actuators.Env{NodeID: "n1", Runtime: r})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.(*CPULimit); !ok {
		t.Fatalf("actuator %T over a kubernetes runtime; want *CPULimit", a)
	}
	dry, err := runtime.Build("dry-run", runtime.Env{NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	a, err = actuators.Build("cpu.limit", actuators.Env{NodeID: "n1", Runtime: dry})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.(*CPULimit); ok || a.Describe().Action != "cpu.limit" {
		t.Fatalf("actuator %T (%s) over dry-run; want the static stand-in", a, a.Describe().Action)
	}
	l := 0.5
	w := runtime.Workload{Subject: "pod:x", Resources: map[string]runtime.Resource{"cpu": {Limit: &l}}}
	if v, set, err := a.Read(context.Background(), w); err != nil || !set || v != 0.5 {
		t.Fatalf("stand-in read %v %v %v; want the inventory's limit", v, set, err)
	}
}

// TestThePolicyKnowsEveryAction: the spec refuses an allow or deny entry that is
// not in its own list; every registered action must be there, or no policy could
// allow it.
func TestThePolicyKnowsEveryAction(t *testing.T) {
	for _, a := range actuators.Names() {
		if !slices.Contains(spec.PolicyActions, a) {
			t.Errorf("spec.PolicyActions %v lacks %q", spec.PolicyActions, a)
		}
	}
}

func TestCPULimitReadSet(t *testing.T) {
	r, cs := newRT("n1", burstable("p", "n1", "u", "100m", "1"))
	a := NewCPULimit(r)
	w := workload(t, r, "pod:u")
	ctx := context.Background()
	if v, set, err := a.Read(ctx, w); err != nil || !set || v != 1 {
		t.Fatalf("read %v %v %v", v, set, err)
	}
	if err := a.Set(ctx, w, 0.25, true); err != nil {
		t.Fatal(err)
	}
	p := getPod(t, cs, "p")
	if l := p.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]; l.MilliValue() != 250 {
		t.Fatalf("limit %v", l.String())
	}
	if len(p.Annotations) != 0 {
		t.Fatalf("annotations %v; the actuator records nothing on the pod", p.Annotations)
	}
	if v, set, _ := a.Read(ctx, w); !set || v != 0.25 {
		t.Fatalf("read after set %v %v", v, set)
	}
}

func TestCPULimitUnsetRemovesLimit(t *testing.T) {
	r, cs := newRT("n1", burstable("p", "n1", "u", "100m", "1"))
	a := NewCPULimit(r)
	w := workload(t, r, "pod:u")
	ctx := context.Background()
	if err := a.Set(ctx, w, 0, false); err != nil {
		t.Fatal(err)
	}
	p := getPod(t, cs, "p")
	if _, ok := p.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]; ok {
		t.Fatalf("limit still there: %v", p.Spec.Containers[0].Resources.Limits)
	}
	if req := p.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]; req.MilliValue() != 100 {
		t.Fatal("request must stay")
	}
	if _, set, err := a.Read(ctx, w); err != nil || set {
		t.Fatalf("read %v %v", set, err)
	}
}

func TestCPULimitGone(t *testing.T) {
	r, _ := newRT("n1")
	a := NewCPULimit(r)
	w := runtime.Workload{Subject: "pod:missing"}
	if _, _, err := a.Read(context.Background(), w); !errors.Is(err, actuators.ErrGone) {
		t.Fatalf("read: %v", err)
	}
	if err := a.Set(context.Background(), w, 1, true); !errors.Is(err, actuators.ErrGone) {
		t.Fatalf("set: %v", err)
	}
}

func TestWorkloadsFillMemoryAndQoS(t *testing.T) {
	p := burstable("a", "n1", "ua", "100m", "1")
	p.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("64Mi")
	p.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("128Mi")
	p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "d", Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("32Mi")},
	}})
	r, _ := newRT("n1", p)
	w := workload(t, r, "pod:ua")
	if w.UID != "ua" || w.QoS != "burstable" {
		t.Fatalf("uid %q qos %q", w.UID, w.QoS)
	}
	m := w.Resources["memory"]
	if m.Request == nil || *m.Request != 96*1024*1024 {
		t.Fatalf("memory request %v", m.Request)
	}
	if m.Limit != nil {
		t.Fatal("a container without a memory limit makes the pod's nil")
	}
	if c := w.Resources["cpu"]; c.Request == nil || c.Limit != nil {
		t.Fatalf("cpu %+v", c)
	}
}

func mem(q string) corev1.ResourceList {
	return corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(q)}
}

// TestMemoryCountsSidecarsAndPodLevelResources: the pod cgroup memory.high acts
// on holds every container that keeps running, so a native sidecar's (a
// restartable init container's) request and limit are part of the pod's; an
// init container that runs to completion is not. Pod-level resources, when set,
// are the pod's.
func TestMemoryCountsSidecarsAndPodLevelResources(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	p := burstable("a", "n1", "ua", "100m", "1")
	p.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("64Mi")
	p.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("128Mi")
	p.Spec.InitContainers = []corev1.Container{
		{Name: "proxy", RestartPolicy: &always, Resources: corev1.ResourceRequirements{Requests: mem("32Mi"), Limits: mem("64Mi")}},
		{Name: "migrate", Resources: corev1.ResourceRequirements{Requests: mem("1Gi"), Limits: mem("1Gi")}},
	}
	r, _ := newRT("n1", p)
	m := workload(t, r, "pod:ua").Resources["memory"]
	if m.Request == nil || *m.Request != 96<<20 || m.Limit == nil || *m.Limit != 192<<20 {
		t.Fatalf("memory %v / %v; want the sidecar counted: 96Mi request, 192Mi limit", m.Request, m.Limit)
	}

	q := burstable("b", "n1", "ub", "100m", "1")
	q.Spec.Resources = &corev1.ResourceRequirements{Requests: mem("300Mi"), Limits: mem("512Mi")}
	r, _ = newRT("n1", q)
	m = workload(t, r, "pod:ub").Resources["memory"]
	if m.Request == nil || *m.Request != 300<<20 || m.Limit == nil || *m.Limit != 512<<20 {
		t.Fatalf("memory %v / %v; want the pod-level 300Mi / 512Mi", m.Request, m.Limit)
	}
}
