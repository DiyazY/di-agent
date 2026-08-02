package semmap

import (
	"errors"
	"sort"

	"github.com/DiyazY/di-agent/pkg/statemap"
	"github.com/DiyazY/di-agent/pkg/types"
)

// ErrNoStateModel is returned by the graph surfaces when no state model is attached.
// They cannot be answered from anywhere else, and answering from the construct store
// was the arrangement this replaced, so there is deliberately no fallback: a surface
// that quietly serves a second model's numbers is the failure being removed.
var ErrNoStateModel = errors.New(
	"no state model attached: the graph surfaces render the map's relationships, " +
		"and there is no second model to fall back to")

// The graph surfaces — /graph, /edges, /neighbors, the web viewer, `mapctl edges` —
// are rendered from the state model rather than from the construct graph's own store.
//
// They were rendered from the store, and that was the last place where two models of
// the same relations could disagree in public. Cost, estimates and explanations had all
// moved to the state model; the store kept its own copy of every relation and its own
// estimator kept updating it from the same samples. So an operator opening the viewer
// read weights and confidences that no longer entered any decision, on a page that
// looks exactly like the one that used to. A number that is displayed but not used is
// worse than an absent one, because nobody can tell by looking.
//
// The projection is faithful rather than clever: one relationship becomes one edge
// descriptor, and the wire shape is unchanged, so the viewer and the CLI carry on
// working while now showing the numbers that actually decide things.
//
// Direction of the mapping, for the record:
//
//	FromID, ToID     ← the relationship's endpoints (construct-level properties)
//	PropositionID    ← its label, which is where the proposition ID is carried
//	Direction        ← its sign
//	PriorWeight      ← its prior, the calibrated strength before this system spoke
//	EMAWeight        ← its learned strength
//	Confidence       ← how much of that rests on paired observation here
//	Deprecated       ← retired, with the reason retirement recorded

// edgeFromRelationship renders one relationship as the edge descriptor the graph
// surfaces speak in.
func edgeFromRelationship(r statemap.Relationship) *types.EdgeDescriptor {
	direction := types.Positive
	if r.Sign < 0 {
		direction = types.Negative
	}
	return &types.EdgeDescriptor{
		FromID:        r.From,
		ToID:          r.To,
		PropositionID: r.Label,
		Direction:     direction,
		PriorWeight:   r.Prior,
		EMAWeight:     r.Strength,
		Confidence:    r.Confidence,
		NObservations: r.NObservations,
		// A retired relationship is the state model's form of a withdrawn claim, which
		// is what Deprecated has always meant on this surface.
		Deprecated:       r.Status == statemap.Retired,
		DeprecatedReason: r.RetiredReason,
	}
}

// projectedEdges renders every relationship in the state model, ordered so two calls
// agree. Relationships between metric-level properties are included: the store only
// ever held construct-level edges, but the state model is free to carry more, and
// hiding the extras would make the surface a filtered view that claims to be whole.
func (m *SemanticMap) projectedEdges() []*types.EdgeDescriptor {
	rels := m.state.Relationships("", "")
	out := make([]*types.EdgeDescriptor, 0, len(rels))
	for _, r := range rels {
		out = append(out, edgeFromRelationship(r))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FromID != out[j].FromID {
			return out[i].FromID < out[j].FromID
		}
		if out[i].ToID != out[j].ToID {
			return out[i].ToID < out[j].ToID
		}
		return out[i].PropositionID < out[j].PropositionID
	})
	return out
}
