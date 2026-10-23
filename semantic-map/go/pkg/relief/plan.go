package relief

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/DiyazY/di-agent/pkg/actuators"
	"github.com/DiyazY/di-agent/pkg/policy"
	"github.com/DiyazY/di-agent/pkg/runtime"
	"github.com/DiyazY/di-agent/pkg/spec"
	"github.com/DiyazY/di-agent/pkg/statemap"
)

// Basis says what a candidate's ranking or a plan's prediction rests on: the
// map's relationships, or only the workload's share of node CPU.
const (
	BasisMap   = "map"
	BasisUsage = "usage"
)

// noCapacity is why nothing can be planned on a machine whose capacity in the
// action's unit is unknown, such as its core count for cpu.limit.
func noCapacity(d actuators.Descriptor) string {
	return fmt.Sprintf("no %s fact: shares cannot become %s", d.Capacity, d.Unit)
}

// usageCaveat is carried by a decision whose prediction the map could not make.
const usageCaveat = "predicted from usage share: the map has no relationship for this workload"

// Candidate is a workload that might be throttled, with what removing it is
// predicted to do to node pressure. Drop is zero for a Basis "usage" candidate:
// the map says nothing about it, and its rank comes from Share.
type Candidate struct {
	Subject string  `json:"subject"`
	Share   float64 `json:"share"`
	Drop    float64 `json:"drop"`
	Basis   string  `json:"basis"`
}

// Plan is one step on one workload: the setting it gets, from its usage now, in
// the action's Unit, and the drop in node pressure that step is predicted to
// bring. EstimateID is empty for a Basis "usage" plan, which no estimate
// produced. Effect is what the prediction was scaled by: what acting on the
// workload has done before against what was predicted, 1 with no history.
type Plan struct {
	Subject    string  `json:"subject"`
	From       float64 `json:"from"`
	To         float64 `json:"to"`
	Unit       string  `json:"unit"`
	Predicted  float64 `json:"predicted"`
	EstimateID string  `json:"estimate_id,omitempty"`
	Basis      string  `json:"basis"`
	Rule       int     `json:"rule"`
	Effect     float64 `json:"effect"`
}

// targetOf is the workload as the policy sees it: its kind is the subject's part
// before the colon, so a rule written for pods never matches a unit.
func targetOf(w runtime.Workload) policy.Target {
	kind, _, _ := strings.Cut(w.Subject, ":")
	return policy.Target{Kind: kind, Name: w.Name, Labels: w.Labels, Self: w.Self}
}

// actionable reports whether the actuator can act on this workload and the
// policy lets it. A Check with a reason is the actuator saying why it cannot.
func actionable(w runtime.Workload, a actuators.Actuator, pol *policy.Policy) bool {
	return a.Check(w) == "" && pol.For(targetOf(w)).Allows(a.Describe().Action)
}

// share is the subject's current share of the node's usage, the property
// usage@subject. A property that is not active says nothing about what the
// workload is doing now.
func share(m *statemap.Map, usage, subject string) (float64, bool) {
	p, ok := m.Property(usage + "@" + subject)
	if !ok || p.Status != statemap.Active {
		return 0, false
	}
	return p.Value, true
}

