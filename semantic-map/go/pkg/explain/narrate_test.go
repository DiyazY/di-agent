package explain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/admission"
	"github.com/DiyazY/di-agent/pkg/relief"
	"github.com/DiyazY/di-agent/pkg/statemap"
	"github.com/DiyazY/di-agent/pkg/types"
)

var narrateAt = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

const (
	estID    = "est-1.1-cpu_contention_ratio"
	actID    = "relief-act-cpu.limit-1.2"
	verifyID = "relief-verify-cpu.limit-1.3"

	// The act and estimate sentences every relief outcome below rests on.
	actSentence = "capped pod:a (cpu.limit to 0.5 cores) because cpu_contention_ratio stayed above 0.25 for 1m0s (at 0.31); predicted a drop of 0.08 from the map (usage@pod:a->cpu_contention_ratio, intervened, measured 0.6)"
	actLine     = "  rests on relief-act-cpu.limit-1.2 [relief.act] at 2026-01-01T12:00:00Z: " + actSentence
	estLine     = "  rests on est-1.1-cpu_contention_ratio [estimate] at 2026-01-01T12:00:00Z: estimated cpu_contention_ratio at 0.31 (confidence 0.9, active); sensitivity 0.25 [0.2, 0.3] from 1 influences, contributing 0.1"
)

func narrateMap(t *testing.T) *statemap.Map {
	t.Helper()
	m := statemap.New(statemap.Config{}, statemap.NewJournal(0))
	m.SetClock(func() time.Time { return narrateAt })
	return m
}

// decide commits one decision on m and returns it as the journal holds it.
func decide(t *testing.T, m *statemap.Map, id, kind, q string, cites []string, inputs, answer map[string]any) statemap.Decision {
	t.Helper()
	b := m.Decide(id, kind, q)
	for _, c := range cites {
		b.Cite(c)
	}
	for k, v := range inputs {
		b.Input(k, v)
	}
	if b.Commit(answer) == nil {
		t.Fatalf("decision %s not recorded", id)
	}
	d, ok := m.Journal().Decision(id)
	if !ok {
		t.Fatalf("decision %s not held", id)
	}
	return *d
}

// estimate is the estimate the act rests on: one relationship read, measured
// by an earlier intervention.
func estimate() statemap.Decision {
	measured := 0.6
	return statemap.Decision{ID: estID, At: narrateAt, Kind: "estimate", Question: "estimate cpu_contention_ratio",
		RelationshipsRead: []statemap.Relationship{{ID: "usage@pod:a->cpu_contention_ratio", From: "usage@pod:a",
			To: "cpu_contention_ratio", Sign: 1, Provenance: statemap.Intervened, Measured: &measured}},
		Answer: map[string]any{"target": "cpu_contention_ratio", "level": 0.31, "confidence": 0.9, "status": "active",
			"sensitivity": 0.25, "sensitivity_low": 0.2, "sensitivity_high": 0.3, "contributions": 0.1}}
}

func actPlan() map[string]any {
	return map[string]any{"subject": "pod:a", "from": 1.0, "basis": "map", "estimate_id": estID, "to": 0.5,
		"unit": "cores", "predicted": 0.08, "rule": 0.0, "effect": 0.0}
}

func act(t *testing.T, m *statemap.Map, plan any) statemap.Decision {
	return decide(t, m, actID, "relief.act", "cap pod:a's cpu through cpu.limit to relieve cpu_contention_ratio?",
		[]string{estID}, map[string]any{"plan": plan, "notice": map[string]any{"relieves": "cpu_contention_ratio",
			"enter": 0.25, "enter_for": "1m0s", "pressure": 0.31, "phase": "relieving", "since": narrateAt}},
		map[string]any{"action": "cpu.limit", "subject": "pod:a", "to": 0.5, "unit": "cores", "predicted": 0.08})
}

func verify(t *testing.T, m *statemap.Map, observed float64, verdict string) statemap.Decision {
	return decide(t, m, verifyID, "relief.verify", "did capping pod:a's cpu through cpu.limit relieve cpu_contention_ratio?",
		[]string{actID}, map[string]any{"act": actID, "observed": observed, "predicted": 0.08},
		map[string]any{"verdict": verdict, "observed": observed, "predicted": 0.08})
}

func held(ds ...statemap.Decision) []statemap.ChainLink {
	out := make([]statemap.ChainLink, len(ds))
	for i := range ds {
		out[i] = statemap.ChainLink{ID: ds[i].ID, Decision: &ds[i]}
	}
	return out
}

