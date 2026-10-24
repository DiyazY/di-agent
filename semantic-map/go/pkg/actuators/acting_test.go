package actuators

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/internal/testhooks"
	"github.com/DiyazY/di-agent/pkg/runtime"
)

// memAct is an actuator over an in-memory value per subject.
type memAct struct {
	mu     sync.Mutex
	vals   map[string]float64
	gone   map[string]bool
	sets   []memSet
	setErr error
}

type memSet struct {
	subject string
	v       float64
	set     bool
}

func newMem() *memAct { return &memAct{vals: map[string]float64{}, gone: map[string]bool{}} }

func (f *memAct) Describe() Descriptor            { return Descriptor{Action: "fake"} }
func (f *memAct) Check(w runtime.Workload) string { return "" }
func (f *memAct) Read(ctx context.Context, w runtime.Workload) (float64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gone[w.Subject] {
		return 0, false, ErrGone
	}
	v, ok := f.vals[w.Subject]
	return v, ok, nil
}
func (f *memAct) Set(ctx context.Context, w runtime.Workload, v float64, set bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return f.setErr
	}
	if f.gone[w.Subject] {
		return ErrGone
	}
	f.sets = append(f.sets, memSet{w.Subject, v, set})
	if set {
		f.vals[w.Subject] = v
	} else {
		delete(f.vals, w.Subject)
	}
	return nil
}

func wl(s string) runtime.Workload { return runtime.Workload{Subject: s, Name: s} }

func TestApplyRecordsOriginalOnce(t *testing.T) {
	f := newMem()
	f.vals["s"] = 10
	a := NewActing("n", MemoryLedger(), f)
	ctx := context.Background()
	exp := time.Unix(100, 0)
	e1, err := a.Apply(ctx, wl("s"), "fake", 5, exp, "d1")
	if err != nil {
		t.Fatal(err)
	}
	e2, err := a.Apply(ctx, wl("s"), "fake", 3, exp, "d2")
	if err != nil {
		t.Fatal(err)
	}
	if e1.Original != 10 || !e1.OriginalSet || e2.Original != 10 || !e2.OriginalSet || e2.Applied != 3 || e2.NodeID != "n" || e2.DecisionID != "d2" {
		t.Fatalf("%+v %+v", e1, e2)
	}
	if got := a.ledger.All(); len(got) != 1 || got[0] != e2 {
		t.Fatalf("ledger %+v", got)
	}
}

func TestApplyUnknownActionAndSetError(t *testing.T) {
	f := newMem()
	a := NewActing("n", MemoryLedger(), f)
	if _, err := a.Apply(context.Background(), wl("s"), "nope", 1, time.Time{}, ""); err == nil {
		t.Fatal("want error for no actuator")
	}
	// A failed Set may still land: the step stays pending, for Held to settle.
	f.setErr = errors.New("boom")
	if _, err := a.Apply(context.Background(), wl("s"), "fake", 1, time.Time{}, ""); !errors.Is(err, ErrLeftPending) {
		t.Fatalf("err %v; want the failed step left pending", err)
	}
	if got := a.ledger.All(); len(got) != 1 || !got[0].Pending || got[0].Applied != 1 {
		t.Fatalf("ledger %+v; want the failed step pending", got)
	}
	f.setErr = nil
	if _, err := a.ResolveNeverTook(context.Background(), wl("s"), a.ledger.All()[0]); err != nil {
		t.Fatal(err)
	}
	e, _ := a.Apply(context.Background(), wl("s"), "fake", 2, time.Time{}, "")
	f.setErr = errors.New("boom")
	if _, err := a.Apply(context.Background(), wl("s"), "fake", 9, time.Time{}, ""); !errors.Is(err, ErrLeftPending) {
		t.Fatalf("err %v; want the failed further step left pending", err)
	}
	if got := a.ledger.All(); len(got) != 1 || !got[0].Pending || got[0].PrevApplied != e.Applied || !got[0].PrevSet {
		t.Fatalf("ledger %+v; want the further step pending over %+v", got, e)
	}
}

func TestRevertSetsOriginalAndDeletes(t *testing.T) {
	f := newMem() // no original
	a := NewActing("n", MemoryLedger(), f)
	ctx := context.Background()
	e, err := a.Apply(ctx, wl("s"), "fake", 5, time.Time{}, "d")
	if err != nil || e.OriginalSet {
		t.Fatalf("%+v %v", e, err)
	}
	if err := a.Revert(ctx, wl("s"), e); err != nil {
		t.Fatal(err)
	}
	last := f.sets[len(f.sets)-1]
	if last != (memSet{"s", 0, false}) {
		t.Fatalf("last set %+v", last)
	}
	if len(a.ledger.All()) != 0 {
		t.Fatal("entry not deleted")
	}
}

