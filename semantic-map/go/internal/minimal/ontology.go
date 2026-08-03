package minimal

import (
	"fmt"
	"sync"

	"github.com/DiyazY/di-agent/pkg/domain"
	"github.com/DiyazY/di-agent/pkg/types"
)

// SpecOntology is the edge-minimal OntologyContract implementation: the declaration
// layer over a domain.Spec loaded at startup. Nothing about the model is compiled in.
// Constructs and propositions may be added at runtime; nothing else about them changes
// here.
//
// It used to be larger. It carried each proposition's strength, a deprecation flag, and
// an audit log of its own — all three of which the state model also holds, and holds
// authoritatively, since that is what every answer is read from. Keeping copies here
// meant a reconciliation step on every calibration and two logs to read; it also
// produced a real defect, in which the strength this layer exposed was not the strength
// in force. What remains is the vocabulary, which is the one thing the specification
// actually declares — it declares no strengths at all.
//
// Holding the spec rather than a copy of its contents matters for one case in
// particular: a construct that appears mid-deployment needs a metric routed to it
// before it can accumulate evidence, and routing lives in the same spec. See
// pkg/domain.
type SpecOntology struct {
	spec         *domain.Spec
	mu           sync.RWMutex
	constructs   []*types.Construct
	propositions []*types.Proposition
}

// NewOntologyFromSpec builds an ontology from a loaded domain specification.
func NewOntologyFromSpec(spec *domain.Spec) *SpecOntology {
	o := &SpecOntology{spec: spec}
	for _, c := range spec.Constructs {
		o.constructs = append(o.constructs, &types.Construct{
			ConstructID: c.ConstructID,
			Name:        c.Name,
			Description: c.Description,
		})
	}
	for _, p := range spec.Propositions {
		dir := types.Positive
		if p.Direction == "negative" {
			dir = types.Negative
		}
		o.propositions = append(o.propositions, &types.Proposition{
			PropositionID: p.PropositionID,
			FromConstruct: p.FromConstruct,
			ToConstruct:   p.ToConstruct,
			Direction:     dir,
			// The specification declares no strength, so this is the policy floor as a
			// placeholder — the lowest value an operator would be allowed to set. The
			// number in force is the state model's relationship prior, seeded from the
			// calibration, and the facade overlays it onto what this layer reports.
			PriorStrength:   spec.FloorFor(p.PropositionID),
			Description:     p.Description,
			EvidenceSources: p.EvidenceSources,
		})
	}
	return o
}

// Spec exposes the loaded model so the Bridge can resolve metric routing and the
// facade can read the adjustment policy. Both are part of the domain model, and
// keeping them in one place is what lets a runtime-added construct be reachable.
func (o *SpecOntology) Spec() *domain.Spec { return o.spec }

// Constructs returns a defensive copy of the construct list. Callers may mutate the
// returned slice or its elements without affecting the ontology's internal state; to
// register a new construct, use AddConstruct.
func (o *SpecOntology) Constructs() ([]*types.Construct, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	out := make([]*types.Construct, len(o.constructs))
	for i, c := range o.constructs {
		cp := *c
		out[i] = &cp
	}
	return out, nil
}

// Propositions returns a defensive copy of the declared propositions. The strengths it
// carries are placeholders; SemanticMap.Propositions overlays the values in force from
// the state model, which is what a caller should read.
func (o *SpecOntology) Propositions() ([]*types.Proposition, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	out := make([]*types.Proposition, len(o.propositions))
	for i, p := range o.propositions {
		cp := *p
		// EvidenceSources is a slice — copy it to avoid shared backing array.
		if len(p.EvidenceSources) > 0 {
			cp.EvidenceSources = append([]string(nil), p.EvidenceSources...)
		}
		out[i] = &cp
	}
	return out, nil
}

func (o *SpecOntology) Relationships(constructID string) ([]*types.Proposition, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	var out []*types.Proposition
	for _, p := range o.propositions {
		if p.FromConstruct == constructID || p.ToConstruct == constructID {
			cp := *p
			if len(p.EvidenceSources) > 0 {
				cp.EvidenceSources = append([]string(nil), p.EvidenceSources...)
			}
			out = append(out, &cp)
		}
	}
	return out, nil
}

// AddConstruct appends a new construct to the declaration. Constructs are append-only —
// there is no removal path because constructs are domain-stable per the architecture.
// Duplicate ConstructIDs are rejected. The matching property is declared in the state
// model by SemanticMap.AddConstruct; adding one here alone names something nothing can
// say anything about.
func (o *SpecOntology) AddConstruct(c *types.Construct) error {
	if c == nil || c.ConstructID == "" {
		return fmt.Errorf("AddConstruct: nil construct or empty ConstructID")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, existing := range o.constructs {
		if existing.ConstructID == c.ConstructID {
			return fmt.Errorf("AddConstruct: ConstructID %q already exists", c.ConstructID)
		}
	}
	cp := *c
	o.constructs = append(o.constructs, &cp)
	return nil
}

func (o *SpecOntology) ValidateProposition(fromID, toID string, dir types.Direction) (*types.ValidationResult, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	res := &types.ValidationResult{Valid: true}
	for _, p := range o.propositions {
		if p.FromConstruct == fromID && p.ToConstruct == toID && p.Direction != dir {
			res.Valid = false
			res.Conflicts = append(res.Conflicts, p.PropositionID)
		}
	}
	return res, nil
}

func (o *SpecOntology) AddValidatedProposition(p *types.Proposition) error {
	if p == nil || p.PropositionID == "" {
		return fmt.Errorf("AddValidatedProposition: nil proposition or empty PropositionID")
	}
	res, err := o.ValidateProposition(p.FromConstruct, p.ToConstruct, p.Direction)
	if err != nil {
		return err
	}
	if !res.Valid {
		return fmt.Errorf("proposition contradicts existing backbone: conflicts=%v", res.Conflicts)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	// Defensive copy: the ontology owns its proposition entries; the caller
	// must not be able to mutate them via their original pointer.
	cp := *p
	if len(p.EvidenceSources) > 0 {
		cp.EvidenceSources = append([]string(nil), p.EvidenceSources...)
	}
	o.propositions = append(o.propositions, &cp)
	return nil
}
