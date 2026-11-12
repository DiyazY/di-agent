// internal/minimal/tests/reflective_test.go
package minimal_test

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/admission"
	"github.com/DiyazY/di-agent/pkg/statemap"
)

// TestScenario_ReflectiveCore: one map watches itself. A relationship the machine
// keeps contradicting is retired by the agent; one that held and then stopped holding
// says so; a prediction basis that has been worse than the other is no longer the one
// admission trusts; and all of it survives a restart. The log lines are the evidence.
func TestScenario_ReflectiveCore(t *testing.T) {
	var (
		wall = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
		mono = time.Second
		unit = [2]float64{0, 1}
		cfg  = statemap.Config{Owner: "n1", ConvergenceObservations: 500, Alpha: 0.2, AdmitUnknown: true, Learn: true,
			LearnConfig: statemap.LearnConfig{PairWindowSeconds: 15, MinSupport: 8, Window: 10}}
	)
	step := func(d time.Duration) { wall, mono = wall.Add(d), mono+d }
	m := statemap.New(cfg, statemap.NewJournal(0))
	m.SetClock(func() time.Time { return wall })
	m.SetMonotonic(func() time.Duration { return mono })

	observe := func(id string, v float64, ev string) {
		t.Helper()
		step(time.Second)
		if err := m.ObserveEvent(id, v, wall, ev); err != nil {
			t.Fatal(err)
		}
	}
	// pair feeds the i-th pair of x and y(x), x cycling through ten values.
	pair := func(from, to, tag string, i int, y func(float64) float64) {
		x := float64(i%10) / 10
		observe(from, x, tag+"x"+strconv.Itoa(i))
		observe(to, y(x), tag+"y"+strconv.Itoa(i))
	}
	for _, id := range []string{"resource", "pressure", "queue", "latency"} {
		if err := m.DeclareProperty(statemap.Property{ID: id, Range: unit}); err != nil {
			t.Fatal(err)
		}
	}
	eventsOf := func(kind statemap.EventKind, target string) []statemap.Event {
		var out []statemap.Event
		for _, e := range m.Journal().Events(0, 0) {
			if e.Kind == kind && e.Target == target {
				out = append(out, e)
			}
		}
		return out
	}

	// (a) A seeded relationship the machine contradicts is retired by the agent.
	for _, r := range []statemap.Relationship{
		{From: "resource", To: "pressure", Label: "backwards", Sign: -1},
		{From: "resource", To: "pressure", Label: "correct", Sign: 1},
	} {
		if err := m.DeclareRelationship(r); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 40; i++ {
		pair("resource", "pressure", "a", i, func(x float64) float64 { return 0.9*x + 0.02 })
	}
	backId := statemap.RelationshipID("resource", "pressure", "backwards")
	back, _ := m.Relationship(backId)
	good, _ := m.Relationship(statemap.RelationshipID("resource", "pressure", "correct"))
	retired := eventsOf(statemap.EventRelationshipRetired, backId)
	if back.Status != statemap.Retired || back.RetiredReason != statemap.ReasonSignContradicted ||
		good.Status != statemap.Active || len(retired) != 1 || retired[0].Actor != "agent" {
		t.Fatalf("backwards %v %q, correct %v, retired events %+v", back.Status, back.RetiredReason, good.Status, retired)
	}
	census := m.Census()
	if census.RelationshipsLive != 1 || census.RelationshipsTotal != 2 {
		t.Fatalf("census %+v; want 1 live of 2", census)
	}
	t.Logf("(a) 40 pairs of rising resource and rising pressure against a relationship declared sign -1:")
	t.Logf("    %s actor=%s %v", retired[0].Kind, retired[0].Actor, retired[0].Detail)
	t.Logf("    census: %d relationships live, %d retired; the correctly signed one is %v",
		census.RelationshipsLive, census.RelationshipsTotal-census.RelationshipsLive, good.Status)

	// (b) A correctly signed relationship that held, then stopped holding, is in regime.
	if err := m.DeclareRelationship(statemap.Relationship{From: "queue", To: "latency", Label: "raises", Sign: 1}); err != nil {
		t.Fatal(err)
	}
	relID := statemap.RelationshipID("queue", "latency", "raises")
	for i := 0; i < 300; i++ {
		pair("queue", "latency", "b", i, func(x float64) float64 { return x })
	}
	if r, _ := m.Relationship(relID); r.Regime {
		t.Fatal("regime set while the relationship held")
	}
	for i := 0; i < 100; i++ {
		pair("queue", "latency", "c", i, func(x float64) float64 { return 1 - x })
		if r, _ := m.Relationship(relID); r.Regime {
			break
		}
	}
	rel, _ := m.Relationship(relID)
	regime := eventsOf(statemap.EventRelationshipRegime, relID)
	if !rel.Regime || rel.Status != statemap.Active || len(regime) != 1 || regime[0].Detail["started"] != true {
		t.Fatalf("regime %v status %v events %+v", rel.Regime, rel.Status, regime)
	}
	est := m.Estimate(statemap.EstimateRequest{Target: "latency"})
	var caveat string
	for _, c := range est.Caveats {
		if strings.Contains(c, "present differs from established") {
			caveat = c
		}
	}
	if caveat == "" {
		t.Fatalf("estimate caveats %q; want the regime caveat", est.Caveats)
	}
	t.Logf("(b) 300 pairs that held, then pairs that move against the sign:")
	t.Logf("    %s %v", regime[0].Kind, regime[0].Detail)
	t.Logf("    the estimate of latency says: %s", caveat)

	// (c) Admission trusts the prediction basis with the better record.
	const dom = "cpu_contention_ratio"
	rec := func(id, subject string, v float64) {
		t.Helper()
		step(time.Second)
		if err := m.Record(statemap.Observation{ID: id, Value: v, At: wall, Subject: subject, Range: &unit}); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []statemap.Fact{statemap.NumberFact("cpu_cores", 4, "cores"), statemap.NumberFact("memory_total", 8<<30, "bytes")} {
		if err := m.RecordFacts("test", []statemap.Fact{f}); err != nil {
			t.Fatal(err)
		}
	}
	rec("cpu_utilization", "", 0.4)
	rec("memory_utilization", "", 0.2)
	rec("memory_contention_ratio", "", 0)
	rec(dom, "", 0.02)
	rec("cpu_utilization@pod:a", "pod:a", 0.1)
	if err := m.DeclareRelationship(statemap.Relationship{From: "cpu_utilization@pod:a", To: dom, Sign: 1, Label: "drives"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.RecordIntervention(statemap.InterventionOutcome{From: "cpu_utilization@pod:a", To: dom,
		Agreed: true, DecisionID: "verify-a", HasEffect: true, Effect: 0.2}); err != nil {
		t.Fatal(err)
	}
	offer := admission.Offer{Name: "web", Demand: map[string]admission.Demand{
		"cpu":    {Value: 1, Source: "declared"},
		"memory": {Value: 64 << 20, Source: "declared"},
	}}
	cpu := admission.Domain{Action: "cpu.limit", Resource: "cpu", Relieves: dom, Usage: "cpu_utilization",
		Capacity: "cpu_cores", Exit: 0.05, Enter: 0.25, Settle: time.Minute, Phase: "calm"}
	mem := admission.Domain{Action: "memory.limit", Resource: "memory", Relieves: "memory_contention_ratio",
		Usage: "memory_utilization", Capacity: "memory_total", Exit: 0.05, Enter: 0.08, Settle: time.Minute, Phase: "calm"}
	self := admission.Self{NodeID: "n1", Arch: "amd64"}

	before := admission.Decide(m, offer, []admission.Domain{cpu, mem}, self, m.NewDecisionID("admit"))
	if b := lineOf(before, dom); b.Basis != "map" || b.ChosenBy != "default" {
		t.Fatalf("before any record: basis %s by %s; want map by default", b.Basis, b.ChosenBy)
	}
	for i := 0; i < 6; i++ {
		m.RecordPrediction("admission."+dom, "map", 0.05, 0, true)   // the map missed by 0.05
		m.RecordPrediction("admission."+dom, "usage", 0.01, 0, true) // usage missed by 0.01
	}
	d := admission.Decide(m, offer, []admission.Domain{cpu, mem}, self, m.NewDecisionID("admit"))
	var l admission.Line
	for _, x := range d.Lines {
		if x.Domain == dom {
			l = x
		}
	}
	record, ok := m.Journal().Decision(d.ID)
	if !ok {
		t.Fatalf("decision %s not recorded", d.ID)
	}
	chosen, _ := record.Inputs["basis_chosen_by"].(map[string]string)
	if l.Basis != "usage" || l.ChosenBy != "calibration" || chosen[dom] != "calibration" {
		t.Fatalf("basis %s by %s, recorded %v", l.Basis, l.ChosenBy, chosen)
	}
	t.Logf("(c) six outcomes per basis, the map worse than usage:")
	for _, e := range m.CalibrationList() {
		t.Logf("    %-32s %-5s n=%d mean_err=%+.3f mean_abs_err=%.3f hit_rate=%.2f",
			e.Kind, e.Basis, e.N, e.MeanErr, e.MeanAbsErr, e.HitRate)
	}
	t.Logf("    before the record, %s used the %s basis (by %s); after it, %s: verdict %s on the %s basis, chosen by %s, recorded %v",
		dom, lineOf(before, dom).Basis, lineOf(before, dom).ChosenBy, d.ID, d.Verdict, l.Basis, l.ChosenBy, chosen)

	// (d) The record and the regime survive a restart.
	path := filepath.Join(t.TempDir(), "state.json")
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}
	restored := statemap.New(cfg, statemap.NewJournal(0))
	if ok, err := restored.Load(path); err != nil || !ok {
		t.Fatalf("load: %v %v", ok, err)
	}
	after, _ := restored.Relationship(relID)
	if !after.Regime || !after.RegimeSince.Equal(rel.RegimeSince) {
		t.Fatalf("restored regime %v since %v; had since %v", after.Regime, after.RegimeSince, rel.RegimeSince)
	}
	want, got := m.CalibrationList(), restored.CalibrationList()
	if len(got) != len(want) || len(got) != 2 {
		t.Fatalf("restored calibration %+v; want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("restored calibration %+v; want %+v", got[i], want[i])
		}
	}
	t.Logf("(d) after Save and Load: %s is in regime since %s; calibration returned with %d records, %s/%s at n=%d",
		relID, after.RegimeSince.Format(time.RFC3339), len(got), got[0].Kind, got[0].Basis, got[0].N)
}

func lineOf(d admission.Decision, domain string) admission.Line {
	for _, l := range d.Lines {
		if l.Domain == domain {
			return l
		}
	}
	return admission.Line{}
}