func TestRevertGoneDeletes(t *testing.T) {
	f := newMem()
	a := NewActing("n", MemoryLedger(), f)
	ctx := context.Background()
	e, _ := a.Apply(ctx, wl("s"), "fake", 5, time.Time{}, "d")
	f.gone["s"] = true
	if err := a.Revert(ctx, wl("s"), e); !errors.Is(err, ErrGone) {
		t.Fatalf("err %v; want ErrGone, so the caller can tell the workload left", err)
	}
	if len(a.ledger.All()) != 0 {
		t.Fatal("entry not deleted")
	}
}

func TestReconcileDropsChangedUnderneath(t *testing.T) {
	f := newMem()
	a := NewActing("n", MemoryLedger(), f)
	ctx := context.Background()
	a.Apply(ctx, wl("keep"), "fake", 5, time.Time{}, "")
	a.Apply(ctx, wl("chg"), "fake", 5, time.Time{}, "")
	f.vals["chg"] = 7
	held, dropped, _, err := held4(a, ctx, "fake", []runtime.Workload{wl("keep"), wl("chg")})
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 || held[0].Subject != "keep" || len(dropped) != 1 || dropped[0].Subject != "chg" {
		t.Fatalf("held %+v dropped %+v", held, dropped)
	}
	if len(a.ledger.All()) != 1 {
		t.Fatal("dropped not deleted")
	}
}

func TestReconcileDropsGoneWorkload(t *testing.T) {
	f := newMem()
	a := NewActing("n", MemoryLedger(), f)
	ctx := context.Background()
	a.Apply(ctx, wl("s"), "fake", 5, time.Time{}, "")
	f.gone["s"] = true
	held, dropped, _, err := held4(a, ctx, "fake", []runtime.Workload{wl("x")})
	if err != nil || len(held) != 0 || len(dropped) != 1 || len(a.ledger.All()) != 0 {
		t.Fatalf("%+v %+v %v", held, dropped, err)
	}
}

// TestReconcileKeepsAnUnlistedWorkloadTheActuatorFinds: a workload missing from
// the listing is not taken for gone; the actuator is asked, and one it still
// reads as applied is held.
func TestReconcileKeepsAnUnlistedWorkloadTheActuatorFinds(t *testing.T) {
	f := newMem()
	a := NewActing("n", MemoryLedger(), f)
	ctx := context.Background()
	a.Apply(ctx, wl("s"), "fake", 5, time.Time{}, "")
	held, dropped, _, err := held4(a, ctx, "fake", []runtime.Workload{wl("x")})
	if err != nil || len(held) != 1 || len(dropped) != 0 || len(a.ledger.All()) != 1 {
		t.Fatalf("held %+v dropped %+v err %v; want it held", held, dropped, err)
	}
}

// TestReconcileRefusesAnEmptyListing: no workload listed at all while this node
// holds entries of the action is a listing problem until proven otherwise.
func TestReconcileRefusesAnEmptyListing(t *testing.T) {
	f := newMem()
	a := NewActing("n", MemoryLedger(), f)
	ctx := context.Background()
	a.Apply(ctx, wl("s"), "fake", 5, time.Time{}, "")
	f.gone["s"] = true
	if _, _, _, err := held4(a, ctx, "fake", nil); err == nil || len(a.ledger.All()) != 1 {
		t.Fatalf("err %v ledger %+v; want a refusal and the entry kept", err, a.ledger.All())
	}
}

func TestReconcileIgnoresOtherNode(t *testing.T) {
	f := newMem()
	l := MemoryLedger()
	l.Put(Entry{Action: "fake", Subject: "s", Applied: 5, NodeID: "other"})
	a := NewActing("n", l, f)
	held, dropped, _, err := held4(a, context.Background(), "fake", nil)
	if err != nil || len(held) != 0 || len(dropped) != 0 || len(l.All()) != 1 {
		t.Fatalf("%+v %+v %v", held, dropped, err)
	}
}

func TestActingConcurrent(t *testing.T) {
	f := newMem()
	a := NewActing("n", MemoryLedger(), f)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a.Apply(context.Background(), wl(fmt.Sprint("s", i)), "fake", float64(i+1), time.Time{}, "")
			held4(a, context.Background(), "fake", nil)
		}(i)
	}
	wg.Wait()
}

