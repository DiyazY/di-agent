// Package types defines the shared data structures that cross contract boundaries.
// No contract implementation may define its own wire types — all must use these.
package types

import (
	"regexp"
	"time"
)

// Direction encodes the sign of a proposition edge.
type Direction int

const (
	Positive Direction = iota // source construct increases target construct
	Negative                  // source construct decreases target construct
)

// CandidateStatus tracks the review state of a proposed backbone edge.
type CandidateStatus int

const (
	Pending   CandidateStatus = iota
	Confirmed                 // added to ontology backbone
	Rejected                  // suppressed for this deployment session
	Deferred                  // re-evaluate after more observations
)

// ── Graph primitives ──────────────────────────────────────────────────────────

// NodeDescriptor is gone with the storage graph it belonged to: a construct's current
// value and the confidence behind it live on the state model's property for that
// construct, where they are recomputed from the metrics routed to it rather than stored
// as a second copy.

type EdgeDescriptor struct {
	FromID        string
	ToID          string
	PropositionID string
	Direction     Direction

	// EMAWeight is the recent estimate — the fast EMA over the most recent pairs. It
	// carries no meaning when NObservations is 0.
	EMAWeight float64

	// Established is the long-run estimate this machine has accumulated, and Assertion
	// an operator's override. Both are nil when absent, because zero is the claim that
	// a relationship is worth nothing and absence is not that claim. Neither is ever
	// seeded from a calibration: the field these replaced was, and a number nobody
	// measured then entered every decision with weight (1 − confidence).
	Established *float64
	Assertion   *float64

	// Effective is the value the agent reasons with, nil when there is not one yet,
	// and Basis names which of the three layers it came from.
	Effective *float64
	Basis     string

	Confidence       float64
	NObservations    int
	Deprecated       bool     // mirrors Proposition.Deprecated; set by SemanticMap.Deprecate
	DeprecatedReason string   // mirrors Proposition.DeprecatedReason
	Mu               *float64 // Gaussian mean  (edge-standard profile+); nil if unavailable
	Sigma            *float64 // Gaussian std   (edge-standard profile+); nil if unavailable
}

// ── Ontology primitives ───────────────────────────────────────────────────────

type Construct struct {
	ConstructID string
	Name        string
	Description string
}

type Proposition struct {
	PropositionID string
	FromConstruct string
	ToConstruct   string
	Direction     Direction

	// PriorStrength is the strength in force, overlaid from the state model's
	// relationship for this proposition. The specification declares no strength, so
	// what the declaration layer holds for this field is the policy floor as a
	// placeholder — see Instantiated.
	PriorStrength float64

	// Instantiated reports whether the agent carries a relationship for this
	// proposition. False means the claim is declared but not modelled — seeding skips
	// a proposition whose endpoints are not both observable here — and PriorStrength
	// is then the placeholder rather than a calibrated value. The flag exists so a
	// caller can tell those two cases apart, which reading the number alone cannot.
	Instantiated bool
	// Description is a one-sentence English statement of the causal claim
	// (e.g. "Lightweight distributions reduce pod-startup latency"). Empty
	// for auto-proposed candidates until an operator fills it in via
	// AddValidatedProposition. Populated for the Di-Select bootstrap P1–P15.
	Description     string
	EvidenceSources []string // e.g. ["P1", "P4"]
	// Deprecated marks a proposition the agent no longer endorses, overlaid from
	// whether the state model's relationship for it is retired. It is preserved
	// in-place for history and replay, and a retired relationship leaves the
	// traversal, so no answer consults it. Deprecation is a soft-delete: propositions
	// are never structurally removed.
	Deprecated       bool
	DeprecatedReason string
}

type ValidationResult struct {
	Valid     bool
	Conflicts []string // proposition IDs that contradict the proposed edge
	Warnings  []string
}

// ── Agent query types ─────────────────────────────────────────────────────────

type OffloadContext struct {
	TaskType           string
	SourceNodeID       string
	DataSizeBytes      int64
	LatencyBudgetMs    float64
	EnergyBudgetJoules *float64 // nil = unconstrained
}

