package autonomy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/autonomy"
	"github.com/DiyazY/di-agent/pkg/types"
)

// A part registered from outside the package is built by name like a built-in one.
type neverLow struct{}

func (neverLow) Name() string { return "never-low" }
func (neverLow) Apply(ev autonomy.Evidence) (string, string, bool) {
	if ev.Candidate.MIScore < 0.95 {
		return types.ReviewDefer, "under 0.95", true
	}
	return "", "", false
}

func init() { autonomy.RegisterRule("never-low", func() autonomy.Rule { return neverLow{} }) }

func TestAPartIsChosenByName(t *testing.T) {
	const id = "cpu_utilization@pod:a->cpu_pressure"
	m := newFakeMap(t, directed(id, ""))
	cfg, err := autonomy.Build(m, autonomy.Parts{Rules: []string{"never-low", "composition"}})
	if err != nil {
		t.Fatal(err)
	}
	run(t, m, autonomy.Config{Level: autonomy.Full, Rules: cfg.Rules})
	if c := m.cands[id]; c.Status != types.Deferred || c.Review.Model != "never-low" {
		t.Errorf("%v review %+v; want deferred by the registered rule", c.Status, c.Review)
	}
	for _, p := range []autonomy.Parts{
		{Rules: []string{"no-such-rule"}},
		{DecisionBackend: "no-such-backend"},
		{ReasoningBackend: "openai-tools", Reasoning: autonomy.Spec{Model: "m"}, Tools: []string{"no-such-tool"}},
		{Renderer: "no-such-renderer"},
	} {
		if _, err := autonomy.Build(m, p); err == nil || !strings.Contains(err.Error(), "no ") {
			t.Errorf("%+v built (%v); want an unknown name refused, naming what exists", p, err)
		}
	}
}

// logprobsServer answers every chat call with the given first-token log-probabilities.
func logprobsServer(t *testing.T, top map[string]float64, seen *[]map[string]any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		*seen = append(*seen, req)
		var tl []map[string]any
		for tok, p := range top {
			tl = append(tl, map[string]any{"token": tok, "logprob": math.Log(p)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"message":  map[string]any{"role": "assistant", "content": "A"},
			"logprobs": map[string]any{"content": []any{map[string]any{"token": "A", "logprob": -0.1, "top_logprobs": tl}}},
		}}})
	}))
}

func TestLogprobsReadsADistributionOverTheLetters(t *testing.T) {
	var seen []map[string]any
	srv := logprobsServer(t, map[string]float64{"A": 0.6, " B": 0.2, "C": 0.1, "The": 0.1}, &seen)
	defer srv.Close()
	m := newFakeMap(t)
	cfg, err := autonomy.Build(m, autonomy.Parts{DecisionBackend: "openai-logprobs",
		Decision: autonomy.Spec{URL: srv.URL, Model: "general", Options: map[string]string{"reasoning_effort": "none"}}})
	if err != nil {
		t.Fatal(err)
	}
	v, err := cfg.Decider.Decide(context.Background(), autonomy.Evidence{Candidate: *directed("a@pod:x->b", "")}, "")
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(v.Actions[types.ReviewConfirm]-2.0/3) > 1e-9 || math.Abs(v.OptionMass-0.9) > 1e-9 {
		t.Errorf("actions %v mass %.3f; want confirm 0.667 over the letters and 0.9 on them", v.Actions, v.OptionMass)
	}
	if len(seen) != 1 || seen[0]["max_tokens"] != 1.0 || seen[0]["logprobs"] != true || seen[0]["reasoning_effort"] != "none" {
		t.Errorf("request %v; want one token, logprobs, reasoning_effort none", seen)
	}
}

func TestMostProbabilityOffTheOptionsEscalates(t *testing.T) {
	var seen []map[string]any
	srv := logprobsServer(t, map[string]float64{"A": 0.3, "I": 0.7}, &seen) // sure of A among the letters, but wanting to talk
	defer srv.Close()
	const id = "cpu_utilization@pod:a->cpu_pressure"
	m := newFakeMap(t, directed(id, ""))
	cfg, err := autonomy.Build(m, autonomy.Parts{DecisionBackend: "openai-logprobs",
		Decision: autonomy.Spec{URL: srv.URL, Model: "general"}})
	if err != nil {
		t.Fatal(err)
	}
	run(t, m, autonomy.Config{Level: autonomy.Full, Decider: cfg.Decider})
	if c := m.cands[id]; c.Status != types.Pending || c.Review.Action != types.ReviewEscalate {
		t.Errorf("%v review %+v; want escalated: 0.3 of the probability was on the options", c.Status, c.Review)
	}
}

func TestARefusedReasoningEffortIsDropped(t *testing.T) {
	var seen []map[string]any
	ok := logprobsServer(t, map[string]float64{"A": 0.9, "B": 0.1}, &seen)
	defer ok.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		if _, has := req["reasoning_effort"]; has { // as vLLM answers "none"
			seen = append(seen, req)
			http.Error(w, `{"error":"reasoning_effort must be one of low, medium, high"}`, http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		ok.Config.Handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	cfg, err := autonomy.Build(newFakeMap(t), autonomy.Parts{DecisionBackend: "openai-logprobs",
		Decision: autonomy.Spec{URL: srv.URL, Model: "general", Options: map[string]string{"reasoning_effort": "none"}}})
	if err != nil {
		t.Fatal(err)
	}
	ev := autonomy.Evidence{Candidate: *directed("a@pod:x->b", "")}
	for range 2 {
		if _, err := cfg.Decider.Decide(context.Background(), ev, ""); err != nil {
			t.Fatal(err)
		}
	}
	// refused once, retried without it, and not sent again
	if len(seen) != 3 || seen[0]["reasoning_effort"] != "none" || seen[1]["reasoning_effort"] != nil || seen[2]["reasoning_effort"] != nil {
		t.Errorf("requests %v; want reasoning_effort refused once, then dropped", seen)
	}
}

func TestAReasonerThatWillNotStopCallingToolsIsStopped(t *testing.T) {
	turns := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turns++
		var calls []any
		for i := range 4 { // more in one turn than the budget allows
			calls = append(calls, map[string]any{"id": fmt.Sprint("c", turns, i), "type": "function",
				"function": map[string]any{"name": "get_state", "arguments": "{}"}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"message": map[string]any{"role": "assistant", "content": "", "tool_calls": calls}}}})
	}))
	defer srv.Close()
	m := newFakeMap(t)
	cfg, err := autonomy.Build(m, autonomy.Parts{ReasoningBackend: "openai-tools",
		Reasoning: autonomy.Spec{URL: srv.URL, Model: "chatty"}, Tools: []string{"get_state"}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := cfg.Reasoner.Reason(context.Background(), autonomy.Evidence{Candidate: *directed("a@pod:x->b", "")})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "kept calling tools") {
			t.Errorf("err %v; want the loop stopped", err)
		}
		if turns != 3 { // 4 calls, 2 more (budget 6, the rest refused), then the final turn
			t.Errorf("%d turns; want 3", turns)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the tool loop did not stop")
	}
}
