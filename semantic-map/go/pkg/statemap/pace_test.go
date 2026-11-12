package statemap

import (
	"reflect"
	"testing"
	"time"
)

func countKind(m *Map, k EventKind) int {
	n := 0
	for _, e := range m.Journal().Events(0, 0) {
		if e.Kind == k {
			n++
		}
	}
	return n
}

func TestNewMapIsAtBasePace(t *testing.T) {
	m, _ := newTestMap(t, Config{})
	if m.Pace() != PaceBase {
		t.Fatalf("pace = %q, want base", m.Pace())
	}
	if m.PaceSince().IsZero() {
		t.Fatal("PaceSince is zero")
	}
	if got := m.State(Query{}).Pace; got != "base" {
		t.Fatalf("State().Pace = %q", got)
	}
	if countKind(m, EventPaceChanged) != 0 {
		t.Fatal("a new map journalled pace.changed")
	}
}

func TestSetPaceJournalsOnlyChanges(t *testing.T) {
	m, c := newTestMap(t, Config{})
	c.advance(time.Minute)
	rev := m.Revision()
	if !m.SetPace(PaceSlow, "calm for 10m0s") {
		t.Fatal("SetPace to a new pace returned false")
	}
	e := lastEvent(m)
	if e.Kind != EventPaceChanged || e.Target != "attention" || e.Actor != "agent" || !e.At.Equal(c.t) {
		t.Fatalf("event = %+v", e)
	}
	if e.Detail["from"] != "base" || e.Detail["to"] != "slow" || e.Detail["reason"] != "calm for 10m0s" {
		t.Fatalf("detail = %v", e.Detail)
	}
	if m.Revision() != rev+1 {
		t.Fatalf("revision %d, want %d", m.Revision(), rev+1)
	}
	if !m.PaceSince().Equal(c.t) {
		t.Fatalf("PaceSince = %v", m.PaceSince())
	}
	n := len(m.Journal().Events(0, 0))
	if m.SetPace(PaceSlow, "again") {
		t.Fatal("SetPace to the same pace returned true")
	}
	if len(m.Journal().Events(0, 0)) != n {
		t.Fatal("an unchanged SetPace journalled")
	}
	v := m.State(Query{})
	if v.Pace != "slow" || !v.PaceSince.Equal(c.t) {
		t.Fatalf("view pace = %q since %v", v.Pace, v.PaceSince)
	}
}

func TestDecideRecordsThePace(t *testing.T) {
	m, _ := newTestMap(t, Config{})
	d := m.Decide("d1", "estimate", "q").Commit(nil)
	if d.Inputs["pace"] != "base" {
		t.Fatalf("inputs = %v", d.Inputs)
	}
	m.SetPace(PaceSlow, "x")
	d2 := m.Decide("d2", "estimate", "q").Commit(nil)
	if d2.Inputs["pace"] != "slow" {
		t.Fatalf("inputs = %v", d2.Inputs)
	}
	got, ok := m.Journal().Decision("d2")
	if !ok || got.Inputs["pace"] != "slow" {
		t.Fatalf("journal copy = %+v ok=%v", got, ok)
	}
}

func TestLastAdmittedAndAdmitHook(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	if !m.LastAdmitted().IsZero() {
		t.Fatal("LastAdmitted not zero on a new map")
	}
	var got []string
	m.SetAdmitHook(func(id string) { got = append(got, id) })
	if err := m.Record(Observation{ID: "x", Value: 1, At: c.t}); err != nil {
		t.Fatal(err)
	}
	if !m.LastAdmitted().Equal(c.t) || !reflect.DeepEqual(got, []string{"x"}) {
		t.Fatalf("lastAdmitted %v got %v", m.LastAdmitted(), got)
	}
	if err := m.Record(Observation{ID: "x", Value: 1, At: c.t.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !m.LastAdmitted().Equal(c.t) {
		t.Fatalf("second record changed hook/lastAdmitted: %v %v", got, m.LastAdmitted())
	}
	m.SetAdmitHook(func(string) { panic("boom") })
	if err := m.Record(Observation{ID: "y", Value: 1, At: c.t}); err != nil {
		t.Fatalf("Record = %v, want nil", err)
	}
	if e := lastEvent(m); e.Kind != EventHookFailed || e.Target != "y" {
		t.Fatalf("last event = %+v", e)
	}
}

func TestAdmitHookRunsOutsideTheLock(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	m.SetAdmitHook(func(string) {
		m.SetPace(PaceBase, "admitted")
		_ = m.Pace()
	})
	done := make(chan struct{})
	go func() {
		_ = m.Record(Observation{ID: "z", Value: 1, At: c.t})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Record deadlocked with a hook that calls back into the map")
	}
}
