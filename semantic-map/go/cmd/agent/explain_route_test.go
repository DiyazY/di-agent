package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/DiyazY/di-agent/pkg/explain"
	"github.com/DiyazY/di-agent/pkg/peers"
	"github.com/DiyazY/di-agent/pkg/profiles"
	"github.com/DiyazY/di-agent/pkg/semmap"
)

// explainerReader is the same shim as in main.go's buildExplainer, redeclared
// here so the test file doesn't depend on main.go's private types being
// exported.
type explainerReader struct{ *semmap.SemanticMap }

func (r explainerReader) Peers() *peers.Registry { return r.SemanticMap.Peers() }

// newExplainTestAgent boots a test agent wired with the given Explainer.
func newExplainTestAgent(t *testing.T, e explain.Explainer) (string, *semmap.SemanticMap, func()) {
	t.Helper()
	sm, _, err := profiles.Build("edge-minimal", profiles.Config{DomainSpec: mustSpec(t)})
	if err != nil {
		t.Fatalf("profiles.Build: %v", err)
	}
	mux := http.NewServeMux()
	registerRoutes(mux, sm, e)
	srv := httptest.NewServer(mux)
	return srv.URL, sm, srv.Close
}

func TestExplainRoute_ProviderNone_Returns501(t *testing.T) {
	base, _, cleanup := newExplainTestAgent(t, explain.NewDisabled())
	defer cleanup()

	body := []byte(`{"question":"Why is my cost high?"}`)
	resp, err := http.Post(base+"/explain", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /explain: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		msg, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 501; got %d: %s", resp.StatusCode, string(msg))
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if _, ok := out["error"]; !ok {
		t.Errorf("expected JSON error body; got %v", out)
	}
}

func TestExplainRoute_MissingQuestion_Returns400(t *testing.T) {
	base, _, cleanup := newExplainTestAgent(t, explain.NewDisabled())
	defer cleanup()

	// Empty question — should be a 400, NOT a 501, because the request is malformed
	// before we even try to invoke the (disabled) explainer.
	body := []byte(`{"question":""}`)
	resp, err := http.Post(base+"/explain", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /explain: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		msg, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400; got %d: %s", resp.StatusCode, string(msg))
	}
}

func TestExplainRoute_MissingContentType_Returns400(t *testing.T) {
	base, _, cleanup := newExplainTestAgent(t, explain.NewDisabled())
	defer cleanup()
	body := []byte(`{"question":"hi"}`)
	// Note: not "application/json" — should trip requireJSON.
	resp, err := http.Post(base+"/explain", "text/plain", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /explain: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400; got %d", resp.StatusCode)
	}
}

// TestExplainRoute_EndToEnd_WithMockLLM drives POST /explain against a
// scripted OpenAI-compatible backend and asserts the answer flows through the
// reflection loop, validator, and route handler intact.
func TestExplainRoute_EndToEnd_WithMockLLM(t *testing.T) {
	// Build the SemanticMap first so we know a real edge to cite.
	sm, _, err := profiles.Build("edge-minimal", profiles.Config{DomainSpec: mustSpec(t)})
	if err != nil {
		t.Fatalf("profiles.Build: %v", err)
	}
	edges, _ := sm.AllEdges()
	e0 := edges[0]

	answer, _ := json.Marshal(map[string]any{
		"answer":     fmt.Sprintf("The dominant edge is %s.", e0.PropositionID),
		"citations":  []map[string]any{{"kind": "edge", "id": e0.PropositionID, "ema_weight": e0.EMAWeight}},
		"confidence": "high",
	})
	llmCalls := &atomic.Int32{}
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		llmCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q}}]}`, string(answer))
	}))
	defer llmServer.Close()

	explainer, err := explain.NewOpenAICompatible(explainerReader{sm}, explain.OpenAICompatibleConfig{
		BaseURL:      llmServer.URL,
		Model:        "mock",
		SystemPrompt: "test prompt",
	})
	if err != nil {
		t.Fatalf("NewOpenAICompatible: %v", err)
	}

	mux := http.NewServeMux()
	registerRoutes(mux, sm, explainer)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body := []byte(`{"question":"Which edge dominates?"}`)
	resp, err := http.Post(srv.URL+"/explain", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /explain: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200; got %d: %s", resp.StatusCode, string(msg))
	}
	var er explain.ExplainResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(er.Answer, e0.PropositionID) {
		t.Errorf("expected answer to mention %s; got %q", e0.PropositionID, er.Answer)
	}
	if er.ModelName != "mock" || er.Iterations != 1 || len(er.Citations) != 1 {
		t.Errorf("unexpected response: %+v", er)
	}
	if llmCalls.Load() != 1 {
		t.Errorf("expected 1 LLM call; got %d", llmCalls.Load())
	}
}

// TestExplainRoute_StreamingReturnsNDJSON drives POST /explain with
// stream:true and asserts the response is line-delimited JSON terminating in
// a single `final` event.
func TestExplainRoute_StreamingReturnsNDJSON(t *testing.T) {
	sm, _, err := profiles.Build("edge-minimal", profiles.Config{DomainSpec: mustSpec(t)})
	if err != nil {
		t.Fatalf("profiles.Build: %v", err)
	}
	edges, _ := sm.AllEdges()
	e0 := edges[0]

	answer, _ := json.Marshal(map[string]any{
		"answer":     fmt.Sprintf("Streamed answer about %s.", e0.PropositionID),
		"citations":  []map[string]any{{"kind": "edge", "id": e0.PropositionID, "ema_weight": e0.EMAWeight}},
		"confidence": "high",
	})
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q}}]}`, string(answer))
	}))
	defer llmServer.Close()

	explainer, err := explain.NewOpenAICompatible(explainerReader{sm}, explain.OpenAICompatibleConfig{
		BaseURL:      llmServer.URL,
		Model:        "mock",
		SystemPrompt: "test prompt",
	})
	if err != nil {
		t.Fatalf("NewOpenAICompatible: %v", err)
	}

	mux := http.NewServeMux()
	registerRoutes(mux, sm, explainer)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/explain", "application/json",
		bytes.NewReader([]byte(`{"question":"Why?","stream":true}`)))
	if err != nil {
		t.Fatalf("POST /explain: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); got != "application/x-ndjson" {
		t.Errorf("expected NDJSON content type; got %q", got)
	}

	// Every line must be a standalone JSON event.
	var kinds []string
	var finalSeen bool
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev explain.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line is not valid JSON: %q (%v)", line, err)
		}
		kinds = append(kinds, string(ev.Kind))
		if ev.Kind == explain.EventFinal {
			finalSeen = true
			if ev.Response == nil || !strings.Contains(ev.Response.Answer, e0.PropositionID) {
				t.Errorf("final event should carry the grounded answer; got %+v", ev.Response)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading stream: %v", err)
	}
	if !finalSeen {
		t.Errorf("stream must terminate in a 'final' event; saw %v", kinds)
	}
	if len(kinds) < 2 {
		t.Errorf("expected progress events before the final; saw %v", kinds)
	}
}

