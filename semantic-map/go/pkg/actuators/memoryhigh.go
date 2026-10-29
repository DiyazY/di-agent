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
// kubelet's. A pod the agent holds (env.Held) may differ from max. It refuses a
// tree it could not write memory.high in, unless env.DryRun says it never will.
func NewMemoryHigh(env Env) (Actuator, error) {
	if _, err := os.Stat(filepath.Join(env.CgroupRoot, "cgroup.controllers")); err != nil {
		return nil, fmt.Errorf("memory.high needs cgroup v2: %s has no cgroup.controllers: %w", env.CgroupRoot, err)
	}
	a := &MemoryHigh{root: env.CgroupRoot}
	held := map[string]bool{}
	mine := 0
	if env.Held != nil {
		for _, e := range env.Held("memory.high") {
			held[e.Subject] = true
			if e.NodeID == env.NodeID {
				mine++
			}
		}
	}
	if mine > 0 && !a.hasKubepods() {
		return nil, fmt.Errorf("memory.high: the ledger holds %d change(s) of this node, and %s has no kubepods tree (kubepods or kubepods.slice): "+
			"the cgroup root is wrong, and no change could be found to give back; mount the node's cgroup tree there", mine, a.root)
	}
	if err := a.refuseManaged(held); err != nil {
		return nil, err
	}
	if !env.DryRun { // a dry run never writes, so a read-only tree is no reason to refuse it
		if err := a.probeWritable(); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// probeWritable refuses a tree the agent could never write memory.high in, as
// the DaemonSet's read-only /host: better a refused start naming the lever than
// every step refused one by one. With a pod cgroup present its memory.high is
// opened for writing and closed, nothing written; with none that has the file,
// the kubepods directory (else the root) must be writable.
func (a *MemoryHigh) probeWritable() error {
	readOnly := func(path string, err error) error {
		return fmt.Errorf("memory.high cannot be written under %s: read-only (%s: %w); mount the cgroup tree writable or drop the action", a.root, path, err)
	}
	for _, p := range a.pods() {
		f := filepath.Join(p.dir, "memory.high")
		h, err := os.OpenFile(f, os.O_WRONLY, 0) // no O_TRUNC, no O_CREATE: nothing changes
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue // a pod cgroup on its way out
		case err != nil:
			return readOnly(f, err)
		}
		return h.Close()
	}
	dir := a.root
	for _, d := range a.kubepods() {
		if isDir(d) {
			dir = d
			break
		}
	}
	if err := writable(dir); err != nil {
		return readOnly(dir, err)
	}
	return nil
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
	case errors.Is(err, ErrGone):
		return "pod cgroup not found"
	case err != nil:
		return "pod cgroup unreadable"
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
	if err != nil {
		return 0, false, missingFile(w, dir, err)
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
	if err != nil {
		return missingFile(w, dir, err)
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

// podDir is the pod's cgroup directory. The pod is found by w's UID and QoS
// class, as the runtime lists them; a workload named only by its subject, as the
// ledger records a change to a pod the runtime no longer lists, is found by the
// UID in a pod:<uid> subject in every QoS class. ErrInvalidIdentity when there is
// no valid UID or QoS class to find it by, which is the caller's error and says
// nothing of the pod. ErrGone only when no layout has the pod's directory and a
// QoS parent it would be in is there: a missing parent says the root is wrong,
// not that the pod left, and any error but "does not exist" (EACCES, EIO) is
// returned as it is. Unknown is never gone.
func (a *MemoryHigh) podDir(w runtime.Workload) (string, error) {
	uid := w.UID
	if uid == "" {
		if u, ok := strings.CutPrefix(w.Subject, "pod:"); ok {
			uid = u
		}
	}
	if !podUID.MatchString(uid) {
		return "", fmt.Errorf("%s has no valid pod uid %q: %w", w.Subject, uid, ErrInvalidIdentity)
	}
	qoses := []string{w.QoS}
	switch w.QoS {
	case "guaranteed", "burstable", "besteffort":
	case "":
		qoses = []string{"guaranteed", "burstable", "besteffort"}
	default:
		return "", fmt.Errorf("%s has no valid qos %q: %w", w.Subject, w.QoS, ErrInvalidIdentity)
	}
	var dirs []string
	for _, q := range qoses {
		dirs = append(dirs, a.podDirs(uid, q)...)
	}
	parent := false
	for _, d := range dirs {
		st, err := os.Stat(d)
		switch {
		case err == nil && st.IsDir():
			return d, nil
		case err == nil:
			return "", fmt.Errorf("%s: %s is not a directory", w.Subject, d)
		case !errors.Is(err, fs.ErrNotExist):
			return "", fmt.Errorf("%s: %w", w.Subject, err)
		}
		switch st, err := os.Stat(filepath.Dir(d)); {
		case err == nil && st.IsDir():
			parent = true
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return "", fmt.Errorf("%s: %w", w.Subject, err)
		}
	}
	if !parent {
		return "", fmt.Errorf("%s: no cgroup, and no QoS parent for it under %s: the cgroup root may be wrong", w.Subject, a.root)
	}
	return "", fmt.Errorf("%s: no cgroup: %w", w.Subject, ErrGone)
}

// missingFile is the error for a pod cgroup's memory.high that could not be
// opened: ErrGone when the directory itself has gone since it was found, as a
// pod removed in between; otherwise the error as it is, since a directory
// without the file is one whose memory controller is not enabled, not a pod
// that left.
func missingFile(w runtime.Workload, dir string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		if _, serr := os.Stat(dir); errors.Is(serr, fs.ErrNotExist) {
			return fmt.Errorf("%s: %w", w.Subject, ErrGone)
		}
		return fmt.Errorf("%s: %s has no memory.high (is the memory controller enabled for it?): %w", w.Subject, dir, err)
	}
	return fmt.Errorf("%s: memory.high: %w", w.Subject, err)
}

// kubepods is the top of the pod tree in each driver's layout.
func (a *MemoryHigh) kubepods() []string {
	return []string{filepath.Join(a.root, "kubepods"), filepath.Join(a.root, "kubepods.slice")}
}

// hasKubepods reports whether either layout's pod tree is under the root.
func (a *MemoryHigh) hasKubepods() bool {
	for _, d := range a.kubepods() {
		if isDir(d) {
			return true
		}
	}
	return false
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// podCgroup is one pod's cgroup directory, with the pod's UID.
type podCgroup struct{ dir, uid string }

// pods lists the pod cgroups under the root, in both drivers' layouts.
func (a *MemoryHigh) pods() []podCgroup {
	var out []podCgroup
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
			out = append(out, podCgroup{dir: filepath.Join(g.parent, n), uid: uid})
		}
	}
	return out
}

// refuseManaged errors on the first pod or container cgroup whose memory.high is
// not max and whose pod the agent does not hold.
func (a *MemoryHigh) refuseManaged(held map[string]bool) error {
	for _, p := range a.pods() {
		if held["pod:"+p.uid] {
			continue
		}
		if err := managedBy(p.dir); err != nil {
			return err
		}
		kids, err := os.ReadDir(p.dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue // the pod went mid-scan
		}
		if err != nil {
			return fmt.Errorf("memory.high: cannot tell whether the kubelet manages it: %s could not be read: %w", p.dir, err)
		}
		for _, k := range kids {
			if k.IsDir() {
				if err := managedBy(filepath.Join(p.dir, k.Name())); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// managedBy is an error when dir's memory.high is set to anything but max, or
// cannot be read: a permission or I/O error is no proof the kubelet leaves it
// alone. One that does not exist, as a cgroup removed mid-scan, is not managed.
func managedBy(dir string) error {
	f := filepath.Join(dir, "memory.high")
	b, err := os.ReadFile(f)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("memory.high: cannot tell whether the kubelet manages it: %s could not be read: %w", f, err)
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
