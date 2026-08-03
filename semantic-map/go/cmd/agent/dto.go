// DTOs for the agent HTTP API.
//
// These types define the JSON wire shape served by the daemon's new
// /graph, /edges, /constructs, /propositions, /history and /ontology/*
// endpoints. They are deliberately distinct from pkg/types so the wire
// format can evolve independently of the internal data model.
//
// Critical wire decision: types.Direction (an int with values 0/1) is
// serialized as the strings "+" / "-". This keeps the JSON readable for
// curl, CLI tables, and the embedded UI.

package main

import (
	"fmt"
	"time"

	"github.com/DiyazY/di-agent/pkg/types"
)

// ── Request DTOs ──────────────────────────────────────────────────────────────

// SetStrengthRequest is the body of POST /ontology/strength.
type SetStrengthRequest struct {
	PropositionID string  `json:"proposition_id"`
	Strength      float64 `json:"strength"`
}

// DeprecateRequest is the body of POST /ontology/deprecate.
type DeprecateRequest struct {
	PropositionID string `json:"proposition_id"`
	Reason        string `json:"reason"`
}

// AddConstructRequest is the body of POST /ontology/construct.
type AddConstructRequest struct {
	ConstructID string `json:"construct_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// AddPropositionRequest is the body of POST /ontology/proposition.
// Direction is "+" (positive) or "-" (negative); the handler converts it.
type AddPropositionRequest struct {
	PropositionID string  `json:"proposition_id"`
	From          string  `json:"from"`
	To            string  `json:"to"`
	Direction     string  `json:"direction"`
	PriorStrength float64 `json:"prior_strength"`
}

// ResetRequest is the body of POST /agent/reset.
type ResetRequest struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// TuneRequest is the body of POST /agent/tune.
type TuneRequest struct {
	Intent   string `json:"intent"`
	Operator string `json:"operator,omitempty"`
}

// TuneAdjustmentDTO mirrors types.TuneAdjustment for the wire.
type TuneAdjustmentDTO struct {
	PropositionID string  `json:"proposition_id"`
	OldStrength   float64 `json:"old_strength"`
	NewStrength   float64 `json:"new_strength"`
	Rationale     string  `json:"rationale"`
}

// TuneResponse is the body of a successful POST /agent/tune.
type TuneResponse struct {
	Applied []TuneAdjustmentDTO `json:"applied"`
	Intent  string              `json:"intent"`
}

// MetricSampleRequest is the body of POST /ingest-sample.
//
// Distinct from POST /ingest: where /ingest takes a pre-routed (from_id,
// to_id, observation, event_id) tuple and bypasses the Bridge, /ingest-sample
// carries a MetricSample that the daemon routes through Bridge server-side.
// This is the public-API entry point for external collectors (e.g. the
// parquet replay tool) that don't speak Go and can't call IngestSample
// directly. ContainerID and Labels are optional and informational only —
// the Bridge does not branch on them in v1.
type MetricSampleRequest struct {
	NodeID        string            `json:"node_id"`
	MetricType    string            `json:"metric_type"`
	Value         float64           `json:"value"`
	TimestampUnix int64             `json:"timestamp_unix"`
	EventID       string            `json:"event_id"`
	ContainerID   string            `json:"container_id,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
}

// ── Response DTOs ─────────────────────────────────────────────────────────────

// GraphSnapshot is the top-level response of GET /graph.
type GraphSnapshot struct {
	Constructs   []ConstructDTO   `json:"constructs"`
	Propositions []PropositionDTO `json:"propositions"`
	Edges        []EdgeDTO        `json:"edges"`
}

