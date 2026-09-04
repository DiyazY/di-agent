package statemap

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// EstimateRequest asks the map about one property, optionally under hypotheses.
type EstimateRequest struct {
	ID     string
	Target string
	// Assume substitutes a source property's value in the contribution sum.
	Assume map[string]float64
	// Without takes every active property of a subject (or one named property) to
	// its range floor: a workload leaving means its shares go to zero.
	Without []string
}

// Influence is one incoming relationship's part in the answer.
type Influence struct {
	Relationship string  `json:"relationship"`
	Source       string  `json:"source"`
	SourceValue  float64 `json:"source_value"`
	Strength     float64 `json:"effective_strength,omitempty"`
	Sign         int     `json:"sign"`
	Contribution float64 `json:"contribution,omitempty"`
	Provenance   string  `json:"provenance"`
	Basis        string  `json:"basis"`
	Known        bool    `json:"known"`

	HypotheticalSourceValue  *float64 `json:"hypothetical_source_value,omitempty"`
	HypotheticalContribution *float64 `json:"hypothetical_contribution,omitempty"`
}

// EstimateAnswer is the part of the result the journal records as the answer.
type EstimateAnswer struct {
	Target        string  `json:"target"`
	Level         float64 `json:"level"`
	Confidence    float64 `json:"confidence"`
	Status        Status  `json:"status"`
	Sensitivity   float64 `json:"sensitivity"`
	Contributions float64 `json:"contributions"`
}

// Hypothetical is what the answer becomes under the request's assumptions.
type Hypothetical struct {
	Contributions  float64 `json:"contributions"`
	Delta          float64 `json:"delta"`
	ProjectedLevel float64 `json:"projected_level"`
}

// EstimateResult is the full answer with its trace.
type EstimateResult struct {
	DecisionID   string             `json:"decision_id"`
	Revision     uint64             `json:"revision"`
	Answer       EstimateAnswer     `json:"answer"`
	Influences   []Influence        `json:"influences"`
	Assumptions  map[string]float64 `json:"assumptions,omitempty"`
	Excluded     []string           `json:"excluded,omitempty"`
	Hypothetical *Hypothetical      `json:"hypothetical,omitempty"`
	Rationale    string             `json:"rationale"`
	Caveats      []string           `json:"caveats,omitempty"`
	// Err is set when no answer could be made; the decision is still recorded.
	Err string `json:"error,omitempty"`
}

const slopeCaveat = "strength is a correlation magnitude used as a unit sensitivity, not a fitted slope; " +
	"the projection is linear in normalised units"

// normalise expresses v as a fraction of the property's declared range.
func normalise(p Property, v float64) float64 {
	lo, hi := p.Range[0], p.Range[1]
	if hi <= lo {
		return v
	}
	return (v - lo) / (hi - lo)
}

