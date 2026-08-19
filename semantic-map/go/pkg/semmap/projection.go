package semmap

import (
	"errors"
	"sort"
	"time"

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
//	Established      ← its long-run learned baseline, nil until pairs accumulate
//	Assertion        ← an operator's override, nil unless one was set
//	Effective        ← the value the agent reasons with, nil when there is not one
//	Basis            ← which of the three answered: asserted, established, recent, unknown
//	EMAWeight        ← its recent learned strength
//	Confidence       ← how much of that rests on paired observation here
//	Deprecated       ← retired, with the reason retirement recorded
//
// The descriptor carried a PriorWeight until seeded magnitudes were removed. Nothing
// replaced it with a default, because a relationship with no evidence has no strength
// and reporting zero would be the claim that it is worth nothing.

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
		Established:   r.Established,
		Assertion:     r.Assertion,
		Effective:     effectivePtr(r),
		Basis:         r.Basis(),
		EMAWeight:     r.Strength,
		Confidence:    r.Confidence,
		NObservations: r.NObservations,
		// A retired relationship is the state model's form of a withdrawn claim, which
		// is what Deprecated has always meant on this surface.
		Deprecated:       r.Status == statemap.Retired,
		DeprecatedReason: r.RetiredReason,
	}
}

// projectedPropositions overlays the values in force onto the declared propositions.
//
// The declaration layer answers for the vocabulary — which propositions exist, between
// which constructs, in which direction, with what description. It holds no strength,
// because the specification declares none: what it reports for that field is the policy
// floor as a placeholder. The number in force is the relationship's prior, and whether
// the claim is withdrawn is whether the relationship is retired.
//
// A proposition with no relationship in the model is reported as declared, with its
// placeholder strength and `Instantiated` false. That case is not a gap to hide: seeding
// skips a proposition whose endpoints are not both observable (§3.1 of the P6 paper), so
// the flag is how a caller tells a claim this agent carries from one it merely knows
// about. Reporting the placeholder as though it were calibrated would be the error.
func (m *SemanticMap) projectedPropositions() ([]*types.Proposition, error) {
	declared, err := m.ontology.Propositions()
	if err != nil {
		return nil, err
	}
	if m.state == nil {
		return declared, nil
	}
	byLabel := map[string]statemap.Relationship{}
	for _, r := range m.state.Relationships("", "") {
		if r.Label != "" {
			byLabel[r.Label] = r
		}
	}
	for _, p := range declared {
		r, ok := byLabel[p.PropositionID]
		if !ok {
			continue
		}
		// The declaration layer reports what the map has, which may be nothing. A
		// proposition with no measurement behind it reads 0 here and Instantiated
		// true: declared, and not yet worth anything.
		if v, known := r.Effective(); known {
			p.PriorStrength = v
		} else {
			p.PriorStrength = 0
		}
		p.Instantiated = true
		if r.Status == statemap.Retired {
			p.Deprecated = true
			p.DeprecatedReason = r.RetiredReason
		}
	}
	return declared, nil
}

// projectedHistory renders the state model's journal as ontology events.
//
// There is one journal. This projection exists so the surfaces that predate it —
// `/history`, `mapctl history`, the viewer's audit panel, the explain layer's
// get_history tool — keep working against the record that is actually authoritative.
// The mapping is lossy in one direction only: the journal holds more kinds of event
// than the ontology vocabulary can name (a property admitted, a property gone stale, a
// decision taken), and those are surfaced as their own kinds rather than being dropped
// or forced into a construct-shaped box.
func (m *SemanticMap) projectedHistory(since time.Time) ([]*types.OntologyEvent, error) {
	if m.state == nil {
		return nil, ErrNoStateModel
	}
	events := m.state.Journal().Events(0, 0)
	out := make([]*types.OntologyEvent, 0, len(events))
	for _, e := range events {
		if !since.IsZero() && e.At.Before(since) {
			continue
		}
		detail := make(map[string]any, len(e.Detail)+1)
		for k, v := range e.Detail {
			detail[k] = v
		}
		detail["revision"] = e.Revision
		out = append(out, &types.OntologyEvent{
			Timestamp: e.At,
			Actor:     e.Actor,
			Kind:      ontologyEventKind(e.Kind),
			TargetID:  e.Target,
			Detail:    detail,
		})
	}
	return out, nil
}

// ontologyEventKind maps a journal event onto the audit vocabulary the wire surfaces
// already speak, keeping the names that had a meaning there and passing the rest
// through unchanged rather than collapsing them into an "other" bucket.
func ontologyEventKind(k statemap.EventKind) types.OntologyEventKind {
	switch k {
	case statemap.EventPropertyDeclared, statemap.EventPropertyAdmitted:
		return types.EventConstructAdded
	case statemap.EventRelationshipDeclared:
		return types.EventPropositionAdded
	case statemap.EventRelationshipAsserted:
		return types.EventPropositionStrengthSet
	case statemap.EventRelationshipRetired, statemap.EventPropertyRetired:
		return types.EventPropositionDeprecated
	case statemap.EventOperatorIntent:
		return types.EventOperatorTune
	default:
		return types.OntologyEventKind(k)
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

// effectivePtr renders a relationship's effective strength for a wire descriptor,
// nil when there is not one, so a caller cannot read an absent estimate as zero.
func effectivePtr(r statemap.Relationship) *float64 {
	v, known := r.Effective()
	if !known {
		return nil
	}
	return &v
}
