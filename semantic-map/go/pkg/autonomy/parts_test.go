package autonomy_test

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