// Attribute ranks the workloads a can act on, unfrozen and non-victim, by Drop
// (Basis "map": the predicted fall in the node's Relieves without them), falling
// back to Share (Basis "usage") for those the estimate has no influence for. Every map-ranked candidate comes before every usage-ranked one,
// because a ranking the map supports is evidence and a share is only a guess.
// A workload using less than VictimShare is left out whatever its own pressure:
// one that waits a lot but uses little is suffering the contention, not causing
// it.
func Attribute(m *statemap.Map, a actuators.Actuator, ws []runtime.Workload, pol *policy.Policy, c spec.CPURelief) []Candidate {
	d := a.Describe()
	var byMap, byUsage []Candidate
	for _, w := range ws {
		if !actionable(w, a, pol) {
			continue
		}
		s, ok := share(m, d.Usage, w.Subject)
		if !ok || s < c.VictimShare {
			continue
		}
		cand := Candidate{Subject: w.Subject, Share: s, Basis: BasisUsage}
		res := m.Estimate(statemap.EstimateRequest{Target: d.Relieves, Without: []string{w.Subject}})
		if res.Err == "" && res.Hypothetical != nil && influencedBy(res, d.Usage+"@"+w.Subject) {
			cand.Basis, cand.Drop = BasisMap, -res.Hypothetical.Delta
			byMap = append(byMap, cand)
			continue
		}
		byUsage = append(byUsage, cand)
	}
	sort.SliceStable(byMap, func(i, j int) bool {
		if byMap[i].Drop != byMap[j].Drop {
			return byMap[i].Drop > byMap[j].Drop
		}
		return byMap[i].Subject < byMap[j].Subject
	})
	sort.SliceStable(byUsage, func(i, j int) bool {
		if byUsage[i].Share != byUsage[j].Share {
			return byUsage[i].Share > byUsage[j].Share
		}
		return byUsage[i].Subject < byUsage[j].Subject
	})
	return append(byMap, byUsage...)
}

// influencedBy reports whether the subject's own usage property, source, reaches
// the target through a relationship with a known strength. It is the same source the plan's
// prediction assumes a new value for, so a "map" rank and a "map" prediction
// rest on one relationship; one with no strength yet contributes nothing to the
// projection, so it is no basis for a rank.
func influencedBy(res statemap.EstimateResult, source string) bool {
	for _, inf := range res.Influences {
		if inf.Known && inf.Source == source {
			return true
		}
	}
	return false
}

// MakePlan picks the first candidate whose step clears floors and min_effect.
// ok=false with why when none does; why lists each candidate's reason, so a
// refusal says what was considered.
//
// Current usage, from, is the candidate's share times the node's capacity fact,
// in the action's Unit. The step is Step below it, bounded by the actuator's Min
// and the policy floor: never below Floor.CPUCores, never below Floor.OfOriginal
// of the original setting (the one a reads now, or current usage when there is
// none), and never below the workload's request of the action's resource. It
// must also go below the setting a reads now and the limit the workload lists,
// or it is no step. A Basis "map" candidate is predicted by
// assuming its usage scaled to the new setting; a Basis "usage" candidate, which
// the map cannot predict, by its share of the node's pressure per unit of
// usage, times the fraction of its usage the step actually cuts. effect scales a prediction by what acting on the subject has done before
// against predictions of the same basis; 1 means no history.
func MakePlan(ctx context.Context, m *statemap.Map, a actuators.Actuator, cands []Candidate, ws []runtime.Workload, pol *policy.Policy, c spec.CPURelief, effect func(subject, basis string) float64) (p Plan, ok bool, why string) {
	return makePlan(ctx, m, a, cands, ws, pol, c, effect, nil)
}

