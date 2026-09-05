package scripted

import (
	"math"
	"testing"
	"time"
)

func scenarioForTest() *Scenario {
	depart, ret := 300, 400
	return &Scenario{
		Name: "t", Seed: 7, TickSeconds: 10, DurationSeconds: 600, Noise: 0,
		Node: map[string]Coupling{
			"node_cpu": {Coupling: "sum", Base: 0.1, Of: "cpu_utilization"},
			"pressure": {Coupling: "logistic", Theta: 0.5, K: 0.05, Of: "node_cpu"},
			"idle":     {Coupling: "none", Base: 0.02},
		},
		Subjects: []SubjectSpec{{
			ID: "pod:a", Arrive: 0, Depart: &depart, Return: &ret,
			Properties: map[string]PropertySpec{"cpu_utilization": {Pattern: "constant", Value: 0.3}},
		}},
	}
}

func TestSystemScriptEmitsScopedAndNodeSamplesDeterministically(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a := NewSystemScript("sim", scenarioForTest(), start)
	b := NewSystemScript("sim", scenarioForTest(), start)
	sa, _ := a.Collect()
	sb, _ := b.Collect()
	if len(sa) != 4 || len(sb) != 4 {
		t.Fatalf("tick 1 emitted %d and %d samples; want 4 (one subject property + three node properties)", len(sa), len(sb))
	}
	for i := range sa {
		if sa[i].EventID != sb[i].EventID || sa[i].Value != sb[i].Value || sa[i].TimestampUnix != start.Add(10*time.Second).Unix() {
			t.Errorf("sample %d differs or is mistimed: %+v vs %+v", i, sa[i], sb[i])
		}
	}
	var scoped, node int
	for _, s := range sa {
		if s.Subject == "pod:a" {
			scoped++
			if s.Unit != "fraction" || s.Range == nil {
				t.Errorf("scoped sample lacks declared unit/range: %+v", s)
			}
		} else {
			node++
		}
	}
	if scoped != 1 || node != 3 {
		t.Errorf("scoped=%d node=%d", scoped, node)
	}
}

func TestSystemScriptNodeTruthFollowsCouplings(t *testing.T) {
	s := NewSystemScript("sim", scenarioForTest(), time.Now())
	v := s.NodeValues(100, nil)
	if !nearf(v["node_cpu"], 0.4) || !nearf(v["idle"], 0.02) {
		t.Errorf("node_cpu=%.3f idle=%.3f; want 0.4 and 0.02", v["node_cpu"], v["idle"])
	}
	want := 1 / (1 + math.Exp(-(0.4-0.5)/0.05))
	if !nearf(v["pressure"], want) {
		t.Errorf("pressure=%.4f want %.4f", v["pressure"], want)
	}
	over := s.NodeValues(100, map[string]float64{"cpu_utilization@pod:a": 0.8})
	if !nearf(over["node_cpu"], 0.9) {
		t.Errorf("override ignored: node_cpu=%.3f want 0.9", over["node_cpu"])
	}
}

func TestSystemScriptSubjectLifetime(t *testing.T) {
	s := NewSystemScript("sim", scenarioForTest(), time.Now())
	sub := s.sc.Subjects[0]
	for sec, want := range map[int]bool{0: true, 299: true, 300: false, 399: false, 400: true, 599: true} {
		if got := s.Active(sub, sec); got != want {
			t.Errorf("active at %ds = %v, want %v", sec, got, want)
		}
	}
	if v := s.NodeValues(350, nil); !nearf(v["node_cpu"], 0.1) {
		t.Errorf("while the subject is away node_cpu=%.3f; want the base 0.1", v["node_cpu"])
	}
}

func nearf(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
