package relief

import (
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/actuators"
	"github.com/DiyazY/di-agent/pkg/runtime"
	"github.com/DiyazY/di-agent/pkg/spec"
	"github.com/DiyazY/di-agent/pkg/statemap"
	"github.com/DiyazY/di-agent/pkg/types"
)

var errTest = errors.New("candidate store unavailable")

// fakeCandidates is a discovery queue the loop can settle candidates in.
type fakeCandidates struct {
	mu        sync.Mutex
	pending   []*types.CandidateEdge
	confirmed []confirmation
	err       error
}

type confirmation struct {
	id, from string
	rv       types.CandidateReview
}

func (f *fakeCandidates) PendingCandidates() ([]*types.CandidateEdge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*types.CandidateEdge(nil), f.pending...), nil
}

func (f *fakeCandidates) ConfirmCandidate(id, from string, rv types.CandidateReview) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.confirmed = append(f.confirmed, confirmation{id, from, rv})
	return nil
}

func (f *fakeCandidates) calls() []confirmation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]confirmation(nil), f.confirmed...)
}

// lastVerify is the newest verify decision.
func (h *harness) lastVerify() *statemap.Decision {
	h.t.Helper()
	ds := h.decisions("verify")
	if len(ds) == 0 {
		h.t.Fatal("no verify decision")
	}
	return ds[0] // newest first
}

// judged acts on subject at pressure 0.5 and has the next two minutes read
// pressure p, so the action is judged against p.
func (h *harness) judged(subject string, p float64) {
	h.t.Helper()
	h.act(subject)
	h.run(2*time.Minute, p)
	if a := h.action(subject); a.Outcome == "" {
		h.t.Fatalf("action %+v not judged after settle", a)
	}
}

// secondStep has pod:a's usage reflect its cap and waits for the loop to step it
// again, at pressure p.
func (h *harness) secondStep(subject string, share, p float64) {
	h.t.Helper()
	record(h.t, h.m, usage(subject), subject, share, h.now)
	n := len(h.decisions("act"))
	h.until(3*time.Minute, p, "a further step on "+subject, func() bool { return len(h.decisions("act")) > n })
}

func interventionsOn(t *testing.T, m *statemap.Map, subject string) *statemap.Interventions {
	t.Helper()
	for _, r := range m.Relationships(usage(subject), contention) {
		if r.Interventions != nil {
			return r.Interventions
		}
	}
	return nil
}

func TestOtherWorkloadsMovingConfoundTheOutcome(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4, "pod:b": 0.1}, evidence: true})
	h.act("pod:a")
	if at := h.l.Status().InFlight.SharesAtAct; !near(at["pod:a"], 0.4) || !near(at["pod:b"], 0.1) {
		t.Fatalf("shares at act %v; want every workload's share snapshotted", at)
	}
	record(t, h.m, usage("pod:b"), "pod:b", 0.2, h.now)
	h.run(2*time.Minute, 0.44)
	if a := h.action("pod:a"); a.Outcome != OutcomeKept {
		t.Fatalf("action %+v; want kept", a)
	}
	d := h.lastVerify()
	c, _ := d.Inputs["confounded"].(map[string]any)
	if c == nil || c["confounded"] != true {
		t.Fatalf("confounded input %v; want true", d.Inputs["confounded"])
	}
	if moved, _ := c["moved"].([]string); len(moved) != 1 || moved[0] != "pod:b" {
		t.Errorf("moved %v; want [pod:b]", c["moved"])
	}
	if got := h.l.effects.Of("pod:a", BasisMap); got != 1 {
		t.Errorf("effect %v; a confounded outcome must not teach the prediction", got)
	}
	if iv := interventionsOn(t, h.m, "pod:a"); iv == nil || iv.Confounded != 1 || iv.Agreed != 0 {
		t.Errorf("interventions %+v; want one confounded outcome recorded", iv)
	}
}

