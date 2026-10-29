package relief

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/internal/testhooks"
	"github.com/DiyazY/di-agent/pkg/actuators"
	"github.com/DiyazY/di-agent/pkg/policy"
	"github.com/DiyazY/di-agent/pkg/runtime"
	"github.com/DiyazY/di-agent/pkg/spec"
	"github.com/DiyazY/di-agent/pkg/statemap"
)

const tick = 15 * time.Second

// harness is a loop on a dry-run runtime acting through cpu.limit, a map and a
// clock the test moves. Node usage is 0.8 on 4 cores; each pod named is given a
// share and a relationship of strength 1 to node pressure, so pod:a at 0.4 is
// predicted to cut pressure by 0.1 when stepped to 0.3.
type harness struct {
	t        *testing.T
	now      time.Time
	m        *statemap.Map
	rt       *runtime.DryRun
	actuator actuators.Actuator // cpu.limit over rt's inventory, under any wrapper
	ledger   *actuators.Ledger
	fl       *flaky // what the loop sees, when the fixture asks for a flaky one
	l        *Loop
	ctx      context.Context
	shares   map[string]float64 // each pod's share, as the fixture gave it
	strength float64            // of each share's relationship to pressure
	seen     runtime.Runtime    // the runtime the loop sees
	seenAct  actuators.Actuator // the actuator the loop acts through
	policy   *policy.Policy
	f        fixture
	logged   []string // what the loop logged; guarded by mu

	mu sync.Mutex // guards now against readers on other goroutines; written only by the test
}

type fixture struct {
	shares     map[string]float64
	strength   float64 // of each share's relationship to pressure; 0 means 1
	noFact     bool
	noPressure bool // no pressure reading at start; only with no shares, which relate to it
	flaky      bool
	policy     func(*spec.PolicySpec)
	noRelate   bool       // the shares relate to nothing: the map cannot rank or predict
	evidence   bool       // outcomes feed the map, as on a runtime that really changes it
	candidates Candidates // what the loop settles candidates through; nil for none
	budget     *Budget    // shared with another loop; nil for the loop's own
	// wrap is the runtime and actuator the loop sees, over the dry run and cpu.limit.
	wrap func(*runtime.DryRun, actuators.Actuator) (runtime.Runtime, actuators.Actuator)
}

// inventory is cpu.limit over the dry run's workloads: the value lives in
// memory, and a workload the dry run no longer has is gone, as the Kubernetes
// actuator says of a deleted pod.
type inventory struct {
	actuators.Actuator
	rt *runtime.DryRun
}

func (i inventory) has(ctx context.Context, subject string) bool {
	ws, _ := i.rt.Workloads(ctx)
	return hasSubject(ws, subject)
}

func (i inventory) Read(ctx context.Context, w runtime.Workload) (float64, bool, error) {
	if !i.has(ctx, w.Subject) {
		return 0, false, fmt.Errorf("%s: %w", w.Subject, actuators.ErrGone)
	}
	return i.Actuator.Read(ctx, w)
}

func (i inventory) Set(ctx context.Context, w runtime.Workload, v float64, set bool) error {
	if !i.has(ctx, w.Subject) {
		return fmt.Errorf("%s: %w", w.Subject, actuators.ErrGone)
	}
	return i.Actuator.Set(ctx, w, v, set)
}

// flaky is a runtime and an actuator that fail on request, and a runtime that
// can hide workloads from its listing, as an orchestrator does when its API is
// down or a pod is being replaced.
type flaky struct {
	*runtime.DryRun
	act     actuators.Actuator
	mu      sync.Mutex
	listErr int             // Workloads fails this many more times
	setErr  int             // Set fails this many more times
	stuck   map[string]bool // Set always fails for these workloads
	hide    map[string]bool
}

func (f *flaky) Describe() actuators.Descriptor  { return f.act.Describe() }
func (f *flaky) Check(w runtime.Workload) string { return f.act.Check(w) }
func (f *flaky) Read(ctx context.Context, w runtime.Workload) (float64, bool, error) {
	return f.act.Read(ctx, w)
}

func (f *flaky) Set(ctx context.Context, w runtime.Workload, v float64, set bool) error {
	f.mu.Lock()
	fail := f.setErr > 0
	if fail {
		f.setErr--
	}
	stuck := f.stuck[w.Subject]
	f.mu.Unlock()
	if fail {
		return errors.New("conflict")
	}
	if stuck {
		return errors.New("admission webhook denied the request")
	}
	return f.act.Set(ctx, w, v, set)
}

func (f *flaky) Workloads(ctx context.Context) ([]runtime.Workload, error) {
	f.mu.Lock()
	fail := f.listErr > 0
	if fail {
		f.listErr--
	}
	f.mu.Unlock()
	if fail {
		return nil, errors.New("api unavailable")
	}
	ws, err := f.DryRun.Workloads(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	out := ws[:0]
	for _, w := range ws {
		if !f.hide[w.Subject] {
			out = append(out, w)
		}
	}
	return out, err
}

func (f *flaky) set(fn func(*flaky)) { f.mu.Lock(); fn(f); f.mu.Unlock() }

func newHarness(t *testing.T, f fixture) *harness {
	t.Helper()
	h := &harness{t: t, now: t0, ctx: context.Background(), ledger: actuators.MemoryLedger()}
	h.m = newMap(t, h.clock)
	record(t, h.m, nodeUsage, "", 0.8, h.now)
	if !f.noPressure {
		record(t, h.m, contention, "", 0, h.now)
	}
	if !f.noFact {
		cores(t, h.m, 4)
	}
	rt, err := runtime.Build("dry-run", runtime.Env{NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	h.rt = rt.(*runtime.DryRun)
	h.actuator = inventory{Actuator: cpuActuator(nil), rt: h.rt}
	h.shares, h.strength = f.shares, f.strength
	if h.strength == 0 {
		h.strength = 1
	}
	for s, share := range f.shares {
		record(t, h.m, usage(s), s, share, h.now)
		if !f.noRelate {
			relate(t, h.m, s, h.strength)
		}
		h.rt.Put(pod(s))
	}
	var rt2 runtime.Runtime = h.rt
	act2 := h.actuator
	if f.flaky {
		h.fl = &flaky{DryRun: h.rt, act: h.actuator, hide: map[string]bool{}, stuck: map[string]bool{}}
		rt2, act2 = h.fl, h.fl
	}
	if f.wrap != nil {
		rt2, act2 = f.wrap(h.rt, h.actuator)
	}
	h.seen, h.seenAct, h.policy, h.f = rt2, act2, defaultPolicy(t, f.policy), f
	h.l = h.newLoop(spec.Default().Loops.Relief.Levers[0])
	return h
}

// newLoop is a loop over the harness's runtime, actuator, ledger and map with
// lever, as a restarted agent builds it.
func (h *harness) newLoop(lever spec.Lever) *Loop {
	h.t.Helper()
	l, err := New(Config{
		Interval: spec.Default().Loops.Relief.Interval, Lever: lever, Budget: h.f.budget, NodeID: "n1", Runtime: h.seen,
		Acting: clocked(actuators.NewActing("n1", h.ledger, h.seenAct), h.clock), Policy: h.policy, Map: h.m,
		Now: h.clock, Logf: h.logf, Evidence: h.f.evidence, Candidates: h.f.candidates,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return l
}

// logf logs through the test and keeps what was logged.
func (h *harness) logf(format string, args ...any) {
	h.t.Logf(format, args...)
	h.mu.Lock()
	h.logged = append(h.logged, fmt.Sprintf(format, args...))
	h.mu.Unlock()
}

// clocked is a with its write times read from now.
func clocked(a *actuators.Acting, now func() time.Time) *actuators.Acting {
	a.Now = now
	return a
}

func (h *harness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	h.now = h.now.Add(d)
	h.mu.Unlock()
}

// step moves the clock one interval, reads pressure p, and ticks.
func (h *harness) step(p float64) {
	h.t.Helper()
	h.advance(tick)
	record(h.t, h.m, contention, "", p, h.now)
	h.l.Tick(h.ctx)
}

// run steps at pressure p for d.
func (h *harness) run(d time.Duration, p float64) {
	h.t.Helper()
	for i := time.Duration(0); i < d; i += tick {
		h.step(p)
	}
}

// until steps at pressure p until cond holds, failing after limit.
func (h *harness) until(limit time.Duration, p float64, what string, cond func() bool) {
	h.t.Helper()
	for i := time.Duration(0); i < limit; i += tick {
		h.step(p)
		if cond() {
			return
		}
	}
	h.t.Fatalf("after %s at pressure %v: %s did not happen", limit, p, what)
}

// applied is the ledger's entries by subject: every change held, on any node.
func (h *harness) applied() map[string]actuators.Entry {
	out := map[string]actuators.Entry{}
	for _, e := range h.ledger.All() {
		out[e.Subject] = e
	}
	return out
}

// seed records a change as the ledger of an earlier run holds it, with the
// setting in force at what it applied.
func (h *harness) seed(e actuators.Entry) {
	h.t.Helper()
	if err := h.ledger.Put(e); err != nil {
		h.t.Fatal(err)
	}
	if err := h.actuator.Set(h.ctx, runtime.Workload{Subject: e.Subject}, e.Applied, true); err != nil {
		h.t.Fatal(err)
	}
}

// limit is the subject's CPU limit as the actuator reads it, nil for none.
func (h *harness) limit(subject string) *float64 {
	h.t.Helper()
	v, set, err := h.actuator.Read(h.ctx, runtime.Workload{Subject: subject})
	if err != nil {
		h.t.Fatalf("reading %s: %v", subject, err)
	}
	if !set {
		return nil
	}
	return &v
}

// act steps at 0.5 until a change is applied to subject.
func (h *harness) act(subject string) {
	h.t.Helper()
	h.until(2*time.Hour, 0.5, "an action on "+subject, func() bool { _, ok := h.applied()[subject]; return ok })
}

func (h *harness) action(subject string) Action {
	h.t.Helper()
	for _, a := range h.l.Actions() {
		if a.Subject == subject {
			return a
		}
	}
	h.t.Fatalf("no action on %s in %+v", subject, h.l.Actions())
	return Action{}
}

func (h *harness) decisions(kind string) []*statemap.Decision {
	var out []*statemap.Decision
	for _, d := range h.m.Journal().Decisions(0) {
		if strings.HasPrefix(d.ID, "relief-"+kind+"-") {
			out = append(out, d)
		}
	}
	return out
}

func TestItActsOnceUnderPressure(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4, "pod:b": 0.3}})
	h.run(time.Minute, 0.5)
	if n := len(h.ledger.All()); n != 0 {
		t.Fatalf("%d changes before pressure lasted enter_for", n)
	}
	h.step(0.5)
	if n := len(h.ledger.All()); n != 1 {
		t.Fatalf("%d changes after a minute of pressure; want 1", n)
	}
	u := h.applied()["pod:a"]
	if !near(u.Applied, 1.2) || !u.Expires.Equal(h.now.Add(time.Hour)) {
		t.Errorf("entry %+v; want pod:a capped at 1.2 cores, expiring in an hour", u)
	}
	h.step(0.5)
	st := h.l.Status()
	if n := len(h.ledger.All()); n != 1 || st.Actions != 1 || st.InFlight == nil {
		t.Fatalf("applied %d, status %+v; want one action, in flight", n, st)
	}
	if st.Phase != Relieving {
		t.Errorf("phase %s", st.Phase)
	}
}

func TestAnActionThatHelpedIsKept(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4}})
	h.act("pod:a")
	if p := h.l.Status().InFlight.Plan.Predicted; !near(p, 0.1) {
		t.Fatalf("predicted %v; want 0.1", p)
	}
	h.run(2*time.Minute, 0.44)
	a := h.action("pod:a")
	if a.Outcome != "kept" || a.Observed == nil || !near(*a.Observed, 0.06) {
		t.Fatalf("action %+v; want kept with 0.06 observed", a)
	}
	if l := h.limit("pod:a"); l == nil || !near(*l, 1.2) {
		t.Fatalf("limit %v; want the cap kept at 1.2", l)
	}
	st := h.l.Status()
	if st.InFlight != nil || st.Kept != 1 || st.PredictionsHeld != 1 {
		t.Errorf("status %+v; want nothing in flight, one kept, predictions held 1", st)
	}
}

func TestAnActionThatDidNotHelpIsReverted(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4}})
	h.act("pod:a")
	h.run(2*time.Minute, 0.5)
	a := h.action("pod:a")
	if a.Outcome != "reverted" {
		t.Fatalf("action %+v; want reverted", a)
	}
	if l := h.limit("pod:a"); l != nil {
		t.Fatalf("limit %v; want no limit, as before", *l)
	}
	revertedAt := h.now
	if until := h.l.Status().Cooling["pod:a"]; !until.Equal(revertedAt.Add(40 * time.Minute)) {
		t.Fatalf("cooling until %v; want 40m after the revert", until)
	}
	h.run(40*time.Minute-tick, 0.5)
	if n := len(h.ledger.All()); n != 0 {
		t.Fatalf("%d changes while cooling down", n)
	}
	h.step(0.5)
	if n := len(h.ledger.All()); n != 1 {
		t.Fatalf("%d changes once the cooldown ended; want 1", n)
	}
	if st := h.l.Status(); st.Reverted != 1 || st.PredictionsHeld != 0 {
		t.Errorf("status %+v", st)
	}
}

