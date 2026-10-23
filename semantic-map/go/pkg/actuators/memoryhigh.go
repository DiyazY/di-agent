package actuators

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/DiyazY/di-agent/pkg/runtime"
)

// MemoryHighDescriptor describes the memory.high action, for the real actuator
// and for the dry-run stand-in alike. memory.high is a soft cap: past it the
// kernel throttles the cgroup and reclaims from it, and never kills it, so
// lowering it trades the pod's speed for the node's memory, not its life. The
// floor is 64 MiB, below which a pod would spend its time reclaiming. Usage is a
// share of node capacity in [0,1], as for each pod's memory_utilization@<subject>;
// memory_total turns a share into bytes.
var MemoryHighDescriptor = Descriptor{
	Action:   "memory.high",
	Kinds:    []string{"pod"},
	Resource: "memory",
	Unit:     "bytes",
	Capacity: "memory_total",
	Min:      67108864,
	Relieves: "memory_contention_ratio",
	Usage:    "memory_utilization",
	Describe: "throttle a pod's memory above a soft cap, reclaimed by the kernel and never killed",
}

func init() { Register("memory.high", memoryHighFactory) }

func memoryHighFactory(env Env) (Actuator, error) {
	if env.Runtime == nil || env.Runtime.Name() == "dry-run" {
		return Static(MemoryHighDescriptor, staticCheck, unsetInitial), nil
	}
	return NewMemoryHigh(env)
}

// staticCheck is Check for the dry-run stand-in, which changes nothing: it
// refuses only the agent's own workload.
func staticCheck(w runtime.Workload) string {
	if w.Self {
		return "the agent itself"
	}
	return ""
}

// unsetInitial is the setting of a workload nothing has changed: none.
func unsetInitial(runtime.Workload) (float64, bool) { return 0, false }

// MemoryHigh is the memory.high actuator over the node's cgroup v2 tree. It
// keeps no record: the agent's ledger holds what to give back.
type MemoryHigh struct{ root string }

// NewMemoryHigh builds the actuator over env.CgroupRoot. It refuses a root that
// is not cgroup v2, and one where memory.high is already set by someone else:
// with the kubelet's MemoryQoS on, the kubelet owns the file, would overwrite
// what the agent sets, and the agent could not tell its own value from the
// kubelet's. A pod the agent holds (env.Held) may differ from max.
func NewMemoryHigh(env Env) (Actuator, error) {
	if _, err := os.Stat(filepath.Join(env.CgroupRoot, "cgroup.controllers")); err != nil {
		return nil, fmt.Errorf("memory.high needs cgroup v2: %s has no cgroup.controllers: %w", env.CgroupRoot, err)
	}
	a := &MemoryHigh{root: env.CgroupRoot}
	held := map[string]bool{}
	if env.Held != nil {
		for _, e := range env.Held("memory.high") {
			held[e.Subject] = true
		}
	}
	if err := a.refuseManaged(held); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *MemoryHigh) Describe() Descriptor { return MemoryHighDescriptor }

// Check is why the pod's memory.high cannot usefully be lowered, or "" when it
// can: a pod within its memory request is protected, so it is never the culprit.
func (a *MemoryHigh) Check(w runtime.Workload) string {
	if w.Self {
		return "the agent itself"
	}
	if w.QoS == "besteffort" {
		return "besteffort: no memory promised"
	}
	req := w.Resources["memory"].Request
	if req == nil {
		return "no memory request"
	}
	dir, err := a.podDir(w)
	switch {
	case errors.Is(err, ErrInvalidIdentity):
		return "invalid pod identity"
	case err != nil:
		return "pod cgroup not found"
	}
	cur, err := readInt(filepath.Join(dir, "memory.current"))
	if err != nil {
		return "memory usage unreadable"
	}
	if float64(cur) <= *req {
		return "within its memory request: protected, not a culprit"
	}
	return ""
}

// Read is the pod's memory.high in bytes; set is false when it is max.
func (a *MemoryHigh) Read(_ context.Context, w runtime.Workload) (float64, bool, error) {
	dir, err := a.podDir(w)
	if err != nil {
		return 0, false, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "memory.high"))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, fmt.Errorf("%s: %w", w.Subject, ErrGone)
	}
	if err != nil {
		return 0, false, err
	}
	return parseHigh(string(b))
}

// Set writes v bytes to the pod's memory.high, or max when set is false. The
// kernel rounds to a page; the ledger records what Read gives back.
func (a *MemoryHigh) Set(_ context.Context, w runtime.Workload, v float64, set bool) error {
	if set && (math.IsNaN(v) || math.IsInf(v, 0) || v < MemoryHighDescriptor.Min) {
		return fmt.Errorf("%s: memory.high of %v bytes refused: not a number, or below the %v byte floor", w.Subject, v, MemoryHighDescriptor.Min)
	}
	dir, err := a.podDir(w)
	if err != nil {
		return err
	}
	s := "max"
	if set {
		s = strconv.FormatInt(int64(v), 10)
	}
	// No O_CREATE: a cgroup that left must fail the write, not leave a file.
	f, err := os.OpenFile(filepath.Join(dir, "memory.high"), os.O_WRONLY|os.O_TRUNC, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: %w", w.Subject, ErrGone)
	}
	if err != nil {
		return err
	}
	if _, err := f.WriteString(s); err != nil {
		f.Close()
		return fmt.Errorf("%s: write memory.high: %w", w.Subject, err)
	}
	return f.Close()
}