func TestDescriptorsSorted(t *testing.T) {
	a := NewActing("n", MemoryLedger(), newMem())
	if d := a.Descriptors(); len(d) != 1 || d[0].Action != "fake" {
		t.Fatalf("%+v", d)
	}
}

func TestApplyRollsBackWhenLedgerSaveFails(t *testing.T) {
	f := newMem()
	f.vals["s"] = 10
	l := MemoryLedger()
	a := NewActing("n", l, f)
	ctx := context.Background()
	l.saveErr = errors.New("disk full")
	if _, err := a.Apply(ctx, wl("s"), "fake", 5, time.Time{}, ""); err == nil || !errors.Is(err, l.saveErr) {
		t.Fatalf("err = %v", err)
	}
	if f.vals["s"] != 10 || len(l.All()) != 0 {
		t.Fatalf("not restored: %v %+v", f.vals, l.All())
	}
	// With a prior entry, roll back to its Applied.
	l.saveErr = nil
	if _, err := a.Apply(ctx, wl("s"), "fake", 5, time.Time{}, ""); err != nil {
		t.Fatal(err)
	}
	l.saveErr = errors.New("disk full")
	if _, err := a.Apply(ctx, wl("s"), "fake", 3, time.Time{}, ""); err == nil {
		t.Fatal("want error")
	}
	if f.vals["s"] != 5 {
		t.Fatalf("want 5, got %v", f.vals["s"])
	}
	// No original: rollback removes the setting; a failing rollback is joined.
	l.saveErr = errors.New("disk full")
	if _, err := a.Apply(ctx, wl("t"), "fake", 7, time.Time{}, ""); err == nil {
		t.Fatal("want error")
	}
	if _, set := f.vals["t"]; set {
		t.Fatal("setting not removed")
	}
}

type quantised struct{ *memAct }

func (q quantised) Set(ctx context.Context, w runtime.Workload, v float64, set bool) error {
	return q.memAct.Set(ctx, w, float64(int64(v)/4096*4096), set)
}

func TestApplyRecordsReadBackValue(t *testing.T) {
	f := quantised{newMem()}
	a := NewActing("n", MemoryLedger(), f)
	ctx := context.Background()
	e, err := a.Apply(ctx, wl("s"), "fake", 10000, time.Time{}, "")
	if err != nil || e.Applied != 8192 {
		t.Fatalf("%+v %v", e, err)
	}
	held, dropped, _, err := held4(a, ctx, "fake", []runtime.Workload{wl("s")})
	if err != nil || len(held) != 1 || len(dropped) != 0 {
		t.Fatalf("%+v %+v %v", held, dropped, err)
	}
}

func TestReconcileSkipsAnActionWithNoActuator(t *testing.T) {
	l := MemoryLedger()
	l.Put(Entry{Action: "removed", Subject: "s", Applied: 5, NodeID: "n"})
	a := NewActing("n", l, newMem())
	held, dropped, _, err := held4(a, context.Background(), "removed", nil)
	if err != nil || len(held) != 0 || len(dropped) != 0 || len(l.All()) != 1 {
		t.Fatalf("held %+v dropped %+v err %v; want the entry kept, neither held nor dropped", held, dropped, err)
	}
	held, dropped, _, err = held4(a, context.Background(), "fake", nil)
	if err != nil || len(held) != 0 || len(dropped) != 0 || len(l.All()) != 1 {
		t.Fatalf("held %+v dropped %+v err %v; want the entry kept, neither held nor dropped", held, dropped, err)
	}
}

// TestReconcileKeepsAnEntryWhoseReadFails: an entry whose read fails is neither
// held nor dropped: it is returned as failed, with the error, and kept.
func TestReconcileKeepsAnEntryWhoseReadFails(t *testing.T) {
	f := &readErr{memAct: newMem()}
	a := NewActing("n", MemoryLedger(), f)
	ctx := context.Background()
	if _, err := a.Apply(ctx, wl("s"), "fake", 5, time.Time{}, ""); err != nil {
		t.Fatal(err)
	}
	f.fail = true
	held, dropped, failed, err := held4(a, ctx, "fake", []runtime.Workload{wl("s")})
	if err != nil || len(held) != 0 || len(dropped) != 0 || len(failed) != 1 || failed[0].Entry.Subject != "s" || failed[0].Err == nil {
		t.Fatalf("held %+v dropped %+v failed %+v err %v; want s failed with its error", held, dropped, failed, err)
	}
	if len(a.ledger.All()) != 1 {
		t.Fatal("an entry whose read failed was dropped")
	}
}

