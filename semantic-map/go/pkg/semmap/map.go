// Package semmap provides the SemanticMap facade.
// Agent code imports only this package — never contract implementations directly.
package semmap

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/DiyazY/di-agent/pkg/contracts"
	"github.com/DiyazY/di-agent/pkg/domain"
	"github.com/DiyazY/di-agent/pkg/peers"
	"github.com/DiyazY/di-agent/pkg/statemap"
	"github.com/DiyazY/di-agent/pkg/types"
)

// neutralTuneAnchor is the base a tune delta uses for a relationship this machine has
// not measured. It is not a prior: nothing consults it unless an operator tunes, and
// the result is journalled as that operator's assertion.
const neutralTuneAnchor = 0.5

// SemanticMap wires the contracts and exposes the agent API. It also holds the peer
// coordination handles (peers.Registry, peers.Client) so the HTTP layer and the
// reasoner share a single source of truth for peers.
//
// It used to hold a storage graph and an updater as well, and they were the second
// model of the same relations: both learned from every sample, and after cost,
// estimates and explanations moved to the state model, only the storage copy's numbers
// were still being displayed. One model now — the state model — and the graph surfaces
// project it (see projection.go).
type SemanticMap struct {
	ontology contracts.OntologyContract
	reasoner contracts.ReasonerContract
	proposer contracts.ProposerContract
	tuner    contracts.TunerContract

	peers *peers.Registry
	peerc *peers.Client

	// state is the live state model: the properties this system exhibits and the
	// relationships between them. Every ingested sample updates it, and every answer
	// is read from it, so it holds what the system is doing now rather than what a
	// schema said it would do.
	//
	// Required in practice — profiles.Build always attaches one, because an agent
	// without it can answer nothing.
	state *statemap.Map

	// selfID is the machine this map models. The map is node-local: one agent
	// runs per machine and its map holds that machine's evidence, which is why
	// nothing here has a machine dimension — the whole model IS one machine's
	// state. Cluster-level questions are answered by asking peers, not by one
	// agent accumulating everyone's telemetry.
	//
	// Empty means the identity was never set, in which case the map cannot tell
	// its own samples from a peer's and foreign-sample rejection is disabled.
	selfID string

	// acceptForeign lets one map ingest telemetry labelled with other machines'
	// IDs, aggregating them into a single graph. Off by default because the
	// resulting edge weights are means over machines that may be different
	// physical systems — a Cortex-A72 worker and an x86 control-plane host do not
	// share a resource-to-pressure relation. The replay harness sets it
	// deliberately, and says so, because its dataset is a whole testbed.
	acceptForeign bool
}

// SetIdentity declares which machine this map models, and whether it may ingest
// samples belonging to other machines.
func (m *SemanticMap) SetIdentity(selfID string, acceptForeign bool) {
	m.selfID = selfID
	m.acceptForeign = acceptForeign
}

// SelfID returns the machine this map models, or "" if no identity was set.
func (m *SemanticMap) SelfID() string { return m.selfID }

// AcceptsForeignSamples reports whether this map aggregates other machines'
// telemetry. Exposed so an API boundary can explain a rejection.
func (m *SemanticMap) AcceptsForeignSamples() bool { return m.acceptForeign }

// ErrForeignSample is returned by IngestSample for a sample belonging to another
// machine while the map is in self-only scope. It is a configuration answer, not
// a transient failure: the caller should send the sample to that machine's own
// agent.
var ErrForeignSample = errors.New(
	"sample belongs to another machine: this map is node-local and models only its own")

// ErrNotModelled is returned when an operator names a proposition the agent declares but
// does not carry a relationship for. It is the caller naming something that does not
// exist here, not a fault: seeding skips a proposition whose endpoints are not both
// observable, so the claim is real and this agent simply has nothing to apply the action
// to. Transports should render it as a client error — reporting success would tell an
// operator their action took effect somewhere.
var ErrNotModelled = errors.New("proposition is declared but not modelled on this agent")