func TestReleaseRevertsLastFirstAndStopsOnReturn(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4, "pod:b": 0.3}})
	h.act("pod:a")
	h.run(2*time.Minute, 0.44)
	h.until(time.Minute, 0.44, "an action on pod:b", func() bool { _, ok := h.applied()["pod:b"]; return ok })
	h.run(2*time.Minute, 0.38)
	if a, b := h.action("pod:a"), h.action("pod:b"); a.Outcome != "kept" || b.Outcome != "kept" {
		t.Fatalf("outcomes %q, %q; want both kept", a.Outcome, b.Outcome)
	}
	h.until(4*time.Minute, 0.05, "a release", func() bool { return len(h.ledger.All()) < 2 })
	if h.l.Status().Phase != Releasing {
		t.Fatalf("phase %s", h.l.Status().Phase)
	}
	if _, ok := h.applied()["pod:a"]; !ok || h.action("pod:b").Outcome != "released" {
		t.Fatalf("applied %v, pod:b %+v; want the newest (pod:b) released first", h.applied(), h.action("pod:b"))
	}
	if n := len(h.decisions("release")); n != 1 {
		t.Fatalf("%d release decisions", n)
	}
	h.run(time.Minute+tick, 0.4)
	if h.l.Status().Phase != Relieving {
		t.Fatalf("phase %s; want relieving again", h.l.Status().Phase)
	}
	h.run(5*time.Minute, 0.4)
	if _, ok := h.applied()["pod:a"]; !ok {
		t.Fatalf("pod:a was released after pressure returned")
	}
}

func TestRepeatedThrottlingBacksOff(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4}})
	for i, want := range []time.Duration{10 * time.Minute, 20 * time.Minute, 40 * time.Minute} {
		h.act("pod:a")
		h.run(2*time.Minute, 0.44)
		h.until(5*time.Minute, 0.05, "a release", func() bool { return len(h.ledger.All()) == 0 })
		if got := h.l.Status().Cooling["pod:a"].Sub(h.now); got != want {
			t.Fatalf("cycle %d: cooldown %s; want %s", i+1, got, want)
		}
	}
	if got := backoff(10*time.Minute, 5); got != 4*time.Hour {
		t.Errorf("backoff after five cycles %s; want capped at 4h", got)
	}
}

func TestAnExpiredChangeIsRevertedInAnyPhase(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4}})
	h.act("pod:a")
	acted := h.now
	h.run(2*time.Minute, 0.44)
	h.until(time.Hour, 0.44, "expiry", func() bool { return len(h.ledger.All()) == 0 })
	if !h.now.Equal(acted.Add(time.Hour)) {
		t.Fatalf("reverted at %v; want an hour after %v", h.now, acted)
	}
	if a := h.action("pod:a"); a.Outcome != "expired" || h.limit("pod:a") != nil {
		t.Fatalf("action %+v; want expired with the limit given back", a)
	}
	if n := len(h.decisions("expired")); n != 1 {
		t.Fatalf("%d expiry decisions", n)
	}
}

func TestReconcileResumesAndRevertsExpired(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4, "pod:b": 0.3, "pod:c": 0.2}})
	record(t, h.m, contention, "", 0.5, h.now)
	b := h.m.Decide("relief-act-1-1", "earlier act")
	b.Input("plan", Plan{Subject: "pod:a", Predicted: 0.1})
	b.Commit(nil)
	h.seed(actuators.Entry{NodeID: "n1", Subject: "pod:a", Action: cpu.Action,
		Applied: 1.2, Expires: h.now.Add(30 * time.Minute), DecisionID: "relief-act-1-1"})
	h.seed(actuators.Entry{NodeID: "n1", Subject: "pod:b", Action: cpu.Action,
		Applied: 0.9, Expires: h.now.Add(-time.Minute), DecisionID: "relief-act-1-2"})
	h.seed(actuators.Entry{NodeID: "other", Subject: "pod:c", Action: cpu.Action,
		Applied: 0.6, Expires: h.now.Add(-time.Minute), DecisionID: "theirs"})

	if err := h.l.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	st := h.l.Status()
	if st.InFlight == nil || st.InFlight.Subject != "pod:a" || !near(st.InFlight.Plan.Predicted, 0.1) ||
		!st.InFlight.ActedAt.Equal(h.now) || !near(st.InFlight.ContentionAtAct, 0.5) {
		t.Fatalf("in flight %+v; want pod:a resumed with its journalled prediction", st.InFlight)
	}
	ap := h.applied()
	if _, ok := ap["pod:b"]; ok {
		t.Error("the expired change on pod:b was not reverted")
	}
	if _, ok := ap["pod:c"]; !ok {
		t.Error("another node's change was reverted")
	}
	if a := h.action("pod:b"); a.Outcome != "expired" {
		t.Errorf("pod:b %+v; want expired", a)
	}
}

func TestReconcileKeepsFurtherChangesForRelease(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4, "pod:b": 0.3}})
	record(t, h.m, contention, "", 0.5, h.now)
	h.seed(actuators.Entry{NodeID: "n1", Subject: "pod:a", Action: cpu.Action,
		Applied: 1.2, Expires: h.now.Add(20 * time.Minute), DecisionID: "relief-act-1-1"})
	h.seed(actuators.Entry{NodeID: "n1", Subject: "pod:b", Action: cpu.Action,
		Applied: 0.9, Expires: h.now.Add(30 * time.Minute), DecisionID: "relief-act-1-2"})
	if err := h.l.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	st := h.l.Status()
	if st.InFlight == nil || st.InFlight.Subject != "pod:b" || st.InFlight.Plan.Predicted != 0 {
		t.Fatalf("in flight %+v; want the most recent change, pod:b, with no prediction", st.InFlight)
	}
	if a := h.action("pod:a"); a.Outcome != "kept" {
		t.Fatalf("pod:a %+v; want kept, for release", a)
	}
	// With no prediction to judge against, verify keeps it.
	h.run(2*time.Minute, 0.6)
	if a := h.action("pod:b"); a.Outcome != "kept" {
		t.Fatalf("pod:b %+v; want kept", a)
	}
}

func TestPausedItDoesNotAct(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4, "pod:b": 0.3}})
	record(t, h.m, contention, "", 0.5, h.now)
	h.seed(actuators.Entry{NodeID: "n1", Subject: "pod:b", Action: cpu.Action,
		Applied: 0.9, Expires: h.now.Add(3 * time.Minute), DecisionID: "relief-act-1-1"})
	if err := h.l.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.l.Pause()
	if !h.l.Status().Paused {
		t.Fatal("not paused")
	}
	h.run(5*time.Minute, 0.5)
	if n := len(h.ledger.All()); n != 0 {
		t.Fatalf("%d changes while paused; want none, and the resumed one expired", n)
	}
	if h.l.Status().Phase != Relieving {
		t.Errorf("phase %s; the trigger still runs while paused", h.l.Status().Phase)
	}
	if a := h.action("pod:b"); a.Outcome != "expired" {
		t.Errorf("pod:b %+v; want expired while paused", a)
	}
	h.l.Resume()
	h.step(0.5)
	if _, ok := h.applied()["pod:a"]; !ok {
		t.Fatalf("applied %v after resume; want pod:a", h.applied())
	}
}

func TestAGoneWorkloadFreesTheSlot(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4, "pod:b": 0.3}})
	h.act("pod:a")
	h.rt.Remove("pod:a")
	h.run(2*time.Minute, 0.5)
	if a := h.action("pod:a"); a.Outcome != "gone" {
		t.Fatalf("pod:a %+v; want gone", a)
	}
	if f := h.l.Status().InFlight; f != nil && f.Subject == "pod:a" {
		t.Fatal("the slot is still held by a gone workload")
	}
	if n := len(h.decisions("gone")); n != 1 {
		t.Fatalf("%d gone decisions", n)
	}
	h.step(0.5)
	if _, ok := h.applied()["pod:b"]; !ok {
		t.Fatalf("applied %v; want the next action on pod:b", h.applied())
	}

	// A kept change whose workload is removed is dropped at expiry, once.
	h.run(2*time.Minute, 0.42)
	h.rt.Remove("pod:b")
	h.run(time.Hour, 0.42)
	if a := h.action("pod:b"); a.Outcome != "gone" {
		t.Fatalf("pod:b %+v; want gone at expiry", a)
	}
	n := len(h.m.Journal().Decisions(0))
	h.run(10*time.Minute, 0.42)
	if got := len(h.m.Journal().Decisions(0)); got != n || len(h.decisions("gone")) != 2 {
		t.Fatalf("decisions %d → %d; a gone workload must not be retried", n, got)
	}
}

func TestTheRateLimitHolds(t *testing.T) {
	h := newHarness(t, fixture{
		shares: map[string]float64{"pod:a": 0.4, "pod:b": 0.3},
		policy: func(ps *spec.PolicySpec) { ps.Defaults.MaxActionsPerHour = 1 },
	})
	h.act("pod:a")
	h.run(2*time.Minute, 0.44)
	h.run(5*time.Minute, 0.44)
	if n := len(h.ledger.All()); n != 1 {
		t.Fatalf("%d changes; want the hourly limit of 1 to hold", n)
	}
	refused := h.decisions("refused")
	if len(refused) != 1 {
		t.Fatalf("%d refusals; want one, not one per tick", len(refused))
	}
	if r, ok := refused[0].Inputs["rule"]; !ok || r != -1 {
		t.Fatalf("refusal inputs %v; want the rule that refused", refused[0].Inputs)
	}
	if !strings.Contains(refused[0].Answer["refused"].(string), "max_actions_per_hour") {
		t.Errorf("refusal %v", refused[0].Answer)
	}
}

func TestEveryStepIsADecision(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4}})
	h.act("pod:a")
	h.run(2*time.Minute, 0.44)
	a := h.action("pod:a")
	if !strings.HasPrefix(a.DecisionID, "relief-act-") || a.ID != a.DecisionID {
		t.Fatalf("action %+v; want its id to be its act decision", a)
	}
	act, ok := h.m.Journal().Decision(a.DecisionID)
	if !ok {
		t.Fatalf("no decision %s", a.DecisionID)
	}
	for _, in := range []string{"notice", "attribution", "plan", "policy"} {
		if _, ok := act.Inputs[in]; !ok {
			t.Errorf("act decision inputs %v; missing %s", act.Inputs, in)
		}
	}
	if plan, _ := act.Inputs["plan"].(Plan); plan.EstimateID == "" {
		t.Errorf("plan %+v; want the estimate's decision id", act.Inputs["plan"])
	}
	if a.Entry.DecisionID != a.DecisionID {
		t.Errorf("entry %+v; want it stamped with the act decision", a.Entry)
	}
	verify := h.decisions("verify")
	if len(verify) != 1 || verify[0].Inputs["act"] != a.DecisionID {
		t.Fatalf("verify decisions %+v; want one citing %s", verify, a.DecisionID)
	}
	if a.VerifyID != verify[0].ID {
		t.Errorf("verify_id = %q; want %q", a.VerifyID, verify[0].ID)
	}
	if d, ok := h.m.Journal().Decision(a.VerifyID); !ok || d.Inputs["act"] != a.DecisionID {
		t.Errorf("verify_id %q does not resolve to the verify decision: %+v", a.VerifyID, d)
	}
	if st := h.l.Status(); st.Runtime == "" {
		t.Errorf("status carries no runtime name: %+v", st)
	}
}

func TestNoCoresTheLoopRefusesOnce(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4}, noFact: true})
	h.run(5*time.Minute, 0.5)
	if n := len(h.ledger.All()); n != 0 {
		t.Fatalf("%d changes without a cpu_cores fact", n)
	}
	refused := h.decisions("refused")
	if len(refused) != 1 || refused[0].Answer["refused"] != "no cpu_cores fact: shares cannot become cores" {
		t.Fatalf("refusals %+v; want one naming the missing fact", refused)
	}
}

func TestStalePressureTakesNoAction(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4}})
	h.run(50*time.Second, 0.5)
	// The source goes quiet: no readings, and the map marks pressure stale.
	h.advance(3 * time.Minute)
	h.m.Sweep()
	if p, _ := h.m.Property(contention); p.Status == statemap.Active {
		t.Fatal("setup: pressure still active")
	}
	for i := 0; i < 20; i++ {
		h.advance(tick)
		h.l.Tick(h.ctx)
	}
	if n := len(h.ledger.All()); n != 0 || h.l.Status().Phase != Calm {
		t.Fatalf("applied %d, phase %s; want no action on stale pressure", n, h.l.Status().Phase)
	}
}

func TestAManualRevertCoolsDown(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4}})
	h.act("pod:a")
	id := h.l.Status().InFlight.ID
	if err := h.l.Revert(h.ctx, id); err != nil {
		t.Fatal(err)
	}
	if a := h.action("pod:a"); a.Outcome != "manual" || h.limit("pod:a") != nil {
		t.Fatalf("pod:a %+v; want reverted by hand", a)
	}
	if until := h.l.Status().Cooling["pod:a"]; !until.Equal(h.now.Add(10 * time.Minute)) {
		t.Fatalf("cooling until %v", until)
	}
	if err := h.l.Revert(h.ctx, id); err == nil {
		t.Fatal("a second revert of the same action succeeded")
	}
	if n := len(h.decisions("manual")); n != 1 {
		t.Fatalf("%d manual decisions", n)
	}
}

// TestReadersDoNotRaceTheLoop: Status, Actions, Pause and Resume are called from
// HTTP handlers while the ticker runs passes.
func TestReadersDoNotRaceTheLoop(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4, "pod:b": 0.3}})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			_ = h.l.Status()
			_ = h.l.Actions()
			if i%50 == 0 {
				h.l.Pause()
				h.l.Resume()
			}
		}
	}()
	for i := 0; i < 40; i++ {
		h.step(0.5)
	}
	<-done
}

// quiet moves the clock and ticks n times with no new reading and no sweep, as
// when the source has just gone quiet and the map has not yet noticed.
func (h *harness) quiet(n int) {
	for i := 0; i < n; i++ {
		h.advance(tick)
		h.l.Tick(h.ctx)
	}
}

func TestAFrozenReadingIsNotEvidence(t *testing.T) {
	t.Run("it does not complete enter_for", func(t *testing.T) {
		h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4}})
		h.run(50*time.Second, 0.5)
		h.quiet(20)
		if p, _ := h.m.Property(contention); p.Status != statemap.Active {
			t.Fatal("setup: the map has marked pressure stale; this case is about before it does")
		}
		if n := len(h.ledger.All()); n != 0 || h.l.Status().Phase != Calm {
			t.Fatalf("applied %d, phase %s; a reading repeated is not pressure that lasted", n, h.l.Status().Phase)
		}
	})
	t.Run("it does not judge an action", func(t *testing.T) {
		h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4}})
		h.act("pod:a")
		h.quiet(12) // 3m: past settle, with nothing read since the act
		if n := len(h.decisions("verify")); n != 0 {
			t.Fatalf("%d verify decisions on a reading from before settle", n)
		}
		if f := h.l.Status().InFlight; f == nil || f.Subject != "pod:a" {
			t.Fatalf("in flight %+v; want pod:a still waiting", f)
		}
		h.step(0.44)
		if a := h.action("pod:a"); a.Outcome != "kept" {
			t.Fatalf("pod:a %+v; want judged on the first fresh reading", a)
		}
	})
}

