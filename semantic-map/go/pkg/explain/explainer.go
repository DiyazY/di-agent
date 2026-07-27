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
type ExplainRequest struct {
	Question string        `json:"question"`
	Budget   ExplainBudget `json:"budget,omitempty"`
}

// ExplainBudget bounds an Explain call so a runaway loop can't burn tokens
// indefinitely. Zero values fall back to package defaults.
type ExplainBudget struct {
	MaxIterations int           `json:"max_iterations,omitempty"` // reflection cap; default 3
	MaxToolCalls  int           `json:"max_tool_calls,omitempty"` // total across the session; default 10
	Timeout       time.Duration `json:"timeout,omitempty"`        // wall-clock; default 60s
}

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
		b.Timeout = 60 * time.Second
	}
	return b
}

// ExplainResponse carries the LLM's grounded answer plus everything a
// reviewer needs to verify or reject that answer.
type ExplainResponse struct {
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

	// ModelName + PromptVersion + Iterations pin the response to a
	// reproducible provenance triple. Callers can re-run against the same
	// pair and see whether the model's behaviour has drifted.
	ModelName     string `json:"model_name"`
	PromptVersion string `json:"prompt_version"`
	Iterations    int    `json:"iterations"`
}

// Citation identifies one live graph element the answer references. The
// validator confirms that each citation exists and that the recorded values
// match the current graph state within a small epsilon.
type Citation struct {
	Kind          string  `json:"kind"` // "edge"|"proposition"|"peer"|"event"|"construct"
	ID            string  `json:"id"`   // proposition_id / peer_id / construct_id / event_target
	EMAWeight     float64 `json:"ema_weight,omitempty"`
	PriorWeight   float64 `json:"prior_weight,omitempty"`
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