func wantNarrative(t *testing.T, got string, want ...string) {
	t.Helper()
	if w := strings.Join(want, "\n"); got != w {
		t.Fatalf("narrative\n%s\nwant\n%s", got, w)
	}
}

func TestNarrateReliefChainReadsAsOneSentence(t *testing.T) {
	m := narrateMap(t)
	a := act(t, m, actPlan())
	v := verify(t, m, 0.05, "keep")
	wantNarrative(t, Narrate(v, held(a, estimate())),
		"relief-verify-cpu.limit-1.3 [relief.verify] at 2026-01-01T12:00:00Z: capped pod:a (cpu.limit to 0.5 cores) because cpu_contention_ratio stayed above 0.25 for 1m0s (at 0.31); predicted a drop of 0.08 from the map (usage@pod:a->cpu_contention_ratio, intervened, measured 0.6); observed 0.05; kept",
		actLine, estLine)
}

func TestNarrateSaysWhenAnAncestorIsEvicted(t *testing.T) {
	m := narrateMap(t)
	v := verify(t, m, 0.05, "keep")
	wantNarrative(t, Narrate(v, []statemap.ChainLink{{ID: actID, Evicted: true}}),
		"relief-verify-cpu.limit-1.3 [relief.verify] at 2026-01-01T12:00:00Z: the change to pod:a (act relief-act-cpu.limit-1.2 no longer held); observed 0.05; kept",
		"  rests on relief-act-cpu.limit-1.2: no longer held")
}

func TestNarrate_ReliefActWithItsEstimateEvicted(t *testing.T) {
	m := narrateMap(t)
	a := act(t, m, actPlan())
	wantNarrative(t, Narrate(a, []statemap.ChainLink{{ID: estID, Evicted: true}}),
		"relief-act-cpu.limit-1.2 [relief.act] at 2026-01-01T12:00:00Z: capped pod:a (cpu.limit to 0.5 cores) because cpu_contention_ratio stayed above 0.25 for 1m0s (at 0.31); predicted a drop of 0.08 from the map (estimate est-1.1-cpu_contention_ratio no longer held)",
		"  rests on est-1.1-cpu_contention_ratio: no longer held")
}

func TestNarrate_ReliefActOnTheUsageBasis(t *testing.T) {
	m := narrateMap(t)
	plan := actPlan()
	plan["basis"], plan["effect"] = "usage", 0.7
	a := act(t, m, plan)
	wantNarrative(t, Narrate(a, held(estimate())),
		"relief-act-cpu.limit-1.2 [relief.act] at 2026-01-01T12:00:00Z: capped pod:a (cpu.limit to 0.5 cores) because cpu_contention_ratio stayed above 0.25 for 1m0s (at 0.31); predicted a drop of 0.08 from the workload's share of usage (effect 0.7)",
		estLine)
}

func TestNarrate_ReliefVerifyRevertedAndConfounded(t *testing.T) {
	m := narrateMap(t)
	a := act(t, m, actPlan())
	v := decide(t, m, verifyID, "relief.verify", "did capping pod:a's cpu through cpu.limit relieve cpu_contention_ratio?",
		[]string{actID}, map[string]any{"act": actID, "observed": 0.01,
			"confounded":   map[string]any{"confounded": true, "moved": []string{"pod:b"}},
			"intervention": map[string]any{"relationship": "usage@pod:a->cpu_contention_ratio", "changed": "measured"}},
		map[string]any{"verdict": "revert"})
	wantNarrative(t, Narrate(v, held(a, estimate())),
		"relief-verify-cpu.limit-1.3 [relief.verify] at 2026-01-01T12:00:00Z: "+actSentence+"; observed 0.01; reverted (confounded: other workloads moved); relationship usage@pod:a->cpu_contention_ratio measured",
		actLine, estLine)
}

func TestNarrate_ReliefRevert(t *testing.T) {
	m := narrateMap(t)
	a := act(t, m, actPlan())
	v := verify(t, m, 0.01, "revert")
	r := decide(t, m, "relief-revert-cpu.limit-1.4", "relief.revert", "give back pod:a's change: capping it did not help",
		[]string{actID, verifyID}, map[string]any{"act": actID, "verify": verifyID,
			"entry":    map[string]any{"subject": "pod:a"},
			"restored": map[string]any{"act": "relief-act-cpu.limit-1.1", "to": 1.2, "unit": "cores"}},
		map[string]any{"reverted": "pod:a"})
	got := Narrate(r, held(a, estimate(), v))
	first, _, _ := strings.Cut(got, "\n")
	wantNarrative(t, first,
		"relief-revert-cpu.limit-1.4 [relief.revert] at 2026-01-01T12:00:00Z: "+actSentence+"; observed 0.01; reverted, restoring 1.2 cores from relief-act-cpu.limit-1.1")
}