func TestSmallPodsDoNotConfound(t *testing.T) {
	m := newMap(t, func() time.Time { return t0 })
	record(t, m, usage("pod:a"), "pod:a", 0.3, t0)
	record(t, m, usage("pod:c"), "pod:c", 0.02, t0)
	at := map[string]float64{"pod:a": 0.4, "pod:c": 0.01}
	if moved, _, _ := movedSince(m, nodeUsage, at, "pod:a", 0.25); len(moved) != 0 {
		t.Fatalf("moved %v; a pod going 0.01 → 0.02 of the node is not a confound", moved)
	}
	if confounded(m, nodeUsage, at, "pod:a", 0.25, true) {
		t.Fatal("confounded; want not")
	}
	// pod:a back at its share at act, so pod:c's rise is not CPU the cap freed.
	record(t, m, usage("pod:a"), "pod:a", 0.4, t0)
	record(t, m, usage("pod:c"), "pod:c", 0.04, t0)
	if !confounded(m, nodeUsage, at, "pod:a", 0.25, true) {
		t.Fatal("pod:c at 0.04 from 0.01 moved 0.03: want confounded")
	}
	// A workload that arrived since the act moved from nothing.
	record(t, m, usage("pod:c"), "pod:c", 0.01, t0)
	record(t, m, usage("pod:d"), "pod:d", 0.1, t0)
	if moved, _, _ := movedSince(m, nodeUsage, at, "pod:a", 0.25); len(moved) != 1 || moved[0] != "pod:d" {
		t.Fatalf("moved %v; want the newcomer pod:d", moved)
	}
}

func TestThePredictionLearnsTheWorkload(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4}, evidence: true})
	h.judged("pod:a", 0.44) // predicted 0.1, observed 0.06
	want := 0.7*1 + 0.3*0.6
	if got := h.l.effects.Of("pod:a", BasisMap); !near(got, want) {
		t.Fatalf("effect %v after observing 0.6 of the prediction; want %v", got, want)
	}
	h.secondStep("pod:a", 0.3, 0.44)
	// 0.3 → 0.225 of the node, strength 1: the map predicts 0.075, scaled by what
	// acting on pod:a has done before.
	p := h.l.Status().InFlight.Plan
	if !near(p.Predicted, 0.075*want) || !near(p.Effect, want) {
		t.Fatalf("plan %+v; want the map's 0.075 scaled by %v", p, want)
	}
	h.run(2*time.Minute, 0.395) // observed 0.045: 0.6 of the unscaled prediction
	if a := h.action("pod:a"); a.Outcome != OutcomeKept {
		t.Fatalf("action %+v; want kept", a)
	}
	want = 0.7*want + 0.3*0.6
	if got := h.l.effects.Of("pod:a", BasisMap); !near(got, want) {
		t.Fatalf("effect %v; want %v, learned against the unscaled prediction", got, want)
	}
	if h.l.effects.Of("pod:other", BasisMap) != 1 {
		t.Error("an unknown workload's effect is not 1")
	}
}

func TestEffectsAreClamped(t *testing.T) {
	var e effects
	for range 20 {
		e.Observe("pod:a", BasisMap, 10)
		e.Observe("pod:b", BasisMap, -3)
	}
	if a, b := e.Of("pod:a", BasisMap), e.Of("pod:b", BasisMap); a != 2 || b != 0.1 {
		t.Fatalf("effects %v, %v; want clamped to 2 and 0.1", a, b)
	}
	e.Observe("pod:c", BasisMap, math.NaN())
	if c := e.Of("pod:c", BasisMap); c != 1 {
		t.Fatalf("effect %v after a NaN ratio; want it ignored", c)
	}
}

// settleFixture: pod:a's usage relates to nothing, so its steps are predicted
// from its share, and two that hold create the relationship.
func settleFixture(t *testing.T, cands *fakeCandidates, evidence bool) *harness {
	return newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4}, noRelate: true,
		evidence: evidence, candidates: cands})
}

// twoAgreeing: two steps on pod:a, each observed to drop pressure well beyond
// half its usage-based prediction.
func (h *harness) twoAgreeing() {
	h.t.Helper()
	h.judged("pod:a", 0.44)
	h.secondStep("pod:a", 0.3, 0.44)
	h.run(2*time.Minute, 0.40)
	if a := h.action("pod:a"); a.Outcome != OutcomeKept {
		h.t.Fatalf("second step %+v; want kept", a)
	}
}

