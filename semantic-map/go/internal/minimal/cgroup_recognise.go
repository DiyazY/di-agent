package minimal

import (
	"path"
	"regexp"
	"strings"
)

// subjectInfo is what a recogniser knows about a cgroup directory that is a subject.
type subjectInfo struct {
	subject string
	labels  map[string]string
}

// recogniser maps a cgroup path (relative to the root, '/'-separated) to a subject.
// The map never sees these shapes; they are this collector's business.
type recogniser func(relPath string) (subjectInfo, bool)

var podUIDRe = regexp.MustCompile(`^(?:kubepods-(?:besteffort|burstable)-|kubepods-)?pod([0-9a-fA-F]{8})[-_]([0-9a-fA-F]{4})[-_]([0-9a-fA-F]{4})[-_]([0-9a-fA-F]{4})[-_]([0-9a-fA-F]{12})(\.slice)?$`)

// recogniseKubepods recognises a pod-level cgroup under the kubelet's tree, for both
// the systemd driver (…/kubepods-<qos>-pod<uid_with_underscores>.slice) and the
// cgroupfs driver (kubepods/<qos>/pod<uid>). Container scopes below a pod are not
// subjects: the pod is the unit of workload and its cgroup survives container
// restarts.
func recogniseKubepods(relPath string) (subjectInfo, bool) {
	segs := strings.Split(relPath, "/")
	if len(segs) < 2 || !strings.HasPrefix(segs[0], "kubepods") {
		return subjectInfo{}, false
	}
	last := segs[len(segs)-1]
	m := podUIDRe.FindStringSubmatch(last)
	if m == nil {
		return subjectInfo{}, false
	}
	uid := strings.ToLower(strings.Join(m[1:6], "-"))
	qos := "guaranteed"
	switch {
	case strings.Contains(relPath, "besteffort"):
		qos = "besteffort"
	case strings.Contains(relPath, "burstable"):
		qos = "burstable"
	}
	driver := "cgroupfs"
	if strings.HasSuffix(last, ".slice") {
		driver = "systemd"
	}
	return subjectInfo{
		subject: "pod:" + uid,
		labels:  map[string]string{"kind": "pod", "pod_uid": uid, "qos": qos, "driver": driver, "cgroup": relPath},
	}, true
}

// recogniseUnits recognises direct children of system.slice whose name matches one
// of the globs — an explicit allowlist, because a host has dozens of units and few
// are workloads. An empty allowlist recognises nothing.
func recogniseUnits(globs []string) recogniser {
	return func(relPath string) (subjectInfo, bool) {
		segs := strings.Split(relPath, "/")
		if len(segs) != 2 || segs[0] != "system.slice" {
			return subjectInfo{}, false
		}
		name := segs[1]
		for _, g := range globs {
			if ok, _ := path.Match(g, name); ok {
				return subjectInfo{
					subject: "unit:" + name,
					labels:  map[string]string{"kind": "unit", "unit": name, "cgroup": relPath},
				}, true
			}
		}
		return subjectInfo{}, false
	}
}