// outcome is a relief outcome of kind citing the act, with its entry.
func outcome(t *testing.T, kind string, inputs, answer map[string]any) string {
	t.Helper()
	m := narrateMap(t)
	a := act(t, m, actPlan())
	in := map[string]any{"act": actID, "entry": map[string]any{"subject": "pod:a", "decision_id": actID}}
	for k, v := range inputs {
		in[k] = v
	}
	d := decide(t, m, "relief-x-cpu.limit-1.9", kind, "give back pod:a's change?", []string{actID}, in, answer)
	got := Narrate(d, held(a, estimate()))
	if !strings.HasSuffix(got, "\n"+actLine+"\n"+estLine) {
		t.Fatalf("narrative %q does not rest on the act and estimate", got)
	}
	first, _, _ := strings.Cut(got, "\n")
	return first
}

func TestNarrate_ReliefExpired(t *testing.T) {
	wantNarrative(t, outcome(t, "relief.expired", nil, map[string]any{"expired": "pod:a"}),
		"relief-x-cpu.limit-1.9 [relief.expired] at 2026-01-01T12:00:00Z: "+actSentence+"; the change expired and was given back")
}

func TestNarrate_ReliefRelease(t *testing.T) {
	wantNarrative(t, outcome(t, "relief.release", nil, map[string]any{"released": "pod:a"}),
		"relief-x-cpu.limit-1.9 [relief.release] at 2026-01-01T12:00:00Z: "+actSentence+"; the runtime no longer listed the workload and the change was given back")
}

func TestNarrate_ReliefFloored(t *testing.T) {
	wantNarrative(t, outcome(t, "relief.floored", nil, map[string]any{"floored": "pod:a"}),
		"relief-x-cpu.limit-1.9 [relief.floored] at 2026-01-01T12:00:00Z: "+actSentence+"; the workload's floor rose above the cap and the change was given back")
}

func TestNarrate_ReliefManual(t *testing.T) {
	wantNarrative(t, outcome(t, "relief.manual", nil, map[string]any{"manual": "pod:a"}),
		"relief-x-cpu.limit-1.9 [relief.manual] at 2026-01-01T12:00:00Z: "+actSentence+"; an operator undid the change")
}

func TestNarrate_ReliefGone(t *testing.T) {
	wantNarrative(t, outcome(t, "relief.gone", nil, map[string]any{"gone": "pod:a"}),
		"relief-x-cpu.limit-1.9 [relief.gone] at 2026-01-01T12:00:00Z: "+actSentence+"; the workload left; there was nothing to give back")
}

func TestNarrate_ReliefStuck(t *testing.T) {
	wantNarrative(t, outcome(t, "relief.stuck", map[string]any{"while": "reverting", "error": "admission webhook denied the request"},
		map[string]any{"stuck": "pod:a"}),
		"relief-x-cpu.limit-1.9 [relief.stuck] at 2026-01-01T12:00:00Z: "+actSentence+"; giving the change back failed while reverting: admission webhook denied the request")
}

func TestNarrate_ReliefNotApplied(t *testing.T) {
	m := narrateMap(t)
	// The act is not held: the entry's subject names the change.
	d := decide(t, m, "relief-not_applied-cpu.limit-1.5", "relief.not_applied", "did the change to pod:a's cpu through cpu.limit take?",
		[]string{actID}, map[string]any{"entry": map[string]any{"subject": "pod:a", "decision_id": actID}},
		map[string]any{"not_applied": "pod:a"})
	wantNarrative(t, Narrate(d, []statemap.ChainLink{{ID: actID, Evicted: true}}),
		"relief-not_applied-cpu.limit-1.5 [relief.not_applied] at 2026-01-01T12:00:00Z: the change to pod:a (act relief-act-cpu.limit-1.2 no longer held); the change never took effect: pod:a still read as before once settle had passed",
		"  rests on relief-act-cpu.limit-1.2: no longer held")
}

