package relief

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/actuators"
	"github.com/DiyazY/di-agent/pkg/policy"
	"github.com/DiyazY/di-agent/pkg/runtime"
	"github.com/DiyazY/di-agent/pkg/runtime/kubernetes"
	"github.com/DiyazY/di-agent/pkg/spec"
	"github.com/DiyazY/di-agent/pkg/statemap"
)

var unit = [2]float64{0, 1}

// cpu is the descriptor the loop is tested with: the cpu.limit action's own, so
// the metric names, unit and capacity fact are the ones the agent runs with.
var cpu = kubernetes.CPULimitDescriptor

// The node properties cpu.limit relieves and moves.
var (
	contention = cpu.Relieves
	nodeUsage  = cpu.Usage
)

// cpuActuator is cpu.limit over the workloads' own inventory: Read is the
// limit the workload lists until a Set, and check (nil: always) is its Check.
func cpuActuator(check func(runtime.Workload) string) actuators.Actuator {
	return actuators.Static(cpu, check, func(w runtime.Workload) (float64, bool) {
		if l := w.Resources["cpu"].Limit; l != nil {
			return *l, true
		}
		return 0, false
	})
}

// limiter is the actuator the plan tests attribute and plan with; they never Set.
var limiter = cpuActuator(nil)

var ctx = context.Background()

// reliefCfg is the spec's defaults, so a test reads against the numbers an
// operator gets.
func reliefCfg() spec.Lever { return spec.Default().Loops.Relief.Levers[0] }

// defaultPolicy is the spec's default policy with the hourly limit and
// cooldown a test may want to change.
func defaultPolicy(t *testing.T, edit func(*spec.PolicySpec)) *policy.Policy {
	t.Helper()
	ps := spec.Default().Policy
	if edit != nil {
		edit(&ps)
	}
	p, err := policy.New(ps)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// newMap is a map whose properties take the latest reading as their value
// (Alpha 1), so a test that says "pressure falls to 0.44" means the map says so.
func newMap(t *testing.T, now func() time.Time) *statemap.Map {
	t.Helper()
	m := statemap.New(statemap.Config{AdmitUnknown: true, Alpha: 1}, statemap.NewJournal(0))
	m.SetClock(now)
	return m
}

func record(t *testing.T, m *statemap.Map, id, subject string, v float64, at time.Time) {
	t.Helper()
	if err := m.Record(statemap.Observation{ID: id, Value: v, At: at, Subject: subject, Range: &unit}); err != nil {
		t.Fatal(err)
	}
}

func usage(subject string) string { return nodeUsage + "@" + subject }

// relate declares subject's usage as driving node pressure with strength s.
func relate(t *testing.T, m *statemap.Map, subject string, s float64) {
	t.Helper()
	r := statemap.Relationship{From: usage(subject), To: contention, Sign: 1, Label: "drives"}
	if err := m.DeclareRelationship(r); err != nil {
		t.Fatal(err)
	}
	if err := m.AssertRelationshipStrength(statemap.RelationshipID(r.From, r.To, r.Label), s, "test", "fixture"); err != nil {
		t.Fatal(err)
	}
}

func cores(t *testing.T, m *statemap.Map, n float64) {
	t.Helper()
	if err := m.RecordFacts("test", []statemap.Fact{statemap.NumberFact("cpu_cores", n, "cores")}); err != nil {
		t.Fatal(err)
	}
}

func pod(subject string) runtime.Workload {
	return runtime.Workload{Subject: subject, Name: strings.TrimPrefix(subject, "pod:")}
}

// limited gives w a CPU limit; it keeps any request.
func limited(w runtime.Workload, limit float64) runtime.Workload {
	r := w.Resources["cpu"]
	r.Limit = &limit
	w.Resources = map[string]runtime.Resource{"cpu": r}
	return w
}

// requested gives w a CPU request; it keeps any limit.
func requested(w runtime.Workload, request float64) runtime.Workload {
	r := w.Resources["cpu"]
	r.Request = &request
	w.Resources = map[string]runtime.Resource{"cpu": r}
	return w
}

func one(string, string) float64 { return 1 }

// planFixture: node pressure 0.5 and usage 0.8 on 4 cores; pod:a uses 0.4 of
// the node and drives pressure with strength s.
func planFixture(t *testing.T, s float64) *statemap.Map {
	t.Helper()
	m := newMap(t, func() time.Time { return t0 })
	record(t, m, contention, "", 0.5, t0)
	record(t, m, nodeUsage, "", 0.8, t0)
	record(t, m, usage("pod:a"), "pod:a", 0.4, t0)
	relate(t, m, "pod:a", s)
	cores(t, m, 4)
	return m
}

func subjects(cs []Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Subject
	}
	return out
}

