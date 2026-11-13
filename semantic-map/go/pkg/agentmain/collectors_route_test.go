package agentmain

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/collectors"
	"github.com/DiyazY/di-agent/pkg/types"
)

// fakeCollector is a member that never needs to be called: the route reports
// status, it does not collect.
type fakeCollector struct{ samples []*types.MetricSample }

func (f fakeCollector) Collect() ([]*types.MetricSample, error) { return f.samples, nil }
func (fakeCollector) SourceID() string                          { return "fake" }
func (fakeCollector) AvailableMetrics() []types.MetricType      { return nil }
func (fakeCollector) Health() error                             { return nil }

func getCollectors(t *testing.T, sup *collectors.Supervisor) map[string]any {
	t.Helper()
	mux := http.NewServeMux()
	registerCollectorsRoute(mux, sup, 10*time.Second)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/collectors", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not JSON: %v: %s", err, rec.Body)
	}
	if got["interval_ms"] != float64(10000) {
		t.Errorf("interval_ms = %v, want 10000", got["interval_ms"])
	}
	return got
}

// TestCollectorsRouteLists pins that GET /collectors reports each member under the
// field names the spec gives.
func TestCollectorsRouteLists(t *testing.T) {
	sup := collectors.NewSupervisor(collectors.Env{NodeID: "n1"}, []collectors.Member{
		{Name: "probe", Kind: "type", Type: "fake", C: fakeCollector{}, Timeout: time.Second},
	})
	got := getCollectors(t, sup)
	list, ok := got["collectors"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("collectors = %v, want one entry", got["collectors"])
	}
	m := list[0].(map[string]any)
	if m["name"] != "probe" || m["kind"] != "type" || m["state"] != "starting" {
		t.Errorf("collectors[0] = %v, want name probe, kind type, state starting", m)
	}
}

// TestCollectorsRouteNil pins that an agent with no collectors answers with an
// empty list rather than null, so a client can range over it unconditionally.
func TestCollectorsRouteNil(t *testing.T) {
	mux := http.NewServeMux()
	registerCollectorsRoute(mux, nil, 10*time.Second)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/collectors", nil))
	if !strings.Contains(rec.Body.String(), `"collectors":[]`) {
		t.Fatalf("body = %s, want \"collectors\":[]", rec.Body)
	}
	getCollectors(t, nil)
}

// TestCollectorsRouteReportsViolationKinds pins that violations are broken down by
// reason in the response.
func TestCollectorsRouteReportsViolationKinds(t *testing.T) {
	sup := collectors.NewSupervisor(collectors.Env{NodeID: "n1"}, []collectors.Member{
		{Name: "probe", Kind: "type", Type: "fake", C: fakeCollector{samples: []*types.MetricSample{nil}}, Timeout: time.Second},
	})
	if _, err := sup.Collect(); err != nil {
		t.Fatal(err)
	}
	got := getCollectors(t, sup)
	m := got["collectors"].([]any)[0].(map[string]any)
	kinds, ok := m["violation_kinds"].(map[string]any)
	if !ok || kinds["nil sample"] != float64(1) {
		t.Errorf("violation_kinds = %v, want {\"nil sample\": 1}", m["violation_kinds"])
	}
}