// An unknown session_id is the CALLER's mistake. Returning 500 would send an
// operator hunting for a server fault over an expired or mistyped session id.
func TestExplainRoute_UnknownSessionReturns400(t *testing.T) {
	sm, _, err := profiles.Build("edge-minimal", profiles.Config{DomainSpec: mustSpec(t)})
	if err != nil {
		t.Fatalf("profiles.Build: %v", err)
	}
	// The LLM is never reached — session resolution fails first.
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the LLM must not be called when the session id is unknown")
	}))
	defer llmServer.Close()

	explainer, err := explain.NewOpenAICompatible(explainerReader{sm}, explain.OpenAICompatibleConfig{
		BaseURL:      llmServer.URL,
		Model:        "mock",
		SystemPrompt: "test prompt",
		Sessions:     explain.NewSessionStore(explain.SessionConfig{}),
	})
	if err != nil {
		t.Fatalf("NewOpenAICompatible: %v", err)
	}

	mux := http.NewServeMux()
	registerRoutes(mux, sm, explainer)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/explain", "application/json",
		bytes.NewReader([]byte(`{"question":"Why?","session_id":"does-not-exist"}`)))
	if err != nil {
		t.Fatalf("POST /explain: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400 for an unknown session; got %d: %s", resp.StatusCode, string(body))
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if msg, _ := out["error"].(string); !strings.Contains(msg, "session not found") {
		t.Errorf("the error body should name the cause; got %v", out)
	}
}

// A streaming request against an Explainer that cannot stream must fail
// clearly rather than silently degrading to a buffered response.
func TestExplainRoute_StreamingAgainstDisabledReturns501(t *testing.T) {
	base, _, cleanup := newExplainTestAgent(t, explain.NewDisabled())
	defer cleanup()

	resp, err := http.Post(base+"/explain", "application/json",
		bytes.NewReader([]byte(`{"question":"Why?","stream":true}`)))
	if err != nil {
		t.Fatalf("POST /explain: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("expected 501; got %d", resp.StatusCode)
	}
}
