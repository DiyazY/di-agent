package statemap

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
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
	// Without takes every present property of a subject — active or stale, since a
	// departing subject's readings are already going quiet — or one named property,
	// to its range floor: a workload leaving means its shares go to zero.
	Without []string
}

// Influence is one incoming relationship's part in the answer.
type Influence struct {
	Relationship string  `json:"relationship"`
	Source       string  `json:"source"`
	SourceValue  float64 `json:"source_value"`
	Strength     float64 `json:"effective_strength"`
	Sign         int     `json:"sign"`
	Contribution float64 `json:"contribution"`
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

// hypothesisDigest fingerprints a request's hypothesis — its assumptions and its
// exclusions — so two questions that differ only in what they suppose cannot share a
// decision id. It reads the request, not the resolved substitution, so the digest is
// what the caller asked rather than what the map made of it.
func hypothesisDigest(req EstimateRequest) string {
	keys := make([]string, 0, len(req.Assume))
	for k := range req.Assume {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+"="+strconv.FormatFloat(req.Assume[k], 'g', -1, 64))
	}
	without := append([]string(nil), req.Without...)
	sort.Strings(without)
	h := sha256.Sum256([]byte(strings.Join(pairs, ",") + "|" + strings.Join(without, ",")))
	return hex.EncodeToString(h[:])[:8]
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
// that produced it, including whatever was hypothesised. The target and every source
// it reads are read through a DecisionBuilder, so the record of what the arithmetic
// consumed is produced by the reading itself and cannot drift from it. The `without`
// exclusions are resolved into floor assumptions from the map before the decision is
// opened, and the record holds those assumptions by value; a floored property that
// relates to the target through nothing is in Assumptions and not in PropertiesRead.
func (m *Map) Estimate(req EstimateRequest) EstimateResult {
	target := req.Target
	for _, w := range req.Without {
		if w == "" {
			// Query.Subject "" means no restriction, so an empty exclusion would take
			// every property in the map, the target included, to its floor and
			// answer with a confident projection of nothing. Refused before the
			// question is even named.
			return EstimateResult{Err: "without must name a subject or a property"}
		}
	}
	for k, v := range req.Assume {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			// Record refuses a non-finite reading; an assumption is a reading the
			// caller supposes, held to the same rule.
			return EstimateResult{Err: "assumption on " + k + " is not a finite number"}
		}
	}
	if req.ID != "" {
		if _, used := m.journal.Decision(req.ID); used {
			// The journal keeps one record per id; a second estimate under the same
			// id would silently replace the first, which is the trace of an answer
			// already given.
			return EstimateResult{Err: "decision id " + req.ID + " is already used"}
		}
	}
	id := req.ID
	if id == "" {
		id = "est-" + strconv.FormatUint(m.Revision(), 10) + "-" + target
		// Neither Decide nor Commit advances the revision, so a baseline and a
		// counterfactual asked at the same revision would claim the same id and
		// the journal would keep only the last of them. The hypothesis is part of
		// the question, so it is part of the name of the answer.
		if len(req.Assume) > 0 || len(req.Without) > 0 {
			id += "-" + hypothesisDigest(req)
		}
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

	// keys is the sorted assumption-key order, built once and reused wherever
	// assume is iterated (the question text here, and the unmatched-assumption
	// loop below) so two identical estimates against an unchanged map produce
	// byte-identical rationale and caveats instead of drifting with Go's
	// randomised map iteration order.
	keys := make([]string, 0, len(assume))
	for k := range assume {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	question := "estimate " + target
	if len(assume) > 0 {
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
	// unknownAssumed tracks sources whose assumption reached the target only through
	// edges with no strength yet, so the caveat that names them fires once per source.
	unknownAssumed := map[string]bool{}
	// seenSource tracks which source properties have already had their
	// source-scoped caveats (range-not-declared, out-of-range assumption,
	// derived-assumed) and their b.Assume rationale line emitted. A source with
	// two incoming relationships must still contribute per-edge arithmetic for
	// each edge, but those caveats and the assumption line describe the source,
	// not the edge, so they must fire only once.
	seenSource := map[string]bool{}
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
			if a, assumed := assume[rel.From]; assumed {
				// The source does relate to the target; what is missing is the
				// strength. "Does not influence" would be the wrong claim.
				assumedSeen[rel.From] = true
				if !unknownAssumed[rel.From] {
					unknownAssumed[rel.From] = true
					b.Assume(rel.From, a)
					b.Caveat("assumption on %s reaches %s through %s, which has no strength yet; it is not in the projection",
						rel.From, target, rel.ID)
				}
			}
			influences = append(influences, inf)
			continue
		}
		firstForSource := !seenSource[rel.From]
		seenSource[rel.From] = true
		if firstForSource && !src.RangeDeclared {
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
			if firstForSource {
				b.Assume(rel.From, a)
				if a < src.Range[0] || a > src.Range[1] {
					b.Caveat("assumed value %g for %s is outside its declared range [%g, %g]", a, src.ID, src.Range[0], src.Range[1])
				}
				if src.Kind == Derived {
					b.Caveat("%s is derived and was assumed directly; its members are unchanged", src.ID)
				}
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
	for _, k := range keys {
		if !assumedSeen[k] {
			b.Assume(k, assume[k])
			b.Caveat("assumption on %s does not influence %s through any relationship", k, target)
		}
	}

	if unknown > 0 {
		// Omitted from the sensitivity and the contributions above, so the answer
		// must say so whether or not anything was assumed.
		b.Caveat("%d influences have no strength yet and are omitted from the sensitivity and the projection", unknown)
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
	// Assumptions and Excluded are copied out of the Decision, not referenced: the
	// Decision is the journal's audit record (journal.go's Commit hands it the
	// builder's own map and slice without copying), so a caller mutating the
	// result must not be able to corrupt what the journal remembers.
	if len(assume) > 0 {
		res.Assumptions = make(map[string]float64, len(d.Assumptions))
		for k, v := range d.Assumptions {
			res.Assumptions[k] = v
		}
	}
	if len(excluded) > 0 {
		res.Excluded = make([]string, len(d.Excluded))
		copy(res.Excluded, d.Excluded)
	}
	return res
}