func TestAFailedReconcileBlocksActing(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4}, flaky: true})
	h.fl.set(func(f *flaky) { f.listErr = 1000 })
	h.run(3*time.Minute, 0.5)
	if n := len(h.ledger.All()); n != 0 {
		t.Fatalf("%d changes before this node's earlier changes were known", n)
	}
	if h.l.Status().Phase != Relieving {
		t.Fatalf("phase %s; the trigger still runs", h.l.Status().Phase)
	}
	h.fl.set(func(f *flaky) { f.listErr = 0 })
	h.step(0.5)
	if n := len(h.ledger.All()); n != 1 {
		t.Fatalf("%d changes once reconcile succeeded; want 1", n)
	}
}

func TestAnExpiredChangeThatWillNotRevertIsRetried(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:b": 0.3}, flaky: true})
	h.seed(actuators.Entry{NodeID: "n1", Subject: "pod:b", Action: cpu.Action,
		Applied: 0.9, Expires: h.now.Add(-time.Minute), DecisionID: "relief-act-1-1"})
	h.fl.set(func(f *flaky) { f.setErr = 1 })
	if err := h.l.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.applied()["pod:b"]; !ok {
		t.Fatal("setup: the revert did not fail")
	}
	h.step(0.05)
	if _, ok := h.applied()["pod:b"]; ok {
		t.Fatal("the expired change was not retried")
	}
	if a := h.action("pod:b"); a.Outcome != "expired" {
		t.Fatalf("pod:b %+v; want expired", a)
	}
}

// TestAnUnlistedWorkloadIsGivenBackNotDropped: a workload the runtime stops
// listing is not taken for gone. Its change is given back through its subject,
// retried while the actuator refuses, and closed as released once it goes
// through: the actuator still had the workload, so it was not gone.
func TestAnUnlistedWorkloadIsGivenBackNotDropped(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4}, flaky: true})
	h.act("pod:a")
	h.fl.set(func(f *flaky) { f.hide["pod:a"], f.setErr = true, 1 })
	h.run(2*time.Minute, 0.5)
	if f := h.l.Status().InFlight; f == nil || f.Subject != "pod:a" {
		t.Fatalf("in flight %+v; a change the runtime still holds must not be dropped", f)
	}
	if _, ok := h.applied()["pod:a"]; !ok {
		t.Fatal("setup: the revert did not fail")
	}
	h.step(0.5)
	if a := h.action("pod:a"); a.Outcome != OutcomeReleased {
		t.Fatalf("pod:a %+v; want released once the revert went through", a)
	}
	if _, ok := h.applied()["pod:a"]; ok {
		t.Fatal("the runtime still holds the change")
	}
}

func TestAResumedChangeWaitsForAFreshBaseline(t *testing.T) {
	h := newHarness(t, fixture{noPressure: true})
	h.rt.Put(pod("pod:a"))
	h.seed(actuators.Entry{NodeID: "n1", Subject: "pod:a", Action: cpu.Action,
		Applied: 1.2, Expires: h.now.Add(30 * time.Minute), DecisionID: "relief-act-1-1"})
	if err := h.l.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if f := h.l.Status().InFlight; f == nil || !f.ActedAt.IsZero() || f.ContentionAtAct != 0 {
		t.Fatalf("in flight %+v; want no baseline without a reading", f)
	}
	h.step(0.5)
	if f := h.l.Status().InFlight; f == nil || !near(f.ContentionAtAct, 0.5) || !f.ActedAt.Equal(h.now) {
		t.Fatalf("in flight %+v; want the first fresh reading as its baseline", f)
	}
}

func TestADominantWorkloadIsSteppedAgainAndReleasedOnce(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4}})
	h.act("pod:a")
	h.run(2*time.Minute, 0.44)
	// The cap took hold: pod:a now uses 0.3 of the node.
	record(t, h.m, usage("pod:a"), "pod:a", 0.3, h.now)
	h.until(3*time.Minute, 0.44, "a second step", func() bool { return near(h.applied()["pod:a"].Applied, 0.9) })
	u := h.applied()["pod:a"]
	if u.OriginalSet {
		t.Fatalf("entry %+v; want the original from before the first step (no limit)", u)
	}
	acts := h.l.Actions()
	if len(acts) != 1 || acts[0].DecisionID == acts[0].ID {
		t.Fatalf("actions %+v; want one action per workload, now carrying its second step", acts)
	}
	second, _ := h.m.Journal().Decision(acts[0].DecisionID)
	if prev, ok := second.Inputs["previous"].(map[string]any); !ok || prev["act"] != acts[0].ID {
		t.Fatalf("second act inputs %v; want it to cite the first", second.Inputs)
	}
	h.run(2*time.Minute, 0.40)
	if a := h.action("pod:a"); a.Outcome != "kept" {
		t.Fatalf("pod:a %+v; want the second step kept", a)
	}
	h.until(5*time.Minute, 0.05, "a release", func() bool { return len(h.ledger.All()) == 0 })
	if l := h.limit("pod:a"); l != nil {
		t.Fatalf("limit %v; want the pre-agent no limit back in one revert", *l)
	}
	if n := len(h.decisions("release")); n != 1 {
		t.Fatalf("%d release decisions; want one", n)
	}
}

func TestAFailedSecondStepReturnsToTheFirst(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4}})
	h.act("pod:a")
	h.run(2*time.Minute, 0.44)
	record(t, h.m, usage("pod:a"), "pod:a", 0.3, h.now)
	h.until(3*time.Minute, 0.44, "a second step", func() bool { return near(h.applied()["pod:a"].Applied, 0.9) })
	h.run(2*time.Minute, 0.44)
	u, ok := h.applied()["pod:a"]
	if !ok || !near(u.Applied, 1.2) || u.OriginalSet {
		t.Fatalf("entry %+v; want the first step's cap back, with the original kept", u)
	}
	a := h.action("pod:a")
	if a.Outcome != "kept" || a.ID != a.DecisionID {
		t.Fatalf("pod:a %+v; want the first step, kept", a)
	}
	if until := h.l.Status().Cooling["pod:a"]; !until.Equal(h.now.Add(40 * time.Minute)) {
		t.Fatalf("cooling until %v; want 40m", until)
	}
}

func TestKeptChangesAreReleasedOutsideRelieving(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4, "pod:b": 0.3}})
	record(t, h.m, contention, "", 0.05, h.now)
	h.seed(actuators.Entry{NodeID: "n1", Subject: "pod:a", Action: cpu.Action,
		Applied: 1.2, Expires: h.now.Add(20 * time.Minute), DecisionID: "relief-act-1-1"})
	h.seed(actuators.Entry{NodeID: "n1", Subject: "pod:b", Action: cpu.Action,
		Applied: 0.9, Expires: h.now.Add(30 * time.Minute), DecisionID: "relief-act-1-2"})
	if err := h.l.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.until(5*time.Minute, 0.05, "a first release", func() bool { return len(h.ledger.All()) < 2 })
	if _, ok := h.applied()["pod:a"]; !ok {
		t.Fatalf("applied %v; want the newest (pod:b) released first", h.applied())
	}
	first := h.now
	h.until(5*time.Minute, 0.05, "a second release", func() bool { return len(h.ledger.All()) == 0 })
	if gap := h.now.Sub(first); gap != 2*time.Minute {
		t.Fatalf("second release %s after the first; want one settle", gap)
	}
	if h.l.Status().Phase != Calm {
		t.Errorf("phase %s", h.l.Status().Phase)
	}
}

func TestNoPlanIsNotReattributedEveryTick(t *testing.T) {
	// strength 0.1: pod:a is predicted to cut pressure by 0.01, below min_effect.
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4}, strength: 0.1})
	estimates := func() int {
		n := 0
		for _, d := range h.m.Journal().Decisions(0) {
			if strings.HasPrefix(d.ID, "est-") {
				n++
			}
		}
		return n
	}
	h.until(2*time.Minute, 0.5, "relieving", func() bool { return h.l.Status().Phase == Relieving })
	first := estimates()
	if first == 0 {
		t.Fatal("no attribution on entering relieving")
	}
	h.run(time.Minute, 0.5)
	if n := estimates(); n != first {
		t.Fatalf("estimates %d → %d within a settle; want no re-attribution", first, n)
	}
	h.run(time.Minute, 0.5)
	if n := estimates(); n != 2*first {
		t.Fatalf("estimates %d after a settle; want %d (one more attribution)", n, 2*first)
	}
	if n := len(h.ledger.All()); n != 0 {
		t.Fatalf("%d changes below min_effect", n)
	}
}

// resumeCapped seeds a change this node made to pod:a, capped at 1.2 cores with no
// limit before, as a restarted agent finds it, and reconciles.
func (h *harness) resumeCapped(decisionID string) {
	h.t.Helper()
	h.rt.Put(limited(pod("pod:a"), 1.2))
	record(h.t, h.m, contention, "", 0.5, h.now)
	h.seed(actuators.Entry{NodeID: "n1", Subject: "pod:a", Action: cpu.Action,
		Applied: 1.2, Expires: h.now.Add(30 * time.Minute), DecisionID: decisionID})
	if err := h.l.Reconcile(h.ctx); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) originRefusals() int {
	n := 0
	for _, d := range h.decisions("refused") {
		if s, _ := d.Answer["refused"].(string); strings.Contains(s, "origin") {
			n++
		}
	}
	return n
}

// TestAResumedChangeOfUnknownOriginIsNotSteppedAgain: with no original limit and
// no first act to read the pre-agent usage from, a further step's floor would be
// a share of the agent's own cap.
func TestAResumedChangeOfUnknownOriginIsNotSteppedAgain(t *testing.T) {
	t.Run("no journal", func(t *testing.T) {
		h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.3}})
		h.resumeCapped("relief-act-1-1")
		h.run(6*time.Minute, 0.5)
		if u := h.applied()["pod:a"]; !near(u.Applied, 1.2) {
			t.Fatalf("entry %+v; want the cap left at 1.2, not stepped", u)
		}
		if n := h.originRefusals(); n != 1 {
			t.Fatalf("%d origin refusals; want one", n)
		}
		if a := h.action("pod:a"); a.Outcome != "kept" {
			t.Fatalf("pod:a %+v; want kept, for release", a)
		}
	})
	t.Run("only a further step journalled", func(t *testing.T) {
		h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.3}})
		b := h.m.Decide("relief-act-1-2", "a further step")
		b.Input("plan", Plan{Subject: "pod:a", From: 1.6, To: 1.2, Predicted: 0.1})
		b.Input("previous", map[string]any{"act": "relief-act-1-1", "action": "relief-act-1-1"})
		b.Commit(nil)
		h.resumeCapped("relief-act-1-2")
		if f := h.l.Status().InFlight; f == nil || f.ID != "relief-act-1-1" || !near(f.Plan.Predicted, 0.1) {
			t.Fatalf("in flight %+v; want the action named by its first act, judged by its latest", f)
		}
		h.run(6*time.Minute, 0.44)
		if u := h.applied()["pod:a"]; !near(u.Applied, 1.2) {
			t.Fatalf("entry %+v; want the cap left at 1.2", u)
		}
		if n := h.originRefusals(); n != 1 {
			t.Fatalf("%d origin refusals; want one", n)
		}
	})
	t.Run("the first act anchors the floor", func(t *testing.T) {
		h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.3}})
		first := h.m.Decide("relief-act-1-1", "the first step")
		first.Input("plan", Plan{Subject: "pod:a", From: 4, To: 3})
		first.Commit(nil)
		later := h.m.Decide("relief-act-1-2", "a further step")
		later.Input("plan", Plan{Subject: "pod:a", From: 1.6, To: 1.2, Predicted: 0.1})
		later.Input("previous", map[string]any{"act": "relief-act-1-1", "action": "relief-act-1-1"})
		later.Commit(nil)
		h.resumeCapped("relief-act-1-2")
		h.run(6*time.Minute, 0.44)
		// Usage 0.3 of 4 cores = 1.2; half the first act's 4 cores is 2.0, so the
		// floor is reached. A floor from the further step's 1.6 would allow 0.9.
		if u := h.applied()["pod:a"]; !near(u.Applied, 1.2) {
			t.Fatalf("entry %+v; want no step below the floor set by the first act", u)
		}
		if n := h.originRefusals(); n != 0 {
			t.Fatalf("%d origin refusals; the origin is known", n)
		}
		var floor bool
		for _, d := range h.decisions("refused") {
			if s, _ := d.Answer["refused"].(string); strings.Contains(s, "floor reached") {
				floor = true
			}
		}
		if !floor {
			t.Fatal("no refusal at the floor")
		}
	})
}

// lyingSet records a value and then reports failure, as a resize that took
// effect although its call timed out.
type lyingSet struct {
	actuators.Actuator
	mu   sync.Mutex
	fail int // this many more Sets record the value and fail
}

func (r *lyingSet) Set(ctx context.Context, w runtime.Workload, v float64, set bool) error {
	if err := r.Actuator.Set(ctx, w, v, set); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail > 0 {
		r.fail--
		return errors.New("resize default/a: context deadline exceeded")
	}
	return nil
}

