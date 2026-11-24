package compliance

import (
	"errors"
	"testing"

	"github.com/DiyazY/di-agent/pkg/contracts"
	"github.com/DiyazY/di-agent/pkg/types"
)

// ReasonerFactory builds a fresh ReasonerContract for one compliance subtest.
// The factory is responsible for seeding any storage/ontology state the
// reasoner depends on — the compliance suite assumes the returned reasoner
// is fully usable (e.g. backbone propositions present, at least one peer
// reachable for RecommendPeer if the implementation supports peers).
type ReasonerFactory func(t *testing.T) contracts.ReasonerContract

// RunReasonerCompliance verifies the behavioral guarantees of ReasonerContract:
//
//   - Traceable rationale: CostOfAction returns a non-empty Rationale.
//   - Graph path populated: CostOfAction returns at least one GraphPathUsed entry.
//   - Confidence in [0, 1] for all queries.
//   - RecommendPeer returns either a rationale, ErrInsufficientTrust, or ErrPeerCostQuery.
//   - SimulateOutcome is pure: calling it twice yields identical results and
//     does not affect subsequent CostOfAction queries.
//   - SimulateOutcome P95 (when set) is ≥ ExpectedLatency.
func RunReasonerCompliance(t *testing.T, factory ReasonerFactory) {
	t.Helper()

	defaultCtx := func() *types.OffloadContext {
		return &types.OffloadContext{
			TaskType:        "pod-scheduling",
			SourceNodeID:    "node_1",
			DataSizeBytes:   1024,
			LatencyBudgetMs: 500.0,
		}
	}

	// ── CostOfAction ─────────────────────────────────────────────────────────

	t.Run("CostOfActionHasRationale", func(t *testing.T) {
		r := factory(t)
		cost, err := r.CostOfAction("pod-scheduling", "node_1")
		if err != nil {
			t.Fatal(err)
		}
		if cost.Rationale == "" {
			t.Error("Rationale must not be empty — reasoner must explain its decisions")
		}
	})

	t.Run("CostOfActionHasGraphPath", func(t *testing.T) {
		r := factory(t)
		cost, err := r.CostOfAction("pod-scheduling", "node_1")
		if err != nil {
			t.Fatal(err)
		}
		if len(cost.GraphPathUsed) == 0 {
			t.Error("GraphPathUsed must not be empty — every decision must reference graph edges")
		}
	})

	t.Run("CostOfActionConfidenceInRange", func(t *testing.T) {
		r := factory(t)
		cost, err := r.CostOfAction("pod-scheduling", "node_1")
		if err != nil {
			t.Fatal(err)
		}
		if cost.Confidence < 0.0 || cost.Confidence > 1.0 {
			t.Errorf("Confidence must be in [0, 1]; got %.4f", cost.Confidence)
		}
	})

	t.Run("CostOfActionNonNegativeCosts", func(t *testing.T) {
		r := factory(t)
		cost, err := r.CostOfAction("pod-scheduling", "node_1")
		if err != nil {
			t.Fatal(err)
		}
		if cost.CPUCost < 0 {
			t.Errorf("CPUCost must be ≥ 0; got %.4f", cost.CPUCost)
		}
		if cost.ResourceCost < 0 {
			t.Errorf("ResourceCost must be ≥ 0; got %.4f", cost.ResourceCost)
		}
		if cost.LatencyEstimate < 0 {
			t.Errorf("LatencyEstimate must be ≥ 0; got %.4f", cost.LatencyEstimate)
		}
	})

	// ── RecommendPeer ─────────────────────────────────────────────────────────

	t.Run("RecommendPeerHasRationaleOrInsufficientTrust", func(t *testing.T) {
		r := factory(t)
		rec, err := r.RecommendPeer(defaultCtx())
		if err != nil {
			if !errors.Is(err, contracts.ErrInsufficientTrust) && !errors.Is(err, contracts.ErrPeerCostQuery) {
				t.Fatalf("expected nil, ErrInsufficientTrust or ErrPeerCostQuery; got %v", err)
			}
			return // acceptable: no peers meet the trust threshold
		}
		if rec.Rationale == "" {
			t.Error("Rationale must not be empty when RecommendPeer returns a peer")
		}
	})

	// ── SimulateOutcome ───────────────────────────────────────────────────────

	t.Run("SimulateOutcomeIsPure", func(t *testing.T) {
		r := factory(t)
		ctx := defaultCtx()
		first, err := r.SimulateOutcome(ctx, "node_2")
		if err != nil {
			t.Fatal(err)
		}
		second, err := r.SimulateOutcome(ctx, "node_2")
		if err != nil {
			t.Fatal(err)
		}
		if first.ExpectedLatency != second.ExpectedLatency {
			t.Errorf("SimulateOutcome must be pure: ExpectedLatency drifted %.4f → %.4f",
				first.ExpectedLatency, second.ExpectedLatency)
		}
		if first.ExpectedResourceCost != second.ExpectedResourceCost {
			t.Errorf("SimulateOutcome must be pure: ExpectedResourceCost drifted %.4f → %.4f",
				first.ExpectedResourceCost, second.ExpectedResourceCost)
		}
	})

	t.Run("SimulateOutcomeDoesNotAffectCostOfAction", func(t *testing.T) {
		r := factory(t)
		ctx := defaultCtx()
		before, err := r.CostOfAction(ctx.TaskType, ctx.SourceNodeID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.SimulateOutcome(ctx, "node_2"); err != nil {
			t.Fatal(err)
		}
		after, err := r.CostOfAction(ctx.TaskType, ctx.SourceNodeID)
		if err != nil {
			t.Fatal(err)
		}
		if before.LatencyEstimate != after.LatencyEstimate ||
			before.ResourceCost != after.ResourceCost ||
			before.Confidence != after.Confidence {
			t.Errorf("SimulateOutcome leaked state — CostOfAction changed after a simulate call")
		}
	})

	t.Run("SimulateOutcomeConfidenceInRange", func(t *testing.T) {
		r := factory(t)
		sim, err := r.SimulateOutcome(defaultCtx(), "node_2")
		if err != nil {
			t.Fatal(err)
		}
		if sim.Confidence < 0.0 || sim.Confidence > 1.0 {
			t.Errorf("Confidence must be in [0, 1]; got %.4f", sim.Confidence)
		}
	})

	t.Run("SimulateOutcomeP95GreaterThanMean", func(t *testing.T) {
		r := factory(t)
		sim, err := r.SimulateOutcome(defaultCtx(), "node_2")
		if err != nil {
			t.Fatal(err)
		}
		// P95Latency is nil on profiles without Gaussian descriptors — that's
		// permitted by the contract. When it IS set, it must be ≥ the mean.
		if sim.P95Latency != nil && *sim.P95Latency < sim.ExpectedLatency {
			t.Errorf("P95Latency %.4f must be ≥ ExpectedLatency %.4f",
				*sim.P95Latency, sim.ExpectedLatency)
		}
		if sim.P95ResourceCost != nil && *sim.P95ResourceCost < sim.ExpectedResourceCost {
			t.Errorf("P95ResourceCost %.4f must be ≥ ExpectedResourceCost %.4f",
				*sim.P95ResourceCost, sim.ExpectedResourceCost)
		}
	})
}
