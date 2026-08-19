// Package explain implements a natural-language operator interface that grounds
// its answers in the live semantic-map graph.
//
// The Explainer treats the graph as the world model (in Ng's sense — see
// ARCHITECTURE.md §8 "Theoretical framing"). It exposes read-only tools over
// the SemanticMap facade to a local LLM, runs a reflection loop with
// deterministic citation validation, and returns a structured response
// containing an English answer, cited edges/propositions/peers, and an
// optional draft Proposal that an operator can act on via existing mutation
// endpoints. The Explainer NEVER mutates state — draft proposals stay drafts
// until an operator confirms them.
//
// Design choice (v1): concrete, not contract, following the pkg/peers
// precedent. When a second implementation (e.g. hosted API) is required, we
// promote the surface to pkg/contracts at that point. Keeping the contract
// set at six ({Storage, Ontology, Updater, Reasoner, Proposer, Tuner,
// Collector}) preserves the "no new contract without a second implementation"
// discipline documented in ARCHITECTURE.md §2.
//
// Reproducibility stance: the Explainer's outputs are non-deterministic (the
// LLM is a black box), but its inputs and validation are fully deterministic.
// A response records the ModelName, PromptVersion, and ToolTrace so that a
// disagreement can be attributed to the model rather than to the graph. P6
// results do not depend on any Explainer output — the Explainer sits on the
// operator-facing surface, not on the ingestion or reasoning path.
package explain

import (
	"context"
	"errors"
	"time"
)

// ExplainRequest is the input to Explainer.Explain. Callers phrase a question
// in natural language; the Explainer decides which tools to invoke.
//
// All fields beyond Question and Budget are v2 opt-in features. Leaving them
// zero produces v1 behaviour (single answering turn + deterministic validator).
type ExplainRequest struct {
	Question string        `json:"question"`
	Budget   ExplainBudget `json:"budget,omitempty"`

	// SessionID opts into session memory. Pass "" to start a fresh
	// conversation (the Explainer will mint a new ID and return it), or an
	// existing ID to continue. Missing IDs return ErrSessionNotFound rather
	// than silently minting — a misconfigured client should fail loudly.
	SessionID string `json:"session_id,omitempty"`

	// UsePlanner turns on Ng's planning stage: an extra LLM turn produces a
	// structured Plan before tool execution. Costs one LLM turn; buys plan
	// inspectability and better tool selection.
	UsePlanner bool `json:"use_planner,omitempty"`

	// UseCritic turns on the multi-agent critic: a second LLM instance
	// reviews the primary agent's answer against the graph and the original
	// question. Costs one LLM turn per reflection round.
	UseCritic bool `json:"use_critic,omitempty"`

	// Stream, when true, changes the HTTP response shape to NDJSON-over-
	// chunked-encoding with per-event progress markers. See streaming.go
	// for the event vocabulary.
	Stream bool `json:"stream,omitempty"`
}

// ExplainBudget bounds an Explain call so a runaway loop can't burn tokens
// indefinitely. Zero values fall back to package defaults.
//
// Timeout covers the WHOLE call — planner turn, every tool dispatch, every
// answering iteration, and every critic turn. The default is sized for a
// local quantised 7B model doing a planner + answer + critic sequence over a
// growing conversation, which is several times slower than a single hosted
// API round-trip. Operators on faster backends can tighten it per request.
type ExplainBudget struct {
	MaxIterations int           `json:"max_iterations,omitempty"` // reflection cap; default 3
	MaxToolCalls  int           `json:"max_tool_calls,omitempty"` // total across the call; default 10
	Timeout       time.Duration `json:"timeout,omitempty"`        // whole-call wall clock; default 5m
}

// DefaultExplainTimeout bounds a full Explain call. Measured against
// qwen2.5:7b-instruct (Q4_K_M) on Apple Silicon: a planner + answer + critic
// sequence runs ~15s warm, but a tool-looping model over an accumulated
// context can reach several minutes before the forced-answer path kicks in.
const DefaultExplainTimeout = 5 * time.Minute

