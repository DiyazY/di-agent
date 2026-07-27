package explain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// OpenAICompatibleConfig configures OpenAICompatibleExplainer. Sensible
// defaults target a local Ollama server on port 11434 with the OpenAI-compat
// endpoint; the same shape works for llama-server, LM Studio, vLLM, and any
// other backend that exposes /v1/chat/completions with function-tool
// semantics.
type OpenAICompatibleConfig struct {
	BaseURL      string        // e.g. "http://localhost:11434/v1"
	Model        string        // e.g. "qwen2.5:7b-instruct"
	APIKey       string        // usually empty for local models; passed via Authorization if set
	SystemPrompt string        // full text of explain-v1.md (or newer)
	PromptFile   string        // path used, kept for provenance in the response
	HTTPTimeout  time.Duration // per-request timeout; default 60s

	// PlannerPrompt is the system prompt for the planning stage. When empty,
	// UsePlanner requests fall back to unplanned execution with a note in the
	// response — a missing optional prompt degrades the feature rather than
	// failing the request.
	PlannerPrompt string

	// CriticPrompt is the system prompt for the multi-agent critic. Same
	// fallback semantics as PlannerPrompt.
	CriticPrompt string

	// KeepAlive is passed to Ollama-style backends as the `keep_alive` field
	// so the model stays resident between calls. Empty means "don't send the
	// field" — backends that don't understand it are unaffected either way.
	KeepAlive string

	// DisableStructuredDecoding turns OFF the token-level JSON constraint we
	// otherwise request from backends that support it (Ollama `format`, vLLM
	// guided decoding). Named negatively so the zero value keeps the good
	// default (constrained decoding on) without a pointer or sentinel.
	DisableStructuredDecoding bool

	// Sessions is the store backing multi-turn conversations and the tool
	// result cache. Nil disables session support — requests carrying a
	// SessionID then fail with ErrSessionNotFound.
	Sessions *SessionStore
}

// OpenAICompatibleExplainer speaks the OpenAI /v1/chat/completions surface
// with function-tool semantics. It's the concrete Explainer implementation
// for local-model deployments.
type OpenAICompatibleExplainer struct {
	cfg           OpenAICompatibleConfig
	http          *http.Client
	reader        SemanticMapReader
	promptVersion string // sha256[:12] of the answering system prompt
}

// Sessions exposes the configured session store so the HTTP layer can mint
// IDs before delegating. Returns nil when session support is disabled.
func (e *OpenAICompatibleExplainer) Sessions() *SessionStore { return e.cfg.Sessions }

// NewOpenAICompatible builds an Explainer against the given reader. The
// SystemPrompt is loaded from disk once at construction time — callers must
// re-instantiate the Explainer to pick up prompt changes. This matches the
// -metrics-file / -priors-file pattern: prompt is a startup input, not a
// runtime knob.
func NewOpenAICompatible(reader SemanticMapReader, cfg OpenAICompatibleConfig) (*OpenAICompatibleExplainer, error) {
	if reader == nil {
		return nil, errors.New("explain: reader is required")
	}
	if cfg.BaseURL == "" {
		return nil, errors.New("explain: BaseURL is required (e.g. http://localhost:11434/v1)")
	}
	if cfg.Model == "" {
		return nil, errors.New("explain: Model is required")
	}
	if cfg.SystemPrompt == "" {
		return nil, errors.New("explain: SystemPrompt is required (load from PromptFile before construction)")
	}
	if cfg.HTTPTimeout <= 0 {
		// Per-request, not per-call. Must be generous enough for one slow
		// local-model turn over a long conversation; the whole-call bound is
		// ExplainBudget.Timeout, which the caller can tune per request.
		cfg.HTTPTimeout = 2 * time.Minute
	}
	sum := sha256.Sum256([]byte(cfg.SystemPrompt))
	return &OpenAICompatibleExplainer{
		cfg:           cfg,
		http:          &http.Client{Timeout: cfg.HTTPTimeout},
		reader:        reader,
		promptVersion: hex.EncodeToString(sum[:6]),
	}, nil
}

// LoadPrompt reads a system-prompt file from disk. Convenience helper for
// callers who want to construct the Explainer with a file path.
func LoadPrompt(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("explain: read prompt: %w", err)
	}
	return string(b), nil
}

