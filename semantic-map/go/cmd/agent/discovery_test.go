package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/explain"
	"github.com/DiyazY/di-agent/pkg/profiles"
	"github.com/DiyazY/di-agent/pkg/statemap"
	"github.com/DiyazY/di-agent/pkg/types"
)

// TestNodePairDiscoveredAndConfirmedWithADirection: two node-level properties that
// move together become one undirected candidate. Confirming it without saying which
// end it runs from is refused with 400 and leaves it pending; confirming with "from"
// declares a Discovered relationship in the state model running that way.
func TestNodePairDiscoveredAndConfirmedWithADirection(t *testing.T) {
	state := newTestState(t)
	sm, _, err := profiles.Build("edge-minimal", profiles.Config{
		StateMap: state, EMAAlpha: 0.2, ConvergenceThreshold: 500, MinTrustScore: 0.5,
		UseRuleBasedTuner: true, UseProposer: true, ProposerThreshold: 0.8, ProposerMinPairs: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerRoutes(mux, sm, explain.NewDisabled())
	registerStateRoutes(mux, state, sm.Discovery, nil)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	rng := [2]float64{0, 1}
	t0 := time.Now().Add(-10 * time.Minute)
	for i := 0; i < 30; i++ {
		x := float64(i%10) / 10
		at := t0.Add(time.Duration(i) * 5 * time.Second).Unix()
		for _, s := range []*types.MetricSample{
			{MetricType: types.CPUUtilization, Value: 0.1 + 0.8*x},
			{MetricType: types.CPUPressureRatio, Value: 0.05 + 0.4*x},
		} {
			s.Unit, s.Range, s.TimestampUnix = "fraction", &rng, at
			s.EventID = string(s.MetricType) + time.Unix(at, 0).Format(time.RFC3339)
			if err := sm.IngestSample(s); err != nil {
				t.Fatalf("ingest %s: %v", s.MetricType, err)
			}
		}
	}

	const id = "cpu_pressure_ratio<->cpu_utilization"
	cands, _ := sm.PendingCandidates()
	var found bool
	for _, c := range cands {
		if c.CandidateID == id && c.Undirected {
			found = true
		}
	}
	if !found {
		t.Fatalf("no undirected %s among %d pending candidates", id, len(cands))
	}

	confirm := func(body string) int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/candidates/"+url.PathEscape(id)+"/confirm", bytes.NewBufferString(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := confirm(""); code != http.StatusBadRequest {
		t.Errorf("confirm without a direction: %d; want 400", code)
	}
	if code := confirm(`{"from":"memory_utilization"}`); code != http.StatusBadRequest {
		t.Errorf("confirm from a property that is not an end: %d; want 400", code)
	}
	if code := confirm(`{"from":"cpu_utilization"}`); code != http.StatusNoContent {
		t.Fatalf("confirm from cpu_utilization: %d; want 204", code)
	}

	// The confirmed candidate leaves the pending list and stays in the history.
	list := func(query string) (int, []types.CandidateEdge) {
		resp, err := http.Get(srv.URL + "/candidates" + query)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var cs []types.CandidateEdge
		if resp.StatusCode == http.StatusOK {
			_ = json.NewDecoder(resp.Body).Decode(&cs)
		}
		return resp.StatusCode, cs
	}
	has := func(cs []types.CandidateEdge) bool {
		for _, c := range cs {
			if c.CandidateID == id {
				return c.Status == types.Confirmed
			}
		}
		return false
	}
	if _, cs := list(""); has(cs) {
		t.Error("a confirmed candidate is still listed as pending")
	}
	for _, q := range []string{"?status=all", "?status=confirmed"} {
		if code, cs := list(q); code != http.StatusOK || !has(cs) {
			t.Errorf("GET /candidates%s: %d; want 200 listing %s as confirmed", q, code, id)
		}
	}
	if _, cs := list("?status=rejected"); len(cs) != 0 {
		t.Errorf("?status=rejected lists %d candidates; none was rejected", len(cs))
	}
	if code, _ := list("?status=bogus"); code != http.StatusBadRequest {
		t.Errorf("?status=bogus: %d; want 400", code)
	}

	var v statemap.StateView
	if code := getState(t, srv.URL+"/state", &v); code != 200 {
		t.Fatalf("GET /state: %d", code)
	}
	for _, r := range v.Relationships {
		if r.From == "cpu_utilization" && r.To == "cpu_pressure_ratio" {
			if r.Provenance != statemap.Discovered {
				t.Errorf("provenance %q; want discovered", r.Provenance)
			}
			b, _ := json.Marshal(r)
			t.Logf("declared: %s", b)
			return
		}
	}
	t.Error("no cpu_utilization -> cpu_pressure_ratio relationship after the confirm")
}

// TestStateReportsHowFarDiscoveryWasRationed: GET /state carries the proposer's
// counts, so an empty candidate list can be told apart from a budget too small to
// look. An agent without a proposer reports no discovery block at all.
func TestStateReportsHowFarDiscoveryWasRationed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		proposer bool
	}{{"budget of one", true}, {"no proposer", false}} {
		t.Run(tc.name, func(t *testing.T) {
			state := newTestState(t)
			sm, _, err := profiles.Build("edge-minimal", profiles.Config{
				StateMap: state, EMAAlpha: 0.2, ConvergenceThreshold: 500, MinTrustScore: 0.5,
				UseRuleBasedTuner: true, UseProposer: tc.proposer, ProposerMaxPairs: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			registerStateRoutes(mux, state, sm.Discovery, nil)
			srv := httptest.NewServer(mux)
			defer srv.Close()

			rng := [2]float64{0, 1}
			at := time.Now().Add(-time.Minute).Unix()
			for i, mt := range []types.MetricType{types.CPUUtilization, types.MemoryUtilization, types.CPUPressureRatio} {
				s := &types.MetricSample{MetricType: mt, Value: 0.1 * float64(i+1), Unit: "fraction", Range: &rng,
					TimestampUnix: at, EventID: string(mt)}
				if err := sm.IngestSample(s); err != nil {
					t.Fatalf("ingest %s: %v", mt, err)
				}
			}
			var v stateResponse
			if code := getState(t, srv.URL+"/state", &v); code != 200 {
				t.Fatalf("GET /state: %d", code)
			}
			if !tc.proposer {
				if v.Discovery != nil {
					t.Errorf("discovery %+v reported with the proposer off", v.Discovery)
				}
				return
			}
			d := v.Discovery
			if d == nil {
				t.Fatal("no discovery block in /state")
			}
			if d.Budget != 1 || d.Properties != 3 || d.EligiblePairs != 3 || d.TestedPairs != 1 || d.TestedByClass[types.PairNodeNode] != 1 {
				t.Errorf("discovery %+v; want budget 1, 3 properties, 3 eligible, 1 node<->node tested", d)
			}
			if d.FDR != 0.05 || d.PBar <= 0 {
				t.Errorf("fdr %.3f, bar %.3g; want the default 5%% rate and the bar it gives", d.FDR, d.PBar)
			}
		})
	}
}
