// Package domain carries the agent's domain model as data.
//
// Constructs, the metric-to-construct routing table, the causal propositions,
// the operator-intent vocabulary, and the bounds on operator adjustment all live
// in a JSON specification rather than in Go literals. Three reasons, in
// increasing order of importance:
//
//  1. The model is domain knowledge, not program logic. Which constructs an
//     agent tracks depends on what its cluster exposes, and that is a property of
//     a deployment.
//
//  2. The Semantic Map claims to be a live structure whose propositions and
//     properties may appear or disappear during a deployment. A baseline compiled
//     into the binary contradicts that claim for the baseline itself.
//
//  3. A construct that appears while a cluster is running is useless unless a
//     metric can reach it. Routing therefore has to be as mutable as the
//     construct set, which rules out a package-level map.
//
// The daemon loads a Spec at startup. Runtime additions go through the ontology
// surface and extend the loaded model in place; nothing here is a compile-time
// constant.
package domain

import (
	"encoding/json"
	"fmt"
	"os"
)

// Spec is the whole domain model as loaded from disk.
type Spec struct {
	Version     string `json:"version"`
	Description string `json:"description"`

	Constructs    []Construct      `json:"constructs"`
	MetricRouting []MetricRoute    `json:"metric_routing"`
	Propositions  []Proposition    `json:"propositions"`
	Policy        AdjustmentPolicy `json:"adjustment_policy"`
	IntentRules   []IntentRule     `json:"intent_rules"`
	// CostModel names which construct plays which role in the Reasoner's cost
	// estimate. Without it the cost function is the last place in the daemon that
	// hardcodes a construct ID.
	CostModel CostModel `json:"cost_model"`
}

type Construct struct {
	ConstructID string `json:"construct_id"`
	Name        string `json:"name"`
	Description string `json:"description"`

	// Polarity is the direction this construct's value runs: HigherIsWorse for a
	// cost or a penalty, HigherIsBetter for a capability or a headroom. Empty
	// defaults to HigherIsWorse.
	//
	// It exists because a proposition's declared direction is only meaningful
	// against a stated polarity on both endpoints. Leaving polarity implicit made a
	// whole class of error undetectable: a proposition inherited from a framework
	// that phrased its outcome as a *goodness* quantity ("reduces throughput")
	// attached to a construct measured as a *badness* quantity (latency, pressure)
	// asserts a sign the system can never show, so its evidence gate never opens and
	// it sits at zero strength looking merely unobserved.
	Polarity Polarity `json:"polarity,omitempty"`
}

// Polarity says which way "better" runs for a measured quantity.
type Polarity string

const (
	// HigherIsWorse suits latency, pressure, utilisation, loss and cost.
	HigherIsWorse Polarity = "higher_is_worse"
	// HigherIsBetter suits throughput, headroom and availability.
	HigherIsBetter Polarity = "higher_is_better"
)

func (p Polarity) withDefault() Polarity {
	if p == "" {
		return HigherIsWorse
	}
	return p
}

func (p Polarity) valid() bool {
	switch p.withDefault() {
	case HigherIsWorse, HigherIsBetter:
		return true
	}
	return false
}

// MetricRoute binds one MetricType to the construct it informs. Range is
// advisory: the ingestion path clamps to it so a mis-scaled collector cannot
// push an edge weight outside the domain its divergence metric is defined on.
type MetricRoute struct {
	MetricType  string     `json:"metric_type"`
	ConstructID string     `json:"construct_id"`
	Unit        string     `json:"unit"`
	Range       [2]float64 `json:"range"`

	// Polarity is the direction the raw metric runs, which need not be the
	// direction its construct runs. When they differ the ingestion path reflects the
	// value within Range before it reaches the map, so a construct fed by both a
	// latency and a throughput summarises two quantities that agree rather than
	// averaging two that cancel. Empty defaults to HigherIsWorse.
	Polarity Polarity `json:"polarity,omitempty"`
}

type Proposition struct {
	PropositionID   string   `json:"proposition_id"`
	FromConstruct   string   `json:"from_construct"`
	ToConstruct     string   `json:"to_construct"`
	Direction       string   `json:"direction"`
	Description     string   `json:"description"`
	EvidenceSources []string `json:"evidence_sources"`
}