func TestAnInterventionSettlesThePendingCandidate(t *testing.T) {
	cands := &fakeCandidates{pending: []*types.CandidateEdge{
		{CandidateID: "other", FromID: usage("pod:b"), ToID: contention},
		{CandidateID: "c1", FromID: usage("pod:a"), ToID: contention},
	}}
	h := settleFixture(t, cands, true)
	h.judged("pod:a", 0.44)
	if n := len(cands.calls()); n != 0 {
		t.Fatalf("%d confirmations after one agreeing outcome; want none", n)
	}
	h.secondStep("pod:a", 0.3, 0.44)
	h.run(2*time.Minute, 0.40)
	calls := cands.calls()
	if len(calls) != 1 {
		t.Fatalf("confirmations %+v; want one", calls)
	}
	c := calls[0]
	verify := h.lastVerify()
	if c.id != "c1" || c.from != usage("pod:a") || c.rv.By != types.ReviewerIntervention ||
		c.rv.Action != types.ReviewConfirm || c.rv.Tier != 0 || c.rv.DecisionID != verify.ID || !c.rv.At.Equal(h.now) {
		t.Fatalf("confirmation %+v; want c1 from pod:a's usage by intervention, citing %s", c, verify.ID)
	}
	iv, _ := verify.Inputs["intervention"].(map[string]any)
	if iv == nil || iv["changed"] != "created" || iv["relationship"] != statemap.RelationshipID(usage("pod:a"), contention, "") {
		t.Fatalf("intervention input %v; want the created relationship", verify.Inputs["intervention"])
	}
	if verify.Inputs["candidate"] != "c1" {
		t.Errorf("candidate input %v; want c1", verify.Inputs["candidate"])
	}
	r, ok := h.m.Relationship(statemap.RelationshipID(usage("pod:a"), contention, ""))
	if !ok || r.Provenance != statemap.Intervened {
		t.Fatalf("relationship %+v; want it created with intervened provenance", r)
	}
}

func TestAnUndirectedCandidateIsSettledFromTheWorkload(t *testing.T) {
	cands := &fakeCandidates{pending: []*types.CandidateEdge{
		{CandidateID: "u1", FromID: contention, ToID: usage("pod:a"), Undirected: true},
	}}
	h := settleFixture(t, cands, true)
	h.twoAgreeing()
	calls := cands.calls()
	if len(calls) != 1 || calls[0].id != "u1" || calls[0].from != usage("pod:a") {
		t.Fatalf("confirmations %+v; want u1 oriented from pod:a's usage", calls)
	}
}

func TestAReversedDirectedCandidateIsNotSettled(t *testing.T) {
	cands := &fakeCandidates{pending: []*types.CandidateEdge{
		{CandidateID: "rev", FromID: contention, ToID: usage("pod:a")},
	}}
	h := settleFixture(t, cands, true)
	h.twoAgreeing()
	if calls := cands.calls(); len(calls) != 0 {
		t.Fatalf("confirmations %+v; a candidate claiming pressure drives the workload is not what was shown", calls)
	}
}

func TestAFailedSettlementIsACaveat(t *testing.T) {
	cands := &fakeCandidates{err: errTest, pending: []*types.CandidateEdge{
		{CandidateID: "c1", FromID: usage("pod:a"), ToID: contention},
	}}
	h := settleFixture(t, cands, true)
	h.twoAgreeing()
	d := h.lastVerify()
	found := false
	for _, c := range d.Caveats {
		if c == "settling candidate c1: "+errTest.Error() {
			found = true
		}
	}
	if !found {
		t.Fatalf("caveats %v; want the failed settlement stated", d.Caveats)
	}
	if _, ok := h.m.Relationship(statemap.RelationshipID(usage("pod:a"), contention, "")); !ok {
		t.Fatal("the relationship the interventions created is missing")
	}
}

