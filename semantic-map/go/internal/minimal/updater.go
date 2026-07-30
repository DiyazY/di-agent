package minimal

import (
	"fmt"
	"math"
	"sync"

	"github.com/DiyazY/di-agent/pkg/types"
)

// EMAUpdater is the edge-minimal UpdaterContract implementation.
//
// It maintains an Exponential Moving Average per edge:
//
//	new_ema = alpha * observation + (1 - alpha) * old_ema
//
// alpha controls memory speed: high alpha reacts fast (good for volatile
// environments), low alpha is stable (good for noisy sensors).
//
// Confidence grows as: min(n_observations / convergenceThreshold, 1.0)
// The agent blends prior and EMA as:
//
//	effective = (1 - confidence) * prior + confidence * ema
type EMAUpdater struct {
	storage              storageWriter
	alpha                float64
	convergenceThreshold float64

	mu   sync.Mutex
	seen map[string]struct{} // processed event IDs (idempotency)
}

type storageWriter interface {
	GetEdge(fromID, toID string) (*types.EdgeDescriptor, error)
	GetEdgesByPair(fromID, toID string) ([]*types.EdgeDescriptor, error)
	PutEdge(d *types.EdgeDescriptor) error
	GetNode(nodeID string) (*types.NodeDescriptor, error)
	PutNode(d *types.NodeDescriptor) error
}

// NewEMAUpdater creates an EMAUpdater.
//   - alpha: EMA decay factor, typically 0.1–0.3 for edge environments.
//   - convergenceThreshold: number of observations at which confidence reaches 1.0.
func NewEMAUpdater(storage storageWriter, alpha, convergenceThreshold float64) *EMAUpdater {
	return &EMAUpdater{
		storage:              storage,
		alpha:                alpha,
		convergenceThreshold: convergenceThreshold,
		seen:                 make(map[string]struct{}),
	}
}

// UpdateEdge incorporates one telemetry observation into every edge between
// (fromID, toID). For conflict-pair propositions this means both halves of
// the pair receive the same observation but maintain independent EMAs — they
// diverge over time as evidence accumulates and the dominant mechanism in
// THIS deployment wins.
//
// Idempotency is per-edge: the seen-set key is (fromID, toID, propositionID,
// eventID), so replaying the same observation leaves every affected edge
// unchanged.
//
// Returns the canonical edge for (fromID, toID) after updating — the entry
// with the lexicographically smallest PropositionID, matching GetEdge's
// convention. Callers needing all updated edges should call GetEdgesByPair.
func (u *EMAUpdater) UpdateEdge(fromID, toID string, obs float64, eventID string) (*types.EdgeDescriptor, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	edges, err := u.storage.GetEdgesByPair(fromID, toID)
	if err != nil {
		return nil, err
	}
	if len(edges) == 0 {
		return nil, fmt.Errorf("edge %s→%s not found in storage", fromID, toID)
	}

	for _, edge := range edges {
		key := fmt.Sprintf("%s→%s:%s:%s", fromID, toID, edge.PropositionID, eventID)
		if _, ok := u.seen[key]; ok {
			continue // this (edge, event) already applied
		}
		updated := *edge
		updated.EMAWeight = u.alpha*obs + (1-u.alpha)*edge.EMAWeight
		updated.NObservations = edge.NObservations + 1
		updated.Confidence = math.Min(float64(updated.NObservations)/u.convergenceThreshold, 1.0)

		if err := u.storage.PutEdge(&updated); err != nil {
			return nil, err
		}
		u.seen[key] = struct{}{}
	}

	// Return the canonical edge (smallest propID) for backward compatibility.
	return u.storage.GetEdge(fromID, toID)
}

func (u *EMAUpdater) UpdateNode(nodeID string, obs float64, eventID string) (*types.NodeDescriptor, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	key := fmt.Sprintf("node:%s:%s", nodeID, eventID)
	if _, ok := u.seen[key]; ok {
		return u.storage.GetNode(nodeID)
	}

	node, err := u.storage.GetNode(nodeID)
	if err != nil {
		return nil, err
	}
	if node == nil {
		return nil, fmt.Errorf("node %s not found in storage", nodeID)
	}

	updated := *node
	updated.EMAValue = u.alpha*obs + (1-u.alpha)*node.EMAValue
	updated.NObservations = node.NObservations + 1
	updated.Confidence = math.Min(float64(updated.NObservations)/u.convergenceThreshold, 1.0)

	if err := u.storage.PutNode(&updated); err != nil {
		return nil, err
	}
	u.seen[key] = struct{}{}
	return &updated, nil
}

// Reset restores every edge between (fromID, toID) to its prior state:
// EMAWeight = PriorWeight, NObservations = 0, Confidence = 0.0. For
// conflict-pair propositions this resets both halves together so the agent
// re-enters cold-start mode for that construct pair consistently.
func (u *EMAUpdater) Reset(fromID, toID string) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	edges, err := u.storage.GetEdgesByPair(fromID, toID)
	if err != nil {
		return err
	}
	if len(edges) == 0 {
		return fmt.Errorf("edge %s→%s not found", fromID, toID)
	}

	for _, edge := range edges {
		reset := *edge
		reset.EMAWeight = edge.PriorWeight
		reset.NObservations = 0
		reset.Confidence = 0.0
		if err := u.storage.PutEdge(&reset); err != nil {
			return err
		}
	}

	// Clear seen entries for this construct pair so future events are
	// processed normally. The seen-key format from UpdateEdge starts with
	// "from→to:" so the prefix match drops every (propID, eventID) for this
	// pair in one pass.
	prefix := fmt.Sprintf("%s→%s:", fromID, toID)
	for k := range u.seen {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(u.seen, k)
		}
	}

	return nil
}