func TestAVictimIsNotACandidate(t *testing.T) {
	m := planFixture(t, 1)
	record(t, m, usage("pod:v"), "pod:v", 0.02, t0)
	record(t, m, contention+"@pod:v", "pod:v", 0.9, t0)
	cs := Attribute(m, limiter, []runtime.Workload{pod("pod:a"), pod("pod:v")}, defaultPolicy(t, nil), reliefCfg())
	for _, c := range cs {
		if c.Subject == "pod:v" {
			t.Fatalf("candidates %v: a workload using 2%% of the node is a victim, not a cause", subjects(cs))
		}
	}
	if len(cs) != 1 {
		t.Fatalf("candidates %v; want pod:a alone", subjects(cs))
	}
}

func TestTheMapRanksBeforeUsage(t *testing.T) {
	m := planFixture(t, 1)
	record(t, m, usage("pod:b"), "pod:b", 0.6, t0)
	cs := Attribute(m, limiter, []runtime.Workload{pod("pod:b"), pod("pod:a")}, defaultPolicy(t, nil), reliefCfg())
	if len(cs) != 2 || cs[0].Subject != "pod:a" || cs[1].Subject != "pod:b" {
		t.Fatalf("candidates %v; want pod:a (the map) before pod:b (usage)", subjects(cs))
	}
	if cs[0].Basis != "map" || cs[1].Basis != "usage" {
		t.Errorf("bases %q, %q; want map and usage", cs[0].Basis, cs[1].Basis)
	}
	if !near(cs[0].Drop, 0.4) || !near(cs[0].Share, 0.4) || !near(cs[1].Share, 0.6) {
		t.Errorf("candidates %+v; want pod:a drop 0.4 share 0.4, pod:b share 0.6", cs)
	}
}

func TestAFrozenWorkloadIsSkipped(t *testing.T) {
	m := planFixture(t, 1)
	w := pod("pod:a")
	w.Labels = map[string]string{"di-agent/freeze": "true"}
	if cs := Attribute(m, limiter, []runtime.Workload{w}, defaultPolicy(t, nil), reliefCfg()); len(cs) != 0 {
		t.Fatalf("candidates %v; a frozen workload is not one", subjects(cs))
	}
	self := pod("pod:a")
	self.Self = true
	if cs := Attribute(m, limiter, []runtime.Workload{self}, defaultPolicy(t, nil), reliefCfg()); len(cs) != 0 {
		t.Fatalf("candidates %v; the agent itself is not one", subjects(cs))
	}
}

func TestAWorkloadThatCannotBeResizedIsSkipped(t *testing.T) {
	m := planFixture(t, 1)
	refusing := cpuActuator(func(runtime.Workload) string { return "QoS would change" })
	if cs := Attribute(m, refusing, []runtime.Workload{pod("pod:a")}, defaultPolicy(t, nil), reliefCfg()); len(cs) != 0 {
		t.Fatalf("candidates %v; the actuator said it cannot resize this one", subjects(cs))
	}
}

func TestTheStepRespectsTheFloors(t *testing.T) {
	m := newMap(t, func() time.Time { return t0 })
	record(t, m, contention, "", 0.5, t0)
	record(t, m, nodeUsage, "", 0.8, t0)
	record(t, m, usage("pod:a"), "pod:a", 0.1, t0)
	relate(t, m, "pod:a", 1)
	cores(t, m, 4)
	ws := []runtime.Workload{limited(pod("pod:a"), 0.5)}
	pol := defaultPolicy(t, nil)
	c := reliefCfg()
	c.MinEffect = 0.01 // a 0.1 share stepped by a quarter predicts 0.025
	p, ok, why := MakePlan(ctx, m, limiter, Attribute(m, limiter, ws, pol, c), ws, pol, c, one)
	if !ok {
		t.Fatalf("no plan: %s", why)
	}
	// 0.1 of 4 cores = 0.4; max(0.4·0.75, 0.1, 0.5·0.5) = 0.3
	if !near(p.From, 0.4) || !near(p.To, 0.3) {
		t.Fatalf("plan %+v; want 0.4 → 0.3 cores", p)
	}
	if p.EstimateID == "" || p.Subject != "pod:a" || p.Rule != -1 {
		t.Errorf("plan %+v; want pod:a, an estimate id and the defaults' rule -1", p)
	}

	// A floor at or above usage leaves no step to take.
	high := defaultPolicy(t, func(ps *spec.PolicySpec) { ps.Defaults.Floor = spec.Floor{"cpu.limit": spec.NewBound(0.4, 0)} })
	if _, ok, why := MakePlan(ctx, m, limiter, Attribute(m, limiter, ws, high, c), ws, high, c, one); ok || !strings.Contains(why, "floor") {
		t.Fatalf("ok=%v why=%q; want refused at the floor", ok, why)
	}
}

