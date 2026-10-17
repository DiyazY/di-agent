// Package types defines the shared data structures that cross contract boundaries.
// No contract implementation may define its own wire types — all must use these.
package types

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
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

var candidateStatusNames = [...]string{"pending", "confirmed", "rejected", "deferred"}

func (s CandidateStatus) String() string {
	if int(s) >= 0 && int(s) < len(candidateStatusNames) {
		return candidateStatusNames[s]
	}
	return strconv.Itoa(int(s))
}

// MarshalJSON puts the name on the wire: as an integer, Pending serialised as 0,
// which a reader could not tell from a missing field.
func (s CandidateStatus) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

// UnmarshalJSON accepts the name, and still the number for readings written by an
// older agent.
func (s *CandidateStatus) UnmarshalJSON(b []byte) error {
	var name string
	if err := json.Unmarshal(b, &name); err == nil {
		for i, n := range candidateStatusNames {
			if n == name {
				*s = CandidateStatus(i)
				return nil
			}
		}
		return fmt.Errorf("unknown candidate status %q", name)
	}
	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("candidate status must be a name or a number: %s", b)
	}
	*s = CandidateStatus(n)
	return nil
}

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
// A collector declares what each value means: Unit names the quantity and Range its
// bounds, and the map normalises by the declared range wherever it needs a fraction.
// Types that a domain specification routes into a construct are still expected as
// fractions on [0,1], against whatever reference the deployment considers saturation
// — link capacity for throughput, a latency budget for durations — because construct
// polarity and the divergence measure are defined on that interval and an
// out-of-range value there is clipped while the aggregates keep looking healthy. An
// unrouted type (any name a producer sends, most often scoped to a subject) carries
// whatever unit and range it declares.
//
//	CPUUtilization       share of the node's CPU (cgroup collector)
//	MemoryUtilization    share of the node's memory
//	CPUThrottleRatio     throttled CFS periods over elapsed periods (share-of-periods)
//	BlockIOUtil          block I/O bandwidth consumed
//	CPUPressureRatio     PSI cpu.some stall fraction
//	IOPressureRatio      PSI io.some stall fraction
//	MemoryPressureRatio  PSI memory.some stall fraction
//	CPUStealRatio        share of CPU time the hypervisor gave to others (node-level)
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
	MemoryPressureRatio MetricType = "memory_pressure_ratio"
	CPUStealRatio       MetricType = "cpu_steal_ratio"
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
// Chart names the series a producer computes together from one reading of its
// source, as parts of one whole (a stacked Netdata chart, one line of /proc/stat);
// empty when it groups nothing. Series drawn together but measured apart, such as an
// interface's received and sent, are not one chart.
// Series of one chart and one subject that stay perfectly correlated are sums,
// complements or smoothings of each other, and discovery treats them as one
// arithmetic group rather than as findings.
// PartOf names the node-level metric type this subject's quantity is a part of, when
// the producer knows it is summed into one: a physical disk's writes into the node's
// I/O out, a core's user time into the node's. Discovery marks a candidate from it to
// that type composition, as it does a subject's metric against the node's metric of
// the same name, which is the case PartOf need not state. It holds per sample: a
// sample without it withdraws what an earlier one declared.
// Labels is informational; nothing branches on it.
type MetricSample struct {
	NodeID        string            `json:"node_id"`
	MetricType    MetricType        `json:"metric_type"`
	Value         float64           `json:"value"`
	TimestampUnix int64             `json:"timestamp_unix"`
	EventID       string            `json:"event_id"`
	Subject       string            `json:"subject,omitempty"`
	Unit          string            `json:"unit,omitempty"`
	Range         *[2]float64       `json:"range,omitempty"`
	Source        string            `json:"source,omitempty"`
	Chart         string            `json:"chart,omitempty"`
	PartOf        string            `json:"part_of,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
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

// PropertyReading is one observed property value as the proposer sees it. Range is
// the producer's declared range, nil when none was declared: the proposer's variation
// gate measures movement against it and cannot without it. Chart is the sample's
// chart: properties of one chart and one subject may form an arithmetic group. PartOf
// is the node-level property the sample's producer said it is a part of.
type PropertyReading struct {
	ID      string
	Subject string
	Chart   string
	PartOf  string
	Value   float64
	At      time.Time
	Range   *[2]float64
}

// Candidate kinds: what is known about a pair before any correlation is read.
const (
	// CandidateKindComposition marks a candidate whose source is part of its target.
	CandidateKindComposition = "composition"
	// CandidateKindSameMetric marks two subjects' readings of one metric. They often
	// move together because something drives both (the same cron, the same client),
	// not because either drives the other.
	CandidateKindSameMetric = "same-metric"
	// CandidateKindSameSubject marks two metrics of one subject, such as a capped
	// pod's CPU and its throttling: one workload's own quantities often move together
	// by construction, so the pair is more likely accounting than a finding.
	CandidateKindSameSubject = "same-subject"
)

// Pair classes, as DiscoveryCounts reports them.
const (
	PairSubjectNode    = "subject->node"
	PairNodeNode       = "node<->node"
	PairSubjectSubject = "subject<->subject"
)

// DiscoveryCounts says how far discovery was rationed: how many pairs could be
// tested, how many are, and how many were displaced to make room. Without it an
// empty candidate list cannot be told apart from a budget too small to look.
type DiscoveryCounts struct {
	// Budget is how many pairs may be tested at once; 0 means no limit.
	Budget int `json:"budget"`
	// Properties is how many properties were observed within the pair window of the
	// latest reading: the ones a pair can be formed from now.
	Properties    int `json:"properties"`
	EligiblePairs int `json:"eligible_pairs"`
	// TestedPairs is the pairs being tested whose ends were both observed within the
	// pair window; IdlePairs those with an end gone quiet. Both hold budget: an idle
	// pair is displaced first when a new pair needs room, and dropped when its
	// property retires.
	TestedPairs int `json:"tested_pairs"`
	IdlePairs   int `json:"idle_pairs"`
	// TestedByClass splits TestedPairs by pair class.
	TestedByClass map[string]int `json:"tested_by_class"`
	// Displaced counts pairs dropped, since the start, to make room for more
	// variable ones.
	Displaced uint64 `json:"displaced"`
	// Groups are the arithmetic groups found: properties of one chart and one
	// subject whose pairs reached |r| ≥ 0.99 and have not since fallen more than
	// their sampling error below it. Only each group's representative is paired
	// outside it.
	Groups []ArithmeticGroup `json:"groups,omitempty"`
	// FDR is the false-discovery rate the candidate bar holds to, 0 when
	// uncorrected; PBar is the bar a p-value must be at or under now, over the
	// PValuesTested pairs being tested (arithmetic links aside), counting those
	// without enough readings yet as tests that found nothing.
	FDR           float64 `json:"fdr"`
	PBar          float64 `json:"p_bar"`
	PValuesTested int     `json:"p_values_tested"`
}

// ArithmeticGroup is properties of one chart and one subject that move as one: each
// is linked to another member by a tested pair that reached |r| ≥ 0.99 and holds
// within one standard error of Fisher's z of it. Representative, the
// most variable member when the group last changed, is the only one paired outside.
type ArithmeticGroup struct {
	Chart          string   `json:"chart"`
	Subject        string   `json:"subject,omitempty"`
	Representative string   `json:"representative"`
	Members        []string `json:"members"`
}

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
	// Undirected is true when nothing says which end drives the other: both are
	// node-level or both belong to subjects, and correlation is symmetric. FromID and ToID are then the pair in
	// lexicographic order, not a claim, and confirming one requires the operator to
	// name the end the relationship runs from.
	Undirected bool
	// Kind is "composition" when the source is a component of the target by
	// definition (a subject's metric against the node's total of the same metric),
	// so the correlation is accounting rather than something learned; "same-metric"
	// for two subjects' readings of one metric, where a shared driver is the likely
	// explanation; "same-subject" for two metrics of one subject, which often hold
	// together by construction; empty otherwise.
	Kind string
	// PBar is the p-value bar the candidate passed at its last update: corrected for
	// how many pairs were being tested (Benjamini–Hochberg at the proposer's
	// false-discovery rate), or 0 when the proposer applies no correction.
	PBar float64
	// Caveats states what the proposer could not check for this candidate, such as a
	// variation gate skipped because an endpoint declared no range.
	Caveats []string
	// Reason says who deferred a Deferred candidate: "cap" when the proposer's
	// pending cap chose it as the weakest, "untested" when its pair was displaced
	// from the pair budget and its numbers stopped being updated, "grouped" when an
	// end joined an arithmetic group it does not represent, "bar" when its p-value
	// no longer clears the corrected bar as more pairs came to be tested, "weakened"
	// when its |r| fell below the candidate threshold by more than its sampling
	// error, "operator" when an operator deferred it, "agent" when the agent did. The
	// proposer's own deferrals are provisional and re-enter when the evidence holds
	// again; an agent's re-enters when the evidence has moved since its review
	// (CandidateReview.Outdated); an operator's is not overturned. A Rejected
	// candidate carries "operator" or "agent" the same way: only an operator's
	// rejection is permanent for the session.
	Reason string
	// Review is the latest review of the candidate: who looked at it, what they
	// decided and on what evidence. Nil until someone has.
	Review *CandidateReview
}

// Who reviewed a candidate.
const (
	ReviewerOperator       = "operator"        // through the review endpoints
	ReviewerRules          = "rules"           // tier 0, in the daemon
	ReviewerDecisionModel  = "decision-model"  // tier 1, one forward pass
	ReviewerReasoningAgent = "reasoning-agent" // tier 2, a model calling tools
)

// What a review decided. Escalate means no tier was sure enough to act, and the
// candidate stays in the operator's queue.
const (
	ReviewConfirm  = "confirm"
	ReviewReject   = "reject"
	ReviewDefer    = "defer"
	ReviewEscalate = "escalate"
)

// CandidateReview records one review of a candidate. Applied is false when the
// reviewer was only allowed to suggest, under the autonomy level in force: the
// action is then what it would have done. DecisionID names the decision record
// (GET /state/decisions/{id}) holding everything the review read; an operator's
// review has none. The evidence fields are the candidate's numbers when it was
// reviewed, so Outdated can tell when they have moved.
type CandidateReview struct {
	By            string
	Model         string `json:",omitempty"`
	Tier          int
	Action        string
	Applied       bool
	DecisionID    string `json:",omitempty"`
	At            time.Time
	MIScore       float64
	NObservations int
	Direction     Direction
	Kind          string
}

// Outdated reports whether c's evidence has moved materially since the review: its
// sign or kind changed, or its |r| moved by more than two standard errors of
// Fisher's z at the smaller of the two windows. A review that is outdated no
// longer speaks for the candidate, so an agent's rejection or deferral reopens and
// an escalated candidate is reviewed again.
func (r *CandidateReview) Outdated(c *CandidateEdge) bool {
	if r == nil {
		return true
	}
	if c.Direction != r.Direction || c.Kind != r.Kind {
		return true
	}
	n := min(c.NObservations, r.NObservations)
	if n <= 3 {
		return c.NObservations != r.NObservations
	}
	z := func(v float64) float64 { return math.Atanh(math.Min(math.Abs(v), 0.9999)) }
	return math.Abs(z(c.MIScore)-z(r.MIScore)) > 2/math.Sqrt(float64(n-3))
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