// readErr is memAct whose Read fails while fail is set.
type readErr struct {
	*memAct
	fail bool
}

func (r *readErr) Read(ctx context.Context, w runtime.Workload) (float64, bool, error) {
	if r.fail {
		return 0, false, errors.New("api unavailable")
	}
	return r.memAct.Read(ctx, w)
}

// lying records the value and then reports failure, as a resize whose call timed
// out after it took.
type lying struct{ *memAct }

func (l lying) Set(ctx context.Context, w runtime.Workload, v float64, set bool) error {
	if err := l.memAct.Set(ctx, w, v, set); err != nil {
		return err
	}
	return errors.New("context deadline exceeded")
}

func TestAFailedSetThatTookEffectIsRecorded(t *testing.T) {
	f := lying{newMem()}
	f.vals["s"] = 10
	a := NewActing("n", MemoryLedger(), f)
	e, err := a.Apply(context.Background(), wl("s"), "fake", 5, time.Time{}, "d")
	if err == nil {
		t.Fatal("want the Set error")
	}
	if got := a.ledger.All(); len(got) != 1 || got[0].Applied != 5 || got[0].Original != 10 || got[0] != e {
		t.Fatalf("ledger %+v; want the change that took effect recorded", got)
	}
}

type refusing struct{ *memAct }

func (refusing) Check(runtime.Workload) string { return "not running" }

func TestApplyAsksCheckFirst(t *testing.T) {
	f := refusing{newMem()}
	a := NewActing("n", MemoryLedger(), f)
	if _, err := a.Apply(context.Background(), wl("s"), "fake", 5, time.Time{}, ""); err == nil || len(f.sets) != 0 {
		t.Fatalf("err %v sets %+v; want refused before any Set", err, f.sets)
	}
}

// timesOut sets the value and then runs out the caller's context, as a resize
// whose call outlived the pass deadline after the API server took it. Its Read
// honours the context it is given.
type timesOut struct {
	*memAct
	cancel context.CancelFunc
}

func (t timesOut) Read(ctx context.Context, w runtime.Workload) (float64, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	return t.memAct.Read(ctx, w)
}

func (t timesOut) Set(ctx context.Context, w runtime.Workload, v float64, set bool) error {
	if err := t.memAct.Set(ctx, w, v, set); err != nil {
		return err
	}
	t.cancel()
	return ctx.Err()
}

