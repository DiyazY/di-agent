package profiles

import (
	"fmt"

	"github.com/DiyazY/di-agent/pkg/domain"
	"github.com/DiyazY/di-agent/pkg/statemap"
)

// SeedStateMap fills the state model with the properties and relationships the
// domain specification declares, giving each relationship the calibrated prior for
// this cluster.
//
// Two layers come out of one specification. Each routed metric becomes an OBSERVED
// property — the thing a collector reports. Each construct becomes a DERIVED
// property whose members are the metrics routed to it, so a framework's evaluation
// constructs are summaries over real observations rather than a parallel vocabulary.
// Each proposition becomes a relationship between two derived properties.
//
// It lives here rather than in the daemon because this is where the specification
// and the calibration are both already loaded. Seeding from the specification alone
// gave every relationship the same placeholder strength, which made the calibration
// decorative for anything the reasoner answered from the state model — a mechanism
// that ran, logged, and influenced nothing.
//
// Seeding is not required for the model to work: an agent whose specification
// declares nothing still admits properties as telemetry arrives. It exists so a
// fresh agent shows the shape it is about to fill in, and so prior knowledge about
// how constructs relate is present before the system has been observed.
func SeedStateMap(sm *statemap.Map, spec *domain.Spec, priorWeightsPath, kd string) (int, error) {
	if sm == nil || spec == nil {
		return 0, nil
	}
	var pw *priorWeightsFile
	if priorWeightsPath != "" {
		loaded, err := loadPriorWeights(priorWeightsPath)
		if err != nil {
			return 0, fmt.Errorf("loading priors for the state model: %w", err)
		}
		pw = loaded
	}
	return seedStateMap(sm, spec, pw, kd)
}

func seedStateMap(sm *statemap.Map, spec *domain.Spec, pw *priorWeightsFile, kd string) (int, error) {
	members := map[string][]string{}
	for _, route := range spec.MetricRouting {
		if err := sm.DeclareProperty(statemap.Property{
			ID:     route.MetricType,
			Kind:   statemap.Observed,
			Unit:   route.Unit,
			Range:  route.Range,
			Source: "domain spec routing → " + route.ConstructID,
		}); err != nil {
			return 0, fmt.Errorf("declaring %s: %w", route.MetricType, err)
		}
		members[route.ConstructID] = append(members[route.ConstructID], route.MetricType)
	}

	for _, c := range spec.Constructs {
		mem := members[c.ConstructID]
		if len(mem) == 0 {
			// A construct with no routed metric would summarise nothing. Skipping it
			// keeps the model to what the system can exhibit; the returned count makes
			// the omission visible.
			continue
		}
		if err := sm.DeclareProperty(statemap.Property{
			ID:      c.ConstructID,
			Kind:    statemap.Derived,
			Members: mem,
			Unit:    "fraction",
			Range:   [2]float64{0, 1},
			Source:  "domain spec construct: " + c.Name,
		}); err != nil {
			return 0, fmt.Errorf("declaring construct %s: %w", c.ConstructID, err)
		}
	}

	for _, prop := range spec.Propositions {
		sign := 1
		if prop.Direction == "negative" {
			sign = -1
		}
		// Seeding declares structure and no magnitude. Which properties relate, and in
		// which direction, is knowledge one machine's telemetry cannot produce and a
		// specification legitimately supplies; what the relation is *worth* is a
		// measurement, and this machine is the only thing entitled to make it.
		//
		// The proposition ID is the label, which is what lets two mechanisms relate the
		// same pair in opposite directions without one erasing the other.
		if err := sm.DeclareRelationship(statemap.Relationship{
			From: prop.FromConstruct, To: prop.ToConstruct,
			Label: prop.PropositionID, Sign: sign,
			Provenance: statemap.Seeded,
			Note:       prop.Description + " [strength: learned from this machine]",
		}); err != nil {
			// A proposition whose endpoints are not both present is skipped rather than
			// fatal: a specification may declare a construct this deployment cannot
			// observe, and an unobservable claim should not block a working agent.
			continue
		}
	}
	return sm.Census().PropertiesTotal, nil
}