// New constructs a SemanticMap without peer coordination. The facade still
// satisfies its peer-facing methods (Peers, PeerClient) by lazily allocating
// an empty registry + default client on first access — preserving backward
// compatibility for callers that don't yet wire peers explicitly.
func New(
	ontology contracts.OntologyContract,
	reasoner contracts.ReasonerContract,
	proposer contracts.ProposerContract,
	tuner contracts.TunerContract,
) *SemanticMap {
	return &SemanticMap{
		ontology: ontology,
		reasoner: reasoner,
		proposer: proposer,
		tuner:    tuner,
	}
}

// NewWithPeers is the peer-aware constructor used by profiles.Build. Both
// peerRegistry and peerClient may be nil — Peers() and PeerClient() lazily
// fall back to fresh instances in that case.
func NewWithPeers(
	ontology contracts.OntologyContract,
	reasoner contracts.ReasonerContract,
	proposer contracts.ProposerContract,
	tuner contracts.TunerContract,
	peerRegistry *peers.Registry,
	peerClient *peers.Client,
) *SemanticMap {
	return &SemanticMap{
		ontology: ontology,
		reasoner: reasoner,
		proposer: proposer,
		tuner:    tuner,
		peers:    peerRegistry,
		peerc:    peerClient,
	}
}

// AttachState gives this map a state model to feed. Samples ingested afterwards
// create and update its properties, and a property that retires — by silence or by
// an operator — is forgotten by the proposer through the map's retire hook, which
// neither path can bypass.
func (m *SemanticMap) AttachState(s *statemap.Map) {
	m.state = s
	if s != nil && m.proposer != nil {
		s.SetRetireHook(func(id string) { _ = m.proposer.Forget(id) })
	}
}

// State returns the attached state model, or nil.
func (m *SemanticMap) State() *statemap.Map { return m.state }

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

// IngestSample records one MetricSample in the state model, and notifies the proposer
// of the property it observed.
//
// The whole path is now: observe the property, let the map recompute whatever derives
// from it, and let the map's own estimator fold the observation into the relationships
// incident to it. There used to be a second path alongside, fanning the sample out
// to every construct edge in a storage graph, with its own idempotency, its own EMA
// and its own relational variant. It learned the same relations from the same
// samples into a different structure — one structure too many, and it has since
// been removed.
//
// Every property, routed or not, scoped or not, is recorded and reaches the proposer.
// The routing table decides only the polarity in which an unscoped reading is expressed —
// before it is stored or learned from. A metric nobody routed is still recorded as an
// unrouted property; the HTTP layer reports such an admission with a 202 status code.
// What the routing table decides is which construct summarises an unrouted metric; what
// it does not decide is what the system is allowed to exhibit.
//
// Errors from the proposer are swallowed: it is advisory, and must not block telemetry.
func (m *SemanticMap) IngestSample(sample *types.MetricSample) error {
	if sample == nil {
		return nil
	}
	if m.selfID != "" && !m.acceptForeign && sample.NodeID != "" && sample.NodeID != m.selfID {
		return fmt.Errorf("%w: sample node_id=%q, this agent is %q",
			ErrForeignSample, sample.NodeID, m.selfID)
	}
	if m.state == nil {
		return ErrNoStateModel
	}

	// A scoped sample is a property of something narrower than the node — its id
	// carries the subject — and it is never routed or normalised: polarity and
	// construct membership are node-level concerns, so a scoped reading of a routed
	// metric type still passes through untouched. An unscoped sample is expressed in
	// its construct's polarity before anything stores or learns from it, because a
	// metric that runs opposite to the construct it informs would otherwise pull that
	// construct's summary the wrong way, and every relationship learned from it would
	// be asked to agree with a sign the reading contradicts; an unrouted or
	// same-polarity metric passes through untouched either way.
	id := string(sample.MetricType)
	value := sample.Value
	router := m.router()
	if sample.Subject != "" {
		id = id + "@" + sample.Subject
	} else if router != nil {
		value = router.NormalizeForConstruct(string(sample.MetricType), value)
	}

	if err := m.state.Record(statemap.Observation{
		ID: id, Value: value, At: time.Unix(sample.TimestampUnix, 0), EventID: sample.EventID,
		Subject: sample.Subject, Unit: sample.Unit, Range: sample.Range,
		Source: sample.Source, Labels: sample.Labels,
	}); err != nil {
		return err
	}

	// Every observed property reaches the proposer; it applies the scope rule itself.
	// Advisory: its errors must not block telemetry.
	if m.proposer != nil {
		_ = m.proposer.ObserveProperty(id, sample.Subject, value, time.Unix(sample.TimestampUnix, 0))
	}
	return nil
}

