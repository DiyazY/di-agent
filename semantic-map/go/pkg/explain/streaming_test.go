package explain_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/explain"
	"github.com/DiyazY/di-agent/pkg/profiles"
)

// collectEvents runs a streaming Explain and returns every emitted event.
func collectEvents(t *testing.T, e *explain.OpenAICompatibleExplainer, req explain.ExplainRequest) ([]explain.Event, error) {
	t.Helper()
	var events []explain.Event
	resp, err := e.ExplainStream(context.Background(), req, func(ev explain.Event) {
		events = append(events, ev)
	})
	_ = resp
	return events, err
}

func kindsOf(events []explain.Event) []explain.EventKind {
	out := make([]explain.EventKind, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.Kind)
	}
	return out
}

func hasKind(events []explain.Event, k explain.EventKind) bool {
	for _, ev := range events {
		if ev.Kind == k {
			return true
		}
	}
	return false
}

func TestExplainStream_EmitsFinalOnSuccess(t *testing.T) {
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

	llm := &scriptedLLM{responses: []string{assistantOnly(string(answer))}}
	srv := httptest.NewServer(llm.handler())
	defer srv.Close()

	exp, err := explain.NewOpenAICompatible(smReader{sm}, explain.OpenAICompatibleConfig{
		BaseURL: srv.URL, Model: "m", SystemPrompt: "p", HTTPTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	events, err := collectEvents(t, exp, explain.ExplainRequest{Question: "Why?"})
	if err != nil {
		t.Fatalf("ExplainStream: %v", err)
	}
	if !hasKind(events, explain.EventAnswering) {
		t.Errorf("expected an 'answering' event; got %v", kindsOf(events))
	}
	if !hasKind(events, explain.EventValidating) {
		t.Errorf("expected a 'validating' event; got %v", kindsOf(events))
	}
	// The terminal event must be exactly one 'final'.
	last := events[len(events)-1]
	if last.Kind != explain.EventFinal {
		t.Fatalf("expected the stream to end in 'final'; got %q (all: %v)", last.Kind, kindsOf(events))
	}
	if last.Response == nil || last.Response.Answer == "" {
		t.Error("the final event must carry the completed response")
	}
}

func TestExplainStream_EmitsErrorWhenLLMUnreachable(t *testing.T) {
	sm, _, err := profiles.Build("edge-minimal", profiles.Config{})
	if err != nil {
		t.Fatal(err)
	}
	// Server that immediately 500s — no scripted responses at all.
	llm := &scriptedLLM{responses: nil}
	srv := httptest.NewServer(llm.handler())
	defer srv.Close()

	exp, err := explain.NewOpenAICompatible(smReader{sm}, explain.OpenAICompatibleConfig{
		BaseURL: srv.URL, Model: "m", SystemPrompt: "p", HTTPTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := collectEvents(t, exp, explain.ExplainRequest{Question: "Why?"})
	if err == nil {
		t.Fatal("expected an error when the LLM is unreachable")
	}
	last := events[len(events)-1]
	if last.Kind != explain.EventError || last.Error == "" {
		t.Errorf("expected the stream to end in a populated 'error' event; got %+v", last)
	}
}

func TestExplainStream_EmitsPlanAndToolEvents(t *testing.T) {
	plan := assistantOnly(`{"approach":"peek at peers","steps":[{"tool":"get_peers","args":{}}]}`)
	e, sm, llm := newV2Explainer(t, []string{plan, ""}, nil)
	edges, _ := sm.AllEdges()
	e0 := edges[0]
	answer, _ := json.Marshal(map[string]any{
		"answer":     "No peers; " + e0.PropositionID + " unaffected.",
		"citations":  []map[string]any{{"kind": "edge", "id": e0.PropositionID, "ema_weight": e0.EMAWeight}},
		"confidence": "high",
	})
	llm.responses[1] = assistantOnly(string(answer))

	events, err := collectEvents(t, e, explain.ExplainRequest{Question: "Peers?", UsePlanner: true})
	if err != nil {
		t.Fatalf("ExplainStream: %v", err)
	}
	for _, want := range []explain.EventKind{
		explain.EventPlanning, explain.EventPlan,
		explain.EventToolCall, explain.EventToolResult,
		explain.EventAnswering, explain.EventValidating, explain.EventFinal,
	} {
		if !hasKind(events, want) {
			t.Errorf("missing %q event; got %v", want, kindsOf(events))
		}
	}
	// The plan event must carry the parsed plan.
	for _, ev := range events {
		if ev.Kind == explain.EventPlan && ev.Plan == nil {
			t.Error("the 'plan' event must carry the plan")
		}
	}
}

func TestExplainStream_EmitsSessionEvent(t *testing.T) {
	store := explain.NewSessionStore(explain.SessionConfig{})
	e, sm, llm := newV2Explainer(t, []string{""}, store)
	edges, _ := sm.AllEdges()
	e0 := edges[0]
	answer, _ := json.Marshal(map[string]any{
		"answer":     "Hi, " + e0.PropositionID + ".",
		"citations":  []map[string]any{{"kind": "edge", "id": e0.PropositionID, "ema_weight": e0.EMAWeight}},
		"confidence": "high",
	})
	llm.responses[0] = assistantOnly(string(answer))

	events, err := collectEvents(t, e, explain.ExplainRequest{Question: "Hi?"})
	if err != nil {
		t.Fatalf("ExplainStream: %v", err)
	}
	var sessionEv *explain.Event
	for i := range events {
		if events[i].Kind == explain.EventSession {
			sessionEv = &events[i]
			break
		}
	}
	if sessionEv == nil {
		t.Fatalf("expected a 'session' event; got %v", kindsOf(events))
	}
	if sessionEv.SessionID == "" {
		t.Error("the session event must carry the minted ID")
	}
}

func TestExplainStream_EmitsValidationFailedOnFabrication(t *testing.T) {
	sm, _, err := profiles.Build("edge-minimal", profiles.Config{})
	if err != nil {
		t.Fatal(err)
	}
	edges, _ := sm.AllEdges()
	e0 := edges[0]
	bad := assistantOnly(`{"answer":"P99 did it.","citations":[{"kind":"edge","id":"P99","ema_weight":0.42}],"confidence":"high"}`)
	good, _ := json.Marshal(map[string]any{
		"answer":     "Corrected: " + e0.PropositionID + ".",
		"citations":  []map[string]any{{"kind": "edge", "id": e0.PropositionID, "ema_weight": e0.EMAWeight}},
		"confidence": "high",
	})

	llm := &scriptedLLM{responses: []string{bad, assistantOnly(string(good))}}
	srv := httptest.NewServer(llm.handler())
	defer srv.Close()

	exp, err := explain.NewOpenAICompatible(smReader{sm}, explain.OpenAICompatibleConfig{
		BaseURL: srv.URL, Model: "m", SystemPrompt: "p", HTTPTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := collectEvents(t, exp, explain.ExplainRequest{Question: "Why?"})
	if err != nil {
		t.Fatalf("ExplainStream: %v", err)
	}
	if !hasKind(events, explain.EventValidationFailed) {
		t.Errorf("expected a 'validation_failed' event for the fabricated citation; got %v", kindsOf(events))
	}
	// The failure must carry the issues so a UI can show them live.
	for _, ev := range events {
		if ev.Kind == explain.EventValidationFailed && len(ev.Issues) == 0 {
			t.Error("'validation_failed' must carry the validator's issues")
		}
	}
}

// The contract clients are told to implement is "loop until final or error".
// That only works if exactly one of them ever arrives. A partial result — an
// answer that parsed but failed a gate — must ride along on the error event,
// not be emitted as a second terminal.
func TestExplainStream_PartialResultEmitsExactlyOneTerminalEvent(t *testing.T) {
	sm, _, err := profiles.Build("edge-minimal", profiles.Config{})
	if err != nil {
		t.Fatal(err)
	}
	// Every attempt cites a fabricated P99 → parses fine, fails Gate 1 on all
	// three iterations → explain() returns (partial response, error).
	bad := assistantOnly(`{"answer":"P99 did it.","citations":[{"kind":"edge","id":"P99","ema_weight":0.42}],"confidence":"high"}`)
	llm := &scriptedLLM{responses: []string{bad, bad, bad}}
	srv := httptest.NewServer(llm.handler())
	defer srv.Close()

	exp, err := explain.NewOpenAICompatible(smReader{sm}, explain.OpenAICompatibleConfig{
		BaseURL: srv.URL, Model: "m", SystemPrompt: "p", HTTPTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	events, err := collectEvents(t, exp, explain.ExplainRequest{Question: "Why?"})
	if err == nil {
		t.Fatal("expected a validation failure after MaxIterations")
	}

	var terminals []explain.EventKind
	for _, ev := range events {
		if ev.Kind == explain.EventFinal || ev.Kind == explain.EventError {
			terminals = append(terminals, ev.Kind)
		}
	}
	if len(terminals) != 1 {
		t.Fatalf("expected exactly one terminal event; got %d %v (all: %v)", len(terminals), terminals, kindsOf(events))
	}
	if terminals[0] != explain.EventError {
		t.Errorf("a failed call must terminate in 'error', not %q", terminals[0])
	}

	last := events[len(events)-1]
	if last.Kind != explain.EventError {
		t.Errorf("the terminal event must be last; got %q", last.Kind)
	}
	if last.Error == "" {
		t.Error("the error event must carry the reason")
	}
	// The partial answer must still reach the client — that is the whole point
	// of not simply dropping it.
	if last.Response == nil {
		t.Error("the error event must carry the partial response when one exists")
	}
}

// The success counterpart: exactly one terminal, and it is 'final'.
func TestExplainStream_SuccessEmitsExactlyOneTerminalEvent(t *testing.T) {
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
	llm := &scriptedLLM{responses: []string{assistantOnly(string(answer))}}
	srv := httptest.NewServer(llm.handler())
	defer srv.Close()

	exp, err := explain.NewOpenAICompatible(smReader{sm}, explain.OpenAICompatibleConfig{
		BaseURL: srv.URL, Model: "m", SystemPrompt: "p", HTTPTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := collectEvents(t, exp, explain.ExplainRequest{Question: "Why?"})
	if err != nil {
		t.Fatal(err)
	}
	var terminals int
	for _, ev := range events {
		if ev.Kind == explain.EventFinal || ev.Kind == explain.EventError {
			terminals++
		}
	}
	if terminals != 1 {
		t.Fatalf("expected exactly one terminal event; got %d (all: %v)", terminals, kindsOf(events))
	}
}

func TestExplainStream_NilEmitIsSafe(t *testing.T) {
	sm, _, err := profiles.Build("edge-minimal", profiles.Config{})
	if err != nil {
		t.Fatal(err)
	}
	edges, _ := sm.AllEdges()
	e0 := edges[0]
	answer, _ := json.Marshal(map[string]any{
		"answer":     "Fine, " + e0.PropositionID + ".",
		"citations":  []map[string]any{{"kind": "edge", "id": e0.PropositionID, "ema_weight": e0.EMAWeight}},
		"confidence": "high",
	})
	llm := &scriptedLLM{responses: []string{assistantOnly(string(answer))}}
	srv := httptest.NewServer(llm.handler())
	defer srv.Close()

	exp, err := explain.NewOpenAICompatible(smReader{sm}, explain.OpenAICompatibleConfig{
		BaseURL: srv.URL, Model: "m", SystemPrompt: "p", HTTPTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exp.ExplainStream(context.Background(), explain.ExplainRequest{Question: "Why?"}, nil); err != nil {
		t.Fatalf("a nil emitter must be tolerated: %v", err)
	}
}

// The base Explainer contract must still be satisfied — a caller holding an
// explain.Explainer can type-assert to StreamingExplainer and get streaming.
func TestOpenAICompatible_SatisfiesStreamingExplainer(t *testing.T) {
	var e explain.Explainer = &explain.OpenAICompatibleExplainer{}
	if _, ok := e.(explain.StreamingExplainer); !ok {
		t.Fatal("OpenAICompatibleExplainer must satisfy StreamingExplainer")
	}
}

// DisabledExplainer deliberately does NOT stream — the route falls back to a
// clear 501 rather than pretending.
func TestDisabled_DoesNotSatisfyStreamingExplainer(t *testing.T) {
	var e explain.Explainer = explain.NewDisabled()
	if _, ok := e.(explain.StreamingExplainer); ok {
		t.Fatal("DisabledExplainer should not advertise streaming support")
	}
}