type ActionCost struct {
	CPUCost         float64
	ResourceCost    float64 // observed level of the resource construct, confidence-blended
	EnergyCost      float64 // placeholder: zero until EnergyJoules observations are available
	LatencyEstimate float64 // observed level of the pressure construct, confidence-blended
	Confidence      float64
	Rationale       string // must reference specific node/edge IDs
	GraphPathUsed   []string

	// ResourceSensitivity and PressureSensitivity are the weighted sums over the
	// edges terminating at each cost construct: how much the target would move per
	// unit change in a source construct, signed by each proposition's direction.
	//
	// They are reported beside the levels rather than folded into them. A level
	// answers "what is it now", which the observed construct value answers best; a
	// sensitivity answers "what would it become if load changed", which only the
	// relations can answer and which the level cannot contain. Adding the two was
	// measured and made next-interval predictions monotonically worse, so the
	// separation is empirical.
	//
	// Sensitivities are fully informed at cold start, since they come from the
	// calibrated priors; levels are uninformed at cold start and accumulate. The
	// two halves of the map are therefore useful at opposite ends of a deployment.
	ResourceSensitivity float64
	PressureSensitivity float64

	// DecisionID identifies this answer in the state map's journal. Fetching it
	// returns the properties and relationships the answer was computed from, at the
	// values they held — so "why did the agent say that" is answerable after the
	// system has moved on, rather than only from a rationale string that cannot be
	// checked against anything.
	//
	// Empty when the reasoner has no state map attached, which is the signal that
	// this answer is not traceable rather than that nothing happened.
	DecisionID string

	// Caveats name the reasons this answer may be weak: inputs that are stale, that
	// have never been observed, or that are missing. An agent that reports these is
	// reviewable; one that reports only its conclusion is not.
	Caveats []string
}

type PeerRecommendation struct {
	PeerID          string
	ExpectedSavings float64
	Rationale       string
	GraphPathUsed   []string
}

type OutcomeSimulation struct {
	ExpectedLatency      float64
	ExpectedResourceCost float64 // resource overhead derived from CPU/memory observations
	ExpectedEnergy       float64 // placeholder: zero until EnergyJoules observations are available
	Confidence           float64
	GraphPathUsed        []string
	P95Latency           *float64 // nil if Gaussian descriptors unavailable
	P95ResourceCost      *float64
	RiskFlags            []string
}

// ── Collector types ───────────────────────────────────────────────────────────

// MetricType is the semantic kind of an observation emitted by a collector.
//
// Collectors must normalize every value to a fraction on [0,1] before emitting,
// against whatever reference the deployment considers saturation — link capacity
// for throughput, a latency budget for durations, an energy budget for joules.
// This is a correctness requirement, not a convention: edge weights are Bernoulli
// parameters, so an out-of-range value is clipped and the affected edge stops
// responding to evidence while the aggregates keep looking healthy.
//
//	CPUUtilization       CPU quota consumed
//	MemoryUtilization    memory limit consumed
//	CPUThrottleRatio     scheduling periods throttled
//	BlockIOUtil          block I/O bandwidth consumed
//	CPUPressureRatio     PSI cpu.some stall fraction
//	IOPressureRatio      PSI io.some stall fraction
//	PodStartupMs         pod creation → running, against a budget
//	SchedulingLatencyMs  pod pending → scheduled, against a budget
//	NetworkRxBps         receive throughput, against link capacity
//	NetworkTxBps         transmit throughput, against link capacity
//	NetworkLossRatio     packet loss
//	NetworkLatencyMs     RTT to a peer node, against a budget
//	EnergyJoules         energy in the sample interval, against a budget
//
// Which construct each type routes to is declared in domain_spec.json, not here:
// these constants exist so collectors and tests can name a type without a string
// literal. A type the loaded specification does not route is ignored rather than
// rejected, so a collector upgraded ahead of the specification still ingests.
type MetricType string

const (
	CPUUtilization      MetricType = "cpu_utilization"
	MemoryUtilization   MetricType = "memory_utilization"
	CPUThrottleRatio    MetricType = "cpu_throttle_ratio"
	BlockIOUtil         MetricType = "block_io_util"
	CPUPressureRatio    MetricType = "cpu_pressure_ratio"
	IOPressureRatio     MetricType = "io_pressure_ratio"
	PodStartupMs        MetricType = "pod_startup_ms"
	SchedulingLatencyMs MetricType = "scheduling_latency_ms"
	NetworkRxBps        MetricType = "network_rx_bps"
	NetworkTxBps        MetricType = "network_tx_bps"
	NetworkLossRatio    MetricType = "network_loss_ratio"
	NetworkLatencyMs    MetricType = "network_latency_ms"
	EnergyJoules        MetricType = "energy_joules"
)

