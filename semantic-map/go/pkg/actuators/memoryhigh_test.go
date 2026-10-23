package actuators

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/runtime"
)

type nameRT struct{ name string }

func (r nameRT) Name() string                                        { return r.name }
func (nameRT) Workloads(context.Context) ([]runtime.Workload, error) { return nil, nil }

const mhUID = "1234abcd-0000-1111-2222-333344445555"

// mhRoot is a cgroup v2 root with the controllers file.
func mhRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpu memory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func mhPodDirs(root, qos, uid string, systemd bool) string {
	if systemd {
		u := strings.ReplaceAll(uid, "-", "_")
		if qos == "guaranteed" {
			return filepath.Join(root, "kubepods.slice", "kubepods-pod"+u+".slice")
		}
		return filepath.Join(root, "kubepods.slice", "kubepods-"+qos+".slice", "kubepods-"+qos+"-pod"+u+".slice")
	}
	if qos == "guaranteed" {
		return filepath.Join(root, "kubepods", "pod"+uid)
	}
	return filepath.Join(root, "kubepods", qos, "pod"+uid)
}

func mhMkPod(t *testing.T, root, qos, uid string, systemd bool, high, current string) string {
	t.Helper()
	d := mhPodDirs(root, qos, uid, systemd)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	mhWrite(t, filepath.Join(d, "memory.high"), high)
	if current != "" {
		mhWrite(t, filepath.Join(d, "memory.current"), current)
	}
	return d
}

