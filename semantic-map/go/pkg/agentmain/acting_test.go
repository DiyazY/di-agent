package agentmain

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/actuators"
	"github.com/DiyazY/di-agent/pkg/policy"
	"github.com/DiyazY/di-agent/pkg/relief"
	"github.com/DiyazY/di-agent/pkg/runtime"
	"github.com/DiyazY/di-agent/pkg/spec"
	"github.com/DiyazY/di-agent/pkg/statemap"
)

func actingMux(t *testing.T) *http.ServeMux {
	t.Helper()
	mux, _, _, _ := actingMuxWith(t, false)
	return mux
}

func f64(v float64) *float64 { return &v }

// actingMuxWith is the acting routes over a dry-run runtime, acting as the agent
// does under it with relief enabled (both default levers), with the relief loops
// when withLoop is set; the loop returned is cpu.limit's. pod:a is frozen and has
// no CPU limit; pod:b has a limit of 1 core.
func actingMuxWith(t *testing.T, withLoop bool) (*http.ServeMux, *relief.Loop, *runtime.DryRun, *actuators.Acting) {
	t.Helper()
	s := spec.Default()
	rt, err := runtime.Build("dry-run", runtime.Env{NodeID: "n1", Spec: s.Runtime})
	if err != nil {
		t.Fatal(err)
	}
	dr := rt.(*runtime.DryRun)
	dr.Put(runtime.Workload{Subject: "pod:a", Name: "a",
		Labels: map[string]string{"di-agent/freeze": "true"}})
	dr.Put(runtime.Workload{Subject: "pod:b", Name: "b",
		Resources: map[string]runtime.Resource{"cpu": {Request: f64(0.1), Limit: f64(1)}}})
	pol, err := policy.New(s.Policy)
	if err != nil {
		t.Fatal(err)
	}
	s.Node.ID = "n1"
	s.Loops.Relief.Enabled = true
	acting, _, err := buildActing(s, rt, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	var loop *relief.Loop
	var loops []*relief.Loop
	if withLoop {
		loops, err = buildRelief(s, rt, acting, pol, statemap.New(statemap.Config{Owner: "n1"}, nil), false, nil, t.Logf)
		if err != nil {
			t.Fatal(err)
		}
		loop = loops[0]
	}
	mux := http.NewServeMux()
	registerActingRoutes(mux, rt, acting, pol, loops)
	return mux, loop, dr, acting
}

func post(mux *http.ServeMux, url string, json bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, url, nil)
	if json {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func get(mux *http.ServeMux, url string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	return rec
}

func TestWorkloadsCarryTheirPolicy(t *testing.T) {
	rec := get(actingMux(t), "/workloads")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got []struct {
		Subject   string                         `json:"subject"`
		Labels    map[string]string              `json:"labels"`
		Resources map[string]map[string]*float64 `json:"resources"`
		Can       map[string]string              `json:"can"`
		Policy    struct {
			Frozen bool `json:"frozen"`
		} `json:"policy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d workloads, want 2: %s", len(got), rec.Body)
	}
	frozen := map[string]bool{}
	for _, w := range got {
		frozen[w.Subject] = w.Policy.Frozen
	}
	for _, w := range got {
		if w.Labels == nil || w.Can == nil || w.Resources == nil {
			t.Errorf("%s: labels, can and resources must be objects: %+v", w.Subject, w)
		}
		if w.Subject == "pod:a" && w.Labels["di-agent/freeze"] != "true" {
			t.Errorf("pod:a labels = %v", w.Labels)
		}
		if _, ok := w.Can["cpu.limit"]; !ok {
			t.Errorf("%s: can = %v, want cpu.limit", w.Subject, w.Can)
		}
	}
	for _, w := range got {
		switch w.Subject {
		case "pod:a":
			if w.Can["cpu.limit"] != "" {
				t.Errorf("pod:a can = %v; want cpu.limit possible: a dry run acts on a pod with no limit too", w.Can)
			}
		case "pod:b":
			if cpu := w.Resources["cpu"]; len(cpu) != 2 || *cpu["limit"] != 1 || w.Can["cpu.limit"] != "" {
				t.Errorf("pod:b resources %v can %v; want cpu {request, limit: 1}, and cpu.limit possible", w.Resources, w.Can)
			}
		}
	}
	if !frozen["pod:a"] || frozen["pod:b"] {
		t.Errorf("frozen = %v, want pod:a frozen and pod:b not", frozen)
	}

	// The settings a policy carries are snake_case like the rest of the API, not Go field names.
	var raw []struct {
		Policy struct {
			Settings map[string]any `json:"settings"`
		} `json:"policy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"allow", "deny", "floor", "max_actions_per_hour", "cooldown"} {
		if _, ok := raw[0].Policy.Settings[key]; !ok {
			t.Errorf("policy.settings has no %q: %v", key, raw[0].Policy.Settings)
		}
	}
	// A duration reads as it does in agent.yaml and /config, not as nanoseconds.
	if got := raw[0].Policy.Settings["cooldown"]; got != "10m0s" {
		t.Errorf("policy.settings.cooldown = %v (%T), want \"10m0s\"", got, got)
	}
}

func TestPolicyRouteExplainsOne(t *testing.T) {
	mux := actingMux(t)
	rec := get(mux, "/policy?subject=pod:b")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var eff policy.Effective
	if err := json.Unmarshal(rec.Body.Bytes(), &eff); err != nil {
		t.Fatal(err)
	}
	if eff.Rule != -1 || eff.Frozen {
		t.Errorf("pod:b = rule %d frozen %v, want -1 and false", eff.Rule, eff.Frozen)
	}
	if eff.Settings.Cooldown != 10*time.Minute {
		t.Errorf("cooldown read back as %s, want 10m", eff.Settings.Cooldown)
	}
	if c := get(mux, "/policy").Code; c != http.StatusBadRequest {
		t.Errorf("missing subject: %d, want 400", c)
	}
	if c := get(mux, "/policy?subject=pod:nope").Code; c != http.StatusNotFound {
		t.Errorf("unknown subject: %d, want 404", c)
	}
}

func TestReliefRoutesNeedTheLoop(t *testing.T) {
	mux := actingMux(t)
	for _, r := range []*httptest.ResponseRecorder{
		get(mux, "/relief"), get(mux, "/relief/actions"),
		post(mux, "/relief/pause", true), post(mux, "/relief/resume", true),
		post(mux, "/relief/actions/x/revert", true),
	} {
		if r.Code != http.StatusNotFound || !strings.Contains(r.Body.String(), "relief is not enabled in the spec") {
			t.Errorf("status %d body %s, want 404 naming the spec", r.Code, r.Body)
		}
	}
}

func TestPauseAndResume(t *testing.T) {
	mux, _, _, _ := actingMuxWith(t, true)
	paused := func() bool {
		var st struct {
			Levers []struct {
				Paused bool `json:"paused"`
			} `json:"levers"`
		}
		rec := get(mux, "/relief")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /relief = %d: %s", rec.Code, rec.Body)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil || len(st.Levers) != 2 {
			t.Fatalf("GET /relief = %s (%v); want both default levers", rec.Body, err)
		}
		if st.Levers[0].Paused != st.Levers[1].Paused {
			t.Fatalf("GET /relief = %s; pause and resume act on every lever", rec.Body)
		}
		return st.Levers[0].Paused
	}
	if paused() {
		t.Fatal("a new loop is paused")
	}
	if rec := post(mux, "/relief/pause", true); rec.Code != http.StatusOK || !paused() {
		t.Fatalf("pause: %d, paused=%v", rec.Code, paused())
	}
	if rec := post(mux, "/relief/resume", true); rec.Code != http.StatusOK || paused() {
		t.Fatalf("resume: %d, paused=%v", rec.Code, paused())
	}
}

func TestManualRevert(t *testing.T) {
	mux, loop, dr, acting := actingMuxWith(t, true)
	c := runtime.Workload{Subject: "pod:c", Name: "c", Resources: map[string]runtime.Resource{"cpu": {Limit: f64(0.5)}}}
	dr.Put(c)
	// A change an earlier run made and the ledger holds.
	if _, err := acting.Apply(context.Background(), c, "cpu.limit", 0.25, time.Now().Add(time.Hour), "act-1"); err != nil {
		t.Fatal(err)
	}
	if err := loop.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	var acts []relief.Action
	if err := json.Unmarshal(get(mux, "/relief/actions").Body.Bytes(), &acts); err != nil || len(acts) != 1 {
		t.Fatalf("actions = %v, err %v", acts, err)
	}
	var raw []map[string]any
	if err := json.Unmarshal(get(mux, "/relief/actions").Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw[0]["entry"].(map[string]any)["node_id"]; !ok {
		t.Errorf("entry keys = %v, want snake_case node_id", raw[0]["entry"])
	}
	if rec := post(mux, "/relief/actions/nope/revert", true); rec.Code != http.StatusNotFound {
		t.Errorf("unknown id = %d, want 404", rec.Code)
	}
	if rec := post(mux, "/relief/actions/"+acts[0].ID+"/revert", true); rec.Code != http.StatusOK {
		t.Fatalf("revert = %d: %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(get(mux, "/relief/actions").Body.Bytes(), &acts); err != nil || acts[0].Outcome != relief.OutcomeManual {
		t.Fatalf("outcome = %q, want manual (%v)", acts[0].Outcome, err)
	}
	if held := acting.Entries("cpu.limit"); len(held) != 0 {
		t.Errorf("ledger %+v; want the change given back and forgotten", held)
	}
}

func TestMutatingReliefRoutesNeedJSON(t *testing.T) {
	mux, _, _, _ := actingMuxWith(t, true)
	for _, u := range []string{"/relief/pause", "/relief/resume", "/relief/actions/x/revert"} {
		// The same refusal as every other mutating route: requireJSON, 400.
		if rec := post(mux, u, false); rec.Code != http.StatusBadRequest {
			t.Errorf("POST %s without a JSON content type = %d, want 400", u, rec.Code)
		}
	}
}

func TestOnlyCPUConsumersAreWorkloads(t *testing.T) {
	m := statemap.New(statemap.Config{Owner: "n1", AdmitUnknown: true}, nil)
	unit := [2]float64{0, 1}
	for _, o := range []statemap.Observation{
		{ID: "cpu_utilization@pod:a", Subject: "pod:a", Value: 0.2},
		{ID: "cpu.cpu.user@cpu:cpu0", Subject: "cpu:cpu0", Value: 0.3},
		{ID: "disk.io.reads@disk:sda", Subject: "disk:sda", Value: 0.1},
	} {
		o.At, o.Range = time.Now(), &unit
		if err := m.Record(o); err != nil {
			t.Fatal(err)
		}
	}
	got := workloadSubjects(m)()
	if len(got) != 1 || got[0] != "pod:a" {
		t.Errorf("workload subjects = %v, want [pod:a]", got)
	}
}
