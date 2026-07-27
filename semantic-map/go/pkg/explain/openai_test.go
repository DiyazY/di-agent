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
	"github.com/DiyazY/di-agent/pkg/profiles"
	"github.com/DiyazY/di-agent/pkg/semmap"
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

// ── v2: planner + critic + session integration ──────────────────────────────

// newV2Explainer builds an explainer with planner/critic prompts and a
// session store, driven by a scripted LLM.
func newV2Explainer(t *testing.T, script []string, sessions *explain.SessionStore) (*explain.OpenAICompatibleExplainer, *semmap.SemanticMap, *scriptedLLM) {
	t.Helper()
	sm, _, err := profiles.Build("edge-minimal", profiles.Config{})
	if err != nil {
		t.Fatalf("profiles.Build: %v", err)
	}
	llm := &scriptedLLM{responses: script}
	srv := httptest.NewServer(llm.handler())
	t.Cleanup(srv.Close)

	e, err := explain.NewOpenAICompatible(smReader{sm}, explain.OpenAICompatibleConfig{
		BaseURL:       srv.URL,
		Model:         "test-model",
		SystemPrompt:  "answering prompt",
		PlannerPrompt: "planner prompt",
		CriticPrompt:  "critic prompt",
		Sessions:      sessions,
		HTTPTimeout:   5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewOpenAICompatible: %v", err)
	}
	return e, sm, llm
}

// groundedAnswer builds an answer JSON citing a real edge so the
// deterministic validator passes.
func groundedAnswer(e0 edgeFacts, text string) string {
	body, _ := json.Marshal(map[string]any{
		"answer": text,
		"citations": []map[string]any{{
			"kind": "edge", "id": e0.ID,
			"ema_weight": e0.EMA, "prior_weight": e0.Prior, "confidence": e0.Conf,
		}},
		"confidence": "high",
	})
	return assistantOnly(string(body))
}

type edgeFacts struct {
	ID    string
	EMA   float64
	Prior float64
	Conf  float64
}

func firstEdge(t *testing.T, sm *semmap.SemanticMap) edgeFacts {
	t.Helper()
	edges, err := sm.AllEdges()
	if err != nil || len(edges) == 0 {
		t.Fatalf("AllEdges: %v (n=%d)", err, len(edges))
	}
	e := edges[0]
	return edgeFacts{ID: e.PropositionID, EMA: e.EMAWeight, Prior: e.PriorWeight, Conf: e.Confidence}
}

func TestExplain_PlannerRunsToolsBeforeAnswering(t *testing.T) {
	// Script: [0] planner emits a plan, [1] answering agent answers.
	// The answering agent makes NO tool calls — the plan already gathered
	// the evidence, which is the whole point of the planning stage.
	plan := assistantOnly(`{"approach":"look at peers","steps":[
		{"tool":"get_peers","args":{}},
		{"synthesize":"summarise peer trust"}
	]}`)
	e, sm, llm := newV2Explainer(t, []string{plan, ""}, nil)
	e0 := firstEdge(t, sm)
	llm.responses[1] = groundedAnswer(e0, "No peers are registered; "+e0.ID+" is unaffected.")

	resp, err := e.Explain(context.Background(), explain.ExplainRequest{
		Question:   "What peers do I have?",
		UsePlanner: true,
	})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if resp.Plan == nil {
		t.Fatal("expected the plan to be returned for audit")
	}
	if resp.Plan.Approach != "look at peers" || len(resp.Plan.Steps) != 2 {
		t.Errorf("unexpected plan: %+v", resp.Plan)
	}
	// The plan's get_peers step must show up in the trace even though the
	// answering agent never called a tool itself.
	if len(resp.ToolTrace) != 1 || resp.ToolTrace[0].Name != "get_peers" {
		t.Errorf("expected the plan's tool call in the trace; got %+v", resp.ToolTrace)
	}
	if resp.Usage.LLMTurns != 2 {
		t.Errorf("expected 2 LLM turns (planner + answer); got %d", resp.Usage.LLMTurns)
	}
	if resp.SchemaVersion != explain.SchemaVersion {
		t.Errorf("expected schema_version %q; got %q", explain.SchemaVersion, resp.SchemaVersion)
	}
}

func TestExplain_PlannerFailureFallsBackToUnplanned(t *testing.T) {
	// Planner emits garbage; the request must still succeed via the v1 path.
	e, sm, llm := newV2Explainer(t, []string{assistantOnly("I am not JSON"), ""}, nil)
	e0 := firstEdge(t, sm)
	llm.responses[1] = groundedAnswer(e0, "Answered without a plan: "+e0.ID+".")

	resp, err := e.Explain(context.Background(), explain.ExplainRequest{
		Question:   "Anything?",
		UsePlanner: true,
	})
	if err != nil {
		t.Fatalf("a broken planner must not fail the request: %v", err)
	}
	if resp.Plan != nil {
		t.Errorf("expected no plan on planner failure; got %+v", resp.Plan)
	}
}

func TestExplain_CriticApprovesGroundedAnswer(t *testing.T) {
	e, sm, llm := newV2Explainer(t, []string{"", assistantOnly(`{"approved":true,"issues":[]}`)}, nil)
	e0 := firstEdge(t, sm)
	llm.responses[0] = groundedAnswer(e0, "Driven by "+e0.ID+".")

	resp, err := e.Explain(context.Background(), explain.ExplainRequest{
		Question:  "Why?",
		UseCritic: true,
	})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if resp.CriticVerdict == nil || !resp.CriticVerdict.Approved {
		t.Errorf("expected an approving verdict; got %+v", resp.CriticVerdict)
	}
	if resp.Usage.LLMTurns != 2 {
		t.Errorf("expected 2 LLM turns (answer + critic); got %d", resp.Usage.LLMTurns)
	}
}

// The headline v2 case: an answer that is structurally perfect (every cited
// value matches live graph state) but semantically wrong. The deterministic
// validator waves it through; only the critic catches it.
func TestExplain_CriticCatchesStructurallyValidButWrongAnswer(t *testing.T) {
	e, sm, llm := newV2Explainer(t, []string{"", "", "", ""}, nil)
	e0 := firstEdge(t, sm)

	llm.responses[0] = groundedAnswer(e0, e0.ID+" targets Resource & Cost.") // wrong claim, right numbers
	llm.responses[1] = assistantOnly(`{"approved":false,"issues":["` + e0.ID + ` does not target RC — check the edge's destination construct."],"suggested_revision":"State the correct destination construct."}`)
	llm.responses[2] = groundedAnswer(e0, "Corrected: "+e0.ID+" has a specific destination construct in the graph.")
	llm.responses[3] = assistantOnly(`{"approved":true,"issues":[]}`)

	resp, err := e.Explain(context.Background(), explain.ExplainRequest{
		Question:  "What does it target?",
		UseCritic: true,
	})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if resp.Iterations != 2 {
		t.Errorf("expected 2 iterations (rejected then corrected); got %d", resp.Iterations)
	}
	if !strings.Contains(resp.Answer, "Corrected") {
		t.Errorf("expected the corrected answer; got %q", resp.Answer)
	}
	if resp.CriticVerdict == nil || !resp.CriticVerdict.Approved || resp.CriticVerdict.Round != 2 {
		t.Errorf("expected an approving round-2 verdict; got %+v", resp.CriticVerdict)
	}
	if resp.Usage.LLMTurns != 4 {
		t.Errorf("expected 4 LLM turns (answer, critic-reject, revise, critic-approve); got %d", resp.Usage.LLMTurns)
	}
}

// A critic that errors must not block a structurally valid answer.
func TestExplain_CriticFailureShipsValidAnswerWithNote(t *testing.T) {
	sm, _, err := profiles.Build("edge-minimal", profiles.Config{})
	if err != nil {
		t.Fatal(err)
	}
	edges, _ := sm.AllEdges()
	e0 := edges[0]
	answer, _ := json.Marshal(map[string]any{
		"answer":     "Fine answer citing " + e0.PropositionID + ".",
		"citations":  []map[string]any{{"kind": "edge", "id": e0.PropositionID, "ema_weight": e0.EMAWeight}},
		"confidence": "high",
	})

	llm := &scriptedLLM{responses: []string{assistantOnly(string(answer))}} // no critic response scripted → 500
	srv := httptest.NewServer(llm.handler())
	defer srv.Close()

	exp, err := explain.NewOpenAICompatible(smReader{sm}, explain.OpenAICompatibleConfig{
		BaseURL:      srv.URL,
		Model:        "test-model",
		SystemPrompt: "answering prompt",
		CriticPrompt: "critic prompt",
		HTTPTimeout:  5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := exp.Explain(context.Background(), explain.ExplainRequest{Question: "Why?", UseCritic: true})
	if err != nil {
		t.Fatalf("a broken critic must not fail a structurally valid answer: %v", err)
	}
	if resp.CriticVerdict == nil || !resp.CriticVerdict.Approved {
		t.Fatalf("expected a degraded-but-approving verdict; got %+v", resp.CriticVerdict)
	}
	if len(resp.CriticVerdict.Issues) == 0 || !strings.Contains(resp.CriticVerdict.Issues[0], "critic unavailable") {
		t.Errorf("expected the verdict to record the degradation; got %+v", resp.CriticVerdict.Issues)
	}
}

func TestExplain_SessionMintsIDAndPersistsTurn(t *testing.T) {
	store := explain.NewSessionStore(explain.SessionConfig{})
	e, sm, llm := newV2Explainer(t, []string{""}, store)
	e0 := firstEdge(t, sm)
	llm.responses[0] = groundedAnswer(e0, "First turn about "+e0.ID+".")

	resp, err := e.Explain(context.Background(), explain.ExplainRequest{Question: "First?"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if resp.SessionID == "" {
		t.Fatal("expected a minted session ID")
	}
	sess, err := store.Get(resp.SessionID)
	if err != nil {
		t.Fatalf("session should exist after the turn: %v", err)
	}
	if len(sess.Turns) != 1 || sess.Turns[0].Question != "First?" {
		t.Errorf("expected the turn recorded; got %+v", sess.Turns)
	}
}

func TestExplain_UnknownSessionIDFailsLoudly(t *testing.T) {
	store := explain.NewSessionStore(explain.SessionConfig{})
	e, _, _ := newV2Explainer(t, []string{assistantOnly(`{"answer":"x","citations":[]}`)}, store)

	_, err := e.Explain(context.Background(), explain.ExplainRequest{
		Question:  "Continue?",
		SessionID: "does-not-exist",
	})
	if err == nil || !strings.Contains(err.Error(), "session not found") {
		t.Errorf("expected a session-not-found error; got %v", err)
	}
}

func TestExplain_SessionIDWithoutStoreIsRejected(t *testing.T) {
	e, _, _ := newV2Explainer(t, []string{assistantOnly(`{"answer":"x","citations":[]}`)}, nil)
	_, err := e.Explain(context.Background(), explain.ExplainRequest{
		Question:  "Continue?",
		SessionID: "abc123",
	})
	if err == nil || !strings.Contains(err.Error(), "session support is disabled") {
		t.Errorf("expected a disabled-session error; got %v", err)
	}
}

// A model that loops on tool calls must not fail the request. Once the tool
// budget is spent we strip the tools and demand an answer from the evidence
// already gathered. Regression test for the v2 live smoke, where qwen2.5:7b
// burned all 10 tool calls on "how many propositions are in the graph?" and
// the daemon returned a bare "exceeded MaxToolCalls" with no answer.
func TestExplain_ToolLoopIsForcedToAnswerRatherThanFailing(t *testing.T) {
	sm, _, err := profiles.Build("edge-minimal", profiles.Config{})
	if err != nil {
		t.Fatal(err)
	}
	edges, _ := sm.AllEdges()
	e0 := edges[0]

	// Script: three tool calls (budget is 3), then a grounded answer.
	answer, _ := json.Marshal(map[string]any{
		"answer":     "Forced to answer from partial evidence: " + e0.PropositionID + ".",
		"citations":  []map[string]any{{"kind": "edge", "id": e0.PropositionID, "ema_weight": e0.EMAWeight}},
		"confidence": "low",
	})
	llm := &scriptedLLM{responses: []string{
		assistantToolCall("c1", "get_peers", `{}`),
		assistantToolCall("c2", "get_peers", `{}`),
		assistantToolCall("c3", "get_peers", `{}`),
		assistantOnly(string(answer)), // tools stripped by now
	}}
	srv := httptest.NewServer(llm.handler())
	defer srv.Close()

	exp, err := explain.NewOpenAICompatible(smReader{sm}, explain.OpenAICompatibleConfig{
		BaseURL: srv.URL, Model: "m", SystemPrompt: "p", HTTPTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := exp.Explain(context.Background(), explain.ExplainRequest{
		Question: "Count them",
		Budget:   explain.ExplainBudget{MaxToolCalls: 3},
	})
	if err != nil {
		t.Fatalf("budget exhaustion must degrade to an answer, not an error: %v", err)
	}
	if !strings.Contains(resp.Answer, e0.PropositionID) {
		t.Errorf("expected a grounded answer after the forced finish; got %q", resp.Answer)
	}
	if resp.Usage.ToolCalls != 3 {
		t.Errorf("expected exactly the budgeted 3 tool calls; got %d", resp.Usage.ToolCalls)
	}
}

func TestExplain_UsageAccountsTokensAndWallClock(t *testing.T) {
	sm, _, err := profiles.Build("edge-minimal", profiles.Config{})
	if err != nil {
		t.Fatal(err)
	}
	edges, _ := sm.AllEdges()
	e0 := edges[0]
	answer, _ := json.Marshal(map[string]any{
		"answer":     "Citing " + e0.PropositionID + ".",
		"citations":  []map[string]any{{"kind": "edge", "id": e0.PropositionID, "ema_weight": e0.EMAWeight}},
		"confidence": "high",
	})
	// Include a usage block so the accumulator has something to read.
	quoted, _ := json.Marshal(string(answer))
	body := fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":%s}}],"usage":{"prompt_tokens":120,"completion_tokens":45,"total_tokens":165}}`, string(quoted))

	llm := &scriptedLLM{responses: []string{body}}
	srv := httptest.NewServer(llm.handler())
	defer srv.Close()

	exp, err := explain.NewOpenAICompatible(smReader{sm}, explain.OpenAICompatibleConfig{
		BaseURL: srv.URL, Model: "m", SystemPrompt: "p", HTTPTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := exp.Explain(context.Background(), explain.ExplainRequest{Question: "Why?"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.PromptTokens != 120 || resp.Usage.CompletionTokens != 45 || resp.Usage.TotalTokens != 165 {
		t.Errorf("token accounting wrong: %+v", resp.Usage)
	}
	if resp.Usage.LLMTurns != 1 {
		t.Errorf("expected 1 LLM turn; got %d", resp.Usage.LLMTurns)
	}
	if resp.Usage.WallClockMs < 0 {
		t.Errorf("wall clock must be non-negative; got %d", resp.Usage.WallClockMs)
	}
}
