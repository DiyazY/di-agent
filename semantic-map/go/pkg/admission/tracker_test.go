package admission

import (
	"context"
	"fmt"
	"maps"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/statemap"
)

// rig is a tracker over a node map with a fake clock, a fake workload list and fakes
// of relief's Shares and Moved that read the map directly.
type rig struct {
	t   *testing.T
	m   *statemap.Map
	j   *statemap.Journal
	tr  *Tracker
	now time.Time
	wls map[string]string
	// actedOn is the action relief acted on in the window, at contention peak.
	actedOn string
	peak    float64
	logs    []string
	mu      sync.Mutex
}

func newRig(t *testing.T) *rig {
	t.Helper()
	m, j := node(t, 0.4, 0.02)
	r := &rig{t: t, m: m, j: j, now: t0, wls: map[string]string{}}
	shares := func(usage string) map[string]float64 {
		out := map[string]float64{}
		for _, s := range m.Subjects() {
			if p, ok := m.Property(usage + "@" + s); ok && p.Status != statemap.Retired {
				out[s] = p.Value
			}
		}
		return out
	}
	r.tr = NewTracker(TrackerConfig{
		Map: m,
		Workloads: func(context.Context) (map[string]string, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			return maps.Clone(r.wls), nil
		},
		Shares: shares,
		Moved: func(usage string, at map[string]float64, except string) []string {
			var out []string
			for s, v := range shares(usage) {
				if s != except && math.Abs(v-at[s]) > 0.02 {
					out = append(out, s)
				}
			}
			return out
		},
		Acted: func(action string, _ time.Time) (float64, bool) {
			return r.peak, action != "" && action == r.actedOn
		},
		Now: func() time.Time { return r.now },
		Logf: func(f string, a ...any) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.logs = append(r.logs, fmt.Sprintf(f, a...))
		},
	})
	return r
}

// accept registers an accept for name: cpu at pressure 0.02 with map prediction
// 0.095 and usage prediction 0.05, memory at 0 predicted 0.01 on the usage basis.
func (r *rig) accept(name string) Decision {
	r.t.Helper()
	mp, up, mu := 0.095, 0.05, 0.01
	d := Decision{ID: "admit-1-" + name, Verdict: Accept, Lines: []Line{
		{Domain: cpuDom.Relieves, Basis: "map", Pressure: 0.02, MapPred: &mp, UsagePred: &up, Pass: true},
		{Domain: memDom.Relieves, Basis: "usage", Pressure: 0, UsagePred: &mu, Pass: true},
	}}
	if err := r.tr.Accepted(Offer{Name: name}, d, both); err != nil {
		r.t.Fatal(err)
	}
	return d
}

// arrive lists name as pod:<name> and records its usage on both domains.
func (r *rig) arrive(name string) {
	r.mu.Lock()
	r.wls[name] = "pod:" + name
	r.mu.Unlock()
	record(r.t, r.m, "cpu_utilization@pod:"+name, "pod:"+name, 0.1)
	record(r.t, r.m, "memory_utilization@pod:"+name, "pod:"+name, 0.05)
}

func (r *rig) tick(after time.Duration) {
	r.now = r.now.Add(after)
	r.tr.Tick(context.Background())
}

func (r *rig) only() Outcome {
	r.t.Helper()
	out := r.tr.Outcomes()
	if len(out) != 1 {
		r.t.Fatalf("outcomes %+v; want one", out)
	}
	return out[0]
}

func (r *rig) result(want string) Outcome {
	r.t.Helper()
	o := r.only()
	if o.Result != want {
		r.t.Fatalf("result %q; want %q (%+v)", o.Result, want, o)
	}
	return o
}

// arrived runs an accept through arrival and the snapshot.
func arrived(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	r.accept("web")
	r.arrive("web")
	r.tick(10 * time.Second)
	if len(r.tr.Outcomes()) != 0 || !r.tr.Pending("web") {
		t.Fatal("judged before settle")
	}
	return r
}

