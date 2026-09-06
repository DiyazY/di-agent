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

// OntologyContract is the declaration layer: which constructs exist, which
// propositions relate them, and whether a proposed new one is structurally valid.
//
// It holds no magnitudes and no history, and that boundary is the whole point of the
// interface. A proposition's strength lives on the state model's relationship for it,
// where it is blended with what this system has shown and read by every answer; a
// second copy here would be a number the agent does not use, kept in step by hand.
// The same applies to history: the state model's journal records every mutation with
// actor and reason, and a second log would be another record to reconcile. Both used
// to be here, and §7.3 of the P6 paper documents the defect from the period when the
// strength this layer exposed was not the strength in force.
//
// What it does own is the vocabulary. The specification declares no prior strengths at
// all, so there is nothing numeric for this layer to be authoritative about — the
// declared set, the causal directions, and the validation rules are what it answers
// for, and those are exactly what a caller needs before it can name anything.
//
// Guarantees:
//   - Non-empty: Constructs() and Propositions() are both non-empty, with every
//     proposition endpoint a declared construct. The counts follow the loaded
//     domain specification; the contract fixes no particular scope.
//   - No removal, no reversal: a proposition is never structurally removed and its
//     Direction never reverses. Withdrawal is retirement in the state model, which
//     is what takes a claim out of reasoning; Propositions() keeps reporting it so
//     history and replay stay intact.
//   - Append-only constructs: AddConstruct is supported but constructs are never
//     removed (they are domain-stable per the architecture).
//   - Pure query: ValidateProposition, Constructs, Propositions and Relationships
//     never modify state.
//   - Additions are declarations, not decisions: AddConstruct and
//     AddValidatedProposition extend the vocabulary. On their own they change no
//     answer — the facade declares the matching property or relationship in the
//     state model in the same call, and that is the half that has an effect.
//   - Implementations that intentionally do not support an addition (a static
//     read-only profile, say) return ErrNotImplemented rather than silently
//     succeeding.
type OntologyContract interface {
	// Read surface.
	Constructs() ([]*types.Construct, error)
	Propositions() ([]*types.Proposition, error)
	Relationships(constructID string) ([]*types.Proposition, error)
	ValidateProposition(fromID, toID string, dir types.Direction) (*types.ValidationResult, error)

	// Vocabulary extension. Each is paired by the facade with a declaration in the
	// state model; neither has an effect on any answer by itself.
	AddValidatedProposition(p *types.Proposition) error
	AddConstruct(c *types.Construct) error
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
//   - Pure simulation: SimulateOutcome never writes to the state model or any contract.
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

// RelationshipLookup answers whether a relationship already runs from -> to with a
// sign, so a proposer does not re-propose what the model already holds. The state
// map implements it (a retired relationship does not count); LookupOntology adapts
// the declaration layer for callers that pair constructs explicitly.
type RelationshipLookup interface {
	Covered(from, to string, sign int) bool
}

// ProposerContract detects statistical patterns suggesting new relationships.
//
// ObserveProperty is the entry point from IngestSample: every observed property is
// fed, and the implementation pairs a scoped property (non-empty subject) only with
// unscoped ones, direction scoped -> unscoped, inside a time tolerance. Observe is
// the explicit path for a caller that already knows the pair; ObserveConstruct is
// the explicit construct-pairing path kept for callers that drive it directly.
//
// Guarantees:
//   - Read-only observation: no Observe* method modifies the model.
//   - Confirm never writes: it returns the proposition and the facade applies it,
//     which is what makes "never modifies the backbone directly" true.
//   - Settled candidates: a candidate is keyed on (from, to). After Reject, Confirm,
//     or an operator's Defer, that pair is not re-emitted under either sign within
//     the session; Forget clears it, so a subject that departs and returns can be
//     proposed again. A cap deferral is the proposer's own and provisional: it
//     re-enters when it outranks a pending candidate.
//   - Candidates: GetCandidates returns only Pending entries.
//   - Forget drops every buffer and candidate involving a property, so a departed
//     subject does not accumulate.
type ProposerContract interface {
	Observe(fromID, toID string, valueA, valueB float64) error
	ObserveConstruct(constructID string, value float64) error
	ObserveProperty(id, subject string, value float64, at time.Time) error
	Forget(propertyID string) error
	GetCandidates() ([]*types.CandidateEdge, error)
	Confirm(candidateID string) (*types.Proposition, error)
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
// kubelet /metrics, a pushing application) and the facade's ingestion. It turns
// observations into MetricSamples that declare their subject, unit and range. It
// knows nothing about the graph — routing a sample into a construct is the domain
// specification's business, done at ingestion.
//
// Guarantees:
//   - Pure read: Collect never modifies any system state.
//   - Empty on no data: Collect returns ([], nil) when no new samples are ready;
//     it never returns a non-nil error for a temporarily unavailable source.
//   - Deterministic EventID: the same physical observation (source, node, subject,
//     metric, anchor timestamp) always produces the same EventID.
//   - Metric type stability: AvailableMetrics returns the same set for the entire
//     lifetime of the instance; Collect never emits a MetricType outside it. Subjects
//     change the instances of a type, never the set of types.
//   - Node ID completeness: every emitted MetricSample has a non-empty NodeID.
//   - SourceID stability: SourceID returns the same string across restarts.
//   - Subjects are implied by observation: a collector MAY set Subject on a sample; a
//     subject exists iff something observed it this tick. A collector never emits a
//     "gone" event — silence is the signal, and the map interprets it.
//   - Declared meaning: a collector MUST set Unit and Range on every sample of a type
//     it emits, and keep them stable per type.
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
