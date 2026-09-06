package minimal

import (
	"fmt"
	"strings"
	"testing"

	"github.com/DiyazY/di-agent/pkg/types"
)

func TestRecogniseKubepodsShapes(t *testing.T) {
	uid := "8f3c1234-aaaa-bbbb-cccc-1234567890ab"
	cases := []struct {
		path        string
		wantSubject string
		wantQoS     string
		wantDriver  string
		ok          bool
	}{
		{"kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod8f3c1234_aaaa_bbbb_cccc_1234567890ab.slice", "pod:" + uid, "burstable", "systemd", true},
		{"kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod8f3c1234_aaaa_bbbb_cccc_1234567890ab.slice", "pod:" + uid, "besteffort", "systemd", true},
		{"kubepods.slice/kubepods-pod8f3c1234_aaaa_bbbb_cccc_1234567890ab.slice", "pod:" + uid, "guaranteed", "systemd", true},
		{"kubepods/burstable/pod" + uid, "pod:" + uid, "burstable", "cgroupfs", true},
		{"kubepods/pod" + uid, "pod:" + uid, "guaranteed", "cgroupfs", true},
		// container scopes under a pod are not subjects
		{"kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod8f3c1234_aaaa_bbbb_cccc_1234567890ab.slice/cri-containerd-abc.scope", "", "", "", false},
		{"kubepods/burstable/pod" + uid + "/0123456789abcdef", "", "", "", false},
		// the QoS slice itself and unrelated trees are not subjects
		{"kubepods.slice/kubepods-burstable.slice", "", "", "", false},
		{"system.slice/ssh.service", "", "", "", false},
	}
	for _, c := range cases {
		info, ok := recogniseKubepods(c.path)
		if ok != c.ok {
			t.Errorf("%s: ok=%v want %v", c.path, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if info.subject != c.wantSubject || info.labels["qos"] != c.wantQoS || info.labels["driver"] != c.wantDriver ||
			info.labels["kind"] != "pod" || info.labels["pod_uid"] != uid || info.labels["cgroup"] != c.path {
			t.Errorf("%s: %+v", c.path, info)
		}
	}
}

func TestRecogniseUnitsHonoursGlobs(t *testing.T) {
	r := recogniseUnits([]string{"k0s*.service", "containerd.service"})
	if info, ok := r("system.slice/k0sworker.service"); !ok || info.subject != "unit:k0sworker.service" || info.labels["kind"] != "unit" {
		t.Errorf("k0sworker: ok=%v info=%+v", ok, info)
	}
	if _, ok := r("system.slice/ssh.service"); ok {
		t.Error("ssh.service is not in the allowlist")
	}
	if _, ok := r("system.slice/k0sworker.service/child"); ok {
		t.Error("only direct children of system.slice are units")
	}
	if _, ok := recogniseUnits(nil)("system.slice/k0sworker.service"); ok {
		t.Error("an empty allowlist recognises nothing")
	}

	// Systemd instantiated units containing @ must have the subject sanitised
	r2 := recogniseUnits([]string{"getty@*"})
	info, ok := r2("system.slice/getty@tty1.service")
	if !ok {
		t.Errorf("getty@tty1.service: ok=false, expected true")
	} else if !strings.HasPrefix(info.subject, "unit:getty_tty1.service-") || !types.ValidSubject(info.subject) {
		t.Errorf("getty@tty1.service: subject=%q want unit:getty_tty1.service-<hash>, valid on the wire", info.subject)
	} else if info.labels["unit"] != "getty@tty1.service" {
		t.Errorf("getty@tty1.service: labels[unit]=%q want getty@tty1.service", info.labels["unit"])
	}
}

func TestRecognisedSubjectsAreValidOnTheWire(t *testing.T) {
	// Test kubepods subjects
	uid := "8f3c1234-aaaa-bbbb-cccc-1234567890ab"
	kubepodsTestCases := []string{
		"kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod8f3c1234_aaaa_bbbb_cccc_1234567890ab.slice",
		"kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod8f3c1234_aaaa_bbbb_cccc_1234567890ab.slice",
		"kubepods.slice/kubepods-pod8f3c1234_aaaa_bbbb_cccc_1234567890ab.slice",
		"kubepods/burstable/pod" + uid,
		"kubepods/pod" + uid,
	}

	for _, path := range kubepodsTestCases {
		info, ok := recogniseKubepods(path)
		if !ok {
			t.Errorf("recogniseKubepods(%q) returned ok=false", path)
			continue
		}
		if !types.ValidSubject(info.subject) {
			t.Errorf("recogniseKubepods(%q): subject %q is not valid on the wire", path, info.subject)
		}
	}

	// Test unit subjects with various special characters
	unitTestCases := []string{
		"k0sworker.service",
		"containerd.service",
		"getty@tty1.service",
		"serial-getty@ttyS0.service",
		"foo@instance.service",
	}
	r := recogniseUnits([]string{"*"}) // allowlist everything for this test

	for _, unitName := range unitTestCases {
		path := "system.slice/" + unitName
		info, ok := r(path)
		if !ok {
			t.Errorf("recogniseUnits(*) did not recognise %q", path)
			continue
		}
		if !types.ValidSubject(info.subject) {
			t.Errorf("recogniseUnits(*) for %q: subject %q is not valid on the wire", path, info.subject)
		}
	}
}

// TestSanitisedUnitNamesStayDistinct: sanitising to the subject charset must be
// injective, or two units whose names differ only in a replaced byte share one
// property, one EMA and one CPU snapshot, and the `unit` label flips between them
// every tick.
func TestSanitisedUnitNamesStayDistinct(t *testing.T) {
	r := recogniseUnits([]string{"*.service"})
	a, okA := r("system.slice/a@b.service")
	b, okB := r("system.slice/a_b.service")
	if !okA || !okB {
		t.Fatalf("both units must be recognised: %v %v", okA, okB)
	}
	if a.subject == b.subject {
		t.Errorf("a@b.service and a_b.service sanitised to the same subject %q", a.subject)
	}
	for _, info := range []subjectInfo{a, b} {
		if !types.ValidSubject(info.subject) {
			t.Errorf("subject %q is not valid on the wire", info.subject)
		}
	}
	if plain, _ := r("system.slice/plain.service"); plain.subject != "unit:plain.service" {
		t.Errorf("a name that needs no sanitising must be used as is; got %q", plain.subject)
	}
}

// TestBadUnitGlobIsDroppedAndLogged: path.Match reports a malformed pattern on every
// call, and the error was discarded, so `-cgroup-units='[k0s'` matched nothing
// silently. The validator refuses it and the constructor drops it with a line.
func TestBadUnitGlobIsDroppedAndLogged(t *testing.T) {
	if err := ValidUnitGlobs([]string{"k0s*.service", "[k0s"}); err == nil {
		t.Error("a malformed glob passed validation")
	}
	root, _, _, _, procRoot := fakeTree(t)
	var lines []string
	c := NewCgroupCollectorWithOptions("n1", root, CgroupOptions{Subjects: true, UnitGlobs: []string{"[k0s", "k0s*.service"},
		MaxSubjects: 8, MemTotalBytes: 4 << 30, ProcRoot: procRoot,
		Logf: func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }})
	samples, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if bySubject(samples)["unit:k0sworker.service"] == nil {
		t.Error("the well-formed glob beside the bad one stopped matching")
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "[k0s") {
		t.Errorf("bad glob logged %d times (%v); want one line naming it", len(lines), lines)
	}
}
