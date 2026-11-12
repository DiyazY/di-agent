package agentmain

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/statemap"
)

type paceFixture struct {
	t        *testing.T
	m        *statemap.Map
	j        *statemap.Journal
	p        *Pace
	t0, now  time.Time
	phases   []string
	onPhases func() // called by LeverPhases before it reads phases, when set
	pending  int
	due      int
	logs     []string
}

func newPaceFixture(t *testing.T) *paceFixture {
	t.Helper()
	f := &paceFixture{t: t, t0: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	f.now = f.t0
	clock := func() time.Time { return f.now }
	f.j = statemap.NewJournal(0)
	f.m = statemap.New(statemap.Config{AdmitUnknown: true}, f.j)
	f.m.SetClock(clock)
	f.p = NewPace(f.m, PaceInputs{
		LeverPhases: func() []string {
			if f.onPhases != nil {
				f.onPhases()
			}
			return f.phases
		},
		PendingAccepts: func() int { return f.pending },
		ReviewDue:      func() int { return f.due },
		LastAdmitted:   f.m.LastAdmitted,
	}, clock, func(format string, args ...any) { f.logs = append(f.logs, fmt.Sprintf(format, args...)) })
	return f
}

func (f *paceFixture) minutes(n int) {
	for i := 0; i < n; i++ {
		f.now = f.now.Add(paceTick)
		f.p.Tick()
	}
}

func (f *paceFixture) changes() []statemap.Event {
	var out []statemap.Event
	for _, e := range f.j.Events(0, 0) {
		if e.Kind == statemap.EventPaceChanged {
			out = append(out, e)
		}
	}
	return out
}

func (f *paceFixture) wantPace(want string) {
	f.t.Helper()
	if got, _ := f.p.Current(); got != want {
		f.t.Fatalf("pace at %s = %q, want %q", f.now.Sub(f.t0), got, want)
	}
	if got := f.m.Pace(); got != want {
		f.t.Fatalf("map pace at %s = %q, want %q", f.now.Sub(f.t0), got, want)
	}
}

func (f *paceFixture) lastChange() statemap.Event {
	f.t.Helper()
	ch := f.changes()
	if len(ch) == 0 {
		f.t.Fatal("no pace.changed event")
	}
	return ch[len(ch)-1]
}

func TestPaceStartsBase(t *testing.T) {
	f := newPaceFixture(t)
	pace, since := f.p.Current()
	if pace != paceBase || !since.Equal(f.m.PaceSince()) {
		t.Fatalf("Current = (%q, %s), want (base, %s)", pace, since, f.m.PaceSince())
	}
	if n := len(f.changes()); n != 0 {
		t.Fatalf("changes = %d, want 0", n)
	}
}

func TestPaceGoesSlowAfterExactlyCalmFor(t *testing.T) {
	f := newPaceFixture(t)
	f.minutes(9)
	f.wantPace(paceBase)
	f.minutes(1)
	f.wantPace(paceSlow)
	ch := f.changes()
	if len(ch) != 1 {
		t.Fatalf("changes = %d, want 1", len(ch))
	}
	if r := ch[0].Detail["reason"]; r != "calm for 10m0s" {
		t.Fatalf("reason = %v", r)
	}
	if !ch[0].At.Equal(f.t0.Add(10 * time.Minute)) {
		t.Fatalf("At = %s, want t0+10m", ch[0].At)
	}
	if _, since := f.p.Current(); !since.Equal(f.t0.Add(10 * time.Minute)) {
		t.Fatalf("since = %s, want t0+10m", since)
	}
}

func TestLeverRelievingHoldsBase(t *testing.T) {
	f := newPaceFixture(t)
	f.phases = []string{"calm", "relieving"}
	f.minutes(20)
	f.wantPace(paceBase)
	if n := len(f.changes()); n != 0 {
		t.Fatalf("changes = %d, want 0", n)
	}
	f.phases = []string{"calm", "calm"}
	f.p.Tick() // calm observed at t0+20m
	f.minutes(9)
	f.wantPace(paceBase)
	f.minutes(1)
	f.wantPace(paceSlow)
	if at := f.lastChange().At; !at.Equal(f.t0.Add(30 * time.Minute)) {
		t.Fatalf("slow at %s, want t0+30m", at.Sub(f.t0))
	}
}

func TestLeverLeavingCalmReturnsToBase(t *testing.T) {
	f := newPaceFixture(t)
	f.minutes(10)
	f.wantPace(paceSlow)
	f.phases = []string{"releasing"}
	f.minutes(1)
	f.wantPace(paceBase)
	if r := f.lastChange().Detail["reason"]; r != "relief releasing" {
		t.Fatalf("reason = %v", r)
	}
}

func TestNoLeversIsVacuouslyCalm(t *testing.T) {
	f := newPaceFixture(t)
	f.phases, f.pending, f.due = nil, 0, 0
	f.minutes(9)
	f.wantPace(paceBase)
	f.minutes(1)
	f.wantPace(paceSlow)
}

func TestPendingAcceptHoldsBase(t *testing.T) {
	f := newPaceFixture(t)
	f.pending = 1
	f.minutes(20)
	f.wantPace(paceBase)
	f.pending = 0
	f.p.Tick()
	f.minutes(9)
	f.wantPace(paceBase)
	f.minutes(1)
	f.wantPace(paceSlow)
}

func TestPendingAcceptReturnsToBase(t *testing.T) {
	f := newPaceFixture(t)
	f.minutes(10)
	f.wantPace(paceSlow)
	f.pending = 1
	f.minutes(1)
	f.wantPace(paceBase)
	if r := f.lastChange().Detail["reason"]; r != "accept pending" {
		t.Fatalf("reason = %v", r)
	}
}

func TestReviewDueHoldsBase(t *testing.T) {
	f := newPaceFixture(t)
	f.due = 1
	f.minutes(20)
	f.wantPace(paceBase)
	f.due = 0
	f.p.Tick()
	f.minutes(9)
	f.wantPace(paceBase)
	f.minutes(1)
	f.wantPace(paceSlow)
}

func TestReviewDueReturnsToBase(t *testing.T) {
	f := newPaceFixture(t)
	f.minutes(10)
	f.wantPace(paceSlow)
	f.due = 2
	f.minutes(1)
	f.wantPace(paceBase)
	if r := f.lastChange().Detail["reason"]; r != "review due" {
		t.Fatalf("reason = %v", r)
	}
}

func TestCalmStartsAtTheLastAdmission(t *testing.T) {
	f := newPaceFixture(t)
	f.minutes(3)
	if err := f.m.Record(statemap.Observation{ID: "new", Value: 1, At: f.now}); err != nil {
		t.Fatal(err)
	}
	f.minutes(9)
	f.wantPace(paceBase)
	f.minutes(1)
	f.wantPace(paceSlow)
	if at := f.lastChange().At; !at.Equal(f.t0.Add(13 * time.Minute)) {
		t.Fatalf("slow at %s, want t0+13m", at.Sub(f.t0))
	}
}

func TestCalmCannotBeginBeforeAnAdmissionThePaceDidNotSee(t *testing.T) {
	f := &paceFixture{t: t, t0: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	f.now = f.t0
	clock := func() time.Time { return f.now }
	f.j = statemap.NewJournal(0)
	f.m = statemap.New(statemap.Config{AdmitUnknown: true}, f.j)
	f.m.SetClock(clock)
	f.now = f.t0.Add(2 * time.Minute)
	// admitted before the pace exists: no hook saw it
	if err := f.m.Record(statemap.Observation{ID: "early", Value: 1, At: f.now}); err != nil {
		t.Fatal(err)
	}
	f.now = f.t0
	f.p = NewPace(f.m, PaceInputs{LastAdmitted: f.m.LastAdmitted}, clock, func(string, ...any) {})
	f.minutes(11) // t0+11m: calm from t0+2m, only 9m
	f.wantPace(paceBase)
	f.minutes(1)
	f.wantPace(paceSlow)
}

func TestAdmissionWakesAtOnce(t *testing.T) {
	f := newPaceFixture(t)
	f.minutes(10)
	f.wantPace(paceSlow)
	if err := f.m.Record(statemap.Observation{ID: "late", Value: 1, At: f.now}); err != nil {
		t.Fatal(err)
	}
	f.wantPace(paceBase)
	if r := f.lastChange().Detail["reason"]; r != "property admitted: late" {
		t.Fatalf("reason = %v", r)
	}
}

func TestWakeReturnsToBaseAndJournalsOnce(t *testing.T) {
	f := newPaceFixture(t)
	f.minutes(10)
	f.wantPace(paceSlow)
	f.now = f.now.Add(10 * time.Second)
	f.p.Wake("request:POST /tasks")
	f.wantPace(paceBase)
	ch := f.changes()
	if len(ch) != 2 {
		t.Fatalf("changes = %d, want 2", len(ch))
	}
	last := ch[1]
	if last.Detail["reason"] != "request:POST /tasks" || !last.At.Equal(f.now) {
		t.Fatalf("last change = %+v", last)
	}
	_, since := f.p.Current()
	f.now = f.now.Add(10 * time.Second)
	f.p.Wake("again")
	if n := len(f.changes()); n != 2 {
		t.Fatalf("changes after second wake = %d, want 2", n)
	}
	if _, s := f.p.Current(); !s.Equal(since) {
		t.Fatalf("since moved: %s → %s", since, s)
	}
}

func TestWakeRestartsTheCalmCount(t *testing.T) {
	f := newPaceFixture(t)
	f.minutes(5)
	f.p.Wake("x")
	if n := len(f.changes()); n != 0 {
		t.Fatalf("wake at base journalled %d changes", n)
	}
	f.minutes(9)
	f.wantPace(paceBase)
	f.minutes(1)
	f.wantPace(paceSlow)
	if at := f.lastChange().At; !at.Equal(f.t0.Add(15 * time.Minute)) {
		t.Fatalf("slow at %s, want t0+15m", at.Sub(f.t0))
	}
}

func TestIntervalIsFourTimesUnderSlow(t *testing.T) {
	f := newPaceFixture(t)
	if got := f.p.Interval(10 * time.Second); got != 10*time.Second {
		t.Fatalf("base interval = %s", got)
	}
	f.minutes(10)
	if got := f.p.Interval(10 * time.Second); got != 40*time.Second {
		t.Fatalf("slow interval = %s", got)
	}
	if got := (*Pace)(nil).Interval(10 * time.Second); got != 10*time.Second {
		t.Fatalf("nil interval = %s", got)
	}
}

func TestWakeDuringTheReadWins(t *testing.T) {
	f := newPaceFixture(t)
	f.minutes(9)
	woke := false
	f.onPhases = func() {
		if !woke {
			woke = true
			f.p.Wake("offer") // would deadlock if the inputs were read under the pace lock
		}
	}
	f.minutes(1)
	f.wantPace(paceBase)
	if !woke {
		t.Fatal("the input was not read")
	}
	f.minutes(9)
	f.wantPace(paceBase)
	f.minutes(1)
	f.wantPace(paceSlow)
	if at := f.lastChange().At; !at.Equal(f.t0.Add(20 * time.Minute)) {
		t.Fatalf("slow at %s, want t0+20m", at.Sub(f.t0))
	}
	if n := len(f.changes()); n != 1 {
		t.Fatalf("changes = %d, want 1 (only the final slow)", n)
	}
}

func TestClockStepForwardCompletesCalm(t *testing.T) {
	f := newPaceFixture(t)
	f.minutes(1)
	f.now = f.now.Add(time.Hour)
	f.p.Tick()
	f.wantPace(paceSlow)
}

func TestClockStepBackwardRestartsCalm(t *testing.T) {
	f := newPaceFixture(t)
	f.minutes(5)
	f.now = f.now.Add(-time.Hour)
	f.p.Tick()
	f.wantPace(paceBase)
	f.minutes(9)
	f.wantPace(paceBase)
	f.minutes(1)
	f.wantPace(paceSlow)
}

func TestPaceAllowed(t *testing.T) {
	for _, c := range []struct {
		interval, stale time.Duration
		ok              bool
	}{
		{10 * time.Second, 2 * time.Minute, true},
		{30 * time.Second, 2 * time.Minute, false},
		{0, 2 * time.Minute, true},
		{time.Minute, 5 * time.Minute, true},  // 4m < 5m (the brief's table said false; its formula says true)
		{time.Minute, 4 * time.Minute, false}, // 4m ≥ 4m: equality pins
		{29 * time.Second, 2 * time.Minute, true},
	} {
		ok, why := paceAllowed(c.interval, c.stale)
		if ok != c.ok {
			t.Errorf("paceAllowed(%s, %s) = %v, want %v", c.interval, c.stale, ok, c.ok)
		}
		if !ok && !strings.Contains(why, "stale_after") {
			t.Errorf("paceAllowed(%s, %s) why = %q", c.interval, c.stale, why)
		}
	}
}

func TestPinnedPaceNeverGoesSlow(t *testing.T) {
	f := newPaceFixture(t)
	f.p.Pin("sources.interval 30s × 4 ≥ map.stale_after 2m0s")
	f.minutes(30)
	f.wantPace(paceBase)
	if n := len(f.changes()); n != 0 {
		t.Fatalf("changes = %d, want 0", n)
	}
	n := 0
	for _, l := range f.logs {
		if strings.Contains(l, "never slow") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("never-slow log lines = %d, want 1: %q", n, f.logs)
	}
}

func TestWakefulWakesOnMutationsOnly(t *testing.T) {
	f := newPaceFixture(t)
	mux := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := httptest.NewServer(f.p.Wakeful(mux))
	defer srv.Close()
	do := func(method, path string) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s = %d", method, path, resp.StatusCode)
		}
	}
	f.minutes(10)
	f.wantPace(paceSlow)
	do(http.MethodGet, "/state")
	f.wantPace(paceSlow)
	// A pushed sample is telemetry, not attention: a node fed by samples can still slow.
	do(http.MethodPost, "/ingest-sample")
	f.wantPace(paceSlow)
	do(http.MethodPost, "/tasks")
	f.wantPace(paceBase)
	if r := f.lastChange().Detail["reason"]; r != "request:POST /tasks" {
		t.Fatalf("reason = %v", r)
	}
	n := len(f.changes())
	do(http.MethodOptions, "/x")
	if len(f.changes()) != n {
		t.Fatal("OPTIONS changed the pace")
	}
}

func TestRunReturnsOnCancel(t *testing.T) {
	f := newPaceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.p.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return within 1s of cancel")
	}
}

func TestWaitStopsOnCancelAndIsNilSafe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	if !(*Pace)(nil).wait(ctx, time.Millisecond) {
		t.Fatal("nil wait stopped early")
	}
	cancel()
	if (*Pace)(nil).wait(ctx, time.Hour) {
		t.Fatal("wait on a cancelled context reported continue")
	}
}