func mhWrite(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mhRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func mhNew(t *testing.T, root string, held ...string) Actuator {
	t.Helper()
	a, err := NewMemoryHigh(Env{CgroupRoot: root, Held: func(string) []Entry {
		var es []Entry
		for _, s := range held {
			es = append(es, Entry{Action: "memory.high", Subject: s})
		}
		return es
	}})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func mhWL(qos string) runtime.Workload {
	return runtime.Workload{Subject: "pod:" + mhUID, UID: mhUID, QoS: qos,
		Resources: map[string]runtime.Resource{"memory": {Request: fp(100 << 20)}}}
}

func TestMemoryHighPathsBothDrivers(t *testing.T) {
	for _, systemd := range []bool{false, true} {
		for _, qos := range []string{"guaranteed", "burstable", "besteffort"} {
			root := mhRoot(t)
			d := mhMkPod(t, root, qos, mhUID, systemd, "max", "")
			a := mhNew(t, root)
			if err := a.Set(context.Background(), mhWL(qos), 256<<20, true); err != nil {
				t.Fatalf("systemd=%v qos=%s: %v", systemd, qos, err)
			}
			if got := mhRead(t, filepath.Join(d, "memory.high")); got != "268435456" {
				t.Errorf("systemd=%v qos=%s: wrote %q", systemd, qos, got)
			}
		}
	}
}

func TestMemoryHighReadSetUnset(t *testing.T) {
	root := mhRoot(t)
	d := mhMkPod(t, root, "burstable", mhUID, false, "max", "")
	a := mhNew(t, root)
	ctx := context.Background()
	w := mhWL("burstable")
	if v, set, err := a.Read(ctx, w); err != nil || set || v != 0 {
		t.Fatalf("max read = %v %v %v", v, set, err)
	}
	if err := a.Set(ctx, w, 123456789, true); err != nil {
		t.Fatal(err)
	}
	if got := mhRead(t, filepath.Join(d, "memory.high")); got != "123456789" {
		t.Fatalf("wrote %q", got)
	}
	if v, set, err := a.Read(ctx, w); err != nil || !set || v != 123456789 {
		t.Fatalf("read = %v %v %v", v, set, err)
	}
	if err := a.Set(ctx, w, 0, false); err != nil {
		t.Fatal(err)
	}
	if got := mhRead(t, filepath.Join(d, "memory.high")); got != "max" {
		t.Fatalf("unset wrote %q", got)
	}
	if v, set, err := a.Read(ctx, w); err != nil || set || v != 0 {
		t.Fatalf("read after unset = %v %v %v", v, set, err)
	}
}

func TestMemoryHighGone(t *testing.T) {
	root := mhRoot(t)
	a := mhNew(t, root)
	ctx := context.Background()
	w := mhWL("burstable")
	if _, _, err := a.Read(ctx, w); !errors.Is(err, ErrGone) {
		t.Errorf("Read err = %v", err)
	}
	if err := a.Set(ctx, w, 1<<30, true); !errors.Is(err, ErrGone) {
		t.Errorf("Set err = %v", err)
	}
	// Nothing was created by the failed Set.
	if _, err := os.Stat(filepath.Join(root, "kubepods")); err == nil {
		t.Error("Set created cgroup directories")
	}
}

func TestMemoryHighSetDoesNotCreateFile(t *testing.T) {
	root := mhRoot(t)
	d := mhMkPod(t, root, "burstable", mhUID, false, "max", "")
	os.Remove(filepath.Join(d, "memory.high"))
	a := mhNew(t, root)
	if err := a.Set(context.Background(), mhWL("burstable"), 1<<30, true); err == nil {
		t.Fatal("Set created memory.high")
	}
	if _, err := os.Stat(filepath.Join(d, "memory.high")); err == nil {
		t.Fatal("memory.high was created")
	}
}

func TestMemoryHighCheck(t *testing.T) {
	root := mhRoot(t)
	mhMkPod(t, root, "burstable", mhUID, false, "max", "52428800") // 50 MiB, request 100 MiB
	a := mhNew(t, root)

	self := mhWL("burstable")
	self.Self = true
	if got := a.Check(self); got != "the agent itself" {
		t.Errorf("self: %q", got)
	}
	if got := a.Check(mhWL("besteffort")); got != "besteffort: no memory promised" {
		t.Errorf("besteffort: %q", got)
	}
	noReq := mhWL("burstable")
	noReq.Resources = nil
	if got := a.Check(noReq); got != "no memory request" {
		t.Errorf("no request: %q", got)
	}
	other := mhWL("burstable")
	other.UID = "ffff0000-0000-0000-0000-000000000000"
	if got := a.Check(other); got != "pod cgroup not found" {
		t.Errorf("missing cgroup: %q", got)
	}
	if got := a.Check(mhWL("burstable")); got != "within its memory request: protected, not a culprit" {
		t.Errorf("within request: %q", got)
	}
	mhWrite(t, filepath.Join(mhPodDirs(root, "burstable", mhUID, false), "memory.current"), "209715200")
	if got := a.Check(mhWL("burstable")); got != "" {
		t.Errorf("over request: %q", got)
	}
}

func TestMemoryHighPodRestartReadsMax(t *testing.T) {
	root := mhRoot(t)
	d := mhMkPod(t, root, "burstable", mhUID, false, "max", "")
	a := mhNew(t, root)
	w := mhWL("burstable")
	act := NewActing("n", MemoryLedger(), a)
	ctx := context.Background()
	if _, err := act.Apply(ctx, w, "memory.high", 256<<20, time.Now().Add(time.Hour), "d1"); err != nil {
		t.Fatal(err)
	}
	if held, dropped, err := act.Held(ctx, "memory.high", []runtime.Workload{w}); err != nil || len(held) != 1 || len(dropped) != 0 {
		t.Fatalf("before restart: held=%v dropped=%v err=%v", held, dropped, err)
	}
	if err := os.RemoveAll(d); err != nil {
		t.Fatal(err)
	}
	mhMkPod(t, root, "burstable", mhUID, false, "max", "")
	held, dropped, err := act.Held(ctx, "memory.high", []runtime.Workload{w})
	if err != nil || len(held) != 0 || len(dropped) != 1 {
		t.Fatalf("after restart: held=%v dropped=%v err=%v", held, dropped, err)
	}
}

func TestMemoryHighRefusesKubeletManaged(t *testing.T) {
	// Pod level, cgroupfs.
	root := mhRoot(t)
	d := mhMkPod(t, root, "burstable", mhUID, false, "999999999", "")
	_, err := NewMemoryHigh(Env{CgroupRoot: root})
	if err == nil || !strings.Contains(err.Error(), "memory.high is managed by the kubelet (MemoryQoS)") || !strings.Contains(err.Error(), d) {
		t.Fatalf("pod level: %v", err)
	}
	// Held pod's own value is fine.
	if _, err := NewMemoryHigh(Env{CgroupRoot: root, Held: func(string) []Entry {
		return []Entry{{Subject: "pod:" + mhUID}}
	}}); err != nil {
		t.Fatalf("held pod refused: %v", err)
	}
	// A different pod held does not excuse it.
	if _, err := NewMemoryHigh(Env{CgroupRoot: root, Held: func(string) []Entry {
		return []Entry{{Subject: "pod:other"}}
	}}); err == nil {
		t.Fatal("other pod's hold excused it")
	}
	// Container level, systemd.
	root = mhRoot(t)
	pd := mhMkPod(t, root, "guaranteed", mhUID, true, "max", "")
	c := filepath.Join(pd, "cri-containerd-abc.scope")
	if err := os.MkdirAll(c, 0o755); err != nil {
		t.Fatal(err)
	}
	mhWrite(t, filepath.Join(c, "memory.high"), "777777777")
	_, err = NewMemoryHigh(Env{CgroupRoot: root})
	if err == nil || !strings.Contains(err.Error(), "MemoryQoS") || !strings.Contains(err.Error(), c) {
		t.Fatalf("container level: %v", err)
	}
	// All max is fine.
	root = mhRoot(t)
	mhMkPod(t, root, "besteffort", mhUID, false, "max", "")
	if _, err := NewMemoryHigh(Env{CgroupRoot: root}); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryHighNeedsCgroupV2(t *testing.T) {
	root := t.TempDir()
	_, err := NewMemoryHigh(Env{CgroupRoot: root})
	if err == nil || !strings.Contains(err.Error(), root) {
		t.Fatalf("err = %v", err)
	}
}

func TestMemoryHighRegistered(t *testing.T) {
	// Other tests empty the registry; init's registration is memoryHighFactory.
	resetForTest()
	Register("memory.high", memoryHighFactory)
	for _, rt := range []runtime.Runtime{nil, nameRT{"dry-run"}} {
		a, err := Build("memory.high", Env{Runtime: rt})
		if err != nil {
			t.Fatal(err)
		}
		if a.Describe().Action != "memory.high" || a.Describe().Min != 67108864 {
			t.Errorf("descriptor %+v", a.Describe())
		}
		self := mhWL("besteffort")
		self.Self = true
		if a.Check(self) != "the agent itself" || a.Check(mhWL("besteffort")) != "" {
			t.Error("static check")
		}
		if v, set, _ := a.Read(context.Background(), mhWL("burstable")); set || v != 0 {
			t.Error("static initial")
		}
	}
	root := mhRoot(t)
	if a, err := Build("memory.high", Env{Runtime: nameRT{"kubernetes"}, CgroupRoot: root}); err != nil || a == nil {
		t.Fatalf("real: %v", err)
	}
	if _, err := Build("memory.high", Env{Runtime: nameRT{"kubernetes"}, CgroupRoot: t.TempDir()}); err == nil {
		t.Fatal("real build on non-v2 root succeeded")
	}
}