// podUID is the canonical Kubernetes pod UID: a lower-case hex UUID. Anything
// else never goes into a path.
var podUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// podDirs lists where a pod's cgroup may be, cgroupfs layout first, then systemd.
func (a *MemoryHigh) podDirs(uid, qos string) []string {
	u := strings.ReplaceAll(uid, "-", "_")
	if qos == "guaranteed" {
		return []string{
			filepath.Join(a.root, "kubepods", "pod"+uid),
			filepath.Join(a.root, "kubepods.slice", "kubepods-pod"+u+".slice"),
		}
	}
	return []string{
		filepath.Join(a.root, "kubepods", qos, "pod"+uid),
		filepath.Join(a.root, "kubepods.slice", "kubepods-"+qos+".slice", "kubepods-"+qos+"-pod"+u+".slice"),
	}
}

// podDir is the pod's cgroup directory: ErrInvalidIdentity when w has no valid
// UID and QoS class to find it by, which is the caller's error and says nothing
// of the pod, and ErrGone when the identity is valid and neither layout has it.
func (a *MemoryHigh) podDir(w runtime.Workload) (string, error) {
	if !podUID.MatchString(w.UID) {
		return "", fmt.Errorf("%s has no valid pod uid %q: %w", w.Subject, w.UID, ErrInvalidIdentity)
	}
	switch w.QoS {
	case "guaranteed", "burstable", "besteffort":
	default:
		return "", fmt.Errorf("%s has no valid qos %q: %w", w.Subject, w.QoS, ErrInvalidIdentity)
	}
	for _, d := range a.podDirs(w.UID, w.QoS) {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			return d, nil
		}
	}
	return "", fmt.Errorf("%s: no cgroup: %w", w.Subject, ErrGone)
}

// refuseManaged errors on the first pod or container cgroup whose memory.high is
// not max and whose pod the agent does not hold.
func (a *MemoryHigh) refuseManaged(held map[string]bool) error {
	groups := []struct{ parent, podPrefix, suffix string }{
		{filepath.Join(a.root, "kubepods"), "pod", ""},
		{filepath.Join(a.root, "kubepods", "burstable"), "pod", ""},
		{filepath.Join(a.root, "kubepods", "besteffort"), "pod", ""},
		{filepath.Join(a.root, "kubepods.slice"), "kubepods-pod", ".slice"},
		{filepath.Join(a.root, "kubepods.slice", "kubepods-burstable.slice"), "kubepods-burstable-pod", ".slice"},
		{filepath.Join(a.root, "kubepods.slice", "kubepods-besteffort.slice"), "kubepods-besteffort-pod", ".slice"},
	}
	for _, g := range groups {
		ents, err := os.ReadDir(g.parent)
		if err != nil {
			continue
		}
		for _, e := range ents {
			n := e.Name()
			if !e.IsDir() || !strings.HasPrefix(n, g.podPrefix) || !strings.HasSuffix(n, g.suffix) {
				continue
			}
			uid := strings.TrimSuffix(strings.TrimPrefix(n, g.podPrefix), g.suffix)
			if g.suffix != "" {
				uid = strings.ReplaceAll(uid, "_", "-")
			}
			if held["pod:"+uid] {
				continue
			}
			pod := filepath.Join(g.parent, n)
			if err := managedBy(pod); err != nil {
				return err
			}
			kids, err := os.ReadDir(pod)
			if err != nil {
				continue
			}
			for _, k := range kids {
				if k.IsDir() {
					if err := managedBy(filepath.Join(pod, k.Name())); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// managedBy is an error when dir's memory.high is set to anything but max.
func managedBy(dir string) error {
	b, err := os.ReadFile(filepath.Join(dir, "memory.high"))
	if err != nil {
		return nil
	}
	if _, set, err := parseHigh(string(b)); err == nil && !set {
		return nil
	}
	return fmt.Errorf("memory.high is managed by the kubelet (MemoryQoS): %s is %q, not max; turn MemoryQoS off or drop the action",
		filepath.Join(dir, "memory.high"), strings.TrimSpace(string(b)))
}

// parseHigh reads a memory.high file: max is no setting, else bytes.
func parseHigh(s string) (float64, bool, error) {
	s = strings.TrimSpace(s)
	if s == "max" {
		return 0, false, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("memory.high %q: %w", s, err)
	}
	return float64(n), true, nil
}

func readInt(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}
