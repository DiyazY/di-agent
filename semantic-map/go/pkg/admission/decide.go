package admission

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/DiyazY/di-agent/pkg/statemap"
)

// Domain is one relief lever this node runs, as admission reads it: the actuator
// descriptor's metric names, the lever's levels, and its loop's current phase.
type Domain struct {
	Action, Resource, Relieves, Usage, Capacity string
	Exit, Enter                                 float64
	Settle                                      time.Duration
	// Phase is the loop's relief.Phase as a string; this package does not import
	// relief, which imports peers, which carries offers.
	Phase string
}

// Self is the answering node.
type Self struct{ NodeID, Arch string }

// Line is one domain's evaluation: what was read, both predictions of Relieves
// after the workload arrives, and which one decided.
type Line struct {
	Domain, Phase, Basis, Reason string
	Share, Pressure, Usage, Exit float64
	MapPred, UsagePred, K        *float64
	MemoryHeadroom               *float64
	KLastSource                  string
	KLastDecision                string
	KObservations                int
	predictionDeclined           bool
	Pass                         bool
}

// Decision is the answer to an offer and the evaluation behind it.
type Decision struct {
	ID, Verdict, Reason string
	Lines               []Line
	PredictionReached   bool
	PredictionChanged   bool
}

// Decide answers an offer from this node's map and records the answer under id.
// It accepts only when every domain predicts its pressure at or below Exit, the
// level relief lets caps go at: admitting up to Enter would hold pressure in the
// band where caps never release.
func Decide(m *statemap.Map, o Offer, domains []Domain, self Self, id string) Decision {
	b := m.Decide(id, "admission", "admit "+o.Name+"?")
	b.Input("offer", cloneOffer(o))
	lines := make([]Line, len(domains))
	for i, d := range domains {
		lines[i] = Line{Domain: d.Relieves, Phase: d.Phase, Exit: d.Exit}
	}
	predictionReached := false
	predictionChanged := false
	answer := func(verdict, reason string) Decision {
		b.Input("domains", slices.Clone(lines))
		b.Commit(map[string]any{"verdict": verdict, "reason": reason})
		return Decision{ID: id, Verdict: verdict, Reason: reason, Lines: lines,
			PredictionReached: predictionReached, PredictionChanged: predictionChanged}
	}

	if len(o.Arch) > 0 && !slices.ContainsFunc(o.Arch, func(a string) bool { return strings.EqualFold(a, self.Arch) }) {
		return answer(Decline, "cannot run on "+self.Arch)
	}
	// A node never admits blind.
	if len(domains) == 0 {
		return answer(Decline, "no admission domains")
	}
	// Headroom created by throttling is not spare capacity, and a phase this
	// package does not know is not calm. "calm" is relief.Calm.
	for _, d := range domains {
		if d.Phase != "calm" {
			return answer(Decline, "relief active on "+d.Relieves)
		}
	}
	for _, d := range domains {
		dem, ok := o.Demand[d.Resource]
		if !ok {
			return answer(Decline, "unknown "+d.Resource+" demand")
		}
		// No data relates one architecture's CPU to another's; memory carries over.
		if d.Resource == "cpu" && dem.Arch != "" && !strings.EqualFold(dem.Arch, self.Arch) {
			return answer(Decline, "cpu demand not from this architecture")
		}
	}

	// The headroom gate is Admit's: a calm node offered more than it has left reads
	// 0 on every pressure-based prediction, so the shares must fit first.
	req := statemap.AdmitRequest{ID: id + "-gate",
		CPUCores: o.Demand["cpu"].Value, MemoryBytes: o.Demand["memory"].Value}
	for _, d := range domains {
		switch d.Relieves {
		case "cpu_contention_ratio":
			req.CPUEnter = d.Enter
		case "memory_contention_ratio":
			req.MemoryEnter = d.Enter
		}
	}
	gate := m.Admit(req)
	b.Input("gate", gate.DecisionID)
	b.Cite(gate.DecisionID)
	if gate.Err != "" {
		return answer(Decline, gate.Err)
	}
	if !gate.Accepted {
		return answer(Decline, gate.Reason)
	}
	predictionReached = true

	facts := map[string]statemap.Fact{}
	for _, f := range b.Facts() {
		facts[f.Key] = f
	}
	reason := ""
	nonPredictionFailure := false
	for i, d := range domains {
		lines[i] = predict(b, m, d, o.Demand[d.Resource].Value, facts[d.Capacity])
		predictionChanged = predictionChanged || lines[i].predictionDeclined
		nonPredictionFailure = nonPredictionFailure || (!lines[i].Pass && !lines[i].predictionDeclined)
		if !lines[i].Pass && reason == "" {
			reason = lines[i].Reason
		}
		predictionChanged = predictionChanged && !nonPredictionFailure
	}
	if reason != "" {
		return answer(Decline, reason)
	}
	return answer(Accept, "every domain stays at or below its exit level")
}

