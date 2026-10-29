package actuators

import (
	"context"
	"errors"
	"math"
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
	// The pod's QoS parent is there and the pod is not: it is gone.
	if err := os.MkdirAll(filepath.Join(root, "kubepods", "burstable"), 0o755); err != nil {
		t.Fatal(err)
	}
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
	if ents, _ := os.ReadDir(filepath.Join(root, "kubepods", "burstable")); len(ents) != 0 {
		t.Error("Set created cgroup directories")
	}
}

// TestMemoryHighIsNotGoneWithoutItsQoSParent: a pod is gone only when its cgroup
// is missing from a QoS parent that is there. A tree with no kubepods, or with
// no parent for the pod's QoS class, says the root is wrong, not that the pod
// left: Read and Set fail, and nothing may forget a change on their say-so.
func TestMemoryHighIsNotGoneWithoutItsQoSParent(t *testing.T) {
	ctx := context.Background()
	for name, mk := range map[string]string{"no kubepods": "", "no QoS parent": "kubepods"} {
		root := mhRoot(t)
		if mk != "" {
			if err := os.MkdirAll(filepath.Join(root, mk), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		a := mhNew(t, root)
		w := mhWL("burstable")
		if _, _, err := a.Read(ctx, w); err == nil || errors.Is(err, ErrGone) {
			t.Errorf("%s: Read err = %v; want an error that is not ErrGone", name, err)
		}
		if err := a.Set(ctx, w, 1<<30, true); err == nil || errors.Is(err, ErrGone) {
			t.Errorf("%s: Set err = %v; want an error that is not ErrGone", name, err)
		}
	}
}

// TestMemoryHighStatErrorIsNotGone: a pod cgroup that cannot be looked at (EACCES,
// EIO) is not one that left: the error is returned as it is, and a revert that
// meets it keeps the entry.
func TestMemoryHighStatErrorIsNotGone(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads through file modes")
	}
	root := mhRoot(t)
	d := mhMkPod(t, root, "burstable", mhUID, false, "max", "209715200")
	a := mhNew(t, root)
	l := MemoryLedger()
	acting := NewActing("n1", l, a)
	ctx := context.Background()
	e, err := acting.Apply(ctx, mhWL("burstable"), "memory.high", 128<<20, time.Now().Add(time.Hour), "d1")
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(d)
	if err := os.Chmod(parent, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(parent, 0o755) })
	if _, _, err := a.Read(ctx, mhWL("burstable")); err == nil || errors.Is(err, ErrGone) {
		t.Fatalf("Read err = %v; want the stat error, not ErrGone", err)
	}
	if err := acting.Revert(ctx, mhWL("burstable"), e); err == nil || errors.Is(err, ErrGone) {
		t.Fatalf("Revert err = %v; want the stat error, not ErrGone", err)
	}
	if len(l.All()) != 1 {
		t.Fatalf("ledger %+v; want the entry kept", l.All())
	}
}

// TestMemoryHighWithoutItsFileIsNotGone: a pod cgroup that is there but has no
// memory.high (the memory controller not enabled for it) is not a pod that left.
func TestMemoryHighWithoutItsFileIsNotGone(t *testing.T) {
	root := mhRoot(t)
	d := mhMkPod(t, root, "burstable", mhUID, false, "max", "")
	a := mhNew(t, root)
	os.Remove(filepath.Join(d, "memory.high"))
	ctx := context.Background()
	if _, _, err := a.Read(ctx, mhWL("burstable")); err == nil || errors.Is(err, ErrGone) {
		t.Errorf("Read err = %v; want an error that is not ErrGone", err)
	}
	if err := a.Set(ctx, mhWL("burstable"), 1<<30, true); err == nil || errors.Is(err, ErrGone) {
		t.Errorf("Set err = %v; want an error that is not ErrGone", err)
	}
}