func TestASetThatTimedOutAfterTakingEffectIsAdopted(t *testing.T) {
	for _, further := range []bool{false, true} {
		m := newMem()
		m.vals["s"] = 10
		l := MemoryLedger()
		if further {
			pre := NewActing("n", l, m)
			if _, err := pre.Apply(context.Background(), wl("s"), "fake", 7, time.Time{}, "d0"); err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		a := NewActing("n", l, timesOut{m, cancel})
		if _, err := a.Apply(ctx, wl("s"), "fake", 5, time.Time{}, "d"); !errors.Is(err, context.Canceled) {
			t.Fatalf("further=%v: err %v; want the Set's context error", further, err)
		}
		got := l.All()
		if len(got) != 1 || got[0].Pending || got[0].Applied != 5 || got[0].Original != 10 || !got[0].OriginalSet {
			t.Fatalf("further=%v: ledger %+v; want the change adopted, with the first original", further, got)
		}
		held, dropped, _, err := held4(a, context.Background(), "fake", []runtime.Workload{wl("s")})
		if err != nil || len(held) != 1 || len(dropped) != 0 {
			t.Fatalf("further=%v: held %+v dropped %+v err %v", further, held, dropped, err)
		}
	}
}

// TestAFailedSetThatChangedNothingStaysPending: a failed further step whose
// read-back shows the step before is not restored at once, as it may still
// land; once settled as never taken, the prior entry comes back exactly.
func TestAFailedSetThatChangedNothingStaysPending(t *testing.T) {
	f := newMem()
	f.vals["s"] = 10
	a := NewActing("n", MemoryLedger(), f)
	prior, err := a.Apply(context.Background(), wl("s"), "fake", 7, time.Unix(50, 0), "d0")
	if err != nil {
		t.Fatal(err)
	}
	f.setErr = errors.New("conflict")
	if _, err := a.Apply(context.Background(), wl("s"), "fake", 5, time.Unix(99, 0), "d1"); !errors.Is(err, ErrLeftPending) {
		t.Fatalf("err %v; want the step left pending", err)
	}
	got := a.ledger.All()
	if len(got) != 1 || !got[0].Pending || got[0].PrevDecisionID != "d0" {
		t.Fatalf("ledger %+v; want the step pending over the prior one", got)
	}
	if _, err := a.ResolveNeverTook(context.Background(), wl("s"), got[0]); err != nil {
		t.Fatal(err)
	}
	if got := a.ledger.All(); len(got) != 1 || got[0] != prior {
		t.Fatalf("ledger %+v; want the prior entry %+v back once settled", got, prior)
	}
}

// TestHeldResolvesAPendingEntry: an entry written ahead and never committed, as
// after a crash, is settled by what reads back. One that reads as before its
// Apply is not judged by Held: it is returned as NeverTook and left pending, and
// ResolveNeverTook settles it, back to the step before or to none.
func TestHeldResolvesAPendingEntry(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name      string
		e         Entry
		value     float64
		neverTook bool
		left      int // entries left in the ledger once settled
		applied   float64
	}{
		{"first step, crashed before Set", Entry{Original: 10, OriginalSet: true, Applied: 5}, 10, true, 0, 0},
		{"first step, crashed after Set", Entry{Original: 10, OriginalSet: true, Applied: 5}, 5, false, 1, 5},
		{"further step, crashed before Set", Entry{Original: 10, OriginalSet: true, Applied: 5, PrevApplied: 7, PrevSet: true}, 7, true, 1, 7},
		{"further step, crashed after Set", Entry{Original: 10, OriginalSet: true, Applied: 5, PrevApplied: 7, PrevSet: true}, 5, false, 1, 5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newMem()
			f.vals["s"] = c.value
			l := MemoryLedger()
			c.e.Action, c.e.Subject, c.e.NodeID, c.e.Pending = "fake", "s", "n", true
			l.Put(c.e)
			a := NewActing("n", l, f)
			r, err := a.Held(ctx, "fake", []runtime.Workload{wl("s")})
			if err != nil || len(r.Dropped) != 0 || (len(r.NeverTook) == 1) != c.neverTook || (len(r.Held) == 1) == c.neverTook {
				t.Fatalf("reconciled %+v err %v", r, err)
			}
			if c.neverTook {
				if got := l.All(); len(got) != 1 || got[0] != c.e {
					t.Fatalf("ledger %+v; want the entry left pending for the caller", got)
				}
				if _, err := a.ResolveNeverTook(ctx, wl("s"), r.NeverTook[0]); err != nil {
					t.Fatal(err)
				}
			}
			if len(l.All()) != c.left {
				t.Fatalf("ledger %+v; want %d entries", l.All(), c.left)
			}
			if c.left == 1 {
				if e := l.All()[0]; e.Pending || e.Applied != c.applied || e.PrevSet {
					t.Fatalf("entry %+v; want committed at %v", e, c.applied)
				}
			}
		})
	}
}

// flakyRead is quantised and fails the read that follows its next Set.
type flakyRead struct {
	quantised
	failNext *bool
}

func (f flakyRead) Set(ctx context.Context, w runtime.Workload, v float64, set bool) error {
	*f.failNext = true
	return f.quantised.Set(ctx, w, v, set)
}

func (f flakyRead) Read(ctx context.Context, w runtime.Workload) (float64, bool, error) {
	if *f.failNext {
		*f.failNext = false
		return 0, false, errors.New("api unavailable")
	}
	return f.quantised.Read(ctx, w)
}

func TestAQuantisedValueWhoseReadBackFailedIsSettledByHeld(t *testing.T) {
	var fail bool
	a := NewActing("n", MemoryLedger(), flakyRead{quantised{newMem()}, &fail})
	ctx := context.Background()
	e, err := a.Apply(ctx, wl("s"), "fake", 10000, time.Time{}, "")
	if err != nil || !e.Pending {
		t.Fatalf("%+v %v; want the change left pending", e, err)
	}
	held, dropped, _, err := held4(a, ctx, "fake", []runtime.Workload{wl("s")})
	if err != nil || len(held) != 1 || held[0].Applied != 8192 || held[0].Pending || len(dropped) != 0 {
		t.Fatalf("held %+v dropped %+v err %v; want the quantised value adopted", held, dropped, err)
	}
}