// RoutedConstruct reports which construct a metric type is routed to by the
// loaded domain specification, and whether it is routed at all. Exposed so an
// API boundary can reject an unrouted type loudly without keeping its own copy
// of the routing table.
func (m *SemanticMap) RoutedConstruct(metricType string) (string, bool) {
	router := m.router()
	if router == nil {
		return "", false
	}
	return router.ConstructForMetric(metricType)
}

// assertStateStrength applies an operator strength adjustment to every state-model
// relationship carrying this proposition as its label.
//
// Resolving by label rather than by a constructed ID keeps the two naming schemes
// independent: the state model is free to carry relationships the construct graph
// does not.
// It returns how many relationships it touched, so a caller can tell an assertion that
// took effect from one that named a proposition this agent declares but does not model.
func (m *SemanticMap) assertStateStrength(propositionID string, strength float64, actor, reason string) int {
	if m.state == nil {
		return 0
	}
	var n int
	for _, r := range m.state.Relationships("", "") {
		if r.Label != propositionID {
			continue
		}
		if err := m.state.AssertRelationshipStrength(r.ID, strength, actor, reason); err != nil {
			log.Printf("statemap: asserting %s: %v", r.ID, err)
			continue
		}
		n++
	}
	return n
}

// retireStateRelationships retires every state-model relationship labelled with this
// proposition, so a withdrawn claim leaves the reasoning path.
// It returns how many relationships it retired, for the reason assertStateStrength
// gives.
func (m *SemanticMap) retireStateRelationships(propositionID, reason, actor string) int {
	if m.state == nil {
		return 0
	}
	var n int
	for _, r := range m.state.Relationships("", "") {
		if r.Label != propositionID {
			continue
		}
		if err := m.state.RetireRelationship(r.ID, reason, actor); err != nil {
			log.Printf("statemap: retiring %s: %v", r.ID, err)
			continue
		}
		n++
	}
	return n
}

// MetricRouter maps a metric type to the construct that summarises it, and
// expresses a raw reading in that construct's polarity.
type MetricRouter interface {
	ConstructForMetric(metricType string) (string, bool)
	// NormalizeForConstruct reflects a value within its declared range when the
	// metric and its construct run in opposite directions, and returns it unchanged
	// otherwise. An unrouted metric is returned unchanged.
	NormalizeForConstruct(metricType string, value float64) float64
}

// SpecCarrier is implemented by ontologies built from a domain specification. The
// facade uses it to recover the routing table without threading the spec through
// every constructor.
type SpecCarrier interface {
	Spec() *domain.Spec
}

// router returns the metric routing table for this map, which is the loaded
// domain specification. Nil when the ontology carries no specification, in which
// case the proposer simply hears nothing: a metric routed to a construct the
// deployment never declared would put evidence under a name nobody asked for.
func (m *SemanticMap) router() MetricRouter {
	if c, ok := m.ontology.(SpecCarrier); ok {
		if s := c.Spec(); s != nil {
			return s
		}
	}
	return nil
}

