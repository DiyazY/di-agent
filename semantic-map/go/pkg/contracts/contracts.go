// Package contracts defines the interface contracts of the Semantic Map:
// Collector, Ontology, Reasoner, Proposer and Tuner.
//
// Behavioral guarantees are documented on each interface. All implementations
// must satisfy these guarantees and pass the shared compliance suite in
// github.com/DiyazY/di-agent/compliance.
//
// Storage and Updater used to be here as well, and they described the second model:
// a graph of construct edges with its own estimator, kept beside the state model and
// learning the same relations from the same telemetry. The state model in
// pkg/statemap holds the properties, the relationships and the estimator, so the two
// contracts had no subject left — a StorageContract implementation would be a place
// to put numbers nothing reads.
package contracts

import (
	"time"

	"github.com/DiyazY/di-agent/pkg/types"
)

// ── Ontology ──────────────────────────────────────────────────────────────────

// OntologyContract answers structural questions about constructs and
// propositions and supports the live-ontology lifecycle: priors get
// recalibrated, the Proposer discovers new propositions, operators deprecate
// stale ones, and new domains may add constructs.
//
// Guarantees:
//   - Non-empty: Constructs() and Propositions() are both non-empty, with every
//     proposition endpoint a declared construct. The counts follow the loaded
//     domain specification; the contract fixes no particular scope.
//   - Soft-delete only: existing propositions are never structurally removed
//     and their Direction never reverses. Deprecate marks a proposition as
//     no-longer-endorsed but keeps it in Propositions() for history/replay.
//     Reasoners must skip deprecated propositions during cost computation.
//   - Append-only constructs: AddConstruct is supported but constructs are
//     never removed (they are domain-stable per the architecture).
//   - Pure query: ValidateProposition, Constructs, Propositions,
//     Relationships, GetHistory never modify state.
//   - Audit log: every mutation (SetPropositionStrength,
//     AddValidatedProposition, AddConstruct, Deprecate) appends one
//     OntologyEvent that GetHistory exposes in chronological insertion order.
//     RecordTune emits an "operator-tune" audit event consolidating a
//     batch of operator-driven strength adjustments.
//   - Implementations that intentionally do not support a mutation (e.g. a
//     truly static cloud-cache implementation) return ErrNotImplemented
//     from that method rather than silently succeeding.
type OntologyContract interface {
	// Read surface.
	Constructs() ([]*types.Construct, error)
	Propositions() ([]*types.Proposition, error)
	Relationships(constructID string) ([]*types.Proposition, error)
	ValidateProposition(fromID, toID string, dir types.Direction) (*types.ValidationResult, error)

	// Write surface — the "live" mutations. Each emits one OntologyEvent.
	AddValidatedProposition(p *types.Proposition) error
	SetPropositionStrength(propositionID string, strength float64) error
	AddConstruct(c *types.Construct) error
	Deprecate(propositionID, reason string) error

	// Audit. Returns events appended at or after `since`; pass zero time to
	// retrieve the full history. Order is chronological by insertion.
	GetHistory(since time.Time) ([]*types.OntologyEvent, error)

	// RecordTune appends a consolidated "operator-tune" event to the audit log
	// without modifying any proposition strength. It records the operator's
	// intent string alongside the proposition IDs that were adjusted in the
	// same batch. Implementations that cannot record return nil (best-effort;
	// never blocks Tune).
	RecordTune(text, operator string, appliedIDs []string) error
}

// ErrNotImplemented is returned by an OntologyContract implementation that
// intentionally does not support a particular mutation in its profile.
var ErrNotImplemented = contractError("operation not implemented by this ontology profile")

// ── Reasoner ──────────────────────────────────────────────────────────────────

// ReasonerContract produces agent decisions with traceable rationales.
//
// Guarantees:
//   - Traceable rationale: every returned value includes a non-empty Rationale
//     string referencing specific node/edge IDs. Implementations that cannot
//     produce a rationale must return ErrNoRationale.
//   - Pure simulation: SimulateOutcome never writes to Storage or any contract.
//   - Trust filtering: RecommendPeer never returns a peer below the minimum
//     trust threshold; returns ErrInsufficientTrust if no peer qualifies.
type ReasonerContract interface {
	CostOfAction(taskType, nodeID string) (*types.ActionCost, error)
	RecommendPeer(ctx *types.OffloadContext) (*types.PeerRecommendation, error)
	SimulateOutcome(ctx *types.OffloadContext, targetNodeID string) (*types.OutcomeSimulation, error)
}

