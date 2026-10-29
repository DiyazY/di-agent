package relief

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/actuators"
	"github.com/DiyazY/di-agent/pkg/runtime"
	"github.com/DiyazY/di-agent/pkg/runtime/kubernetes"
	"github.com/DiyazY/di-agent/pkg/spec"
	"github.com/DiyazY/di-agent/pkg/statemap"
)

// pods is a runtime that lists the pods it is given, with their identity, as
// the Kubernetes runtime does.
type pods struct {
	mu   sync.Mutex
	ws   []runtime.Workload
	fail int // Workloads fails this many more times
}

func (p *pods) Name() string { return "kubernetes" }

func (p *pods) Workloads(context.Context) ([]runtime.Workload, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail > 0 {
		p.fail--
		return nil, errors.New("api unavailable")
	}
	return append([]runtime.Workload(nil), p.ws...), nil
}

const memUID = "1234abcd-0000-1111-2222-333344445555"

// memHarness is a memory.high loop over the real actuator and a fake cgroup v2
// tree: one burstable pod using 0.6 of an 8 GiB node, related to memory
// contention with strength 1, both levers' real descriptors and the default
// memory lever.
type memHarness struct {
	t      *testing.T
	now    time.Time
	m      *statemap.Map
	ledger *actuators.Ledger
	l      *Loop
	rt     *pods
	root   string
	lever  spec.Lever
	high   string // the pod's memory.high file
	mu     sync.Mutex
}

const memTotal = 8 << 30