func TestHeld(t *testing.T) {
	r := arrived(t)
	record(t, r.m, "cpu_contention_ratio", "", 0.07)
	r.tick(30 * time.Second)
	if len(r.tr.Outcomes()) != 0 {
		t.Fatal("judged before settle elapsed")
	}
	r.tick(30 * time.Second)
	o := r.result("held")
	if o.Subject != "pod:web" || o.Name != "web" || r.tr.Pending("web") {
		t.Fatalf("outcome %+v pending %v", o, r.tr.Pending("web"))
	}
	summary := r.tr.PredictionSummary()
	if summary.Outcomes != 1 || summary.Paired != 1 || summary.Map.N != 1 || summary.Usage.N != 1 {
		t.Fatalf("prediction summary %+v; want the shared CPU predictions scored", summary)
	}
	if summary.Map.MAE == summary.Usage.MAE {
		t.Fatalf("prediction summary %+v; expected distinct basis errors", summary)
	}
}

func TestBreached(t *testing.T) {
	r := arrived(t)
	record(t, r.m, "cpu_contention_ratio", "", 0.26)
	r.tick(time.Minute)
	o := r.result("breached")
	if !o.Lines[0].Breached || o.Lines[1].Breached {
		t.Fatalf("lines %+v; want cpu breached only", o.Lines)
	}
}

func TestConfoundedByOtherWorkload(t *testing.T) {
	r := arrived(t)
	record(t, r.m, "cpu_utilization@pod:a", "pod:a", 0.4)
	record(t, r.m, "cpu_contention_ratio", "", 0.26)
	r.tick(time.Minute)
	o := r.result("confounded")
	for _, l := range o.Lines {
		if l.ObservedRise != nil || l.MapRise != nil || l.MapErr != nil || l.UsageErr != nil {
			t.Fatalf("confounded line scored: %+v", l)
		}
	}
}

// TestBreachedByRelief: relief acts only past Enter, so an accept relief acted on
// is the canonical false accept: breached, with relief's reading as the peak, and
// not scored, since relief has already lowered the reading.
func TestBreachedByRelief(t *testing.T) {
	r := arrived(t)
	r.actedOn, r.peak = cpuDom.Action, 0.31
	record(t, r.m, "cpu_contention_ratio", "", 0.07)
	r.tick(time.Minute)
	o := r.result("breached")
	c, mem := o.Lines[0], o.Lines[1]
	if !c.Breached || !c.ReliefActed || !ptrNear(c.Peak, 0.31) || mem.Breached || mem.ReliefActed {
		t.Fatalf("lines %+v; want cpu breached by relief only", o.Lines)
	}
	for _, l := range o.Lines {
		if l.ObservedRise != nil || l.MapRise != nil || l.MapErr != nil || l.UsageRise != nil || l.UsageErr != nil {
			t.Fatalf("relief-acted outcome scored: %+v", l)
		}
	}
	rec, _ := r.j.Decision(o.OutcomeID)
	peaks, _ := rec.Inputs["peak"].(map[string]float64)
	if rec.Inputs["relief_acted"] != true || !near(peaks[cpuDom.Relieves], 0.31) {
		t.Fatalf("inputs %v", rec.Inputs)
	}
}

// TestReliefActedOutranksMoved: relief capping a workload frees CPU the others
// absorb, so they move; the accept is still breached, with the move recorded.
func TestReliefActedOutranksMoved(t *testing.T) {
	r := arrived(t)
	r.actedOn, r.peak = cpuDom.Action, 0.31
	record(t, r.m, "cpu_utilization@pod:a", "pod:a", 0.4)
	r.tick(time.Minute)
	o := r.result("breached")
	if !o.Lines[0].ReliefActed {
		t.Fatalf("lines %+v; want cpu relief_acted", o.Lines)
	}
	rec, _ := r.j.Decision(o.OutcomeID)
	if moved, _ := rec.Inputs["moved"].([]string); len(moved) != 1 || moved[0] != "pod:a" {
		t.Fatalf("moved %v; want [pod:a] recorded", rec.Inputs["moved"])
	}
}

// TestNoLongerReportingConfounds: an admitted workload whose usage has been
// retired by settle is not judged on readings it no longer contributes to.
func TestNoLongerReportingConfounds(t *testing.T) {
	r := arrived(t)
	if err := r.m.RetireProperty("memory_utilization@pod:web", "gone", "test"); err != nil {
		t.Fatal(err)
	}
	r.tick(time.Minute)
	o := r.result("confounded")
	if o.Reason != "admitted workload no longer reporting" {
		t.Fatalf("reason %q", o.Reason)
	}
}