func TestAStepThatDoesNotHelpIsNotTaken(t *testing.T) {
	// share 0.4, step to 0.3: delta 0.1·0.1 = 0.01 < min_effect 0.03
	m := planFixture(t, 0.1)
	ws := []runtime.Workload{pod("pod:a")}
	pol := defaultPolicy(t, nil)
	c := reliefCfg()
	_, ok, why := MakePlan(ctx, m, limiter, Attribute(m, limiter, ws, pol, c), ws, pol, c, one)
	if ok || !strings.Contains(why, "min_effect") {
		t.Fatalf("ok=%v why=%q; want refused below min_effect", ok, why)
	}
}

func TestAUsageCandidateIsPredictedFromItsShare(t *testing.T) {
	m := planFixture(t, 1)
	record(t, m, usage("pod:b"), "pod:b", 0.6, t0)
	ws := []runtime.Workload{pod("pod:b")}
	pol := defaultPolicy(t, nil)
	c := reliefCfg()
	p, ok, why := MakePlan(ctx, m, limiter, Attribute(m, limiter, ws, pol, c), ws, pol, c, one)
	if !ok {
		t.Fatalf("no plan: %s", why)
	}
	// 0.6 · 0.25 · (0.5 / 0.8)
	if !near(p.Predicted, 0.6*0.25*0.5/0.8) || p.Basis != "usage" || p.EstimateID != "" {
		t.Fatalf("plan %+v; want a usage prediction with no estimate", p)
	}
}

func TestNoCoresNoPlan(t *testing.T) {
	m := newMap(t, func() time.Time { return t0 })
	record(t, m, contention, "", 0.5, t0)
	record(t, m, nodeUsage, "", 0.8, t0)
	record(t, m, usage("pod:a"), "pod:a", 0.4, t0)
	relate(t, m, "pod:a", 1)
	ws := []runtime.Workload{pod("pod:a")}
	pol := defaultPolicy(t, nil)
	c := reliefCfg()
	_, ok, why := MakePlan(ctx, m, limiter, Attribute(m, limiter, ws, pol, c), ws, pol, c, one)
	if ok || why != "no cpu_cores fact: shares cannot become cores" {
		t.Fatalf("ok=%v why=%q", ok, why)
	}
}

func near(a, b float64) bool { d := a - b; return d < 1e-9 && d > -1e-9 }

// TestOnlyItsOwnUsageRanksAWorkloadByTheMap: a relationship from another of the
// workload's properties says nothing about what capping its CPU would do, which
// is what the plan predicts by assuming a new cpu_utilization@<subject>.
func TestOnlyItsOwnUsageRanksAWorkloadByTheMap(t *testing.T) {
	m := newMap(t, func() time.Time { return t0 })
	record(t, m, contention, "", 0.5, t0)
	record(t, m, nodeUsage, "", 0.8, t0)
	record(t, m, usage("pod:q"), "pod:q", 0.4, t0)
	record(t, m, "queue@pod:q", "pod:q", 0.9, t0)
	r := statemap.Relationship{From: "queue@pod:q", To: contention, Sign: 1, Label: "drives"}
	if err := m.DeclareRelationship(r); err != nil {
		t.Fatal(err)
	}
	if err := m.AssertRelationshipStrength(statemap.RelationshipID(r.From, r.To, r.Label), 1, "test", "fixture"); err != nil {
		t.Fatal(err)
	}
	cs := Attribute(m, limiter, []runtime.Workload{pod("pod:q")}, defaultPolicy(t, nil), reliefCfg())
	if len(cs) != 1 || cs[0].Basis != BasisUsage {
		t.Fatalf("candidates %+v; want pod:q ranked on usage", cs)
	}
}