// Close is a no-op — http.Client has no long-lived resources to release.
func (e *OpenAICompatibleExplainer) Close() error { return nil }

// Explain runs the reflection loop: draft → validate against live graph →
// revise until valid or budget exhausted. Progress is discarded; callers who
// want it should use ExplainStream.
func (e *OpenAICompatibleExplainer) Explain(ctx context.Context, req ExplainRequest) (*ExplainResponse, error) {
	return e.explain(ctx, req, discardEmit)
}

// ExplainStream is Explain with progress reporting. Every phase transition,
// tool dispatch, validation outcome, and critic verdict is emitted before the
// final response. Satisfies StreamingExplainer.
func (e *OpenAICompatibleExplainer) ExplainStream(ctx context.Context, req ExplainRequest, emit EmitFunc) (*ExplainResponse, error) {
	if emit == nil {
		emit = discardEmit
	}
	resp, err := e.explain(ctx, req, emit)
	switch {
	case err != nil && resp != nil:
		// Partial result: emit both so the client sees the response that
		// failed and the reason it failed.
		emit(Event{Kind: EventFinal, Response: resp})
		emit(Event{Kind: EventError, Error: err.Error()})
	case err != nil:
		emit(Event{Kind: EventError, Error: err.Error()})
	default:
		emit(Event{Kind: EventFinal, Response: resp})
	}
	return resp, err
}

func (e *OpenAICompatibleExplainer) explain(ctx context.Context, req ExplainRequest, emit EmitFunc) (*ExplainResponse, error) {
	budget := req.Budget.Defaults()
	ctx, cancel := context.WithTimeout(ctx, budget.Timeout)
	defer cancel()

	if strings.TrimSpace(req.Question) == "" {
		return nil, errors.New("explain: question is empty")
	}
	startedAt := time.Now()
	usage := Usage{}

	// ── session resolution ───────────────────────────────────────────────
	// An empty SessionID with a configured store mints a fresh session; a
	// non-empty ID must resolve or the request fails (silently minting would
	// hide a client bug behind an amnesiac conversation).
	var session *Session
	if e.cfg.Sessions != nil {
		if req.SessionID == "" {
			session = e.cfg.Sessions.Create()
		} else {
			s, err := e.cfg.Sessions.Get(req.SessionID)
			if err != nil {
				return nil, fmt.Errorf("explain: %w (id=%s)", err, req.SessionID)
			}
			session = s
		}
		e.invalidateStaleCache(session)
	} else if req.SessionID != "" {
		return nil, errors.New("explain: session_id supplied but session support is disabled on this daemon")
	}
	sessionID := ""
	if session != nil {
		sessionID = session.ID
		emit(Event{Kind: EventSession, SessionID: sessionID})
	}

	tools := buildToolsForOpenAI()

	var (
		trace          []ToolInvocation
		toolCallsSpent int
		lastResp       *ExplainResponse
		lastIssues     []string
		plan           *Plan
		lastVerdict    *CriticVerdict
		evidence       string
		// criticRejected routes the revision prompt: when the critic (not the
		// deterministic validator) is what failed the round, the answering
		// agent should see the critic's reasoning, not a citation diff.
		criticRejected bool
		// toolBudgetAnnounced ensures the "no more tools" instruction is
		// injected exactly once, not on every subsequent turn.
		toolBudgetAnnounced bool
	)

	// finish stamps every non-user-facing bookkeeping field on a candidate
	// response so both the success and partial-failure paths report the same
	// provenance shape. Called just before we return.
	finish := func(resp *ExplainResponse, iter int) {
		if resp == nil {
			return
		}
		resp.SchemaVersion = SchemaVersion
		resp.ToolTrace = trace
		resp.ModelName = e.cfg.Model
		resp.PromptVersion = e.promptVersion
		resp.Iterations = iter
		resp.Plan = plan
		resp.CriticVerdict = lastVerdict
		resp.SessionID = sessionID
		usage.ToolCalls = toolCallsSpent
		usage.WallClockMs = time.Since(startedAt).Milliseconds()
		resp.Usage = usage
		if session != nil {
			if raw, err := json.Marshal(resp); err == nil {
				_ = e.cfg.Sessions.AppendTurn(session.ID, req.Question, raw)
			}
		}
	}

	// ── build the answering agent's opening context ───────────────────────
	messages := []chatMessage{{Role: "system", Content: e.cfg.SystemPrompt}}

	// Prior turns give the answering agent conversational continuity so the
	// operator can ask follow-ups ("what about diag-2?") without restating
	// context. Bounded by the session store's MaxMessagesPerSes.
	if session != nil {
		for _, turn := range session.Turns {
			messages = append(messages,
				chatMessage{Role: "user", Content: turn.Question},
				chatMessage{Role: "assistant", Content: string(turn.Response)},
			)
		}
	}
	messages = append(messages, chatMessage{Role: "user", Content: req.Question})

	// ── planning stage (Ng Step 3) ────────────────────────────────────────
	if req.UsePlanner {
		emit(Event{Kind: EventPlanning})
		p, exec, err := e.runPlanner(ctx, req.Question, budget.MaxToolCalls-toolCallsSpent, sessionID, &usage, emit)
		switch {
		case err != nil:
			// A failed planner must not fail the request. Fall through to
			// unplanned execution and tell the answering agent why its
			// evidence bundle is missing.
			emit(Event{Kind: EventPlan, Message: "planner failed, falling back to unplanned execution", Error: err.Error()})
			messages = append(messages, chatMessage{
				Role:    "user",
				Content: "NOTE: the planning stage failed (" + err.Error() + "). Gather evidence with tool calls yourself.",
			})
		default:
			plan = p
			trace = append(trace, exec.Trace...)
			toolCallsSpent += exec.ToolCalls
			usage.ToolCacheHits += exec.CacheHits
			evidence = exec.Evidence
			messages = append(messages, chatMessage{Role: "user", Content: exec.Evidence})
		}
	}

	for iter := 1; iter <= budget.MaxIterations; iter++ {
		emit(Event{Kind: EventAnswering, Iteration: iter})
		// One full LLM turn: keep letting the model call tools until it
		// produces a final assistant message with no tool calls, or until we
		// exceed the tool-call budget.
		for {
			if err := ctx.Err(); err != nil {
				return nil, err
			}

			// Tool budget exhausted: rather than failing the request, strip
			// the tools and demand an answer from the evidence already
			// gathered. A model that loops on tool calls (small models do
			// this) still produces something citable, and the deterministic
			// validator still gates it. Erroring out here would throw away
			// perfectly good evidence.
			turnTools := tools
			if toolCallsSpent >= budget.MaxToolCalls {
				if !toolBudgetAnnounced {
					messages = append(messages, chatMessage{
						Role: "user",
						Content: fmt.Sprintf("You have used all %d permitted tool calls. "+
							"Do not request more tools. Answer now, using only the evidence already gathered. "+
							"If that evidence is insufficient, say so plainly and set confidence to \"low\".",
							budget.MaxToolCalls),
					})
					toolBudgetAnnounced = true
					emit(Event{Kind: EventAnswering, Iteration: iter, Message: "tool budget exhausted; forcing final answer"})
				}
				turnTools = nil
			}

			resp, err := e.chat(ctx, messages, turnTools)
			if err != nil {
				return nil, err
			}
			if len(resp.Choices) == 0 {
				return nil, errors.New("explain: model returned no choices")
			}
			usage.LLMTurns++
			usage.PromptTokens += resp.Usage.PromptTokens
			usage.CompletionTokens += resp.Usage.CompletionTokens
			usage.TotalTokens += resp.Usage.TotalTokens
			msg := resp.Choices[0].Message
			messages = append(messages, msg)

			if len(msg.ToolCalls) == 0 {
				// Final answer this turn — parse and validate.
				parsed, parseErr := parseExplainResponse(msg.Content)
				if parseErr != nil {
					lastIssues = []string{fmt.Sprintf("could not parse response as JSON: %v", parseErr)}
					break // fall through to revision path
				}
				// Gate 1 — deterministic validator. Structural grounding is
				// non-negotiable; a fabricated citation never reaches the
				// critic, let alone the operator.
				emit(Event{Kind: EventValidating, Iteration: iter})
				v := Validate(e.reader, parsed)
				if !v.IsValid {
					emit(Event{Kind: EventValidationFailed, Iteration: iter, Issues: v.Issues})
					lastResp = parsed
					lastIssues = v.Issues
					criticRejected = false
					break // enter revision path
				}

				// Gate 2 — multi-agent critic (Ng Step 4). Catches semantic
				// errors that survive structural validation: wrong causal
				// reading, direction-sign mistakes, unsupported conclusions,
				// miscalibrated confidence.
				if req.UseCritic {
					emit(Event{Kind: EventCritic, Iteration: iter})
					verdict, cErr := e.runCritic(ctx, req.Question, parsed, evidence, iter, &usage)
					if cErr != nil {
						// A broken critic must not block a structurally valid
						// answer. Log the degradation in the verdict and ship.
						lastVerdict = &CriticVerdict{
							Approved: true,
							Round:    iter,
							Issues:   []string{"critic unavailable: " + cErr.Error()},
						}
						finish(parsed, iter)
						return parsed, nil
					}
					lastVerdict = verdict
					emit(Event{Kind: EventCriticVerdict, Iteration: iter, Verdict: verdict})
					if !verdict.Approved {
						lastResp = parsed
						criticRejected = true
						break // enter revision path
					}
				}

				finish(parsed, iter)
				return parsed, nil
			}

			// Model asked for tools. Run each and feed the result back.
			for _, tc := range msg.ToolCalls {
				if toolCallsSpent >= budget.MaxToolCalls {
					// Budget ran out partway through a batch of tool calls.
					// The protocol requires a tool message for every call the
					// model made, so answer each remaining one with a refusal
					// rather than leaving the conversation malformed.
					messages = append(messages, chatMessage{
						Role:       "tool",
						ToolCallID: tc.ID,
						Content:    `{"error":"tool-call budget exhausted","hint":"answer from the evidence you already have"}`,
					})
					continue
				}
				toolCallsSpent++
				args := map[string]any{}
				if raw := tc.Function.Arguments; raw != "" {
					_ = json.Unmarshal([]byte(raw), &args)
				}
				emit(Event{Kind: EventToolCall, Tool: tc.Function.Name, Args: args, Iteration: iter})

				inv := ToolInvocation{Name: tc.Function.Name, Arguments: args}
				var content string

				// Session-scoped cache: an answering agent that re-asks for
				// data the planner already fetched pays nothing.
				if cached, ok := e.cachedTool(sessionID, tc.Function.Name, args); ok {
					usage.ToolCacheHits++
					inv.ResultDigest = cached.Digest + " (cached)"
					content = string(cached.Payload)
				} else if result, err := Dispatch(e.reader, tc.Function.Name, args); err != nil {
					inv.Error = err.Error()
					// Per-tool error feedback: the model sees what went wrong
					// and can correct its arguments on the next call rather
					// than abandoning the turn.
					content = fmt.Sprintf(`{"error":%q,"hint":"check the tool's required arguments and retry"}`, err.Error())
				} else {
					inv.ResultDigest = result.Digest
					content = string(result.Payload)
					e.cacheTool(sessionID, tc.Function.Name, args, result)
				}
				emit(Event{Kind: EventToolResult, Tool: tc.Function.Name, Digest: inv.ResultDigest, Iteration: iter, Error: inv.Error})
				trace = append(trace, inv)
				messages = append(messages, chatMessage{
					Role:       "tool",
					ToolCallID: tc.ID,
					Content:    content,
				})
			}
			// Continue the same turn — the LLM will see the tool results and
			// either call more tools or produce a final answer.
		}

		if iter >= budget.MaxIterations {
			break
		}
		// Hand the LLM a critique of the last response and let it revise.
		// Which critique depends on which gate failed.
		critique := FormatIssuesForLLM(lastIssues)
		if criticRejected {
			critique = FormatCriticVerdictForLLM(lastVerdict)
		}
		messages = append(messages, chatMessage{Role: "user", Content: critique})
	}

	// Reflection budget exhausted without a response that cleared both gates.
	if lastResp != nil {
		finish(lastResp, budget.MaxIterations)
		if criticRejected {
			return lastResp, fmt.Errorf("explain: critic rejected the response after %d iterations: %s",
				budget.MaxIterations, strings.Join(lastVerdict.Issues, "; "))
		}
		return lastResp, fmt.Errorf("explain: response failed validation after %d iterations: %s",
			budget.MaxIterations, strings.Join(lastIssues, "; "))
	}
	return nil, fmt.Errorf("explain: no parseable response after %d iterations", budget.MaxIterations)
}

// runPlanner executes the planning stage: one LLM turn with the planner
// system prompt, then deterministic execution of the resulting plan's tool
// steps. Returns the plan and its execution result.
//
// The planner turn is given NO tools — it emits a plan as JSON rather than
// calling anything itself. That separation is what makes the plan auditable:
// the tools that run are exactly the ones the plan named, executed by Go, not
// by the model.
func (e *OpenAICompatibleExplainer) runPlanner(
	ctx context.Context,
	question string,
	remainingToolCalls int,
	sessionID string,
	usage *Usage,
	emit EmitFunc,
) (*Plan, planExecution, error) {
	if strings.TrimSpace(e.cfg.PlannerPrompt) == "" {
		return nil, planExecution{}, errors.New("planner prompt not configured")
	}
	if remainingToolCalls <= 0 {
		return nil, planExecution{}, errors.New("no tool-call budget remains for planning")
	}

	resp, err := e.chat(ctx, []chatMessage{
		{Role: "system", Content: e.cfg.PlannerPrompt},
		{Role: "user", Content: question},
	}, nil) // nil tools — the planner writes a plan, it does not call tools
	if err != nil {
		return nil, planExecution{}, err
	}
	if len(resp.Choices) == 0 {
		return nil, planExecution{}, errors.New("planner returned no choices")
	}
	usage.LLMTurns++
	usage.PromptTokens += resp.Usage.PromptTokens
	usage.CompletionTokens += resp.Usage.CompletionTokens
	usage.TotalTokens += resp.Usage.TotalTokens

	plan, err := parsePlan(resp.Choices[0].Message.Content)
	if err != nil {
		return nil, planExecution{}, err
	}
	if issues := validatePlan(plan); len(issues) > 0 {
		return nil, planExecution{}, fmt.Errorf("plan is structurally invalid: %s", strings.Join(issues, "; "))
	}
	emit(Event{Kind: EventPlan, Plan: plan})

	var cache toolCache
	if e.cfg.Sessions != nil {
		cache = e.cfg.Sessions
	}
	exec := executePlan(e.reader, plan, remainingToolCalls, cache, sessionID, emit)
	return plan, exec, nil
}

// cachedTool looks up a session-scoped tool result. Returns (nil, false) when
// sessions are disabled or the entry is absent/expired.
func (e *OpenAICompatibleExplainer) cachedTool(sessionID, name string, args map[string]any) (*ToolResult, bool) {
	if e.cfg.Sessions == nil || sessionID == "" {
		return nil, false
	}
	return e.cfg.Sessions.CachedTool(sessionID, name, args)
}

// cacheTool stores a tool result under the session. No-op when sessions are
// disabled — the cache is a session-scoped optimisation, not a global one,
// because a shared cache would leak one operator's view into another's.
func (e *OpenAICompatibleExplainer) cacheTool(sessionID, name string, args map[string]any, result *ToolResult) {
	if e.cfg.Sessions == nil || sessionID == "" || result == nil {
		return
	}
	_ = e.cfg.Sessions.CacheTool(sessionID, name, args, result.Payload, result.Digest)
}

// runCritic executes one critic turn against a candidate answer. Like the
// planner, the critic gets no tools — it reviews the same evidence the
// answering agent saw, so its objections are always actionable.
func (e *OpenAICompatibleExplainer) runCritic(
	ctx context.Context,
	question string,
	candidate *ExplainResponse,
	evidence string,
	round int,
	usage *Usage,
) (*CriticVerdict, error) {
	if strings.TrimSpace(e.cfg.CriticPrompt) == "" {
		return nil, errors.New("critic prompt not configured")
	}
	resp, err := e.chat(ctx, []chatMessage{
		{Role: "system", Content: e.cfg.CriticPrompt},
		{Role: "user", Content: buildCriticPrompt(question, candidate, evidence)},
	}, nil)
	if err != nil {
		return nil, err
	}
	if len(resp.Choices) == 0 {
		return nil, errors.New("critic returned no choices")
	}
	usage.LLMTurns++
	usage.PromptTokens += resp.Usage.PromptTokens
	usage.CompletionTokens += resp.Usage.CompletionTokens
	usage.TotalTokens += resp.Usage.TotalTokens

	verdict, err := parseCriticVerdict(resp.Choices[0].Message.Content)
	if err != nil {
		return nil, err
	}
	verdict.Round = round
	return verdict, nil
}