// TestMemoryHighRefusesHeldEntriesWithoutKubepods: a ledger that holds this
// node's memory.high changes over a root with no kubepods tree at all is a wrong
// cgroup root, as after a restart with another mount: start is refused, naming
// the root, rather than every change taken for gone.
func TestMemoryHighRefusesHeldEntriesWithoutKubepods(t *testing.T) {
	root := mhRoot(t)
	held := func(string) []Entry {
		return []Entry{{Action: "memory.high", Subject: "pod:" + mhUID, NodeID: "n1", Applied: 1 << 30}}
	}
	_, err := NewMemoryHigh(Env{NodeID: "n1", CgroupRoot: root, Held: held})
	if err == nil || !strings.Contains(err.Error(), root) || !strings.Contains(err.Error(), "kubepods") {
		t.Fatalf("err %v; want a refusal naming %s and kubepods", err, root)
	}
	if err := os.MkdirAll(filepath.Join(root, "kubepods.slice"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewMemoryHigh(Env{NodeID: "n1", CgroupRoot: root, Held: held}); err != nil {
		t.Fatalf("with a kubepods tree: %v", err)
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
	d := mhMkPod(t, root, "burstable", mhUID, false, "max", "209715200")
	a := mhNew(t, root)
	w := mhWL("burstable")
	act := NewActing("n", MemoryLedger(), a)
	ctx := context.Background()
	if _, err := act.Apply(ctx, w, "memory.high", 256<<20, time.Now().Add(time.Hour), "d1"); err != nil {
		t.Fatal(err)
	}
	if held, dropped, _, err := held4(act, ctx, "memory.high", []runtime.Workload{w}); err != nil || len(held) != 1 || len(dropped) != 0 {
		t.Fatalf("before restart: held=%v dropped=%v err=%v", held, dropped, err)
	}
	if err := os.RemoveAll(d); err != nil {
		t.Fatal(err)
	}
	mhMkPod(t, root, "burstable", mhUID, false, "max", "209715200")
	held, dropped, _, err := held4(act, ctx, "memory.high", []runtime.Workload{w})
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

func TestMemoryHighRejectsUnsafeIdentity(t *testing.T) {
	root := mhRoot(t)
	mhMkPod(t, root, "burstable", mhUID, false, "max", "209715200")
	decoy := filepath.Join(root, "decoy")
	if err := os.MkdirAll(decoy, 0o755); err != nil {
		t.Fatal(err)
	}
	mhWrite(t, filepath.Join(decoy, "memory.high"), "max")
	mhWrite(t, filepath.Join(decoy, "memory.current"), "209715200")
	a := mhNew(t, root)
	ctx := context.Background()
	cases := []struct{ name, uid, qos string }{
		{"traversal", "../../decoy", "burstable"},
		{"traversal into pod parent", "../" + "../decoy", "besteffort"},
		{"slash", "a/b", "burstable"},
		{"empty uid, subject traversal", "", "burstable"},
		{"upper case uid", strings.ToUpper(mhUID), "burstable"},
		{"unknown qos", mhUID, "../decoy"},
	}
	for _, c := range cases {
		w := mhWL("burstable")
		w.UID, w.QoS = c.uid, c.qos
		if c.uid == "" {
			w.Subject = "pod:../../decoy" // the UID a subject-only workload is found by
		}
		// A workload without a valid identity is the caller's error, not a pod
		// that left: nothing may forget a change on its say-so.
		if _, _, err := a.Read(ctx, w); !errors.Is(err, ErrInvalidIdentity) || errors.Is(err, ErrGone) {
			t.Errorf("%s: Read err = %v", c.name, err)
		}
		if err := a.Set(ctx, w, 1<<30, true); !errors.Is(err, ErrInvalidIdentity) || errors.Is(err, ErrGone) {
			t.Errorf("%s: Set err = %v", c.name, err)
		}
		if c.qos != "besteffort" {
			if got := a.Check(w); got != "invalid pod identity" {
				t.Errorf("%s: Check = %q", c.name, got)
			}
		}
		if got := mhRead(t, filepath.Join(decoy, "memory.high")); got != "max" {
			t.Fatalf("%s: decoy changed to %q", c.name, got)
		}
	}
}

func TestMemoryHighCheckUnreadableUsage(t *testing.T) {
	root := mhRoot(t)
	d := mhMkPod(t, root, "burstable", mhUID, false, "max", "")
	a := mhNew(t, root)
	if got := a.Check(mhWL("burstable")); got != "memory usage unreadable" {
		t.Errorf("missing: %q", got)
	}
	mhWrite(t, filepath.Join(d, "memory.current"), "lots")
	if got := a.Check(mhWL("burstable")); got != "memory usage unreadable" {
		t.Errorf("unparsable: %q", got)
	}
}

func TestMemoryHighSetRefusesUnsafeValue(t *testing.T) {
	root := mhRoot(t)
	d := mhMkPod(t, root, "burstable", mhUID, false, "max", "")
	a := mhNew(t, root)
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), 0, -1, MemoryHighDescriptor.Min - 1} {
		if err := a.Set(context.Background(), mhWL("burstable"), v, true); err == nil {
			t.Errorf("Set(%v) succeeded", v)
		}
		if got := mhRead(t, filepath.Join(d, "memory.high")); got != "max" {
			t.Fatalf("Set(%v) wrote %q", v, got)
		}
	}
	if err := a.Set(context.Background(), mhWL("burstable"), MemoryHighDescriptor.Min, true); err != nil {
		t.Errorf("Set(Min): %v", err)
	}
}

// TestMemoryHighFindsAPodBySubject: a workload named only by its subject, as
// the ledger records it, is found by the UID in the subject in every QoS class
// and both drivers' layouts, so a change can be given back to a pod the runtime
// no longer lists.
func TestMemoryHighFindsAPodBySubject(t *testing.T) {
	ctx := context.Background()
	for _, systemd := range []bool{false, true} {
		for _, qos := range []string{"guaranteed", "burstable", "besteffort"} {
			root := mhRoot(t)
			d := mhMkPod(t, root, qos, mhUID, systemd, "max", "")
			a := mhNew(t, root)
			w := runtime.Workload{Subject: "pod:" + mhUID}
			if err := a.Set(ctx, w, 256<<20, true); err != nil {
				t.Fatalf("systemd=%v qos=%s: %v", systemd, qos, err)
			}
			if v, set, err := a.Read(ctx, w); err != nil || !set || v != 256<<20 {
				t.Fatalf("systemd=%v qos=%s: read %v %v %v", systemd, qos, v, set, err)
			}
			if got := mhRead(t, filepath.Join(d, "memory.high")); got != "268435456" {
				t.Errorf("systemd=%v qos=%s: wrote %q", systemd, qos, got)
			}
		}
	}
	// Gone only when no QoS parent holds it and at least one is there.
	root := mhRoot(t)
	if err := os.MkdirAll(filepath.Join(root, "kubepods", "burstable"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := mhNew(t, root)
	if _, _, err := a.Read(ctx, runtime.Workload{Subject: "pod:" + mhUID}); !errors.Is(err, ErrGone) {
		t.Fatalf("Read err %v; want ErrGone", err)
	}
	// A subject that is not a pod's is not a pod identity.
	if _, _, err := a.Read(ctx, runtime.Workload{Subject: "unit:x"}); !errors.Is(err, ErrInvalidIdentity) {
		t.Fatalf("Read err %v; want ErrInvalidIdentity", err)
	}
}

// TestMemoryHighRevertBySubjectGivesItBack: a revert through a workload named
// only by its subject finds the pod and writes max.
func TestMemoryHighRevertBySubjectGivesItBack(t *testing.T) {
	root := mhRoot(t)
	d := mhMkPod(t, root, "burstable", mhUID, false, "max", "209715200")
	a := mhNew(t, root)
	l := MemoryLedger()
	acting := NewActing("n1", l, a)
	ctx := context.Background()
	e, err := acting.Apply(ctx, mhWL("burstable"), "memory.high", 128<<20, time.Now().Add(time.Hour), "d1")
	if err != nil {
		t.Fatal(err)
	}
	if err := acting.Revert(ctx, runtime.Workload{Subject: e.Subject}, e); err != nil {
		t.Fatal(err)
	}
	if got := mhRead(t, filepath.Join(d, "memory.high")); got != "max" || len(l.All()) != 0 {
		t.Fatalf("after revert: memory.high %q, ledger %+v", got, l.All())
	}
}

// TestHeldAsksTheActuatorAboutAnUnlistedPod: the listing does not decide that a
// pod left. An entry whose pod is not listed is read through its subject: one
// whose cgroup still holds the cap is held, one whose cgroup has left is
// dropped. An empty listing while this node holds entries is refused.
func TestHeldAsksTheActuatorAboutAnUnlistedPod(t *testing.T) {
	root := mhRoot(t)
	d := mhMkPod(t, root, "burstable", mhUID, false, "max", "209715200")
	a := mhNew(t, root)
	l := MemoryLedger()
	acting := NewActing("n1", l, a)
	ctx := context.Background()
	if _, err := acting.Apply(ctx, mhWL("burstable"), "memory.high", 128<<20, time.Now().Add(time.Hour), "d1"); err != nil {
		t.Fatal(err)
	}
	other := runtime.Workload{Subject: "pod:ffff0000-0000-0000-0000-000000000000", UID: "ffff0000-0000-0000-0000-000000000000", QoS: "burstable"}
	held, dropped, _, err := held4(acting, ctx, "memory.high", []runtime.Workload{other})
	if err != nil || len(held) != 1 || len(dropped) != 0 || len(l.All()) != 1 {
		t.Fatalf("cgroup there: held %+v dropped %+v err %v; want it held", held, dropped, err)
	}
	if _, _, _, err := held4(acting, ctx, "memory.high", nil); err == nil || len(l.All()) != 1 {
		t.Fatalf("empty listing: err %v ledger %+v; want a refusal and the ledger unchanged", err, l.All())
	}
	if err := os.RemoveAll(d); err != nil {
		t.Fatal(err)
	}
	held, dropped, _, err = held4(acting, ctx, "memory.high", []runtime.Workload{other})
	if err != nil || len(held) != 0 || len(dropped) != 1 || len(l.All()) != 0 {
		t.Fatalf("cgroup gone: held %+v dropped %+v err %v; want it dropped", held, dropped, err)
	}
}

// TestMemoryHighRefusesAReadOnlyTree: an actuator that could never write
// memory.high refuses to build, naming the root and that it is read-only, rather
// than refusing every step: with a pod, by opening its memory.high for writing;
// with none yet, by the kubepods directory's write access.
func TestMemoryHighRefusesAReadOnlyTree(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through file modes")
	}
	t.Run("a pod's memory.high", func(t *testing.T) {
		root := mhRoot(t)
		d := mhMkPod(t, root, "burstable", mhUID, false, "max", "")
		if err := os.Chmod(filepath.Join(d, "memory.high"), 0o444); err != nil {
			t.Fatal(err)
		}
		_, err := NewMemoryHigh(Env{CgroupRoot: root})
		if err == nil || !strings.Contains(err.Error(), root) || !strings.Contains(err.Error(), "read-only") {
			t.Fatalf("err %v; want a refusal naming %s and read-only", err, root)
		}
		if got := mhRead(t, filepath.Join(d, "memory.high")); got != "max" {
			t.Fatalf("the probe changed memory.high to %q", got)
		}
	})
	t.Run("no pod yet", func(t *testing.T) {
		root := mhRoot(t)
		kp := filepath.Join(root, "kubepods")
		if err := os.MkdirAll(kp, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(kp, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(kp, 0o755) })
		_, err := NewMemoryHigh(Env{CgroupRoot: root})
		if err == nil || !strings.Contains(err.Error(), root) || !strings.Contains(err.Error(), "read-only") {
			t.Fatalf("err %v; want a refusal naming %s and read-only", err, root)
		}
	})
	t.Run("writable", func(t *testing.T) {
		root := mhRoot(t)
		mhMkPod(t, root, "burstable", mhUID, false, "max", "")
		if _, err := NewMemoryHigh(Env{CgroupRoot: root}); err != nil {
			t.Fatal(err)
		}
	})
}

// TestMemoryHighDryRunSkipsTheWriteProbe: an actuator that will be wrapped in a
// dry run never writes, so a read-only tree, as the DaemonSet's /host, does not
// refuse it; every other precondition still applies.
func TestMemoryHighDryRunSkipsTheWriteProbe(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through file modes")
	}
	root := mhRoot(t)
	d := mhMkPod(t, root, "burstable", mhUID, false, "max", "")
	if err := os.Chmod(filepath.Join(d, "memory.high"), 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := NewMemoryHigh(Env{CgroupRoot: root}); err == nil {
		t.Fatal("a read-only tree built without DryRun")
	}
	if _, err := NewMemoryHigh(Env{CgroupRoot: root, DryRun: true}); err != nil {
		t.Fatalf("dry run refused on a read-only tree: %v", err)
	}
	if _, err := NewMemoryHigh(Env{CgroupRoot: t.TempDir(), DryRun: true}); err == nil {
		t.Fatal("dry run built without cgroup v2")
	}
}

// TestMemoryHighRefusesAnUnreadableMemoryHigh: a memory.high that cannot be read
// (here a directory, EISDIR, which holds even as root) is not taken as unmanaged:
// Build fails naming the file, without claiming MemoryQoS. One that is absent,
// as a cgroup removed mid-scan, is skipped.
func TestMemoryHighRefusesAnUnreadableMemoryHigh(t *testing.T) {
	// Pod level.
	root := mhRoot(t)
	d := mhPodDirs(root, "burstable", mhUID, false)
	if err := os.MkdirAll(filepath.Join(d, "memory.high"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := NewMemoryHigh(Env{CgroupRoot: root, DryRun: true})
	if err == nil || strings.Contains(err.Error(), "MemoryQoS") || !strings.Contains(err.Error(), "could not be read") ||
		!strings.Contains(err.Error(), filepath.Join(d, "memory.high")) {
		t.Fatalf("pod level: %v", err)
	}
	// Container level.
	root = mhRoot(t)
	pd := mhMkPod(t, root, "guaranteed", mhUID, true, "max", "")
	c := filepath.Join(pd, "cri-containerd-abc.scope")
	if err := os.MkdirAll(filepath.Join(c, "memory.high"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = NewMemoryHigh(Env{CgroupRoot: root, DryRun: true})
	if err == nil || strings.Contains(err.Error(), "MemoryQoS") || !strings.Contains(err.Error(), filepath.Join(c, "memory.high")) {
		t.Fatalf("container level: %v", err)
	}
	// Absent is skipped.
	root = mhRoot(t)
	if err := os.MkdirAll(mhPodDirs(root, "besteffort", mhUID, false), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewMemoryHigh(Env{CgroupRoot: root, DryRun: true}); err != nil {
		t.Fatalf("absent memory.high: %v", err)
	}
}
