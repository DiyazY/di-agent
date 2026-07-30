// Package types defines the shared data structures that cross contract boundaries.
// No contract implementation may define its own wire types — all must use these.
package types

import "time"

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

type NodeDescriptor struct {
	NodeID        string
	ConstructType string
	PriorValue    float64
	EMAValue      float64
	Confidence    float64 // 0.0 = prior-dominated, 1.0 = evidence-dominated
	NObservations int
}

type EdgeDescriptor struct {
	FromID           string
	ToID             string
	PropositionID    string
	Direction        Direction
	PriorWeight      float64
	EMAWeight        float64
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
	PriorStrength float64
	// Description is a one-sentence English statement of the causal claim
	// (e.g. "Lightweight distributions reduce pod-startup latency"). Empty
	// for auto-proposed candidates until an operator fills it in via
	// AddValidatedProposition. Populated for the Di-Select bootstrap P1–P15.
	Description     string
	EvidenceSources []string // e.g. ["P1", "P4"]
	// Deprecated marks a proposition that the ontology no longer endorses but
	// is preserved in-place (history/replay). Reasoners must skip deprecated
	// propositions during cost computation. Deprecation is a soft-delete:
	// existing propositions are never structurally removed.
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
	ResourceCost    float64 // resource overhead derived from CPU/memory observations
	EnergyCost      float64 // placeholder: zero until EnergyJoules observations are available
	LatencyEstimate float64
	Confidence      float64
	Rationale       string // must reference specific node/edge IDs
	GraphPathUsed   []string
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
// Values are fixed — collectors must normalize raw source data to these units:
//
//	CPUUtilization       fraction [0,1]  CPU quota consumed
//	MemoryUtilization    fraction [0,1]  memory limit consumed
//	CPUThrottleRatio     fraction [0,1]  scheduling periods throttled
//	BlockIOUtil          fraction [0,1]  block I/O bandwidth consumed
//	PodStartupMs         milliseconds    pod creation → running
//	SchedulingLatencyMs  milliseconds    pod pending → scheduled
//	NetworkRxBps         bytes/sec       receive throughput
//	NetworkTxBps         bytes/sec       transmit throughput
//	NetworkLossRatio     fraction [0,1]  packet loss
//	NetworkLatencyMs     milliseconds    RTT to a peer node
//	EnergyJoules         joules          energy in the sample interval
type MetricType string

const (
	CPUUtilization      MetricType = "cpu_utilization"
	MemoryUtilization   MetricType = "memory_utilization"
	CPUThrottleRatio    MetricType = "cpu_throttle_ratio"
	BlockIOUtil         MetricType = "block_io_util"
	PodStartupMs        MetricType = "pod_startup_ms"
	SchedulingLatencyMs MetricType = "scheduling_latency_ms"
	NetworkRxBps        MetricType = "network_rx_bps"
	NetworkTxBps        MetricType = "network_tx_bps"
	NetworkLossRatio    MetricType = "network_loss_ratio"
	NetworkLatencyMs    MetricType = "network_latency_ms"
	EnergyJoules        MetricType = "energy_joules"
)

// MetricSample is one normalized observation emitted by a CollectorContract.
//
// EventID must be deterministic: the same physical observation (same SourceID,
// NodeID, ContainerID, MetricType, and TimestampUnix) must produce the same
// EventID across calls and restarts, so that the Updater's idempotency
// guarantee holds end-to-end.
//
// ContainerID is empty for node-level aggregates.
// Labels carries source-specific metadata and is informational only.
type MetricSample struct {
	NodeID        string
	MetricType    MetricType
	Value         float64
	TimestampUnix int64
	EventID       string
	ContainerID   string            // empty = node-level aggregate
	Labels        map[string]string // informational; bridge must not branch on these
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
