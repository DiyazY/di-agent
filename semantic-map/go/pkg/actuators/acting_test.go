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
	if err := a.Revert(ctx, wl("s"), e); err != nil {
		t.Fatalf("ErrGone should be absorbed, got %v", err)
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
	held, dropped, err := a.Held(ctx, []runtime.Workload{wl("keep"), wl("chg")})
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
	held, dropped, err := a.Held(ctx, nil)
	if err != nil || len(held) != 0 || len(dropped) != 1 || len(a.ledger.All()) != 0 {
		t.Fatalf("%+v %+v %v", held, dropped, err)
	}
}

func TestReconcileIgnoresOtherNode(t *testing.T) {
	f := newMem()
	l := MemoryLedger()
	l.Put(Entry{Action: "fake", Subject: "s", Applied: 5, NodeID: "other"})
	a := NewActing("n", l, f)
	held, dropped, err := a.Held(context.Background(), nil)
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
			a.Held(context.Background(), nil)
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
