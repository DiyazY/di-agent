package kubernetes

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/DiyazY/di-agent/pkg/actuators"
	"github.com/DiyazY/di-agent/pkg/runtime"
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
		{"another node's record", burstable("p", "n1", "u", "100m", "1", func(p *corev1.Pod) {
			p.Annotations = map[string]string{Annotation: `{"node_id":"other"}`}
		}), "changed by another node: other"},
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
	if _, ok := p.Annotations[Annotation]; ok {
		t.Fatal("the actuator must not write the change annotation")
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
