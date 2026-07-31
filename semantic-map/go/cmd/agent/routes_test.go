package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/explain"
	"github.com/DiyazY/di-agent/pkg/profiles"
	"github.com/DiyazY/di-agent/pkg/semmap"
	"github.com/DiyazY/di-agent/pkg/statemap"
	"github.com/DiyazY/di-agent/pkg/types"
)

// newTestAgent wires a SemanticMap via the edge-minimal profile and returns
// the base URL of an httptest server bound to that map. Tests use it to
// avoid duplicating the agent boot sequence.
func newTestAgent(t *testing.T) (baseURL string, sm *semmap.SemanticMap, cleanup func()) {
	t.Helper()
	sm, _, err := profiles.Build("edge-minimal", profiles.Config{
		StateMap:             newTestState(t),
		DomainSpec:           mustSpec(t),
		EMAAlpha:             0.2,
		ConvergenceThreshold: 500,
		MinTrustScore:        0.5,
	})
	if err != nil {
		t.Fatalf("profiles.Build: %v", err)
	}
	mux := http.NewServeMux()
	registerRoutes(mux, sm, explain.NewDisabled())
	srv := httptest.NewServer(mux)
	return srv.URL, sm, srv.Close
}

// getJSON is a tiny helper that does a GET, asserts 200, and decodes JSON.
func getJSON(t *testing.T, url string, out any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s: status=%d body=%s", url, resp.StatusCode, string(body))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

func TestGetGraph_ReturnsSevenConstructsAndFifteenPropositions(t *testing.T) {
	base, _, cleanup := newTestAgent(t)
	defer cleanup()
	var snap GraphSnapshot
	getJSON(t, base+"/graph", &snap)
	if want := len(mustSpec(t).Constructs); len(snap.Constructs) != want {
		t.Errorf("constructs: got %d, want %d", len(snap.Constructs), want)
	}
	if want := len(mustSpec(t).Propositions); len(snap.Propositions) != want {
		t.Errorf("propositions: got %d, want %d", len(snap.Propositions), want)
	}
	if want := len(mustSpec(t).Propositions); len(snap.Edges) != want {
		t.Errorf("edges: got %d, want %d", len(snap.Edges), want)
	}
}

func TestGetGraph_EncodesDirectionAsPlusMinus(t *testing.T) {
	base, _, cleanup := newTestAgent(t)
	defer cleanup()
	var snap GraphSnapshot
	getJSON(t, base+"/graph", &snap)
	for _, p := range snap.Propositions {
		if p.Direction != "+" && p.Direction != "-" {
			t.Errorf("proposition %s: direction=%q, want + or -", p.PropositionID, p.Direction)
		}
	}
	for _, e := range snap.Edges {
		if e.Direction != "+" && e.Direction != "-" {
			t.Errorf("edge %s: direction=%q, want + or -", e.PropositionID, e.Direction)
		}
	}
}

func TestGetEdges_NoFilterReturnsFifteen(t *testing.T) {
	base, _, cleanup := newTestAgent(t)
	defer cleanup()
	var edges []EdgeDTO
	getJSON(t, base+"/edges", &edges)
	if want := len(mustSpec(t).Propositions); len(edges) != want {
		t.Errorf("/edges: got %d, want %d", len(edges), want)
	}
}

func TestGetEdges_FilterByFromTo_RC_PS_ReturnsBothP2AndP3(t *testing.T) {
	base, _, cleanup := newTestAgent(t)
	defer cleanup()
	var edges []EdgeDTO
	getJSON(t, base+"/edges?from=RC&to=PS", &edges)
	if len(edges) != 2 {
		t.Fatalf("RC→PS pair: got %d edges, want 2 (P2 and P3)", len(edges))
	}
	seen := map[string]bool{}
	for _, e := range edges {
		seen[e.PropositionID] = true
	}
	if !seen["P2"] || !seen["P3"] {
		t.Errorf("expected P2 and P3 in RC→PS pair; got %v", seen)
	}
}

func TestGetHistory_RespectsRFC3339Since(t *testing.T) {
	base, sm, cleanup := newTestAgent(t)
	defer cleanup()
	// Future timestamp should return zero events.
	future := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	var events []OntologyEventDTO
	getJSON(t, base+"/history?since="+future, &events)
	if len(events) != 0 {
		t.Errorf("future since: got %d events, want 0", len(events))
	}
	// Trigger a mutation, then query with zero time — must include it.
	if err := sm.SetPropositionStrength(firstProp(t), 0.77); err != nil {
		t.Fatal(err)
	}
	getJSON(t, base+"/history", &events)
	if len(events) != 1 {
		t.Errorf("after one mutation: got %d events, want 1", len(events))
	}
}

func TestGetHistory_RespectsDurationSince(t *testing.T) {
	base, sm, cleanup := newTestAgent(t)
	defer cleanup()
	if err := sm.SetPropositionStrength(firstProp(t), 0.5); err != nil {
		t.Fatal(err)
	}
	var events []OntologyEventDTO
	getJSON(t, base+"/history?since=1h", &events)
	if len(events) != 1 {
		t.Errorf("since=1h: got %d events, want 1", len(events))
	}
}

func TestHealthz_OK(t *testing.T) {
	base, _, cleanup := newTestAgent(t)
	defer cleanup()
	var h HealthResponse
	getJSON(t, base+"/healthz", &h)
	if !h.OK {
		t.Error("/healthz returned ok=false")
	}
}

func TestVersion_ReturnsStructWithCounts(t *testing.T) {
	base, _, cleanup := newTestAgent(t)
	defer cleanup()
	var v VersionResponse
	getJSON(t, base+"/version", &v)
	if v.AgentVersion == "" {
		t.Error("agent_version empty")
	}
	if v.GoVersion == "" {
		t.Error("go_version empty")
	}
	// Counts come from the loaded domain spec rather than from literals, so a
	// change to the graph's scope surfaces as a spec change rather than as a
	// test failure that has to be hand-reconciled.
	spec := mustSpec(t)
	if v.SemmapConstructs != len(spec.Constructs) {
		t.Errorf("semmap_constructs: got %d, want %d", v.SemmapConstructs, len(spec.Constructs))
	}
	if v.SemmapPropositions != len(spec.Propositions) {
		t.Errorf("semmap_propositions: got %d, want %d", v.SemmapPropositions, len(spec.Propositions))
	}
}

func TestGetNeighbors_ReturnsTargetConstructs(t *testing.T) {
	base, _, cleanup := newTestAgent(t)
	defer cleanup()
	// Derive the expectation from the spec: a construct's neighbours are the
	// targets of every proposition it sources.
	spec := mustSpec(t)
	source := spec.Constructs[0].ConstructID
	want := map[string]bool{}
	for _, p := range spec.Propositions {
		if p.FromConstruct == source {
			want[p.ToConstruct] = true
		}
	}
	if len(want) == 0 {
		t.Skipf("construct %s sources no propositions in the spec", source)
	}

	var neighbors []string
	getJSON(t, base+"/neighbors?node="+source, &neighbors)
	got := map[string]bool{}
	for _, n := range neighbors {
		got[n] = true
	}
	for w := range want {
		if !got[w] {
			t.Errorf("%s neighbors missing %s; got %v", source, w, neighbors)
		}
	}
}

// ── shared post helper (used by mutation tests added in step 1.7) ────────────

// postJSON does a POST with Content-Type: application/json and returns the
// response. Tests assert status and decode the body themselves.
func postJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

// postRaw posts a raw body without setting Content-Type. Used to exercise
// requireJSON's CSRF guard.
func postRaw(t *testing.T, url, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

// Pretty-print helper used in failure messages.
func body(resp *http.Response) string {
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return fmt.Sprintf("status=%d body=%s", resp.StatusCode, string(b))
}

// ── Mutation tests ───────────────────────────────────────────────────────────

func TestSetStrength_UpdatesAndAppearsInHistory(t *testing.T) {
	base, _, cleanup := newTestAgent(t)
	defer cleanup()
	resp := postJSON(t, base+"/ontology/strength", SetStrengthRequest{
		PropositionID: firstProp(t),
		Strength:      0.95,
	})
	if resp.StatusCode != 204 {
		t.Fatalf("set strength: %s", body(resp))
	}
	resp.Body.Close()

	var props []PropositionDTO
	getJSON(t, base+"/propositions", &props)
	for _, p := range props {
		if p.PropositionID == firstProp(t) {
			if p.PriorStrength != 0.95 {
				t.Errorf("P1 strength after set: got %.3f, want 0.95", p.PriorStrength)
			}
		}
	}

	var events []OntologyEventDTO
	getJSON(t, base+"/history", &events)
	if len(events) != 1 {
		t.Fatalf("history: got %d events, want 1", len(events))
	}
	if events[0].Kind != "proposition_strength_set" {
		t.Errorf("event kind: got %q, want proposition_strength_set", events[0].Kind)
	}
	if events[0].TargetID != firstProp(t) {
		t.Errorf("event target: got %q, want P1", events[0].TargetID)
	}
}

func TestDeprecate_FlagsPropositionAndReasonerSkipsIt(t *testing.T) {
	base, sm, cleanup := newTestAgent(t)
	defer cleanup()

	before, err := sm.CostOfAction("pod-scheduling", "node_1")
	if err != nil {
		t.Fatal(err)
	}

	resp := postJSON(t, base+"/ontology/deprecate", DeprecateRequest{
		PropositionID: firstProp(t),
		Reason:        "http test",
	})
	if resp.StatusCode != 204 {
		t.Fatalf("deprecate: %s", body(resp))
	}
	resp.Body.Close()

	var props []PropositionDTO
	getJSON(t, base+"/propositions", &props)
	var p1 *PropositionDTO
	for i := range props {
		if props[i].PropositionID == firstProp(t) {
			p1 = &props[i]
		}
	}
	if p1 == nil {
		t.Fatal("P1 disappeared from /propositions after deprecate (must be soft-delete)")
	}
	if !p1.Deprecated {
		t.Error("P1 not flagged deprecated after POST /ontology/deprecate")
	}
	if p1.DeprecatedReason != "http test" {
		t.Errorf("P1 deprecated_reason: got %q, want \"http test\"", p1.DeprecatedReason)
	}

	after, err := sm.CostOfAction("pod-scheduling", "node_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(after.GraphPathUsed) != len(before.GraphPathUsed)-1 {
		t.Errorf("graph path should shrink by 1 after deprecate; got %d → %d",
			len(before.GraphPathUsed), len(after.GraphPathUsed))
	}
}

func TestAddConstruct_AppearsInConstructs(t *testing.T) {
	base, _, cleanup := newTestAgent(t)
	defer cleanup()
	resp := postJSON(t, base+"/ontology/construct", AddConstructRequest{
		ConstructID: "PR",
		Name:        "Privacy & Data Sovereignty",
		Description: "added by http test",
	})
	if resp.StatusCode != 204 {
		t.Fatalf("add construct: %s", body(resp))
	}
	resp.Body.Close()

	var constructs []ConstructDTO
	getJSON(t, base+"/constructs", &constructs)
	found := false
	for _, c := range constructs {
		if c.ConstructID == "PR" {
			found = true
			if c.Name != "Privacy & Data Sovereignty" {
				t.Errorf("PR name: got %q", c.Name)
			}
		}
	}
	if !found {
		t.Error("PR construct not found after POST /ontology/construct")
	}
}

func TestAddValidatedProposition_AppearsInPropositions(t *testing.T) {
	base, _, cleanup := newTestAgent(t)
	defer cleanup()
	// PS→SC is not in the Di-Select bootstrap; safe to add.
	resp := postJSON(t, base+"/ontology/proposition", AddPropositionRequest{
		PropositionID: "P-http",
		From:          "PS",
		To:            "SC",
		Direction:     "+",
		PriorStrength: 0.42,
	})
	if resp.StatusCode != 204 {
		t.Fatalf("add proposition: %s", body(resp))
	}
	resp.Body.Close()

	var props []PropositionDTO
	getJSON(t, base+"/propositions", &props)
	for _, p := range props {
		if p.PropositionID == "P-http" {
			if p.FromConstruct != "PS" || p.ToConstruct != "SC" {
				t.Errorf("P-http endpoints: from=%s to=%s", p.FromConstruct, p.ToConstruct)
			}
			if p.Direction != "+" {
				t.Errorf("P-http direction: got %q, want +", p.Direction)
			}
			return
		}
	}
	t.Error("P-http proposition not found after POST /ontology/proposition")
}

func TestResetEdge_RestoresPriorAfterUpdates(t *testing.T) {
	base, sm, cleanup := newTestAgent(t)
	defer cleanup()

	// Stream a few observations into PS→RC to move EMA off prior.
	for i := 0; i < 50; i++ {
		if err := sm.Ingest("PS", "RC", 0.9, fmt.Sprintf("evt-%d", i)); err != nil {
			t.Fatal(err)
		}
	}

	// Reset via HTTP.
	resp := postJSON(t, base+"/agent/reset", ResetRequest{From: "PS", To: "RC"})
	if resp.StatusCode != 204 {
		t.Fatalf("reset: %s", body(resp))
	}
	resp.Body.Close()

	var edges []EdgeDTO
	getJSON(t, base+"/edges?from=PS&to=RC", &edges)
	if len(edges) == 0 {
		t.Fatal("no PS→RC edges returned after reset")
	}
	for _, e := range edges {
		if e.EMAWeight != e.PriorWeight {
			t.Errorf("edge %s: EMA=%.3f != prior=%.3f after reset", e.PropositionID, e.EMAWeight, e.PriorWeight)
		}
		if e.NObservations != 0 {
			t.Errorf("edge %s: n_observations=%d after reset; want 0", e.PropositionID, e.NObservations)
		}
		if e.Confidence != 0.0 {
			t.Errorf("edge %s: confidence=%.3f after reset; want 0.0", e.PropositionID, e.Confidence)
		}
	}
}

func TestPostWithoutJSONContentType_Returns400(t *testing.T) {
	base, _, cleanup := newTestAgent(t)
	defer cleanup()
	resp := postRaw(t, base+"/ontology/strength", `{"proposition_id":firstProp(t),"strength":0.5}`)
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("missing Content-Type: got %d, want 400", resp.StatusCode)
	}
	var er ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if !strings.Contains(er.Error, "application/json") {
		t.Errorf("error message %q should mention application/json", er.Error)
	}
}

func TestStaticUI_ServesAppJS(t *testing.T) {
	// Phase 2B replaced the placeholder with the real index.html / app.js /
	// style.css triple. We assert the embed.FS → /ui/ chain end-to-end by
	// fetching app.js, which (unlike index.html) is not subject to Go's
	// FileServer canonicalization redirect (/index.html → ./), so it is the
	// simplest probe for "file is embedded and served verbatim".
	base, _, cleanup := newTestAgent(t)
	defer cleanup()
	resp, err := http.Get(base + "/ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("app.js: status %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	if !strings.Contains(body, "Semantic-Map embedded UI controller") {
		t.Errorf("app.js should contain the module banner; got %q", body[:min(len(body), 200)])
	}
}

func TestStaticUI_ServesIndexHTML(t *testing.T) {
	// Validate that index.html is embedded and at least one of:
	//   (a) returned as 200 with the page body, or
	//   (b) 301-redirected to ./ by Go's FileServer canonicalization.
	// Both responses confirm the file is present in the embed.FS; option (b)
	// is the stdlib's intentional behavior for any URL ending in /index.html
	// and is independent of our handler wiring.
	base, _, cleanup := newTestAgent(t)
	defer cleanup()
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(base + "/ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 200:
		b, _ := io.ReadAll(resp.Body)
		body := string(b)
		if !strings.Contains(body, "<title>Semantic Map") {
			t.Errorf("index.html body missing title; got %q", body[:min(len(body), 200)])
		}
	case 301:
		if loc := resp.Header.Get("Location"); loc != "./" {
			t.Errorf("expected canonicalization redirect to ./, got %q", loc)
		}
	default:
		t.Errorf("/ui/index.html: status %d, want 200 or 301", resp.StatusCode)
	}
}

func TestStaticUI_RootServesIndex(t *testing.T) {
	// http.FileServer serves the directory's index.html for "/" requests
	// directly (200 with body), without a redirect. We do NOT install an
	// explicit /ui/{$} → /ui/index.html redirect because that would loop
	// with the stdlib's /index.html → ./ canonicalization.
	base, _, cleanup := newTestAgent(t)
	defer cleanup()
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(base + "/ui/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("/ui/: status %d, want 200", resp.StatusCode)
	}
}

// ── /ingest-sample (Bridge-routed telemetry) ──────────────────────────────────

func TestIngestSample_AppliesBridgeRouting(t *testing.T) {
	// A cpu_utilization sample routes to RC via the Bridge, which fans out to
	// every edge touching RC (P1: SC→RC, P2: RC→PS, P3: RC→PS, P8: MU→RC,
	// P10: PS→RC, P14: RC→SC). After one POST, all of those edges must show
	// n_observations >= 1 — we assert on the SC→RC pair for headline proof
	// and then verify the RC-side fan-out via /edges.
	base, _, cleanup := newTestAgent(t)
	defer cleanup()

	resp := postJSON(t, base+"/ingest-sample", MetricSampleRequest{
		NodeID:        "master",
		MetricType:    "cpu_utilization",
		Value:         0.7,
		TimestampUnix: time.Now().Unix(),
		EventID:       "ingest-sample-test-1",
	})
	if resp.StatusCode != 204 {
		t.Fatalf("POST /ingest-sample: %s", body(resp))
	}
	resp.Body.Close()

	// At least one edge touching RC must register the observation. Edges
	// before SC→RC (P1) are the most direct proof: a Bridge that ignored the
	// sample would leave them at n=0.
	var edges []EdgeDTO
	getJSON(t, base+"/edges?from="+pairFrom(t)+"&to="+pairTo(t), &edges)
	if len(edges) == 0 {
		t.Fatalf("%s→%s returned no edges", pairFrom(t), pairTo(t))
	}
	for _, e := range edges {
		if e.NObservations < 1 {
			t.Errorf("edge %s (SC→RC) n_observations=%d after one sample; want >=1",
				e.PropositionID, e.NObservations)
		}
	}
}

// TestIngestSample_UnroutedMetricIsRecordedNotRejected pins the contract that
// replaced a 400. An unrouted metric type is something the system reported: the
// state model records it as a property, because a map that can only represent what
// someone declared in advance describes the system as it was when they wrote it
// down. The response is 202 rather than 204 so a typo is still visible — the caller
// learns the reading was kept but nothing summarises it.
func TestIngestSample_UnroutedMetricIsRecordedNotRejected(t *testing.T) {
	base, _, cleanup := newTestAgent(t)
	defer cleanup()
	resp := postJSON(t, base+"/ingest-sample", MetricSampleRequest{
		NodeID:        "master",
		MetricType:    "bogus_metric",
		Value:         1.0,
		TimestampUnix: time.Now().Unix(),
		EventID:       "ingest-sample-unknown",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("unrouted metric_type: got %d, want 202 (%s)", resp.StatusCode, body(resp))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type: got %q, want application/json", ct)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got["routed"] != false {
		t.Errorf("body should report routed=false, got %v", got["routed"])
	}
	if got["metric_type"] != "bogus_metric" {
		t.Errorf("body should name the metric type, got %v", got["metric_type"])
	}
}

func TestIngestSample_RequiresJSON(t *testing.T) {
	base, _, cleanup := newTestAgent(t)
	defer cleanup()
	resp := postRaw(t, base+"/ingest-sample",
		`{"node_id":"master","metric_type":"cpu_utilization","value":0.5,"timestamp_unix":1,"event_id":"x"}`)
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("missing Content-Type: got %d, want 400", resp.StatusCode)
	}
	var er ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if !strings.Contains(er.Error, "application/json") {
		t.Errorf("error %q should mention application/json", er.Error)
	}
}

func TestSetStrength_UnknownProposition_Returns500WithErrorJSON(t *testing.T) {
	base, _, cleanup := newTestAgent(t)
	defer cleanup()
	resp := postJSON(t, base+"/ontology/strength", SetStrengthRequest{
		PropositionID: "P-nonexistent",
		Strength:      0.5,
	})
	defer resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Errorf("unknown prop: got %d, want 500", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("error Content-Type: got %q, want application/json", ct)
	}
	var er ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if er.Error == "" {
		t.Error("error body empty")
	}
}

// ── /peers and /offload (multi-agent coordination) ───────────────────────────

func TestPeers_AddListRemove(t *testing.T) {
	base, _, cleanup := newTestAgent(t)
	defer cleanup()

	// Initially empty.
	var list []PeerDTO
	getJSON(t, base+"/peers", &list)
	if len(list) != 0 {
		t.Errorf("initial /peers: got %d entries, want 0", len(list))
	}

	// Add one peer.
	resp := postJSON(t, base+"/peers", AddPeerRequest{URL: "http://node_1:8080", Note: "rpi-1"})
	if resp.StatusCode != 200 {
		t.Fatalf("POST /peers: %s", body(resp))
	}
	var added PeerDTO
	if err := json.NewDecoder(resp.Body).Decode(&added); err != nil {
		t.Fatalf("decode added: %v", err)
	}
	resp.Body.Close()
	if added.URL != "http://node_1:8080" {
		t.Errorf("added URL: got %q", added.URL)
	}
	if added.Trust != 0.5 {
		t.Errorf("added trust: got %.3f, want 0.5 default", added.Trust)
	}
	if added.Note != "rpi-1" {
		t.Errorf("added note: got %q", added.Note)
	}

	// List shows it.
	getJSON(t, base+"/peers", &list)
	if len(list) != 1 {
		t.Fatalf("after add, /peers: got %d, want 1", len(list))
	}

	// Re-adding the same URL is idempotent (same ID, no extra row).
	resp = postJSON(t, base+"/peers", AddPeerRequest{URL: "http://node_1:8080", Note: "different"})
	resp.Body.Close()
	getJSON(t, base+"/peers", &list)
	if len(list) != 1 {
		t.Errorf("after duplicate POST, /peers: got %d, want 1 (idempotent on URL)", len(list))
	}

	// Remove via DELETE.
	req, _ := http.NewRequest(http.MethodDelete, base+"/peers/"+added.ID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 204 {
		t.Errorf("DELETE /peers/{id}: got %d, want 204 (%s)", resp.StatusCode, body(resp))
	}
	resp.Body.Close()

	getJSON(t, base+"/peers", &list)
	if len(list) != 0 {
		t.Errorf("after delete, /peers: got %d, want 0", len(list))
	}
}

func TestPeers_TrustSet(t *testing.T) {
	base, _, cleanup := newTestAgent(t)
	defer cleanup()
	resp := postJSON(t, base+"/peers", AddPeerRequest{URL: "http://node_X:8080"})
	if resp.StatusCode != 200 {
		t.Fatalf("add: %s", body(resp))
	}
	var added PeerDTO
	_ = json.NewDecoder(resp.Body).Decode(&added)
	resp.Body.Close()

	// Override trust.
	resp = postJSON(t, base+"/peers/"+added.ID+"/trust", SetTrustRequest{Value: 0.85})
	if resp.StatusCode != 204 {
		t.Fatalf("trust set: %s", body(resp))
	}
	resp.Body.Close()

	var list []PeerDTO
	getJSON(t, base+"/peers", &list)
	if len(list) != 1 {
		t.Fatalf("list len: %d", len(list))
	}
	if list[0].Trust != 0.85 {
		t.Errorf("trust after override: got %.3f, want 0.85", list[0].Trust)
	}
}

func TestPeers_AddMissingURL_Returns400(t *testing.T) {
	base, _, cleanup := newTestAgent(t)
	defer cleanup()
	resp := postJSON(t, base+"/peers", AddPeerRequest{URL: ""})
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("empty URL: got %d, want 400 (%s)", resp.StatusCode, body(resp))
	}
	var er ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(er.Error), "url") {
		t.Errorf("error body should mention url: %q", er.Error)
	}
}

func TestPeers_DeleteUnknownReturns404(t *testing.T) {
	base, _, cleanup := newTestAgent(t)
	defer cleanup()
	req, _ := http.NewRequest(http.MethodDelete, base+"/peers/does-not-exist", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("DELETE unknown: got %d, want 404", resp.StatusCode)
	}
}

func TestOffload_AcceptWithinBudget(t *testing.T) {
	base, sm, cleanup := newTestAgent(t)
	defer cleanup()

	// Probe local cost to size budgets above its actual values.
	cost, err := sm.CostOfAction("pod-scheduling", "node_self")
	if err != nil {
		t.Fatal(err)
	}
	resp := postJSON(t, base+"/offload", OffloadHTTPRequest{
		TaskType:        "pod-scheduling",
		SourceNodeID:    "node_self",
		DataSizeBytes:   1024,
		LatencyBudgetMs: cost.LatencyEstimate*2 + 100, // generous
	})
	if resp.StatusCode != 200 {
		t.Fatalf("/offload: %s", body(resp))
	}
	var out OffloadHTTPResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !out.Accepted {
		t.Errorf("Accepted: got false, want true (reason=%s; latency=%.3f budget=%.3f)",
			out.Reason, out.ExpectedLatency, cost.LatencyEstimate*2+100)
	}
	if out.ExpectedLatency != cost.LatencyEstimate {
		t.Errorf("ExpectedLatency: got %.3f, want %.3f", out.ExpectedLatency, cost.LatencyEstimate)
	}
}

// drivePositiveCost biases EMAs so that both ResourceCost (sum over edges
// into RC) and LatencyEstimate (sum over edges into PS) come out > 0.
//
// Without biasing, the bootstrap propositions sum to ≤0 (positives cancel
// negatives) and both costs clamp to zero — making budget rejections
// untestable. We push the dominant positive-target edges high and the
// negative-target edges low, plus drive CO→PS into negative territory so
// its (-1)*(-effective) = +effective contribution lifts the latency
// estimate over zero. RC→PS is a conflict pair (P2−/P3+) sharing one
// (from, to); Ingest updates both EMAs identically, so the pair always
// contributes net zero regardless of bias direction — useful here, since
// it means LatencyEstimate is driven entirely by CO→PS once the third edge
// is biased.
//
// alpha/convergence are tuned in newOffloadTestAgent to make the EMA reach
// usable values within ~200 ticks.
func drivePositiveCost(t *testing.T, sm *semmap.SemanticMap) {
	t.Helper()
	// Cost is the observed level of the cost constructs, read from the state model, so
	// making it positive means giving those constructs telemetry. The previous version
	// drove edge observations, which moved the construct graph and — once cost came
	// from the state model — stopped affecting the answer at all.
	spec := mustSpec(t)
	targets := map[string]bool{
		spec.CostModel.ResourceConstruct: true,
		spec.CostModel.PressureConstruct: true,
	}
	var metrics []string
	for _, r := range spec.MetricRouting {
		if targets[r.ConstructID] {
			metrics = append(metrics, r.MetricType)
		}
	}
	if len(metrics) == 0 {
		t.Fatal("no metric routes to either cost construct")
	}
	for i := 0; i < 200; i++ {
		for _, m := range metrics {
			if err := sm.IngestSample(&types.MetricSample{
				NodeID:        "node_self",
				MetricType:    types.MetricType(m),
				Value:         0.8,
				TimestampUnix: int64(1700000000 + i),
				EventID:       fmt.Sprintf("bias-%s-%d", m, i),
			}); err != nil {
				t.Fatalf("ingesting %s: %v", m, err)
			}
		}
	}
}

func newOffloadTestAgent(t *testing.T) (baseURL string, sm *semmap.SemanticMap, cleanup func()) {
	t.Helper()
	sm, _, err := profiles.Build("edge-minimal", profiles.Config{
		StateMap:             newTestState(t),
		DomainSpec:           mustSpec(t),
		EMAAlpha:             0.5,
		ConvergenceThreshold: 100,
		MinTrustScore:        0.5,
	})
	if err != nil {
		t.Fatalf("profiles.Build: %v", err)
	}
	mux := http.NewServeMux()
	registerRoutes(mux, sm, explain.NewDisabled())
	srv := httptest.NewServer(mux)
	return srv.URL, sm, srv.Close
}

func TestOffload_RejectExceedsLatencyBudget(t *testing.T) {
	base, sm, cleanup := newOffloadTestAgent(t)
	defer cleanup()
	drivePositiveCost(t, sm)

	cost, err := sm.CostOfAction("pod-scheduling", "node_self")
	if err != nil {
		t.Fatal(err)
	}
	if cost.LatencyEstimate <= 0 {
		t.Fatalf("test precondition: latency=%.3f must be > 0 after biasing", cost.LatencyEstimate)
	}

	resp := postJSON(t, base+"/offload", OffloadHTTPRequest{
		TaskType:        "pod-scheduling",
		SourceNodeID:    "node_self",
		DataSizeBytes:   1024,
		LatencyBudgetMs: cost.LatencyEstimate / 10, // tight
	})
	if resp.StatusCode != 200 {
		t.Fatalf("/offload: %s", body(resp))
	}
	var out OffloadHTTPResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if out.Accepted {
		t.Errorf("expected reject; got Accepted=true reason=%q (latency=%.3f budget=%.3f)",
			out.Reason, out.ExpectedLatency, cost.LatencyEstimate/10)
	}
	if !strings.Contains(out.Reason, "latency") {
		t.Errorf("reject reason should mention latency; got %q", out.Reason)
	}
}

func TestOffload_RejectExceedsEnergyBudget(t *testing.T) {
	base, sm, cleanup := newOffloadTestAgent(t)
	defer cleanup()
	drivePositiveCost(t, sm)

	cost, err := sm.CostOfAction("pod-scheduling", "node_self")
	if err != nil {
		t.Fatal(err)
	}
	if cost.ResourceCost <= 0 {
		t.Fatalf("test precondition: resource cost=%.3f must be > 0 after biasing", cost.ResourceCost)
	}
	tight := cost.ResourceCost / 10
	resp := postJSON(t, base+"/offload", OffloadHTTPRequest{
		TaskType:           "pod-scheduling",
		SourceNodeID:       "node_self",
		DataSizeBytes:      1024,
		LatencyBudgetMs:    1e9, // unbounded latency
		EnergyBudgetJoules: &tight,
	})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("/offload: %s", body(resp))
	}
	var out OffloadHTTPResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Accepted {
		t.Errorf("expected reject on energy budget; got Accepted=true reason=%q", out.Reason)
	}
	if !strings.Contains(out.Reason, "energy") {
		t.Errorf("reject reason should mention energy; got %q", out.Reason)
	}
}

func TestOffload_RequiresJSON(t *testing.T) {
	base, _, cleanup := newTestAgent(t)
	defer cleanup()
	resp := postRaw(t, base+"/offload", `{"task_type":"x"}`)
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("missing Content-Type: got %d, want 400", resp.StatusCode)
	}
}

// newTestState gives a fixture the state model a real daemon always attaches: cost is
// answered from the map, so an agent built without one cannot answer at all.
func newTestState(t *testing.T) *statemap.Map {
	t.Helper()
	// Mirrors the daemon's defaults: admission on, so a metric nobody declared still
	// becomes a property, and a low convergence count so a fixture reaches useful
	// confidence without thousands of samples.
	return statemap.New(statemap.Config{
		ConvergenceObservations: 100,
		AdmitUnknown:            true,
	}, statemap.NewJournal(0))
}
