// Package semmap provides the SemanticMap facade.
// Agent code imports only this package — never contract implementations directly.
package semmap

import (
	"log"
	"time"

	"github.com/DiyazY/di-agent/pkg/contracts"
	"github.com/DiyazY/di-agent/pkg/peers"
	"github.com/DiyazY/di-agent/pkg/types"
)

// SemanticMap wires the six contracts and exposes the agent API. It also
// holds the peer coordination handles (peers.Registry, peers.Client) so the
// HTTP layer and the reasoner share a single source of truth for peers.
type SemanticMap struct {
	storage  contracts.StorageContract
	ontology contracts.OntologyContract
	updater  contracts.UpdaterContract
	reasoner contracts.ReasonerContract
	proposer contracts.ProposerContract
	tuner    contracts.TunerContract

	peers *peers.Registry
	peerc *peers.Client
}

// New constructs a SemanticMap without peer coordination. The facade still
// satisfies its peer-facing methods (Peers, PeerClient) by lazily allocating
// an empty registry + default client on first access — preserving backward
// compatibility for callers that don't yet wire peers explicitly.
func New(
	storage contracts.StorageContract,
	ontology contracts.OntologyContract,
	updater contracts.UpdaterContract,
	reasoner contracts.ReasonerContract,
	proposer contracts.ProposerContract,
	tuner contracts.TunerContract,
) *SemanticMap {
	return &SemanticMap{
		storage:  storage,
		ontology: ontology,
		updater:  updater,
		reasoner: reasoner,
		proposer: proposer,
		tuner:    tuner,
	}
}

// NewWithPeers is the peer-aware constructor used by profiles.Build. Both
// peerRegistry and peerClient may be nil — Peers() and PeerClient() lazily
// fall back to fresh instances in that case.
func NewWithPeers(
	storage contracts.StorageContract,
	ontology contracts.OntologyContract,
	updater contracts.UpdaterContract,
	reasoner contracts.ReasonerContract,
	proposer contracts.ProposerContract,
	tuner contracts.TunerContract,
	peerRegistry *peers.Registry,
	peerClient *peers.Client,
) *SemanticMap {
	return &SemanticMap{
		storage:  storage,
		ontology: ontology,
		updater:  updater,
		reasoner: reasoner,
		proposer: proposer,
		tuner:    tuner,
		peers:    peerRegistry,
		peerc:    peerClient,
	}
}

// Peers returns the peer registry attached to this map. If no registry was
// wired at construction time, a fresh empty one is allocated and cached so
// callers see a stable reference.
func (m *SemanticMap) Peers() *peers.Registry {
	if m.peers == nil {
		m.peers = peers.NewRegistry()
	}
	return m.peers
}

// PeerClient returns the HTTP client used for outbound peer calls. If no
// client was wired, a default client with a 2s timeout is allocated and
// cached on first access.
func (m *SemanticMap) PeerClient() *peers.Client {
	if m.peerc == nil {
		m.peerc = peers.NewClient(2 * time.Second)
	}
	return m.peerc
}

// ── Agent queries ─────────────────────────────────────────────────────────────

func (m *SemanticMap) CostOfAction(taskType, nodeID string) (*types.ActionCost, error) {
	return m.reasoner.CostOfAction(taskType, nodeID)
}

func (m *SemanticMap) RecommendPeer(ctx *types.OffloadContext) (*types.PeerRecommendation, error) {
	return m.reasoner.RecommendPeer(ctx)
}

func (m *SemanticMap) SimulateOutcome(ctx *types.OffloadContext, targetNodeID string) (*types.OutcomeSimulation, error) {
	return m.reasoner.SimulateOutcome(ctx, targetNodeID)
}

// ── Telemetry ingestion ───────────────────────────────────────────────────────

// Ingest feeds one telemetry observation into the evidence layer.
// It updates the edge descriptor and notifies the proposer.
func (m *SemanticMap) Ingest(fromID, toID string, observation float64, eventID string) error {
	if _, err := m.updater.UpdateEdge(fromID, toID, observation, eventID); err != nil {
		return err
	}
	return m.proposer.Observe(fromID, toID, observation, observation)
}