// TestHeldReconcilesEachEntryOnItsOwn: one entry whose read fails does not
// stop the others being held or dropped.
func TestHeldReconcilesEachEntryOnItsOwn(t *testing.T) {
	f := &subjectReadErr{memAct: newMem(), fail: map[string]bool{}}
	a := NewActing("n", MemoryLedger(), f)
	ctx := context.Background()
	for _, s := range []string{"a", "b", "c"} {
		if _, err := a.Apply(ctx, wl(s), "fake", 5, time.Time{}, ""); err != nil {
			t.Fatal(err)
		}
	}
	f.fail["b"] = true
	f.gone["c"] = true
	held, dropped, failed, err := held4(a, ctx, "fake", []runtime.Workload{wl("a"), wl("b"), wl("c")})
	if err != nil || len(held) != 1 || held[0].Subject != "a" || len(dropped) != 1 || dropped[0].Subject != "c" ||
		len(failed) != 1 || failed[0].Entry.Subject != "b" {
		t.Fatalf("held %+v dropped %+v failed %+v err %v", held, dropped, failed, err)
	}
	if n := len(a.ledger.All()); n != 2 {
		t.Fatalf("%d entries; want a and b kept", n)
	}
}

// subjectReadErr is memAct whose Read fails for the subjects in fail.
type subjectReadErr struct {
	*memAct
	fail map[string]bool
}

func (r *subjectReadErr) Read(ctx context.Context, w runtime.Workload) (float64, bool, error) {
	if r.fail[w.Subject] {
		return 0, false, errors.New("api unavailable")
	}
	return r.memAct.Read(ctx, w)
}

func TestHeldTouchesOnlyItsAction(t *testing.T) {
	l := MemoryLedger()
	l.Put(Entry{Action: "other", Subject: "s", Applied: 5, NodeID: "n"})
	a := NewActing("n", l, newMem())
	if _, dropped, _, err := held4(a, context.Background(), "fake", nil); err != nil || len(dropped) != 0 || len(l.All()) != 1 {
		t.Fatalf("dropped %+v err %v ledger %+v; another action's entry must be left alone", dropped, err, l.All())
	}
}

// TestRestoreIsNotGatedByCheck: putting back an earlier value of the agent's own
// change never asks Check, which gates only cuts; with nothing held there is
// nothing to restore.
func TestRestoreIsNotGatedByCheck(t *testing.T) {
	m := newMem()
	m.vals["s"] = 10
	l := MemoryLedger()
	if _, err := NewActing("n", l, m).Apply(context.Background(), wl("s"), "fake", 7, time.Time{}, "d0"); err != nil {
		t.Fatal(err)
	}
	a := NewActing("n", l, refusing{m})
	if _, err := a.Apply(context.Background(), wl("s"), "fake", 5, time.Time{}, "d1"); err == nil {
		t.Fatal("Apply past a refusing Check")
	}
	e, err := a.Restore(context.Background(), wl("s"), "fake", 8, time.Time{}, "d0")
	if err != nil || e.Applied != 8 || e.Original != 10 || m.vals["s"] != 8 {
		t.Fatalf("entry %+v err %v value %v; want 8 restored with the first original", e, err, m.vals["s"])
	}
	if _, err := a.Restore(context.Background(), wl("t"), "fake", 8, time.Time{}, "d0"); err == nil {
		t.Fatal("Restore with nothing held succeeded")
	}
}

// TestUnmanagedSaysWhy: every ledger entry this Acting does not give back is
// listed, with why: another node's, an action with no actuator, and one a real
// run holds while this is a dry run.
func TestUnmanagedSaysWhy(t *testing.T) {
	l := MemoryLedger()
	for _, e := range []Entry{
		{Action: "fake", Subject: "mine", NodeID: "n", Applied: 1},
		{Action: "fake", Subject: "theirs", NodeID: "m", Applied: 1},
		{Action: "removed", Subject: "s", NodeID: "n", Applied: 1},
	} {
		if err := l.Put(e); err != nil {
			t.Fatal(err)
		}
	}
	a := NewActing("n", l, newMem())
	a.HeldByRealRun([]Entry{{Action: "fake", Subject: "real", NodeID: "n", Applied: 2}})
	got := map[string]string{}
	for _, u := range a.Unmanaged() {
		got[u.Subject] = u.Why
	}
	want := map[string]string{
		"theirs": "recorded by node m; this agent is n; not managed",
		"s":      "no lever runs for this action",
		"real":   "held by a real run and not given back while dry_run is on",
	}
	for s, w := range want {
		if !strings.Contains(got[s], w) {
			t.Errorf("%s: why %q; want it to say %q", s, got[s], w)
		}
	}
	if _, ok := got["mine"]; ok || len(got) != 3 {
		t.Errorf("unmanaged %v; want exactly the three", got)
	}
}

// lagging is memAct whose Set takes effect only after lag more reads, as a
// kubelet that applies an accepted resize a moment later: until then Read
// returns the setting from before.
type lagging struct {
	*memAct
	lag     int
	left    int
	pending *memSet
}