// Sentinel errors returned by ReasonerContract implementations.
var (
	ErrNoRationale       = contractError("reasoner must provide a non-empty rationale")
	ErrInsufficientTrust = contractError("no peer meets the minimum trust threshold")
)

// ── Proposer ──────────────────────────────────────────────────────────────────

// ProposerContract detects statistical patterns suggesting new backbone edges.
//
// The natural entry point from the Bridge and IngestSample is ObserveConstruct,
// which feeds a single construct value and internally pairs it against every
// other construct the proposer has seen. Observe remains public for callers
// (tests, control-surface HTTP handlers) that already know the pair to feed.
//
// Guarantees:
//   - Read-only observation: Observe and ObserveConstruct never modify Storage
//     or Ontology.
//   - Confirm delegates: Confirm calls OntologyContract.AddValidatedProposition;
//     it never writes to Storage directly.
//   - Permanent suppression: after Reject, the same (fromID, toID, direction)
//     triple is not re-proposed within the current deployment session.
//   - Candidates: GetCandidates returns only Pending entries.
type ProposerContract interface {
	Observe(fromID, toID string, valueA, valueB float64) error
	// ObserveConstruct records the latest value observed for a single construct.
	// The proposer internally pairs construct values across its latestValues map
	// so callers (Bridge, IngestSample) need not know which pairs to supply.
	ObserveConstruct(constructID string, value float64) error
	GetCandidates() ([]*types.CandidateEdge, error)
	Confirm(candidateID string) error
	Reject(candidateID string) error
	Defer(candidateID string) error
	GetHistory() ([]*types.CandidateEdge, error)
}

// ── Tuner ─────────────────────────────────────────────────────────────────────

// TunerContract maps operator natural-language intent to validated proposition
// strength adjustments. The parser is pluggable — v1 uses a rule-based
// implementation; a richer profile may substitute an SLM (Phi-3 Mini,
// Gemma 2B) without changing the contract or the wiring downstream.
//
// The Tuner is never in the execution path. It preprocesses intent text into
// structured TuneIntents; SemanticMap.Tune validates and applies them via
// SetPropositionStrength + RecordTune.
//
// Guarantees:
//   - ParseIntent is a pure function: it never modifies the graph.
//   - ParseIntent returns (empty, nil) for unrecognized or ambiguous text;
//     it never returns an error on well-formed input.
//   - Validate is stateless: it checks hard bounds only, not current values.
//   - Validate returns nil iff every TuneAdjustment.NewStrength is within the
//     allowed bounds for its proposition. Otherwise it returns a descriptive
//     error listing every violation.
type TunerContract interface {
	ParseIntent(text string) ([]*types.TuneIntent, error)
	Validate(adjustments []*types.TuneAdjustment) error
}

// ── Collector ─────────────────────────────────────────────────────────────────

// CollectorContract reads raw metrics from a source and emits normalized samples.
//
// The collector sits between a metric source (cgroup filesystem, Netdata HTTP API,
// kubelet /metrics, etc.) and the Updater. It normalizes observations into
// MetricSamples. It knows nothing about the graph topology — that mapping is
// the bridge's responsibility.
//
// Guarantees:
//   - Pure read: Collect never modifies any system state.
//   - Empty on no data: Collect returns ([], nil) when no new samples are ready;
//     it never returns a non-nil error for a temporarily unavailable source.
//   - Deterministic EventID: the same physical observation always produces the
//     same EventID, enabling end-to-end idempotency with the Updater.
//   - Metric type stability: AvailableMetrics returns the same set for the
//     entire lifetime of the instance; Collect never emits a MetricType outside it.
//   - Node ID completeness: every emitted MetricSample has a non-empty NodeID.
//   - SourceID stability: SourceID returns the same string across restarts.
type CollectorContract interface {
	// Collect reads one batch of current metric samples from the source.
	// Returns an empty slice (not an error) when no new data is available.
	Collect() ([]*types.MetricSample, error)

	// SourceID returns a stable identifier for this collector instance.
	// Used as a component in EventID generation.
	SourceID() string

	// AvailableMetrics returns the metric types this implementation can produce.
	// The returned slice is static for the lifetime of the instance.
	AvailableMetrics() []types.MetricType
}

// ── helpers ───────────────────────────────────────────────────────────────────

type contractError string

func (e contractError) Error() string { return string(e) }
