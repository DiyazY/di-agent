package admission

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/statemap"
)

var unit = [2]float64{0, 1}

var t0 = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

// The domains are the cpu.limit lever's descriptor and a memory lever's, with the
// spec's default levels.
var (
	cpuDom = Domain{Action: "cpu.limit", Resource: "cpu", Relieves: "cpu_contention_ratio",
		Usage: "cpu_utilization", Capacity: "cpu_cores", Exit: 0.10, Enter: 0.25, Settle: time.Minute, Phase: "calm"}
	memDom = Domain{Action: "memory.limit", Resource: "memory", Relieves: "memory_contention_ratio",
		Usage: "memory_utilization", Capacity: "memory_total", Exit: 0.05, Enter: 0.08, Settle: time.Minute, Phase: "calm"}
	both = []Domain{cpuDom, memDom}
	self = Self{NodeID: "n1", Arch: "amd64"}
)

// newMap is a map whose properties take the latest reading as their value
// (Alpha 1), with the journal kept so a test can read the decision back.
func newMap(t *testing.T) (*statemap.Map, *statemap.Journal) {
	t.Helper()
	j := statemap.NewJournal(0)
	m := statemap.New(statemap.Config{AdmitUnknown: true, Alpha: 1}, j)
	m.SetClock(func() time.Time { return t0 })
	m.SetMonotonic(func() time.Duration { return time.Second })
	return m, j
}

func record(t *testing.T, m *statemap.Map, id, subject string, v float64) {
	t.Helper()
	if err := m.Record(statemap.Observation{ID: id, Value: v, At: t0, Subject: subject, Range: &unit}); err != nil {
		t.Fatal(err)
	}
}

func cores(t *testing.T, m *statemap.Map, n float64) {
	t.Helper()
	if err := m.RecordFacts("test", []statemap.Fact{statemap.NumberFact("cpu_cores", n, "cores")}); err != nil {
		t.Fatal(err)
	}
}

func memTotal(t *testing.T, m *statemap.Map, n float64) {
	t.Helper()
	if err := m.RecordFacts("test", []statemap.Fact{statemap.NumberFact("memory_total", n, "bytes")}); err != nil {
		t.Fatal(err)
	}
}

