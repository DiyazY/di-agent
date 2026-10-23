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

// Read is the pod's CPU limit in force, in cores; set is false when it has none.
// In force is what the kubelet has applied, the container's status.resources,
// when the status reports it, and the spec otherwise. The limit in force is not
// settled, and Read is an error (never ErrGone), while a resize is pending or in
// progress, while the kubelet has not observed the pod's latest generation, and
// while the spec and the applied limit differ, as right after a resize the
// kubelet has accepted and not yet applied: the caller cannot tell what it
// would be giving back.
func (a *CPULimit) Read(ctx context.Context, w runtime.Workload) (float64, bool, error) {
	p, err := a.pod(ctx, w)
	if err != nil {
		return 0, false, err
	}
	if c, ok := resizeCondition(p); ok {
		return 0, false, fmt.Errorf("%s/%s: a resize is %s (%s): the CPU limit in force is not settled", p.Namespace, p.Name, resizeState(c), c.Message)
	}
	if p.Generation > p.Status.ObservedGeneration && p.Status.ObservedGeneration > 0 {
		return 0, false, fmt.Errorf("%s/%s: the kubelet has not yet observed generation %d (at %d): the CPU limit in force is not settled",
			p.Namespace, p.Name, p.Generation, p.Status.ObservedGeneration)
	}
	specMilli, specSet := specLimit(p)
	appliedMilli, appliedSet, reported := statusLimit(p)
	if !reported {
		appliedMilli, appliedSet = specMilli, specSet
	} else if appliedSet != specSet || appliedMilli != specMilli {
		// Accepted and not yet applied, as right after a resize: the kubelet lags.
		return 0, false, fmt.Errorf("%s/%s: the spec asks for a CPU limit the kubelet has not applied yet: the CPU limit in force is not settled", p.Namespace, p.Name)
	}
	if !appliedSet {
		return 0, false, nil
	}
	return float64(appliedMilli) / 1000, true, nil
}

// specLimit is the single container's spec limits.cpu in millicores.
func specLimit(p corev1.Pod) (int64, bool) {
	if len(p.Spec.Containers) != 1 {
		if l := cpuOf(p).Limit; l != nil {
			return millis(*l), true
		}
		return 0, false
	}
	q, ok := p.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]
	return q.MilliValue(), ok
}

// statusLimit is the CPU limit the kubelet reports applied to the single
// container, from status.containerStatuses[].resources; reported is false when
// the status says nothing of it.
func statusLimit(p corev1.Pod) (milli int64, set, reported bool) {
	if len(p.Spec.Containers) != 1 {
		return 0, false, false
	}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Name != p.Spec.Containers[0].Name || cs.Resources == nil {
			continue
		}
		q, ok := cs.Resources.Limits[corev1.ResourceCPU]
		return q.MilliValue(), ok, true
	}
	return 0, false, false
}

// resizeCondition is the pod's PodResizePending or PodResizeInProgress
// condition, when one is true.
func resizeCondition(p corev1.Pod) (corev1.PodCondition, bool) {
	for _, c := range p.Status.Conditions {
		if (c.Type == corev1.PodResizePending || c.Type == corev1.PodResizeInProgress) && c.Status == corev1.ConditionTrue {
			return c, true
		}
	}
	return corev1.PodCondition{}, false
}

// resizeState names a resize condition: pending with its reason (Deferred,
// Infeasible), or in progress.
func resizeState(c corev1.PodCondition) string {
	if c.Type == corev1.PodResizePending {
		return "pending, " + c.Reason
	}
	return "in progress"
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
	if err := a.r.resize(ctx, p, milli); err != nil {
		return err
	}
	return a.refused(ctx, w)
}

// refused is a Set error when, after the resize call returned, the kubelet has
// marked the resize Infeasible or Deferred: the spec asks for it, and it has not
// taken effect.
func (a *CPULimit) refused(ctx context.Context, w runtime.Workload) error {
	p, err := a.pod(ctx, w)
	if err != nil {
		return err
	}
	c, ok := resizeCondition(p)
	if !ok || c.Type != corev1.PodResizePending || (c.Reason != corev1.PodReasonInfeasible && c.Reason != corev1.PodReasonDeferred) {
		return nil
	}
	return fmt.Errorf("%s/%s: the kubelet holds the resize %s: %s", p.Namespace, p.Name, c.Reason, c.Message)
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
// milli millicores, both asked for in the spec and applied, as far as the status
// reports it: a spec at milli whose status still holds another limit is not at
// it, so a retried give-back is not taken for done.
func atLimit(p corev1.Pod, milli int64) bool {
	if len(p.Spec.Containers) != 1 {
		return false
	}
	l, ok := p.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]
	if !ok || l.MilliValue() != milli {
		return false
	}
	if a, set, reported := statusLimit(p); reported && (!set || a != milli) {
		return false
	}
	return true
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
