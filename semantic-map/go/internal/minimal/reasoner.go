package minimal

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"github.com/DiyazY/di-agent/pkg/contracts"
	"github.com/DiyazY/di-agent/pkg/domain"
	"github.com/DiyazY/di-agent/pkg/peers"
	"github.com/DiyazY/di-agent/pkg/types"
)

// RuleEngineReasoner is the edge-minimal ReasonerContract implementation.
//
// All decisions are deterministic: the reasoner traverses the Semantic Map
// graph using current blended values (prior + EMA weighted by confidence)
// and applies proposition-based rules. No ML model is involved.
//
// Blending formula:
//
//	effective = (1 - confidence) * prior + confidence * ema
//
// RecommendPeer goes one step further than CostOfAction: it queries each
// registered peer's /cost endpoint via peers.Client, filters out anyone
// below the configured trust floor, and ranks the survivors by
// trust-weighted savings (myEnergy − peerEnergy) × peer.Trust.
type RuleEngineReasoner struct {
	storage       contracts.StorageContract
	ontology      contracts.OntologyContract
	minTrustScore float64

	// peers is the live registry of remote agents this reasoner can offload
	// to. nil is permitted (and equivalent to an empty registry): RecommendPeer
	// returns ErrInsufficientTrust with an explanatory rationale when there is
	// no one to query.
	peers *peers.Registry

	// peerc is the HTTP client used to talk to remote peers. nil is permitted
	// alongside an empty registry — RecommendPeer never reaches it in that
	// case. When set, transport errors are logged and treated as a soft trust
	// penalty (peerPenalty below); they never abort the recommendation run.
	peerc *peers.Client
}

// peerPenalty is the trust delta applied to a peer that fails a Cost query.
// Small enough that a single transient blip barely registers; large enough
// that a peer that is persistently down drains out of the eligible set after
// ~20 attempts.
const peerPenalty = -0.05

// peerCostQueryTimeout caps a single RecommendPeer pass when the caller does
// not supply a context. Generous (3s) — peers are LAN-local in v1, but the
// daemon must not block on a hung peer forever.
const peerCostQueryTimeout = 3 * time.Second

// NewRuleEngineReasoner constructs the edge-minimal reasoner.
//
// peerRegistry and peerClient are optional. Pass nil for both when the
// profile has no peers configured — RecommendPeer will simply return
// ErrInsufficientTrust ("no peers registered") with a clear rationale.
// Compliance tests rely on this graceful-no-peers behavior.
func NewRuleEngineReasoner(
	storage contracts.StorageContract,
	ontology contracts.OntologyContract,
	minTrustScore float64,
	peerRegistry *peers.Registry,
	peerClient *peers.Client,
) *RuleEngineReasoner {
	return &RuleEngineReasoner{
		storage:       storage,
		ontology:      ontology,
		minTrustScore: minTrustScore,
		peers:         peerRegistry,
		peerc:         peerClient,
	}
}