func newMemHarness(t *testing.T) *memHarness {
	t.Helper()
	h := &memHarness{t: t, now: t0, ledger: actuators.MemoryLedger()}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpu memory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "kubepods", "burstable", "pod"+memUID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	h.high = filepath.Join(dir, "memory.high")
	for f, v := range map[string]string{"memory.high": "max", "memory.current": "5153960755"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	act, err := actuators.NewMemoryHigh(actuators.Env{CgroupRoot: root, Held: h.ledger.For})
	if err != nil {
		t.Fatal(err)
	}
	d := act.Describe()
	if d.Action != actuators.MemoryHighDescriptor.Action || d.Relieves != actuators.MemoryHighDescriptor.Relieves {
		t.Fatalf("descriptor %+v", d)
	}
	subject := "pod:" + memUID
	req := float64(100 << 20)
	rt := &pods{ws: []runtime.Workload{{Subject: subject, Name: "hog", UID: memUID, QoS: "burstable",
		Resources: map[string]runtime.Resource{"memory": {Request: &req}}}}}

	h.m = newMap(t, h.clock)
	record(t, h.m, d.Usage, "", 0.8, h.now)
	record(t, h.m, d.Relieves, "", 0, h.now)
	record(t, h.m, d.Usage+"@"+subject, subject, 0.6, h.now)
	r := statemap.Relationship{From: d.Usage + "@" + subject, To: d.Relieves, Sign: 1, Label: "drives"}
	if err := h.m.DeclareRelationship(r); err != nil {
		t.Fatal(err)
	}
	if err := h.m.AssertRelationshipStrength(statemap.RelationshipID(r.From, r.To, r.Label), 1, "test", "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := h.m.RecordFacts("test", []statemap.Fact{statemap.NumberFact(d.Capacity, memTotal, d.Unit)}); err != nil {
		t.Fatal(err)
	}
	relief := spec.Default().Loops.Relief
	var lever spec.Lever
	for _, l := range relief.Levers {
		if l.Action == d.Action {
			lever = l
		}
	}
	h.rt, h.root, h.lever = rt, root, lever
	h.l = h.newLoop()
	return h
}

// newLoop is a memory.high loop over the harness's tree, ledger and runtime, as
// a restarted agent builds it.
func (h *memHarness) newLoop() *Loop {
	h.t.Helper()
	act, err := actuators.NewMemoryHigh(actuators.Env{NodeID: "n1", CgroupRoot: h.root, Held: h.ledger.For})
	if err != nil {
		h.t.Fatal(err)
	}
	cpuAct := actuators.Static(kubernetes.CPULimitDescriptor, nil, func(runtime.Workload) (float64, bool) { return 0, false })
	l, err := New(Config{Interval: spec.Default().Loops.Relief.Interval, Lever: h.lever, NodeID: "n1", Runtime: h.rt,
		Acting: clocked(actuators.NewActing("n1", h.ledger, act, cpuAct), h.clock), Policy: defaultPolicy(h.t, nil), Map: h.m,
		Now: h.clock, Logf: h.t.Logf})
	if err != nil {
		h.t.Fatal(err)
	}
	return l
}

// restart replaces the loop with a new one and reconciles it.
func (h *memHarness) restart() *Loop {
	h.t.Helper()
	h.l = h.newLoop()
	if err := h.l.Reconcile(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	return h.l
}

func (h *memHarness) clock() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.now }

func (h *memHarness) step(p float64) {
	h.t.Helper()
	h.mu.Lock()
	h.now = h.now.Add(tick)
	h.mu.Unlock()
	record(h.t, h.m, actuators.MemoryHighDescriptor.Relieves, "", p, h.clock())
	h.l.Tick(context.Background())
}

func (h *memHarness) until(limit time.Duration, p float64, what string, cond func() bool) {
	h.t.Helper()
	for i := time.Duration(0); i < limit; i += tick {
		h.step(p)
		if cond() {
			return
		}
	}
	h.t.Fatalf("after %s at pressure %v: %s did not happen", limit, p, what)
}

func (h *memHarness) file() string {
	h.t.Helper()
	b, err := os.ReadFile(h.high)
	if err != nil {
		h.t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// act steps at pressure 0.5 until memory.high is lowered, and returns the action.
func (h *memHarness) act() Action {
	h.t.Helper()
	h.until(10*time.Minute, 0.5, "a memory.high cap", func() bool { return len(h.ledger.All()) == 1 })
	if got := h.file(); got == "max" {
		h.t.Fatalf("ledger holds %+v but memory.high is max", h.ledger.All())
	}
	return h.l.Actions()[0]
}

// given back is memory.high at max, nothing in the ledger, and the action closed
// with outcome.
func (h *memHarness) givenBack(id, outcome string) {
	h.t.Helper()
	if got := h.file(); got != "max" {
		h.t.Errorf("memory.high = %q after %s; want max", got, outcome)
	}
	if es := h.ledger.All(); len(es) != 0 {
		h.t.Errorf("ledger still holds %+v", es)
	}
	for _, a := range h.l.Actions() {
		if a.ID == id && a.Outcome != outcome {
			h.t.Errorf("action %s outcome %q; want %q", id, a.Outcome, outcome)
		}
	}
}

func outcomeOf(l *Loop, id string) string {
	for _, a := range l.Actions() {
		if a.ID == id {
			return a.Outcome
		}
	}
	return ""
}

func TestMemoryHighIsGivenBackOnRelease(t *testing.T) {
	h := newMemHarness(t)
	a := h.act()
	h.until(30*time.Minute, 0, "a release", func() bool {
		o := outcomeOf(h.l, a.ID)
		return o != "" && o != OutcomeKept
	})
	h.givenBack(a.ID, OutcomeReleased)
}

func TestMemoryHighIsGivenBackOnExpiry(t *testing.T) {
	h := newMemHarness(t)
	a := h.act()
	// Pressure stays high enough to keep the loop relieving, low enough that the
	// cap is judged to have helped.
	h.until(2*time.Hour, 0.3, "expiry", func() bool {
		o := outcomeOf(h.l, a.ID)
		return o != "" && o != OutcomeKept
	})
	h.givenBack(a.ID, OutcomeExpired)
}

func TestMemoryHighIsGivenBackByHand(t *testing.T) {
	h := newMemHarness(t)
	a := h.act()
	if err := h.l.Revert(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	h.givenBack(a.ID, OutcomeManual)
}

// TestMemoryHighIsNotGivenBackWithoutAListing: an expiry that falls in a pass
// whose listing fails gives nothing back and journals nothing gone; the cap stays
// in force and is given back by the next pass that can list.
func TestMemoryHighIsNotGivenBackWithoutAListing(t *testing.T) {
	h := newMemHarness(t)
	a := h.act()
	h.until(2*time.Hour, 0.3, "a kept cap near expiry", func() bool {
		return !h.clock().Add(tick).Before(a.Entry.Expires)
	})
	h.rt.mu.Lock()
	h.rt.fail = 1
	h.rt.mu.Unlock()
	h.step(0.3)
	if o := outcomeOf(h.l, a.ID); o != OutcomeKept {
		t.Fatalf("outcome %q after an expiry with no listing; want it still kept", o)
	}
	if got := h.file(); got == "max" || len(h.ledger.All()) != 1 {
		t.Fatalf("memory.high %q, ledger %+v; want the cap and its entry in force", got, h.ledger.All())
	}
	h.step(0.3)
	h.givenBack(a.ID, OutcomeExpired)
}

// TestMemoryHighManualRevertWithoutAListingFails: an operator's revert that
// cannot list the workloads is an error, and the change stays.
func TestMemoryHighManualRevertWithoutAListingFails(t *testing.T) {
	h := newMemHarness(t)
	a := h.act()
	h.rt.mu.Lock()
	h.rt.fail = 1
	h.rt.mu.Unlock()
	if err := h.l.Revert(context.Background(), a.ID); err == nil {
		t.Fatal("revert without a listing succeeded")
	}
	if got := h.file(); got == "max" || len(h.ledger.All()) != 1 {
		t.Fatalf("memory.high %q, ledger %+v; want the cap and its entry in force", got, h.ledger.All())
	}
	if err := h.l.Revert(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	h.givenBack(a.ID, OutcomeManual)
}

// unlist makes the runtime list another pod in place of the memory hog, as a
// listing that has lost it while its cgroup may still be there.
func (h *memHarness) unlist() {
	h.rt.mu.Lock()
	h.rt.ws = []runtime.Workload{{Subject: "pod:ffff0000-0000-0000-0000-000000000000", Name: "other",
		UID: "ffff0000-0000-0000-0000-000000000000", QoS: "burstable"}}
	h.rt.mu.Unlock()
}

// TestMemoryHighOfAPodNoLongerListedIsGivenBack: the listing does not decide that
// a pod left; the actuator does. A pod the runtime stops listing whose cgroup is
// still there is found by the UID in its subject, its memory.high written back
// to max, and the change closed as released, not gone.
func TestMemoryHighOfAPodNoLongerListedIsGivenBack(t *testing.T) {
	h := newMemHarness(t)
	a := h.act()
	h.unlist()
	h.until(2*time.Hour, 0, "given back", func() bool {
		o := outcomeOf(h.l, a.ID)
		return o != "" && o != OutcomeKept
	})
	h.givenBack(a.ID, OutcomeReleased)
}

// TestMemoryHighOfAnUnlistedPodIsGivenBackAtExpiry: the expiry path too gives
// back through the subject's UID, and closes the change as expired.
func TestMemoryHighOfAnUnlistedPodIsGivenBackAtExpiry(t *testing.T) {
	h := newMemHarness(t)
	a := h.act()
	h.until(2*time.Hour, 0.3, "a kept cap near expiry", func() bool {
		return outcomeOf(h.l, a.ID) == OutcomeKept && !h.clock().Add(tick).Before(a.Entry.Expires)
	})
	h.unlist()
	h.step(0.3)
	h.givenBack(a.ID, OutcomeExpired)
}

// TestMemoryHighOfAPodWhoseCgroupLeftIsGone: unlisted and with its cgroup gone
// from a QoS parent that is there, the pod has left: the change is gone.
func TestMemoryHighOfAPodWhoseCgroupLeftIsGone(t *testing.T) {
	h := newMemHarness(t)
	a := h.act()
	h.unlist()
	if err := os.RemoveAll(filepath.Dir(h.high)); err != nil {
		t.Fatal(err)
	}
	h.until(2*time.Hour, 0, "gone", func() bool { return outcomeOf(h.l, a.ID) == OutcomeGone })
	if es := h.ledger.All(); len(es) != 0 {
		t.Fatalf("ledger still holds %+v", es)
	}
}

// TestMemoryHighReconcileOfAnUnlistedPodKeepsIt: after a restart, an entry
// whose pod the runtime does not list but whose cgroup still holds the cap is
// taken up, not dropped, and given back like any other.
func TestMemoryHighReconcileOfAnUnlistedPodKeepsIt(t *testing.T) {
	h := newMemHarness(t)
	a := h.act()
	h.unlist()
	h2 := h.restart()
	if es := h.ledger.All(); len(es) != 1 {
		t.Fatalf("ledger %+v after reconcile; want the entry kept", es)
	}
	h.until(2*time.Hour, 0, "given back", func() bool { return len(h.ledger.All()) == 0 })
	if got := h.file(); got != "max" {
		t.Fatalf("memory.high %q; want max", got)
	}
	if o := outcomeOf(h2, a.ID); o == OutcomeGone {
		t.Fatalf("outcome %q; want it given back, not gone", o)
	}
}

// TestMemoryHighReconcileRefusesAnEmptyListing: a listing that holds no pod at
// all on a node whose ledger holds caps is a listing problem until proven
// otherwise: the reconcile fails and the ledger is unchanged.
func TestMemoryHighReconcileRefusesAnEmptyListing(t *testing.T) {
	h := newMemHarness(t)
	h.act()
	h.rt.mu.Lock()
	h.rt.ws = nil
	h.rt.mu.Unlock()
	before := h.ledger.All()
	l2 := h.newLoop()
	if err := l2.Reconcile(context.Background()); err == nil {
		t.Fatal("reconcile on an empty listing succeeded")
	}
	if got := h.ledger.All(); len(got) != 1 || got[0] != before[0] {
		t.Fatalf("ledger %+v; want %+v unchanged", got, before)
	}
}

// TestMemoryHighFurtherStepIsUndonePastCheck: a further step that did not help
// is undone by putting the earlier cap back, and that undo is never gated by
// Check: the tighter cap may have reclaimed the pod down to its request, which
// Check refuses for a cut, and the earlier, looser cap must still go back.
func TestMemoryHighFurtherStepIsUndonePastCheck(t *testing.T) {
	h := newMemHarness(t)
	first := h.act()
	h.until(10*time.Minute, 0.3, "the first step kept", func() bool { return outcomeOf(h.l, first.ID) == OutcomeKept })
	earlier := h.ledger.All()[0].Applied
	// The pod now uses what its cap lets it, and still drives the pressure.
	subject := "pod:" + memUID
	h.until(time.Hour, 0.3, "a further step", func() bool {
		record(h.t, h.m, actuators.MemoryHighDescriptor.Usage+"@"+subject, subject, 0.45, h.clock())
		es := h.ledger.All()
		return len(es) == 1 && es[0].Applied < earlier
	})
	// The tighter cap reclaimed the pod down to its request (100 MiB).
	if err := os.WriteFile(filepath.Join(filepath.Dir(h.high), "memory.current"), []byte("104857600\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.until(10*time.Minute, 0.3, "the further step undone", func() bool {
		es := h.ledger.All()
		return len(es) == 1 && es[0].Applied == earlier
	})
	if got := h.file(); got != strconv.FormatInt(int64(earlier), 10) {
		t.Fatalf("memory.high %q; want the earlier step %v back", got, earlier)
	}
	a := h.action(first.ID)
	if a.Outcome != OutcomeKept || a.RevertAttempts != 0 || a.Entry.Applied != earlier {
		t.Fatalf("action %+v; want kept at the earlier step, no failed undo", a)
	}
	if h.l.Status().Reverted != 1 {
		t.Fatalf("status %+v; want one revert", h.l.Status())
	}
}

func (h *memHarness) action(id string) Action {
	h.t.Helper()
	for _, a := range h.l.Actions() {
		if a.ID == id {
			return a
		}
	}
	h.t.Fatalf("no action %s", id)
	return Action{}
}

// TestAnEmptyListingAtStartStillExpires: a restart whose listing holds no pod
// refuses to reconcile, but does not wedge: the node's changes are shown as
// stuck with the reconcile error, and one past its expiry is given back through
// its subject, memory.high written back to max and the change closed as expired.
func TestAnEmptyListingAtStartStillExpires(t *testing.T) {
	h := newMemHarness(t)
	h.act()
	h.rt.mu.Lock()
	h.rt.ws = nil
	h.rt.mu.Unlock()
	h.mu.Lock()
	h.now = h.now.Add(2 * time.Hour)
	h.mu.Unlock()
	h.l = h.newLoop()
	if err := h.l.Reconcile(context.Background()); err == nil {
		t.Fatal("reconcile on an empty listing succeeded")
	}
	st := h.l.Status()
	if st.ReconcileError == "" || st.Stuck != 1 || len(st.Unread) != 1 {
		t.Fatalf("status %+v; want the reconcile error shown and the change stuck", st)
	}
	h.step(0)
	h.step(0)
	if got := h.file(); got != "max" {
		t.Fatalf("memory.high %q; want max", got)
	}
	if es := h.ledger.All(); len(es) != 0 {
		t.Fatalf("ledger %+v; want the expired change given back", es)
	}
	var found bool
	for _, a := range h.l.Actions() {
		found = found || a.Outcome == OutcomeExpired
	}
	if !found {
		t.Fatalf("actions %+v; want one expired", h.l.Actions())
	}
}