func TestAConfoundedOutcomeTeachesNothing(t *testing.T) {
	cands := &fakeCandidates{pending: []*types.CandidateEdge{
		{CandidateID: "c1", FromID: usage("pod:a"), ToID: contention},
	}}
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4, "pod:b": 0.1}, noRelate: true,
		evidence: true, candidates: cands})
	h.act("pod:a")
	record(t, h.m, usage("pod:b"), "pod:b", 0.2, h.now)
	h.run(2*time.Minute, 0.44)
	h.secondStep("pod:a", 0.3, 0.44)
	record(t, h.m, usage("pod:b"), "pod:b", 0.3, h.now)
	h.run(2*time.Minute, 0.40)
	if a := h.action("pod:a"); a.Outcome != OutcomeKept {
		t.Fatalf("second step %+v; want kept", a)
	}
	if got := h.l.effects.Of("pod:a", BasisUsage); got != 1 {
		t.Errorf("effect %v; want 1", got)
	}
	if rs := h.m.Relationships(usage("pod:a"), contention); len(rs) != 0 {
		t.Errorf("relationships %+v; confounded outcomes must not create one", rs)
	}
	if calls := cands.calls(); len(calls) != 0 {
		t.Errorf("confirmations %+v; want none", calls)
	}
	iv, _ := h.lastVerify().Inputs["intervention"].(map[string]any)
	if iv == nil || iv["changed"] != "" {
		t.Errorf("intervention input %v; want recorded with no change", h.lastVerify().Inputs["intervention"])
	}
}

func TestDryRunTeachesNothing(t *testing.T) {
	cands := &fakeCandidates{pending: []*types.CandidateEdge{
		{CandidateID: "c1", FromID: usage("pod:a"), ToID: contention},
	}}
	h := settleFixture(t, cands, false)
	h.twoAgreeing()
	if rs := h.m.Relationships(usage("pod:a"), contention); len(rs) != 0 {
		t.Errorf("relationships %+v; a dry run changes nothing, so it shows nothing", rs)
	}
	if got := h.l.effects.Of("pod:a", BasisUsage); got != 1 {
		t.Errorf("effect %v; want 1", got)
	}
	if calls := cands.calls(); len(calls) != 0 {
		t.Errorf("confirmations %+v; want none", calls)
	}
	d := h.lastVerify()
	ev, _ := d.Inputs["evidence"].(map[string]any)
	if ev == nil || ev["fed"] != false || ev["why"] == "" {
		t.Fatalf("evidence input %v; want not fed, with why", d.Inputs["evidence"])
	}
	if _, ok := d.Inputs["intervention"]; ok {
		t.Error("an intervention was recorded for a dry run")
	}
}

func TestAResumedActionWithoutAPredictionTeachesNothing(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4}, evidence: true})
	record(t, h.m, contention, "", 0.5, h.now)
	h.seed(actuators.Entry{NodeID: "n1", Subject: "pod:a", Action: cpu.Action,
		Applied: 1.2, Expires: h.now.Add(30 * time.Minute), DecisionID: "relief-act-1-1"})
	if err := h.l.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if f := h.l.Status().InFlight; f == nil || f.SharesAtAct == nil {
		t.Fatalf("in flight %+v; want shares snapshotted at its baseline", f)
	}
	h.run(2*time.Minute, 0.44)
	if a := h.action("pod:a"); a.Outcome != OutcomeKept {
		t.Fatalf("action %+v; want kept", a)
	}
	if iv := interventionsOn(t, h.m, "pod:a"); iv != nil {
		t.Errorf("interventions %+v; a resumed action with no prediction is no evidence", iv)
	}
	if got := h.l.effects.Of("pod:a", BasisMap); got != 1 {
		t.Errorf("effect %v; want 1", got)
	}
	ev, _ := h.lastVerify().Inputs["evidence"].(map[string]any)
	if ev == nil || ev["fed"] != false {
		t.Fatalf("evidence input %v; want not fed", h.lastVerify().Inputs["evidence"])
	}
}

func TestAResumedActionWithAPredictionTeachesNothing(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4}, evidence: true})
	resumeWithPrediction(h)
	h.step(0.5)
	// Flat: the baseline was taken with the cap in force, so pressure has no drop
	// left to show, and a good cap must not be reverted for it.
	d := h.firstVerify(2*time.Minute, 0.5)
	if d.Answer["verdict"] != "keep" {
		t.Fatalf("verify %v; want the resumed change kept", d.Answer)
	}
	if iv := interventionsOn(t, h.m, "pod:a"); iv != nil {
		t.Fatalf("interventions %+v; a resumed action's baseline was taken with its cap in force", iv)
	}
	if got := h.l.effects.Of("pod:a", BasisMap); got != 1 {
		t.Errorf("effect %v; want 1", got)
	}
	ev, _ := d.Inputs["evidence"].(map[string]any)
	if ev == nil || ev["fed"] != false || !strings.HasPrefix(ev["why"].(string), "resumed") {
		t.Fatalf("evidence input %v; want not fed, as resumed", d.Inputs["evidence"])
	}
}

