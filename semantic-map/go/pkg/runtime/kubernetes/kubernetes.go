// Package kubernetes is the runtime that acts on a Kubernetes node's pods. It
// lists the pods scheduled on this node, and changes one pod's CPU limit in place
// through the pod "resize" subresource, so the container is never restarted.
//
// Every change is recorded on the pod itself, in the Annotation, as the JSON of
// the runtime.Undo that gives it back. That keeps the record where the pod is: a
// restarted agent finds its outstanding changes with Changed, and a pod that is
// deleted takes its record with it.
package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/DiyazY/di-agent/pkg/runtime"
)

// Annotation is the pod annotation that holds the JSON of the runtime.Undo of the
// outstanding change on that pod.
const Annotation = "di-agent/change"

// The labels a workload carries beside the pod's own.
const (
	LabelNamespace     = "k8s/namespace"
	LabelPriorityClass = "k8s/priority-class"
	LabelQoS           = "k8s/qos"
	LabelOwnerKind     = "k8s/owner-kind"
)

// PodUIDEnv is the environment variable that names the agent's own pod, set from
// the downward API's metadata.uid. Without it the agent cannot recognise itself
// among the pods it may cap.
const PodUIDEnv = "POD_UID"

// callTimeout bounds every request to the API server, so one that stops
// answering cannot hold the relief loop.
const callTimeout = 30 * time.Second

func init() {
	runtime.Register("kubernetes", func(env runtime.Env) (runtime.Runtime, error) {
		if os.Getenv(PodUIDEnv) == "" {
			if env.Spec.Kubeconfig == "" {
				return nil, fmt.Errorf("runtime.kind kubernetes in a cluster needs %s, the agent's own pod UID, "+
					"from the downward API (env %s: valueFrom.fieldRef.fieldPath: metadata.uid); "+
					"without it the agent cannot recognise its own pod and could cap itself", PodUIDEnv, PodUIDEnv)
			}
			log.Printf("runtime: %s is unset, so the agent's own pod cannot be recognised among this node's pods; "+
				"run it outside the cluster, or set %s from metadata.uid", PodUIDEnv, PodUIDEnv)
		}
		client, err := newClient(env.Spec.Kubeconfig)
		if err != nil {
			return nil, err
		}
		return New(env, client), nil
	})
}

// newClient is the in-cluster client when no kubeconfig is named, else the one the
// file describes. Neither connects: the first call does.
func newClient(kubeconfig string) (kubernetes.Interface, error) {
	cfg, err := restConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("runtime.kubeconfig: %w", err)
	}
	return client, nil
}

// restConfig is the client configuration for a kubeconfig, or in-cluster.
func restConfig(kubeconfig string) (*rest.Config, error) {
	var cfg *rest.Config
	var err error
	if kubeconfig == "" {
		cfg, err = rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("runtime.kubeconfig is empty and the agent is not in a cluster: %w", err)
		}
	} else {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("runtime.kubeconfig %s: %w", kubeconfig, err)
		}
	}
	cfg.Timeout = callTimeout
	return cfg, nil
}

// Runtime acts on the pods of one node.
type Runtime struct {
	env    runtime.Env
	client kubernetes.Interface
	node   string
}

// New builds the runtime over a client. The node is the spec's runtime.node, or
// the node ID.
func New(env runtime.Env, client kubernetes.Interface) *Runtime {
	node := env.Spec.Node
	if node == "" {
		node = env.NodeID
	}
	return &Runtime{env: env, client: client, node: node}
}

// Name is "kubernetes".
func (r *Runtime) Name() string { return "kubernetes" }

// pods lists this node's pods. The field selector narrows the list on a real API
// server; the node name is checked again because not every server honours it.
func (r *Runtime) pods(ctx context.Context) ([]corev1.Pod, error) {
	list, err := r.client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + r.node})
	if err != nil {
		return nil, fmt.Errorf("list pods on node %s: %w", r.node, err)
	}
	out := make([]corev1.Pod, 0, len(list.Items))
	for _, p := range list.Items {
		if p.Spec.NodeName == r.node {
			out = append(out, p)
		}
	}
	return out, nil
}

func subjectOf(p corev1.Pod) string { return "pod:" + string(p.UID) }

// find is the pod behind a subject, or ErrGone: a pod that was deleted, and one
// that restarted under a new UID, are both not there.
func (r *Runtime) find(ctx context.Context, subject string) (corev1.Pod, error) {
	ps, err := r.pods(ctx)
	if err != nil {
		return corev1.Pod{}, err
	}
	for _, p := range ps {
		if subjectOf(p) == subject {
			return p, nil
		}
	}
	return corev1.Pod{}, fmt.Errorf("%s: %w", subject, runtime.ErrGone)
}

// Workloads lists every pod on the node, including those that are not running,
// each with the reason it cannot take a CPU cap, so a pod absent from the list is
// one that is really gone.
func (r *Runtime) Workloads(ctx context.Context) ([]runtime.Workload, error) {
	ps, err := r.pods(ctx)
	if err != nil {
		return nil, err
	}
	self := os.Getenv(PodUIDEnv)
	out := make([]runtime.Workload, 0, len(ps))
	for _, p := range ps {
		out = append(out, runtime.Workload{
			Subject: subjectOf(p),
			Name:    p.Name,
			Labels:  labelsOf(p),
			CPU:     cpuOf(p),
			Can:     map[string]string{runtime.ActionCPULimit: cannot(p, r.env.NodeID)},
			Self:    self != "" && string(p.UID) == self,
			UID:     string(p.UID),
			QoS:     strings.ToLower(string(p.Status.QOSClass)),
			Resources: map[string]runtime.Resource{
				"cpu":    cpuOf(p),
				"memory": memoryOf(p),
			},
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Subject < out[j].Subject })
	return out, nil
}

func labelsOf(p corev1.Pod) map[string]string {
	l := make(map[string]string, len(p.Labels)+4)
	for k, v := range p.Labels {
		l[k] = v
	}
	l[LabelNamespace] = p.Namespace
	l[LabelPriorityClass] = p.Spec.PriorityClassName
	l[LabelQoS] = string(p.Status.QOSClass)
	if len(p.OwnerReferences) > 0 {
		l[LabelOwnerKind] = p.OwnerReferences[0].Kind
	}
	return l
}

// cpuOf sums the containers' CPU in cores. A limit is nil when any container has
// none, because the pod as a whole is then unlimited.
func cpuOf(p corev1.Pod) runtime.Resource {
	var req, lim int64 // millicores, summed exactly
	var anyReq bool
	allLim := len(p.Spec.Containers) > 0
	for _, c := range p.Spec.Containers {
		if q, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
			req += q.MilliValue()
			anyReq = true
		}
		if q, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
			lim += q.MilliValue()
		} else {
			allLim = false
		}
	}
	var r runtime.Resource
	if anyReq {
		v := float64(req) / 1000
		r.Request = &v
	}
	if allLim {
		v := float64(lim) / 1000
		r.Limit = &v
	}
	return r
}

// memoryOf sums the containers' memory in bytes, by the rule cpuOf applies to
// CPU: a request is nil when any container has none, and so is a limit.
func memoryOf(p corev1.Pod) runtime.Resource {
	var req, lim float64
	allReq := len(p.Spec.Containers) > 0
	allLim := allReq
	for _, c := range p.Spec.Containers {
		if q, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
			req += q.AsApproximateFloat64()
		} else {
			allReq = false
		}
		if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
			lim += q.AsApproximateFloat64()
		} else {
			allLim = false
		}
	}
	var r runtime.Resource
	if allReq {
		r.Request = &req
	}
	if allLim {
		r.Limit = &lim
	}
	return r
}

// cannot is why the pod's CPU limit cannot be changed in place without side
// effects, or "" when it can. Most reasons are what the resize subresource refuses
// or does to a pod: a limit added to a BestEffort pod changes its QoS class and is
// refused; a limit added to a Burstable pod that had none is accepted but can never
// be removed again; a Guaranteed pod's limit is its request, so changing the limit
// alone changes the class. The rest protect the agent's own record: a pod another
// node changed, or whose record cannot be read, is left alone rather than
// overwritten.
func cannot(p corev1.Pod, nodeID string) string {
	if p.Status.Phase != corev1.PodRunning {
		return "not running"
	}
	if p.DeletionTimestamp != nil {
		return "being deleted"
	}
	if p.Spec.Resources != nil || hasSidecar(p) {
		return "pod-level resources or sidecars: the agent cannot account for them"
	}
	if len(p.Spec.Containers) != 1 {
		return "more than one container: the limit cannot be split by the agent"
	}
	if u, present, readable := decode(p); present {
		if !readable {
			return "unreadable " + Annotation + " annotation"
		}
		if u.NodeID != nodeID {
			return "changed by another node: " + u.NodeID
		}
	}
	if p.Status.QOSClass == corev1.PodQOSBestEffort {
		return "BestEffort: a limit would change the pod's QoS class"
	}
	if _, ok := p.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]; !ok {
		return "no CPU limit: a limit the agent added could never be removed"
	}
	if p.Status.QOSClass == corev1.PodQOSGuaranteed {
		return "Guaranteed: changing the limit alone would change the pod's QoS class"
	}
	if restartsOnCPUResize(p.Spec.Containers[0]) {
		return "resizePolicy restarts the container for a CPU change"
	}
	return ""
}

