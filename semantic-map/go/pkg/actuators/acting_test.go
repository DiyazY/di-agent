package actuators

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

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
	f.setErr = errors.New("boom")
	if _, err := a.Apply(context.Background(), wl("s"), "fake", 1, time.Time{}, ""); err == nil {
		t.Fatal("want set error")
	}
	if len(a.ledger.All()) != 0 {
		t.Fatal("ledger recorded a failed Set")
	}
	f.setErr = nil
	e, _ := a.Apply(context.Background(), wl("s"), "fake", 2, time.Time{}, "")
	f.setErr = errors.New("boom")
	if _, err := a.Apply(context.Background(), wl("s"), "fake", 9, time.Time{}, ""); err == nil {
		t.Fatal("want set error")
	}
	if got := a.ledger.All(); len(got) != 1 || got[0] != e {
		t.Fatalf("prior entry changed: %+v", got)
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
	held, dropped, err := a.Held(ctx, "fake", []runtime.Workload{wl("keep"), wl("chg")})
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
	held, dropped, err := a.Held(ctx, "fake", nil)
	if err != nil || len(held) != 0 || len(dropped) != 1 || len(a.ledger.All()) != 0 {
		t.Fatalf("%+v %+v %v", held, dropped, err)
	}
}

func TestReconcileIgnoresOtherNode(t *testing.T) {
	f := newMem()
	l := MemoryLedger()
	l.Put(Entry{Action: "fake", Subject: "s", Applied: 5, NodeID: "other"})
	a := NewActing("n", l, f)
	held, dropped, err := a.Held(context.Background(), "fake", nil)
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
			a.Held(context.Background(), "fake", nil)
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
	held, dropped, err := a.Held(ctx, "fake", []runtime.Workload{wl("s")})
	if err != nil || len(held) != 1 || len(dropped) != 0 {
		t.Fatalf("%+v %+v %v", held, dropped, err)
	}
}

func TestReconcileSkipsAnActionWithNoActuator(t *testing.T) {
	l := MemoryLedger()
	l.Put(Entry{Action: "removed", Subject: "s", Applied: 5, NodeID: "n"})
	a := NewActing("n", l, newMem())
	held, dropped, err := a.Held(context.Background(), "removed", nil)
	if err != nil || len(held) != 0 || len(dropped) != 0 || len(l.All()) != 1 {
		t.Fatalf("held %+v dropped %+v err %v; want the entry kept, neither held nor dropped", held, dropped, err)
	}
	held, dropped, err = a.Held(context.Background(), "fake", nil)
	if err != nil || len(held) != 0 || len(dropped) != 0 || len(l.All()) != 1 {
		t.Fatalf("held %+v dropped %+v err %v; want the entry kept, neither held nor dropped", held, dropped, err)
	}
}

func TestReconcileFailsOnAReadError(t *testing.T) {
	f := &readErr{memAct: newMem()}
	a := NewActing("n", MemoryLedger(), f)
	ctx := context.Background()
	if _, err := a.Apply(ctx, wl("s"), "fake", 5, time.Time{}, ""); err != nil {
		t.Fatal(err)
	}
	f.fail = true
	if _, _, err := a.Held(ctx, "fake", []runtime.Workload{wl("s")}); err == nil {
		t.Fatal("want the read error")
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
		held, dropped, err := a.Held(context.Background(), "fake", []runtime.Workload{wl("s")})
		if err != nil || len(held) != 1 || len(dropped) != 0 {
			t.Fatalf("further=%v: held %+v dropped %+v err %v", further, held, dropped, err)
		}
	}
}

func TestAFailedSetThatChangedNothingRestoresThePriorEntry(t *testing.T) {
	f := newMem()
	f.vals["s"] = 10
	a := NewActing("n", MemoryLedger(), f)
	prior, err := a.Apply(context.Background(), wl("s"), "fake", 7, time.Unix(50, 0), "d0")
	if err != nil {
		t.Fatal(err)
	}
	f.setErr = errors.New("conflict")
	if _, err := a.Apply(context.Background(), wl("s"), "fake", 5, time.Unix(99, 0), "d1"); err == nil {
		t.Fatal("want the Set error")
	}
	if got := a.ledger.All(); len(got) != 1 || got[0] != prior {
		t.Fatalf("ledger %+v; want the prior entry %+v back", got, prior)
	}
}

// TestHeldResolvesAPendingEntry: an entry written ahead and never committed, as
// after a crash, is settled by what reads back.
func TestHeldResolvesAPendingEntry(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		e       Entry
		value   float64
		held    bool
		dropped bool
		left    int // entries left in the ledger
		applied float64
	}{
		{"first step, crashed before Set", Entry{Original: 10, OriginalSet: true, Applied: 5}, 10, false, false, 0, 0},
		{"first step, crashed after Set", Entry{Original: 10, OriginalSet: true, Applied: 5}, 5, true, false, 1, 5},
		{"further step, crashed before Set", Entry{Original: 10, OriginalSet: true, Applied: 5, PrevApplied: 7, PrevSet: true}, 7, true, false, 1, 7},
		{"further step, crashed after Set", Entry{Original: 10, OriginalSet: true, Applied: 5, PrevApplied: 7, PrevSet: true}, 5, true, false, 1, 5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newMem()
			f.vals["s"] = c.value
			l := MemoryLedger()
			c.e.Action, c.e.Subject, c.e.NodeID, c.e.Pending = "fake", "s", "n", true
			l.Put(c.e)
			a := NewActing("n", l, f)
			held, dropped, err := a.Held(ctx, "fake", []runtime.Workload{wl("s")})
			if err != nil || (len(held) == 1) != c.held || (len(dropped) == 1) != c.dropped || len(l.All()) != c.left {
				t.Fatalf("held %+v dropped %+v err %v ledger %+v", held, dropped, err, l.All())
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
	held, dropped, err := a.Held(ctx, "fake", []runtime.Workload{wl("s")})
	if err != nil || len(held) != 1 || held[0].Applied != 8192 || held[0].Pending || len(dropped) != 0 {
		t.Fatalf("held %+v dropped %+v err %v; want the quantised value adopted", held, dropped, err)
	}
}

func TestHeldChangesNothingWhenAReadFails(t *testing.T) {
	f := &readErr{memAct: newMem()}
	a := NewActing("n", MemoryLedger(), f)
	ctx := context.Background()
	for _, s := range []string{"a", "b"} {
		if _, err := a.Apply(ctx, wl(s), "fake", 5, time.Time{}, ""); err != nil {
			t.Fatal(err)
		}
	}
	f.fail = true
	// "a" is not listed and would be dropped; "b" cannot be read.
	if _, _, err := a.Held(ctx, "fake", []runtime.Workload{wl("b")}); err == nil {
		t.Fatal("want the read error")
	}
	if n := len(a.ledger.All()); n != 2 {
		t.Fatalf("%d entries; a failed reconcile must delete nothing", n)
	}
}

func TestHeldTouchesOnlyItsAction(t *testing.T) {
	l := MemoryLedger()
	l.Put(Entry{Action: "other", Subject: "s", Applied: 5, NodeID: "n"})
	a := NewActing("n", l, newMem())
	if _, dropped, err := a.Held(context.Background(), "fake", nil); err != nil || len(dropped) != 0 || len(l.All()) != 1 {
		t.Fatalf("dropped %+v err %v ledger %+v; another action's entry must be left alone", dropped, err, l.All())
	}
}