// Estimate answers a question FROM the map and records the answer with the state
// that produced it, including whatever was hypothesised. The answer is assembled by
// reading through a DecisionBuilder, so the record of what was read is produced by
// the reading itself and cannot drift from it.
func (m *Map) Estimate(req EstimateRequest) EstimateResult {
	target := req.Target
	id := req.ID
	if id == "" {
		id = "est-" + strconv.FormatUint(m.Revision(), 10) + "-" + target
	}

	// Resolve `without` into assumptions at the floor before the question is named.
	assume := map[string]float64{}
	for k, v := range req.Assume {
		assume[k] = v
	}
	var excluded, withoutCaveats []string
	for _, w := range req.Without {
		if p, ok := m.Property(w); ok {
			assume[p.ID] = p.Range[0]
			excluded = append(excluded, w)
			continue
		}
		view := m.State(Query{Subject: w})
		if len(view.Properties) == 0 {
			withoutCaveats = append(withoutCaveats, fmt.Sprintf("without %s names nothing in the map", w))
			continue
		}
		for _, p := range view.Properties {
			assume[p.ID] = p.Range[0]
		}
		excluded = append(excluded, w)
	}

	question := "estimate " + target
	if len(assume) > 0 {
		keys := make([]string, 0, len(assume))
		for k := range assume {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+strconv.FormatFloat(assume[k], 'g', -1, 64))
		}
		question += " assuming " + strings.Join(parts, ", ")
	}
	if len(excluded) > 0 {
		question += " without " + strings.Join(excluded, ", ")
	}

	b := m.Decide(id, question)
	for _, c := range withoutCaveats {
		b.Caveat("%s", c)
	}
	for _, x := range excluded {
		b.Exclude(x)
	}
	p, ok := b.Property(target)
	if !ok {
		d := b.Commit(map[string]any{"error": "unknown property"})
		return EstimateResult{DecisionID: d.ID, Revision: d.Revision, Caveats: d.Caveats, Err: "no property " + target}
	}

	var influences []Influence
	var sensitivity, contributions, hypContributions, delta float64
	var unknown int
	assumedSeen := map[string]bool{}
	for _, rel := range b.RelationshipsInto(target) {
		src, ok := b.Property(rel.From)
		if !ok {
			continue
		}
		eff, known := rel.Effective()
		inf := Influence{Relationship: rel.ID, Source: rel.From, SourceValue: src.Value,
			Sign: rel.Sign, Provenance: string(rel.Provenance), Basis: rel.Basis(), Known: known}
		if !known {
			unknown++
			influences = append(influences, inf)
			continue
		}
		if !src.RangeDeclared {
			b.Caveat("range for %s was assumed, not declared; its contribution is normalised by [%g, %g]",
				src.ID, src.Range[0], src.Range[1])
		}
		vhat := normalise(src, src.Value)
		contribution := eff * float64(rel.Sign) * vhat
		sensitivity += eff * float64(rel.Sign)
		contributions += contribution
		inf.Strength, inf.Contribution = eff, contribution
		hyp := contribution
		if a, assumed := assume[rel.From]; assumed {
			assumedSeen[rel.From] = true
			b.Assume(rel.From, a)
			if a < src.Range[0] || a > src.Range[1] {
				b.Caveat("assumed value %g for %s is outside its declared range [%g, %g]", a, src.ID, src.Range[0], src.Range[1])
			}
			if src.Kind == Derived {
				b.Caveat("%s is derived and was assumed directly; its members are unchanged", src.ID)
			}
			ahat := normalise(src, a)
			hyp = eff * float64(rel.Sign) * ahat
			av, hv := a, hyp
			inf.HypotheticalSourceValue = &av
			inf.HypotheticalContribution = &hv
		}
		hypContributions += hyp
		delta += hyp - contribution
		influences = append(influences, inf)
	}
	for k, v := range assume {
		if !assumedSeen[k] {
			b.Assume(k, v)
			b.Caveat("assumption on %s does not influence %s through any relationship", k, target)
		}
	}

	b.Note("level of %s is %.4f at confidence %.3f from %d observations", target, p.Value, p.Confidence, p.NObservations)
	b.Note("%d influences: sensitivity %+.4f per normalised unit, contributing %+.4f at current values",
		len(influences), sensitivity, contributions)
	if len(influences) == 0 {
		b.Caveat("nothing relates to %s, so the level is all the map can offer about it", target)
	}

	answer := EstimateAnswer{Target: target, Level: p.Value, Confidence: p.Confidence, Status: p.Status,
		Sensitivity: sensitivity, Contributions: contributions}
	commit := map[string]any{"target": target, "level": p.Value, "confidence": p.Confidence,
		"status": string(p.Status), "sensitivity": sensitivity, "contributions": contributions}

	var hypothetical *Hypothetical
	if len(assume) > 0 {
		b.Caveat("%s", slopeCaveat)
		if unknown > 0 {
			b.Caveat("%d influences have no strength yet and are omitted from the projection", unknown)
		}
		lo, hi := p.Range[0], p.Range[1]
		projected := p.Value + delta
		if hi > lo {
			projected = p.Value + delta*(hi-lo)
			if projected < lo {
				projected = lo
			}
			if projected > hi {
				projected = hi
			}
		}
		hypothetical = &Hypothetical{Contributions: hypContributions, Delta: delta, ProjectedLevel: projected}
		commit["hypothetical"] = map[string]any{"contributions": hypContributions, "delta": delta, "projected_level": projected}
		b.Note("under the assumptions: contributions %+.4f, delta %+.4f, projected level %.4f", hypContributions, delta, projected)
	}

	d := b.Commit(commit)
	res := EstimateResult{DecisionID: d.ID, Revision: d.Revision, Answer: answer, Influences: influences,
		Hypothetical: hypothetical, Rationale: d.Rationale, Caveats: d.Caveats}
	if len(assume) > 0 {
		res.Assumptions = d.Assumptions
	}
	if len(excluded) > 0 {
		res.Excluded = d.Excluded
	}
	return res
}
