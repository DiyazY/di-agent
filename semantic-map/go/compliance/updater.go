package compliance

import (
	"testing"

	"github.com/DiyazY/di-agent/pkg/contracts"
	"github.com/DiyazY/di-agent/pkg/types"
)

type UpdaterFactory func(t *testing.T) (contracts.UpdaterContract, contracts.StorageContract)

func RunUpdaterCompliance(t *testing.T, factory UpdaterFactory) {
	t.Helper()

	seed := func(t *testing.T) (contracts.UpdaterContract, contracts.StorageContract) {
		t.Helper()
		u, s := factory(t)
		_ = s.PutEdge(&types.EdgeDescriptor{
			FromID: "a", ToID: "b", PropositionID: "P1",
			Direction: types.Positive, PriorWeight: 0.5, EMAWeight: 0.5,
		})
		return u, s
	}

	t.Run("UpdateIncrementsObservationCount", func(t *testing.T) {
		u, s := seed(t)
		if _, err := u.UpdateEdge("a", "b", 0.6, "evt-1"); err != nil {
			t.Fatal(err)
		}
		edge, _ := s.GetEdge("a", "b")
		if edge.NObservations != 1 {
			t.Errorf("expected 1 observation, got %d", edge.NObservations)
		}
	})

	t.Run("UpdateShiftsEMATowardObservation", func(t *testing.T) {
		u, s := seed(t)
		_, _ = u.UpdateEdge("a", "b", 1.0, "evt-1")
		edge, _ := s.GetEdge("a", "b")
		if edge.EMAWeight <= 0.5 {
			t.Errorf("EMA should have increased toward 1.0, got %.4f", edge.EMAWeight)
		}
	})

	t.Run("IdempotentOnSameEventID", func(t *testing.T) {
		u, s := seed(t)
		_, _ = u.UpdateEdge("a", "b", 0.9, "evt-1")
		after1, _ := s.GetEdge("a", "b")
		_, _ = u.UpdateEdge("a", "b", 0.9, "evt-1")
		after2, _ := s.GetEdge("a", "b")
		if after1.EMAWeight != after2.EMAWeight || after1.NObservations != after2.NObservations {
			t.Error("second call with same event_id must not change state")
		}
	})

	t.Run("DifferentEventIDsAccumulate", func(t *testing.T) {
		u, s := seed(t)
		_, _ = u.UpdateEdge("a", "b", 0.6, "evt-1")
		_, _ = u.UpdateEdge("a", "b", 0.7, "evt-2")
		edge, _ := s.GetEdge("a", "b")
		if edge.NObservations != 2 {
			t.Errorf("expected 2 observations, got %d", edge.NObservations)
		}
	})

	t.Run("ResetRestoresPrior", func(t *testing.T) {
		u, s := seed(t)
		_, _ = u.UpdateEdge("a", "b", 0.9, "evt-1")
		if err := u.Reset("a", "b"); err != nil {
			t.Fatal(err)
		}
		edge, _ := s.GetEdge("a", "b")
		if edge.EMAWeight != edge.PriorWeight {
			t.Errorf("EMA %.4f should equal prior %.4f after reset", edge.EMAWeight, edge.PriorWeight)
		}
		if edge.NObservations != 0 {
			t.Errorf("NObservations should be 0 after reset, got %d", edge.NObservations)
		}
		if edge.Confidence != 0.0 {
			t.Errorf("Confidence should be 0.0 after reset, got %.4f", edge.Confidence)
		}
	})

	t.Run("ResetDoesNotDeleteEdge", func(t *testing.T) {
		u, s := seed(t)
		_ = u.Reset("a", "b")
		edge, _ := s.GetEdge("a", "b")
		if edge == nil {
			t.Error("edge must still exist after reset")
		}
	})

	t.Run("ConfidenceIncreasesWithObservations", func(t *testing.T) {
		u, s := seed(t)
		c0, _ := s.GetEdge("a", "b")
		_, _ = u.UpdateEdge("a", "b", 0.6, "evt-1")
		c1, _ := s.GetEdge("a", "b")
		if c1.Confidence <= c0.Confidence {
			t.Errorf("confidence should increase: %.4f -> %.4f", c0.Confidence, c1.Confidence)
		}
	})

	// ── Multigraph behavior ──────────────────────────────────────────────────
	//
	// When two propositions share the same (from, to) endpoints (a "conflict
	// pair"), one observation must update both edges. Each edge maintains its
	// own EMA so they diverge as evidence accumulates; idempotency is tracked
	// per-edge so replays don't double-count.

	seedConflictPair := func(t *testing.T) (contracts.UpdaterContract, contracts.StorageContract) {
		t.Helper()
		u, s := factory(t)
		_ = s.PutEdge(&types.EdgeDescriptor{
			FromID: "RC", ToID: "PS", PropositionID: "P2",
			Direction: types.Negative, PriorWeight: 0.5, EMAWeight: 0.5,
		})
		_ = s.PutEdge(&types.EdgeDescriptor{
			FromID: "RC", ToID: "PS", PropositionID: "P3",
			Direction: types.Positive, PriorWeight: 0.5, EMAWeight: 0.5,
		})
		return u, s
	}

	t.Run("UpdateEdgeReachesEveryProposition", func(t *testing.T) {
		u, s := seedConflictPair(t)
		if _, err := u.UpdateEdge("RC", "PS", 1.0, "evt-1"); err != nil {
			t.Fatal(err)
		}
		pair, _ := s.GetEdgesByPair("RC", "PS")
		if len(pair) != 2 {
			t.Fatalf("expected 2 edges on RC→PS; got %d", len(pair))
		}
		for _, e := range pair {
			if e.NObservations != 1 {
				t.Errorf("edge %s NObservations = %d; expected 1 (every edge in the pair must receive the update)",
					e.PropositionID, e.NObservations)
			}
			if e.EMAWeight <= 0.5 {
				t.Errorf("edge %s EMA = %.4f; expected > 0.5 after positive observation",
					e.PropositionID, e.EMAWeight)
			}
		}
	})

	t.Run("IdempotencyIsPerEdgeInPair", func(t *testing.T) {
		u, s := seedConflictPair(t)
		// Same observation, same eventID, replayed.
		_, _ = u.UpdateEdge("RC", "PS", 0.9, "evt-X")
		_, _ = u.UpdateEdge("RC", "PS", 0.9, "evt-X")
		pair, _ := s.GetEdgesByPair("RC", "PS")
		for _, e := range pair {
			if e.NObservations != 1 {
				t.Errorf("edge %s NObservations = %d; replayed eventID must not double-count",
					e.PropositionID, e.NObservations)
			}
		}
	})

	t.Run("ResetClearsEveryEdgeInPair", func(t *testing.T) {
		u, s := seedConflictPair(t)
		_, _ = u.UpdateEdge("RC", "PS", 0.9, "evt-1")
		_, _ = u.UpdateEdge("RC", "PS", 0.8, "evt-2")
		if err := u.Reset("RC", "PS"); err != nil {
			t.Fatal(err)
		}
		pair, _ := s.GetEdgesByPair("RC", "PS")
		for _, e := range pair {
			if e.EMAWeight != e.PriorWeight {
				t.Errorf("edge %s EMAWeight = %.4f; should equal PriorWeight = %.4f after Reset",
					e.PropositionID, e.EMAWeight, e.PriorWeight)
			}
			if e.NObservations != 0 {
				t.Errorf("edge %s NObservations = %d; should be 0 after Reset", e.PropositionID, e.NObservations)
			}
			if e.Confidence != 0.0 {
				t.Errorf("edge %s Confidence = %.4f; should be 0 after Reset", e.PropositionID, e.Confidence)
			}
		}
	})
}