// ── Graph extension ───────────────────────────────────────────────────────────

func (m *SemanticMap) PendingCandidates() ([]*types.CandidateEdge, error) {
	return m.proposer.GetCandidates()
}

// CandidateHistory returns every candidate the proposer has ever emitted with its
// current status — the audit surface for discovery, including deferred ones.
func (m *SemanticMap) CandidateHistory() ([]*types.CandidateEdge, error) {
	return m.proposer.GetHistory()
}

// ConfirmCandidate applies an operator's confirmation. A candidate with a scoped
// endpoint becomes a state-map relationship with Discovered provenance — never a
// Di-Select proposition, whose vocabulary is constructs. A candidate whose endpoints
// are both unscoped (the explicit construct path) goes through the ontology as before.
func (m *SemanticMap) ConfirmCandidate(candidateID string) error {
	prop, err := m.proposer.Confirm(candidateID)
	if err != nil {
		return err
	}
	if prop == nil {
		return nil // nothing to add: a disabled proposer, or already confirmed
	}
	if m.state != nil && (m.isScoped(prop.FromConstruct) || m.isScoped(prop.ToConstruct)) {
		sign := 1
		if prop.Direction == types.Negative {
			sign = -1
		}
		return m.state.DeclareRelationship(statemap.Relationship{
			From: prop.FromConstruct, To: prop.ToConstruct, Label: "discovered", Sign: sign,
			Provenance: statemap.Discovered,
			Note:       "[discovered: " + prop.Description + "; confirmed by operator]",
		})
	}
	// The facade applies it, because this is the only path that reaches both the
	// declaration and the state model. A confirmed candidate that landed only in the
	// declaration would appear in Propositions() and take part in no answer.
	return m.AddValidatedProposition(prop)
}