// TestAFailedApplyThatTookEffectIsAdopted: a change the runtime recorded although
// Apply failed is found by the next reconcile, held, and given back at expiry.
func TestAFailedApplyThatTookEffectIsAdopted(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4},
		wrap: func(d *runtime.DryRun, a actuators.Actuator) (runtime.Runtime, actuators.Actuator) {
			return d, &lyingSet{Actuator: a, fail: 1}
		}})
	h.act("pod:a")
	acted := h.now
	if st := h.l.Status(); st.InFlight != nil || st.Refused != 1 {
		t.Fatalf("status %+v; want the failed apply refused", st)
	}
	h.step(0.5)
	st := h.l.Status()
	if st.InFlight == nil || st.InFlight.Subject != "pod:a" || !st.InFlight.Resumed {
		t.Fatalf("in flight %+v; want the change the runtime holds adopted as resumed", st.InFlight)
	}
	h.l.Pause()
	h.until(2*time.Hour, 0.5, "expiry", func() bool { return len(h.ledger.All()) == 0 })
	if !h.now.Equal(acted.Add(time.Hour)) {
		t.Fatalf("given back at %v; want at expiry, an hour after %v", h.now, acted)
	}
	if a := h.action("pod:a"); a.Outcome != OutcomeExpired {
		t.Fatalf("pod:a %+v; want expired", a)
	}
}

// blocking is a runtime and an actuator whose calls wait for their context while
// block is set, as an orchestrator that has stopped answering.
type blocking struct {
	*runtime.DryRun
	act   actuators.Actuator
	mu    sync.Mutex
	block bool
}

func (b *blocking) wait(ctx context.Context) error {
	b.mu.Lock()
	on := b.block
	b.mu.Unlock()
	if !on {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

func (b *blocking) Workloads(ctx context.Context) ([]runtime.Workload, error) {
	if err := b.wait(ctx); err != nil {
		return nil, err
	}
	return b.DryRun.Workloads(ctx)
}

func (b *blocking) Describe() actuators.Descriptor  { return b.act.Describe() }
func (b *blocking) Check(w runtime.Workload) string { return b.act.Check(w) }

func (b *blocking) Read(ctx context.Context, w runtime.Workload) (float64, bool, error) {
	if err := b.wait(ctx); err != nil {
		return 0, false, err
	}
	return b.act.Read(ctx, w)
}

func (b *blocking) Set(ctx context.Context, w runtime.Workload, v float64, set bool) error {
	if err := b.wait(ctx); err != nil {
		return err
	}
	return b.act.Set(ctx, w, v, set)
}

// within fails the test if fn has not returned after d.
func within(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %s", what, d)
	}
}

// TestAHungOrchestratorDoesNotHangTheLoop: every pass and every manual revert
// gives up on the runtime after the loop's timeout.
func TestAHungOrchestratorDoesNotHangTheLoop(t *testing.T) {
	var b *blocking
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4},
		wrap: func(d *runtime.DryRun, a actuators.Actuator) (runtime.Runtime, actuators.Actuator) {
			b = &blocking{DryRun: d, act: a}
			return b, b
		}})
	h.l.timeout = 50 * time.Millisecond
	h.act("pod:a")
	id := h.l.Status().InFlight.ID
	h.advance(2 * time.Minute) // the next tick verifies, and so asks the runtime for its workloads
	b.mu.Lock()
	b.block = true
	b.mu.Unlock()
	within(t, 5*time.Second, "Tick", func() { h.step(0.5) })
	var err error
	within(t, 5*time.Second, "Revert", func() { err = h.l.Revert(h.ctx, id) })
	if err == nil {
		t.Fatal("a revert the runtime never answered succeeded")
	}
	if _, ok := h.applied()["pod:a"]; !ok {
		t.Fatal("the change was dropped although the runtime never gave it back")
	}
}

// TestARestartDoesNotResetTheHourlyBudget: changes taken up after a restart count
// toward max_actions_per_hour, at their journalled act time or else now.
func TestARestartDoesNotResetTheHourlyBudget(t *testing.T) {
	setup := func(t *testing.T) *harness {
		h := newHarness(t, fixture{
			shares: map[string]float64{"pod:a": 0.3, "pod:b": 0.3, "pod:c": 0.3},
			policy: func(ps *spec.PolicySpec) { ps.Defaults.MaxActionsPerHour = 2 },
		})
		for i, s := range []string{"pod:a", "pod:b"} {
			h.seed(actuators.Entry{NodeID: "n1", Subject: s, Action: cpu.Action,
				Applied: 0.9, Expires: h.now.Add(50 * time.Minute), DecisionID: fmt.Sprintf("earlier-act-%d", i+1)})
		}
		return h
	}
	t.Run("no journal: counted from now", func(t *testing.T) {
		h := setup(t)
		if err := h.l.Reconcile(h.ctx); err != nil {
			t.Fatal(err)
		}
		h.run(40*time.Minute, 0.5)
		if n := len(h.decisions("act")); n != 0 {
			t.Fatalf("%d acts within the hour; two resumed changes already spent max_actions_per_hour 2", n)
		}
	})
	t.Run("journalled: counted from the act", func(t *testing.T) {
		h := setup(t)
		b := h.m.Decide("earlier-act-1", "earlier act")
		b.Commit(nil)
		h.advance(61 * time.Minute)
		for _, s := range []string{"pod:a", "pod:b", "pod:c"} {
			record(t, h.m, usage(s), s, 0.3, h.now)
		}
		record(t, h.m, nodeUsage, "", 0.8, h.now)
		for _, s := range []string{"pod:a", "pod:b"} {
			u := h.applied()[s]
			u.Expires = h.now.Add(50 * time.Minute)
			h.seed(u)
		}
		if err := h.l.Reconcile(h.ctx); err != nil {
			t.Fatal(err)
		}
		h.run(40*time.Minute, 0.5)
		if n := len(h.decisions("act")); n != 1 {
			t.Fatalf("%d acts; want one: pod:a's act is over an hour old, pod:b's counts from now", n)
		}
	})
}

// TestARetriedRevertCitesItsVerdict: a revert the runtime refused at first still
// records the verify decision that caused it when it succeeds.
func TestARetriedRevertCitesItsVerdict(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4}, flaky: true})
	h.act("pod:a")
	h.fl.set(func(f *flaky) { f.setErr = 1 })
	h.until(5*time.Minute, 0.5, "the revert", func() bool { return len(h.decisions("revert")) == 1 })
	verify := h.decisions("verify")
	if len(verify) != 1 {
		t.Fatalf("%d verify decisions", len(verify))
	}
	if got := h.decisions("revert")[0].Inputs["verify"]; got != verify[0].ID {
		t.Fatalf("revert cites verify %v; want %s", got, verify[0].ID)
	}
}

// TestAStuckReleaseDoesNotBlockTheOthers: the newest kept change, whose undo the
// runtime keeps refusing, does not hold back the release of the others. Its
// failures are counted on the action and in Status, and journalled once.
func TestAStuckReleaseDoesNotBlockTheOthers(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4, "pod:b": 0.3}, flaky: true})
	h.act("pod:a")
	h.run(2*time.Minute, 0.44)
	h.until(time.Minute, 0.44, "an action on pod:b", func() bool { _, ok := h.applied()["pod:b"]; return ok })
	h.run(2*time.Minute, 0.38)
	if a, b := h.action("pod:a"), h.action("pod:b"); a.Outcome != OutcomeKept || b.Outcome != OutcomeKept {
		t.Fatalf("outcomes %q, %q; want both kept", a.Outcome, b.Outcome)
	}
	h.fl.set(func(f *flaky) { f.stuck["pod:b"] = true })
	h.until(10*time.Minute, 0.05, "pod:a released", func() bool { return h.action("pod:a").Outcome == OutcomeReleased })
	h.run(2*time.Minute, 0.05)
	b := h.action("pod:b")
	if b.Outcome != OutcomeKept || b.RevertAttempts < 2 || !strings.Contains(b.LastError, "webhook") {
		t.Fatalf("pod:b %+v; want still kept, with its failed undos counted and the last error", b)
	}
	if st := h.l.Status(); st.Stuck != 1 {
		t.Fatalf("status %+v; want one stuck change", st)
	}
	if n := len(h.decisions("stuck")); n != 1 {
		t.Fatalf("%d stuck decisions; want the first failure journalled once", n)
	}
	h.fl.set(func(f *flaky) { f.stuck["pod:b"] = false })
	h.until(2*time.Minute, 0.05, "pod:b released", func() bool { return h.action("pod:b").Outcome == OutcomeReleased })
	if st := h.l.Status(); st.Stuck != 0 {
		t.Fatalf("status %+v; want nothing stuck once it went through", st)
	}
	if b := h.action("pod:b"); b.RevertAttempts != 0 || b.LastError != "" {
		t.Fatalf("pod:b %+v; want a released change to show no failing undo", b)
	}
}

// TestAChangeMadeUnderneathIsDroppedAsGone: a held change whose setting no longer
// reads as what the agent applied, as after a pod restart or another hand, is
// not the agent's to give back: reconcile drops it and journals it gone.
func TestAChangeMadeUnderneathIsDroppedAsGone(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4}})
	h.seed(actuators.Entry{NodeID: "n1", Subject: "pod:a", Action: cpu.Action,
		Applied: 1.2, Expires: h.now.Add(30 * time.Minute), DecisionID: "relief-act-1-1"})
	if err := h.actuator.Set(h.ctx, runtime.Workload{Subject: "pod:a"}, 2, true); err != nil {
		t.Fatal(err)
	}
	if err := h.l.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(h.ledger.All()); n != 0 {
		t.Fatalf("%d entries; want the changed one dropped", n)
	}
	if a := h.action("pod:a"); a.Outcome != OutcomeGone || h.l.Status().InFlight != nil {
		t.Fatalf("pod:a %+v; want gone, holding no slot", a)
	}
	gone := h.decisions("gone")
	if len(gone) != 1 || !strings.Contains(gone[0].Rationale, "changed underneath") {
		t.Fatalf("gone decisions %+v; want one saying the setting was changed underneath", gone)
	}
	if l := h.limit("pod:a"); l == nil || *l != 2 {
		t.Fatalf("limit %v; the setting someone else made must be left alone", l)
	}
}

// TestAnotherLeversChangesAreLeftAlone: a loop reconciles only its own action's
// entries; another lever's, even one no actuator is configured for, stay in the
// ledger untouched.
func TestAnotherLeversChangesAreLeftAlone(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4}})
	other := actuators.Entry{NodeID: "n1", Subject: "pod:a", Action: "memory.high",
		Applied: 1 << 30, Expires: h.now.Add(-time.Minute), DecisionID: "elsewhere"}
	if err := h.ledger.Put(other); err != nil {
		t.Fatal(err)
	}
	if err := h.l.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.ledger.All(); len(got) != 1 || got[0] != other {
		t.Fatalf("ledger %+v; want the other lever's entry untouched", got)
	}
	if acts := h.l.Actions(); len(acts) != 0 {
		t.Fatalf("actions %+v; want none taken up", acts)
	}
}

// blind is an actuator that cannot read the subjects in noRead, nor set those
// in noSet, as an API that answers for some pods and not others.
type blind struct {
	actuators.Actuator
	mu     sync.Mutex
	noRead map[string]bool
	noSet  map[string]bool
}

func (b *blind) see(subject string) {
	b.mu.Lock()
	delete(b.noRead, subject)
	delete(b.noSet, subject)
	b.mu.Unlock()
}

func (b *blind) Read(ctx context.Context, w runtime.Workload) (float64, bool, error) {
	b.mu.Lock()
	fail := b.noRead[w.Subject]
	b.mu.Unlock()
	if fail {
		return 0, false, errors.New("api unavailable")
	}
	return b.Actuator.Read(ctx, w)
}

func (b *blind) Set(ctx context.Context, w runtime.Workload, v float64, set bool) error {
	b.mu.Lock()
	fail := b.noSet[w.Subject]
	b.mu.Unlock()
	if fail {
		return errors.New("api unavailable")
	}
	return b.Actuator.Set(ctx, w, v, set)
}

func blindHarness(t *testing.T, noSet bool, subjects ...string) (*harness, *blind) {
	var b *blind
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4, "pod:b": 0.3},
		wrap: func(d *runtime.DryRun, a actuators.Actuator) (runtime.Runtime, actuators.Actuator) {
			b = &blind{Actuator: a, noRead: map[string]bool{}, noSet: map[string]bool{}}
			for _, s := range subjects {
				b.noRead[s] = true
				b.noSet[s] = noSet
			}
			return d, b
		}})
	return h, b
}

// TestAnUnreadableEntryDoesNotBlockTheRest: one entry whose setting cannot be
// read is kept untouched and shown as stuck with its error; the others are
// resumed and given back as usual, and the unreadable one is retried every tick
// and given back from the ledger's record once its expiry has passed.
func TestAnUnreadableEntryDoesNotBlockTheRest(t *testing.T) {
	h, b := blindHarness(t, false, "pod:b")
	record(t, h.m, contention, "", 0.5, h.now)
	h.seed(actuators.Entry{NodeID: "n1", Subject: "pod:a", Action: cpu.Action,
		Applied: 1.2, Expires: h.now.Add(30 * time.Minute), DecisionID: "relief-act-1-1"})
	h.seed(actuators.Entry{NodeID: "n1", Subject: "pod:b", Action: cpu.Action,
		Applied: 0.9, Expires: h.now.Add(40 * time.Minute), DecisionID: "relief-act-1-2"})
	if err := h.l.Reconcile(h.ctx); err != nil {
		t.Fatalf("reconcile: %v; one unreadable entry must not fail the rest", err)
	}
	st := h.l.Status()
	if st.InFlight == nil || st.InFlight.Subject != "pod:a" {
		t.Fatalf("in flight %+v; want pod:a resumed", st.InFlight)
	}
	if st.Stuck != 1 || len(st.Unread) != 1 || st.Unread[0].Entry.Subject != "pod:b" ||
		!strings.Contains(st.Unread[0].Error, "api unavailable") {
		t.Fatalf("status %+v; want pod:b stuck, listed with its error", st)
	}
	h.run(2*time.Minute, 0.5)
	if st := h.l.Status(); len(st.Unread) != 1 || st.Unread[0].Attempts < 8 {
		t.Fatalf("unread %+v; want pod:b retried every tick", st.Unread)
	}
	h.until(time.Hour, 0.5, "pod:a given back at expiry", func() bool { _, ok := h.applied()["pod:a"]; return !ok })
	if a := h.action("pod:a"); a.Outcome != OutcomeExpired {
		t.Fatalf("pod:a %+v; want expired", a)
	}
	h.until(time.Hour, 0.5, "pod:b given back at expiry", func() bool { return h.limit("pod:b") == nil })
	// Given back from the record, but not confirmed while it cannot be read: the
	// entry stays, and so does its place among the stuck.
	h.step(0.5)
	if _, ok := h.applied()["pod:b"]; !ok {
		t.Fatal("the entry went before the give-back could be confirmed")
	}
	if st := h.l.Status(); len(st.Unread) != 1 {
		t.Fatalf("status %+v; want pod:b still unread", st)
	}
	b.see("pod:b")
	h.step(0.5)
	if _, ok := h.applied()["pod:b"]; ok {
		t.Fatal("pod:b's entry is still held once it reads back given back")
	}
	if a := h.action("pod:b"); a.Outcome != OutcomeExpired {
		t.Fatalf("pod:b %+v; want expired", a)
	}
	if st := h.l.Status(); st.Stuck != 0 || len(st.Unread) != 0 {
		t.Fatalf("status %+v; want nothing stuck", st)
	}
}

