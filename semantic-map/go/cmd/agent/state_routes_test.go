package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/profiles"
	"github.com/DiyazY/di-agent/pkg/statemap"
)

// stateFixture builds a state model seeded from the committed domain specification
// and serves it, so these tests exercise the surface an operator actually calls
// rather than the package API underneath it.
func stateFixture(t *testing.T) (*statemap.Map, *httptest.Server) {
	t.Helper()
	spec := mustSpec(t)
	sm := statemap.New(statemap.Config{
		ConvergenceObservations: 4,
		AdmitUnknown:            true,
	}, statemap.NewJournal(0))
	// Seeded without a calibration file: relationships take the neutral placeholder,
	// which is what these tests are about. The calibrated path is covered in
	// pkg/profiles, where the priors live.
	if _, err := profiles.SeedStateMap(sm, spec, "", ""); err != nil {
		t.Fatalf("seed: %v", err)
	}
	mux := http.NewServeMux()
	registerStateRoutes(mux, sm)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return sm, srv
}

// getState issues a GET and decodes a success body, returning the status so a test
// can assert on it. Named apart from routes_test.go's getJSON, which has a
// different contract.
func getState(t *testing.T, url string, into any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if into != nil && resp.StatusCode < 300 {
		if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
			t.Fatalf("decoding %s: %v", url, err)
		}
	}
	return resp.StatusCode
}

// TestSeedingBuildsTwoLayersFromOneSpec pins the shape the seed produces: the
// metrics a collector reports become observed properties, and each construct
// becomes a derived property summarising the metrics routed to it. Without the
// second layer the framework's vocabulary would be a parallel structure instead of
// a summary over real observations.
func TestSeedingBuildsTwoLayersFromOneSpec(t *testing.T) {
	sm, srv := stateFixture(t)

	var view statemap.StateView
	if code := getState(t, srv.URL+"/state", &view); code != 200 {
		t.Fatalf("GET /state returned %d", code)
	}
	if view.Counts.Observed == 0 || view.Counts.Derived == 0 {
		t.Fatalf("seeded model has %d observed and %d derived properties; the spec's "+
			"metrics and constructs should produce both",
			view.Counts.Observed, view.Counts.Derived)
	}

	// Every derived property must summarise properties that exist, or it is a
	// summary of nothing.
	for _, p := range view.Properties {
		if p.Kind != statemap.Derived {
			continue
		}
		if len(p.Members) == 0 {
			t.Errorf("derived property %s has no members", p.ID)
		}
		for _, m := range p.Members {
			if _, ok := sm.Property(m); !ok {
				t.Errorf("derived property %s summarises %s, which is not in the map", p.ID, m)
			}
		}
	}

	// A freshly seeded model must not claim to know any value.
	for _, p := range view.Properties {
		if p.NObservations != 0 || p.Confidence != 0 {
			t.Errorf("%s reports %d observations at confidence %.3f before any telemetry",
				p.ID, p.NObservations, p.Confidence)
		}
	}
	if view.Counts.Seeded == 0 {
		t.Error("no seeded relationships: prior knowledge about how constructs relate " +
			"should be present before the system is observed")
	}
}

// TestObservationFlowsToDerivedPropertyOverHTTP is the update path end to end: a
// metric arrives, its property moves, and the summary that includes it moves too.
func TestObservationFlowsToDerivedPropertyOverHTTP(t *testing.T) {
	sm, srv := stateFixture(t)

	if err := sm.Observe("cpu_utilization", 0.75, timeZero()); err != nil {
		t.Fatal(err)
	}

	var body struct {
		Property statemap.Property `json:"property"`
	}
	if code := getState(t, srv.URL+"/state/properties/cpu_utilization", &body); code != 200 {
		t.Fatalf("GET property returned %d", code)
	}
	if body.Property.Value != 0.75 {
		t.Errorf("observed property value %.4f, want 0.75", body.Property.Value)
	}

	// RC is the derived property that summarises cpu_utilization in the committed
	// spec; find whichever derived property lists it rather than hardcoding the name.
	var derived string
	for _, p := range sm.State(statemap.Query{Kinds: []statemap.Kind{statemap.Derived}}).Properties {
		for _, m := range p.Members {
			if m == "cpu_utilization" {
				derived = p.ID
			}
		}
	}
	if derived == "" {
		t.Skip("no derived property summarises cpu_utilization in the committed spec")
	}
	d, _ := sm.Property(derived)
	if d.NObservations == 0 {
		t.Errorf("derived property %s did not move when its member was observed", derived)
	}
}

