package explain_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/explain"
)

// scriptedLLM is an httptest server that responds to /chat/completions with a
// pre-programmed sequence of assistant messages. Each POST advances the index.
type scriptedLLM struct {
	responses []string
	idx       atomic.Int32
	seen      atomic.Int32 // number of requests received
}

func (s *scriptedLLM) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.seen.Add(1)
		i := int(s.idx.Add(1)) - 1
		if i >= len(s.responses) {
			http.Error(w, "scriptedLLM: out of programmed responses", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, s.responses[i])
	})
}

// assistantOnly wraps a raw assistant content string in the chat-completions
// wire shape. No tool calls.
func assistantOnly(content string) string {
	// json-escape by using json.Marshal on the content
	quoted, _ := json.Marshal(content)
	return fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":%s}}]}`, string(quoted))
}

// assistantToolCall wraps a request for tool calls (no final content).
func assistantToolCall(callID, toolName, argsJSON string) string {
	return fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}]}}]}`, callID, toolName, argsJSON)
}

func newExplainer(t *testing.T, script []string) (*explain.OpenAICompatibleExplainer, *scriptedLLM) {
	t.Helper()
	reader, _ := newTestMap(t)
	llm := &scriptedLLM{responses: script}
	srv := httptest.NewServer(llm.handler())
	t.Cleanup(srv.Close)
	e, err := explain.NewOpenAICompatible(reader, explain.OpenAICompatibleConfig{
		BaseURL:      srv.URL + "/v1", // just a shape; httptest server ignores path segments beyond the mux root
		Model:        "test-model",
		SystemPrompt: "test prompt v1",
		PromptFile:   "explain-v1.md",
		HTTPTimeout:  5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewOpenAICompatible: %v", err)
	}
	// The scriptedLLM handler ignores request path, so pointing BaseURL at the
	// server root works even though the client will append /chat/completions.
	// We do that by re-configuring BaseURL to the server root:
	e, err = explain.NewOpenAICompatible(reader, explain.OpenAICompatibleConfig{
		BaseURL:      srv.URL,
		Model:        "test-model",
		SystemPrompt: "test prompt v1",
		PromptFile:   "explain-v1.md",
		HTTPTimeout:  5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewOpenAICompatible: %v", err)
	}
	return e, llm
}

func TestExplain_HappyPath_NoToolCall(t *testing.T) {
	// The model answers immediately with a grounded citation.
	script := []string{
		assistantOnly(`{
			"answer": "The RC construct exists in the ontology.",
			"citations": [{"kind":"construct","id":"RC"}],
			"confidence":"high"
		}`),
	}
	e, llm := newExplainer(t, script)
	resp, err := e.Explain(context.Background(), explain.ExplainRequest{Question: "What is RC?"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if resp.Answer == "" || resp.ModelName != "test-model" || resp.Iterations != 1 {
		t.Errorf("unexpected response: %+v", resp)
	}
	if llm.seen.Load() != 1 {
		t.Errorf("expected 1 LLM call; got %d", llm.seen.Load())
	}
}

func TestExplain_UsesTool_ThenAnswers(t *testing.T) {
	// Model asks for get_edges once, then answers.
	// We construct the answer *after* we know which edge came back — but for
	// the test we just accept whatever edge_minimal ships. The scriptedLLM's
	// second response must still validate, so cite an edge we know exists.
	reader, sm := newTestMap(t)
	edges, _ := sm.AllEdges()
	e0 := edges[0]

	answerJSON, _ := json.Marshal(map[string]any{
		"answer": fmt.Sprintf("Fetched edges and confirmed %s exists.", e0.PropositionID),
		"citations": []map[string]any{
			{"kind": "edge", "id": e0.PropositionID, "ema_weight": e0.EMAWeight, "prior_weight": e0.PriorWeight, "confidence": e0.Confidence, "n_observations": e0.NObservations},
		},
		"confidence": "high",
	})
	script := []string{
		assistantToolCall("call1", "get_edges", `{}`),
		assistantOnly(string(answerJSON)),
	}
	llm := &scriptedLLM{responses: script}
	srv := httptest.NewServer(llm.handler())
	t.Cleanup(srv.Close)

	exp, err := explain.NewOpenAICompatible(smReader{sm}, explain.OpenAICompatibleConfig{
		BaseURL:      srv.URL,
		Model:        "test-model",
		SystemPrompt: "test prompt v1",
		HTTPTimeout:  5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewOpenAICompatible: %v", err)
	}
	_ = reader // silence unused

	resp, err := exp.Explain(context.Background(), explain.ExplainRequest{Question: "What edges exist?"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(resp.ToolTrace) != 1 || resp.ToolTrace[0].Name != "get_edges" {
		t.Errorf("expected exactly one get_edges tool call; got trace=%+v", resp.ToolTrace)
	}
	if resp.Iterations != 1 {
		t.Errorf("expected 1 iteration; got %d", resp.Iterations)
	}
}

func TestExplain_ReflectionLoop_FixesFabrication(t *testing.T) {
	// First response cites P99 (fabricated). Second response fixes it.
	reader, sm := newTestMap(t)
	edges, _ := sm.AllEdges()
	e0 := edges[0]

	badAnswer := `{"answer":"P99 explains everything.","citations":[{"kind":"edge","id":"P99","ema_weight":0.42}],"confidence":"high"}`
	goodAnswer := fmt.Sprintf(`{"answer":"Actually %s is the driver.","citations":[{"kind":"edge","id":"%s","ema_weight":%f,"prior_weight":%f,"confidence":%f}],"confidence":"high"}`,
		e0.PropositionID, e0.PropositionID, e0.EMAWeight, e0.PriorWeight, e0.Confidence)

	llm := &scriptedLLM{responses: []string{assistantOnly(badAnswer), assistantOnly(goodAnswer)}}
	srv := httptest.NewServer(llm.handler())
	t.Cleanup(srv.Close)

	exp, err := explain.NewOpenAICompatible(smReader{sm}, explain.OpenAICompatibleConfig{
		BaseURL:      srv.URL,
		Model:        "test-model",
		SystemPrompt: "test prompt v1",
		HTTPTimeout:  5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewOpenAICompatible: %v", err)
	}
	_ = reader

	resp, err := exp.Explain(context.Background(), explain.ExplainRequest{Question: "Why?"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if resp.Iterations != 2 {
		t.Errorf("expected 2 iterations (fabricated then fixed); got %d", resp.Iterations)
	}
	if !strings.Contains(resp.Answer, e0.PropositionID) {
		t.Errorf("expected corrected answer to reference %s; got %q", e0.PropositionID, resp.Answer)
	}
}

func TestExplain_DisabledReturnsErrNotEnabled(t *testing.T) {
	e := explain.NewDisabled()
	if _, err := e.Explain(context.Background(), explain.ExplainRequest{Question: "hi"}); err != explain.ErrNotEnabled {
		t.Errorf("expected ErrNotEnabled; got %v", err)
	}
}