func (l *lagging) Set(ctx context.Context, w runtime.Workload, v float64, set bool) error {
	if l.pending != nil && *l.pending == (memSet{w.Subject, v, set}) {
		return nil // asked again for what is on its way: the kubelet does not start over
	}
	l.pending, l.left = &memSet{w.Subject, v, set}, l.lag
	return nil
}

func (l *lagging) Read(ctx context.Context, w runtime.Workload) (float64, bool, error) {
	if l.pending != nil {
		if l.left == 0 {
			p := *l.pending
			l.pending = nil
			if err := l.memAct.Set(ctx, w, p.v, p.set); err != nil {
				return 0, false, err
			}
		} else {
			l.left--
		}
	}
	return l.memAct.Read(ctx, w)
}

// TestASetThatReadsBackAsBeforeStaysPending: a Set that succeeded and reads back
// as the setting before it, not the value asked, has not been seen to take: the
// entry stays pending, and is never committed with the original as Applied.
func TestASetThatReadsBackAsBeforeStaysPending(t *testing.T) {
	m := newMem()
	m.vals["s"] = 10
	a := NewActing("n", MemoryLedger(), &lagging{memAct: m, lag: 2})
	e, err := a.Apply(context.Background(), wl("s"), "fake", 5, time.Time{}, "d")
	if err != nil || !e.Pending || e.Applied != 5 {
		t.Fatalf("entry %+v err %v; want pending at the 5 asked for", e, err)
	}
	if got := a.ledger.All(); len(got) != 1 || !got[0].Pending || got[0].Applied != 5 {
		t.Fatalf("ledger %+v; want the step pending, never Applied=Original", got)
	}
}

// TestARevertIsConfirmedBeforeTheEntryGoes: a revert deletes the entry only once
// the original reads back; one that has not yet taken is an error, and the entry
// stays for the next try.
func TestARevertIsConfirmedBeforeTheEntryGoes(t *testing.T) {
	m := newMem()
	m.vals["s"] = 10
	l := MemoryLedger()
	e, err := NewActing("n", l, m).Apply(context.Background(), wl("s"), "fake", 5, time.Time{}, "d")
	if err != nil {
		t.Fatal(err)
	}
	a := NewActing("n", l, &lagging{memAct: m, lag: 2})
	if err := a.Revert(context.Background(), wl("s"), e); err == nil || errors.Is(err, ErrGone) {
		t.Fatalf("revert err %v; want not yet confirmed", err)
	}
	if len(l.All()) != 1 {
		t.Fatal("the entry went before the original read back")
	}
	for i := 0; i < 3 && len(l.All()) == 1; i++ {
		a.Revert(context.Background(), wl("s"), e)
	}
	if len(l.All()) != 0 || m.vals["s"] != 10 {
		t.Fatalf("ledger %+v value %v; want the entry gone once 10 read back", l.All(), m.vals["s"])
	}
}

// failSetBlindRead fails every Set without effect and fails the read after it
// once, as a resize whose call errored and whose read-back timed out.
type failSetBlindRead struct {
	*memAct
	failNext bool
}

func (f *failSetBlindRead) Set(ctx context.Context, w runtime.Workload, v float64, set bool) error {
	f.failNext = true
	return errors.New("conflict")
}

func (f *failSetBlindRead) Read(ctx context.Context, w runtime.Workload) (float64, bool, error) {
	if f.failNext {
		f.failNext = false
		return 0, false, errors.New("api unavailable")
	}
	return f.memAct.Read(ctx, w)
}

// TestApplySaysWhenItLeftAStepPending: Apply's error says whether this step was
// left pending (its outcome unknown, so possibly in force); a step restored to
// the entry before it was not, even when that entry was itself pending.
func TestApplySaysWhenItLeftAStepPending(t *testing.T) {
	m := newMem()
	m.vals["s"] = 10
	a := NewActing("n", MemoryLedger(), &failSetBlindRead{memAct: m})
	_, err := a.Apply(context.Background(), wl("s"), "fake", 5, time.Time{}, "d")
	if !errors.Is(err, ErrLeftPending) {
		t.Fatalf("err %v; want ErrLeftPending for a step whose outcome is unknown", err)
	}
	l := MemoryLedger()
	prior := Entry{Action: "fake", Subject: "s", NodeID: "n", Original: 10, OriginalSet: true, Applied: 7, Pending: true}
	if err := l.Put(prior); err != nil {
		t.Fatal(err)
	}
	m.vals["s"] = 10
	f := newMem()
	f.vals["s"] = 10
	f.setErr = errors.New("conflict")
	_, err = NewActing("n", l, f).Apply(context.Background(), wl("s"), "fake", 5, time.Time{}, "d2")
	if err == nil || errors.Is(err, ErrLeftPending) {
		t.Fatalf("err %v; want the step adopted at what reads back (it moved), not left pending", err)
	}
}

