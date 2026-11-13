package agentmain

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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
	return stateFixtureWith(t, statemap.NewJournal(0), false)
}

// stateFixtureWithLog is stateFixture with a journal of two and a journal log,
// so a decision is evicted from memory after two more and answered from the log.
func stateFixtureWithLog(t *testing.T) (*statemap.Map, *httptest.Server) {
	t.Helper()
	return stateFixtureWith(t, statemap.NewJournal(2), true)
}

func stateFixtureWith(t *testing.T, j *statemap.Journal, withLog bool) (*statemap.Map, *httptest.Server) {
	t.Helper()
	spec := mustSpec(t)
	sm := statemap.New(statemap.Config{
		ConvergenceObservations: 4,
		AdmitUnknown:            true,
	}, j)
	if withLog {
		if err := sm.OpenLog(t.TempDir()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { sm.CloseLog() })
	}
	// Seeded without a calibration file: relationships take the neutral placeholder,
	// which is what these tests are about. The calibrated path is covered in
	// pkg/profiles, where the priors live.
	if _, err := profiles.SeedStateMap(sm, spec, "", ""); err != nil {
		t.Fatalf("seed: %v", err)
	}
	mux := http.NewServeMux()
	registerStateRoutes(mux, sm, nil, nil)
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

// TestJournalRouteShowsRestoreFailedFirst: a map that found no usable snapshot says
// so as the first thing its journal holds.
func TestJournalRouteShowsRestoreFailedFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap.json")
	for _, p := range []string{path, path + ".prev"} {
		if err := os.WriteFile(p, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sm := statemap.New(statemap.Config{ConvergenceObservations: 4, AdmitUnknown: true}, statemap.NewJournal(0))
	if r, err := sm.Load(path); err != nil || r.Loaded {
		t.Fatalf("Load = %+v, %v; want a cold start", r, err)
	}
	mux := http.NewServeMux()
	registerStateRoutes(mux, sm, nil, nil)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	var journal struct {
		Events []statemap.Event `json:"events"`
	}
	if code := getState(t, srv.URL+"/state/journal", &journal); code != 200 {
		t.Fatalf("GET /state/journal returned %d", code)
	}
	if len(journal.Events) == 0 || journal.Events[0].Kind != "restore.failed" {
		t.Fatalf("events %v; want restore.failed first", journal.Events)
	}
}

// TestJournalEventsCarrySeqAcrossRestore: every event the journal route returns
// carries a seq, strictly increasing across a save and restore, and the body names
// the incarnation and the counter.
func TestJournalEventsCarrySeqAcrossRestore(t *testing.T) {
	sm, _ := stateFixture(t)
	for _, id := range []string{"a_metric", "b_metric", "c_metric"} {
		if err := sm.Observe(id, 0.5, timeZero()); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "snap.json")
	if err := sm.Save(path); err != nil {
		t.Fatal(err)
	}
	sm2 := statemap.New(statemap.Config{ConvergenceObservations: 4, AdmitUnknown: true}, statemap.NewJournal(0))
	if _, err := sm2.Load(path); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"d_metric", "e_metric"} {
		if err := sm2.Observe(id, 0.5, timeZero()); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	registerStateRoutes(mux, sm2, nil, nil)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	var journal struct {
		Events      []statemap.Event `json:"events"`
		Incarnation uint64           `json:"incarnation"`
		Seq         *uint64          `json:"seq"`
	}
	if code := getState(t, srv.URL+"/state/journal", &journal); code != 200 {
		t.Fatalf("GET /state/journal returned %d", code)
	}
	if journal.Incarnation != 2 {
		t.Errorf("incarnation %d; want 2", journal.Incarnation)
	}
	var prev uint64
	for i, e := range journal.Events {
		if e.Seq <= prev {
			t.Fatalf("event %d (%s %s) has seq %d after %d; want strictly increasing", i, e.Kind, e.Target, e.Seq, prev)
		}
		prev = e.Seq
	}
	if journal.Seq == nil || *journal.Seq != prev {
		t.Errorf("body seq %v; want the last event's %d", journal.Seq, prev)
	}
	if last := journal.Events[len(journal.Events)-1]; last.Target != "e_metric" {
		t.Errorf("last event targets %q; want the post-restore e_metric", last.Target)
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

	b := sm.Decide("d-http-1", "estimate", "how loaded is this system?")
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

// TestDecisionChainOverHTTP: ?chain=1 walks a decision's ancestry, and ?kind=
// filters the list.
func TestDecisionChainOverHTTP(t *testing.T) {
	sm, srv := stateFixture(t)
	sm.Decide("act", "relief.act", "cap?").Commit(nil)
	v := sm.Decide("verify", "relief.verify", "did it help?")
	v.Cite("act")
	v.Commit(nil)
	r := sm.Decide("revert", "relief.revert", "give it back?")
	r.Cite("verify")
	r.Cite("act")
	r.Commit(nil)

	var chain struct {
		Decision *statemap.Decision `json:"decision"`
		Cites    []struct {
			ID      string `json:"id"`
			Evicted bool   `json:"evicted"`
		} `json:"cites"`
		CitedBy []string `json:"cited_by"`
	}
	if code := getState(t, srv.URL+"/state/decisions/revert?chain=1", &chain); code != 200 {
		t.Fatalf("GET chain returned %d", code)
	}
	if chain.Decision == nil || chain.Decision.ID != "revert" || len(chain.Cites) != 2 ||
		chain.Cites[0].ID != "verify" || chain.Cites[1].ID != "act" || chain.CitedBy == nil || len(chain.CitedBy) != 0 {
		t.Errorf("chain %+v; want revert citing verify then act, cited by none", chain)
	}

	var list struct {
		Decisions []statemap.Decision `json:"decisions"`
	}
	if code := getState(t, srv.URL+"/state/decisions?kind=relief.verify", &list); code != 200 {
		t.Fatalf("GET decisions returned %d", code)
	}
	if len(list.Decisions) != 1 || list.Decisions[0].ID != "verify" {
		t.Errorf("relief.verify decisions %+v; want verify alone", list.Decisions)
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

func TestEstimate_ReusedIdAndNonFiniteAssumeAreRejected(t *testing.T) {
	sm, srv := stateFixture(t)
	_ = sm.Observe("cpu_pressure_ratio", 0.3, time.Now())
	if code := getState(t, srv.URL+"/state/estimate?target=cpu_pressure_ratio&id=ask-1", nil); code != 200 {
		t.Fatalf("first estimate under ask-1: %d", code)
	}
	if code := getState(t, srv.URL+"/state/estimate?target=cpu_pressure_ratio&id=ask-1", nil); code != 409 {
		t.Errorf("reused decision id returned %d, want 409", code)
	}
	if code := getState(t, srv.URL+"/state/estimate?target=cpu_pressure_ratio&assume=cpu_pressure_ratio=NaN", nil); code != 400 {
		t.Errorf("NaN assume returned %d, want 400", code)
	}
}

func TestEstimate_RequestShapeErrorsAndIdPassthrough(t *testing.T) {
	sm, srv := stateFixture(t)
	_ = sm.Observe("cpu_pressure_ratio", 0.3, time.Now())
	if code := getState(t, srv.URL+"/state/estimate", nil); code != 400 {
		t.Errorf("missing target returned %d, want 400", code)
	}
	if code := getState(t, srv.URL+"/state/estimate?target=cpu_pressure_ratio&assume=cpu_pressure_ratio=abc", nil); code != 400 {
		t.Errorf("non-numeric assume returned %d, want 400", code)
	}
	resp, err := http.Get(srv.URL + "/state/estimate?target=nothing_here")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("unknown target returned %d, want 404", resp.StatusCode)
	}
	if id, _ := body["decision_id"].(string); id == "" {
		t.Errorf("unknown target's 404 body lacks the decision id that was still recorded: %v", body)
	}
	var res statemap.EstimateResult
	if code := getState(t, srv.URL+"/state/estimate?target=cpu_pressure_ratio&id=ask-9", &res); code != 200 || res.DecisionID != "ask-9" {
		t.Errorf("id passthrough: code=%d decision_id=%q, want 200 and ask-9", code, res.DecisionID)
	}
	if code := getState(t, srv.URL+"/state/decisions/ask-9", nil); code != 200 {
		t.Errorf("the decision recorded under the caller's id is not retrievable: %d", code)
	}
}

// TestState_SubjectFilter: GET /state?subject= narrows the view to one subject's
// properties, while the census still reports the whole map. The data-model filter
// (Query.Subject) existed but the handler did not read the parameter.
func TestState_SubjectFilter(t *testing.T) {
	sm, srv := stateFixture(t)
	now := time.Now()
	_ = sm.Record(statemap.Observation{ID: "cpu@pod:a", Value: 0.4, At: now, Subject: "pod:a"})
	_ = sm.Record(statemap.Observation{ID: "cpu@pod:b", Value: 0.5, At: now, Subject: "pod:b"})

	var v statemap.StateView
	if code := getState(t, srv.URL+"/state?subject=pod:a", &v); code != 200 {
		t.Fatalf("code=%d", code)
	}
	var sawA bool
	for _, p := range v.Properties {
		if p.Subject != "pod:a" {
			t.Errorf("property %s (subject %q) leaked through subject=pod:a; want only pod:a", p.ID, p.Subject)
		}
		if p.ID == "cpu@pod:a" {
			sawA = true
		}
	}
	if !sawA {
		t.Error("pod:a's property is missing from the filtered view")
	}
	if v.Counts.Subjects != 2 {
		t.Errorf("census subjects=%d; want 2 — the census is the whole map, not the filter", v.Counts.Subjects)
	}
}

// TestState_UnknownSubjectIsEmptyNotAnError: a subject that names nothing is a valid
// query that matches nothing — an empty selection with HTTP 200, and a census that
// still reports the whole map. A regression that 404s an unknown subject, or that
// counts the census after filtering, is what this guards.
func TestState_UnknownSubjectIsEmptyNotAnError(t *testing.T) {
	sm, srv := stateFixture(t)
	now := time.Now()
	_ = sm.Record(statemap.Observation{ID: "cpu@pod:a", Value: 0.4, At: now, Subject: "pod:a"})
	_ = sm.Record(statemap.Observation{ID: "cpu@pod:b", Value: 0.5, At: now, Subject: "pod:b"})

	var v statemap.StateView
	if code := getState(t, srv.URL+"/state?subject=pod:zzz", &v); code != 200 {
		t.Fatalf("unknown subject returned %d; want 200 — a filter that matches nothing is not an error", code)
	}
	if len(v.Properties) != 0 {
		t.Errorf("unknown subject returned %d properties; want an empty selection: %+v", len(v.Properties), v.Properties)
	}
	if v.Counts.Subjects != 2 {
		t.Errorf("census subjects=%d; want 2 — the census is the whole map, not the filtered (empty) selection", v.Counts.Subjects)
	}
}

// TestState_StatusAndKindFilters: status and kind take a closed set of values,
// repeated or comma-separated. A value outside the set is a 400 that names the
// allowed values; matched against nothing, it would return an empty selection that
// reads as "there are none".
func TestState_StatusAndKindFilters(t *testing.T) {
	sm, srv := stateFixture(t)
	now := time.Now()
	_ = sm.Record(statemap.Observation{ID: "cpu@pod:a", Value: 0.4, At: now, Subject: "pod:a"})
	_ = sm.Record(statemap.Observation{ID: "cpu@pod:b", Value: 0.5, At: now, Subject: "pod:b"})
	if err := sm.RetireProperty("cpu@pod:b", "test", "test"); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		query string
		want  int
	}{
		{"status=active", 1},
		{"status=active,retired", 2},
		{"status=active&status=retired", 2},
		{"status=active,%20retired", 2},
		{"kind=observed,derived&status=retired", 1},
	} {
		var v statemap.StateView
		if code := getState(t, srv.URL+"/state?"+tc.query, &v); code != 200 {
			t.Errorf("%s: code=%d, want 200", tc.query, code)
			continue
		}
		pods := 0 // the fixture's spec also declares node-level properties
		for _, p := range v.Properties {
			if p.Subject != "" {
				pods++
			}
		}
		if pods != tc.want {
			t.Errorf("%s: %d pod properties, want %d", tc.query, pods, tc.want)
		}
	}

	for _, query := range []string{"status=bogus", "status=active,retird", "kind=measured"} {
		resp, err := http.Get(srv.URL + "/state?" + query)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: code=%d, want 400 rather than an empty selection", query, resp.StatusCode)
		}
		if !strings.Contains(string(b), "is not one of") {
			t.Errorf("%s: error %q does not name the allowed values", query, b)
		}
	}
}

func TestOnlyTheMapMayClaimIntervened(t *testing.T) {
	_, srv := stateFixture(t)
	for _, prov := range []string{"intervened", "learned"} {
		body := `{"from":"cpu_utilization","to":"memory_utilization","sign":1,"provenance":"` + prov + `"}`
		resp, err := http.Post(srv.URL+"/state/relationships", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("provenance %q: status %d, want 400", prov, resp.StatusCode)
		}
	}
}

func TestStateCarriesCalibration(t *testing.T) {
	sm, srv := stateFixture(t)
	sm.RecordPrediction("admission.cpu_contention_ratio", "map", 0.09, 0.07, true)
	var body map[string]any
	if code := getState(t, srv.URL+"/state", &body); code != 200 {
		t.Fatalf("status %d", code)
	}
	list, _ := body["calibration"].([]any)
	if len(list) != 1 {
		t.Fatalf("calibration %v; want one entry", body["calibration"])
	}
	e, _ := list[0].(map[string]any)
	for _, k := range []string{"kind", "basis", "n", "mean_err", "mean_abs_err", "hit_rate"} {
		if _, ok := e[k]; !ok {
			t.Errorf("entry %v lacks %q", e, k)
		}
	}
	if e["n"] != float64(1) {
		t.Errorf("n %v", e["n"])
	}
}

// driveRegime feeds correlated, then opposed pairs on from -> to until the
// relationship id is in regime.
func driveRegime(t *testing.T, sm *statemap.Map, now *time.Time, from, to, id string, limit int) {
	t.Helper()
	unit := [2]float64{0, 1}
	obs := func(prop string, v float64, ev string) {
		*now = now.Add(time.Second)
		if err := sm.Record(statemap.Observation{ID: prop, Value: v, At: *now, Range: &unit, EventID: ev}); err != nil {
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
		obs(from, x, "x"+strconv.Itoa(i))
		obs(to, y, "y"+strconv.Itoa(i))
		if r, _ := sm.Relationship(id); r.Regime {
			return
		}
	}
}

func TestEstimate_RegimeCaveatIsServed(t *testing.T) {
	sm := statemap.New(statemap.Config{AdmitUnknown: true, Alpha: 1, Learn: true,
		LearnConfig: statemap.LearnConfig{PairWindowSeconds: 15, MinSupport: 8, Window: 10}}, statemap.NewJournal(0))
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	sm.SetClock(func() time.Time { return now })
	mux := http.NewServeMux()
	registerStateRoutes(mux, sm, nil, nil)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	const from, to = "resource", "pressure"
	unit := [2]float64{0, 1}
	for _, id := range []string{from, to} {
		if err := sm.DeclareProperty(statemap.Property{ID: id, Range: unit}); err != nil {
			t.Fatal(err)
		}
	}
	if err := sm.DeclareRelationship(statemap.Relationship{From: from, To: to, Sign: 1, Label: "raises"}); err != nil {
		t.Fatal(err)
	}
	driveRegime(t, sm, &now, from, to, statemap.RelationshipID(from, to, "raises"), 400)

	var res statemap.EstimateResult
	if code := getState(t, srv.URL+"/state/estimate?target="+to, &res); code != 200 {
		t.Fatalf("estimate: %d", code)
	}
	for _, c := range res.Caveats {
		if strings.Contains(c, "present differs from established") {
			return
		}
	}
	t.Fatalf("caveats %q; want the regime caveat", res.Caveats)
}

func TestStateRouteAcceptsStatusUnobservable(t *testing.T) {
	sm, srv := stateFixture(t)
	if err := sm.Record(statemap.Observation{ID: "q_depth", Value: 1, At: time.Now(), Source: "netdata"}); err != nil {
		t.Fatal(err)
	}
	sm.SetSourceDown("netdata", true)
	sm.Sweep()

	var out struct {
		Properties []struct{ ID string }
		Counts     map[string]any
	}
	if code := getState(t, srv.URL+"/state?status=unobservable", &out); code != http.StatusOK {
		t.Fatalf("status %d; want 200", code)
	}
	if n, ok := out.Counts["properties_unobservable"].(float64); !ok || n != 1 {
		t.Errorf("counts %v; want properties_unobservable 1", out.Counts)
	}
	if len(out.Properties) != 1 || out.Properties[0].ID != "q_depth" {
		t.Errorf("properties %+v; want only q_depth", out.Properties)
	}
	var bad map[string]any
	if code := getState(t, srv.URL+"/state?status=bogus", &bad); code != http.StatusBadRequest {
		t.Errorf("status %d for ?status=bogus; want 400", code)
	}
}

func TestForgottenPropertyAnswers410(t *testing.T) {
	sm, srv := stateFixture(t)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	mono := time.Second
	sm.SetClock(func() time.Time { return now })
	sm.SetMonotonic(func() time.Duration { return mono })
	if err := sm.Record(statemap.Observation{ID: "cpu@pod:a", Value: 0.5, At: now, Subject: "pod:a"}); err != nil {
		t.Fatal(err)
	}
	if err := sm.RetireProperty("cpu@pod:a", "test", "operator"); err != nil {
		t.Fatal(err)
	}
	now, mono = now.Add(25*time.Hour), mono+25*time.Hour
	sm.Sweep()

	resp, err := http.Get(srv.URL + "/state/properties/cpu@pod:a")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("status %d, want 410", resp.StatusCode)
	}
	var body struct {
		ID        string    `json:"id"`
		RetiredAt time.Time `json:"retired_at"`
		Forgotten bool      `json:"forgotten"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.ID != "cpu@pod:a" || !body.Forgotten || body.RetiredAt.IsZero() {
		t.Errorf("body %+v", body)
	}
	if code := getState(t, srv.URL+"/state/properties/never-seen", nil); code != http.StatusNotFound {
		t.Errorf("unknown id answered %d, want 404", code)
	}
}

// TestStateCarriesThePace: GET /state says the attention level and since when, and a
// decision records the pace it was taken at.
func TestStateCarriesThePace(t *testing.T) {
	sm, srv := stateFixture(t)
	var view struct {
		Pace      string `json:"pace"`
		PaceSince string `json:"pace_since"`
	}
	if code := getState(t, srv.URL+"/state", &view); code != 200 || view.Pace != "base" {
		t.Fatalf("GET /state: code=%d pace=%q, want 200 and base", code, view.Pace)
	}
	if at, err := time.Parse(time.RFC3339, view.PaceSince); err != nil || at.IsZero() {
		t.Fatalf("pace_since %q does not parse as a non-zero RFC3339 time: %v", view.PaceSince, err)
	}
	sm.SetPace(statemap.PaceSlow, "test")
	if code := getState(t, srv.URL+"/state", &view); code != 200 || view.Pace != "slow" {
		t.Fatalf("GET /state after SetPace: code=%d pace=%q, want slow", code, view.Pace)
	}
	if err := sm.Observe("cpu_pressure_ratio", 0.3, time.Now()); err != nil {
		t.Fatal(err)
	}
	var res statemap.EstimateResult
	if code := getState(t, srv.URL+"/state/estimate?target=cpu_pressure_ratio", &res); code != 200 || res.DecisionID == "" {
		t.Fatalf("estimate: code=%d decision_id=%q", code, res.DecisionID)
	}
	var d struct {
		Inputs map[string]any `json:"inputs"`
	}
	if code := getState(t, srv.URL+"/state/decisions/"+res.DecisionID, &d); code != 200 || d.Inputs["pace"] != "slow" {
		t.Fatalf("decision %s: code=%d inputs.pace=%v, want slow", res.DecisionID, code, d.Inputs["pace"])
	}
}

// GET /state/decisions/{id} falls through to the log once the bounded journal has
// evicted a decision; 410 still means neither holds it.
func TestDecisionFallsThroughToTheLogOverHTTP(t *testing.T) {
	sm, srv := stateFixtureWithLog(t)
	var ids []string
	for i := 0; i < 3; i++ {
		id := sm.NewDecisionID("est")
		sm.Decide(id, "estimate", "q").Commit(map[string]any{"i": i})
		ids = append(ids, id)
	}
	var first map[string]any
	if code := getState(t, srv.URL+"/state/decisions/"+ids[0], &first); code != http.StatusOK {
		t.Fatalf("evicted decision: status %d", code)
	}
	if first["from"] != "log" || first["id"] != ids[0] {
		t.Fatalf("evicted decision body: from %v, id %v", first["from"], first["id"])
	}
	var third map[string]any
	if code := getState(t, srv.URL+"/state/decisions/"+ids[2], &third); code != http.StatusOK {
		t.Fatalf("held decision: status %d", code)
	}
	if _, has := third["from"]; has || third["id"] != ids[2] {
		t.Fatalf("held decision body: %v", third)
	}
	var chain map[string]any
	if code := getState(t, srv.URL+"/state/decisions/"+ids[0]+"?chain=1", &chain); code != http.StatusOK {
		t.Fatalf("evicted chain: status %d", code)
	}
	if d, _ := chain["decision"].(map[string]any); chain["from"] != "log" || d["id"] != ids[0] {
		t.Fatalf("evicted chain body: %v", chain)
	}
	if code := getState(t, srv.URL+"/state/decisions/never", nil); code != http.StatusGone {
		t.Fatalf("unknown id: status %d, want 410", code)
	}
	var journal struct {
		Log *statemap.LogStats `json:"log"`
	}
	if code := getState(t, srv.URL+"/state/journal", &journal); code != http.StatusOK {
		t.Fatalf("journal: status %d", code)
	}
	if journal.Log == nil || journal.Log.Indexed != 3 || !strings.HasSuffix(journal.Log.File, "journal.ndjson") {
		t.Fatalf("journal log = %+v", journal.Log)
	}

	// Without a log the journal route carries no "log" key.
	_, plain := stateFixture(t)
	var body map[string]any
	getState(t, plain.URL+"/state/journal", &body)
	if _, has := body["log"]; has {
		t.Fatal("journal without a log reports one")
	}
}

func TestEstimateReportsBoundsOverHTTP(t *testing.T) {
	sm, srv := stateFixture(t)
	rng := [2]float64{0, 100}
	_ = sm.Record(statemap.Observation{ID: "queue@pod:a", Value: 50, At: time.Now(), Subject: "pod:a", Range: &rng})
	_ = sm.Observe("cpu_pressure_ratio", 0.3, time.Now())
	_ = sm.DeclareRelationship(statemap.Relationship{From: "queue@pod:a", To: "cpu_pressure_ratio", Sign: 1, Label: "discovered"})
	_ = sm.AssertRelationshipStrength(statemap.RelationshipID("queue@pod:a", "cpu_pressure_ratio", "discovered"), 0.8, "op", "t")

	var body struct {
		Answer     map[string]any   `json:"answer"`
		Influences []map[string]any `json:"influences"`
	}
	if code := getState(t, srv.URL+"/state/estimate?target=cpu_pressure_ratio", &body); code != 200 {
		t.Fatalf("code %d", code)
	}
	for _, k := range []string{"sensitivity_low", "sensitivity_high"} {
		if _, ok := body.Answer[k]; !ok {
			t.Errorf("answer lacks %s: %v", k, body.Answer)
		}
	}
	if len(body.Influences) == 0 {
		t.Fatal("no influences")
	}
	for _, inf := range body.Influences {
		for _, k := range []string{"contribution_low", "contribution_high"} {
			if _, ok := inf[k]; !ok {
				t.Errorf("influence lacks %s: %v", k, inf)
			}
		}
	}
}
