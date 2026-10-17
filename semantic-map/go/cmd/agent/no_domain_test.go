package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/explain"
	"github.com/DiyazY/di-agent/pkg/profiles"
	"github.com/DiyazY/di-agent/pkg/statemap"
	"github.com/DiyazY/di-agent/pkg/types"
)

// TestAgentRunsWithoutADomainModel: a node may have no domain_spec.json. The agent
// still starts, still records every property it observes, and still serves the
// state model; the operations defined over declared constructs (cost, tuning)
// answer 501 and say why, rather than 500 or a cost of zero.
func TestAgentRunsWithoutADomainModel(t *testing.T) {
	state := newTestState(t)
	sm, _, err := profiles.Build("edge-minimal", profiles.Config{
		StateMap:             state,
		DomainSpec:           nil,
		EMAAlpha:             0.2,
		ConvergenceThreshold: 500,
		MinTrustScore:        0.5,
		UseRuleBasedTuner:    true,
	})
	if err != nil {
		t.Fatalf("an agent without a domain model must still build: %v", err)
	}
	mux := http.NewServeMux()
	registerRoutes(mux, sm, explain.NewDisabled())
	registerStateRoutes(mux, state, sm.Discovery, nil)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Nothing is declared before telemetry arrives.
	var v statemap.StateView
	if code := getState(t, srv.URL+"/state", &v); code != 200 || len(v.Properties) != 0 {
		t.Fatalf("fresh agent: code=%d properties=%d, want 200 and none declared", code, len(v.Properties))
	}

	// A node-level reading and a subject's reading are both admitted as properties,
	// with the unit and range they declare; nothing is routed into a construct.
	rng := [2]float64{0, 1}
	for _, s := range []*types.MetricSample{
		{MetricType: types.CPUUtilization, Value: 0.4, Unit: "fraction", Range: &rng},
		{MetricType: types.CPUUtilization, Value: 0.3, Unit: "fraction", Range: &rng, Subject: "pod:a"},
	} {
		s.TimestampUnix = time.Now().Unix()
		s.EventID = s.Subject + string(s.MetricType)
		if err := sm.IngestSample(s); err != nil {
			t.Fatalf("ingest %s@%s: %v", s.MetricType, s.Subject, err)
		}
	}
	if code := getState(t, srv.URL+"/state", &v); code != 200 {
		t.Fatalf("code=%d", code)
	}
	got := map[string]bool{}
	for _, p := range v.Properties {
		got[p.ID] = true
	}
	for _, id := range []string{"cpu_utilization", "cpu_utilization@pod:a"} {
		if !got[id] {
			t.Errorf("property %s was not admitted; the map needs no domain model to record it", id)
		}
	}

	resp, err := http.Get(srv.URL + "/cost?task=pod-scheduling")
	if err != nil {
		t.Fatal(err)
	}
	expectNoDomain(t, "GET /cost", resp)

	body, _ := json.Marshal(TuneRequest{Intent: "prioritize performance", Operator: "op"})
	resp, err = http.Post(srv.URL+"/agent/tune", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	expectNoDomain(t, "POST /agent/tune", resp)
}

func expectNoDomain(t *testing.T, what string, resp *http.Response) {
	t.Helper()
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("%s without a domain model: code=%d body=%s, want 501", what, resp.StatusCode, b)
	}
	if !strings.Contains(string(b), "without a domain model") {
		t.Errorf("%s: error %q does not say the agent runs without a domain model", what, b)
	}
}

// TestNoAdmitWithoutADomainModelIsRefused: with nothing declared, -no-admit would
// drop every sample while the agent looked healthy. A restored snapshot supplies
// declared properties, so that combination is still allowed.
func TestNoAdmitWithoutADomainModelIsRefused(t *testing.T) {
	if err := checkCanRecord(false, true, 0); err == nil {
		t.Error("-no-admit with no domain model and no restored state must be refused")
	}
	for _, c := range []struct {
		hasSpec, noAdmit bool
		restored         int
	}{{true, true, 0}, {false, false, 0}, {false, true, 3}} {
		if err := checkCanRecord(c.hasSpec, c.noAdmit, c.restored); err != nil {
			t.Errorf("spec=%v no-admit=%v restored=%d: %v", c.hasSpec, c.noAdmit, c.restored, err)
		}
	}
}
