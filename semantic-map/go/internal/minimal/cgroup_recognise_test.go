package minimal

import "testing"

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
}