func TestNarrate_ReliefRefusedWithAPlan(t *testing.T) {
	m := narrateMap(t)
	d := decide(t, m, "relief-refused-cpu.limit-1.6", "relief.refused", "cap pod:a's cpu through cpu.limit to relieve cpu_contention_ratio?",
		[]string{estID}, map[string]any{"plan": actPlan(), "error": "conflict"}, map[string]any{"refused": "actuator: conflict"})
	wantNarrative(t, Narrate(d, held(estimate())),
		"relief-refused-cpu.limit-1.6 [relief.refused] at 2026-01-01T12:00:00Z: refused to cap pod:a (to 0.5 cores): actuator: conflict",
		estLine)
}

func TestNarrate_ReliefRefusedWithoutAPlan(t *testing.T) {
	m := narrateMap(t)
	d := decide(t, m, "relief-refused-cpu.limit-1.7", "relief.refused", "relieve cpu_contention_ratio through cpu.limit?",
		nil, map[string]any{"actions_last_hour": 4.0}, map[string]any{"refused": "max_actions_per_hour 4 reached"})
	wantNarrative(t, Narrate(d, nil),
		"relief-refused-cpu.limit-1.7 [relief.refused] at 2026-01-01T12:00:00Z: refused to relieve: max_actions_per_hour 4 reached")
}

func TestNarrate_Admit(t *testing.T) {
	m := narrateMap(t)
	d := decide(t, m, "admit-1.1", "admit", "admit pod:web declaring 0.5 CPU cores and 268435456 bytes of memory for node-b", nil,
		map[string]any{"request": map[string]any{"from": "node-b", "workload": "pod:web", "cpu_cores": 0.5, "memory_bytes": 268435456.0}},
		map[string]any{"accepted": true, "reason": "fits", "basis": "declared", "cpu_after": 0.42, "memory_after": 0.37})
	wantNarrative(t, Narrate(d, nil),
		"admit-1.1 [admit] at 2026-01-01T12:00:00Z: accepted pod:web declaring 0.5 cores and 2.68e+08 bytes for node-b: fits (basis declared, cpu after 0.42, memory after 0.37)")
}

func TestNarrate_Admission(t *testing.T) {
	m := narrateMap(t)
	mp, up := 0.12, 0.08
	d := decide(t, m, "admit-1.2", "admission", "admit web?", nil,
		map[string]any{"offer": admission.Offer{Name: "web"}, "domains": []admission.Line{
			{Domain: "cpu_contention_ratio", Basis: "map", Pressure: 0.05, Share: 0.1, MapPred: &mp, UsagePred: &up, Exit: 0.1},
			{Domain: "memory_contention_ratio", Basis: "usage", Pressure: 0.01, Share: 0.02, UsagePred: &up, Exit: 0.03, Pass: false},
		}},
		map[string]any{"verdict": "decline", "reason": "cpu_contention_ratio would reach 0.12"})
	wantNarrative(t, Narrate(d, nil),
		"admit-1.2 [admission] at 2026-01-01T12:00:00Z: declined web: cpu_contention_ratio would reach 0.12; cpu_contention_ratio fails: pressure 0.05 + share 0.1 → 0.12 (map) against exit 0.1; memory_contention_ratio fails: pressure 0.01 + share 0.02 → 0.08 (usage) against exit 0.03")
}

func TestNarrate_AdmissionOutcome(t *testing.T) {
	m := narrateMap(t)
	obs, mr := 0.24, 0.07
	d := decide(t, m, "admit-1.3", "admission.outcome", "outcome of web?", []string{"admit-1.2"},
		map[string]any{"name": "web", "lines": []admission.OutcomeLine{
			{Domain: "cpu_contention_ratio", ObservedRise: &obs, MapRise: &mr, Breached: true}}},
		map[string]any{"outcome": "breached"})
	wantNarrative(t, Narrate(d, []statemap.ChainLink{{ID: "admit-1.2", Evicted: true}}),
		"admit-1.3 [admission.outcome] at 2026-01-01T12:00:00Z: web: breached; cpu_contention_ratio rose 0.24 against map 0.07 / usage ?, breached",
		"  rests on admit-1.2: no longer held")
}