// IngestSample feeds one MetricSample through the Bridge. The Bridge maps the
// metric type to its primary construct, looks up every relationship that
// touches that construct, and calls UpdateEdge on each unique (from, to)
// pair. Idempotency is per-edge — replaying the same sample is a no-op.
//
// After the Bridge runs, the proposer is notified via ObserveConstruct so it
// can pair the new value against every other construct it has seen. Errors
// from ObserveConstruct are intentionally swallowed — the proposer is advisory
// and must not block telemetry ingestion.
//
// Returns nil even when the metric type has no mapping (forward-compat with
// future MetricTypes). Per-edge errors are returned (first one wins) so the
// caller can decide whether to keep looping; the Bridge itself processes
// every reachable edge regardless of individual failures.
func (m *SemanticMap) IngestSample(sample *types.MetricSample) error {
	if err := Bridge(sample, m.ontology, m.updater); err != nil {
		return err
	}
	if m.proposer != nil {
		if construct, ok := ConstructForMetric(sample.MetricType); ok {
			_ = m.proposer.ObserveConstruct(construct, sample.Value)
		}
	}
	return nil
}

// ── Graph extension ───────────────────────────────────────────────────────────

func (m *SemanticMap) PendingCandidates() ([]*types.CandidateEdge, error) {
	return m.proposer.GetCandidates()
}

func (m *SemanticMap) ConfirmCandidate(candidateID string) error {
	return m.proposer.Confirm(candidateID)
}

func (m *SemanticMap) RejectCandidate(candidateID string) error {
	return m.proposer.Reject(candidateID)
}

func (m *SemanticMap) DeferCandidate(candidateID string) error {
	return m.proposer.Defer(candidateID)
}

// ── Read-side facade (introspection) ──────────────────────────────────────────
//
// These pass-throughs expose graph state to transports (HTTP, CLI, UI). They
// intentionally live on the facade — many small methods over one mega-Snapshot
// — so the package stays transport-agnostic and easy to consume.

// Constructs returns every construct currently registered in the ontology.
func (m *SemanticMap) Constructs() ([]*types.Construct, error) {
	return m.ontology.Constructs()
}

// Propositions returns every proposition (including deprecated ones, flagged
// via Proposition.Deprecated).
func (m *SemanticMap) Propositions() ([]*types.Proposition, error) {
	return m.ontology.Propositions()
}

// AllEdges returns every edge descriptor currently held in storage.
func (m *SemanticMap) AllEdges() ([]*types.EdgeDescriptor, error) {
	return m.storage.AllEdges()
}

// EdgesByPair returns every edge between (from, to). Conflict-pair endpoints
// (e.g. RC→PS for P2/P3) yield more than one descriptor.
func (m *SemanticMap) EdgesByPair(from, to string) ([]*types.EdgeDescriptor, error) {
	return m.storage.GetEdgesByPair(from, to)
}

// Neighbors returns the set of construct IDs reachable from nodeID via one
// outgoing edge.
func (m *SemanticMap) Neighbors(nodeID string) ([]string, error) {
	return m.storage.Neighbors(nodeID)
}

// History returns ontology mutation events appended at or after `since`.
// Pass the zero time.Time to retrieve the full audit log.
func (m *SemanticMap) History(since time.Time) ([]*types.OntologyEvent, error) {
	return m.ontology.GetHistory(since)
}

// ── Write-side facade (ontology mutations) ────────────────────────────────────