func (m *SemanticMap) isScoped(id string) bool {
	p, ok := m.state.Property(id)
	return ok && p.Subject != ""
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

// Propositions returns every declared proposition, with the strength in force and the
// retirement status overlaid from the state model. Deprecated ones are included, flagged
// via Proposition.Deprecated; a proposition the agent declares but does not model
// carries Instantiated false. See projectedPropositions for why the overlay exists.
func (m *SemanticMap) Propositions() ([]*types.Proposition, error) {
	return m.projectedPropositions()
}

// AllEdges returns every relation the agent holds, rendered as edge descriptors.
//
// Read from the state model, which is where relations live and learn. It read from
// storage until the state model became the single source of every answer, at which
// point this surface was showing an operator numbers that entered no decision — see
// projection.go for why that is worse than showing nothing.
func (m *SemanticMap) AllEdges() ([]*types.EdgeDescriptor, error) {
	if m.state == nil {
		return nil, ErrNoStateModel
	}
	return m.projectedEdges(), nil
}

// EdgesByPair returns every relation between (from, to). Endpoints carrying a conflict
// pair — two claims in opposite directions over the same pair — yield more than one
// descriptor, which is the case the multigraph exists for.
func (m *SemanticMap) EdgesByPair(from, to string) ([]*types.EdgeDescriptor, error) {
	if m.state == nil {
		return nil, ErrNoStateModel
	}
	rels := m.state.Relationships(from, to)
	out := make([]*types.EdgeDescriptor, 0, len(rels))
	for _, r := range rels {
		out = append(out, edgeFromRelationship(r))
	}
	return out, nil
}

// Neighbors returns the properties reachable from nodeID via one outgoing relation.
func (m *SemanticMap) Neighbors(nodeID string) ([]string, error) {
	if m.state == nil {
		return nil, ErrNoStateModel
	}
	seen := map[string]bool{}
	var out []string
	for _, r := range m.state.Relationships(nodeID, "") {
		if r.Status == statemap.Retired || seen[r.To] {
			continue
		}
		seen[r.To] = true
		out = append(out, r.To)
	}
	sort.Strings(out)
	return out, nil
}

// History returns mutation events recorded at or after `since`, from the state model's
// journal — the one record there is. Pass the zero time.Time for everything held.
//
// The journal is bounded, so "everything held" is not "everything that happened";
// GET /state/journal reports how many entries were dropped, and a caller that needs to
// know whether the record is complete should read it there.
func (m *SemanticMap) History(since time.Time) ([]*types.OntologyEvent, error) {
	return m.projectedHistory(since)
}

// ── Write-side facade (ontology mutations) ────────────────────────────────────

// SetPropositionStrength recalibrates a proposition's strength: it asserts the value on
// every relationship carrying that proposition as its label, and journals the assertion
// with actor and reason.
//
// This is the write path for "this claim's magnitude should change because better
// knowledge arrived" — a fresh scan, a new paper, an operator tune. It writes one place,
// because there is one place: the strength this changes is the strength every answer
// reads, and /propositions reports it by overlay rather than by keeping a copy. The
// arrangement it replaced wrote both a declaration and a model, and the period when only
// the first write landed is documented in §7.3 of the P6 paper.
//
// The value is absolute: a new scan yields a new magnitude, not a delta. Callers that
// think in deltas (see Tune) resolve the current value first and pass the result.
//
// What it writes is the PRIOR, and what was learned from this system is left untouched.
// Those are separate fields precisely so that recalibrating an assumption cannot rewrite
// observation history, and the effective strength blends the two by confidence — so on a
// well-observed relationship an operator's number moves the answer very little. That is
// the arithmetic behaving as designed rather than the write failing, and §7.3 measures
// it. Asserting deliberately supersedes the per-cluster calibration that prior_init
// seeded: an operator asserting a value for *this* deployment outranks a cross-cluster
// estimate, the provenance field records that it is now an assertion, and cold-start
// equivalence to Di-Select (paper §4.3) is a property of the state at c = 0 before any
// operator action rather than an invariant for all time.
//
// Returns an error when no relationship carries the proposition — a declared claim the
// agent does not model cannot be recalibrated, and reporting success would tell an
// operator their action took effect somewhere.
func (m *SemanticMap) SetPropositionStrength(id string, strength float64) error {
	if m.state == nil {
		return ErrNoStateModel
	}
	if n := m.assertStateStrength(id, strength, "operator", "proposition strength set"); n == 0 {
		return fmt.Errorf("%w: no relationship carries %q, so there is no strength to set",
			ErrNotModelled, id)
	}
	return nil
}

// Deprecate withdraws a proposition (soft delete): it retires every relationship
// carrying that proposition as its label, which takes the claim out of the traversal so
// no answer consults it, and leaves it retrievable with its evidence and a stated reason.
//
// Retirement in the state model is the whole operation. The declaration keeps reporting
// the proposition — flagged deprecated by the overlay of projectedPropositions — because
// history and replay need it to still be there, and because a decision taken before the
// withdrawal must remain reconstructible.
//
// Returns an error when no relationship carries the proposition, for the reason
// SetPropositionStrength gives.
func (m *SemanticMap) Deprecate(id, reason string) error {
	if m.state == nil {
		return ErrNoStateModel
	}
	if n := m.retireStateRelationships(id, reason, "operator"); n == 0 {
		return fmt.Errorf("%w: no relationship carries %q, so there is nothing to withdraw",
			ErrNotModelled, id)
	}
	return nil
}

// AddConstruct appends a new construct to the ontology and declares the matching
// property in the state model.
//
// The state-model write is the operative half, for the same reason as everywhere else:
// a construct that exists only as a declaration appears in Propositions() and in
// GET /graph and takes part in no answer.
//
// It is declared as observed rather than derived, which is not a technicality. A
// derived property must have members, and nothing routes to a construct added at
// runtime — the routing table is specification data, so it cannot gain members until
// the spec changes. Declaring it derived and memberless would make it a summary of
// nothing. Observed says the truthful thing instead: its value can only come from
// outside, and until something supplies one it reports zero at zero confidence. If a
// later specification routes metrics to it, seeding re-declares it as derived with
// those members.
func (m *SemanticMap) AddConstruct(c *types.Construct) error {
	if err := m.ontology.AddConstruct(c); err != nil {
		return err
	}
	if m.state == nil {
		return nil
	}
	return m.state.DeclareProperty(statemap.Property{
		ID:     c.ConstructID,
		Kind:   statemap.Observed,
		Range:  [2]float64{0, 1},
		Source: "operator: " + c.Name + " (no metric routed yet)",
	})
}

// AddValidatedProposition appends a new proposition after the ontology has validated it
// against the existing backbone, and declares the matching relationship in the state
// model.
//
// The second half is what makes a confirmed candidate mean anything. Without it the
// proposition appears in Propositions() and in GET /graph's proposition list while
// every answer is computed from relationships that have never heard of it — so a
// candidate an operator confirmed would sit in the backbone influencing nothing. It
// starts at its prior with zero confidence, which is the cold-start state every seeded
// relationship is in.
func (m *SemanticMap) AddValidatedProposition(p *types.Proposition) error {
	if err := m.ontology.AddValidatedProposition(p); err != nil {
		return err
	}
	if m.state == nil {
		return nil
	}
	// Both endpoints have to exist as properties before a relationship between them can
	// be declared. Seeding skips a construct nothing routes to — correctly, since it
	// would summarise nothing — so a proposition an operator adds may name one. Declaring
	// the missing endpoint here keeps the declaration and the model from diverging, which
	// is the failure this whole path exists to prevent. It is observed, for the reason
	// AddConstruct gives: its value can only come from outside, and until something
	// supplies one it reports zero at zero confidence.
	for _, id := range []string{p.FromConstruct, p.ToConstruct} {
		if _, exists := m.state.Property(id); exists {
			continue
		}
		if err := m.state.DeclareProperty(statemap.Property{
			ID: id, Kind: statemap.Observed, Range: [2]float64{0, 1},
			Source: "endpoint of proposition " + p.PropositionID + " (no metric routed)",
		}); err != nil {
			return err
		}
	}
	sign := 1
	if p.Direction == types.Negative {
		sign = -1
	}
	return m.state.DeclareRelationship(statemap.Relationship{
		From: p.FromConstruct, To: p.ToConstruct, Label: p.PropositionID,
		Sign: sign, Provenance: statemap.Seeded,
		Note: "[declared: proposition " + p.PropositionID + "; strength is learned]",
	})
}

// ResetEdge discards what was learned about every relationship between (from, to),
// leaving each at its prior.
func (m *SemanticMap) ResetEdge(from, to string) error {
	if m.state == nil {
		return ErrNoStateModel
	}
	rels := m.state.Relationships(from, to)
	if len(rels) == 0 {
		return fmt.Errorf("no relationship from %q to %q", from, to)
	}
	for _, r := range rels {
		if err := m.state.ResetRelationship(r.ID, "operator", "reset requested"); err != nil {
			return err
		}
	}
	return nil
}

// ── Operator tuning ───────────────────────────────────────────────────────────

// tuneFloor returns the minimum allowed strength for a proposition, from the
// domain specification the ontology carries.
//
// This was previously a hardcoded switch naming four propositions, duplicated
// from internal/minimal/tuner.go because the two packages cannot import each
// other. Both copies named propositions that a later change to the graph's scope
// removed, so both became dead policy that still looked authoritative. Reading
// the spec removes the duplication and the staleness together: a proposition
// added at runtime gets a floor without either package being rebuilt.
// tuneCeiling mirrors tuneFloor for the upper bound.
func (m *SemanticMap) tuneCeiling() float64 {
	if o, ok := m.ontology.(interface{ Spec() *domain.Spec }); ok {
		if s := o.Spec(); s != nil {
			return s.Policy.GlobalCeiling
		}
	}
	return 0.95
}

func (m *SemanticMap) tuneFloor(propID string) float64 {
	if o, ok := m.ontology.(interface{ Spec() *domain.Spec }); ok {
		if s := o.Spec(); s != nil {
			return s.FloorFor(propID)
		}
	}
	return 0.10 // conservative fallback for an ontology that exposes no spec
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

	// Resolve current magnitudes from the state model's relationships, which carry this
	// cluster's calibrated priors. The delta has to be anchored to the value in force,
	// not to the cross-cluster proposition strength: anchoring to the global figure
	// would turn a small nudge into a jump and discard the per-cluster calibration in
	// one operator action.
	if m.state == nil {
		return nil, ErrNoStateModel
	}
	strengthByID := map[string]float64{}
	anchored := map[string]bool{}
	for _, r := range m.state.Relationships("", "") {
		if r.Status == statemap.Retired || r.Label == "" {
			continue
		}
		// A tune is expressed as a delta, so it needs a base. The base is whatever the
		// relationship currently stands at — an assertion, else what the machine has
		// established, else its recent estimate.
		//
		// A relationship the machine has not measured has no base. It is anchored to the
		// neutral midpoint instead of skipped, because cold start is when an operator's
		// knowledge is worth most — "this link is lossy, I have run it for years" is
		// exactly the claim an agent cannot yet make for itself. The anchoring is
		// recorded on the adjustment, and the result is an assertion with an actor and a
		// reason: unlike the seeded prior this replaced, a placeholder here exists only
		// because somebody asked for it and is attributed to them.
		if v, known := r.Effective(); known {
			strengthByID[r.Label] = v
		} else {
			strengthByID[r.Label] = neutralTuneAnchor
			anchored[r.Label] = true
		}
	}

	// Build bounded adjustments.
	adjustments := make([]*types.TuneAdjustment, 0, len(intents))
	for _, intent := range intents {
		old, ok := strengthByID[intent.PropositionID]
		if !ok {
			continue // proposition not found — skip silently
		}
		floor := m.tuneFloor(intent.PropositionID)
		newS := old + intent.Delta
		if newS < floor {
			newS = floor
		}
		if newS > m.tuneCeiling() {
			newS = 0.95
		}
		rationale := intent.Rationale
		if anchored[intent.PropositionID] {
			rationale += " [anchored to the neutral midpoint: this machine has not " +
				"measured this relationship, so the delta had no observed base]"
		}
		adjustments = append(adjustments, &types.TuneAdjustment{
			PropositionID: intent.PropositionID,
			OldStrength:   old,
			NewStrength:   newS,
			Rationale:     rationale,
		})
	}

	// Validate final values.
	if err := m.tuner.Validate(adjustments); err != nil {
		return nil, err
	}

	// Apply and collect results.
	//
	// This goes through the facade's SetPropositionStrength, not the ontology's, so
	// the new magnitude reaches both the storage EdgeDescriptor and the state model's
	// relationship — the latter being what the Reasoner reads. Calling the ontology
	// method directly would leave the tune visible in Propositions() and in the audit
	// log while changing no agent decision.
	var applied []*types.TuneAdjustment
	var appliedIDs []string
	for _, a := range adjustments {
		if err := m.SetPropositionStrength(a.PropositionID, a.NewStrength); err != nil {
			return applied, err
		}
		applied = append(applied, a)
		appliedIDs = append(appliedIDs, a.PropositionID)
	}

	// The consolidated act, recorded alongside the individual assertions it produced.
	// Reading those separately afterwards gives no way to tell one coordinated
	// adjustment from several unrelated ones that landed together.
	if m.state != nil {
		m.state.RecordOperatorIntent(text, operator, appliedIDs)
	}

	return applied, nil
}
