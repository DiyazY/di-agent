package autonomy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// openAI is a minimal client for the OpenAI-compatible chat completions surface,
// shared by the backends that speak it.
type openAI struct {
	url    string // the base, ending in /v1
	model  string
	apiKey string
	http   *http.Client
}

func newOpenAI(s Spec, timeout time.Duration) *openAI {
	return &openAI{url: strings.TrimRight(s.URL, "/"), model: s.Model, apiKey: s.APIKey,
		http: &http.Client{Timeout: timeout}}
}

type chatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Tools       []any         `json:"tools,omitempty"`
	Temperature float64       `json:"temperature"`
	Stream      bool          `json:"stream"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Logprobs    bool          `json:"logprobs,omitempty"`
	TopLogprobs int           `json:"top_logprobs,omitempty"`
	// ReasoningEffort asks a thinking model not to think ("none") where the backend
	// honours it: a one-token answer must be the answer, not the start of a thought.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	KeepAlive       string `json:"keep_alive,omitempty"` // Ollama extension; omitted when empty
}

type tokenLogprob struct {
	Token   string  `json:"token"`
	Logprob float64 `json:"logprob"`
}

type chatResponse struct {
	Choices []struct {
		Message  chatMessage `json:"message"`
		Logprobs *struct {
			Content []struct {
				tokenLogprob
				TopLogprobs []tokenLogprob `json:"top_logprobs"`
			} `json:"content"`
		} `json:"logprobs"`
	} `json:"choices"`
}

func (c *openAI) chat(ctx context.Context, body chatRequest) (*chatResponse, error) {
	body.Model = c.model
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", c.model, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", c.model, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &statusError{model: c.model, code: resp.StatusCode, body: strings.TrimSpace(string(b))}
	}
	var out chatResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("%s: decode: %w", c.model, err)
	}
	if len(out.Choices) == 0 {
		return nil, errors.New(c.model + " returned no choices")
	}
	return &out, nil
}

// statusError is a response other than 200, kept whole so a caller can tell a
// request the server refused (400) from one it could not serve.
type statusError struct {
	model string
	code  int
	body  string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("%s returned %d: %s", e.model, e.code, e.body)
}

// refused reports whether err is the server rejecting a request field it names.
func refused(err error, field string) bool {
	var se *statusError
	return errors.As(err, &se) && se.code == http.StatusBadRequest && strings.Contains(se.body, field)
}