// MetricSample is one observation crossing the sample boundary. In-process collectors
// and external producers on POST /ingest-sample both hand the facade this; the map
// cannot tell them apart and must not try.
//
// EventID must be deterministic: the same physical observation (same source, node,
// subject, metric and timestamp) must produce the same EventID across calls and
// restarts, so idempotency holds end-to-end.
//
// Subject is "" for the node itself and "<kind>:<identity>" for anything narrower —
// a pod, a disk, a systemd unit, an application component. Kind is whatever the
// producer says; the map never interprets it. Unit and Range are the producer's
// declaration of what the value means; the map stamps them at admission.
// Labels is informational; nothing branches on it.
type MetricSample struct {
	NodeID        string
	MetricType    MetricType
	Value         float64
	TimestampUnix int64
	EventID       string
	Subject       string
	Unit          string
	Range         *[2]float64
	Source        string
	Labels        map[string]string
}

var subjectRe = regexp.MustCompile(`^[A-Za-z0-9._-]+:[A-Za-z0-9._:-]+$`)

var metricTypeRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ValidMetricType reports whether s is a non-empty single path segment over
// [A-Za-z0-9._-]. '@' is excluded because a property id is metric_type@subject, so an
// unscoped sample named "cpu@pod:a" would land on the scoped property's id; '/' is
// excluded because ids are single path segments in the HTTP surface.
func ValidMetricType(s string) bool { return metricTypeRe.MatchString(s) }

// ValidSubject reports whether s is empty (node scope) or has the form
// <kind>:<identity> over [A-Za-z0-9._:-]. A '/' is never allowed: property ids are
// single path segments in the HTTP surface.
func ValidSubject(s string) bool {
	if s == "" {
		return true
	}
	return subjectRe.MatchString(s)
}

// ── Tuner types ───────────────────────────────────────────────────────────────

// TuneIntent is the raw output of TunerContract.ParseIntent — a signed delta
// to apply to one proposition's prior strength.
type TuneIntent struct {
	PropositionID string
	Delta         float64 // signed: +0.12 to increase, -0.12 to decrease
	Rationale     string  // e.g. "intent:prioritize security (keyword: security, direction: increase)"
}

// TuneAdjustment is a finalized, bounded adjustment ready for application.
// OldStrength is the proposition's strength before the tune.
// NewStrength = clamp(OldStrength + Delta, propositionFloor, 0.95).
type TuneAdjustment struct {
	PropositionID string
	OldStrength   float64
	NewStrength   float64
	Rationale     string
}

// ── Proposer types ────────────────────────────────────────────────────────────

type CandidateEdge struct {
	CandidateID     string
	FromID          string
	ToID            string
	Direction       Direction
	MIScore         float64
	PValue          float64
	NObservations   int
	DeploymentsSeen int
	Status          CandidateStatus
	// Reason says who deferred a Deferred candidate: "cap" when the proposer's
	// pending cap chose it as the weakest, "operator" when Defer was called. A
	// cap deferral is provisional and re-enters when it outranks a pending one; an
	// operator's is not overturned.
	Reason string
}

// ── Ontology event log ────────────────────────────────────────────────────────
//
// The ontology is a live data structure: priors get recalibrated as new
// empirical evidence arrives, the Proposer discovers new propositions, and
// operators may deprecate stale claims. Every mutation emits an OntologyEvent
// so the agent can answer "why is this edge weight what it is?" at any point
// in time. The event log is append-only — entries are never modified or
// removed. Edge-minimal implementations keep the log in memory (ephemeral
// across restarts); richer profiles persist it.

// OntologyEventKind classifies what changed in the ontology.
type OntologyEventKind string

const (
	EventConstructAdded         OntologyEventKind = "construct_added"
	EventPropositionAdded       OntologyEventKind = "proposition_added"
	EventPropositionStrengthSet OntologyEventKind = "proposition_strength_set"
	EventPropositionDeprecated  OntologyEventKind = "proposition_deprecated"

	// EventOperatorTune records one operator intent that spanned several
	// propositions, alongside the individual strength events it produced. Reading
	// those separately gives no way to tell a coordinated adjustment from unrelated
	// ones that happened to land together.
	EventOperatorTune OntologyEventKind = "operator-tune"
)

// OntologyEvent is one entry in the ontology audit log.
//
// TargetID is the affected construct_id or proposition_id, depending on Kind.
// Detail carries structured context relevant to the event:
//
//	EventConstructAdded         -> {"name": ..., "description": ...}
//	EventPropositionAdded       -> {"from": ..., "to": ..., "direction": ..., "prior_strength": ...}
//	EventPropositionStrengthSet -> {"strength_old": ..., "strength_new": ...}
//	EventPropositionDeprecated  -> {"reason": ...}
type OntologyEvent struct {
	Timestamp time.Time
	Actor     string // "system", "operator:alice", "proposer", "prior_init_pipeline", …
	Kind      OntologyEventKind
	TargetID  string
	Detail    map[string]any
}