func TestNarrate_Review(t *testing.T) {
	m := narrateMap(t)
	d := decide(t, m, "review-1.1-ab12cd34", "review", "review candidate a<->b", nil,
		map[string]any{"candidate": types.CandidateEdge{CandidateID: "a<->b"}},
		map[string]any{"action": "confirm", "applied": true, "tier": 1, "by": "rules", "level": "full", "model": "small"})
	wantNarrative(t, Narrate(d, nil),
		"review-1.1-ab12cd34 [review] at 2026-01-01T12:00:00Z: reviewed candidate a<->b: confirm by rules at tier 1 (full), applied, model small")
}

func TestNarrate_Cost(t *testing.T) {
	m := narrateMap(t)
	d := decide(t, m, "cost-1.1-infer-self", "cost", "cost of infer on self", nil, nil,
		map[string]any{"task_type": "infer", "node": "self", "resource_cost": 0.4, "latency_estimate": 0.2,
			"resource_sensitivity": 0.15, "pressure_sensitivity": -0.05, "confidence": 0.8})
	wantNarrative(t, Narrate(d, nil),
		"cost-1.1-infer-self [cost] at 2026-01-01T12:00:00Z: cost of infer on self: resource 0.4, latency 0.2, sensitivities 0.15 / -0.05, confidence 0.8")
}

func TestNarrate_EstimateUnderAnAssumption(t *testing.T) {
	e := estimate()
	e.Assumptions = map[string]float64{"usage@pod:a": 0.1}
	e.Answer["hypothetical"] = map[string]any{"projected_level": 0.19, "delta": -0.12}
	e.Caveats = []string{"one", "two"}
	wantNarrative(t, Narrate(e, nil),
		"est-1.1-cpu_contention_ratio [estimate] at 2026-01-01T12:00:00Z: estimated cpu_contention_ratio at 0.31 (confidence 0.9, active); sensitivity 0.25 [0.2, 0.3] from 1 influences, contributing 0.1; under usage@pod:a=0.1: projected 0.19; 2 caveats")
}

func TestNarrate_UnknownKind(t *testing.T) {
	m := narrateMap(t)
	d := decide(t, m, "x-1.1", "x", "what is x?", nil, map[string]any{"b": 1.0}, map[string]any{"z": "last", "a": 0.5})
	wantNarrative(t, Narrate(d, nil),
		"x-1.1 [x] at 2026-01-01T12:00:00Z: decided what is x?: a=0.5, z=last; inputs: b, pace")
	d.Kind = ""
	wantNarrative(t, Narrate(d, nil),
		"x-1.1 [?] at 2026-01-01T12:00:00Z: decided what is x?: a=0.5, z=last; inputs: b, pace")
}

// restored is d as a snapshot hands it back: encoded and decoded, as Load does.
func restored(t *testing.T, d statemap.Decision) statemap.Decision {
	t.Helper()
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var out statemap.Decision
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestNarrateReadsSnapshotRestoredInputsTheSame(t *testing.T) {
	m := narrateMap(t)
	a := act(t, m, actPlan())
	v := verify(t, m, 0.05, "keep")
	want := Narrate(v, held(a, estimate()))
	if got := Narrate(restored(t, v), held(restored(t, a), restored(t, estimate()))); got != want {
		t.Fatalf("restored narrative\n%s\nwant\n%s", got, want)
	}

	// A typed relief.Plan, as the producer records it, reads as its map form.
	typed := act(t, narrateMap(t), relief.Plan{Subject: "pod:a", From: 1, To: 0.5, Unit: "cores", Predicted: 0.08,
		EstimateID: estID, Basis: relief.BasisMap})
	if got := Narrate(v, held(typed, estimate())); got != want {
		t.Fatalf("typed plan narrative\n%s\nwant\n%s", got, want)
	}
}

func TestNarrateUsesThreeSignificantDigits(t *testing.T) {
	m := narrateMap(t)
	d := decide(t, m, "relief-refused-cpu.limit-1.8", "relief.refused", "cap?", nil,
		map[string]any{"plan": map[string]any{"subject": "pod:a", "to": 1234.5, "unit": "millicores"}},
		map[string]any{"refused": "no"})
	if got := Narrate(d, nil); !strings.HasSuffix(got, "refused to cap pod:a (to 1.23e+03 millicores): no") {
		t.Fatalf("narrative %q; want 1234.5 as 1.23e+03", got)
	}
	v := verify(t, m, 0.048123, "keep")
	if got := Narrate(v, nil); !strings.HasSuffix(got, "; observed 0.0481; kept") {
		t.Fatalf("narrative %q; want 0.048123 as 0.0481", got)
	}
}