// TestUndeclaredMetricBecomesAPropertyAndIsJournalled is the create path: the model
// follows a system that changes, and the change is discoverable afterwards.
func TestUndeclaredMetricBecomesAPropertyAndIsJournalled(t *testing.T) {
	sm, srv := stateFixture(t)

	if err := sm.Observe("gpu_temperature_c", 0.42, timeZero()); err != nil {
		t.Fatalf("an undeclared metric was rejected: %v", err)
	}
	if code := getState(t, srv.URL+"/state/properties/gpu_temperature_c", nil); code != 200 {
		t.Fatalf("GET on the admitted property returned %d", code)
	}

	var journal struct {
		Events []statemap.Event `json:"events"`
	}
	if code := getState(t, srv.URL+"/state/journal", &journal); code != 200 {
		t.Fatalf("GET /state/journal returned %d", code)
	}
	var found bool
	for _, e := range journal.Events {
		if e.Kind == statemap.EventPropertyAdmitted && e.Target == "gpu_temperature_c" {
			found = true
		}
	}
	if !found {
		t.Error("the admission is not in the journal, so nobody can find out why the " +
			"map contains a property they never declared")
	}
}

// TestRetirementRequiresAReason keeps the audit trail meaningful: a withdrawal with
// no stated basis records that something happened without recording why.
func TestRetirementRequiresAReason(t *testing.T) {
	_, srv := stateFixture(t)

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/state/properties/cpu_utilization", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("retirement without a reason returned %d, want 400", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodDelete,
		srv.URL+"/state/properties/cpu_utilization?reason=collector+removed&actor=operator:ada", nil)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("retirement with a reason returned %d", resp2.StatusCode)
	}

	// The retired property must leave the default view but stay retrievable, since a
	// decision taken before the retirement has to remain reconstructible.
	var view statemap.StateView
	getState(t, srv.URL+"/state", &view)
	for _, p := range view.Properties {
		if p.ID == "cpu_utilization" {
			t.Error("a retired property is still in the default view of the current system")
		}
	}
	if view.Counts.PropertiesRetired == 0 {
		t.Error("the census does not report the retirement, so the default view hides " +
			"the fact that something was excluded")
	}
	if code := getState(t, srv.URL+"/state/properties/cpu_utilization", nil); code != 200 {
		t.Errorf("retired property is no longer retrievable (%d); its record is what "+
			"makes an earlier decision reconstructible", code)
	}
}

// TestExplainIsReadableInATerminal covers the text form: the first question after a
// surprising decision should be answerable with curl.
func TestExplainIsReadableInATerminal(t *testing.T) {
	sm, srv := stateFixture(t)
	if err := sm.Observe("cpu_utilization", 0.5, timeZero()); err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/state/properties/cpu_utilization", nil)
	req.Header.Set("Accept", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	for _, want := range []string{"cpu_utilization", "confidence", "observed"} {
		if !strings.Contains(out, want) {
			t.Errorf("text explanation omits %q:\n%s", want, out)
		}
	}
}

// TestDecisionIsRetrievableByID is the traceability path over HTTP: an answer, then
// the state that produced it.
func TestDecisionIsRetrievableByID(t *testing.T) {
	sm, srv := stateFixture(t)
	if err := sm.Observe("cpu_utilization", 0.6, timeZero()); err != nil {
		t.Fatal(err)
	}

	b := sm.Decide("d-http-1", "how loaded is this system?")
	p, _ := b.Property("cpu_utilization")
	b.Note("read cpu_utilization=%.3f", p.Value)
	b.Commit(map[string]any{"load": p.Value})

	var got statemap.Decision
	if code := getState(t, srv.URL+"/state/decisions/d-http-1", &got); code != 200 {
		t.Fatalf("GET decision returned %d", code)
	}
	if got.Question == "" || len(got.PropertiesRead) == 0 {
		t.Errorf("decision came back without its question or inputs: %+v", got)
	}
	if got.Revision == 0 {
		t.Error("decision has no revision, so the state it read cannot be identified")
	}

	// A decision that never existed is a 404; one that was evicted would be a 410.
	// Distinguishing them matters because "dropped" and "never happened" are
	// different answers to an auditor.
	if code := getState(t, srv.URL+"/state/decisions/never-happened", nil); code != http.StatusNotFound {
		t.Errorf("unknown decision returned %d, want 404", code)
	}
}

// TestSweepIsExposed lets an operator inspecting a quiet system see the model admit
// that its properties have gone quiet, without waiting for a timer.
func TestSweepIsExposed(t *testing.T) {
	_, srv := stateFixture(t)
	resp, err := http.Post(srv.URL+"/state/sweep", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("POST /state/sweep returned %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["revision"]; !ok {
		t.Error("sweep response omits the revision")
	}
}

// timeZero is a fixed instant for observations in these tests, so a property's
// timestamps are deterministic.
func timeZero() time.Time {
	return time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
}

func TestEstimate_AssumeAndWithoutParams(t *testing.T) {
	sm, srv := stateFixture(t)
	rng := [2]float64{0, 100}
	_ = sm.Record(statemap.Observation{ID: "queue@pod:a", Value: 50, At: time.Now(), Subject: "pod:a", Range: &rng})
	_ = sm.Observe("cpu_pressure_ratio", 0.3, time.Now())
	_ = sm.DeclareRelationship(statemap.Relationship{From: "queue@pod:a", To: "cpu_pressure_ratio", Sign: 1, Label: "discovered"})
	_ = sm.AssertRelationshipStrength(statemap.RelationshipID("queue@pod:a", "cpu_pressure_ratio", "discovered"), 0.8, "op", "t")

	var res statemap.EstimateResult
	code := getState(t, srv.URL+"/state/estimate?target=cpu_pressure_ratio&assume=queue@pod:a=100", &res)
	if code != 200 || res.Hypothetical == nil {
		t.Fatalf("code=%d hypothetical=%v", code, res.Hypothetical)
	}
	if res.Hypothetical.Delta < 0.39 || res.Hypothetical.Delta > 0.41 {
		t.Errorf("delta=%.3f; want ≈0.4", res.Hypothetical.Delta)
	}
	code = getState(t, srv.URL+"/state/estimate?target=cpu_pressure_ratio&without=pod:a", &res)
	if code != 200 || len(res.Excluded) != 1 || res.Excluded[0] != "pod:a" {
		t.Errorf("without: code=%d excluded=%v", code, res.Excluded)
	}
	var d statemap.Decision
	if code := getState(t, srv.URL+"/state/decisions/"+res.DecisionID, &d); code != 200 || len(d.Excluded) != 1 {
		t.Errorf("decision replay: code=%d assumptions=%v excluded=%v", code, d.Assumptions, d.Excluded)
	}
	if _, ok := d.Assumptions["queue@pod:a"]; !ok {
		t.Errorf("decision replay lacks the floor assumption: %v", d.Assumptions)
	}
	if code := getState(t, srv.URL+"/state/estimate?target=cpu_pressure_ratio&assume=garbage", nil); code != 400 {
		t.Errorf("malformed assume returned %d, want 400", code)
	}
}

func TestEstimate_EmptyWithoutIsRejected(t *testing.T) {
	sm, srv := stateFixture(t)
	_ = sm.Observe("cpu_pressure_ratio", 0.3, time.Now())
	if code := getState(t, srv.URL+"/state/estimate?target=cpu_pressure_ratio&without=", nil); code != 400 {
		t.Errorf("empty without returned %d, want 400", code)
	}
}