// TestAResumedActionIsKeptWhenPressureIsFlat: a change taken up after a restart
// has no pre-cap baseline, as pressure was first read with the cap in force. It
// is kept whatever pressure does, with a caveat saying why, and still expires.
func TestAResumedActionIsKeptWhenPressureIsFlat(t *testing.T) {
	// One action an hour, and the resumed change counts toward it: nothing steps
	// pod:a again, so the change seen at the end is the resumed one.
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4}, evidence: true,
		policy: func(ps *spec.PolicySpec) { ps.Defaults.MaxActionsPerHour = 1 }})
	resumeWithPrediction(h)
	h.step(0.5)
	d := h.firstVerify(2*time.Minute, 0.5)
	if d.Answer["verdict"] != "keep" {
		t.Fatalf("verify %v; want kept, as there is no pre-cap baseline", d.Answer)
	}
	if !strings.Contains(strings.Join(d.Caveats, " "), "resumed") {
		t.Errorf("caveats %v; want one saying a resumed change is kept unjudged", d.Caveats)
	}
	if a := h.action("pod:a"); a.Outcome != OutcomeKept {
		t.Fatalf("action %+v; want kept", a)
	}
	if u, ok := h.applied()["pod:a"]; !ok || u.DecisionID != "relief-act-1-1" {
		t.Fatalf("applied %v; want the resumed change still in force", h.applied())
	}
	if st := h.l.Status(); st.Reverted != 0 || st.Kept != 0 || !st.Cooling["pod:a"].IsZero() {
		t.Fatalf("status %+v; want nothing judged, as a forced keep is no prediction that held, and no failed-cap cooldown", st)
	}
	h.until(time.Hour, 0.5, "the resumed change expiring", func() bool { return h.action("pod:a").Outcome == OutcomeExpired })
}

func TestAFurtherStepAfterResumeTeaches(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4}, evidence: true})
	resumeWithPrediction(h)
	h.step(0.5)
	h.firstVerify(2*time.Minute, 0.44)
	// The resumed cap holds pod:a at 1.2 cores, 0.3 of the node: a further step
	// goes below it, to 0.9.
	record(t, h.m, usage("pod:a"), "pod:a", 0.3, h.now)
	h.until(3*time.Minute, 0.44, "a further step", func() bool { return len(h.decisions("act")) > 1 }) // the journalled act, then this loop's
	if a := h.action("pod:a"); a.Resumed || a.Outcome != "" || !near(a.Plan.To, 0.9) {
		t.Fatalf("action %+v; want the further step to 0.9 in flight, no longer resumed", a)
	}
	h.run(2*time.Minute, 0.38) // predicted 0.075 from 0.44, observed 0.06
	if iv := interventionsOn(t, h.m, "pod:a"); iv == nil || iv.Agreed != 1 {
		t.Fatalf("interventions %+v; want the further step's agreed outcome", iv)
	}
}

// firstVerify steps at pressure p until the loop's first verify decision, and
// returns it, failing after limit.
func (h *harness) firstVerify(limit time.Duration, p float64) *statemap.Decision {
	h.t.Helper()
	h.until(limit, p, "a verify", func() bool { return len(h.decisions("verify")) > 0 })
	ds := h.decisions("verify")
	return ds[len(ds)-1]
}

// resumeWithPrediction puts a change on pod:a whose act the journal holds, for
// the loop to take up at its first tick.
func resumeWithPrediction(h *harness) {
	b := h.m.Decide("relief-act-1-1", "earlier act")
	b.Input("plan", Plan{Subject: "pod:a", Predicted: 0.1, From: 1.6, To: 1.2, Basis: BasisMap})
	b.Commit(nil)
	h.seed(actuators.Entry{NodeID: "n1", Subject: "pod:a", Action: cpu.Action,
		Applied: 1.2, Expires: h.now.Add(30 * time.Minute), DecisionID: "relief-act-1-1"})
}

// capBinds acts on pod:a (capped at 1.2 cores), has it use share of the node by
// verify, and returns the verify decision.
func capBinds(t *testing.T, share float64) (*harness, *statemap.Decision) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4}, evidence: true})
	h.act("pod:a")
	record(t, h.m, usage("pod:a"), "pod:a", share, h.now)
	d := h.firstVerify(2*time.Minute, 0.44)
	if d.Answer["verdict"] != "keep" {
		t.Fatalf("verify %v; the verdict is the cap's own, whatever it teaches", d.Answer)
	}
	return h, d
}

func TestACapThatDidNotBindTeachesNothing(t *testing.T) {
	h, d := capBinds(t, 0.26) // 1.04 cores, under 0.9 × 1.2 = 1.08
	ev, _ := d.Inputs["evidence"].(map[string]any)
	if ev == nil || ev["fed"] != false || ev["why"] != "the cap did not bind: usage 1.040 cores under the 1.200-core limit" {
		t.Fatalf("evidence input %v; want not fed, the cap not binding", d.Inputs["evidence"])
	}
	if iv := interventionsOn(t, h.m, "pod:a"); iv != nil {
		t.Errorf("interventions %+v; want none", iv)
	}
	if got := h.l.effects.Of("pod:a", BasisMap); got != 1 {
		t.Errorf("effect %v; want 1", got)
	}
	ss, _ := d.Inputs["subject_share"].(map[string]any)
	if ss == nil || !near(ss["at_act"].(float64), 0.4) || !near(ss["at_verify"].(float64), 0.26) {
		t.Errorf("subject_share %v; want 0.4 at act, 0.26 at verify", d.Inputs["subject_share"])
	}
}

func TestACapThatBoundTeaches(t *testing.T) {
	h, d := capBinds(t, 0.28) // 1.12 cores, at least 0.9 × 1.2
	if ev, _ := d.Inputs["evidence"].(map[string]any); ev == nil || ev["fed"] != true {
		t.Fatalf("evidence input %v; want fed", d.Inputs["evidence"])
	}
	if iv := interventionsOn(t, h.m, "pod:a"); iv == nil || iv.Agreed != 1 {
		t.Fatalf("interventions %+v; want one agreed", iv)
	}
}

func TestAWorkloadThatLeftConfounds(t *testing.T) {
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4, "pod:b": 0.1, "pod:c": 0.01}, evidence: true})
	h.act("pod:a")
	for _, s := range []string{"pod:b", "pod:c"} {
		if err := h.m.RetireProperty(usage(s), "the pod was deleted", "test"); err != nil {
			t.Fatal(err)
		}
	}
	h.run(2*time.Minute, 0.44)
	c, _ := h.lastVerify().Inputs["confounded"].(map[string]any)
	if c == nil || c["confounded"] != true {
		t.Fatalf("confounded input %v; want true", h.lastVerify().Inputs["confounded"])
	}
	if left, _ := c["left"].([]string); len(left) != 1 || left[0] != "pod:b" {
		t.Fatalf("left %v; want pod:b, and not pod:c, whose 0.01 is noise", c["left"])
	}
	if got := h.l.effects.Of("pod:a", BasisMap); got != 1 {
		t.Errorf("effect %v; want 1", got)
	}
}

func TestAUsageEffectDoesNotScaleAMapPrediction(t *testing.T) {
	var e effects
	for range 10 {
		e.Observe("pod:a", BasisUsage, 0.2)
	}
	m := planFixture(t, 1)
	ws := []runtime.Workload{pod("pod:a")}
	pol, c := defaultPolicy(t, nil), reliefCfg()
	p, ok, why := MakePlan(ctx, m, limiter, Attribute(m, limiter, ws, pol, c), ws, pol, c, e.Of)
	if !ok || p.Basis != BasisMap || !near(p.Predicted, 0.1) || p.Effect != 1 {
		t.Fatalf("plan %+v ok=%v why=%q; want the map's 0.1, unscaled by the usage factor", p, ok, why)
	}
	e.Observe("pod:a", BasisMap, 0.5)
	if p, _, _ = MakePlan(ctx, m, limiter, Attribute(m, limiter, ws, pol, c), ws, pol, c, e.Of); !near(p.Predicted, 0.1*0.85) {
		t.Fatalf("plan %+v; want the map factor 0.85 applied", p)
	}
}

// TestAVictimTakingUpTheFreedCPUDoesNotConfound: capping a workload frees CPU, and a
// contended workload beside it takes it up — that is the relief, not another workload
// moving. Rises up to what the capped workload gave up are not confounds; a rise
// beyond it, a fall, an arrival and a departure still are.
func TestAVictimTakingUpTheFreedCPUDoesNotConfound(t *testing.T) {
	m := newMap(t, func() time.Time { return t0 })
	at := map[string]float64{"pod:hog": 0.5, "pod:victim": 0.38}
	record(t, m, usage("pod:hog"), "pod:hog", 0.38, t0)
	record(t, m, usage("pod:victim"), "pod:victim", 0.5, t0)
	if moved, left, absorbed := movedSince(m, nodeUsage, at, "pod:hog", 0.25); len(moved)+len(left) != 0 || len(absorbed) != 1 {
		t.Fatalf("moved %v left %v absorbed %v; the victim took up the 0.12 the hog gave up", moved, left, absorbed)
	}
	// The victim rose by more than the hog gave up: something besides the cap moved it.
	record(t, m, usage("pod:victim"), "pod:victim", 0.7, t0)
	if moved, _, _ := movedSince(m, nodeUsage, at, "pod:hog", 0.25); len(moved) != 1 || moved[0] != "pod:victim" {
		t.Fatalf("moved %v; a rise of 0.32 against 0.12 freed is a confound", moved)
	}
	// A workload that fell lowered contention on its own.
	record(t, m, usage("pod:victim"), "pod:victim", 0.2, t0)
	if moved, _, _ := movedSince(m, nodeUsage, at, "pod:hog", 0.25); len(moved) != 1 || moved[0] != "pod:victim" {
		t.Fatalf("moved %v; a fall is a confound whatever the hog gave up", moved)
	}
}

// TestAnAbsorbedRiseConfoundsARevert: a rise passed over as the victim taking up freed
// CPU is only the relief working when the outcome agreed. When contention did not drop,
// the same rise may be a workload growing on its own and hiding the cap's effect, so a
// revert with an absorbed rise beside it is confounded rather than a null that teaches.
func TestAnAbsorbedRiseConfoundsARevert(t *testing.T) {
	absorbed := []string{"pod:victim"}
	if c := confoundedBy(nil, nil, absorbed, true); len(c) != 0 {
		t.Fatalf("confounded by %v; an agreeing outcome with the freed CPU taken up is the relief", c)
	}
	if c := confoundedBy(nil, nil, absorbed, false); len(c) != 1 || c[0] != "pod:victim" {
		t.Fatalf("confounded by %v; a revert beside an absorbed rise must not teach", c)
	}
	if c := confoundedBy([]string{"pod:x"}, []string{"pod:y"}, nil, true); len(c) != 2 {
		t.Fatalf("confounded by %v; moved and left confound whatever the verdict", c)
	}
}

// TestNothingIsFreedByAWorkloadTheMapNoLongerHolds: with no live share for the capped
// workload, what it gave up is unknown, not its whole share at act, so rises beside it
// are not passed over.
func TestNothingIsFreedByAWorkloadTheMapNoLongerHolds(t *testing.T) {
	m := newMap(t, func() time.Time { return t0 })
	at := map[string]float64{"pod:hog": 0.5, "pod:victim": 0.38}
	record(t, m, usage("pod:victim"), "pod:victim", 0.6, t0)
	if moved, _, absorbed := movedSince(m, nodeUsage, at, "pod:hog", 0.25); len(moved) != 1 || len(absorbed) != 0 {
		t.Fatalf("moved %v absorbed %v; nothing is known to have been freed", moved, absorbed)
	}
}

