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
}

// OpenAICompatibleExplainer speaks the OpenAI /v1/chat/completions surface
// with function-tool semantics. It's the concrete Explainer implementation
// for local-model deployments.
type OpenAICompatibleExplainer struct {
	cfg           OpenAICompatibleConfig
	http          *http.Client
	reader        SemanticMapReader
	promptVersion string // sha256[:12] of the system prompt for provenance
}

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
		cfg.HTTPTimeout = 60 * time.Second
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
// revise until valid or budget exhausted.
func (e *OpenAICompatibleExplainer) Explain(ctx context.Context, req ExplainRequest) (*ExplainResponse, error) {
	budget := req.Budget.Defaults()
	ctx, cancel := context.WithTimeout(ctx, budget.Timeout)
	defer cancel()

	if strings.TrimSpace(req.Question) == "" {
		return nil, errors.New("explain: question is empty")
	}
	startedAt := time.Now()
	usage := Usage{}

	messages := []chatMessage{
		{Role: "system", Content: e.cfg.SystemPrompt},
		{Role: "user", Content: req.Question},
	}
	tools := buildToolsForOpenAI()

	var (
		trace          []ToolInvocation
		toolCallsSpent int
		lastResp       *ExplainResponse
		lastIssues     []string
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
		usage.ToolCalls = toolCallsSpent
		usage.WallClockMs = time.Since(startedAt).Milliseconds()
		resp.Usage = usage
	}

	for iter := 1; iter <= budget.MaxIterations; iter++ {
		// One full LLM turn: keep letting the model call tools until it
		// produces a final assistant message with no tool calls, or until we
		// exceed the tool-call budget.
		for {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if toolCallsSpent >= budget.MaxToolCalls {
				return nil, fmt.Errorf("explain: exceeded MaxToolCalls=%d", budget.MaxToolCalls)
			}

			resp, err := e.chat(ctx, messages, tools)
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
				v := Validate(e.reader, parsed)
				if v.IsValid {
					finish(parsed, iter)
					return parsed, nil
				}
				lastResp = parsed
				lastIssues = v.Issues
				break // enter revision path
			}

			// Model asked for tools. Run each and feed the result back.
			for _, tc := range msg.ToolCalls {
				if toolCallsSpent >= budget.MaxToolCalls {
					return nil, fmt.Errorf("explain: exceeded MaxToolCalls=%d mid-turn", budget.MaxToolCalls)
				}
				toolCallsSpent++
				args := map[string]any{}
				if raw := tc.Function.Arguments; raw != "" {
					_ = json.Unmarshal([]byte(raw), &args)
				}
				result, err := Dispatch(e.reader, tc.Function.Name, args)
				inv := ToolInvocation{Name: tc.Function.Name, Arguments: args}
				var content string
				if err != nil {
					inv.Error = err.Error()
					content = fmt.Sprintf(`{"error":%q}`, err.Error())
				} else {
					inv.ResultDigest = result.Digest
					content = string(result.Payload)
				}
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
		critique := FormatIssuesForLLM(lastIssues)
		messages = append(messages, chatMessage{Role: "user", Content: critique})
	}

	// Reflection budget exhausted without a valid response.
	if lastResp != nil {
		finish(lastResp, budget.MaxIterations)
		return lastResp, fmt.Errorf("explain: response failed validation after %d iterations: %s", budget.MaxIterations, strings.Join(lastIssues, "; "))
	}
	return nil, fmt.Errorf("explain: no parseable response after %d iterations", budget.MaxIterations)
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
		// Ask for JSON output when the backend supports it. Many local
		// backends ignore this hint; the prompt itself is the real guard.
		ResponseFormat: map[string]string{"type": "json_object"},
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
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, errors.New("assistant message is empty")
	}
	// Strip ``` fences if present.
	if strings.HasPrefix(content, "```") {
		if idx := strings.Index(content, "\n"); idx >= 0 {
			content = content[idx+1:]
		}
		content = strings.TrimSuffix(content, "```")
		content = strings.TrimSpace(content)
	}
	// Trim any leading non-JSON prose.
	if i := strings.Index(content, "{"); i > 0 {
		content = content[i:]
	}
	// Trim trailing prose.
	if i := strings.LastIndex(content, "}"); i >= 0 && i < len(content)-1 {
		content = content[:i+1]
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