func TestTheStepNeverGoesBelowTheRequest(t *testing.T) {
	m := planFixture(t, 1)
	ws := []runtime.Workload{requested(limited(pod("pod:a"), 2), 1.4)}
	pol := defaultPolicy(t, nil)
	c := reliefCfg()
	c.MinEffect = 0.001
	p, ok, why := MakePlan(ctx, m, limiter, Attribute(m, limiter, ws, pol, c), ws, pol, c, one)
	if !ok {
		t.Fatalf("no plan: %s", why)
	}
	// 0.4 of 4 cores = 1.6; the step alone would reach 1.2, but the request is 1.4.
	if !near(p.From, 1.6) || !near(p.To, 1.4) {
		t.Fatalf("plan %+v; want 1.6 → 1.4 cores", p)
	}
}

// TestTheUsagePredictionUsesTheFlooredCut: a floor that stops the step short of
// a full Step cuts less, and the usage prediction is of the cut actually made.
func TestTheUsagePredictionUsesTheFlooredCut(t *testing.T) {
	m := planFixture(t, 1)
	record(t, m, usage("pod:b"), "pod:b", 0.6, t0)
	ws := []runtime.Workload{requested(pod("pod:b"), 2.0)}
	pol := defaultPolicy(t, nil)
	c := reliefCfg()
	p, ok, why := MakePlan(ctx, m, limiter, Attribute(m, limiter, ws, pol, c), ws, pol, c, one)
	if !ok {
		t.Fatalf("no plan: %s", why)
	}
	// 0.6 of 4 cores = 2.4; the step would reach 1.8, the request holds it at 2.0:
	// a cut of 1 − 2.0/2.4 = 1/6, not the step's 0.25.
	if !near(p.To, 2.0) || p.Basis != BasisUsage {
		t.Fatalf("plan %+v; want a usage plan to 2.0 cores", p)
	}
	if want := 0.6 * (1 - 2.0/2.4) * 0.5 / 0.8; !near(p.Predicted, want) {
		t.Fatalf("predicted %v; want %v, the floored cut", p.Predicted, want)
	}
}

// TestARefusalSaysTheFloorHeldTheStep: a workload already capped near its floor
// can only be stepped a little further, and a refusal for the small drop that
// follows must say the floor made the step small, not leave it reading as a weak
// prediction. Seen live: a hog capped from 3 to 1.518 cores, floor 0.5 of 3.
func TestARefusalSaysTheFloorHeldTheStep(t *testing.T) {
	m := planFixture(t, 1)
	record(t, m, usage("pod:a"), "pod:a", 0.3795, t0)
	ws := []runtime.Workload{limited(pod("pod:a"), 1.518)}
	pol := defaultPolicy(t, nil) // floor 0.1 cores, 0.5 of the original limit
	c := reliefCfg()
	_, ok, why := makePlan(ctx, m, limiter, Attribute(m, limiter, ws, pol, c), ws, pol, c, one, map[string]float64{"pod:a": 3})
	if ok {
		t.Fatal("planned; want refused, the step is 1.518 → 1.5 cores")
	}
	if !strings.Contains(why, "below min_effect") || !strings.Contains(why, "the floor holds the step to 1.500 cores") {
		t.Fatalf("why=%q; want the min_effect refusal to name the floor that held the step", why)
	}
}

// TestPlanCPUUnchanged: the plan the descriptor-driven arithmetic makes for CPU
// is, to the last bit, the one the CPU-only arithmetic made before it: a map
// candidate capped at 2 cores and a usage candidate with no limit, on 4 cores.
func TestPlanCPUUnchanged(t *testing.T) {
	m := planFixture(t, 1)
	record(t, m, usage("pod:b"), "pod:b", 0.6, t0)
	ws := []runtime.Workload{limited(pod("pod:a"), 2), pod("pod:b")}
	pol := defaultPolicy(t, nil)
	c := reliefCfg()
	for _, want := range []Plan{
		{Subject: "pod:a", From: 1.6000000000000001, To: 1.2000000000000002, Predicted: 0.099999999999999978, Basis: BasisMap},
		{Subject: "pod:b", From: 2.3999999999999999, To: 1.7999999999999998, Predicted: 0.09375, Basis: BasisUsage},
	} {
		var in []runtime.Workload
		for _, w := range ws {
			if w.Subject == want.Subject {
				in = append(in, w)
			}
		}
		p, ok, why := MakePlan(ctx, m, limiter, Attribute(m, limiter, in, pol, c), ws, pol, c, one)
		if !ok {
			t.Fatalf("%s: no plan: %s", want.Subject, why)
		}
		if p.From != want.From || p.To != want.To || p.Predicted != want.Predicted || p.Basis != want.Basis || p.Unit != "cores" {
			t.Errorf("plan %+v; want from %.17g to %.17g predicted %.17g (%s) in cores", p, want.From, want.To, want.Predicted, want.Basis)
		}
	}
}
