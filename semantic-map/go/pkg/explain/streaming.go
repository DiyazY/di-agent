package explain

import "context"

// Event is one progress marker emitted during a streaming Explain call.
// Events are serialised as NDJSON — one compact JSON object per line — so a
// client can consume them with a line-oriented reader and react before the
// final answer lands.
//
// Only the fields relevant to a given Kind are populated; the rest are
// omitted from the wire form.
type Event struct {
	Kind EventKind `json:"event"`

	SessionID string `json:"session_id,omitempty"`
	Iteration int    `json:"iteration,omitempty"`

	// Tool-related.
	Tool   string         `json:"tool,omitempty"`
	Args   map[string]any `json:"args,omitempty"`
	Digest string         `json:"digest,omitempty"`

	// Plan / verdict payloads.
	Plan    *Plan          `json:"plan,omitempty"`
	Verdict *CriticVerdict `json:"verdict,omitempty"`

	// Issues carries validator or critic objections.
	Issues []string `json:"issues,omitempty"`

	// Response is populated only on EventFinal.
	Response *ExplainResponse `json:"response,omitempty"`

	// Error is populated only on EventError.
	Error string `json:"error,omitempty"`

	// Message is a short human-readable note. Optional on any event.
	Message string `json:"message,omitempty"`
}

// EventKind enumerates the streaming progress vocabulary. Clients should
// tolerate unknown kinds — new markers may be added without a schema bump,
// since every stream still terminates in EventFinal or EventError.
type EventKind string

const (
	// EventSession fires once when a session is resolved or minted.
	EventSession EventKind = "session"
	// EventPlanning fires when the planner turn starts.
	EventPlanning EventKind = "planning"
	// EventPlan carries the parsed plan once the planner returns. Its Plan
	// field is always non-nil — a planner that failed emits EventPlanFailed
	// instead, so a consumer switching on EventPlan can dereference safely.
	EventPlan EventKind = "plan"
	// EventPlanFailed reports that planning did not produce a usable plan.
	// The request continues without one; Error carries the reason.
	EventPlanFailed EventKind = "plan_failed"
	// EventToolCall fires immediately before a tool is dispatched.
	EventToolCall EventKind = "tool_call"
	// EventToolResult fires after a tool returns, carrying its digest.
	EventToolResult EventKind = "tool_result"
	// EventAnswering fires at the start of each answering-agent iteration.
	EventAnswering EventKind = "answering"
	// EventValidating fires when the deterministic validator runs.
	EventValidating EventKind = "validating"
	// EventValidationFailed carries the validator's objections.
	EventValidationFailed EventKind = "validation_failed"
	// EventCritic fires when the critic turn starts.
	EventCritic EventKind = "critic"
	// EventCriticVerdict carries the critic's verdict.
	EventCriticVerdict EventKind = "critic_verdict"
	// EventFinal carries the completed response. Always the last event on a
	// successful stream.
	EventFinal EventKind = "final"
	// EventError terminates a stream that could not produce a response.
	EventError EventKind = "error"
)

// EmitFunc receives progress events. Implementations must be safe to call
// from the goroutine running Explain and should not block for long — a slow
// emitter directly stalls the explain loop.
type EmitFunc func(Event)

// discardEmit is the no-op emitter used by the non-streaming path.
func discardEmit(Event) {}

// StreamingExplainer is implemented by Explainers that can report progress.
// The HTTP layer type-asserts on this interface, so an Explainer that only
// implements the base contract still works — it just cannot stream.
//
// Kept as a separate optional interface rather than widening Explainer so
// DisabledExplainer and any future minimal implementation stay trivial.
type StreamingExplainer interface {
	Explainer
	ExplainStream(ctx context.Context, req ExplainRequest, emit EmitFunc) (*ExplainResponse, error)
}