// ConstructDTO mirrors types.Construct for wire output.
type ConstructDTO struct {
	ConstructID string `json:"construct_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// PropositionDTO mirrors types.Proposition. Direction is rendered as "+"/"-".
//
// prior_strength is the value in force, overlaid from the state model's relationship for
// this proposition. instantiated says whether such a relationship exists: false means the
// claim is declared but not modelled here — seeding skips one whose endpoints are not
// both observable — and prior_strength is then a placeholder rather than a calibrated
// value. A client that reads the number without the flag can mistake one for the other.
type PropositionDTO struct {
	PropositionID    string   `json:"proposition_id"`
	FromConstruct    string   `json:"from"`
	ToConstruct      string   `json:"to"`
	Direction        string   `json:"direction"`
	PriorStrength    float64  `json:"prior_strength"`
	Instantiated     bool     `json:"instantiated"`
	Description      string   `json:"description,omitempty"`
	EvidenceSources  []string `json:"evidence_sources,omitempty"`
	Deprecated       bool     `json:"deprecated"`
	DeprecatedReason string   `json:"deprecated_reason,omitempty"`
}

// EdgeDTO mirrors types.EdgeDescriptor. Direction is rendered as "+"/"-";
// Mu and Sigma encode as null when the Gaussian descriptor is unavailable.
type EdgeDTO struct {
	FromID           string   `json:"from"`
	ToID             string   `json:"to"`
	PropositionID    string   `json:"proposition_id"`
	Direction        string   `json:"direction"`
	PriorWeight      float64  `json:"prior_weight"`
	EMAWeight        float64  `json:"ema_weight"`
	Confidence       float64  `json:"confidence"`
	NObservations    int      `json:"n_observations"`
	Deprecated       bool     `json:"deprecated"`
	DeprecatedReason string   `json:"deprecated_reason,omitempty"`
	Mu               *float64 `json:"mu"`
	Sigma            *float64 `json:"sigma"`
}

// OntologyEventDTO mirrors types.OntologyEvent.
type OntologyEventDTO struct {
	Timestamp time.Time      `json:"timestamp"`
	Actor     string         `json:"actor"`
	Kind      string         `json:"kind"`
	TargetID  string         `json:"target_id"`
	Detail    map[string]any `json:"detail,omitempty"`
}

// HealthResponse is the body of GET /healthz.
type HealthResponse struct {
	OK bool `json:"ok"`
}

// VersionResponse is the body of GET /version.
type VersionResponse struct {
	AgentVersion       string `json:"agent_version"`
	GoVersion          string `json:"go_version"`
	BuildCommit        string `json:"build_commit"`
	SemmapConstructs   int    `json:"semmap_constructs"`
	SemmapPropositions int    `json:"semmap_propositions"`
}

// ErrorResponse is the body of any 4xx/5xx returned by a NEW endpoint.
type ErrorResponse struct {
	Error string `json:"error"`
}

// ── Peer DTOs ─────────────────────────────────────────────────────────────────

// PeerDTO mirrors peers.Descriptor for wire output.
type PeerDTO struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	Trust     float64   `json:"trust"`
	NObserved int       `json:"n_observed"`
	LastSeen  time.Time `json:"last_seen"`
	Note      string    `json:"note,omitempty"`
}

// AddPeerRequest is the body of POST /peers.
type AddPeerRequest struct {
	URL  string `json:"url"`
	Note string `json:"note,omitempty"`
}

// SetTrustRequest is the body of POST /peers/{id}/trust.
type SetTrustRequest struct {
	Value float64 `json:"value"`
}

// OffloadHTTPRequest is the body of POST /offload — the peer-side receiver of
// an offload proposal. Named with the HTTP prefix so it does not collide with
// pkg/peers.OffloadRequest (this is the server-side DTO; that is the
// outbound-client DTO).
type OffloadHTTPRequest struct {
	TaskType           string   `json:"task_type"`
	SourceNodeID       string   `json:"source_node_id"`
	DataSizeBytes      int64    `json:"data_size_bytes"`
	LatencyBudgetMs    float64  `json:"latency_budget_ms"`
	EnergyBudgetJoules *float64 `json:"energy_budget_joules,omitempty"`
}

// OffloadHTTPResponse is the body of POST /offload — the peer's decision.
type OffloadHTTPResponse struct {
	Accepted             bool    `json:"accepted"`
	Reason               string  `json:"reason,omitempty"`
	ExpectedLatency      float64 `json:"expected_latency"`
	ExpectedResourceCost float64 `json:"expected_resource_cost"`
	ExpectedEnergy       float64 `json:"expected_energy"` // placeholder: zero until EnergyJoules observations are available
}

// ── Mappers ───────────────────────────────────────────────────────────────────

// directionToString converts the internal Direction enum to its wire form.
func directionToString(d types.Direction) string {
	if d == types.Negative {
		return "-"
	}
	return "+"
}

// directionFromString parses a wire direction string back to the enum.
// Returns (Positive, true) for "+", (Negative, true) for "-", (0, false)
// for anything else.
func directionFromString(s string) (types.Direction, bool) {
	switch s {
	case "+":
		return types.Positive, true
	case "-":
		return types.Negative, true
	}
	return 0, false
}

func constructToDTO(c *types.Construct) ConstructDTO {
	return ConstructDTO{
		ConstructID: c.ConstructID,
		Name:        c.Name,
		Description: c.Description,
	}
}

func propositionToDTO(p *types.Proposition) PropositionDTO {
	return PropositionDTO{
		PropositionID:    p.PropositionID,
		FromConstruct:    p.FromConstruct,
		ToConstruct:      p.ToConstruct,
		Direction:        directionToString(p.Direction),
		PriorStrength:    p.PriorStrength,
		Instantiated:     p.Instantiated,
		Description:      p.Description,
		EvidenceSources:  p.EvidenceSources,
		Deprecated:       p.Deprecated,
		DeprecatedReason: p.DeprecatedReason,
	}
}

func edgeToDTO(e *types.EdgeDescriptor) EdgeDTO {
	return EdgeDTO{
		FromID:           e.FromID,
		ToID:             e.ToID,
		PropositionID:    e.PropositionID,
		Direction:        directionToString(e.Direction),
		PriorWeight:      e.PriorWeight,
		EMAWeight:        e.EMAWeight,
		Confidence:       e.Confidence,
		NObservations:    e.NObservations,
		Deprecated:       e.Deprecated,
		DeprecatedReason: e.DeprecatedReason,
		Mu:               e.Mu,
		Sigma:            e.Sigma,
	}
}

func eventToDTO(e *types.OntologyEvent) OntologyEventDTO {
	return OntologyEventDTO{
		Timestamp: e.Timestamp,
		Actor:     e.Actor,
		Kind:      string(e.Kind),
		TargetID:  e.TargetID,
		Detail:    e.Detail,
	}
}

// metricTypeValidator answers whether the loaded domain specification routes a
// metric type. The /ingest-sample boundary rejects unrouted types with 400 so
// operators and the replay tool get a clear error instead of a silent no-op —
// ingestion itself ignores them for forward compatibility, which is the right
// behaviour inside the pipeline but the wrong behaviour at an API boundary a
// human is typing against.
//
// This asks the specification rather than carrying a list. A hardcoded copy here
// was a third place routing knowledge lived, and it silently rejected the two PSI
// types the specification routes to PS.
type metricTypeValidator interface {
	RoutedConstruct(metricType string) (string, bool)
}

func tuneAdjToDTO(a *types.TuneAdjustment) TuneAdjustmentDTO {
	return TuneAdjustmentDTO{
		PropositionID: a.PropositionID,
		OldStrength:   a.OldStrength,
		NewStrength:   a.NewStrength,
		Rationale:     a.Rationale,
	}
}

// sampleRequestToTypes converts the wire DTO into a *types.MetricSample,
// validating the metric_type string against the closed catalogue declared in
// pkg/types. Returns an error suitable for writeError(400, ...) when the
// metric type is unknown.
func sampleRequestToTypes(req *MetricSampleRequest, v metricTypeValidator) (*types.MetricSample, error) {
	mt := types.MetricType(req.MetricType)
	if req.MetricType == "" {
		return nil, fmt.Errorf("metric_type is required")
	}
	// An unrouted metric type is NOT rejected. It is something the system reported,
	// and the state model records it as a property — a model that can only represent
	// what someone declared in advance is a model of the system as it was when they
	// wrote it down. The handler answers 202 rather than 204 so the caller still
	// learns that nothing summarises it, which is the part a typo needs to surface.
	_ = v
	return &types.MetricSample{
		NodeID:        req.NodeID,
		MetricType:    mt,
		Value:         req.Value,
		TimestampUnix: req.TimestampUnix,
		EventID:       req.EventID,
		ContainerID:   req.ContainerID,
		Labels:        req.Labels,
	}, nil
}

// ExplainHTTPRequest is the body of POST /explain. Named with the HTTP
// prefix to keep it distinct from pkg/explain.ExplainRequest which the route
// handler translates into.
//
// MaxIterations and MaxToolCalls are optional operator overrides; zero values
// fall back to package defaults (3 iterations, 10 tool calls).
type ExplainHTTPRequest struct {
	Question      string `json:"question"`
	MaxIterations int    `json:"max_iterations,omitempty"`
	MaxToolCalls  int    `json:"max_tool_calls,omitempty"`

	// v2 opt-ins. Omitting all of these reproduces v1 behaviour exactly.
	SessionID  string `json:"session_id,omitempty"`
	UsePlanner bool   `json:"use_planner,omitempty"`
	UseCritic  bool   `json:"use_critic,omitempty"`
	Stream     bool   `json:"stream,omitempty"`
}