// SetPropositionStrength recalibrates the prior strength of an existing
// proposition, appends an event to the audit log, and writes the new magnitude
// through to every matching EdgeDescriptor in storage.
//
// The storage write is the point of the operation, not bookkeeping. This is the
// write path for "this edge's magnitude should change because better knowledge
// arrived" — a fresh kube-bench scan revising an SC score, a new paper revising
// a proposition, or an operator tune. The Reasoner computes cost from storage
// edge weights, so an ontology-only write leaves the agent's decisions exactly
// as they were and the recalibration is silently cosmetic.
//
// The value is absolute: a new scan yields a new magnitude, not a delta. Callers
// that think in deltas (see Tune) resolve the current value first and pass the
// result. Writing here deliberately supersedes the per-distribution calibration
// that prior_init seeded — an operator asserting a value for *this* deployment
// outranks a cross-distribution estimate, and the audit log records that it
// happened. Cold-start equivalence to Di-Select (paper §4.3) is a property of
// the state at c = 0 before any operator action, not an invariant for all time.
//
// EMAWeight is handled conditionally, and the condition matters. On an edge that
// has accumulated observations it is evidence, and recalibrating a prior must not
// rewrite observation history — so it is left untouched. On an edge with zero
// observations it is not evidence at all, merely the seed value that
// seedFromOntology set equal to the prior; leaving it behind would anchor the
// first future observation's EMA to a magnitude the operator has already
// superseded. That is not a corner case: the recalibration path exists primarily
// for the six edges with no telemetry analog (paper §5.2), which carry zero
// observations by definition.
func (m *SemanticMap) SetPropositionStrength(id string, strength float64) error {
	if err := m.ontology.SetPropositionStrength(id, strength); err != nil {
		return err
	}
	edges, err := m.storage.AllEdges()
	if err != nil {
		return nil // ontology already updated; storage sync is best-effort
	}
	for _, e := range edges {
		if e.PropositionID != id {
			continue
		}
		e.PriorWeight = strength
		if e.NObservations == 0 {
			e.EMAWeight = strength
		}
		_ = m.storage.PutEdge(e)
	}
	return nil
}

// Deprecate marks a proposition as no-longer-endorsed (soft delete).
// Reasoners must skip deprecated propositions during cost computation.
// The flag is synced to every matching EdgeDescriptor in storage so that
// GET /graph reflects the deprecation without a proposition join.
func (m *SemanticMap) Deprecate(id, reason string) error {
	if err := m.ontology.Deprecate(id, reason); err != nil {
		return err
	}
	edges, err := m.storage.AllEdges()
	if err != nil {
		return nil // ontology already updated; storage sync is best-effort
	}
	for _, e := range edges {
		if e.PropositionID == id {
			e.Deprecated = true
			e.DeprecatedReason = reason
			_ = m.storage.PutEdge(e)
		}
	}
	return nil
}

// AddConstruct appends a new construct to the ontology and materializes its
// node descriptor in storage.
//
// The storage write is not optional bookkeeping: the Reasoner and the Updater
// address constructs through storage, so an ontology-only construct is
// invisible to every read path that matters. Constructs added at startup get
// their node from seedFromOntology; one added at runtime needs the same
// treatment, at the neutral 0.5 prior seedFromOntology uses.
func (m *SemanticMap) AddConstruct(c *types.Construct) error {
	if err := m.ontology.AddConstruct(c); err != nil {
		return err
	}
	return m.storage.PutNode(&types.NodeDescriptor{
		NodeID:        c.ConstructID,
		ConstructType: c.Name,
		PriorValue:    0.5,
		EMAValue:      0.5,
		Confidence:    0.0,
		NObservations: 0,
	})
}

// AddValidatedProposition appends a new proposition after the ontology has
// validated it against the existing backbone, and seeds the corresponding
// EdgeDescriptor in storage.
//
// Without the storage write the proposition exists only in the ontology: it
// appears in Propositions() and in GET /graph's proposition list, but the
// Reasoner iterates AllEdges() and would never traverse it, so a confirmed
// candidate edge silently fails to participate in any cost computation. The
// edge starts at EMAWeight == PriorWeight with zero confidence, matching the
// cold-start invariant every seeded edge satisfies (§4.3 of the paper).
func (m *SemanticMap) AddValidatedProposition(p *types.Proposition) error {
	if err := m.ontology.AddValidatedProposition(p); err != nil {
		return err
	}
	return m.storage.PutEdge(&types.EdgeDescriptor{
		FromID:        p.FromConstruct,
		ToID:          p.ToConstruct,
		PropositionID: p.PropositionID,
		Direction:     p.Direction,
		PriorWeight:   p.PriorStrength,
		EMAWeight:     p.PriorStrength,
		Confidence:    0.0,
		NObservations: 0,
	})
}