// TestAnUnreadableEntryIsGivenBackOnceItCanBe: an entry that can be neither read
// nor set stays in force past its expiry, stuck; once the actuator answers again
// it is given back.
func TestAnUnreadableEntryIsGivenBackOnceItCanBe(t *testing.T) {
	h, b := blindHarness(t, true, "pod:b")
	h.seed(actuators.Entry{NodeID: "n1", Subject: "pod:b", Action: cpu.Action,
		Applied: 0.9, Expires: h.now.Add(10 * time.Minute), DecisionID: "relief-act-1-2"})
	if err := h.l.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.run(15*time.Minute, 0.05)
	if _, ok := h.applied()["pod:b"]; !ok {
		t.Fatal("an entry that could not be set was dropped")
	}
	if st := h.l.Status(); st.Stuck != 1 || len(st.Unread) != 1 {
		t.Fatalf("status %+v; want pod:b stuck past its expiry", st)
	}
	b.see("pod:b")
	h.step(0.05)
	h.step(0.05)
	if _, ok := h.applied()["pod:b"]; ok || h.limit("pod:b") != nil {
		t.Fatalf("applied %v limit %v; want pod:b given back once readable", h.applied(), h.limit("pod:b"))
	}
	if a := h.action("pod:b"); a.Outcome != OutcomeExpired {
		t.Fatalf("pod:b %+v; want expired", a)
	}
}

// unreadable is an actuator whose Read fails while fail is set.
type unreadable struct {
	actuators.Actuator
	mu   sync.Mutex
	fail bool
}

func (u *unreadable) set(v bool) { u.mu.Lock(); u.fail = v; u.mu.Unlock() }

func (u *unreadable) Read(ctx context.Context, w runtime.Workload) (float64, bool, error) {
	u.mu.Lock()
	fail := u.fail
	u.mu.Unlock()
	if fail {
		return 0, false, errors.New("api unavailable")
	}
	return u.Actuator.Read(ctx, w)
}

// TestAnAdoptedChangeKeepsItsSpend: an Apply that failed yet left the change in
// force (adopted) is an action in force, so its hourly slot is not refunded for
// another lever to take before the next pass takes the change up; that pass
// does not count it twice either. One that changed nothing is refunded.
func TestAnAdoptedChangeKeepsItsSpend(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4},
		wrap: func(d *runtime.DryRun, a actuators.Actuator) (runtime.Runtime, actuators.Actuator) {
			return d, &lyingSet{Actuator: a, fail: 1}
		}})
	h.act("pod:a")
	if n := h.l.budget.Count(h.now); n != 1 {
		t.Fatalf("hourly count %d after an adopted change; want 1, the change is in force", n)
	}
	h.step(0.5)
	if f := h.l.Status().InFlight; f == nil || !f.Resumed {
		t.Fatalf("in flight %+v; want the adopted change taken up", f)
	}
	if n := h.l.budget.Count(h.now); n != 1 {
		t.Fatalf("hourly count %d after it was taken up; want 1, not counted twice", n)
	}
}

// TestARefusedChangeIsRefunded: an Apply whose Set failed and changed nothing
// keeps its hourly slot while the step may still land, and gives it back once
// the step is settled as never taken.
func TestARefusedChangeIsRefunded(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4}, flaky: true})
	h.fl.set(func(f *flaky) { f.stuck["pod:a"] = true })
	h.until(2*time.Hour, 0.5, "a refused change", func() bool { return h.l.Status().Refused > 0 })
	// A refusal may still have landed: the step is pending, its spend kept.
	if n := h.l.budget.Count(h.now); n != 1 || len(h.ledger.All()) != 1 || !h.ledger.All()[0].Pending {
		t.Fatalf("hourly count %d, ledger %+v; want the step pending with its spend", n, h.ledger.All())
	}
	h.until(10*time.Minute, 0.5, "the step settled", func() bool { return len(h.ledger.All()) == 0 })
	if n := h.l.budget.Count(h.now); n != 0 || len(h.decisions(OutcomeNotApplied)) != 1 {
		t.Fatalf("hourly count %d; want the slot given back once it never took", n)
	}
}

// TestTheDescriptorIsTheActuators: a loop takes its descriptor from the
// actuator of its lever's action, so the two cannot disagree; a lever with no
// actuator is refused, naming the action.
func TestTheDescriptorIsTheActuators(t *testing.T) {
	h := newHarness(t, fixture{})
	lever := spec.Default().Loops.Relief.Levers[0]
	cfg := Config{Interval: tick, Lever: lever, NodeID: "n1", Runtime: h.rt,
		Acting: actuators.NewActing("n1", h.ledger, h.actuator), Policy: h.l.cfg.Policy, Map: h.m}
	l, err := New(cfg)
	if err != nil {
		t.Fatalf("New without a separate descriptor: %v", err)
	}
	if got := l.Status().Action; got != cpu.Action {
		t.Fatalf("action %q; want %q", got, cpu.Action)
	}
	cfg.Acting = actuators.NewActing("n1", h.ledger)
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), lever.Action) {
		t.Fatalf("err %v; want a refusal naming %s", err, lever.Action)
	}
}

// blindAfterSet is an actuator that cannot read a workload from its first Set
// until see is called, as an API that stops answering right after a resize.
type blindAfterSet struct {
	actuators.Actuator
	mu    sync.Mutex
	blind map[string]bool
	seen  bool
}

func (b *blindAfterSet) see() { b.mu.Lock(); b.seen = true; b.mu.Unlock() }

func (b *blindAfterSet) Set(ctx context.Context, w runtime.Workload, v float64, set bool) error {
	b.mu.Lock()
	if !b.seen {
		b.blind[w.Subject] = true
	}
	b.mu.Unlock()
	return b.Actuator.Set(ctx, w, v, set)
}

func (b *blindAfterSet) Read(ctx context.Context, w runtime.Workload) (float64, bool, error) {
	b.mu.Lock()
	fail := b.blind[w.Subject] && !b.seen
	b.mu.Unlock()
	if fail {
		return 0, false, errors.New("api unavailable")
	}
	return b.Actuator.Read(ctx, w)
}

// TestAPendingChangeInFlightIsResolved: a change applied and not read back is in
// flight with a pending entry. While it cannot be read it is shown as unread,
// with its error; once it reads, the entry Held settles is the action's own.
func TestAPendingChangeInFlightIsResolved(t *testing.T) {
	var b *blindAfterSet
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4},
		wrap: func(d *runtime.DryRun, a actuators.Actuator) (runtime.Runtime, actuators.Actuator) {
			b = &blindAfterSet{Actuator: a, blind: map[string]bool{}}
			return d, b
		}})
	h.act("pod:a")
	if f := h.l.Status().InFlight; f == nil || !f.Entry.Pending {
		t.Fatalf("in flight %+v; want the change pending", f)
	}
	h.step(0.5)
	st := h.l.Status()
	if len(st.Unread) != 1 || st.Unread[0].Entry.Subject != "pod:a" || !strings.Contains(st.Unread[0].Error, "api unavailable") || st.Stuck < 1 {
		t.Fatalf("status %+v; want the pending change shown unread with its error", st)
	}
	b.see()
	h.step(0.5)
	st = h.l.Status()
	if len(st.Unread) != 0 {
		t.Fatalf("unread %+v once it reads", st.Unread)
	}
	a := h.action("pod:a")
	if a.Entry.Pending || a.Entry.Applied != h.applied()["pod:a"].Applied || h.applied()["pod:a"].Pending {
		t.Fatalf("action entry %+v, ledger %+v; want the settled entry, the same in both", a.Entry, h.applied()["pod:a"])
	}
}

// refusedUnread fails a Set without effect and the read after it once.
type refusedUnread struct {
	actuators.Actuator
	mu       sync.Mutex
	failNext bool
}

func (r *refusedUnread) Set(context.Context, runtime.Workload, float64, bool) error {
	r.mu.Lock()
	r.failNext = true
	r.mu.Unlock()
	return errors.New("conflict")
}

func (r *refusedUnread) Read(ctx context.Context, w runtime.Workload) (float64, bool, error) {
	r.mu.Lock()
	fail := r.failNext
	r.failNext = false
	r.mu.Unlock()
	if fail {
		return 0, false, errors.New("api unavailable")
	}
	return r.Actuator.Read(ctx, w)
}

// TestAPendingStepThatNeverTookIsRefunded: a failed Apply whose outcome is not
// known keeps its hourly spend, as it may be in force; once the reconcile finds
// it never took, the spend is given back.
func TestAPendingStepThatNeverTookIsRefunded(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4},
		wrap: func(d *runtime.DryRun, a actuators.Actuator) (runtime.Runtime, actuators.Actuator) {
			return d, &refusedUnread{Actuator: a}
		}})
	h.until(2*time.Hour, 0.5, "a refused change", func() bool { return h.l.Status().Refused > 0 })
	if n := h.l.budget.Count(h.now); n != 1 {
		t.Fatalf("hourly count %d with the step's outcome unknown; want 1", n)
	}
	// It may yet land while its step is within settle; past that it never took.
	h.step(0.5)
	if len(h.ledger.All()) != 1 {
		t.Fatalf("ledger %+v; want the step kept pending within settle", h.ledger.All())
	}
	h.until(10*time.Minute, 0.5, "the step settled", func() bool { return len(h.ledger.All()) == 0 })
	if n := h.l.budget.Count(h.now); n != 0 || h.decisions(OutcomeNotApplied) == nil {
		t.Fatalf("hourly count %d once it never took; want the spend back", n)
	}
}

// TestDropsBeforeAFailedSaveAreJournalled: a reconcile whose ledger save fails
// partway through still journals the changes it dropped before the failure.
func TestDropsBeforeAFailedSaveAreJournalled(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4, "pod:b": 0.3, "pod:c": 0.2}})
	for _, s := range []string{"pod:a", "pod:b"} {
		h.seed(actuators.Entry{NodeID: "n1", Subject: s, Action: cpu.Action, Applied: 0.9, Expires: h.now.Add(time.Hour), DecisionID: "d-" + s})
		h.rt.Remove(s)
	}
	testhooks.LedgerSave = testhooks.FailAfter(1, errors.New("disk full"))
	t.Cleanup(func() { testhooks.LedgerSave = nil })
	if err := h.l.Reconcile(h.ctx); err == nil {
		t.Fatal("want the save error")
	}
	if n := len(h.ledger.All()); n != 1 {
		t.Fatalf("%d entries; want one dropped before the failure", n)
	}
	if g := h.decisions("gone"); len(g) != 1 {
		t.Fatalf("%d gone decisions; want the drop that happened journalled", len(g))
	}
}

// TestAnUnlistedCPULimitChangeIsTakenUpAndGivenBack: after a restart, a cpu.limit
// change whose pod the listing has lost but the actuator still finds by subject
// is taken up, not dropped, and given back through its subject at expiry.
func TestAnUnlistedCPULimitChangeIsTakenUpAndGivenBack(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4, "pod:b": 0.3}, flaky: true})
	h.seed(actuators.Entry{NodeID: "n1", Subject: "pod:b", Action: cpu.Action,
		Applied: 0.9, Expires: h.now.Add(10 * time.Minute), DecisionID: "relief-act-1-1"})
	h.fl.set(func(f *flaky) { f.hide["pod:b"] = true })
	if err := h.l.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.applied()["pod:b"]; !ok || len(h.decisions("gone")) != 0 {
		t.Fatalf("applied %v, gone %d; want pod:b taken up, not dropped", h.applied(), len(h.decisions("gone")))
	}
	h.until(20*time.Minute, 0.05, "pod:b given back", func() bool { _, ok := h.applied()["pod:b"]; return !ok })
	if h.limit("pod:b") != nil {
		t.Fatalf("pod:b limit %v; want none, its original", *h.limit("pod:b"))
	}
	if a := h.action("pod:b"); a.Outcome == OutcomeGone {
		t.Fatalf("pod:b %+v; want given back, not gone", a)
	}
}

// TestAnUnreadEntryWhoseWorkloadLeftIsGone: an entry that could not be read and
// whose workload the actuator no longer finds at its expiry is journalled gone.
func TestAnUnreadEntryWhoseWorkloadLeftIsGone(t *testing.T) {
	h, _ := blindHarness(t, false, "pod:b")
	h.seed(actuators.Entry{NodeID: "n1", Subject: "pod:b", Action: cpu.Action,
		Applied: 0.9, Expires: h.now.Add(5 * time.Minute), DecisionID: "relief-act-1-2"})
	if err := h.l.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.rt.Remove("pod:b")
	h.until(10*time.Minute, 0.05, "pod:b closed", func() bool { _, ok := h.applied()["pod:b"]; return !ok })
	if a := h.action("pod:b"); a.Outcome != OutcomeGone {
		t.Fatalf("pod:b %+v; want gone", a)
	}
	if st := h.l.Status(); len(st.Unread) != 0 {
		t.Fatalf("unread %+v; want none", st.Unread)
	}
}

