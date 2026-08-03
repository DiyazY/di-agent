// Package client provides a Go HTTP client for the di-agent semantic-map
// daemon. The types in this file deliberately duplicate the wire DTOs from
// cmd/agent/dto.go: the CLI treats the daemon as a remote service and must
// not import server-side packages. Thirty lines of duplication buy us a
// clean contract boundary that lets the wire format and the server
// internals evolve independently.
package client

import "time"

// GraphSnapshot is the top-level response of GET /graph.
type GraphSnapshot struct {
	Constructs   []ConstructDTO   `json:"constructs"`
	Propositions []PropositionDTO `json:"propositions"`
	Edges        []EdgeDTO        `json:"edges"`
}

// ConstructDTO mirrors the server's ConstructDTO.
type ConstructDTO struct {
	ConstructID string `json:"construct_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// PropositionDTO mirrors the server's PropositionDTO. Direction is "+" / "-".
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

// EdgeDTO mirrors the server's EdgeDTO. Mu/Sigma are nil when the Gaussian
// descriptor is unavailable.
type EdgeDTO struct {
	FromID        string   `json:"from"`
	ToID          string   `json:"to"`
	PropositionID string   `json:"proposition_id"`
	Direction     string   `json:"direction"`
	PriorWeight   float64  `json:"prior_weight"`
	EMAWeight     float64  `json:"ema_weight"`
	Confidence    float64  `json:"confidence"`
	NObservations int      `json:"n_observations"`
	Mu            *float64 `json:"mu"`
	Sigma         *float64 `json:"sigma"`
}

// OntologyEventDTO mirrors the server's OntologyEventDTO.
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

// ErrorResponse is the body of any 4xx/5xx from the new endpoints.
type ErrorResponse struct {
	Error string `json:"error"`
}

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

// ── Agent-query DTOs (existing endpoints) ─────────────────────────────────────

// OffloadContext is the body of POST /recommend and the inner "context" of
// POST /simulate. Mirrors types.OffloadContext as serialized by the daemon
// (the existing endpoints use Go-default encoding/json — exported field names).
type OffloadContext struct {
	TaskType           string   `json:"TaskType,omitempty"`
	SourceNodeID       string   `json:"SourceNodeID,omitempty"`
	DataSizeBytes      int64    `json:"DataSizeBytes,omitempty"`
	LatencyBudgetMs    float64  `json:"LatencyBudgetMs,omitempty"`
	EnergyBudgetJoules *float64 `json:"EnergyBudgetJoules,omitempty"`
}

// PeerRecommendation is the response of POST /recommend.
type PeerRecommendation struct {
	PeerID          string   `json:"PeerID"`
	ExpectedSavings float64  `json:"ExpectedSavings"`
	Rationale       string   `json:"Rationale"`
	GraphPathUsed   []string `json:"GraphPathUsed"`
}

// SimulateRequest is the body of POST /simulate.
type SimulateRequest struct {
	Context      OffloadContext `json:"context"`
	TargetNodeID string         `json:"target_node_id"`
}

// OutcomeSimulation is the response of POST /simulate.
type OutcomeSimulation struct {
	ExpectedLatency      float64  `json:"ExpectedLatency"`
	ExpectedResourceCost float64  `json:"ExpectedResourceCost"`
	ExpectedEnergy       float64  `json:"ExpectedEnergy"` // placeholder: zero until EnergyJoules observations are available
	Confidence           float64  `json:"Confidence"`
	GraphPathUsed        []string `json:"GraphPathUsed"`
	P95Latency           *float64 `json:"P95Latency"`
	P95ResourceCost      *float64 `json:"P95ResourceCost"`
	RiskFlags            []string `json:"RiskFlags"`
}

// CandidateEdge mirrors types.CandidateEdge for the GET /candidates response.
type CandidateEdge struct {
	CandidateID     string  `json:"CandidateID"`
	FromID          string  `json:"FromID"`
	ToID            string  `json:"ToID"`
	Direction       int     `json:"Direction"`
	MIScore         float64 `json:"MIScore"`
	PValue          float64 `json:"PValue"`
	NObservations   int     `json:"NObservations"`
	DeploymentsSeen int     `json:"DeploymentsSeen"`
	Status          int     `json:"Status"`
}

// ── Peer coordination DTOs ────────────────────────────────────────────────────

// PeerDTO mirrors the server's PeerDTO.
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

// ── Tuner DTOs ────────────────────────────────────────────────────────────────

// TuneRequest is the body of POST /agent/tune.
type TuneRequest struct {
	Intent   string `json:"intent"`
	Operator string `json:"operator,omitempty"`
}

// TuneAdjustmentDTO mirrors types.TuneAdjustment on the wire.
type TuneAdjustmentDTO struct {
	PropositionID string  `json:"proposition_id"`
	OldStrength   float64 `json:"old_strength"`
	NewStrength   float64 `json:"new_strength"`
	Rationale     string  `json:"rationale"`
}

// TuneResponse is the response of POST /agent/tune.
type TuneResponse struct {
	Applied []TuneAdjustmentDTO `json:"applied"`
	Intent  string              `json:"intent"`
}