// RelationalUpdaterFactory builds an updater that supports the paired path,
// together with the storage it writes to.
type RelationalUpdaterFactory func(t *testing.T) (contracts.RelationalUpdaterContract, contracts.StorageContract)

// RunRelationalUpdaterCompliance verifies the behavioural guarantees of
// RelationalUpdaterContract on top of the base UpdaterContract suite:
//
//   - Support threshold: while the implementation holds too few pairs to
//     estimate a relation, n_observations must not advance. Confidence is the
//     agent's report of what it has learned, and an edge that has seen two
//     points has not learned a relation from them.
//   - Idempotency on the paired eventID: a replayed pair changes neither the
//     stored edge nor the internal pairing state, so replaying a batch cannot
//     inflate the estimate by re-adding the same points.
//   - Direction discrimination: a pair stream whose sign matches one
//     proposition's declared direction must raise that proposition's weight and
//     not the weight of a sibling on the same endpoints with the opposite
//     direction. This is the property a single-value updater cannot have, and
//     the reason the contract exists.
//   - Scale: the learned weight stays within [0,1], the domain PriorWeight lives
//     on, so the Reasoner's blend of the two remains meaningful.
func RunRelationalUpdaterCompliance(t *testing.T, factory RelationalUpdaterFactory) {
	t.Helper()

	// A conflict pair: same endpoints, opposite directions.
	seedPair := func(t *testing.T) (contracts.RelationalUpdaterContract, contracts.StorageContract) {
		t.Helper()
		u, s := factory(t)
		_ = s.PutEdge(&types.EdgeDescriptor{
			FromID: "a", ToID: "b", PropositionID: "Ppos",
			Direction: types.Positive, PriorWeight: 0.5, EMAWeight: 0.5,
		})
		_ = s.PutEdge(&types.EdgeDescriptor{
			FromID: "a", ToID: "b", PropositionID: "Pneg",
			Direction: types.Negative, PriorWeight: 0.5, EMAWeight: 0.5,
		})
		return u, s
	}

	// feed drives n pairs of a positively correlated stream (y rises with x).
	feed := func(t *testing.T, u contracts.RelationalUpdaterContract, n int, prefix string) {
		t.Helper()
		for i := 0; i < n; i++ {
			x := float64(i%10) / 10.0
			y := 0.9*x + 0.02
			if _, err := u.UpdateEdgeRelation("a", "b", x, y, prefix+string(rune('A'+i%26))+string(rune('0'+i/26))); err != nil {
				t.Fatalf("UpdateEdgeRelation: %v", err)
			}
		}
	}

	t.Run("HoldsObservationCountUntilSupported", func(t *testing.T) {
		u, s := seedPair(t)
		feed(t, u, 2, "few-")
		edges, _ := s.GetEdgesByPair("a", "b")
		for _, e := range edges {
			if e.NObservations != 0 {
				t.Errorf("%s advanced to %d observations on 2 pairs; a relation cannot be "+
					"estimated from that few, so confidence must still report zero",
					e.PropositionID, e.NObservations)
			}
		}
	})

	t.Run("LearnsOnceSupported", func(t *testing.T) {
		u, s := seedPair(t)
		feed(t, u, 40, "many-")
		edges, _ := s.GetEdgesByPair("a", "b")
		var moved bool
		for _, e := range edges {
			if e.NObservations > 0 {
				moved = true
			}
			if e.EMAWeight < 0 || e.EMAWeight > 1 {
				t.Errorf("%s learned EMAWeight %v, outside the [0,1] domain PriorWeight lives on",
					e.PropositionID, e.EMAWeight)
			}
		}
		if !moved {
			t.Error("no edge advanced after 40 pairs")
		}
	})

	t.Run("PairedUpdateIsIdempotent", func(t *testing.T) {
		u, s := seedPair(t)
		feed(t, u, 40, "idem-")
		before, _ := s.GetEdgesByPair("a", "b")
		snapshot := map[string][2]float64{}
		for _, e := range before {
			snapshot[e.PropositionID] = [2]float64{e.EMAWeight, float64(e.NObservations)}
		}
		feed(t, u, 40, "idem-") // identical event IDs
		after, _ := s.GetEdgesByPair("a", "b")
		for _, e := range after {
			want := snapshot[e.PropositionID]
			if e.EMAWeight != want[0] || float64(e.NObservations) != want[1] {
				t.Errorf("%s changed on replay: (ema %v, n %d) vs (ema %v, n %v)",
					e.PropositionID, e.EMAWeight, e.NObservations, want[0], want[1])
			}
		}
	})

	t.Run("DiscriminatesDirectionWithinAConflictPair", func(t *testing.T) {
		u, s := seedPair(t)
		feed(t, u, 60, "dir-")
		edges, _ := s.GetEdgesByPair("a", "b")
		var pos, neg *types.EdgeDescriptor
		for _, e := range edges {
			switch e.PropositionID {
			case "Ppos":
				pos = e
			case "Pneg":
				neg = e
			}
		}
		if pos == nil || neg == nil {
			t.Fatal("conflict pair not present in storage")
		}
		if !(pos.EMAWeight > neg.EMAWeight) {
			t.Errorf("a positively correlated stream left the positive proposition at %.4f "+
				"and the negative one at %.4f; the pair must separate on evidence",
				pos.EMAWeight, neg.EMAWeight)
		}
	})

	t.Run("ResetClearsPairingState", func(t *testing.T) {
		u, s := seedPair(t)
		feed(t, u, 40, "reset-")
		if err := u.Reset("a", "b"); err != nil {
			t.Fatal(err)
		}
		edges, _ := s.GetEdgesByPair("a", "b")
		for _, e := range edges {
			if e.NObservations != 0 || e.EMAWeight != e.PriorWeight {
				t.Errorf("%s not reset: n=%d ema=%v prior=%v",
					e.PropositionID, e.NObservations, e.EMAWeight, e.PriorWeight)
			}
		}
		// After a reset the support window must be empty too, so the first few
		// pairs cannot immediately move the weight again.
		feed(t, u, 2, "post-reset-")
		edges, _ = s.GetEdgesByPair("a", "b")
		for _, e := range edges {
			if e.NObservations != 0 {
				t.Errorf("%s advanced %d observations on 2 pairs after reset — the "+
					"pairing window survived the reset", e.PropositionID, e.NObservations)
			}
		}
	})
}