// lands is an actuator whose Set takes effect only after k more reads, as a
// kubelet that applies an accepted resize a moment later; k < 0 never lands.
type lands struct {
	actuators.Actuator
	mu      sync.Mutex
	k       int
	next    []int // k for each next Set, then k
	left    int
	pending *struct {
		w   runtime.Workload
		v   float64
		set bool
	}
}

func (l *lands) Set(ctx context.Context, w runtime.Workload, v float64, set bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pending = &struct {
		w   runtime.Workload
		v   float64
		set bool
	}{w, v, set}
	if len(l.next) > 0 {
		l.k, l.next = l.next[0], l.next[1:]
	}
	l.left = l.k
	return nil
}

func (l *lands) Read(ctx context.Context, w runtime.Workload) (float64, bool, error) {
	l.mu.Lock()
	if p := l.pending; p != nil && p.w.Subject == w.Subject && l.k >= 0 {
		if l.left == 0 {
			l.pending = nil
			l.mu.Unlock()
			if err := l.Actuator.Set(ctx, p.w, p.v, p.set); err != nil {
				return 0, false, err
			}
			return l.Actuator.Read(ctx, w)
		}
		l.left--
	}
	l.mu.Unlock()
	return l.Actuator.Read(ctx, w)
}

func landsHarness(t *testing.T, k int) (*harness, *lands) {
	var ln *lands
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4},
		wrap: func(d *runtime.DryRun, a actuators.Actuator) (runtime.Runtime, actuators.Actuator) {
			ln = &lands{Actuator: a, k: k}
			return d, ln
		}})
	return h, ln
}

// TestACutThatLandsLateIsKept: a cut applied and read back as before is kept
// pending while it is in flight, and is committed at the cut when it lands: the
// ledger never loses it while it may be in force.
func TestACutThatLandsLateIsKept(t *testing.T) {
	h, _ := landsHarness(t, 4)
	h.act("pod:a")
	cut := h.applied()["pod:a"].Applied
	for i := 0; i < 6; i++ {
		h.step(0.5)
		if _, ok := h.applied()["pod:a"]; !ok {
			t.Fatalf("tick %d: the entry left the ledger while the cut was on its way", i)
		}
	}
	e := h.applied()["pod:a"]
	if e.Pending || e.Applied != cut || h.limit("pod:a") == nil || *h.limit("pod:a") != cut {
		t.Fatalf("entry %+v, limit %v; want the cut %v committed once it landed", e, h.limit("pod:a"), cut)
	}
	if a := h.action("pod:a"); a.Entry.Pending || a.Entry.Applied != cut {
		t.Fatalf("action entry %+v; want it in step with the ledger", a.Entry)
	}
}

// TestACutThatNeverLandsIsClosed: a cut that still reads as before once its
// settle has passed never took: the action is closed as not applied, its entry
// deleted and its hourly spend given back, all explicitly.
func TestACutThatNeverLandsIsClosed(t *testing.T) {
	h, _ := landsHarness(t, -1)
	h.act("pod:a")
	h.until(10*time.Minute, 0.5, "the action closed", func() bool { return h.action("pod:a").Outcome != "" })
	if a := h.action("pod:a"); a.Outcome != OutcomeNotApplied {
		t.Fatalf("pod:a %+v; want not applied", a)
	}
	if len(h.ledger.All()) != 0 || h.l.budget.Count(h.now) != 0 {
		t.Fatalf("ledger %+v, count %d; want the entry gone and the spend given back", h.ledger.All(), h.l.budget.Count(h.now))
	}
	if n := len(h.decisions("not_applied")); n != 1 {
		t.Fatalf("%d not_applied decisions; want the verdict journalled", n)
	}
}

// TestAFurtherStepThatNeverLandsGoesBackAndIsRefunded: a further step that
// still reads as the step before once Settle has passed never took: the ledger
// goes back to the earlier step, the action stays kept at it, and the further
// step's hourly spend is given back.
func TestAFurtherStepThatNeverLandsGoesBackAndIsRefunded(t *testing.T) {
	h, ln := landsHarness(t, 0)
	h.act("pod:a")
	first := h.applied()["pod:a"].Applied
	h.until(10*time.Minute, 0.44, "the first step kept", func() bool { return h.action("pod:a").Outcome == OutcomeKept })
	ln.mu.Lock()
	ln.next = []int{-1} // the further step never lands
	ln.mu.Unlock()
	before := h.l.budget.Count(h.now)
	h.until(time.Hour, 0.44, "a further step", func() bool {
		record(h.t, h.m, usage("pod:a"), "pod:a", 0.28, h.clock())
		e := h.applied()["pod:a"]
		return e.Pending && e.Applied < first
	})
	if n := h.l.budget.Count(h.now); n != before+1 {
		t.Fatalf("count %d; want the further step spent (%d)", n, before+1)
	}
	h.until(10*time.Minute, 0.44, "the further step settled", func() bool { return !h.applied()["pod:a"].Pending })
	if e := h.applied()["pod:a"]; e.Applied != first {
		t.Fatalf("entry %+v; want it back at the first step %v", e, first)
	}
	if a := h.action("pod:a"); a.Outcome != OutcomeKept || a.Entry.Applied != first {
		t.Fatalf("action %+v; want it kept at the first step", a)
	}
	if n := h.l.budget.Count(h.now); n != before {
		t.Fatalf("count %d; want the further step's spend given back (%d)", n, before)
	}
}

// TestAClosedChangeClearsItsKeptSpend: a change whose failed Apply kept its
// spend, once closed, leaves nothing behind for a later reconcile to refund or
// keep: its spend stays counted, once.
func TestAClosedChangeClearsItsKeptSpend(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4},
		wrap: func(d *runtime.DryRun, a actuators.Actuator) (runtime.Runtime, actuators.Actuator) {
			return d, &lyingSet{Actuator: a, fail: 1}
		}})
	h.act("pod:a")
	h.l.Pause()
	h.until(2*time.Hour, 0.5, "expiry", func() bool { return len(h.ledger.All()) == 0 })
	if len(h.l.spent) != 0 {
		t.Fatalf("spent %v after the change closed; want it settled", h.l.spent)
	}
	n := h.l.budget.Count(h.now.Add(-30 * time.Minute))
	h.l.reconciled = false
	h.step(0.5)
	if got := h.l.budget.Count(h.now.Add(-30 * time.Minute)); got != n {
		t.Fatalf("count %d → %d after a later reconcile; a closed change's spend must not move", n, got)
	}
}

// TestAGivenBackChangeAnotherWriterChangedIsGone: a change given back from its
// record while unreadable, that then reads as another writer's value rather than
// its original, is closed as gone, saying so, not as a confirmed expiry.
func TestAGivenBackChangeAnotherWriterChangedIsGone(t *testing.T) {
	h, b := blindHarness(t, false, "pod:b")
	h.seed(actuators.Entry{NodeID: "n1", Subject: "pod:b", Action: cpu.Action,
		Applied: 0.9, Expires: h.now.Add(5 * time.Minute), DecisionID: "relief-act-1-2"})
	if err := h.l.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.until(10*time.Minute, 0.05, "the give-back", func() bool { return h.limit("pod:b") == nil })
	if err := h.actuator.Set(h.ctx, runtime.Workload{Subject: "pod:b"}, 2, true); err != nil { // another controller
		t.Fatal(err)
	}
	b.see("pod:b")
	h.step(0.05)
	if a := h.action("pod:b"); a.Outcome != OutcomeGone {
		t.Fatalf("pod:b %+v; want gone, the setting being another writer's", a)
	}
	g := h.decisions("gone")
	if len(g) != 1 || !strings.Contains(g[0].Rationale, "another writer") {
		t.Fatalf("gone decisions %+v; want one naming another writer", g)
	}
	if l := h.limit("pod:b"); l == nil || *l != 2 {
		t.Fatalf("limit %v; the other writer's setting must be left alone", l)
	}
}

// counting counts the Sets of the actuator it wraps.
type counting struct {
	actuators.Actuator
	mu   sync.Mutex
	sets map[string]int
}

func (c *counting) Set(ctx context.Context, w runtime.Workload, v float64, set bool) error {
	c.mu.Lock()
	c.sets[w.Subject]++
	c.mu.Unlock()
	return c.Actuator.Set(ctx, w, v, set)
}

// TestAGiveBackIsNotRewrittenWithoutBound: a given-back change that never reads
// back is set again only a few times, with backoff, then left for an operator,
// still shown as stuck: never a fight with another controller every tick.
func TestAGiveBackIsNotRewrittenWithoutBound(t *testing.T) {
	var c *counting
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4, "pod:b": 0.3},
		wrap: func(d *runtime.DryRun, a actuators.Actuator) (runtime.Runtime, actuators.Actuator) {
			bl := &blind{Actuator: a, noRead: map[string]bool{"pod:b": true}, noSet: map[string]bool{}}
			c = &counting{Actuator: bl, sets: map[string]int{}}
			return d, c
		}})
	h.seed(actuators.Entry{NodeID: "n1", Subject: "pod:b", Action: cpu.Action,
		Applied: 0.9, Expires: h.now.Add(5 * time.Minute), DecisionID: "relief-act-1-2"})
	if err := h.l.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.run(2*time.Hour, 0.05)
	c.mu.Lock()
	n := c.sets["pod:b"]
	c.mu.Unlock()
	if n < 1 || n > 4 {
		t.Fatalf("%d Sets of pod:b over two hours; want the give-back and at most 3 more", n)
	}
	if st := h.l.Status(); len(st.Unread) != 1 || st.Stuck < 1 || !st.Unread[0].GivenBack {
		t.Fatalf("status %+v; want it still shown stuck, given back", st)
	}
}

// TestAStuckChangeInForceCountsOnce: a change in force whose undo was refused and
// whose pending entry cannot be read is one stuck change, not two.
func TestAStuckChangeInForceCountsOnce(t *testing.T) {
	var b *blindAfterSet
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4},
		wrap: func(d *runtime.DryRun, a actuators.Actuator) (runtime.Runtime, actuators.Actuator) {
			b = &blindAfterSet{Actuator: a, blind: map[string]bool{}}
			return d, b
		}})
	h.act("pod:a")
	h.step(0.5)
	h.l.mu.Lock()
	h.l.inFlight.RevertAttempts, h.l.inFlight.LastError = 1, "refused"
	h.l.mu.Unlock()
	if st := h.l.Status(); len(st.Unread) != 1 || st.Stuck != 1 {
		t.Fatalf("status stuck %d unread %d; want one stuck change", st.Stuck, len(st.Unread))
	}
}

// timesOut is an actuator whose Set on the subjects in it reports an error
// without effect and whose next read fails, as a PATCH that timed out on the
// client; the change then lands on the server once read again after that.
type timesOut struct {
	actuators.Actuator
	mu      sync.Mutex
	on      map[string]bool
	pending map[string]float64
	stage   map[string]int // 1: next read fails; 2: next read is the old value, then it lands
}

func (t *timesOut) Set(ctx context.Context, w runtime.Workload, v float64, set bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.on[w.Subject] {
		return t.Actuator.Set(ctx, w, v, set)
	}
	delete(t.on, w.Subject)
	t.pending[w.Subject], t.stage[w.Subject] = v, 1
	return errors.New("context deadline exceeded")
}

func (t *timesOut) Read(ctx context.Context, w runtime.Workload) (float64, bool, error) {
	t.mu.Lock()
	st := t.stage[w.Subject]
	switch st {
	case 1:
		t.stage[w.Subject] = 2
		t.mu.Unlock()
		return 0, false, errors.New("context deadline exceeded")
	case 2:
		v := t.pending[w.Subject]
		delete(t.stage, w.Subject)
		t.mu.Unlock()
		old, set, err := t.Actuator.Read(ctx, w)
		if err != nil {
			return 0, false, err
		}
		if err := t.Actuator.Set(ctx, w, v, true); err != nil { // lands after this read
			return 0, false, err
		}
		return old, set, nil
	}
	t.mu.Unlock()
	return t.Actuator.Read(ctx, w)
}

// TestATimedOutPatchThatLandsLaterIsRecorded: a change whose Set timed out and
// could not be read back has no action, only a pending entry; that entry is
// not settled as never taken while its step is within settle, so when the
// server applies it a moment later it is committed, never deleted.
func TestATimedOutPatchThatLandsLaterIsRecorded(t *testing.T) {
	var to *timesOut
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4},
		wrap: func(d *runtime.DryRun, a actuators.Actuator) (runtime.Runtime, actuators.Actuator) {
			to = &timesOut{Actuator: a, on: map[string]bool{"pod:a": true}, pending: map[string]float64{}, stage: map[string]int{}}
			return d, to
		}})
	h.until(2*time.Hour, 0.5, "the timed-out step", func() bool { return h.l.Status().Refused > 0 })
	cut := h.applied()["pod:a"].Applied
	for i := 0; i < 6; i++ {
		if _, ok := h.applied()["pod:a"]; !ok {
			t.Fatalf("tick %d: the entry was deleted while the change could still land", i)
		}
		h.step(0.5)
	}
	e, ok := h.applied()["pod:a"]
	if !ok || e.Pending || e.Applied != cut || h.limit("pod:a") == nil || *h.limit("pod:a") != cut {
		t.Fatalf("entry %+v (held %v), limit %v; want the landed cut %v committed", e, ok, h.limit("pod:a"), cut)
	}
}

// failNth is an actuator whose n-th Set reports an error without effect and
// whose read right after it fails: a step left pending that never lands.
type failNth struct {
	actuators.Actuator
	mu       sync.Mutex
	n, sets  int
	failRead bool
}

func (f *failNth) Set(ctx context.Context, w runtime.Workload, v float64, set bool) error {
	f.mu.Lock()
	f.sets++
	fail := f.sets == f.n
	f.failRead = fail
	f.mu.Unlock()
	if fail {
		return errors.New("context deadline exceeded")
	}
	return f.Actuator.Set(ctx, w, v, set)
}

func (f *failNth) Read(ctx context.Context, w runtime.Workload) (float64, bool, error) {
	f.mu.Lock()
	fail := f.failRead
	f.failRead = false
	f.mu.Unlock()
	if fail {
		return 0, false, errors.New("context deadline exceeded")
	}
	return f.Actuator.Read(ctx, w)
}