// TestMovedBeforeArrivalConfounds: another workload that moved between accept and
// arrival confounds the outcome; the snapshot is taken at accept.
func TestMovedBeforeArrivalConfounds(t *testing.T) {
	r := newRig(t)
	r.accept("web")
	record(t, r.m, "cpu_utilization@pod:a", "pod:a", 0.4)
	r.arrive("web")
	r.tick(10 * time.Second)
	r.tick(time.Minute)
	o := r.result("confounded")
	if !strings.Contains(o.Reason, "pod:a") {
		t.Fatalf("reason %q; want pod:a named", o.Reason)
	}
}

// TestRiseFromAcceptBaseline: the rise is measured from the accept's pressure, the
// predictions' baseline, not from arrival; the arrival reading is recorded.
func TestRiseFromAcceptBaseline(t *testing.T) {
	r := newRig(t)
	r.accept("web")
	record(t, r.m, "cpu_contention_ratio", "", 0.04)
	r.arrive("web")
	r.tick(10 * time.Second)
	record(t, r.m, "cpu_contention_ratio", "", 0.07)
	r.tick(time.Minute)
	o := r.result("held")
	if c := o.Lines[0]; !ptrNear(c.ObservedRise, 0.05) || !ptrNear(c.MapErr, 0.025) {
		t.Fatalf("cpu line %+v; want rise 0.05 from the accept's 0.02", c)
	}
	rec, _ := r.j.Decision(o.OutcomeID)
	if at, _ := rec.Inputs["relieves_at_arrival"].([]float64); len(at) != 2 || !near(at[0], 0.04) {
		t.Fatalf("relieves_at_arrival %v", rec.Inputs["relieves_at_arrival"])
	}
}

// TestStaleReadingConfounds: a Relieves gone stale by judgement is not scored.
func TestStaleReadingConfounds(t *testing.T) {
	r := arrived(t)
	r.m.SetClock(func() time.Time { return t0.Add(2 * r.m.StaleAfter()) })
	r.m.SetMonotonic(func() time.Duration { return time.Second + 2*r.m.StaleAfter() })
	r.m.Sweep()
	r.tick(time.Minute)
	o := r.result("confounded")
	if !strings.HasPrefix(o.Reason, "stale reading") {
		t.Fatalf("reason %q", o.Reason)
	}
	for _, l := range o.Lines {
		if l.MapRise != nil || l.UsageErr != nil {
			t.Fatalf("stale outcome scored: %+v", l)
		}
	}
}

func TestNotArrived(t *testing.T) {
	r := newRig(t)
	r.accept("web")
	r.tick(ArrivalWindow)
	if len(r.tr.Outcomes()) != 0 {
		t.Fatal("expired at the window's edge")
	}
	r.tick(time.Second)
	o := r.result("not_arrived")
	for _, l := range o.Lines {
		if l.ObservedRise != nil || l.MapRise != nil || l.UsageRise != nil {
			t.Fatalf("not_arrived line scored: %+v", l)
		}
	}
	if r.tr.Pending("web") {
		t.Fatal("still pending")
	}
}

func TestWaitsForUsageOnEveryDomain(t *testing.T) {
	r := newRig(t)
	r.accept("web")
	r.mu.Lock()
	r.wls["web"] = "pod:web"
	r.mu.Unlock()
	record(t, r.m, "cpu_utilization@pod:web", "pod:web", 0.1)
	r.tick(2 * time.Minute)
	r.tick(2 * time.Minute)
	if len(r.tr.Outcomes()) != 0 {
		t.Fatal("judged without memory usage")
	}
	r.tick(time.Minute + time.Second)
	r.result("not_arrived")
}

func TestPendingCapDropsOldest(t *testing.T) {
	r := newRig(t)
	for i := 0; i <= PendingCap; i++ {
		r.accept(fmt.Sprintf("w%d", i))
	}
	if r.tr.Pending("w0") || !r.tr.Pending("w1") || !r.tr.Pending(fmt.Sprintf("w%d", PendingCap)) {
		t.Fatal("cap did not drop the oldest")
	}
	o := r.result("confounded")
	if o.Name != "w0" || o.Reason != "dropped at the pending cap" {
		t.Fatalf("outcome %+v; want w0 dropped at the pending cap", o)
	}
	if rec, ok := r.j.Decision(o.OutcomeID); !ok || rec.Answer["reason"] != o.Reason {
		t.Fatalf("drop not journalled: %v %+v", ok, rec)
	}
}

func TestAlreadyPending(t *testing.T) {
	r := newRig(t)
	r.accept("web")
	err := r.tr.Accepted(Offer{Name: "web"}, Decision{ID: "x", Verdict: Accept}, both)
	if err == nil || err.Error() != "already pending" {
		t.Fatalf("err %v; want already pending", err)
	}
}

