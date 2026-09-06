package minimal

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

// sanitiseIdentity converts a name to a valid subject identity by replacing every byte
// outside [A-Za-z0-9._-] with '_'. The subject charset is [A-Za-z0-9._:-] after the colon,
// and types.ValidSubject enforces it at the wire; the label keeps the true name so tracing
// back to the original cgroup is always possible. A name that needed a replacement
// gets a short hash of the original appended, so the mapping stays injective:
// without it "a@b.service" and "a_b.service" would share one subject, one EMA and
// one CPU snapshot, with the unit label flipping between them every tick.
func sanitiseIdentity(name string) string {
	b := []byte(name)
	var changed bool
	for i, ch := range b {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') ||
			ch == '.' || ch == '_' || ch == '-') {
			b[i] = '_'
			changed = true
		}
	}
	if !changed {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return string(b) + "-" + hex.EncodeToString(sum[:3])
}

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

// ValidUnitGlobs reports the first -cgroup-units pattern path.Match cannot parse. A
// bad pattern used to match nothing on every call and say nothing about it.
func ValidUnitGlobs(globs []string) error {
	for _, g := range globs {
		if _, err := path.Match(g, ""); err != nil {
			return fmt.Errorf("-cgroup-units pattern %q: %v", g, err)
		}
	}
	return nil
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
					subject: "unit:" + sanitiseIdentity(name),
					labels:  map[string]string{"kind": "unit", "unit": name, "cgroup": relPath},
				}, true
			}
		}
		return subjectInfo{}, false
	}
}