// TestAFailedThirdStepLeavesTheActionAtItsSecond: steps one and two take, the
// third's Apply fails and never lands. The action keeps its second step, plan
// and decision, and its entry goes back to the second step's record; it is not
// rolled back to the first.
func TestAFailedThirdStepLeavesTheActionAtItsSecond(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4},
		policy: func(ps *spec.PolicySpec) {
			low := 0.1
			b := ps.Defaults.Floor["cpu.limit"]
			b.OfOriginal = &low
			ps.Defaults.Floor["cpu.limit"] = b
		},
		wrap: func(d *runtime.DryRun, a actuators.Actuator) (runtime.Runtime, actuators.Actuator) {
			return d, &failNth{Actuator: a, n: 3}
		}})
	h.act("pod:a")
	h.until(10*time.Minute, 0.44, "the first step kept", func() bool { return h.action("pod:a").Outcome == OutcomeKept })
	first := h.applied()["pod:a"].DecisionID
	h.until(time.Hour, 0.44, "a second step", func() bool {
		record(h.t, h.m, usage("pod:a"), "pod:a", 0.28, h.clock())
		return h.applied()["pod:a"].DecisionID != first
	})
	h.until(10*time.Minute, 0.30, "the second step kept", func() bool { return h.action("pod:a").Outcome == OutcomeKept })
	second := h.action("pod:a")
	h.until(time.Hour, 0.30, "a refused third step", func() bool {
		record(h.t, h.m, usage("pod:a"), "pod:a", 0.18, h.clock())
		return h.l.Status().Refused > 0 && h.applied()["pod:a"].Pending
	})
	h.until(10*time.Minute, 0.30, "the third step settled", func() bool { return !h.applied()["pod:a"].Pending })
	a := h.action("pod:a")
	if a.DecisionID != second.DecisionID || a.Plan != second.Plan || a.Entry.Applied != second.Entry.Applied ||
		a.Entry.DecisionID != second.Entry.DecisionID || !a.Entry.Expires.Equal(second.Entry.Expires) {
		t.Fatalf("action %+v; want it at its second step %+v", a, second)
	}
	if e := h.applied()["pod:a"]; e.DecisionID != second.Entry.DecisionID || !e.Expires.Equal(second.Entry.Expires) {
		t.Fatalf("ledger %+v; want the second step's record back", e)
	}
}

// TestANeverTookCloseThatCannotSaveIsRetried: a change that never took whose
// settling cannot be saved stays pending, shows the error as reconcile_error and
// is settled by a later pass once the ledger can be written.
func TestANeverTookCloseThatCannotSaveIsRetried(t *testing.T) {
	h, _ := landsHarness(t, -1)
	h.act("pod:a")
	testhooks.LedgerSave = testhooks.FailAfter(0, errors.New("disk full"))
	t.Cleanup(func() { testhooks.LedgerSave = nil })
	h.run(4*time.Minute, 0.5)
	if e, ok := h.applied()["pod:a"]; !ok || !e.Pending {
		t.Fatalf("ledger %+v; want the change still pending while it cannot be settled", h.applied())
	}
	if st := h.l.Status(); !strings.Contains(st.ReconcileError, "disk full") {
		t.Fatalf("reconcile_error %q; want the save error", st.ReconcileError)
	}
	testhooks.LedgerSave = nil
	h.until(5*time.Minute, 0.5, "settled", func() bool { return len(h.ledger.All()) == 0 })
	if a := h.action("pod:a"); a.Outcome != OutcomeNotApplied {
		t.Fatalf("pod:a %+v; want not applied", a)
	}
	if st := h.l.Status(); st.ReconcileError != "" {
		t.Fatalf("reconcile_error %q after it settled; want none", st.ReconcileError)
	}
}

// timedSets records when each Set of the actuator it wraps happened.
type timedSets struct {
	actuators.Actuator
	mu    sync.Mutex
	clock func() time.Time
	at    []time.Time
}

func (s *timedSets) Set(ctx context.Context, w runtime.Workload, v float64, set bool) error {
	s.mu.Lock()
	s.at = append(s.at, s.clock())
	s.mu.Unlock()
	return s.Actuator.Set(ctx, w, v, set)
}

func givenBackHarness(t *testing.T) (*harness, *blind, *timedSets) {
	var b *blind
	var ts *timedSets
	var h *harness
	h = newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4, "pod:b": 0.3},
		wrap: func(d *runtime.DryRun, a actuators.Actuator) (runtime.Runtime, actuators.Actuator) {
			ts = &timedSets{Actuator: a, clock: func() time.Time { return h.clock() }}
			b = &blind{Actuator: ts, noRead: map[string]bool{"pod:b": true}, noSet: map[string]bool{}}
			return d, b
		}})
	h.seed(actuators.Entry{NodeID: "n1", Subject: "pod:b", Action: cpu.Action,
		Applied: 0.9, Expires: h.now.Add(5 * time.Minute), DecisionID: "relief-act-1-2"})
	ts.mu.Lock()
	ts.at = nil // the seed's own Set
	ts.mu.Unlock()
	if err := h.l.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	return h, b, ts
}

// TestAGiveBackBacksOffDoubling: after the give-back, the original is set again
// three times, waiting twice, four and eight times the loop's interval.
func TestAGiveBackBacksOffDoubling(t *testing.T) {
	h, _, ts := givenBackHarness(t)
	h.run(2*time.Hour, 0.05)
	ts.mu.Lock()
	at := append([]time.Time(nil), ts.at...)
	ts.mu.Unlock()
	if len(at) != 4 {
		t.Fatalf("%d Sets at %v; want the give-back and 3 more", len(at), at)
	}
	iv := h.l.cfg.Interval
	for i, want := range []time.Duration{2 * iv, 4 * iv, 8 * iv} {
		if got := at[i+1].Sub(at[i]); got != want {
			t.Errorf("wait %d: %s; want %s", i+1, got, want)
		}
	}
}

// TestAGiveBackRefusedOutrightKeepsItsBudget: a Set that fails outright, rather
// than going through without reading back, does not count toward the three
// re-sets; it backs off all the same.
func TestAGiveBackRefusedOutrightKeepsItsBudget(t *testing.T) {
	h, b, ts := givenBackHarness(t)
	h.until(10*time.Minute, 0.05, "the give-back", func() bool { ts.mu.Lock(); defer ts.mu.Unlock(); return len(ts.at) == 1 })
	b.mu.Lock()
	b.noSet["pod:b"] = true
	b.mu.Unlock()
	h.run(30*time.Minute, 0.05)
	b.mu.Lock()
	b.noSet["pod:b"] = false
	b.mu.Unlock()
	h.run(3*time.Hour, 0.05)
	ts.mu.Lock()
	n := len(ts.at)
	ts.mu.Unlock()
	if n != 4 {
		t.Fatalf("%d Sets that went through; want the give-back and 3 re-sets, refusals not counted", n)
	}
	if st := h.l.Status(); len(st.Unread) != 1 || !strings.Contains(st.Unread[0].Error, "3 more times") {
		t.Fatalf("unread %+v; want it left for an operator, saying so", st.Unread)
	}
}

// failsThenLands is an actuator whose Set on the subjects in it reports an
// error without effect, whose read-back right after succeeds with the value from
// before, and which then lands on the server one read later, as a PATCH the
// client gave up on.
type failsThenLands struct {
	actuators.Actuator
	mu      sync.Mutex
	on      map[string]bool
	pending map[string]float64
	reads   map[string]int
}

func (f *failsThenLands) Set(ctx context.Context, w runtime.Workload, v float64, set bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.on[w.Subject] {
		return f.Actuator.Set(ctx, w, v, set)
	}
	delete(f.on, w.Subject)
	f.pending[w.Subject], f.reads[w.Subject] = v, 0
	return errors.New("context deadline exceeded")
}

func (f *failsThenLands) Read(ctx context.Context, w runtime.Workload) (float64, bool, error) {
	f.mu.Lock()
	v, ok := f.pending[w.Subject]
	if ok {
		f.reads[w.Subject]++
		if f.reads[w.Subject] > 1 { // the read after the read-back: it has landed
			delete(f.pending, w.Subject)
			f.mu.Unlock()
			if err := f.Actuator.Set(ctx, w, v, true); err != nil {
				return 0, false, err
			}
			return f.Actuator.Read(ctx, w)
		}
	}
	f.mu.Unlock()
	return f.Actuator.Read(ctx, w)
}

// TestAFailedPatchThatReadsAsBeforeAndLandsIsRecorded: a Set that failed on the
// client and whose read-back showed the setting from before is not taken for a
// change that never took: the step stays pending, and when the server applies
// it a read later it is committed at the cut, never restored or deleted.
func TestAFailedPatchThatReadsAsBeforeAndLandsIsRecorded(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4},
		wrap: func(d *runtime.DryRun, a actuators.Actuator) (runtime.Runtime, actuators.Actuator) {
			return d, &failsThenLands{Actuator: a, on: map[string]bool{"pod:a": true}, pending: map[string]float64{}, reads: map[string]int{}}
		}})
	h.until(2*time.Hour, 0.5, "the failed step", func() bool { return h.l.Status().Refused > 0 })
	e, ok := h.applied()["pod:a"]
	if !ok || !e.Pending {
		t.Fatalf("ledger %+v; want the failed step pending, not restored", h.applied())
	}
	cut := e.Applied
	for i := 0; i < 4; i++ {
		h.step(0.5)
		if _, ok := h.applied()["pod:a"]; !ok {
			t.Fatalf("tick %d: the entry was deleted while the change could still land", i)
		}
	}
	if e := h.applied()["pod:a"]; e.Pending || e.Applied != cut || h.limit("pod:a") == nil || *h.limit("pod:a") != cut {
		t.Fatalf("entry %+v, limit %v; want the landed cut %v committed", e, h.limit("pod:a"), cut)
	}
}

// failSets is an actuator whose Sets numbered in fail (counting from 1, every
// subject) report an error without effect, as a PATCH the client gave up on.
// The read right after such a Set returns the setting from before, and so do
// the next `old` reads of that subject; the read after those lands it, as the
// server applying the request late. old < 0 never lands.
type failSets struct {
	actuators.Actuator
	mu      sync.Mutex
	fail    map[int]bool
	old     int
	sets    int
	failed  int
	pending map[string]float64
	reads   map[string]int
}

func newFailSets(old int, fail ...int) func(*runtime.DryRun, actuators.Actuator) (runtime.Runtime, actuators.Actuator) {
	return func(d *runtime.DryRun, a actuators.Actuator) (runtime.Runtime, actuators.Actuator) {
		f := &failSets{Actuator: a, fail: map[int]bool{}, old: old, pending: map[string]float64{}, reads: map[string]int{}}
		for _, n := range fail {
			f.fail[n] = true
		}
		return d, f
	}
}

func (f *failSets) Set(ctx context.Context, w runtime.Workload, v float64, set bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets++
	if !f.fail[f.sets] {
		return f.Actuator.Set(ctx, w, v, set)
	}
	f.failed++
	f.pending[w.Subject], f.reads[w.Subject] = v, 0
	return errors.New("context deadline exceeded")
}

func (f *failSets) Read(ctx context.Context, w runtime.Workload) (float64, bool, error) {
	f.mu.Lock()
	v, ok := f.pending[w.Subject]
	if ok && f.old >= 0 {
		f.reads[w.Subject]++
		if f.reads[w.Subject] > 1+f.old { // the read-back and `old` more saw it as before
			delete(f.pending, w.Subject)
			f.mu.Unlock()
			if err := f.Actuator.Set(ctx, w, v, true); err != nil {
				return 0, false, err
			}
			return f.Actuator.Read(ctx, w)
		}
	}
	f.mu.Unlock()
	return f.Actuator.Read(ctx, w)
}

func (f *failSets) counts() (sets, failed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sets, f.failed
}

// secondStepThatDoesNotHelp takes a first step on pod:a, keeps it, then a second
// that does not help, as TestAFailedSecondStepReturnsToTheFirst, up to the
// second step applied. It returns the first step's entry.
func secondStepThatDoesNotHelp(h *harness) actuators.Entry {
	h.t.Helper()
	h.act("pod:a")
	h.run(2*time.Minute, 0.44)
	first := h.applied()["pod:a"]
	record(h.t, h.m, usage("pod:a"), "pod:a", 0.3, h.now)
	h.until(3*time.Minute, 0.44, "a second step", func() bool { return near(h.applied()["pod:a"].Applied, 0.9) })
	return first
}

// revertOwed reports whether the action in flight still owes its revert.
func (h *harness) revertOwed() bool {
	h.l.mu.Lock()
	defer h.l.mu.Unlock()
	return h.l.inFlight != nil && h.l.inFlight.revertPending
}

// TestARestoreThatTimesOutAndLandsIsRecorded: the restore of the first step,
// after a second that did not help, times out on the client and lands a few
// reads later. It is not settled as never taken at once (its settle runs from
// its own write, not from the earlier step's act), so the ledger never goes
// back to the failed step: it ends committed at the restored cap, the revert
// owed until then, nothing refunded.
func TestARestoreThatTimesOutAndLandsIsRecorded(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4}, wrap: newFailSets(3, 3)})
	fs := h.seenAct.(*failSets)
	first := secondStepThatDoesNotHelp(h)
	spent := h.l.budget.Count(h.now)
	h.until(3*time.Minute, 0.44, "the restore attempted", func() bool { _, f := fs.counts(); return f == 1 })
	for i := 0; i < 12; i++ {
		e, ok := h.applied()["pod:a"]
		if !ok {
			t.Fatalf("tick %d: the entry left the ledger while the restore could still land", i)
		}
		if !e.Pending && e.DecisionID == first.DecisionID {
			break
		}
		if e.Pending && !h.revertOwed() {
			t.Fatalf("tick %d: the restore is unsettled and the revert is no longer owed", i)
		}
		if !e.Pending && e.DecisionID != first.DecisionID {
			t.Fatalf("tick %d: entry %+v; the failed step was put back while the restore could still land", i, e)
		}
		h.step(0.44)
	}
	e := h.applied()["pod:a"]
	if e.Pending || e.DecisionID != first.DecisionID || !near(e.Applied, first.Applied) || h.limit("pod:a") == nil || !near(*h.limit("pod:a"), first.Applied) {
		t.Fatalf("entry %+v, limit %v; want the restored first step %+v committed and in force", e, h.limit("pod:a"), first)
	}
	if a := h.action("pod:a"); a.Outcome != OutcomeKept || !near(a.Entry.Applied, first.Applied) || h.revertOwed() {
		t.Fatalf("action %+v; want it kept at the first step, the revert done", a)
	}
	if n := h.l.budget.Count(h.now); n != spent {
		t.Fatalf("hourly count %d; want %d: a restore spends nothing and refunds nothing", n, spent)
	}
	if st := h.l.Status(); st.Reverted != 1 || len(h.decisions(OutcomeNotApplied)) != 0 {
		t.Fatalf("reverted %d, not_applied %d; want one revert, no never-took verdict", st.Reverted, len(h.decisions(OutcomeNotApplied)))
	}
}