// effect declares subject's usage as driving cpu pressure and records one measured
// intervention on it with effect e.
func effect(t *testing.T, m *statemap.Map, subject string, e float64) {
	t.Helper()
	from := "cpu_utilization@" + subject
	record(t, m, from, subject, 0.1)
	if err := m.DeclareRelationship(statemap.Relationship{From: from, To: "cpu_contention_ratio", Sign: 1, Label: "drives"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.RecordIntervention(statemap.InterventionOutcome{From: from, To: "cpu_contention_ratio",
		Agreed: true, DecisionID: "verify-" + subject, HasEffect: true, Effect: e}); err != nil {
		t.Fatal(err)
	}
}

// node is a 4-core, 8 GiB node running pod:a, at cpu usage u and cpu pressure p
// (p < 0 leaves it unrecorded), memory at 0.2 with no memory pressure. Its gate
// passes for small offers.
func node(t *testing.T, u, p float64) (*statemap.Map, *statemap.Journal) {
	t.Helper()
	m, j := newMap(t)
	cores(t, m, 4)
	memTotal(t, m, 8<<30)
	record(t, m, "cpu_utilization", "", u)
	record(t, m, "memory_utilization", "", 0.2)
	record(t, m, "memory_contention_ratio", "", 0)
	if p >= 0 {
		record(t, m, "cpu_contention_ratio", "", p)
	}
	record(t, m, "cpu_utilization@pod:a", "pod:a", 0.1)
	return m, j
}

// offer asks for cpu cores and 64 MiB, both declared.
func offer(cpu float64) Offer {
	return Offer{Name: "web", Demand: map[string]Demand{
		"cpu":    {Value: cpu, Source: "declared"},
		"memory": {Value: 64 << 20, Source: "declared"},
	}}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func ptrNear(p *float64, want float64) bool { return p != nil && near(*p, want) }

func want(t *testing.T, d Decision, verdict, reason string) {
	t.Helper()
	if d.Verdict != verdict || !strings.HasPrefix(d.Reason, reason) {
		t.Fatalf("got %s %q; want %s %q\nlines %+v", d.Verdict, d.Reason, verdict, reason, d.Lines)
	}
}

func line(t *testing.T, d Decision, domain string) Line {
	t.Helper()
	for _, l := range d.Lines {
		if l.Domain == domain {
			return l
		}
	}
	t.Fatalf("no line for %s in %+v", domain, d.Lines)
	return Line{}
}

func TestArchGate(t *testing.T) {
	m, _ := node(t, 0.4, 0.02)
	o := offer(0.4)
	o.Arch = []string{"arm64"}
	want(t, Decide(m, o, both, self, m.NewDecisionID("admit")), Decline, "cannot run on amd64")
	o.Arch = []string{"arm64", "AMD64"}
	want(t, Decide(m, o, both, self, m.NewDecisionID("admit")), Accept, "")
}

func TestNoDomains(t *testing.T) {
	m, _ := node(t, 0.4, 0.02)
	want(t, Decide(m, offer(0.4), nil, self, m.NewDecisionID("admit")), Decline, "no admission domains")
}

func TestReliefActiveDeclines(t *testing.T) {
	m, _ := node(t, 0.4, 0.02)
	// A phase admission does not know is not calm either.
	for _, phase := range []string{"relieving", "releasing", "", "warming"} {
		c := cpuDom
		c.Phase = phase
		d := Decide(m, offer(0.4), []Domain{memDom, c}, self, m.NewDecisionID("admit"))
		want(t, d, Decline, "relief active on cpu_contention_ratio")
		if l := line(t, d, "cpu_contention_ratio"); l.Phase != phase {
			t.Fatalf("line phase %q; want %q", l.Phase, phase)
		}
	}
}

// TestMissingDemand: a domain whose resource the offer does not price declines; the
// node does not guess what memory a workload needs.
func TestMissingDemand(t *testing.T) {
	m, _ := node(t, 0.4, 0.02)
	o := offer(0.4)
	delete(o.Demand, "memory")
	want(t, Decide(m, o, both, self, m.NewDecisionID("admit")), Decline, "unknown memory demand")
}

func TestCrossArchCPU(t *testing.T) {
	m, _ := node(t, 0.4, 0.02)
	o := offer(0.4)
	o.Demand["cpu"] = Demand{Value: 0.4, Source: "measured on n2", Arch: "arm64"}
	want(t, Decide(m, o, both, self, m.NewDecisionID("admit")), Decline, "cpu demand not from this architecture")

	o = offer(0.4)
	o.Demand["memory"] = Demand{Value: 64 << 20, Source: "measured on n2", Arch: "arm64"}
	want(t, Decide(m, o, both, self, m.NewDecisionID("admit")), Accept, "")
}

// TestHeadroom: a calm node offered more than it has left declines on the gate,
// where every pressure-based prediction would read 0.
func TestHeadroom(t *testing.T) {
	m, _ := node(t, 0.8, 0)
	d := Decide(m, offer(1), both, self, m.NewDecisionID("admit"))
	want(t, d, Decline, "cpu would reach")
	if !strings.Contains(d.Reason, "past 0.90") {
		t.Fatalf("reason %q does not name the limit", d.Reason)
	}
}

// TestZeroDemand: an offer demanding nothing has nothing to price; zero cpu with
// memory still passes the gate and the predictions.
func TestZeroDemand(t *testing.T) {
	m, _ := node(t, 0.4, 0.05)
	o := Offer{Name: "web", Demand: map[string]Demand{"cpu": {Source: "declared"}, "memory": {Source: "declared"}}}
	want(t, Decide(m, o, both, self, m.NewDecisionID("admit")), Decline, "nothing to price")

	d := Decide(m, offer(0), both, self, m.NewDecisionID("admit"))
	want(t, d, Accept, "")
	if l := line(t, d, "cpu_contention_ratio"); !ptrNear(l.UsagePred, 0.05) || l.Share != 0 {
		t.Fatalf("cpu line %+v; want share 0, prediction the pressure", l)
	}
}

func TestMapBasisDecides(t *testing.T) {
	m, _ := node(t, 0.4, 0.02)
	effect(t, m, "pod:a", 0.2)
	effect(t, m, "pod:b", 0.3)
	d := Decide(m, offer(1), both, self, m.NewDecisionID("admit"))
	want(t, d, Accept, "")
	l := line(t, d, "cpu_contention_ratio")
	if !ptrNear(l.K, 0.15) || !ptrNear(l.MapPred, 0.0575) ||
		!ptrNear(l.UsagePred, 0.0325) || l.KObservations != 1 || l.Basis != "map" || !l.Pass {
		t.Fatalf("cpu line %+v; want the strongest single live-source estimate", l)
	}
	if !near(l.Share, 0.25) || !near(l.Pressure, 0.02) || !near(l.Usage, 0.4) || !near(l.Exit, 0.10) {
		t.Fatalf("cpu line inputs %+v", l)
	}
	if l.KLastSource != "cpu_utilization@pod:b" {
		t.Fatalf("k source %q; want the strongest live source property", l.KLastSource)
	}
	if l.KLastDecision != "verify-pod:b" {
		t.Fatalf("k decision %q; want the measured intervention decision", l.KLastDecision)
	}
}

func TestMapBasisIgnoresEffectAfterSourceRetires(t *testing.T) {
	m, _ := node(t, 0.4, 0.02)
	effect(t, m, "pod:gone", 0.3)
	if err := m.RetireProperty("cpu_utilization@pod:gone", "subject left", "test"); err != nil {
		t.Fatal(err)
	}

	d := Decide(m, offer(1), both, self, m.NewDecisionID("admit"))
	l := line(t, d, "cpu_contention_ratio")
	if l.Basis != "usage" || l.K != nil || l.MapPred != nil {
		t.Fatalf("retired source still influenced prediction: %+v", l)
	}
}

func TestMapBasisPreservesResponseSlopeAboveOne(t *testing.T) {
	m, _ := node(t, 0.4, 0.01)
	record(t, m, "memory_admission_headroom_ratio", "", 0.5)
	from := "memory_utilization@pod:hog"
	record(t, m, from, "pod:hog", 0.1)
	if err := m.DeclareRelationship(statemap.Relationship{From: from, To: "memory_contention_ratio", Sign: 1}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.RecordIntervention(statemap.InterventionOutcome{
		From: from, To: "memory_contention_ratio", Agreed: true, DecisionID: "verify-sharp-response",
		HasEffect: true, Effect: 4.8,
	}); err != nil {
		t.Fatal(err)
	}
	o := offer(0.4)
	o.Demand["memory"] = Demand{Value: 0.05 * (8 << 30), Source: "declared"}
	d := Decide(m, o, []Domain{memDom}, self, m.NewDecisionID("admit"))
	want(t, d, Decline, "memory_contention_ratio would reach")
	l := line(t, d, "memory_contention_ratio")
	if l.Basis != "map" || !ptrNear(l.K, 2) || !ptrNear(l.MapPred, 0.10) {
		t.Fatalf("memory line %+v; want capped/shrunk response slope 2", l)
	}
}

func TestPredictionChangedRequiresEveryFailureToBePredictionDriven(t *testing.T) {
	for _, failure := range []string{"missing capacity", "missing reading", "memory headroom"} {
		t.Run(failure, func(t *testing.T) {
			m, _ := node(t, 0.2, 0.08)
			effect(t, m, "pod:a", 3.2)
			domains := []Domain{cpuDom, memDom}
			switch failure {
			case "missing capacity":
				domains[1].Capacity = "missing_capacity"
			case "missing reading":
				domains[1].Relieves = "custom_pressure"
				record(t, m, "custom_pressure@pod:a", "pod:a", 0.01)
			case "memory headroom":
				record(t, m, "memory_admission_headroom_ratio", "", 0)
			}
			got := Decide(m, offer(1.6), domains, self, m.NewDecisionID("admit"))
			if got.Verdict != Decline || !got.Lines[0].predictionDeclined ||
				got.Lines[1].Pass || got.Lines[1].predictionDeclined || got.PredictionChanged {
				t.Fatalf("decision %+v; an independent non-prediction failure must exclude changed_by_prediction", got)
			}
		})
	}
}

func TestUsageBasisFallback(t *testing.T) {
	m, _ := node(t, 0.4, 0.02)
	d := Decide(m, offer(1), both, self, m.NewDecisionID("admit"))
	want(t, d, Accept, "")
	l := line(t, d, "cpu_contention_ratio")
	if l.MapPred != nil || l.K != nil || l.Basis != "usage" || !ptrNear(l.UsagePred, 0.0325) {
		t.Fatalf("cpu line %+v", l)
	}
}

func TestUnavailableMemoryHeadroomIsNamedAndFallsBack(t *testing.T) {
	tests := []struct {
		name   string
		status string
		setup  func(*testing.T, *statemap.Map)
	}{
		{name: "missing", status: "missing"},
		{name: "never observed", status: "no observations", setup: func(t *testing.T, m *statemap.Map) {
			if err := m.DeclareProperty(statemap.Property{ID: "memory_admission_headroom_ratio", Range: unit}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "marked stale", status: "stale", setup: func(t *testing.T, m *statemap.Map) {
			record(t, m, "memory_admission_headroom_ratio", "", 0.005)
			if !m.MarkPropertyStale("memory_admission_headroom_ratio", "test", "test") {
				t.Fatal("headroom property was not marked stale")
			}
		}},
		{name: "expired", status: "stale", setup: func(t *testing.T, m *statemap.Map) {
			record(t, m, "memory_admission_headroom_ratio", "", 0.005)
			m.SetClock(func() time.Time { return t0.Add(m.StaleAfter() + time.Second) })
			m.SetMonotonic(func() time.Duration { return 2*time.Second + m.StaleAfter() })
		}},
		{name: "retired", status: "retired", setup: func(t *testing.T, m *statemap.Map) {
			record(t, m, "memory_admission_headroom_ratio", "", 0.005)
			if err := m.RetireProperty("memory_admission_headroom_ratio", "test", "test"); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := node(t, 0.4, 0.02)
			if tc.setup != nil {
				tc.setup(t, m)
			}
			d := Decide(m, offer(1), both, self, m.NewDecisionID("admit"))
			want(t, d, Accept, "")
			l := line(t, d, "memory_contention_ratio")
			note := "memory admission headroom unavailable: " + tc.status
			if l.MemoryHeadroom != nil || l.Basis != "usage" || !strings.Contains(l.Reason, note) {
				t.Fatalf("memory line %+v; want fallback and caveat %q", l, note)
			}
		})
	}
}

func TestMemoryHeadroomBoundsCalmAdmission(t *testing.T) {
	m, _ := node(t, 0.4, 0.02)
	record(t, m, "memory_admission_headroom_ratio", "", 0.005)
	from := "memory_utilization@pod:a"
	record(t, m, from, "pod:a", 0.1)
	if err := m.DeclareRelationship(statemap.Relationship{From: from, To: "memory_contention_ratio", Sign: 1, Label: "drives"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.RecordIntervention(statemap.InterventionOutcome{
		From: from, To: "memory_contention_ratio", Agreed: true, DecisionID: "memory-verify", HasEffect: true, Effect: 0.1,
	}); err != nil {
		t.Fatal(err)
	}
	d := Decide(m, offer(1), both, self, m.NewDecisionID("admit"))
	want(t, d, Decline, "memory demand share")
	l := line(t, d, "memory_contention_ratio")
	if l.Pass || l.Basis != "memory-headroom" || !ptrNear(l.MemoryHeadroom, 0.005) ||
		l.MapPred == nil || l.UsagePred == nil {
		t.Fatalf("memory line %+v", l)
	}
}

// TestExitBoundary: a prediction at Exit passes, one just above it does not.
func TestExitBoundary(t *testing.T) {
	m, _ := node(t, 0.4, 0.02)
	effect(t, m, "pod:a", 0.3)
	pred := *line(t, Decide(m, offer(1), both, self, m.NewDecisionID("admit")), "cpu_contention_ratio").MapPred

	c := cpuDom
	c.Exit = pred
	want(t, Decide(m, offer(1), []Domain{c, memDom}, self, m.NewDecisionID("admit")), Accept, "")
	c.Exit = pred - 1e-9
	d := Decide(m, offer(1), []Domain{c, memDom}, self, m.NewDecisionID("admit"))
	want(t, d, Decline, "cpu_contention_ratio would reach")
	if l := line(t, d, "cpu_contention_ratio"); l.Pass || l.Basis != "map" {
		t.Fatalf("cpu line %+v", l)
	}
	if l := line(t, d, "memory_contention_ratio"); !l.Pass {
		t.Fatalf("memory line %+v; every domain is evaluated", l)
	}
}

// TestNoSubjectsReadsZero: a node running nothing has no contention reading, which
// is a reading of 0, not a gap.
func TestNoSubjectsReadsZero(t *testing.T) {
	m, _ := newMap(t)
	cores(t, m, 4)
	memTotal(t, m, 8<<30)
	record(t, m, "cpu_utilization", "", 0.4)
	record(t, m, "memory_utilization", "", 0.2)
	d := Decide(m, offer(1), both, self, m.NewDecisionID("admit"))
	want(t, d, Accept, "")
	l := line(t, d, "cpu_contention_ratio")
	if l.Pressure != 0 || !strings.Contains(l.Reason, "no subject reports cpu_contention_ratio") || !ptrNear(l.UsagePred, 0) {
		t.Fatalf("cpu line %+v", l)
	}
}

// TestSubjectsWithoutContentionReadZero: subjects that report no contention of
// their own (units, groups, BestEffort pods) leave node pressure absent, and the
// node is calm, not unpredictable.
func TestSubjectsWithoutContentionReadZero(t *testing.T) {
	m, _ := node(t, 0.4, -1) // pod:a reports usage only
	record(t, m, "cpu_utilization@unit:x", "unit:x", 0.05)
	d := Decide(m, offer(1), both, self, m.NewDecisionID("admit"))
	want(t, d, Accept, "")
	l := line(t, d, "cpu_contention_ratio")
	if l.Pressure != 0 || !strings.Contains(l.Reason, "no subject reports cpu_contention_ratio") || !l.Pass {
		t.Fatalf("cpu line %+v", l)
	}
}

// TestReporterWithoutPressureUnpredictable: a subject reports contention but the
// node has no reading, so the node's worst is unknown.
func TestReporterWithoutPressureUnpredictable(t *testing.T) {
	m, _ := node(t, 0.4, -1)
	record(t, m, "cpu_contention_ratio@pod:a", "pod:a", 0.01)
	d := Decide(m, offer(1), both, self, m.NewDecisionID("admit"))
	want(t, d, Decline, "no cpu_contention_ratio reading")
	if l := line(t, d, "cpu_contention_ratio"); l.Pass || l.MapPred != nil || l.UsagePred != nil {
		t.Fatalf("cpu line %+v", l)
	}
}

// TestMemoryReporterWithoutPressureUnpredictable: memory's per-pod reporter is
// memory_pressure_ratio@pod, as in Admit, so an active one with no node
// memory_contention_ratio reading is a gap, not a calm node.
func TestMemoryReporterWithoutPressureUnpredictable(t *testing.T) {
	m, _ := newMap(t)
	cores(t, m, 4)
	memTotal(t, m, 8<<30)
	record(t, m, "cpu_utilization", "", 0.4)
	record(t, m, "memory_utilization", "", 0.2)
	record(t, m, "cpu_contention_ratio", "", 0.02)
	record(t, m, "memory_pressure_ratio@pod:x", "pod:x", 0.01)
	d := Decide(m, offer(1), both, self, m.NewDecisionID("admit"))
	want(t, d, Decline, "no memory_contention_ratio reading")
	if l := line(t, d, "memory_contention_ratio"); l.Pass || l.UsagePred != nil {
		t.Fatalf("memory line %+v", l)
	}
}

// staleNode is node(t, 0.4, 0.02) whose memory_contention_ratio was last read
// before the staleness window and has been swept stale; every other reading is
// fresh.
func staleNode(t *testing.T) *statemap.Map {
	t.Helper()
	m, _ := newMap(t)
	cores(t, m, 4)
	memTotal(t, m, 8<<30)
	old := t0.Add(-2 * m.StaleAfter())
	if err := m.Record(statemap.Observation{ID: "memory_contention_ratio", Value: 0.04, At: old, Range: &unit}); err != nil {
		t.Fatal(err)
	}
	m.Sweep()
	record(t, m, "cpu_utilization", "", 0.4)
	record(t, m, "memory_utilization", "", 0.2)
	record(t, m, "cpu_contention_ratio", "", 0.02)
	if p, _ := m.Property("memory_contention_ratio"); p.Status != statemap.Stale {
		t.Fatalf("memory_contention_ratio %s; want stale", p.Status)
	}
	return m
}

// TestStaleWithoutReporterReadsZero: a stale node reading with no reporter is an
// emptied node, read as 0, as Admit reads it.
func TestStaleWithoutReporterReadsZero(t *testing.T) {
	m := staleNode(t)
	d := Decide(m, offer(1), both, self, m.NewDecisionID("admit"))
	want(t, d, Accept, "")
	l := line(t, d, "memory_contention_ratio")
	if l.Pressure != 0 || !strings.Contains(l.Reason, "memory_contention_ratio is stale and no subject reports it") {
		t.Fatalf("memory line %+v", l)
	}
}

// TestStaleWithReporterUnpredictable: a stale node reading while a pod still
// reports is a gap. Admit's gate declines it first, so the lever's enter is 0 here
// to show the prediction step judges it the same way.
func TestStaleWithReporterUnpredictable(t *testing.T) {
	m := staleNode(t)
	record(t, m, "memory_pressure_ratio@pod:x", "pod:x", 0.01)
	want(t, Decide(m, offer(1), both, self, m.NewDecisionID("admit")), Decline, "the map's reading of memory_contention_ratio is stale")

	mem := memDom
	mem.Enter = 0
	d := Decide(m, offer(1), []Domain{cpuDom, mem}, self, m.NewDecisionID("admit"))
	want(t, d, Decline, "memory_contention_ratio is stale while memory_pressure_ratio@pod:x still reports it")
	if l := line(t, d, "memory_contention_ratio"); l.Pass || l.UsagePred != nil {
		t.Fatalf("memory line %+v", l)
	}
}

// TestMissingCapacity: a domain whose capacity fact the gate does not read is
// unpredictable; the gate reads only cpu_cores and memory_total.
func TestMissingCapacity(t *testing.T) {
	m, _ := node(t, 0.4, 0.02)
	c := cpuDom
	c.Capacity = "cpu_quota_cores"
	want(t, Decide(m, offer(1), []Domain{c, memDom}, self, m.NewDecisionID("admit")), Decline, "no cpu_quota_cores fact")
}

// TestNoBasis: zero usage and no measured effects leave nothing to predict from.
func TestNoBasis(t *testing.T) {
	m, _ := node(t, 0, 0.02)
	want(t, Decide(m, offer(1), both, self, m.NewDecisionID("admit")), Decline, "no basis to predict cpu_contention_ratio")
}

func TestDecisionRecorded(t *testing.T) {
	m, j := node(t, 0.4, 0.02)
	effect(t, m, "pod:a", 0.3)
	id := m.NewDecisionID("admit")
	d := Decide(m, offer(1), both, self, id)
	rec, ok := j.Decision(id)
	if !ok {
		t.Fatal("no decision recorded")
	}
	if rec.Answer["verdict"] != d.Verdict || rec.Answer["reason"] != d.Reason {
		t.Fatalf("answer %v; want %s %q", rec.Answer, d.Verdict, d.Reason)
	}
	if _, ok := rec.Inputs["offer"].(Offer); !ok {
		t.Fatalf("offer input %T", rec.Inputs["offer"])
	}
	lines, ok := rec.Inputs["domains"].([]Line)
	if !ok || len(lines) != 2 {
		t.Fatalf("domains input %v", rec.Inputs["domains"])
	}
	if lines[0].MapPred == nil || lines[0].UsagePred == nil {
		t.Fatalf("recorded cpu line %+v; want both predictions", lines[0])
	}
	if gate, _ := rec.Inputs["gate"].(string); gate != id+"-gate" {
		t.Fatalf("gate input %v", rec.Inputs["gate"])
	}
	if _, ok := j.Decision(id + "-gate"); !ok {
		t.Fatal("gate decision not recorded")
	}

	// A decline before the gate is recorded too.
	id = m.NewDecisionID("admit")
	Decide(m, offer(1), nil, self, id)
	if rec, ok := j.Decision(id); !ok || rec.Answer["verdict"] != Decline {
		t.Fatalf("early decline recorded %v %v", ok, rec)
	}
}

func feed(t *testing.T, m *statemap.Map, domain, basis string, n int, absErr float64) {
	t.Helper()
	for i := 0; i < n; i++ {
		m.RecordPrediction("admission."+domain, basis, absErr, 0, true)
	}
}

func TestBasisChosenByCalibration(t *testing.T) {
	const dom = "cpu_contention_ratio"
	for _, c := range []struct {
		name               string
		mapN, useN         int
		mapAbs, useAbs     float64
		basis, by          string
		bothCalibrationKey bool
	}{
		{"both below N", 4, 4, 0.05, 0.01, "map", "default", false},
		{"map below N", 4, 6, 0.05, 0.01, "map", "default", false},
		{"usage better", 5, 5, 0.05, 0.01, "usage", "calibration", true},
		{"map better", 5, 5, 0.01, 0.05, "map", "calibration", true},
		{"tie", 5, 5, 0.02, 0.02, "map", "calibration", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, j := node(t, 0.4, 0.02)
			effect(t, m, "pod:a", 0.2)
			feed(t, m, dom, "map", c.mapN, c.mapAbs)
			feed(t, m, dom, "usage", c.useN, c.useAbs)
			id := m.NewDecisionID("admit")
			dd := cpuDom
			if c.basis == "usage" {
				dd.Exit = 0.05 // map (0.07) would decline, usage (0.0325) accepts
			}
			d := Decide(m, offer(1), []Domain{dd, memDom}, self, id)
			l := line(t, d, dom)
			if l.Basis != c.basis || l.ChosenBy != c.by {
				t.Fatalf("basis %s by %s; want %s by %s", l.Basis, l.ChosenBy, c.basis, c.by)
			}
			if c.basis == "usage" {
				want(t, d, Accept, "")
			}
			rec, _ := j.Decision(id)
			if got := rec.Inputs["basis_chosen_by"].(map[string]string)[dom]; got != c.by {
				t.Fatalf("recorded basis_chosen_by %q; want %q", got, c.by)
			}
			cal, _ := rec.Inputs["calibration"].(map[string]statemap.Calibration)
			_, hasMap := cal["admission."+dom+"/map"]
			_, hasUsage := cal["admission."+dom+"/usage"]
			if c.bothCalibrationKey && !(hasMap && hasUsage) {
				t.Fatalf("calibration input %v; want both cpu keys", cal)
			}
		})
	}
}

func TestBasisChoiceNeedsBothPredictions(t *testing.T) {
	m, _ := node(t, 0.4, 0.02)
	feed(t, m, "cpu_contention_ratio", "map", 10, 0.001)
	feed(t, m, "cpu_contention_ratio", "usage", 10, 0.5)
	l := line(t, Decide(m, offer(1), both, self, m.NewDecisionID("admit")), "cpu_contention_ratio")
	if l.Basis != "usage" || l.ChosenBy != "default" {
		t.Fatalf("basis %s by %s; want usage by default", l.Basis, l.ChosenBy)
	}
}

// regimeNode is node(0.4, 0.02) on a learning map whose clock the caller advances.
func regimeNode(t *testing.T) (*statemap.Map, *statemap.Journal, *time.Time) {
	t.Helper()
	j := statemap.NewJournal(0)
	m := statemap.New(statemap.Config{AdmitUnknown: true, Alpha: 1, Learn: true,
		LearnConfig: statemap.LearnConfig{PairWindowSeconds: 15, MinSupport: 8, Window: 10}}, j)
	now := t0
	m.SetClock(func() time.Time { return now })
	m.SetMonotonic(func() time.Duration { return now.Sub(t0) + time.Second })
	cores(t, m, 4)
	memTotal(t, m, 8<<30)
	return m, j, &now
}

// driveRegime feeds correlated, then opposed pairs on cpu_utilization@<subject>
// -> cpu_contention_ratio until the relationship is in regime, then records the node
// readings node(0.4, 0.02) holds, so Decide sees the same state.
func driveRegime(t *testing.T, m *statemap.Map, now *time.Time, subject, id string, limit int) {
	t.Helper()
	from := "cpu_utilization@" + subject
	obs := func(prop, sub string, v float64, ev string) {
		*now = now.Add(time.Second)
		if err := m.Record(statemap.Observation{ID: prop, Value: v, At: *now, Subject: sub, Range: &unit, EventID: ev}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; ; i++ {
		if i == limit {
			t.Fatalf("no regime after %d pairs", limit)
		}
		x := float64(i%10) / 10
		y := x
		if i >= 60 {
			y = 1 - x // against the declared sign: strength folds as zero
		}
		obs(from, subject, x, "x"+strconv.Itoa(i))
		obs("cpu_contention_ratio", "", y, "y"+strconv.Itoa(i))
		if r, _ := m.Relationship(id); r.Regime {
			break
		}
	}
	for _, r := range []struct {
		id, subject string
		v           float64
	}{{"cpu_utilization", "", 0.4}, {"memory_utilization", "", 0.2}, {"memory_contention_ratio", "", 0},
		{"cpu_contention_ratio", "", 0.02}, {from, subject, 0.1}} {
		obs(r.id, r.subject, r.v, "")
	}
}

func TestAcceptCaveatsARegimeRelationship(t *testing.T) {
	m, j, now := regimeNode(t)
	record(t, m, "cpu_contention_ratio", "", 0.02)
	effect(t, m, "pod:a", 0.2)
	id := statemap.RelationshipID("cpu_utilization@pod:a", "cpu_contention_ratio", "drives")
	driveRegime(t, m, now, "pod:a", id, 400)

	d := Decide(m, offer(1), both, self, m.NewDecisionID("admit"))
	want(t, d, Accept, "")
	rec, _ := j.Decision(d.ID)
	caveat := false
	for _, c := range rec.Caveats {
		caveat = caveat || strings.HasPrefix(c, "relationship "+id+": present differs from established")
	}
	if !caveat {
		t.Fatalf("caveats %q; want the regime caveat on %s", rec.Caveats, id)
	}
	read := false
	for _, r := range rec.RelationshipsRead {
		read = read || r.ID == id
	}
	if !read {
		t.Fatalf("relationships read %+v; want %s", rec.RelationshipsRead, id)
	}
}