// Defaults returns a budget with the package defaults populated. Called by
// implementations to normalize a caller-provided budget.
func (b ExplainBudget) Defaults() ExplainBudget {
	if b.MaxIterations <= 0 {
		b.MaxIterations = 3
	}
	if b.MaxToolCalls <= 0 {
		b.MaxToolCalls = 10
	}
	if b.Timeout <= 0 {
		b.Timeout = DefaultExplainTimeout
	}
	return b
}

// ExplainResponse carries the LLM's grounded answer plus everything a
// reviewer needs to verify or reject that answer.
type ExplainResponse struct {
	// SchemaVersion pins the wire format. Bumped when fields are added or
	// renamed in a way callers must branch on. See explain.SchemaVersion.
	SchemaVersion string `json:"schema_version"`

	// Answer is the natural-language response, ≤300 words by prompt policy.
	Answer string `json:"answer"`

	// Citations enumerates every graph element the answer references. The
	// validator rejects an iteration if any citation is fabricated or its
	// values do not match live graph state.
	Citations []Citation `json:"citations"`

	// Confidence is a self-reported estimate from the LLM. Not authoritative;
	// callers should treat it as a hint, not a probability.
	Confidence string `json:"confidence"` // "high"|"medium"|"low"

	// Proposal is populated when the operator asked "what should I do?" and
	// the LLM produced a structured action draft (tune / deprecate / reset /
	// strength). Nil for pure read queries. The operator must invoke the
	// suggested Endpoint explicitly — the Explainer never mutates state.
	Proposal *Proposal `json:"proposal,omitempty"`

	// ToolTrace records every tool the LLM invoked in the order invoked.
	// Useful for debugging and for the audit-trail story in ARCHITECTURE.md.
	ToolTrace []ToolInvocation `json:"tool_trace"`

	// Plan is the structured pre-execution plan produced by the planner
	// agent when Planning was enabled. Nil when the caller opted out of the
	// planning stage.
	Plan *Plan `json:"plan,omitempty"`

	// CriticVerdict summarises the critic agent's review when Multi-agent
	// verify was enabled. Nil when the caller opted out.
	CriticVerdict *CriticVerdict `json:"critic_verdict,omitempty"`

	// SessionID identifies the conversation this turn belongs to. Empty when
	// the caller did not opt into session memory.
	SessionID string `json:"session_id,omitempty"`

	// Usage reports the resource cost of producing this response so callers
	// can enforce budgets and the paper's replication package can report
	// per-turn costs.
	Usage Usage `json:"usage"`

	// ModelName + PromptVersion + Iterations pin the response to a
	// reproducible provenance triple. Callers can re-run against the same
	// pair and see whether the model's behaviour has drifted.
	ModelName     string `json:"model_name"`
	PromptVersion string `json:"prompt_version"`
	Iterations    int    `json:"iterations"`
}

// Usage accounts for the cost of producing an ExplainResponse. Token counts
// come from the LLM backend's response headers when available; zero means the
// backend did not report them (some local models do not).
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// WallClockMs measures the /explain call end-to-end from server accept to
	// response ready, including tool dispatch and validation.
	WallClockMs int64 `json:"wall_clock_ms"`
	// ToolCalls counts every tool invocation, including cache hits. A high
	// ToolCalls with a low PromptTokens delta means the cache is working.
	ToolCalls int `json:"tool_calls"`
	// ToolCacheHits counts how many of ToolCalls were served from the
	// session-scoped result cache. Reported separately so operators can see
	// the cache's effect directly.
	ToolCacheHits int `json:"tool_cache_hits"`
	// LLMTurns counts distinct chat/completions round-trips (planner turns,
	// answer turns, critic turns, revision turns all add up).
	LLMTurns int `json:"llm_turns"`
}