// TestAcceptedConcurrent: many goroutines racing the same name admit exactly one.
func TestAcceptedConcurrent(t *testing.T) {
	r := newRig(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r.tr.Accepted(Offer{Name: "web"}, Decision{ID: "x", Verdict: Accept}, both) == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("%d accepts registered; want 1", ok)
	}
}

func TestOutcomeLinksAccept(t *testing.T) {
	r := arrived(t)
	r.tick(time.Minute)
	o := r.result("held")
	if o.AcceptID != "admit-1-web" || o.OutcomeID == "" {
		t.Fatalf("ids %q %q", o.AcceptID, o.OutcomeID)
	}
	rec, ok := r.j.Decision(o.OutcomeID)
	if !ok {
		t.Fatal("outcome decision not recorded")
	}
	if rec.Inputs["accept"] != o.AcceptID || rec.Answer["outcome"] != "held" {
		t.Fatalf("record inputs %v answer %v", rec.Inputs, rec.Answer)
	}
}

func TestOutcomeCitesTheAccept(t *testing.T) {
	r := arrived(t)
	r.tick(time.Minute)
	o := r.result("held")
	rec, ok := r.j.Decision(o.OutcomeID)
	if !ok {
		t.Fatal("outcome decision not recorded")
	}
	if rec.Kind != "admission.outcome" || len(rec.Cites) != 1 || rec.Cites[0] != o.AcceptID {
		t.Fatalf("outcome kind %q cites %v; want admission.outcome citing %s", rec.Kind, rec.Cites, o.AcceptID)
	}
}

func TestOutcomeIdsComeFromTheMap(t *testing.T) {
	r := arrived(t)
	r.tick(time.Minute)
	if o := r.result("held"); !strings.HasPrefix(o.OutcomeID, "admit-1.") {
		t.Fatalf("outcome id %q; want prefix admit-1.", o.OutcomeID)
	}
}

func TestRisesAndErrors(t *testing.T) {
	r := arrived(t)
	record(t, r.m, "cpu_contention_ratio", "", 0.07)
	r.tick(time.Minute)
	o := r.result("held")
	c := o.Lines[0]
	if c.Domain != cpuDom.Relieves || !ptrNear(c.ObservedRise, 0.05) ||
		!ptrNear(c.MapRise, 0.075) || !ptrNear(c.MapErr, 0.025) ||
		!ptrNear(c.UsageRise, 0.03) || !ptrNear(c.UsageErr, -0.02) {
		t.Fatalf("cpu line %+v", c)
	}
	mem := o.Lines[1]
	if mem.MapRise != nil || mem.MapErr != nil || !ptrNear(mem.UsageErr, 0.01) {
		t.Fatalf("memory line %+v", mem)
	}
}

func TestOutcomesNewestFirst(t *testing.T) {
	r := newRig(t)
	r.accept("a1")
	r.tick(time.Minute)
	r.accept("a2")
	r.tick(5 * time.Minute)
	r.tick(time.Second)
	out := r.tr.Outcomes()
	if len(out) != 2 || out[0].Name != "a2" || out[1].Name != "a1" {
		t.Fatalf("outcomes %+v", out)
	}
}