// invalidateStaleCache drops a session's tool-result cache when the ontology
// has changed since the cache was populated. The watermark is the timestamp
// of the newest OntologyEvent; any mutation (tune, deprecate, strength set,
// construct/proposition added) advances it.
//
// Rationale for whole-cache invalidation over per-key: the graph is 7 nodes
// and 15 edges. Reasoning about which cached tool results a given mutation
// could have affected costs more (in code and in bug surface) than just
// refetching. If the graph grows by orders of magnitude, revisit.
func (e *OpenAICompatibleExplainer) invalidateStaleCache(session *Session) {
	if session == nil || e.cfg.Sessions == nil {
		return
	}
	events, err := e.reader.History(time.Time{})
	if err != nil || len(events) == 0 {
		return
	}
	newest := events[0].Timestamp
	for _, ev := range events[1:] {
		if ev.Timestamp.After(newest) {
			newest = ev.Timestamp
		}
	}
	e.cfg.Sessions.InvalidateOnMutation(session.ID, newest)
}

// ── OpenAI-compatible wire types ────────────────────────────────────────────

type chatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string per the OpenAI spec
}

type chatRequest struct {
	Model          string        `json:"model"`
	Messages       []chatMessage `json:"messages"`
	Tools          []any         `json:"tools,omitempty"`
	ResponseFormat any           `json:"response_format,omitempty"`
	Temperature    float64       `json:"temperature"`
	Stream         bool          `json:"stream"`
	// KeepAlive is an Ollama extension: how long to keep the model resident
	// after this request. Omitted when empty so strict OpenAI backends that
	// reject unknown fields are unaffected.
	KeepAlive string `json:"keep_alive,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Usage chatUsage `json:"usage"`
}

// chatUsage mirrors the OpenAI /v1/chat/completions usage block. Ollama's
// OpenAI-compat endpoint reports these; llama-server / LM Studio / vLLM
// generally do too. Zero when the backend does not populate them.
type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func buildToolsForOpenAI() []any {
	out := make([]any, 0, len(toolSchemas))
	for _, s := range Schemas() {
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        s.Name,
				"description": s.Description,
				"parameters":  s.Parameters,
			},
		})
	}
	return out
}

func (e *OpenAICompatibleExplainer) chat(ctx context.Context, messages []chatMessage, tools []any) (*chatResponse, error) {
	body := chatRequest{
		Model:       e.cfg.Model,
		Messages:    messages,
		Tools:       tools,
		Temperature: 0, // deterministic decoding when the backend honors it
		Stream:      false,
		KeepAlive:   e.cfg.KeepAlive,
	}
	// Token-level JSON constraint where the backend supports it. When tools
	// are in play we must NOT constrain output to a JSON object — the model
	// needs to be free to emit a tool_calls message instead. Structured
	// decoding therefore applies only to tool-free turns (planner, critic,
	// and the final answering turn once evidence is gathered).
	if !e.cfg.DisableStructuredDecoding && len(tools) == 0 {
		body.ResponseFormat = map[string]string{"type": "json_object"}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(e.cfg.BaseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if e.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+e.cfg.APIKey)
	}
	resp, err := e.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("explain: LLM transport: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("explain: LLM returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var out chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("explain: decode LLM response: %w", err)
	}
	return &out, nil
}

// parseExplainResponse extracts the JSON object described by explain-v1.md
// from the model's assistant message. Tolerant of ```json fences and leading
// prose, both of which some local models emit despite the prompt asking for
// pure JSON.
func parseExplainResponse(content string) (*ExplainResponse, error) {
	content = stripJSONEnvelope(content)
	if content == "" {
		return nil, errors.New("assistant message is empty")
	}
	var out ExplainResponse
	if err := json.Unmarshal([]byte(content), &out); err != nil {
		return nil, err
	}
	if out.Confidence == "" {
		out.Confidence = "medium"
	}
	return &out, nil
}