// TestAnInterventionsEffectIsWhatItMeasured: an unconfounded outcome hands the map the
// drop in contention per unit drop in the workload's share — the units the estimate
// projects in — and the relationship the interventions create reasons with it, not
// with the co-variation of a workload the cap holds still.
func TestAnInterventionsEffectIsWhatItMeasured(t *testing.T) {
	h := settleFixture(t, &fakeCandidates{}, true)
	h.act("pod:a")
	// The cap takes 0.1 of the node off pod:a's usage.
	record(t, h.m, usage("pod:a"), "pod:a", h.action("pod:a").SharesAtAct["pod:a"]-0.1, h.now)
	h.run(2*time.Minute, 0.44)
	d := h.lastVerify()
	ss, _ := d.Inputs["subject_share"].(map[string]any)
	iv, _ := d.Inputs["intervention"].(map[string]any)
	atAct, _ := ss["at_act"].(float64)
	atVerify, _ := ss["at_verify"].(float64)
	observed, _ := d.Inputs["observed"].(float64)
	want := math.Min(1, observed/(atAct-atVerify))
	if got, ok := iv["effect"].(float64); !ok || math.Abs(got-want) > 1e-9 {
		t.Fatalf("intervention input %v; want effect %v = observed %v / share drop %v", iv, want, observed, atAct-atVerify)
	}
}

// TestTheCreatedRelationshipReasonsFromTheMeasuredEffect: the relationship two agreeing
// outcomes create carries what they measured, and the map answers from it.
func TestTheCreatedRelationshipReasonsFromTheMeasuredEffect(t *testing.T) {
	h := settleFixture(t, &fakeCandidates{}, true)
	h.judged("pod:a", 0.44)
	h.secondStep("pod:a", 0.3, 0.44)
	// The second cap binds: pod:a runs at its new limit, 0.9 of 4 cores.
	record(t, h.m, usage("pod:a"), "pod:a", 0.225, h.now)
	h.run(2*time.Minute, 0.40)
	r, ok := h.m.Relationship(statemap.RelationshipID(usage("pod:a"), contention, ""))
	if !ok || r.Provenance != statemap.Intervened || r.Measured == nil || r.Basis() != "measured" {
		t.Fatalf("relationship %+v; want it created and reasoning from the measured effect", r)
	}
	if eff, _ := r.Effective(); eff < 0.03 {
		t.Fatalf("effective %v; the measured effect of a cap that worked is not negligible", eff)
	}
}

// TestNoEffectIsMeasuredWhenTheShareDidNotMove: an outcome whose workload's share barely
// moved has nothing to divide by, so it counts toward structure but measures no effect.
func TestNoEffectIsMeasuredWhenTheShareDidNotMove(t *testing.T) {
	if _, ok := measuredEffect(0.3, 0.5, 0.49); ok {
		t.Fatal("a share drop of 0.01 measured an effect; want none")
	}
	if e, ok := measuredEffect(0.3, 0.5, 0.4); !ok || math.Abs(e-1) > 1e-9 {
		t.Fatalf("effect %v ok=%v; want 0.3/0.1 clamped to 1", e, ok)
	}
	if e, ok := measuredEffect(0.05, 0.5, 0.25); !ok || math.Abs(e-0.2) > 1e-9 {
		t.Fatalf("effect %v ok=%v; want 0.05/0.25", e, ok)
	}
	if e, ok := measuredEffect(-0.1, 0.5, 0.25); !ok || e != 0 {
		t.Fatalf("effect %v ok=%v; a rise in contention is an effect of zero", e, ok)
	}
}

// TestAMeasuredEffectReplacesTheMapPredictionsCorrection: the map-basis factor
// corrects a map prediction for what acting has done against it. Once an outcome
// writes a measured strength, the map's prediction already carries what the
// interventions saw, and a factor learned against the old strength would correct it
// a second time.
func TestAMeasuredEffectReplacesTheMapPredictionsCorrection(t *testing.T) {
	h := settleFixture(t, &fakeCandidates{}, true)
	h.l.effects.Observe("pod:a", BasisMap, 2)
	h.judged("pod:a", 0.44)
	h.secondStep("pod:a", 0.3, 0.44)
	record(t, h.m, usage("pod:a"), "pod:a", 0.225, h.now)
	h.run(2*time.Minute, 0.40)
	if r, ok := h.m.Relationship(statemap.RelationshipID(usage("pod:a"), contention, "")); !ok || r.Measured == nil {
		t.Fatalf("relationship %+v; want a measured strength", r)
	}
	if f := h.l.effects.Of("pod:a", BasisMap); f != 1 {
		t.Fatalf("map-basis factor %v after a measured effect; want 1, nothing left to correct", f)
	}
}
