package explain

import (
	"strings"
	"testing"
)

// These tests live in package `explain` (not `explain_test`) because they
// exercise unexported plan parsing and execution helpers directly.

func TestParsePlan_AcceptsBareJSON(t *testing.T) {
	p, err := parsePlan(`{"approach":"x","steps":[{"tool":"get_peers","args":{}}]}`)
	if err != nil {
		t.Fatalf("parsePlan: %v", err)
	}
	if p.Approach != "x" || len(p.Steps) != 1 || p.Steps[0].Tool != "get_peers" {
		t.Errorf("unexpected plan: %+v", p)
	}
}

func TestParsePlan_StripsFencesAndProse(t *testing.T) {
	raw := "Here is my plan:\n```json\n{\"steps\":[{\"tool\":\"get_cost\",\"args\":{\"node_id\":\"master\",\"task_type\":\"t\"}}]}\n```\nHope that helps!"
	p, err := parsePlan(raw)
	if err != nil {
		t.Fatalf("parsePlan: %v", err)
	}
	if len(p.Steps) != 1 || p.Steps[0].Tool != "get_cost" {
		t.Errorf("unexpected plan: %+v", p)
	}
}

func TestParsePlan_RejectsEmptySteps(t *testing.T) {
	if _, err := parsePlan(`{"steps":[]}`); err == nil {
		t.Fatal("expected error for zero-step plan")
	}
}

func TestParsePlan_TruncatesOverlongPlan(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"steps":[`)
	for i := 0; i < MaxPlanSteps+4; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"tool":"get_peers","args":{}}`)
	}
	b.WriteString(`]}`)
	p, err := parsePlan(b.String())
	if err != nil {
		t.Fatalf("parsePlan: %v", err)
	}
	if len(p.Steps) != MaxPlanSteps {
		t.Errorf("expected truncation to %d steps; got %d", MaxPlanSteps, len(p.Steps))
	}
}

func TestValidatePlan_FlagsUnknownTool(t *testing.T) {
	issues := validatePlan(&Plan{Steps: []PlanStep{{Tool: "drop_database"}}})
	if len(issues) == 0 {
		t.Fatal("expected an issue for the unknown tool")
	}
	if !strings.Contains(strings.Join(issues, " "), "drop_database") {
		t.Errorf("issue should name the offending tool; got %v", issues)
	}
}

func TestValidatePlan_FlagsStepWithNeitherToolNorSynthesize(t *testing.T) {
	issues := validatePlan(&Plan{Steps: []PlanStep{{Rationale: "hmm"}}})
	if len(issues) == 0 {
		t.Fatal("expected an issue for the empty step")
	}
}

func TestValidatePlan_FlagsSynthesizeOnlyPlan(t *testing.T) {
	issues := validatePlan(&Plan{Steps: []PlanStep{{Synthesize: "just think about it"}}})
	joined := strings.Join(issues, " ")
	if !strings.Contains(joined, "no tool steps") {
		t.Errorf("expected 'no tool steps' issue; got %v", issues)
	}
}

func TestValidatePlan_AcceptsWellFormedPlan(t *testing.T) {
	issues := validatePlan(&Plan{Steps: []PlanStep{
		{Tool: "get_cost", Args: map[string]any{"node_id": "master", "task_type": "t"}},
		{Synthesize: "rank the edges"},
	}})
	if len(issues) != 0 {
		t.Errorf("expected no issues; got %v", issues)
	}
}

func TestExecutePlan_CollectsEvidenceAndTrace(t *testing.T) {
	r := newPlannerTestReader(t)
	plan := &Plan{
		Approach: "check peers then edges",
		Steps: []PlanStep{
			{Tool: "get_peers", Args: map[string]any{}},
			{Tool: "get_edges", Args: map[string]any{"to": "RC"}},
			{Synthesize: "rank by deviation"},
		},
	}
	exec := executePlan(r, plan, 10, nil, "", nil)
	if exec.ToolCalls != 2 {
		t.Errorf("expected 2 tool calls (synthesize is free); got %d", exec.ToolCalls)
	}
	if len(exec.Trace) != 2 {
		t.Errorf("expected 2 trace entries; got %d", len(exec.Trace))
	}
	if !strings.Contains(exec.Evidence, "check peers then edges") {
		t.Error("evidence bundle should carry the plan approach")
	}
	if !strings.Contains(exec.Evidence, "SYNTHESIZE: rank by deviation") {
		t.Error("evidence bundle should carry the synthesize instruction")
	}
	if !strings.Contains(exec.Evidence, "get_edges") {
		t.Error("evidence bundle should name each executed tool")
	}
}

func TestExecutePlan_ContinuesPastToolError(t *testing.T) {
	r := newPlannerTestReader(t)
	plan := &Plan{Steps: []PlanStep{
		// get_cost without required args → dispatcher returns an error.
		{Tool: "get_cost", Args: map[string]any{}},
		{Tool: "get_peers", Args: map[string]any{}},
	}}
	exec := executePlan(r, plan, 10, nil, "", nil)
	if len(exec.Trace) != 2 {
		t.Fatalf("expected both steps traced; got %d", len(exec.Trace))
	}
	if exec.Trace[0].Error == "" {
		t.Error("first step should have recorded an error")
	}
	if exec.Trace[1].Error != "" {
		t.Error("second step should have succeeded despite the first failing")
	}
	if !strings.Contains(exec.Evidence, "ERROR") {
		t.Error("evidence bundle should surface the failed step")
	}
}

func TestExecutePlan_RespectsRemainingBudget(t *testing.T) {
	r := newPlannerTestReader(t)
	plan := &Plan{Steps: []PlanStep{
		{Tool: "get_peers", Args: map[string]any{}},
		{Tool: "get_peers", Args: map[string]any{}},
		{Tool: "get_peers", Args: map[string]any{}},
	}}
	exec := executePlan(r, plan, 2, nil, "", nil)
	if exec.ToolCalls != 2 {
		t.Errorf("expected budget to cap execution at 2; got %d", exec.ToolCalls)
	}
	if !strings.Contains(exec.Evidence, "budget exhausted") {
		t.Error("evidence bundle should note the truncation so the answering agent knows it is incomplete")
	}
}

func TestExecutePlan_UsesSessionCacheOnRepeatCall(t *testing.T) {
	r := newPlannerTestReader(t)
	store := NewSessionStore(SessionConfig{})
	sess := store.Create()

	plan := &Plan{Steps: []PlanStep{{Tool: "get_peers", Args: map[string]any{}}}}

	first := executePlan(r, plan, 10, store, sess.ID, nil)
	if first.CacheHits != 0 {
		t.Errorf("first run should be a cache miss; got %d hits", first.CacheHits)
	}
	second := executePlan(r, plan, 10, store, sess.ID, nil)
	if second.CacheHits != 1 {
		t.Errorf("second run should hit the cache; got %d hits", second.CacheHits)
	}
	if !strings.Contains(second.Trace[0].ResultDigest, "cached") {
		t.Errorf("cached invocation should be marked in the trace; got %q", second.Trace[0].ResultDigest)
	}
}

func TestStripJSONEnvelope_HandlesPlainObject(t *testing.T) {
	if got := stripJSONEnvelope(`  {"a":1}  `); got != `{"a":1}` {
		t.Errorf("got %q", got)
	}
}

func TestStripJSONEnvelope_HandlesEmpty(t *testing.T) {
	if got := stripJSONEnvelope("   "); got != "" {
		t.Errorf("expected empty string; got %q", got)
	}
}