// makePlan is MakePlan for a loop that has already capped some workloads:
// originals holds, for each, the setting it had before the agent first touched
// it, which is what the of_original floor is a share of. A workload's current
// setting is then the agent's own cap, and a step that would not go below it is
// no step.
func makePlan(ctx context.Context, m *statemap.Map, a actuators.Actuator, cands []Candidate, ws []runtime.Workload, pol *policy.Policy, c spec.CPURelief, effect func(subject, basis string) float64, originals map[string]float64) (p Plan, ok bool, why string) {
	d := a.Describe()
	n, ok := capacity(m, d)
	if !ok {
		return Plan{}, false, noCapacity(d)
	}
	if len(cands) == 0 {
		return Plan{}, false, "no candidate: no actionable workload uses victim_share or more of node CPU"
	}
	byID := make(map[string]runtime.Workload, len(ws))
	for _, w := range ws {
		byID[w.Subject] = w
	}
	var reasons []string
	for _, cand := range cands {
		w, ok := byID[cand.Subject]
		if !ok {
			reasons = append(reasons, cand.Subject+": not a workload the runtime lists")
			continue
		}
		cur, curSet, err := a.Read(ctx, w)
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("%s: reading %s: %v", cand.Subject, d.Action, err))
			continue
		}
		eff := pol.For(targetOf(w))
		from := cand.Share * n
		original := from
		if o, ok := originals[cand.Subject]; ok {
			original = o
		} else if curSet {
			original = cur
		}
		floorMin, floorShare := eff.Settings.Floor.Values()
		to := math.Max(from*(1-c.Step), math.Max(d.Min, math.Max(floorMin, floorShare*original)))
		res := w.Resources[d.Resource]
		if res.Request != nil {
			// A limit below the request is refused by the orchestrator, so the request is a floor.
			to = math.Max(to, *res.Request)
		}
		if to >= from {
			reasons = append(reasons, fmt.Sprintf("%s: floor reached (%.3f %s in use, floor %.3f)", cand.Subject, from, d.Unit, to))
			continue
		}
		if curSet && to >= cur {
			reasons = append(reasons, fmt.Sprintf("%s: already capped at %.3f %s, at or below the step's %.3f", cand.Subject, cur, d.Unit, to))
			continue
		}
		if res.Limit != nil && to >= *res.Limit {
			reasons = append(reasons, fmt.Sprintf("%s: its %s limit is %.3f %s, at or below the step's %.3f", cand.Subject, d.Resource, *res.Limit, d.Unit, to))
			continue
		}
		plan := Plan{Subject: cand.Subject, From: from, To: to, Unit: d.Unit, Basis: cand.Basis, Rule: eff.Rule,
			Effect: effect(cand.Subject, cand.Basis)}
		switch cand.Basis {
		case BasisMap:
			res := m.Estimate(statemap.EstimateRequest{
				Target: d.Relieves,
				Assume: map[string]float64{d.Usage + "@" + cand.Subject: cand.Share * to / from},
			})
			if res.Err != "" || res.Hypothetical == nil {
				reasons = append(reasons, fmt.Sprintf("%s: no prediction (%s)", cand.Subject, res.Err))
				continue
			}
			plan.Predicted, plan.EstimateID = -res.Hypothetical.Delta*plan.Effect, res.DecisionID
		default:
			pressure, okP := m.Property(d.Relieves)
			usage, okU := m.Property(d.Usage)
			if !okP || !okU || usage.Value <= 0 {
				reasons = append(reasons, fmt.Sprintf("%s: no node %s or %s to predict from", cand.Subject, d.Relieves, d.Usage))
				continue
			}
			// The cut actually made, which a floor can hold short of Step.
			plan.Predicted = cand.Share * (1 - to/from) * (pressure.Value / usage.Value) * plan.Effect
		}
		if plan.Predicted < c.MinEffect {
			r := fmt.Sprintf("%s: predicted drop %.4f is below min_effect %g", cand.Subject, plan.Predicted, c.MinEffect)
			if to > from*(1-c.Step) {
				// Otherwise a workload capped near its floor reads as one the map
				// expects little of, when it is the step that is small.
				r += fmt.Sprintf(" (the floor holds the step to %.3f %s, from %.3f)", to, d.Unit, from)
			}
			reasons = append(reasons, r)
			continue
		}
		return plan, true, ""
	}
	return Plan{}, false, strings.Join(reasons, "; ")
}

// capacity is the machine's capacity in the action's unit, the fact the
// descriptor names (cpu_cores for cpu.limit), which turns a share into Unit.
func capacity(m *statemap.Map, d actuators.Descriptor) (float64, bool) {
	f, ok := m.Fact(d.Capacity)
	if !ok || f.Number == nil || *f.Number <= 0 {
		return 0, false
	}
	return *f.Number, true
}