// Plan is the structured pre-execution plan produced by the Planner agent.
// Every step is either a Tool call (executed by the tool registry) or a
// Synthesize marker (informational only — the answering agent uses the
// preceding tool evidence to produce the final answer).
type Plan struct {
	Steps    []PlanStep `json:"steps"`
	Approach string     `json:"approach,omitempty"` // one-line prose summary
}

// PlanStep is one entry in Plan.Steps. Exactly one of Tool or Synthesize
// must be non-empty.
type PlanStep struct {
	Tool       string         `json:"tool,omitempty"`
	Args       map[string]any `json:"args,omitempty"`
	Synthesize string         `json:"synthesize,omitempty"`
	// Rationale records why the planner chose this step. Optional but
	// operators find it useful when auditing why the plan was structured
	// the way it was.
	Rationale string `json:"rationale,omitempty"`
}

// CriticVerdict summarises the critic agent's review of a candidate answer.
type CriticVerdict struct {
	Approved bool     `json:"approved"`
	Issues   []string `json:"issues,omitempty"`
	// SuggestedRevision is a short imperative note the critic hands to the
	// answering agent when Approved=false. Feeds directly into the reflection
	// prompt on the next revision.
	SuggestedRevision string `json:"suggested_revision,omitempty"`
	// Round records which reflection round this verdict came from (1-indexed).
	Round int `json:"round"`
}

// Citation identifies one live graph element the answer references. The
// validator confirms that each citation exists and that the recorded values
// match the current graph state within a small epsilon.
type Citation struct {
	Kind string `json:"kind"` // "property"|"edge"|"proposition"|"peer"|"event"|"construct"
	ID   string `json:"id"`   // property / proposition_id / peer_id / construct_id / event_target

	// Value is the cited value of a property, checked against the state model — the
	// model the agent reasons from. Citing a property is the form an answer about
	// what the system is currently doing should take.
	Value     float64 `json:"value,omitempty"`
	EMAWeight float64 `json:"ema_weight,omitempty"`

	// Established and Effective are pointers so that citing a relationship the map has
	// no strength for is distinguishable from citing one whose strength is zero. An
	// answer that names a number the map does not hold is the failure this validator
	// exists to catch, and the old float form made it unrepresentable.
	Established *float64 `json:"established,omitempty"`
	Effective   *float64 `json:"effective,omitempty"`

	Confidence    float64 `json:"confidence,omitempty"`
	NObservations int     `json:"n_observations,omitempty"`
	Trust         float64 `json:"trust,omitempty"`      // peers only
	Timestamp     string  `json:"timestamp,omitempty"`  // events only, RFC3339
	Deprecated    bool    `json:"deprecated,omitempty"` // edges/props only
}

// Proposal is a draft action the LLM suggests. The Explainer never applies it
// — the operator invokes Endpoint (via the daemon's mutation surface) if they
// choose to act on the suggestion.
type Proposal struct {
	Kind      string         `json:"kind"`      // "tune"|"deprecate"|"reset"|"strength"
	Endpoint  string         `json:"endpoint"`  // e.g. "POST /ontology/deprecate"
	Payload   map[string]any `json:"payload"`   // JSON body the operator would POST
	Rationale string         `json:"rationale"` // 1-2 sentence justification
}

// ToolInvocation records one round-trip: what the LLM asked for and what the
// tool returned.
type ToolInvocation struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
	// ResultDigest is a short summary of the tool response (not the full
	// payload) to keep the trace bounded.
	ResultDigest string `json:"result_digest"`
	Error        string `json:"error,omitempty"`
}

// Explainer is the operator-facing natural-language surface.
type Explainer interface {
	Explain(ctx context.Context, req ExplainRequest) (*ExplainResponse, error)
	// Close releases any long-lived resources (HTTP connections, local model
	// handles). Safe to call multiple times.
	Close() error
}

// ErrNotEnabled is returned by DisabledExplainer.Explain and by the
// POST /explain handler when -explain-provider is "none".
var ErrNotEnabled = errors.New("explain: explainer not enabled — start the daemon with -explain-provider=openai-compatible and a reachable model endpoint")