// restartsOnCPUResize reports a container whose CPU resizePolicy is
// RestartContainer: every cap and every revert would restart it.
func restartsOnCPUResize(c corev1.Container) bool {
	for _, rp := range c.ResizePolicy {
		if rp.ResourceName == corev1.ResourceCPU && rp.RestartPolicy == corev1.RestartContainer {
			return true
		}
	}
	return false
}

// wouldBeGuaranteed reports whether a CPU limit of milli on the single container
// would make the pod Guaranteed: the CPU request equal to the new limit and the
// memory request equal to the memory limit. The QoS class may not change on a
// resize, so such a limit is refused rather than sent.
func wouldBeGuaranteed(p corev1.Pod, milli int64) bool {
	r := p.Spec.Containers[0].Resources
	req, ok := r.Requests[corev1.ResourceCPU]
	if !ok || req.MilliValue() != milli {
		return false
	}
	memReq, okReq := r.Requests[corev1.ResourceMemory]
	memLim, okLim := r.Limits[corev1.ResourceMemory]
	return okReq && okLim && memReq.Cmp(memLim) == 0
}

// hasSidecar reports a native sidecar: an init container that keeps running.
func hasSidecar(p corev1.Pod) bool {
	for _, c := range p.Spec.InitContainers {
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			return true
		}
	}
	return false
}

// Apply caps the pod's CPU limit and records the undo on the pod. The record is
// written first: a record without a cap reverts as a no-op, while a cap without a
// record would never be given back. If the resize then fails the record stays,
// because the resize may have been applied although the call reported failure. A
// pod this node already changed keeps the original from before its first change.
func (r *Runtime) Apply(ctx context.Context, subject string, c runtime.Change) (runtime.Undo, error) {
	if c.Action != runtime.ActionCPULimit {
		return runtime.Undo{}, fmt.Errorf("kubernetes cannot apply action %q", c.Action)
	}
	p, err := r.find(ctx, subject)
	if err != nil {
		return runtime.Undo{}, err
	}
	if why := cannot(p, r.env.NodeID); why != "" {
		return runtime.Undo{}, fmt.Errorf("%s/%s cannot take a cpu.limit: %s", p.Namespace, p.Name, why)
	}
	original := cpuOf(p)
	if prev, present, readable := decode(p); present && readable && prev.NodeID == r.env.NodeID {
		original = prev.Original
	}
	milli := millis(c.CPULimit)
	if wouldBeGuaranteed(p, milli) {
		return runtime.Undo{}, fmt.Errorf("%s/%s cannot take a cpu.limit of %dm: equal to its request, with memory "+
			"request equal to limit, it would make the pod Guaranteed, a QoS class change", p.Namespace, p.Name, milli)
	}
	u := runtime.Undo{
		Runtime:    "kubernetes",
		NodeID:     r.env.NodeID,
		Subject:    subject,
		Action:     c.Action,
		Original:   original,
		Applied:    float64(milli) / 1000,
		Expires:    c.Expires,
		DecisionID: c.DecisionID,
	}
	b, err := json.Marshal(u)
	if err != nil {
		return runtime.Undo{}, err
	}
	if err := r.annotate(ctx, p, string(b)); err != nil {
		return runtime.Undo{}, err
	}
	if err := r.resize(ctx, p, milli); err != nil {
		return runtime.Undo{}, err
	}
	return u, nil
}

