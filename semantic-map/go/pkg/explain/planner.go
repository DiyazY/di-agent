package explain

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// MaxPlanSteps caps how many steps a planner may emit. A plan longer than
// this is truncated (with a note in the evidence bundle) rather than
// rejected — a too-eager planner should degrade, not fail the request.
const MaxPlanSteps = 6

// parsePlan extracts a Plan from the planner agent's assistant message.
// Tolerant of ``` fences and leading/trailing prose for the same reason
// parseExplainResponse is: local models frequently ignore "JSON only".
func parsePlan(content string) (*Plan, error) {
	content = stripJSONEnvelope(content)
	if content == "" {
		return nil, errors.New("planner returned an empty message")
	}
	var p Plan
	if err := json.Unmarshal([]byte(content), &p); err != nil {
		return nil, fmt.Errorf("planner output is not valid JSON: %w", err)
	}
	if len(p.Steps) == 0 {
		return nil, errors.New("planner returned a plan with no steps")
	}
	if len(p.Steps) > MaxPlanSteps {
		p.Steps = p.Steps[:MaxPlanSteps]
	}
	return &p, nil
}

// validatePlan checks a plan's structure before we spend any budget on it.
// Structural problems (unknown tool, step with neither tool nor synthesize)
// are returned as issues rather than errors so the caller can decide whether
// to re-plan or fall back to unplanned execution.
func validatePlan(p *Plan) []string {
	if p == nil {
		return []string{"plan is nil"}
	}
	known := make(map[string]struct{}, len(toolSchemas))
	for _, s := range toolSchemas {
		known[s.Name] = struct{}{}
	}
	var issues []string
	toolSteps := 0
	for i, step := range p.Steps {
		hasTool := strings.TrimSpace(step.Tool) != ""
		hasSynth := strings.TrimSpace(step.Synthesize) != ""
		switch {
		case hasTool && hasSynth:
			issues = append(issues, fmt.Sprintf("steps[%d]: set exactly one of tool/synthesize, not both", i))
		case !hasTool && !hasSynth:
			issues = append(issues, fmt.Sprintf("steps[%d]: step has neither tool nor synthesize", i))
		case hasTool:
			toolSteps++
			if _, ok := known[step.Tool]; !ok {
				issues = append(issues, fmt.Sprintf("steps[%d]: unknown tool %q", i, step.Tool))
			}
		}
	}
	if toolSteps == 0 {
		issues = append(issues, "plan contains no tool steps — nothing to execute")
	}
	return issues
}

// planExecution is the result of running a plan's tool steps.
type planExecution struct {
	// Evidence is the formatted bundle handed to the answering agent as a
	// user message. It contains each tool's name, arguments, and raw JSON
	// result so the answering agent can cite exact values.
	Evidence string
	// Trace records every invocation for the response's ToolTrace.
	Trace []ToolInvocation
	// ToolCalls counts dispatches actually performed (cache hits included).
	ToolCalls int
	// CacheHits counts how many dispatches were served from the session cache.
	CacheHits int
}

// toolCache is the subset of SessionStore the plan executor needs. Declaring
// it as a narrow interface keeps executePlan testable without a live store
// and lets callers pass nil to disable caching entirely.
type toolCache interface {
	CachedTool(sessionID, toolName string, args map[string]any) (*ToolResult, bool)
	CacheTool(sessionID, toolName string, args map[string]any, payload json.RawMessage, digest string) error
}

// executePlan runs the plan's tool steps in order against the reader,
// collecting an evidence bundle for the answering agent.
//
// Failure policy: a step that errors is recorded in both the trace and the
// evidence bundle (as an {"error": ...} payload) and execution continues. A
// single bad step must not blind the answering agent to the evidence the
// other steps gathered — this mirrors the Bridge's log-and-continue stance.
//
// Budget: each tool step consumes one unit of remaining. When remaining hits
// zero the executor stops and notes the truncation in the evidence bundle, so
// the answering agent knows its picture is incomplete.
func executePlan(
	reader SemanticMapReader,
	p *Plan,
	remaining int,
	cache toolCache,
	sessionID string,
	emit EmitFunc,
) planExecution {
	if emit == nil {
		emit = discardEmit
	}
	var (
		b    strings.Builder
		out  planExecution
		used int
	)

	if p.Approach != "" {
		fmt.Fprintf(&b, "PLAN APPROACH: %s\n\n", p.Approach)
	}
	b.WriteString("EVIDENCE COLLECTED BY THE PLAN:\n\n")

	for i, step := range p.Steps {
		if strings.TrimSpace(step.Tool) == "" {
			// Synthesize marker — carried into the bundle as an instruction.
			if s := strings.TrimSpace(step.Synthesize); s != "" {
				fmt.Fprintf(&b, "[step %d] SYNTHESIZE: %s\n\n", i+1, s)
			}
			continue
		}
		if used >= remaining {
			fmt.Fprintf(&b, "[step %d] SKIPPED (%s): tool-call budget exhausted; evidence below is incomplete.\n\n", i+1, step.Tool)
			continue
		}
		used++
		out.ToolCalls++

		args := step.Args
		if args == nil {
			args = map[string]any{}
		}

		emit(Event{Kind: EventToolCall, Tool: step.Tool, Args: args})

		inv := ToolInvocation{Name: step.Tool, Arguments: args}
		var payload json.RawMessage

		if cache != nil && sessionID != "" {
			if hit, ok := cache.CachedTool(sessionID, step.Tool, args); ok {
				out.CacheHits++
				payload = hit.Payload
				inv.ResultDigest = hit.Digest + " (cached)"
			}
		}

		if payload == nil {
			result, err := Dispatch(reader, step.Tool, args)
			if err != nil {
				inv.Error = err.Error()
				out.Trace = append(out.Trace, inv)
				emit(Event{Kind: EventToolResult, Tool: step.Tool, Error: err.Error()})
				fmt.Fprintf(&b, "[step %d] %s(%s) → ERROR: %s\n\n", i+1, step.Tool, compactArgs(args), err.Error())
				continue
			}
			payload = result.Payload
			inv.ResultDigest = result.Digest
			if cache != nil && sessionID != "" {
				_ = cache.CacheTool(sessionID, step.Tool, args, result.Payload, result.Digest)
			}
		}

		emit(Event{Kind: EventToolResult, Tool: step.Tool, Digest: inv.ResultDigest})
		out.Trace = append(out.Trace, inv)
		fmt.Fprintf(&b, "[step %d] %s(%s) →\n%s\n\n", i+1, step.Tool, compactArgs(args), string(payload))
	}

	b.WriteString("Answer the operator's question using ONLY the evidence above. " +
		"Every value you cite must appear in one of these tool results.")
	out.Evidence = b.String()
	return out
}

// compactArgs renders a tool's arguments inline for the evidence bundle.
// Empty args render as "" so the line reads get_peers() rather than get_peers({}).
func compactArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return "?"
	}
	return string(raw)
}

// stripJSONEnvelope removes ``` fences and any prose surrounding a JSON
// object. Shared by parsePlan, parseExplainResponse, and parseCriticVerdict —
// all three consume "JSON only" responses from models that sometimes disagree.
func stripJSONEnvelope(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	if strings.HasPrefix(content, "```") {
		if idx := strings.Index(content, "\n"); idx >= 0 {
			content = content[idx+1:]
		}
		content = strings.TrimSuffix(strings.TrimSpace(content), "```")
		content = strings.TrimSpace(content)
	}
	if i := strings.Index(content, "{"); i > 0 {
		content = content[i:]
	}
	if i := strings.LastIndex(content, "}"); i >= 0 && i < len(content)-1 {
		content = content[:i+1]
	}
	return content
}
