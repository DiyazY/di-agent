package main

import (
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
	sm, _, err := profiles.Build("edge-minimal", profiles.Config{})
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
	sm, _, err := profiles.Build("edge-minimal", profiles.Config{})
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