// Revert resizes the pod back to the limit it had and removes the record. A pod
// already at that limit, as after a resize that never took, is not resized: only
// the record is cleared, so a resize the API keeps refusing cannot keep a change
// in force that is not there. A limit
// of nil is an error: the runtime never accepts a pod without one, so an undo
// without one did not come from here. A pod whose record is another node's or unreadable is left
// untouched and the revert is an error; with no record at all the undo in hand is
// authoritative and is restored. A pod
// that has finished is gone.
func (r *Runtime) Revert(ctx context.Context, u runtime.Undo) error {
	if u.Original.Limit == nil {
		return fmt.Errorf("revert %s: the original has no CPU limit, which this runtime never records", u.Subject)
	}
	p, err := r.find(ctx, u.Subject)
	if err != nil {
		return err
	}
	if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
		return fmt.Errorf("%s/%s has finished: %w", p.Namespace, p.Name, runtime.ErrGone)
	}
	rec, present, readable := decode(p)
	if present && (!readable || rec.NodeID != r.env.NodeID) {
		return fmt.Errorf("revert %s: the pod carries a %s record that is not node %s's; nothing restored", u.Subject, Annotation, r.env.NodeID)
	}
	if want := millis(*u.Original.Limit); !atLimit(p, want) {
		if err := r.resize(ctx, p, want); err != nil {
			return err
		}
	}
	if !present {
		return nil // no record to clear: the undo in hand is authoritative
	}
	return r.annotate(ctx, p, "")
}

// Changed lists the changes nodeID recorded on this node's pods, soonest-expiring
// first, then by subject.
func (r *Runtime) Changed(ctx context.Context, nodeID string) ([]runtime.Undo, error) {
	ps, err := r.pods(ctx)
	if err != nil {
		return nil, err
	}
	var out []runtime.Undo
	for _, p := range ps {
		// A record that names another pod, copied here by a template or by hand, is not this pod's change.
		if u, present, readable := decode(p); present && readable && u.NodeID == nodeID && u.Subject == subjectOf(p) {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Expires.Equal(out[j].Expires) {
			return out[i].Expires.Before(out[j].Expires)
		}
		return out[i].Subject < out[j].Subject
	})
	return out, nil
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

// decode is the undo on the pod's annotation: whether the annotation is there,
// and whether it could be read.
func decode(p corev1.Pod) (u runtime.Undo, present, readable bool) {
	s, present := p.Annotations[Annotation]
	if !present {
		return runtime.Undo{}, false, false
	}
	if err := json.Unmarshal([]byte(s), &u); err != nil {
		return runtime.Undo{}, true, false
	}
	return u, true, true
}

// millis is a limit in millicores, rounded down and never below 1m.
func millis(limit float64) int64 {
	m := int64(math.Floor(limit*1000 + 1e-6))
	if m < 1 {
		m = 1
	}
	return m
}

// resize sets the single container's CPU limit, in millicores, through the resize
// subresource.
func (r *Runtime) resize(ctx context.Context, p corev1.Pod, milli int64) error {
	if len(p.Spec.Containers) != 1 {
		return fmt.Errorf("%s/%s has %d containers; the limit cannot be split", p.Namespace, p.Name, len(p.Spec.Containers))
	}
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"containers": []any{
		map[string]any{"name": p.Spec.Containers[0].Name, "resources": map[string]any{"limits": map[string]string{"cpu": fmt.Sprintf("%dm", milli)}}},
	}}})
	if err != nil {
		return err
	}
	_, err = r.client.CoreV1().Pods(p.Namespace).Patch(ctx, p.Name, types.StrategicMergePatchType, patch, metav1.PatchOptions{}, "resize")
	return r.wrap(err, p, "resize")
}

// annotate sets the change annotation, or removes it when value is empty.
func (r *Runtime) annotate(ctx context.Context, p corev1.Pod, value string) error {
	var v any
	if value != "" {
		v = value
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{Annotation: v}}})
	if err != nil {
		return err
	}
	_, err = r.client.CoreV1().Pods(p.Namespace).Patch(ctx, p.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	return r.wrap(err, p, "annotate")
}

func (r *Runtime) wrap(err error, p corev1.Pod, what string) error {
	if err == nil {
		return nil
	}
	if isThisPodNotFound(err, p) {
		return fmt.Errorf("%s/%s: %w", p.Namespace, p.Name, runtime.ErrGone)
	}
	return fmt.Errorf("%s %s/%s: %w", what, p.Namespace, p.Name, err)
}

// isThisPodNotFound is true for a NotFound about this pod. A 404 about anything
// else, such as a cluster without the resize subresource, is not "gone".
func isThisPodNotFound(err error, p corev1.Pod) bool {
	if !apierrors.IsNotFound(err) {
		return false
	}
	var st apierrors.APIStatus
	if !errors.As(err, &st) {
		return false
	}
	d := st.Status().Details
	return d != nil && d.Name == p.Name && (d.Kind == "pods" || d.Kind == "pod")
}
