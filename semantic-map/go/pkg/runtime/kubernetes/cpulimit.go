package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/DiyazY/di-agent/pkg/actuators"
	"github.com/DiyazY/di-agent/pkg/runtime"
)

// CPULimitDescriptor describes the cpu.limit action, for the real actuator and
// for the dry-run stand-in alike. It relieves node CPU contention rather than
// cpu_pressure_ratio because a cgroup's stall counts the time its own quota held
// it back: a pod at its limit raises node pressure with nobody else waiting, and
// lowering its limit raises it further, so the lever would move the signal the
// wrong way. Usage is a share of node capacity in [0,1], and so is each pod's
// cpu_utilization@<subject>; cpu_cores turns a share into cores.
var CPULimitDescriptor = actuators.Descriptor{
	Action:   "cpu.limit",
	Kinds:    []string{"pod"},
	Resource: "cpu",
	Unit:     "cores",
	Capacity: "cpu_cores",
	Min:      0,
	Relieves: "cpu_contention_ratio",
	Usage:    "cpu_utilization",
	Describe: "cap a pod's CPU limit in place, through the resize subresource",
}

func init() {
	actuators.Register("cpu.limit", func(env actuators.Env) (actuators.Actuator, error) {
		if r, ok := env.Runtime.(*Runtime); ok {
			return NewCPULimit(r), nil
		}
		return actuators.Static(CPULimitDescriptor, staticCheck, cpuLimitInitial), nil
	})
}

// staticCheck is Check for the dry-run stand-in, which changes nothing: it
// refuses only the agent's own workload, so a dry run acts on every workload the
// map holds, limited or not, and giving back a cap restores "no limit".
func staticCheck(w runtime.Workload) string {
	if w.Self {
		return "the agent itself"
	}
	return ""
}

func cpuLimitInitial(w runtime.Workload) (float64, bool) {
	if l := w.Resources["cpu"].Limit; l != nil {
		return *l, true
	}
	return 0, false
}

// CPULimit is the cpu.limit actuator over a Kubernetes runtime. It keeps no
// record on the pod: the agent's ledger holds what to give back.
type CPULimit struct{ r *Runtime }

// NewCPULimit builds the actuator over r.
func NewCPULimit(r *Runtime) actuators.Actuator { return &CPULimit{r: r} }

func (a *CPULimit) Describe() actuators.Descriptor { return CPULimitDescriptor }

// Check is why the pod's CPU limit cannot be changed, or "" when it can, as the
// runtime's latest listing found the pod.
func (a *CPULimit) Check(w runtime.Workload) string {
	if w.Self {
		return "the agent itself"
	}
	why, listed := a.r.reason(w.Subject)
	if !listed {
		return "not a workload the runtime lists"
	}
	return why
}

// Read is the pod's limits.cpu in cores; set is false when it has none.
func (a *CPULimit) Read(ctx context.Context, w runtime.Workload) (float64, bool, error) {
	p, err := a.pod(ctx, w)
	if err != nil {
		return 0, false, err
	}
	if l := cpuOf(p).Limit; l != nil {
		return *l, true, nil
	}
	return 0, false, nil
}

// Set resizes the pod's limits.cpu to v cores, or removes it when set is false. A
// pod already at v is not resized, so giving back a change that never took
// cannot fail on a resize the API refuses.
func (a *CPULimit) Set(ctx context.Context, w runtime.Workload, v float64, set bool) error {
	p, err := a.pod(ctx, w)
	if err != nil {
		return err
	}
	if !set {
		return a.r.removeLimit(ctx, p)
	}
	milli := millis(v)
	if atLimit(p, milli) {
		return nil
	}
	if wouldBeGuaranteed(p, milli) {
		return fmt.Errorf("%s/%s cannot take a cpu.limit of %dm: equal to its request, with memory "+
			"request equal to limit, it would make the pod Guaranteed, a QoS class change", p.Namespace, p.Name, milli)
	}
	return a.r.resize(ctx, p, milli)
}

// pod is the pod behind w, or actuators.ErrGone: a pod that has finished has no
// limit left to read or give back, so it is gone too.
func (a *CPULimit) pod(ctx context.Context, w runtime.Workload) (corev1.Pod, error) {
	p, err := a.r.find(ctx, w.Subject)
	if err != nil {
		return p, err
	}
	if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
		return p, fmt.Errorf("%s/%s has finished: %w", p.Namespace, p.Name, actuators.ErrGone)
	}
	return p, nil
}

// atLimit reports whether the pod's single container already has a CPU limit of
// milli millicores.
func atLimit(p corev1.Pod, milli int64) bool {
	if len(p.Spec.Containers) != 1 {
		return false
	}
	l, ok := p.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]
	return ok && l.MilliValue() == milli
}

// removeLimit drops the single container's limits.cpu through the resize
// subresource.
func (r *Runtime) removeLimit(ctx context.Context, p corev1.Pod) error {
	if len(p.Spec.Containers) != 1 {
		return fmt.Errorf("%s/%s has %d containers; the limit cannot be split", p.Namespace, p.Name, len(p.Spec.Containers))
	}
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"containers": []any{
		map[string]any{"name": p.Spec.Containers[0].Name, "resources": map[string]any{"limits": map[string]any{"cpu": nil}}},
	}}})
	if err != nil {
		return err
	}
	_, err = r.client.CoreV1().Pods(p.Namespace).Patch(ctx, p.Name, types.StrategicMergePatchType, patch, metav1.PatchOptions{}, "resize")
	return r.wrap(err, p, "resize")
}