// CostOfAction estimates what work costs on THIS machine, and how sensitive that
// cost is to a change in load.
//
// The estimate has two halves, and keeping them apart is the substance of the
// design rather than presentation:
//
//	level        the confidence-blended OBSERVED value of the cost construct,
//	             read from its node descriptor. On a node-local map that is this
//	             machine's current resource use and current experienced pressure.
//	sensitivity  the weighted sum over the edges terminating at that construct,
//	             each signed by its proposition's direction. How much the target
//	             would move per unit change in a source construct.
//
// An earlier version had no level at all: it summed edge weights and reported the
// result as a latency. That quantity carried no observed magnitude, so it could not
// distinguish a busy machine from an idle one, and a measurement over 182 replayed
// runs found it no better than the static priors at predicting which node was about
// to be pressured — while the observed level alone was the best predictor available.
// The same measurement swept the coefficient on the relation term and found that
// adding it to the level degraded the prediction monotonically. Hence: levels lead,
// sensitivities are reported alongside, and SimulateOutcome is where a sensitivity
// is actually applied, because a counterfactual is the question a level cannot
// answer.
//
// Which construct plays which role comes from the domain specification's cost_model
// block. This function previously hardcoded the two IDs and was the last place in
// the daemon that knew a construct by name.
//
// Deprecated propositions are filtered out via a one-time lookup against the
// Ontology before edge iteration begins. The Ontology is the source of truth for
// what is endorsed; Storage holds descriptors regardless so the audit trail is
// preserved.
func (r *RuleEngineReasoner) CostOfAction(taskType, nodeID string) (*types.ActionCost, error) {
	resourceID, pressureID, err := r.costConstructs()
	if err != nil {
		return nil, err
	}

	deprecated, err := r.deprecatedPropositionSet()
	if err != nil {
		return nil, err
	}

	edges, err := r.storage.AllEdges()
	if err != nil {
		return nil, err
	}

	var resourceSens, pressureSens float64
	var confidenceSum float64
	var counted int
	var path []string

	for _, e := range edges {
		if deprecated[e.PropositionID] {
			continue
		}
		effective := blend(e)
		path = append(path, fmt.Sprintf("%s→%s[%s](%.2f)",
			e.FromID, e.ToID, e.PropositionID, effective))
		confidenceSum += e.Confidence
		counted++

		switch e.ToID {
		case resourceID:
			resourceSens += effective * sign(e.Direction)
		case pressureID:
			pressureSens += effective * sign(e.Direction)
		}
	}

	// Levels come from the construct descriptors. A construct with no observations
	// blends to its neutral prior, which is the honest cold-start answer: the agent
	// does not yet know this machine's resource state, and says so through the
	// confidence it reports rather than by inventing a level.
	resourceLevel, resourceConf, err := r.constructLevel(resourceID)
	if err != nil {
		return nil, err
	}
	pressureLevel, pressureConf, err := r.constructLevel(pressureID)
	if err != nil {
		return nil, err
	}

	// Confidence averages the edges and the two cost constructs together: a caller
	// asking how much to trust this estimate is asking about both halves, since the
	// levels carry the magnitudes and the edges carry the sensitivities.
	confidenceSum += resourceConf + pressureConf
	counted += 2
	var confidence float64
	if counted > 0 {
		confidence = confidenceSum / float64(counted)
	}

	who := nodeID
	if who == "" {
		who = "self"
	}
	return &types.ActionCost{
		CPUCost:             math.Max(0, resourceLevel*0.1), // proxy until an energy collector lands
		ResourceCost:        math.Max(0, resourceLevel),
		EnergyCost:          0,
		LatencyEstimate:     math.Max(0, pressureLevel),
		Confidence:          confidence,
		ResourceSensitivity: resourceSens,
		PressureSensitivity: pressureSens,
		Rationale: fmt.Sprintf(
			"task=%s node=%s %s_level=%.4f (c=%.2f) %s_level=%.4f (c=%.2f) "+
				"d%s/dsource=%+.4f d%s/dsource=%+.4f path=[%s]",
			taskType, who, resourceID, resourceLevel, resourceConf,
			pressureID, pressureLevel, pressureConf,
			resourceID, resourceSens, pressureID, pressureSens,
			strings.Join(path, ", ")),
		GraphPathUsed: path,
	}, nil
}

// costConstructs resolves the cost roles from the loaded domain specification.
// An ontology that carries no specification cannot name them, and guessing would
// reintroduce exactly the hardcoding this removes.
func (r *RuleEngineReasoner) costConstructs() (resource, pressure string, err error) {
	carrier, ok := r.ontology.(interface{ Spec() *domain.Spec })
	if !ok || carrier.Spec() == nil {
		return "", "", fmt.Errorf("reasoner needs a domain specification to know which " +
			"construct is the resource cost and which is the pressure penalty")
	}
	cm := carrier.Spec().CostModel
	if cm.ResourceConstruct == "" || cm.PressureConstruct == "" {
		return "", "", fmt.Errorf("domain specification declares no cost_model roles")
	}
	return cm.ResourceConstruct, cm.PressureConstruct, nil
}

// constructLevel returns the confidence-blended observed value of one construct
// and the confidence behind it. A construct absent from storage is reported as a
// zero-confidence 0.5 — the neutral prior — rather than as an error, so a graph
// seeded before a construct was added still answers.
func (r *RuleEngineReasoner) constructLevel(constructID string) (level, confidence float64, err error) {
	node, err := r.storage.GetNode(constructID)
	if err != nil {
		return 0, 0, err
	}
	if node == nil {
		return 0.5, 0, nil
	}
	c := node.Confidence
	return (1-c)*node.PriorValue + c*node.EMAValue, c, nil
}