func TestArrivalIsJournalled(t *testing.T) {
	r := newRig(t)
	r.accept("web")
	r.arrive("web")
	r.tick(time.Second)
	var got []statemap.Event
	for _, e := range r.j.Events(0, 0) {
		if e.Kind == statemap.EventAdmissionArrived {
			got = append(got, e)
		}
	}
	if len(got) != 1 {
		t.Fatalf("arrival events %v; want one", got)
	}
	if e := got[0]; e.Target != "web" || e.Detail["subject"] != "pod:web" || e.Detail["accept"] != "admit-1-web" {
		t.Errorf("event %+v", e)
	}
	r.tick(time.Second)
	n := 0
	for _, e := range r.j.Events(0, 0) {
		if e.Kind == statemap.EventAdmissionArrived {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d arrival events after another tick; want still one", n)
	}
}

// TestRestoreKeepsOutcomesAndClosesPendingAccepts: a restarted agent keeps the
// outcomes, and closes each accept still awaiting one as confounded — its
// arrival and settle clocks did not survive the restart — citing the accept.
func TestRestoreKeepsOutcomesAndClosesPendingAccepts(t *testing.T) {
	r := newRig(t)
	r.accept("gone")
	r.tick(ArrivalWindow + time.Second)
	d := r.accept("web")
	r.arrive("web")
	r.tick(10 * time.Second)
	mem := r.tr.Memory()
	if len(mem.Outcomes) != 1 || len(mem.Pending) != 1 || mem.Pending[0].AcceptID != d.ID {
		t.Fatalf("memory %+v; want one outcome and web pending", mem)
	}

	r2 := newRig(t)
	r2.tr.Restore(mem)
	out := r2.tr.Outcomes()
	if len(out) != 2 || out[1].Name != "gone" {
		t.Fatalf("outcomes %+v; want web's newest, then gone", out)
	}
	o := out[0]
	if o.Name != "web" || o.Result != Confounded || o.Reason != "agent restarted" || o.Subject != "pod:web" {
		t.Fatalf("restart outcome %+v; want web confounded by the restart", o)
	}
	rec, ok := r2.j.Decision(o.OutcomeID)
	if !ok || rec.Kind != "admission.outcome" || len(rec.Cites) != 1 || rec.Cites[0] != d.ID {
		t.Fatalf("decision %+v (found %v); want admission.outcome citing %s", rec, ok, d.ID)
	}
	if p := r2.tr.PendingList(); len(p) != 0 {
		t.Fatalf("pending %+v; nothing resumes after a restart", p)
	}
}

func TestHeldFeedsBothBases(t *testing.T) {
	r := arrived(t)
	record(t, r.m, "cpu_contention_ratio", "", 0.07)
	r.tick(time.Minute)
	o := r.result("held")
	cal := r.m.Calibration()
	key := func(k, b string) statemap.CalibrationKey { return statemap.CalibrationKey{Kind: k, Basis: b} }
	cm := cal[key("admission.cpu_contention_ratio", "map")]
	cu := cal[key("admission.cpu_contention_ratio", "usage")]
	mu := cal[key("admission.memory_contention_ratio", "usage")]
	if cm.N != 1 || !near(cm.MeanErr, 0.025) || !near(cm.MeanAbsErr, 0.025) || cm.HitRate != 1 {
		t.Errorf("cpu map %+v", cm)
	}
	if cu.N != 1 || !near(cu.MeanErr, -0.02) || !near(cu.MeanAbsErr, 0.02) || cu.HitRate != 1 {
		t.Errorf("cpu usage %+v", cu)
	}
	if mu.N != 1 || !near(mu.MeanErr, 0.01) || mu.HitRate != 1 {
		t.Errorf("memory usage %+v", mu)
	}
	if len(cal) != 3 {
		t.Errorf("calibration %v; want three keys", cal)
	}
	rec, _ := r.j.Decision(o.OutcomeID)
	in, _ := rec.Inputs["calibration"].(map[string]statemap.Calibration)
	if len(in) != 3 || in["admission.cpu_contention_ratio/map"].N != 1 ||
		in["admission.cpu_contention_ratio/usage"].N != 1 || in["admission.memory_contention_ratio/usage"].N != 1 {
		t.Errorf("calibration input %v", rec.Inputs["calibration"])
	}
}

func TestBreachedOnAReadingFeedsAMiss(t *testing.T) {
	r := arrived(t)
	record(t, r.m, "cpu_contention_ratio", "", 0.26)
	r.tick(time.Minute)
	r.result("breached")
	c := r.m.Calibration()[statemap.CalibrationKey{Kind: "admission.cpu_contention_ratio", Basis: "map"}]
	if c.N != 1 || c.HitRate != 0 {
		t.Fatalf("cpu map %+v; predicted under the exit, observed over it", c)
	}
}

func TestConfoundedAndReliefActedFeedNothing(t *testing.T) {
	r := arrived(t)
	record(t, r.m, "cpu_utilization@pod:a", "pod:a", 0.4)
	record(t, r.m, "cpu_contention_ratio", "", 0.26)
	r.tick(time.Minute)
	r.result("confounded")
	if len(r.m.Calibration()) != 0 {
		t.Fatalf("confounded outcome fed %v", r.m.Calibration())
	}

	r = arrived(t)
	r.actedOn, r.peak = cpuDom.Action, 0.31
	record(t, r.m, "cpu_contention_ratio", "", 0.07)
	r.tick(time.Minute)
	r.result("breached")
	if len(r.m.Calibration()) != 0 {
		t.Fatalf("relief-acted outcome fed %v", r.m.Calibration())
	}
}