// TestHeldReturnsWhatItDroppedBeforeASaveFailed: a save that fails partway
// through Held's deletions returns the entries already dropped with the error,
// so the caller can record them.
func TestHeldReturnsWhatItDroppedBeforeASaveFailed(t *testing.T) {
	f := newMem()
	l := MemoryLedger()
	a := NewActing("n", l, f)
	ctx := context.Background()
	for _, s := range []string{"a", "b", "c"} {
		if _, err := a.Apply(ctx, wl(s), "fake", 5, time.Time{}, ""); err != nil {
			t.Fatal(err)
		}
		f.gone[s] = true
	}
	boom := errors.New("disk full")
	testhooks.LedgerSave = testhooks.FailAfter(1, boom)
	t.Cleanup(func() { testhooks.LedgerSave = nil })
	_, dropped, _, err := held4(a, ctx, "fake", []runtime.Workload{wl("x")})
	if !errors.Is(err, boom) || len(dropped) != 1 || len(l.All()) != 2 {
		t.Fatalf("dropped %+v err %v ledger %+v; want the one deleted before the failure returned with it", dropped, err, l.All())
	}
	for _, e := range l.All() {
		if e.Subject == dropped[0].Subject {
			t.Fatalf("%s returned as dropped and still in the ledger", e.Subject)
		}
	}
}

// held4 is Held's result as held, dropped (their entries), failed and err.
func held4(a *Acting, ctx context.Context, action string, ws []runtime.Workload) ([]Entry, []Entry, []Unread, error) {
	r, err := a.Held(ctx, action, ws)
	var dropped []Entry
	for _, d := range r.Dropped {
		dropped = append(dropped, d.Entry)
	}
	return r.Held, dropped, r.Failed, err
}

// TestANeverTookStepRestoresTheEarlierStepsRecord: a further step that never
// took goes back to the step before it as that step was recorded: its decision
// and its expiry, not the failed step's.
func TestANeverTookStepRestoresTheEarlierStepsRecord(t *testing.T) {
	m := newMem()
	m.vals["s"] = 10
	l := MemoryLedger()
	first, err := NewActing("n", l, m).Apply(context.Background(), wl("s"), "fake", 7, time.Unix(100, 0), "d1")
	if err != nil {
		t.Fatal(err)
	}
	a := NewActing("n", l, &failSetBlindRead{memAct: m})
	if _, err := a.Apply(context.Background(), wl("s"), "fake", 5, time.Unix(200, 0), "d2"); !errors.Is(err, ErrLeftPending) {
		t.Fatalf("err %v; want the further step left pending", err)
	}
	p := l.All()[0]
	if !p.Pending || p.PrevDecisionID != "d1" || !p.PrevExpires.Equal(time.Unix(100, 0)) {
		t.Fatalf("pending entry %+v; want the step before's decision and expiry recorded", p)
	}
	restored, err := a.ResolveNeverTook(context.Background(), wl("s"), p)
	if err != nil {
		t.Fatal(err)
	}
	if restored != first || l.All()[0] != first {
		t.Fatalf("restored %+v, ledger %+v; want the first step's record %+v back", restored, l.All(), first)
	}
}

// TestResolveNeverTookRereadsFirst: a change that has landed since Held read it
// is not settled as never taken: it stays pending.
func TestResolveNeverTookRereadsFirst(t *testing.T) {
	m := newMem()
	m.vals["s"] = 10
	l := MemoryLedger()
	a := NewActing("n", l, &failSetBlindRead{memAct: m})
	if _, err := a.Apply(context.Background(), wl("s"), "fake", 5, time.Time{}, "d"); !errors.Is(err, ErrLeftPending) {
		t.Fatal(err)
	}
	p := l.All()[0]
	m.vals["s"] = 5 // it lands
	if _, err := a.ResolveNeverTook(context.Background(), wl("s"), p); !errors.Is(err, ErrNotSettled) {
		t.Fatalf("err %v; want ErrNotSettled for a change that has landed", err)
	}
	if got := l.All(); len(got) != 1 || !got[0].Pending {
		t.Fatalf("ledger %+v; want it still pending", got)
	}
}