// ResetEdge restores every edge between (from, to) to its prior state.
func (m *SemanticMap) ResetEdge(from, to string) error {
	return m.updater.Reset(from, to)
}

// ── Operator tuning ───────────────────────────────────────────────────────────

// tuneFloor returns the minimum allowed strength for a proposition.
// SC-related propositions have a higher floor so security stays meaningful
// even under resource pressure. This duplicates the logic in
// internal/minimal/tuner.go — the two packages cannot import each other, so
// the function is intentionally duplicated here.
func tuneFloor(propID string) float64 {
	switch propID {
	case "P1", "P4", "P11", "P14":
		return 0.30
	default:
		return 0.10
	}
}

// Tune parses the operator's natural-language intent, resolves current
// proposition strengths, computes bounded adjustments, validates them,
// applies each via SetPropositionStrength, and records the consolidated
// intent in the audit log.
//
// Returns the list of adjustments that were actually applied. Returns
// (empty, nil) when the intent is unrecognized. Partial success is not
// possible: if any adjustment fails to apply, Tune returns the error after
// having applied all preceding adjustments (fail-forward, logged).
func (m *SemanticMap) Tune(text, operator string) ([]*types.TuneAdjustment, error) {
	if m.tuner == nil {
		return nil, nil
	}

	intents, err := m.tuner.ParseIntent(text)
	if err != nil {
		return nil, err
	}
	if len(intents) == 0 {
		return nil, nil
	}

	// Resolve current magnitudes from the storage edges, not from the ontology's
	// proposition strengths. The two differ by construction: prior_init seeds
	// edges from the per-distribution `distribution_edge_weights` table while
	// the ontology carries the global `propositions` table, so on a k0s-seeded
	// daemon P1's edge sits at 0.2138 against a proposition strength of 0.620.
	// Tuning has to nudge the value the Reasoner actually reads — anchoring the
	// delta to the global figure would jump a "+0.12 nudge" from 0.2138 to 0.740
	// and discard the per-distribution calibration in a single operator action.
	edges, err := m.storage.AllEdges()
	if err != nil {
		return nil, err
	}
	strengthByID := make(map[string]float64, len(edges))
	for _, e := range edges {
		strengthByID[e.PropositionID] = e.PriorWeight
	}

	// Build bounded adjustments.
	adjustments := make([]*types.TuneAdjustment, 0, len(intents))
	for _, intent := range intents {
		old, ok := strengthByID[intent.PropositionID]
		if !ok {
			continue // proposition not found — skip silently
		}
		floor := tuneFloor(intent.PropositionID)
		newS := old + intent.Delta
		if newS < floor {
			newS = floor
		}
		if newS > 0.95 {
			newS = 0.95
		}
		adjustments = append(adjustments, &types.TuneAdjustment{
			PropositionID: intent.PropositionID,
			OldStrength:   old,
			NewStrength:   newS,
			Rationale:     intent.Rationale,
		})
	}

	// Validate final values.
	if err := m.tuner.Validate(adjustments); err != nil {
		return nil, err
	}

	// Apply and collect results.
	//
	// This goes through the facade's SetPropositionStrength, not the ontology's,
	// so the new magnitude reaches the storage EdgeDescriptor the Reasoner reads.
	// Calling the ontology method directly would leave the tune visible in
	// Propositions() and in the audit log while changing no agent decision.
	var applied []*types.TuneAdjustment
	var appliedIDs []string
	for _, a := range adjustments {
		if err := m.SetPropositionStrength(a.PropositionID, a.NewStrength); err != nil {
			return applied, err
		}
		applied = append(applied, a)
		appliedIDs = append(appliedIDs, a.PropositionID)
	}

	// Best-effort audit record — don't fail tune on logging failure.
	if err := m.ontology.RecordTune(text, operator, appliedIDs); err != nil {
		log.Printf("SemanticMap.Tune: RecordTune failed: %v", err)
	}

	return applied, nil
}