// deprecatedPropositionSet returns the set of PropositionIDs that the
// Ontology no longer endorses. Read once per CostOfAction call to keep the
// hot loop simple.
func (r *RuleEngineReasoner) deprecatedPropositionSet() (map[string]bool, error) {
	props, err := r.ontology.Propositions()
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool)
	for _, p := range props {
		if p.Deprecated {
			out[p.PropositionID] = true
		}
	}
	return out, nil
}

// RecommendPeer ranks every registered peer by trust-weighted savings and
// returns the best candidate. The algorithm in steps:
//
//  1. List the peer registry. Empty → ErrInsufficientTrust with rationale
//     "no peers registered".
//  2. Compute the local CostOfAction once.
//  3. For each peer with Trust ≥ minTrustScore, GET /cost on the peer URL.
//     a. Success → MarkSeen on the registry; compute savings as
//     (myEnergy − peerEnergy); compute trust-weighted savings as
//     savings × peer.Trust.
//     b. Failure → log via log.Printf and apply peerPenalty to the peer's
//     trust score. Skip this peer; do not abort the run. The reasoner
//     must remain useful when one peer is down.
//  4. Pick the peer with the highest trust-weighted savings. If no peer
//     beats local cost (savings ≤ 0 everywhere) → ErrInsufficientTrust.
//  5. Build a PeerRecommendation citing the peer ID, the trust score we
//     weighted by, and the peer's reported GraphPathUsed.
//
// Context: this contract method does not take a context.Context. We use a
// per-call context with peerCostQueryTimeout to bound the total wall-clock.
// When the ReasonerContract is widened to accept a ctx in a future revision,
// it will flow through here directly.
func (r *RuleEngineReasoner) RecommendPeer(octx *types.OffloadContext) (*types.PeerRecommendation, error) {
	if r.peers == nil {
		return nil, fmt.Errorf("%w: no peer registry configured", contracts.ErrInsufficientTrust)
	}
	list, err := r.peers.List()
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("%w: no peers registered", contracts.ErrInsufficientTrust)
	}

	myCost, err := r.CostOfAction(octx.TaskType, octx.SourceNodeID)
	if err != nil {
		return nil, err
	}

	// Per-call context. Honors the caller's eventual context wiring once the
	// contract is widened — for now context.TODO() with a bounded timeout.
	ctx, cancel := context.WithTimeout(context.TODO(), peerCostQueryTimeout)
	defer cancel()

	type ranked struct {
		peer     *peers.Descriptor
		savings  float64
		weighted float64
		path     []string
	}

	var best ranked
	bestSet := false
	var skippedBelowTrust, skippedHTTPError, skippedNoSavings int

	for _, p := range list {
		if p.Trust < r.minTrustScore {
			skippedBelowTrust++
			continue
		}
		if r.peerc == nil {
			// Registry has entries but no client — treat as soft-fail. Log so
			// operators notice the misconfiguration but keep the loop alive.
			log.Printf("reasoner.RecommendPeer: no peer client configured; skipping peer %s", p.ID)
			skippedHTTPError++
			continue
		}
		peerCost, err := r.peerc.Cost(ctx, p.URL, octx.TaskType, octx.SourceNodeID)
		if err != nil {
			log.Printf("reasoner.RecommendPeer: peer %s (%s) cost query failed: %v", p.ID, p.URL, err)
			// Soft trust penalty so persistently-down peers drain out of the
			// eligible set without hard-banning them on first failure.
			if perr := r.peers.UpdateTrust(p.ID, peerPenalty); perr != nil {
				log.Printf("reasoner.RecommendPeer: penalty update failed: %v", perr)
			}
			skippedHTTPError++
			continue
		}
		// Successful query — record contact.
		if perr := r.peers.MarkSeen(p.ID, time.Now()); perr != nil {
			log.Printf("reasoner.RecommendPeer: MarkSeen failed: %v", perr)
		}
		savings := myCost.ResourceCost - peerCost.ResourceCost
		weighted := savings * p.Trust
		if savings <= 0 {
			skippedNoSavings++
			continue
		}
		if !bestSet || weighted > best.weighted {
			best = ranked{peer: p, savings: savings, weighted: weighted, path: peerCost.GraphPathUsed}
			bestSet = true
		}
	}

	if !bestSet {
		return nil, fmt.Errorf("%w: %d peers below trust floor, %d http errors, %d had no savings (myResourceCost=%.3f)",
			contracts.ErrInsufficientTrust,
			skippedBelowTrust, skippedHTTPError, skippedNoSavings, myCost.ResourceCost)
	}

	return &types.PeerRecommendation{
		PeerID:          best.peer.ID,
		ExpectedSavings: best.savings,
		Rationale: fmt.Sprintf(
			"peer=%s (url=%s trust=%.2f) saves %.3f resource cost vs local (%.3f); trust-weighted=%.3f; peer path=[%s]",
			best.peer.ID, best.peer.URL, best.peer.Trust, best.savings, myCost.ResourceCost, best.weighted,
			strings.Join(best.path, ", "),
		),
		GraphPathUsed: best.path,
	}, nil
}

func (r *RuleEngineReasoner) SimulateOutcome(octx *types.OffloadContext, targetNodeID string) (*types.OutcomeSimulation, error) {
	cost, err := r.CostOfAction(octx.TaskType, targetNodeID)
	if err != nil {
		return nil, err
	}

	// A simulation is a counterfactual: the machine is not running this task yet,
	// so its observed levels do not include the task's load. This is where the
	// sensitivities earn their place — they convert an assumed increase in resource
	// demand into an expected movement of each cost construct. The level answers
	// "what is it now" and cannot answer this; the relations can, and are fully
	// informed from the calibrated priors even at cold start.
	//
	// The assumed demand is deliberately crude: data size against a reference,
	// clamped to [0,1], because the OffloadContext carries no better description of
	// what the task will do. A collector that reported per-task demand would replace
	// this without touching the structure — the term is `sensitivity x demand`
	// either way.
	demand := assumedDemand(octx)
	expectedResource := cost.ResourceCost + cost.ResourceSensitivity*demand
	expectedLatency := cost.LatencyEstimate + cost.PressureSensitivity*demand

	var riskFlags []string
	if octx.LatencyBudgetMs > 0 && expectedLatency > octx.LatencyBudgetMs {
		riskFlags = append(riskFlags, fmt.Sprintf("latency %.1fms exceeds budget %.1fms", expectedLatency, octx.LatencyBudgetMs))
	}
	if octx.EnergyBudgetJoules != nil && expectedResource > *octx.EnergyBudgetJoules {
		riskFlags = append(riskFlags, fmt.Sprintf("resource cost %.3f exceeds energy budget %.3fJ", expectedResource, *octx.EnergyBudgetJoules))
	}

	return &types.OutcomeSimulation{
		ExpectedLatency:      math.Max(0, expectedLatency),
		ExpectedResourceCost: math.Max(0, expectedResource),
		Confidence:           cost.Confidence,
		GraphPathUsed:        cost.GraphPathUsed,
		RiskFlags:            riskFlags,
		// P95 estimates require Gaussian descriptors (edge-standard+); nil here.
	}, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func blend(e *types.EdgeDescriptor) float64 {
	return (1-e.Confidence)*e.PriorWeight + e.Confidence*e.EMAWeight
}

func sign(d types.Direction) float64 {
	if d == types.Positive {
		return 1.0
	}
	return -1.0
}

// assumedDemand converts an offload request into a unit-less [0,1] increase in
// resource demand, for the sensitivity term in SimulateOutcome.
//
// referenceBytes is the payload at which a task is treated as saturating the
// machine. It is a placeholder for a real demand estimate, and it is stated as a
// constant here rather than buried in an expression so that its arbitrariness is
// visible: nothing in the dataset calibrates it.
func assumedDemand(octx *types.OffloadContext) float64 {
	const referenceBytes = 64 * 1024 * 1024
	if octx == nil || octx.DataSizeBytes <= 0 {
		return 0
	}
	d := float64(octx.DataSizeBytes) / referenceBytes
	if d > 1 {
		return 1
	}
	return d
}