// CostModel assigns cost-estimate roles to constructs.
//
// The Reasoner reports two quantities: what running work costs (the resource
// construct) and what performance penalty is being experienced (the pressure
// construct). Each is estimated as the confidence-blended OBSERVED value of that
// construct, with the weighted sum over its incoming edges reported separately as
// a sensitivity — how much the target would move if a source construct changed.
//
// The split is empirical rather than aesthetic: adding the relation sum into the
// level made next-interval predictions monotonically worse, while the level alone
// was the best available predictor. Sensitivity answers the counterfactual the
// level cannot, which is why SimulateOutcome uses it.
type CostModel struct {
	Description       string `json:"description"`
	ResourceConstruct string `json:"resource_construct"`
	PressureConstruct string `json:"pressure_construct"`
	LevelSource       string `json:"level_source"`
}

// AdjustmentPolicy bounds operator adjustment. PerPropositionFloor is keyed by
// proposition ID so a proposition added at runtime can be given a policy without
// a code change.
type AdjustmentPolicy struct {
	Description         string             `json:"description"`
	GlobalFloor         float64            `json:"global_floor"`
	GlobalCeiling       float64            `json:"global_ceiling"`
	PerPropositionFloor map[string]float64 `json:"per_proposition_floor"`
}

// IntentRule maps operator vocabulary to proposition adjustments.
type IntentRule struct {
	Intent   string             `json:"intent"`
	Keywords []string           `json:"keywords"`
	Deltas   map[string]float64 `json:"deltas"`
	Note     string             `json:"note"`
}

// Load reads and validates a Spec.
func Load(path string) (*Spec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("domain spec: cannot read %q: %w", path, err)
	}
	var s Spec
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("domain spec %q: %w", path, err)
	}
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("domain spec %q: %w", path, err)
	}
	return &s, nil
}

// Validate rejects a spec that is internally inconsistent. It fails loud rather
// than dropping the offending element, because a silently-ignored routing entry
// produces a construct that accumulates no evidence and looks, from every
// aggregate, exactly like a construct with nothing happening.
func (s *Spec) Validate() error {
	if len(s.Constructs) == 0 {
		return fmt.Errorf("no constructs declared")
	}
	known := make(map[string]bool, len(s.Constructs))
	for _, c := range s.Constructs {
		if c.ConstructID == "" {
			return fmt.Errorf("construct with empty construct_id")
		}
		if known[c.ConstructID] {
			return fmt.Errorf("duplicate construct %q", c.ConstructID)
		}
		if !c.Polarity.valid() {
			return fmt.Errorf("construct %q has unknown polarity %q (want %q or %q)",
				c.ConstructID, c.Polarity, HigherIsWorse, HigherIsBetter)
		}
		known[c.ConstructID] = true
	}

	seenMetric := map[string]bool{}
	for _, r := range s.MetricRouting {
		if !known[r.ConstructID] {
			return fmt.Errorf("metric %q routes to unknown construct %q", r.MetricType, r.ConstructID)
		}
		if seenMetric[r.MetricType] {
			return fmt.Errorf("metric %q routed more than once", r.MetricType)
		}
		seenMetric[r.MetricType] = true
		if r.Range[1] <= r.Range[0] {
			return fmt.Errorf("metric %q has empty range [%v, %v]", r.MetricType, r.Range[0], r.Range[1])
		}
		if !r.Polarity.valid() {
			return fmt.Errorf("metric %q has unknown polarity %q (want %q or %q)",
				r.MetricType, r.Polarity, HigherIsWorse, HigherIsBetter)
		}
	}

	props := map[string]bool{}
	for _, p := range s.Propositions {
		if p.PropositionID == "" {
			return fmt.Errorf("proposition with empty proposition_id")
		}
		if props[p.PropositionID] {
			return fmt.Errorf("duplicate proposition %q", p.PropositionID)
		}
		props[p.PropositionID] = true
		if !known[p.FromConstruct] {
			return fmt.Errorf("proposition %q has unknown from_construct %q", p.PropositionID, p.FromConstruct)
		}
		if !known[p.ToConstruct] {
			return fmt.Errorf("proposition %q has unknown to_construct %q", p.PropositionID, p.ToConstruct)
		}
		if p.Direction != "positive" && p.Direction != "negative" {
			return fmt.Errorf("proposition %q has direction %q, want positive or negative", p.PropositionID, p.Direction)
		}
	}

	if s.Policy.GlobalFloor < 0 || s.Policy.GlobalCeiling > 1 ||
		s.Policy.GlobalFloor >= s.Policy.GlobalCeiling {
		return fmt.Errorf("adjustment policy bounds [%v, %v] are not a valid sub-interval of [0,1]",
			s.Policy.GlobalFloor, s.Policy.GlobalCeiling)
	}
	for pid := range s.Policy.PerPropositionFloor {
		if !props[pid] {
			return fmt.Errorf("per_proposition_floor names unknown proposition %q", pid)
		}
	}
	for _, r := range s.IntentRules {
		if len(r.Keywords) == 0 {
			return fmt.Errorf("intent %q has no keywords", r.Intent)
		}
		for pid := range r.Deltas {
			if !props[pid] {
				return fmt.Errorf("intent %q adjusts unknown proposition %q", r.Intent, pid)
			}
		}
	}

	// The cost model must name constructs that exist, and must name both. A
	// half-declared cost model would leave the Reasoner reporting one quantity and
	// silently zeroing the other, which reads as "this cluster has no pressure"
	// rather than as a configuration error.
	if s.CostModel.ResourceConstruct == "" || s.CostModel.PressureConstruct == "" {
		return fmt.Errorf("cost_model must name both resource_construct and pressure_construct")
	}
	for role, id := range map[string]string{
		"resource_construct": s.CostModel.ResourceConstruct,
		"pressure_construct": s.CostModel.PressureConstruct,
	} {
		if !known[id] {
			return fmt.Errorf("cost_model %s names unknown construct %q", role, id)
		}
	}
	return nil
}