// TestARestoreThatNeverTakesIsRetried: a restore that genuinely never takes is
// settled after its own settle with the failed step back in force; the revert
// is still owed and retried, the action is not kept by that verdict, and
// nothing is refunded: a restore never spent.
func TestARestoreThatNeverTakesIsRetried(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4}, wrap: newFailSets(-1, 3, 4)})
	fs := h.seenAct.(*failSets)
	first := secondStepThatDoesNotHelp(h)
	spent := h.l.budget.Count(h.now)
	h.until(10*time.Minute, 0.44, "the restore settled as never taken", func() bool { return len(h.decisions(OutcomeNotApplied)) > 0 })
	if !h.revertOwed() {
		t.Fatalf("action %+v; want the revert still owed once the restore never took", h.action("pod:a"))
	}
	if a := h.action("pod:a"); a.Outcome != "" {
		t.Fatalf("action %+v; want it still in flight, not closed or kept by the restore's verdict", a)
	}
	if n := h.l.budget.Count(h.now); n != spent {
		t.Fatalf("hourly count %d; want %d: a restore that never took refunds nothing", n, spent)
	}
	h.until(10*time.Minute, 0.44, "the revert retried until it took", func() bool {
		e := h.applied()["pod:a"]
		return !e.Pending && e.DecisionID == first.DecisionID
	})
	if sets, failed := fs.counts(); failed != 2 || sets < 5 {
		t.Fatalf("%d sets, %d failed; want the restore retried after each never took", sets, failed)
	}
	if a := h.action("pod:a"); a.Outcome != OutcomeKept || !near(a.Entry.Applied, first.Applied) || h.revertOwed() {
		t.Fatalf("action %+v; want it kept at the first step once the revert took", a)
	}
	if n := h.l.budget.Count(h.now); n != spent {
		t.Fatalf("hourly count %d; want %d", n, spent)
	}
}

// TestARaisedExpiryDoesNotSettleAPendingStepEarly: a failed step written before
// a restart that raised the lever's expiry still waits settle from its own
// write: it is not settled as never taken at once, and when it lands it is
// committed, never left in force unrecorded.
func TestARaisedExpiryDoesNotSettleAPendingStepEarly(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4}, wrap: newFailSets(3, 1)})
	h.until(2*time.Hour, 0.5, "the failed step", func() bool { return h.l.Status().Refused > 0 })
	e, ok := h.applied()["pod:a"]
	if !ok || !e.Pending {
		t.Fatalf("ledger %+v; want the failed step pending", h.applied())
	}
	cut := e.Applied
	lever := spec.Default().Loops.Relief.Levers[0]
	lever.Expiry = 3 * time.Hour
	h.l = h.newLoop(lever)
	if err := h.l.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, ok := h.applied()["pod:a"]; !ok {
			t.Fatalf("tick %d: the entry was settled while the change could still land", i)
		}
		h.step(0.5)
	}
	if e := h.applied()["pod:a"]; e.Pending || e.Applied != cut || h.limit("pod:a") == nil || *h.limit("pod:a") != cut {
		t.Fatalf("entry %+v, limit %v; want the landed cut %v committed", e, h.limit("pod:a"), cut)
	}
}

// TestNothingIsAppliedOverAnAwaitingStep: a failed step whose outcome is not
// yet known holds its workload: the loop never plans over it, its spend is
// refunded exactly once when it settles as never taken, however the lever's
// expiry or settle moved since it was written.
func TestNothingIsAppliedOverAnAwaitingStep(t *testing.T) {
	setup := func(t *testing.T) (*harness, *counting) {
		var c *counting
		h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4},
			wrap: func(d *runtime.DryRun, a actuators.Actuator) (runtime.Runtime, actuators.Actuator) {
				c = &counting{Actuator: &refusedUnread{Actuator: a}, sets: map[string]int{}}
				return d, c
			}})
		h.until(2*time.Hour, 0.5, "the failed step", func() bool { return h.l.Status().Refused > 0 })
		return h, c
	}
	sets := func(c *counting) int { c.mu.Lock(); defer c.mu.Unlock(); return c.sets["pod:a"] }
	t.Run("expiry lowered", func(t *testing.T) {
		h, c := setup(t)
		h.l.c.Expiry = 5 * time.Minute // as a config change the loop picks up
		h.until(time.Hour, 0.5, "the step settled", func() bool { return len(h.ledger.All()) == 0 })
		if n := sets(c); n != 1 {
			t.Fatalf("%d Sets of pod:a before its step settled; want only the failed one", n)
		}
		if n := h.l.budget.Count(h.now); n != 0 || len(h.decisions(OutcomeNotApplied)) != 1 {
			t.Fatalf("hourly count %d; want the one spend refunded once", n)
		}
	})
	t.Run("settle outlasting the cooldown", func(t *testing.T) {
		h, c := setup(t)
		h.l.c.Settle = 30 * time.Minute
		h.run(25*time.Minute, 0.5)
		if n, st := sets(c), h.l.Status(); n != 1 || st.Refused != 1 || len(h.decisions("act")) != 0 {
			t.Fatalf("%d Sets, %d refused, %d acts while the step awaits; want none planned over it", n, st.Refused, len(h.decisions("act")))
		}
		h.l.Pause() // once settled, its cooldown long over, the loop would plan it again in the same pass
		h.until(10*time.Minute, 0.5, "the step settled", func() bool { return len(h.ledger.All()) == 0 })
		if n := h.l.budget.Count(h.now); n != 0 {
			t.Fatalf("hourly count %d; want the spend refunded", n)
		}
	})
}

// rereadFails is an actuator whose Set fails without effect, the read after it
// failing too. Once armed, it answers only the first read of a workload at any
// one moment: a second read in the same pass, as the re-read before a step is
// settled as never taken, fails.
type rereadFails struct {
	actuators.Actuator
	clock    func() time.Time
	mu       sync.Mutex
	armed    bool
	failNext bool
	last     map[string]time.Time
}

func (r *rereadFails) arm() { r.mu.Lock(); r.armed = true; r.mu.Unlock() }

func (r *rereadFails) Set(ctx context.Context, w runtime.Workload, v float64, set bool) error {
	r.mu.Lock()
	r.failNext = true
	r.mu.Unlock()
	return errors.New("context deadline exceeded")
}

func (r *rereadFails) Read(ctx context.Context, w runtime.Workload) (float64, bool, error) {
	r.mu.Lock()
	now := r.clock()
	fail := r.failNext || (r.armed && r.last[w.Subject].Equal(now))
	r.failNext = false
	r.last[w.Subject] = now
	r.mu.Unlock()
	if fail {
		return 0, false, errors.New("api unavailable")
	}
	return r.Actuator.Read(ctx, w)
}

// TestARereadThatKeepsFailingIsShown: a step that never took whose re-read
// before settling keeps failing is not left awaiting unseen: it is unread with
// its error, counted stuck, the reconcile error says why, and it is logged.
func TestARereadThatKeepsFailingIsShown(t *testing.T) {
	var h *harness
	var r *rereadFails
	h = newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4},
		wrap: func(d *runtime.DryRun, a actuators.Actuator) (runtime.Runtime, actuators.Actuator) {
			r = &rereadFails{Actuator: a, clock: func() time.Time { return h.clock() }, last: map[string]time.Time{}}
			return d, r
		}})
	h.until(2*time.Hour, 0.5, "the failed step", func() bool { return h.l.Status().Refused > 0 })
	r.arm()
	h.run(5*time.Minute, 0.5)
	st := h.l.Status()
	if len(st.Unread) != 1 || st.Unread[0].Entry.Subject != "pod:a" || !strings.Contains(st.Unread[0].Error, "api unavailable") || st.Stuck < 1 {
		t.Fatalf("status %+v; want the step shown unread and stuck, with the read error", st)
	}
	if !strings.Contains(st.ReconcileError, "api unavailable") {
		t.Fatalf("reconcile_error %q; want the read error", st.ReconcileError)
	}
	if e, ok := h.applied()["pod:a"]; !ok || !e.Pending {
		t.Fatalf("ledger %+v; want the step still pending", h.applied())
	}
	h.mu.Lock()
	logged := strings.Join(h.logged, "\n")
	h.mu.Unlock()
	if !strings.Contains(logged, "api unavailable") {
		t.Fatalf("logged %q; want the read error logged", logged)
	}
	if h.l.reconciled {
		t.Fatal("reconciled while a step cannot be settled; want nothing applied until it can")
	}
}

// TestARestartedNeverTookStepGivesItsEarlierStepBack: after a restart the ledger
// holds a further step, pending past settle, that never took over an earlier
// applied step. Settling it puts the earlier step back in the ledger, and that
// cap is taken up as an action like any other found in force: listed, and given
// back at its own expiry, never left in force with nothing to give it back.
func TestARestartedNeverTookStepGivesItsEarlierStepBack(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4}})
	record(t, h.m, contention, "", 0.5, h.now)
	h.seed(actuators.Entry{NodeID: "n1", Subject: "pod:a", Action: cpu.Action,
		Applied: 1.2, Expires: h.now.Add(20 * time.Minute), DecisionID: "relief-act-1-1"})
	prevExpires := h.now.Add(20 * time.Minute)
	if err := h.ledger.Put(actuators.Entry{NodeID: "n1", Subject: "pod:a", Action: cpu.Action,
		Applied: 0.9, Expires: h.now.Add(30 * time.Minute), DecisionID: "relief-act-1-2", Pending: true, SetAt: h.now.Add(-time.Hour),
		PrevApplied: 1.2, PrevSet: true, PrevDecisionID: "relief-act-1-1", PrevExpires: prevExpires}); err != nil {
		t.Fatal(err)
	}
	if err := h.l.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if e := h.applied()["pod:a"]; e.Pending || e.DecisionID != "relief-act-1-1" || !near(e.Applied, 1.2) {
		t.Fatalf("entry %+v; want the earlier step committed back", e)
	}
	if len(h.decisions(OutcomeNotApplied)) != 1 {
		t.Fatalf("%d not_applied; want the step that never took closed once", len(h.decisions(OutcomeNotApplied)))
	}
	if a := h.action("pod:a"); a.DecisionID != "relief-act-1-1" || !near(a.Entry.Applied, 1.2) || !a.Entry.Expires.Equal(prevExpires) {
		t.Fatalf("action %+v; want the earlier cap taken up, with its own decision and expiry", a)
	}
	h.l.Pause() // nothing new is planned: only the expiry gives the cap back
	h.until(30*time.Minute, 0.5, "the earlier cap given back at its expiry", func() bool {
		_, held := h.applied()["pod:a"]
		return !held
	})
	if h.now.Before(prevExpires) {
		t.Fatalf("given back at %s, before its expiry %s", h.now, prevExpires)
	}
	if l := h.limit("pod:a"); l != nil {
		t.Fatalf("limit %v after expiry; want none", *l)
	}
}

// TestARestoreLeftPendingKeepsItsRevertOwed: the restore of the first step,
// after a second that did not help, is accepted but reads back as before, so it
// is left pending. The revert is not done until it settles: still owed while it
// is pending, retried when it never took, and recorded once a retry lands.
func TestARestoreLeftPendingKeepsItsRevertOwed(t *testing.T) {
	h, ln := landsHarness(t, 0)
	first := secondStepThatDoesNotHelp(h)
	ln.mu.Lock()
	ln.next = []int{-1, -1, 0} // the restore and its first retry never land; the second does
	ln.mu.Unlock()
	spent := h.l.budget.Count(h.now)
	h.until(3*time.Minute, 0.44, "the restore written", func() bool {
		e := h.applied()["pod:a"]
		return e.Pending && e.DecisionID == first.DecisionID
	})
	if !h.revertOwed() || h.l.Status().Reverted != 0 {
		t.Fatalf("action %+v, reverted %d; want the revert still owed while the restore is pending", h.action("pod:a"), h.l.Status().Reverted)
	}
	h.until(10*time.Minute, 0.44, "the restore settled as never taken", func() bool { return len(h.decisions(OutcomeNotApplied)) > 0 })
	if !h.revertOwed() || h.l.Status().Reverted != 0 {
		t.Fatalf("action %+v; want the revert still owed once the restore never took", h.action("pod:a"))
	}
	h.until(30*time.Minute, 0.44, "the revert retried until it took", func() bool {
		e := h.applied()["pod:a"]
		return !e.Pending && e.DecisionID == first.DecisionID
	})
	h.step(0.44)
	if a := h.action("pod:a"); a.Outcome != OutcomeKept || !near(a.Entry.Applied, first.Applied) || h.revertOwed() {
		t.Fatalf("action %+v; want it kept at the first step once the revert took", a)
	}
	if st := h.l.Status(); st.Reverted != 1 {
		t.Fatalf("reverted %d; want the revert recorded once", st.Reverted)
	}
	if n := h.l.budget.Count(h.now); n != spent {
		t.Fatalf("hourly count %d; want %d: a restore spends nothing and refunds nothing", n, spent)
	}
}
