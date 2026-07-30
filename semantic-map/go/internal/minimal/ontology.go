package minimal

import (
	"fmt"
	"sync"
	"time"

	"github.com/DiyazY/di-agent/pkg/domain"
	"github.com/DiyazY/di-agent/pkg/types"
)

// SpecOntology is the edge-minimal OntologyContract implementation. Constructs
// and propositions come from a domain.Spec loaded at startup — nothing about the
// model is compiled in. The ontology is live: constructs and propositions may be
// added, prior strengths recalibrated, and propositions deprecated at runtime,
// and every mutation appends to an in-memory audit log readable via GetHistory.
// The log is ephemeral on this profile; a persisting profile would carry it.
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
	events       []*types.OntologyEvent

	// now overrides time.Now for deterministic testing. Production callers
	// leave it nil and the implementation uses the wall clock.
	now func() time.Time
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
			PropositionID:   p.PropositionID,
			FromConstruct:   p.FromConstruct,
			ToConstruct:     p.ToConstruct,
			Direction:       dir,
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

// appendEvent records one mutation in the audit log. Callers hold o.mu.
// actor defaults to "system" when the parameter is empty.
func (o *SpecOntology) appendEvent(actor string, kind types.OntologyEventKind, targetID string, detail map[string]any) {
	if actor == "" {
		actor = "system"
	}
	var ts time.Time
	if o.now != nil {
		ts = o.now()
	} else {
		ts = time.Now().UTC()
	}
	o.events = append(o.events, &types.OntologyEvent{
		Timestamp: ts,
		Actor:     actor,
		Kind:      kind,
		TargetID:  targetID,
		Detail:    detail,
	})
}

// Constructs returns a defensive copy of the construct list. Callers may mutate
// the returned slice or its elements without affecting the ontology's internal
// state; to register a new construct, use the ontology's setters.
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

// Propositions returns a defensive copy of the proposition list. Mutating
// returned entries does NOT update the ontology — use SetPropositionStrength
// (or AddValidatedProposition) to make changes.
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

// SetPropositionStrength updates the PriorStrength of an existing proposition
// and appends an EventPropositionStrengthSet entry to the history. This is the
// safe write path used by the prior initialization pipeline and by operator
// tuning — pointer mutation through Propositions() is not supported because
// that method returns defensive copies.
//
// Returns an error if the proposition ID is not found.
func (o *SpecOntology) SetPropositionStrength(propositionID string, strength float64) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, p := range o.propositions {
		if p.PropositionID == propositionID {
			old := p.PriorStrength
			p.PriorStrength = strength
			o.appendEvent("system", types.EventPropositionStrengthSet, propositionID, map[string]any{
				"strength_old": old,
				"strength_new": strength,
			})
			return nil
		}
	}
	return fmt.Errorf("proposition %q not found", propositionID)
}

// AddConstruct appends a new construct to the ontology. Constructs are
// append-only — there is no removal path because constructs are domain-stable
// per the architecture. Duplicate ConstructIDs are rejected.
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
	o.appendEvent("system", types.EventConstructAdded, cp.ConstructID, map[string]any{
		"name":        cp.Name,
		"description": cp.Description,
	})
	return nil
}

// Deprecate marks a proposition as no-longer-endorsed. The proposition stays
// in the ontology (visible to GetHistory replay and to clients that walk the
// full backbone) but Reasoners must skip it during cost computation.
// Idempotent: calling Deprecate twice on the same proposition is a no-op on
// the second call (no duplicate event, no error).
//
// Returns an error if the proposition ID is not found.
func (o *SpecOntology) Deprecate(propositionID, reason string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, p := range o.propositions {
		if p.PropositionID == propositionID {
			if p.Deprecated {
				return nil // idempotent
			}
			p.Deprecated = true
			p.DeprecatedReason = reason
			o.appendEvent("system", types.EventPropositionDeprecated, propositionID, map[string]any{
				"reason": reason,
			})
			return nil
		}
	}
	return fmt.Errorf("proposition %q not found", propositionID)
}

// GetHistory returns ontology mutation events appended at or after `since`,
// in chronological insertion order. Pass a zero time.Time to retrieve the
// full log. The returned slice is a defensive copy; mutating it does not
// affect the ontology's internal log.
func (o *SpecOntology) GetHistory(since time.Time) ([]*types.OntologyEvent, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	out := make([]*types.OntologyEvent, 0, len(o.events))
	for _, e := range o.events {
		if !since.IsZero() && e.Timestamp.Before(since) {
			continue
		}
		cp := *e
		if len(e.Detail) > 0 {
			cp.Detail = make(map[string]any, len(e.Detail))
			for k, v := range e.Detail {
				cp.Detail[k] = v
			}
		}
		out = append(out, &cp)
	}
	return out, nil
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
	o.appendEvent("system", types.EventPropositionAdded, cp.PropositionID, map[string]any{
		"from":           cp.FromConstruct,
		"to":             cp.ToConstruct,
		"direction":      cp.Direction,
		"prior_strength": cp.PriorStrength,
	})
	return nil
}

// RecordTune appends a consolidated "operator-tune" event to the audit log
// without modifying any proposition strength. It records the operator's intent
// string alongside the proposition IDs that were adjusted in the same batch.
// Returns nil (best-effort; never blocks Tune).
func (o *SpecOntology) RecordTune(text, operator string, appliedIDs []string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, &types.OntologyEvent{
		Timestamp: func() time.Time {
			if o.now != nil {
				return o.now()
			}
			return time.Now().UTC()
		}(),
		Actor:    operator,
		Kind:     "operator-tune",
		TargetID: "",
		Detail:   map[string]any{"intent": text, "proposition_ids": appliedIDs},
	})
	return nil
}