// ConstructForMetric resolves a MetricType to its construct.
func (s *Spec) ConstructForMetric(metricType string) (string, bool) {
	for _, r := range s.MetricRouting {
		if r.MetricType == metricType {
			return r.ConstructID, true
		}
	}
	return "", false
}

// RangeForMetric returns the declared range for a MetricType.
func (s *Spec) RangeForMetric(metricType string) ([2]float64, bool) {
	for _, r := range s.MetricRouting {
		if r.MetricType == metricType {
			return r.Range, true
		}
	}
	return [2]float64{}, false
}

// PolarityForConstruct reports the direction a construct's value runs.
func (s *Spec) PolarityForConstruct(constructID string) (Polarity, bool) {
	for _, c := range s.Constructs {
		if c.ConstructID == constructID {
			return c.Polarity.withDefault(), true
		}
	}
	return "", false
}

// NormalizeForConstruct expresses a raw metric value in its construct's polarity,
// reflecting it within the declared range when the two disagree:
//
//	v' = range.lo + range.hi - v
//
// An unrouted metric is returned unchanged: it becomes a property in its own right
// and belongs to no construct, so there is no polarity to reconcile it with.
//
// This is the one place the reconciliation happens. Doing it at ingestion rather
// than at read time means the stored property, every derived summary over it, and
// every relationship learned from it all speak the same direction — so a
// proposition's declared sign can be checked against an observed correlation
// without a caller having to know which metrics were inverted on the way in.
func (s *Spec) NormalizeForConstruct(metricType string, value float64) float64 {
	for _, r := range s.MetricRouting {
		if r.MetricType != metricType {
			continue
		}
		want, ok := s.PolarityForConstruct(r.ConstructID)
		if !ok || r.Polarity.withDefault() == want {
			return value
		}
		return r.Range[0] + r.Range[1] - value
	}
	return value
}

// FloorFor returns the adjustment floor for a proposition: its per-proposition
// override when one is declared, otherwise the global floor.
func (s *Spec) FloorFor(propositionID string) float64 {
	if f, ok := s.Policy.PerPropositionFloor[propositionID]; ok {
		return f
	}
	return s.Policy.GlobalFloor
}

// AddConstruct registers a construct discovered at runtime. Idempotent.
func (s *Spec) AddConstruct(c Construct) error {
	if c.ConstructID == "" {
		return fmt.Errorf("empty construct_id")
	}
	for _, e := range s.Constructs {
		if e.ConstructID == c.ConstructID {
			return nil
		}
	}
	s.Constructs = append(s.Constructs, c)
	return nil
}

// AddMetricRoute binds a MetricType to a construct at runtime. This is what
// makes a construct added mid-deployment able to accumulate evidence; without it
// the construct exists and nothing can ever reach it.
func (s *Spec) AddMetricRoute(r MetricRoute) error {
	known := false
	for _, c := range s.Constructs {
		if c.ConstructID == r.ConstructID {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("cannot route %q: construct %q is not declared", r.MetricType, r.ConstructID)
	}
	if r.Range[1] <= r.Range[0] {
		return fmt.Errorf("metric %q has empty range", r.MetricType)
	}
	for i, e := range s.MetricRouting {
		if e.MetricType == r.MetricType {
			s.MetricRouting[i] = r // re-point an existing metric
			return nil
		}
	}
	s.MetricRouting = append(s.MetricRouting, r)
	return nil
}
