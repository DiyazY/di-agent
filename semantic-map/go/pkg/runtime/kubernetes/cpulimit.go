package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/DiyazY/di-agent/pkg/actuators"
	"github.com/DiyazY/di-agent/pkg/runtime"
)

// CPULimitDescriptor describes the cpu.limit action, for the real actuator and
// for the dry-run stand-in alike.
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

// staticCheck is Check for the dry-run stand-in: the workload's own inventory
// says whether it has a limit to change.
func staticCheck(w runtime.Workload) string {
	if w.Self {
		return "the agent itself"
	}
	if w.Resources["cpu"].Limit == nil {
		return "no cpu limit"
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

// Check is why the pod's CPU limit cannot be changed, or "" when it can.
func (a *CPULimit) Check(w runtime.Workload) string {
	if w.Self {
		return "the agent itself"
	}
	return w.Can[runtime.ActionCPULimit]
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

// Set resizes the pod's limits.cpu to v cores, or removes it when set is false.
func (a *CPULimit) Set(ctx context.Context, w runtime.Workload, v float64, set bool) error {
	p, err := a.pod(ctx, w)
	if err != nil {
		return err
	}
	if !set {
		return gone(a.r.removeLimit(ctx, p))
	}
	milli := millis(v)
	if wouldBeGuaranteed(p, milli) {
		return fmt.Errorf("%s/%s cannot take a cpu.limit of %dm: equal to its request, with memory "+
			"request equal to limit, it would make the pod Guaranteed, a QoS class change", p.Namespace, p.Name, milli)
	}
	return gone(a.r.resize(ctx, p, milli))
}

// pod is the pod behind w, or actuators.ErrGone.
func (a *CPULimit) pod(ctx context.Context, w runtime.Workload) (corev1.Pod, error) {
	p, err := a.r.find(ctx, w.Subject)
	return p, gone(err)
}

// gone turns the runtime's ErrGone into the actuators' one.
func gone(err error) error {
	if errors.Is(err, runtime.ErrGone) {
		return fmt.Errorf("%v: %w", err, actuators.ErrGone)
	}
	return err
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
