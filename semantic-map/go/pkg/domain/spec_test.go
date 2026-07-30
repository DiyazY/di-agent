package domain

import (
	"os"
	"path/filepath"
	"testing"
)

// findSpec locates the committed domain_spec.json by walking up from the test's
// working directory, so the test does not hardcode a repo layout.
func findSpec(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		p := filepath.Join(dir, "domain_spec.json")
		if _, err := os.Stat(p); err == nil {
			return p
		}
		dir = filepath.Dir(dir)
	}
	t.Skip("domain_spec.json not found; skipping")
	return ""
}

func TestLoadCommittedSpec(t *testing.T) {
	s, err := Load(findSpec(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(s.Constructs) == 0 || len(s.Propositions) == 0 {
		t.Fatal("committed spec declares no constructs or no propositions")
	}

	// Every proposition endpoint must be a construct that some metric reaches.
	// This is the property that makes an edge able to carry evidence: an edge with
	// an unreachable endpoint is inert in the Reasoner, because cost accumulates
	// (effective - prior) and an unobserved edge has effective == prior.
	routed := map[string]bool{}
	for _, r := range s.MetricRouting {
		routed[r.ConstructID] = true
	}
	for _, p := range s.Propositions {
		if !routed[p.FromConstruct] {
			t.Errorf("%s: from_construct %s has no metric routed to it", p.PropositionID, p.FromConstruct)
		}
		if !routed[p.ToConstruct] {
			t.Errorf("%s: to_construct %s has no metric routed to it", p.PropositionID, p.ToConstruct)
		}
	}
}

func TestValidateRejectsInconsistency(t *testing.T) {
	base := func() *Spec {
		return &Spec{
			Constructs:    []Construct{{ConstructID: "RC", Name: "Resource"}},
			MetricRouting: []MetricRoute{{MetricType: "cpu", ConstructID: "RC", Range: [2]float64{0, 1}}},
			Propositions: []Proposition{{PropositionID: "PX", FromConstruct: "RC",
				ToConstruct: "RC", Direction: "positive"}},
			Policy: AdjustmentPolicy{GlobalFloor: 0.1, GlobalCeiling: 0.95},
		}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("baseline should be valid: %v", err)
	}

	cases := map[string]func(*Spec){
		"metric routed to unknown construct": func(s *Spec) {
			s.MetricRouting[0].ConstructID = "NOPE"
		},
		"proposition with unknown endpoint": func(s *Spec) {
			s.Propositions[0].ToConstruct = "NOPE"
		},
		"bad direction": func(s *Spec) {
			s.Propositions[0].Direction = "sideways"
		},
		"duplicate construct": func(s *Spec) {
			s.Constructs = append(s.Constructs, Construct{ConstructID: "RC"})
		},
		"duplicate metric route": func(s *Spec) {
			s.MetricRouting = append(s.MetricRouting,
				MetricRoute{MetricType: "cpu", ConstructID: "RC", Range: [2]float64{0, 1}})
		},
		"empty metric range": func(s *Spec) {
			s.MetricRouting[0].Range = [2]float64{1, 1}
		},
		"inverted policy bounds": func(s *Spec) {
			s.Policy.GlobalFloor, s.Policy.GlobalCeiling = 0.9, 0.1
		},
		"floor for unknown proposition": func(s *Spec) {
			s.Policy.PerPropositionFloor = map[string]float64{"NOPE": 0.3}
		},
		"intent adjusting unknown proposition": func(s *Spec) {
			s.IntentRules = []IntentRule{{Intent: "x", Keywords: []string{"x"},
				Deltas: map[string]float64{"NOPE": 0.1}}}
		},
		"intent with no keywords": func(s *Spec) {
			s.IntentRules = []IntentRule{{Intent: "x", Deltas: map[string]float64{"PX": 0.1}}}
		},
	}
	for name, mutate := range cases {
		s := base()
		mutate(s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s: expected rejection, got none", name)
		}
	}
}

// A construct appearing mid-deployment is the case the spec exists to support.
// Adding it is not enough on its own — without a metric route nothing can reach
// it, and it would sit in the graph accumulating no evidence while looking
// identical to a construct with nothing happening.
func TestRuntimeConstructNeedsRouteToBeReachable(t *testing.T) {
	s, err := Load(findSpec(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if _, ok := s.ConstructForMetric("thermal_headroom"); ok {
		t.Fatal("setup: thermal_headroom should not be routed yet")
	}

	// Routing before the construct exists must fail rather than create a dangling
	// reference.
	if err := s.AddMetricRoute(MetricRoute{MetricType: "thermal_headroom",
		ConstructID: "TH", Range: [2]float64{0, 1}}); err == nil {
		t.Error("routing to an undeclared construct should fail")
	}

	if err := s.AddConstruct(Construct{ConstructID: "TH", Name: "Thermal Headroom"}); err != nil {
		t.Fatalf("AddConstruct: %v", err)
	}
	if err := s.AddMetricRoute(MetricRoute{MetricType: "thermal_headroom",
		ConstructID: "TH", Unit: "fraction", Range: [2]float64{0, 1}}); err != nil {
		t.Fatalf("AddMetricRoute: %v", err)
	}

	got, ok := s.ConstructForMetric("thermal_headroom")
	if !ok || got != "TH" {
		t.Errorf("ConstructForMetric = (%q, %v), want (TH, true)", got, ok)
	}
	if err := s.Validate(); err != nil {
		t.Errorf("spec should still validate after runtime additions: %v", err)
	}

	// Idempotent: re-adding the same construct is not an error.
	if err := s.AddConstruct(Construct{ConstructID: "TH"}); err != nil {
		t.Errorf("re-adding a construct should be a no-op, got %v", err)
	}
}

func TestFloorForFallsBackToGlobal(t *testing.T) {
	s := &Spec{
		Constructs:   []Construct{{ConstructID: "RC"}},
		Propositions: []Proposition{{PropositionID: "PX", FromConstruct: "RC", ToConstruct: "RC", Direction: "positive"}},
		Policy: AdjustmentPolicy{GlobalFloor: 0.10, GlobalCeiling: 0.95,
			PerPropositionFloor: map[string]float64{"PX": 0.30}},
	}
	if got := s.FloorFor("PX"); got != 0.30 {
		t.Errorf("FloorFor(PX) = %v, want the 0.30 override", got)
	}
	if got := s.FloorFor("PY"); got != 0.10 {
		t.Errorf("FloorFor(PY) = %v, want the 0.10 global floor", got)
	}
}