// predict evaluates one domain. Both predictions are made whenever their inputs
// exist, so the record shows what the basis that did not decide would have said.
func predict(b *statemap.DecisionBuilder, m *statemap.Map, d Domain, demand float64, capacity statemap.Fact) Line {
	l := Line{Domain: d.Relieves, Phase: d.Phase, Exit: d.Exit}
	var notes []string
	fail := func(why string) Line {
		l.Reason = strings.Join(append(notes, why), "; ")
		return l
	}

	usage, _ := reading(b, d.Usage)
	l.Usage = usage
	if capacity.Number == nil || *capacity.Number <= 0 {
		return fail("no " + d.Capacity + " fact")
	}
	l.Share = demand / *capacity.Number
	// Node contention is the worst of the subjects' own, so it is absent when no
	// subject reports one: an idle node, or one running only units and BestEffort
	// pods, is calm. A stale or retired reading is read the same way, as Admit reads
	// it: with no reporter the node has been emptied. While a subject reports one,
	// an absent or silent node reading is a gap.
	var pressure float64
	p, ok := b.Property(d.Relieves)
	switch {
	case ok && p.NObservations > 0 && p.Status == statemap.Active:
		pressure = p.Value
	case !ok || p.NObservations == 0:
		if r := reporters(b, m, d.Relieves); len(r) > 0 {
			return fail("no " + d.Relieves + " reading")
		}
		notes = append(notes, "no subject reports "+d.Relieves)
	default:
		if r := reporters(b, m, d.Relieves); len(r) > 0 {
			return fail(fmt.Sprintf("%s is %s while %s still reports it", d.Relieves, p.Status, r[0]))
		}
		notes = append(notes, fmt.Sprintf("%s is %s and no subject reports it", d.Relieves, p.Status))
	}
	l.Pressure = pressure
	headroomExceeded := false
	memoryHeadroom := 0.0
	if d.Resource == "memory" {
		const id = "memory_admission_headroom_ratio"
		p, ok := b.Property(id)
		switch {
		case !ok:
			notes = append(notes, "memory admission headroom unavailable: missing")
		case p.NObservations == 0:
			notes = append(notes, "memory admission headroom unavailable: no observations")
		case p.Status != statemap.Active:
			notes = append(notes, fmt.Sprintf("memory admission headroom unavailable: %s", p.Status))
		default:
			age := b.At().Sub(p.LastObserved)
			if age < 0 || age > m.StaleAfter() {
				notes = append(notes, "memory admission headroom unavailable: stale")
				break
			}
			headroom := p.Value
			l.MemoryHeadroom = &headroom
			memoryHeadroom = headroom
			headroomExceeded = l.Share > headroom
		}
	}

	// k is the strongest response slope measured on a live relationship from this
	// usage metric to relieves. Unlike relationship strength, it may exceed 1.
	// Its estimate is shrunk toward zero until repeated outcomes build support.
	if e, ok := m.MeasuredDomainEffect(d.Usage, d.Relieves); ok {
		k := e.Estimate()
		l.K, l.KObservations = &k, e.N
		l.KLastSource, l.KLastDecision = e.LastSource, e.LastDecision
	}
	if l.K != nil {
		p := clamp01(pressure + *l.K*l.Share)
		l.MapPred = &p
	}
	if usage > 0 {
		p := clamp01(pressure * (usage + l.Share) / usage)
		l.UsagePred = &p
	}
	if headroomExceeded {
		l.Basis = "memory-headroom"
		return fail(fmt.Sprintf("memory demand share %.4f exceeds admission headroom %.4f", l.Share, memoryHeadroom))
	}
	var pred float64
	switch {
	case l.MapPred != nil:
		l.Basis, pred = "map", *l.MapPred
	case l.UsagePred != nil:
		l.Basis, pred = "usage", *l.UsagePred
	default:
		return fail("no basis to predict " + d.Relieves)
	}
	if pred > d.Exit {
		l.predictionDeclined = true
		return fail(fmt.Sprintf("%s would reach %.4f on the %s basis, above exit %.4f", d.Relieves, pred, l.Basis, d.Exit))
	}
	l.Pass = true
	l.Reason = strings.Join(notes, "; ")
	return l
}

// reading is a property's value when it rests on an observation; a seeded or
// retired property is not a reading.
func reading(b *statemap.DecisionBuilder, id string) (float64, bool) {
	p, ok := b.Property(id)
	if !ok || p.NObservations == 0 || p.Status == statemap.Retired {
		return 0, false
	}
	return p.Value, true
}

// reporters lists the active properties relieves is the worst of, from Admit's
// reporter table (memory_contention_ratio is the worst pod's memory_pressure_ratio),
// or <relieves>@<subject> for a reading the table does not name, and records them.
func reporters(b *statemap.DecisionBuilder, m *statemap.Map, relieves string) []string {
	prefix, ok := statemap.ContentionReporter(relieves)
	if !ok {
		prefix = relieves + "@"
	}
	r := m.ActiveWithPrefix(prefix)
	b.Input(relieves+"_reporters", r)
	return r
}

// cloneOffer copies an offer for the record, which must not change if the caller
// later edits its offer.
func cloneOffer(o Offer) Offer {
	o.Arch = slices.Clone(o.Arch)
	o.Demand = maps.Clone(o.Demand)
	o.Chain = slices.Clone(o.Chain)
	return o
}

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }
