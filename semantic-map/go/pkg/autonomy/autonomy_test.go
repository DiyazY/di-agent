package autonomy_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/autonomy"
	"github.com/DiyazY/di-agent/pkg/statemap"
	"github.com/DiyazY/di-agent/pkg/types"
)

// fakeMap is a state map with a queue of candidates whose review actions only move
// status and record the review, as the proposer does.
type fakeMap struct {
	st    *statemap.Map
	cands map[string]*types.CandidateEdge
	from  map[string]string // the end each confirmed candidate was confirmed from
}

func newFakeMap(t *testing.T, cands ...*types.CandidateEdge) *fakeMap {
	t.Helper()
	st := statemap.New(statemap.Config{AdmitUnknown: true}, statemap.NewJournal(0))
	m := &fakeMap{st: st, cands: map[string]*types.CandidateEdge{}, from: map[string]string{}}
	for _, c := range cands {
		for _, end := range []string{c.FromID, c.ToID} {
			_, subject, _ := strings.Cut(end, "@")
			if err := st.Record(statemap.Observation{ID: end, Subject: subject, Value: 0.5, At: time.Now()}); err != nil {
				t.Fatal(err)
			}
		}
		m.cands[c.CandidateID] = c
	}
	return m
}

func (m *fakeMap) PendingCandidates() ([]*types.CandidateEdge, error) {
	var out []*types.CandidateEdge
	for _, c := range m.cands {
		if c.Status == types.Pending {
			cp := *c
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (m *fakeMap) CandidateHistory() ([]*types.CandidateEdge, error) {
	var out []*types.CandidateEdge
	for _, c := range m.cands {
		cp := *c
		out = append(out, &cp)
	}
	return out, nil
}

func (m *fakeMap) settle(id string, s types.CandidateStatus, rv types.CandidateReview) error {
	c := m.cands[id]
	rv.Applied = true
	rv.MIScore, rv.NObservations, rv.Direction, rv.Kind = c.MIScore, c.NObservations, c.Direction, c.Kind
	rv.PersistentWindows = c.PersistentWindows
	c.Status, c.Review = s, &rv
	c.Reason = types.ReasonAgent // who settled it, as the proposer records it
	switch {
	case s == types.Confirmed:
		c.Reason = ""
	case rv.By == "" || rv.By == types.ReviewerOperator:
		c.Reason = types.ReasonOperator
	}
	return nil
}

func (m *fakeMap) ConfirmCandidate(id, from string, rv types.CandidateReview) error {
	if c := m.cands[id]; !c.Open() {
		return fmt.Errorf("candidate %q is not open (status=%v, reason=%q)", id, c.Status, c.Reason)
	}
	m.from[id] = from
	return m.settle(id, types.Confirmed, rv)
}
func (m *fakeMap) RejectCandidate(id string, rv types.CandidateReview) error {
	return m.settle(id, types.Rejected, rv)
}
func (m *fakeMap) DeferCandidate(id string, rv types.CandidateReview) error {
	return m.settle(id, types.Deferred, rv)
}
func (m *fakeMap) AnnotateCandidate(id string, rv types.CandidateReview) error {
	c := m.cands[id]
	if !c.Open() {
		return fmt.Errorf("candidate %q is not open (status=%v, reason=%q)", id, c.Status, c.Reason)
	}
	rv.MIScore, rv.NObservations, rv.Direction, rv.Kind = c.MIScore, c.NObservations, c.Direction, c.Kind
	rv.PersistentWindows = c.PersistentWindows
	c.Review = &rv
	return nil
}
func (m *fakeMap) State() *statemap.Map { return m.st }

// fixed is a decision model that answers the same probabilities every time, and
// records what it was asked.
type fixed struct {
	actions, orientation map[string]float64
	analyses             []string
	err                  error
}

func (f *fixed) Name() string { return "fixed" }
func (f *fixed) Decide(_ context.Context, e autonomy.Evidence, analysis string) (*autonomy.Verdict, error) {
	f.analyses = append(f.analyses, analysis)
	if f.err != nil {
		return nil, f.err
	}
	o := map[string]float64{}
	for k, v := range f.orientation {
		o[strings.NewReplacer("FROM", e.Candidate.FromID, "TO", e.Candidate.ToID).Replace(k)] = v
	}
	return &autonomy.Verdict{Model: "fixed", Actions: f.actions, Orientation: o}, nil
}

// sure moves a decision model to sure on its second pass, the one scoring an analysis.
type sure struct{ fixed }

func (s *sure) Decide(ctx context.Context, e autonomy.Evidence, analysis string) (*autonomy.Verdict, error) {
	if analysis != "" {
		s.actions = map[string]float64{types.ReviewConfirm: 0.97, types.ReviewReject: 0.02, types.ReviewDefer: 0.01}
	}
	return s.fixed.Decide(ctx, e, analysis)
}

type analyst struct{ calls int }

func (a *analyst) Name() string { return "analyst" }
func (a *analyst) Reason(context.Context, autonomy.Evidence) (*autonomy.Analysis, error) {
	a.calls++
	return &autonomy.Analysis{Model: "analyst", Text: "the pod's CPU is most of the node's", Suggested: "confirm"}, nil
}

func directed(id string, kind string) *types.CandidateEdge {
	from, to, _ := strings.Cut(id, "->")
	return &types.CandidateEdge{CandidateID: id, FromID: from, ToID: to, MIScore: 0.9, NObservations: 120,
		Kind: kind, Status: types.Pending}
}

func run(t *testing.T, m *fakeMap, cfg autonomy.Config) *autonomy.Reviewer {
	t.Helper()
	cfg.Logf = t.Logf
	if cfg.Rules == nil {
		parts, err := autonomy.Build(m, autonomy.Parts{Rules: []string{"composition"}})
		if err != nil {
			t.Fatal(err)
		}
		cfg.Rules = parts.Rules
	}
	r, err := autonomy.New(m, cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.Cycle(context.Background())
	return r
}

func confident(action string) map[string]float64 {
	p := map[string]float64{types.ReviewConfirm: 0.01, types.ReviewReject: 0.01, types.ReviewDefer: 0.01}
	p[action] = 0.98
	return p
}

func TestTheLevelDecidesWhichTierActs(t *testing.T) {
	const comp, plain = "cpu_utilization@pod:a->cpu_utilization", "cpu_utilization@pod:a->cpu_pressure"
	for _, tc := range []struct {
		level            autonomy.Level
		model            string // what the decision model is sure of
		compApplied      bool
		plainStatus      types.CandidateStatus
		plainApplied     bool
		plainReviewTaken string
	}{
		{autonomy.Suggest, types.ReviewConfirm, false, types.Pending, false, types.ReviewConfirm},
		{autonomy.Rules, types.ReviewConfirm, true, types.Pending, false, types.ReviewConfirm},
		{autonomy.Bounded, types.ReviewConfirm, true, types.Confirmed, true, types.ReviewConfirm},
		{autonomy.Bounded, types.ReviewReject, true, types.Pending, false, types.ReviewReject},
		{autonomy.Full, types.ReviewReject, true, types.Rejected, true, types.ReviewReject},
	} {
		m := newFakeMap(t, directed(comp, types.CandidateKindComposition), directed(plain, ""))
		run(t, m, autonomy.Config{Level: tc.level, Decider: &fixed{actions: confident(tc.model)}})
		c, p := m.cands[comp], m.cands[plain]
		if c.Review == nil || c.Review.By != types.ReviewerRules || c.Review.Applied != tc.compApplied ||
			(c.Status == types.Confirmed) != tc.compApplied {
			t.Errorf("%s: composition %v review %+v; want the rule to confirm, applied=%v", tc.level, c.Status, c.Review, tc.compApplied)
		}
		if p.Status != tc.plainStatus || p.Review == nil || p.Review.By != types.ReviewerDecisionModel ||
			p.Review.Applied != tc.plainApplied || p.Review.Action != tc.plainReviewTaken {
			t.Errorf("%s, model sure of %s: %v review %+v; want %v, %s applied=%v",
				tc.level, tc.model, p.Status, p.Review, tc.plainStatus, tc.plainReviewTaken, tc.plainApplied)
		}
	}
}

func TestAnUnsureModelGoesUpATierAndThenWaitsForEvidence(t *testing.T) {
	const id = "cpu_utilization@pod:a->cpu_pressure"
	unsure := map[string]float64{types.ReviewConfirm: 0.6, types.ReviewReject: 0.3, types.ReviewDefer: 0.1}

	// With no tier 2, it is undecided until its evidence moves.
	m := newFakeMap(t, directed(id, ""))
	run(t, m, autonomy.Config{Level: autonomy.Full, Decider: &fixed{actions: unsure}})
	if c := m.cands[id]; c.Status != types.Pending || c.Review.Action != types.ReviewUndecided || c.Review.Tier != 1 {
		t.Errorf("no tier 2: %v review %+v; want undecided at tier 1", c.Status, c.Review)
	}

	// Tier 2's analysis is scored by the decision model, and its number is the one acted on.
	m = newFakeMap(t, directed(id, ""))
	d, a := &sure{fixed{actions: unsure}}, &analyst{}
	run(t, m, autonomy.Config{Level: autonomy.Full, Decider: d, Reasoner: a})
	c := m.cands[id]
	if c.Status != types.Confirmed || c.Review.Tier != 2 || c.Review.By != types.ReviewerReasoningAgent || a.calls != 1 {
		t.Errorf("tier 2: %v review %+v calls %d; want confirmed at tier 2", c.Status, c.Review, a.calls)
	}
	if len(d.analyses) != 2 || d.analyses[1] == "" {
		t.Errorf("the decision model was asked %q; want a first pass and one scoring the analysis", d.analyses)
	}
	dec, ok := m.st.Journal().Decision(c.Review.DecisionID)
	if !ok {
		t.Fatalf("decision %s is not in the journal", c.Review.DecisionID)
	}
	for _, k := range []string{"candidate", "autonomy", "tier1", "tier2_analysis", "tier2"} {
		if _, ok := dec.Inputs[k]; !ok {
			t.Errorf("the decision has no %q input: %v", k, dec.Inputs)
		}
	}
	if len(dec.PropertiesRead) != 2 || dec.Answer["action"] != types.ReviewConfirm || dec.Answer["applied"] != true {
		t.Errorf("decision read %d properties and answered %v", len(dec.PropertiesRead), dec.Answer)
	}

	// A reviewed candidate is not reviewed again until its evidence moves.
	r := run(t, m, autonomy.Config{Level: autonomy.Full, Decider: &fixed{actions: unsure}})
	if s := r.Status(); s.Reviewed != 0 {
		t.Errorf("a settled candidate was reviewed again: %+v", s)
	}
}

func TestReviewCitesThePriorReview(t *testing.T) {
	const id = "cpu_utilization@pod:a->cpu_pressure"
	c := directed(id, "")
	c.Review = &types.CandidateReview{DecisionID: "verify-1"}
	m := newFakeMap(t, c)
	run(t, m, autonomy.Config{Level: autonomy.Full, Decider: &fixed{actions: confident(types.ReviewConfirm)}})
	got := m.cands[id].Review
	if got == nil || got.DecisionID == "verify-1" {
		t.Fatalf("review %+v; want a new review", got)
	}
	dec, ok := m.st.Journal().Decision(got.DecisionID)
	if !ok {
		t.Fatalf("decision %s is not in the journal", got.DecisionID)
	}
	if dec.Kind != "review" || len(dec.Cites) != 1 || dec.Cites[0] != "verify-1" {
		t.Errorf("review kind %q cites %v; want review citing verify-1", dec.Kind, dec.Cites)
	}
}

func TestAnUndirectedConfirmationNeedsADriver(t *testing.T) {
	const id = "cpu_pressure<->io_pressure"
	und := func() *types.CandidateEdge {
		return &types.CandidateEdge{CandidateID: id, FromID: "cpu_pressure", ToID: "io_pressure", MIScore: 0.9,
			NObservations: 120, Undirected: true, Status: types.Pending}
	}
	m := newFakeMap(t, und())
	run(t, m, autonomy.Config{Level: autonomy.Full, Decider: &fixed{actions: confident(types.ReviewConfirm),
		orientation: map[string]float64{"FROM": 0.4, "TO": 0.3, "unclear": 0.3}}})
	if c := m.cands[id]; c.Status != types.Pending || c.Review.Action != types.ReviewUndecided {
		t.Errorf("no clear driver: %v review %+v; want undecided", c.Status, c.Review)
	}
	m = newFakeMap(t, und())
	run(t, m, autonomy.Config{Level: autonomy.Full, Decider: &fixed{actions: confident(types.ReviewConfirm),
		orientation: map[string]float64{"FROM": 0.02, "TO": 0.95, "unclear": 0.03}}})
	if c := m.cands[id]; c.Status != types.Confirmed || m.from[id] != "io_pressure" {
		t.Errorf("a clear driver: %v from %q; want confirmed from io_pressure", c.Status, m.from[id])
	}
}

func TestAnUnreachableModelRecordsNothingAndTriesAgain(t *testing.T) {
	const a, b = "cpu_utilization@pod:a->cpu_pressure", "cpu_utilization@pod:b->cpu_pressure"
	m := newFakeMap(t, directed(a, ""), directed(b, ""))
	down := &fixed{err: errors.New("connection refused")}
	r := run(t, m, autonomy.Config{Level: autonomy.Full, Decider: down})
	for _, id := range []string{a, b} {
		if c := m.cands[id]; c.Status != types.Pending || c.Review != nil {
			t.Errorf("%s %v review %+v; want it untouched: an outage is not a review", id, c.Status, c.Review)
		}
	}
	if s := r.Status(); s.Errors != 1 || s.Reviewed != 0 || !strings.Contains(s.LastError, "refused") {
		t.Errorf("status %+v; want one error and the cycle stopped there", s)
	}
	if len(down.analyses) != 1 {
		t.Errorf("the model was asked %d times; want the cycle to stop at the first failure", len(down.analyses))
	}

	down.err, down.actions = nil, confident(types.ReviewConfirm)
	r.Cycle(context.Background())
	if c := m.cands[a]; c.Status != types.Confirmed {
		t.Errorf("%v review %+v; want it reviewed once the model is back", c.Status, c.Review)
	}
}

func TestAReasonerNeedsADecider(t *testing.T) {
	if _, err := autonomy.New(newFakeMap(t), autonomy.Config{Reasoner: &analyst{}}); err == nil {
		t.Error("a reasoning model without a decision model was accepted")
	}
}

// The models are given the machine the metrics belong to, and the decision keeps the
// facts it read.
func TestTheModelsAreGivenTheMachine(t *testing.T) {
	const id = "cpu_utilization@pod:a->cpu_pressure"
	m := newFakeMap(t, directed(id, ""))
	if err := m.st.RecordFacts("host", []statemap.Fact{
		statemap.NumberFact("cpu_cores", 4, "cores"), {Key: "os", Value: "Ubuntu 24.04.3 LTS"},
	}); err != nil {
		t.Fatal(err)
	}
	d := &telling{fixed: fixed{actions: confident(types.ReviewConfirm)}}
	run(t, m, autonomy.Config{Level: autonomy.Full, Decider: d})
	if len(d.facts) != 2 {
		t.Errorf("the decision model was given facts %+v", d.facts)
	}
	dec, _ := m.st.Journal().Decision(m.cands[id].Review.DecisionID)
	if dec == nil || len(dec.FactsRead) != 2 {
		t.Fatalf("decision = %+v", dec)
	}
}

type telling struct {
	fixed
	facts []statemap.Fact
}

func (d *telling) Decide(ctx context.Context, e autonomy.Evidence, analysis string) (*autonomy.Verdict, error) {
	d.facts = e.Facts
	return d.fixed.Decide(ctx, e, analysis)
}

// down is a reasoning model that cannot be reached.
type down struct{ calls int }

func (d *down) Name() string { return "down" }
func (d *down) Reason(context.Context, autonomy.Evidence) (*autonomy.Analysis, error) {
	d.calls++
	return nil, errors.New("connection refused")
}

// TestAnOutageSkipsOnlyWhatNeedsTheModel: on a machine with no reasoning model, the
// review still runs. What needs the missing tier waits, unrecorded — an outage is not
// a judgement — and the rest of the queue is settled by the tiers that answer, so a
// default that names a reasoning model cannot stall review on a node without one.
func TestAnOutageSkipsOnlyWhatNeedsTheModel(t *testing.T) {
	const a, b = "cpu_utilization@pod:a->cpu_pressure", "cpu_utilization@pod:b->cpu_pressure"
	const comp = "cpu_utilization@pod:c->cpu_utilization"
	unsure := map[string]float64{types.ReviewConfirm: 0.6, types.ReviewReject: 0.3, types.ReviewDefer: 0.1}
	m := newFakeMap(t, directed(a, ""), directed(b, ""), directed(comp, types.CandidateKindComposition))
	m.cands[comp].MIScore = 0.1 // reviewed last
	rs := &down{}
	r := run(t, m, autonomy.Config{Level: autonomy.Full, Decider: &fixed{actions: unsure}, Reasoner: rs})
	for _, id := range []string{a, b} {
		if c := m.cands[id]; c.Status != types.Pending || c.Review != nil {
			t.Errorf("%s %v review %+v; want it untouched until the reasoning model answers", id, c.Status, c.Review)
		}
	}
	if c := m.cands[comp]; c.Status != types.Confirmed {
		t.Errorf("composition %v review %+v; want the rule to settle it despite the outage", c.Status, c.Review)
	}
	if rs.calls != 1 {
		t.Errorf("the reasoning model was asked %d times; want once, then skipped for the cycle", rs.calls)
	}
	if s := r.Status(); s.Errors != 1 {
		t.Errorf("status %+v; want the outage counted once", s)
	}
}

// TestRulesSettleWhatTheyCanBeforeAModelIsAsked: tier 0 costs nothing and a model call
// costs a minute or more on the node, so a cycle settles every candidate a rule decides
// before it spends models on the rest. Ordered strongest-first with the models' cap,
// a composition candidate behind slower ones waited for an hour on the case-study node.
func TestRulesSettleWhatTheyCanBeforeAModelIsAsked(t *testing.T) {
	const comp = "memory_utilization@pod:wave->memory_utilization"
	var cands []*types.CandidateEdge
	for _, p := range []string{"a", "b", "c", "d", "e"} {
		cands = append(cands, directed("cpu_utilization@pod:"+p+"->cpu_pressure", ""))
	}
	weak := directed(comp, types.CandidateKindComposition)
	weak.MIScore = 0.01
	cands = append(cands, weak)
	m := newFakeMap(t, cands...)
	unsure := map[string]float64{types.ReviewConfirm: 0.6, types.ReviewReject: 0.3, types.ReviewDefer: 0.1}
	d := &fixed{actions: unsure}
	r := run(t, m, autonomy.Config{Level: autonomy.Full, Decider: d, MaxPer: 2})
	if c := m.cands[comp]; c.Status != types.Confirmed || c.Review == nil || c.Review.By != types.ReviewerRules {
		t.Fatalf("composition %v review %+v; want the rule to confirm it in the first cycle", c.Status, c.Review)
	}
	if len(d.analyses) != 2 {
		t.Fatalf("the model was asked %d times; want the cap of 2, the rule's candidate not counted", len(d.analyses))
	}
	if s := r.Status(); s.Reviewed != 3 {
		t.Fatalf("reviewed %d; want the rule's one and the models' two", s.Reviewed)
	}
}

// A candidate the operator or the agent settled is closed: tier 0 leaves it alone
// even when the rule would hold and it was never reviewed.
func TestTier0LeavesAClosedCandidateAlone(t *testing.T) {
	const deferred, rejected = "cpu_utilization@pod:a->cpu_utilization", "cpu_utilization@pod:b->cpu_utilization"
	d, rj := directed(deferred, types.CandidateKindComposition), directed(rejected, types.CandidateKindComposition)
	d.Status, d.Reason = types.Deferred, types.ReasonOperator
	rj.Status, rj.Reason = types.Rejected, types.ReasonAgent
	m := newFakeMap(t, d, rj)
	r := run(t, m, autonomy.Config{Level: autonomy.Full})
	for _, c := range []*types.CandidateEdge{m.cands[deferred], m.cands[rejected]} {
		if c.Status == types.Confirmed || c.Review != nil {
			t.Errorf("%s: %v review %+v; a closed candidate is not reviewed", c.CandidateID, c.Status, c.Review)
		}
	}
	if s := r.Status(); s.Reviewed != 0 || s.Errors != 0 {
		t.Errorf("reviewed %d, errors %d; want neither", s.Reviewed, s.Errors)
	}
	if ds := m.st.Journal().Decisions(0, ""); len(ds) != 0 {
		t.Errorf("%d decisions journaled for closed candidates", len(ds))
	}
}

// Tier 0 reviews a candidate once per evidence: a suggestion left on the queue is not
// made again next cycle, and is once the evidence moves past what it was made on.
func TestTier0ReviewsAgainOnlyWhenTheEvidenceMoves(t *testing.T) {
	const id = "cpu_utilization@pod:a->cpu_utilization"
	m := newFakeMap(t, directed(id, types.CandidateKindComposition))
	r := run(t, m, autonomy.Config{Level: autonomy.Suggest})
	if c := m.cands[id]; c.Status != types.Pending || c.Review == nil || c.Review.By != types.ReviewerRules {
		t.Fatalf("first cycle: %v review %+v; want a rule's suggestion on a pending candidate", c.Status, c.Review)
	}
	r.Cycle(context.Background())
	if s := r.Status(); s.Reviewed != 1 {
		t.Fatalf("reviewed %d times at the same evidence; want once", s.Reviewed)
	}
	m.cands[id].MIScore, m.cands[id].NObservations = 0.6, 240
	r.Cycle(context.Background())
	if s := r.Status(); s.Reviewed != 2 {
		t.Errorf("reviewed %d times after the evidence moved; want twice", s.Reviewed)
	}
}

// A refused annotation — the operator closed the candidate between the cycle reading
// it and the review being recorded — is not discarded: it is counted, and the decision
// says so.
func TestARefusedAnnotationIsCounted(t *testing.T) {
	const id = "cpu_utilization@pod:a->cpu_utilization"
	c := directed(id, types.CandidateKindComposition)
	c.Status, c.Reason = types.Deferred, "weakened"
	m := &refusingMap{newFakeMap(t, c)}
	cfg := autonomy.Config{Level: autonomy.Full, Logf: t.Logf}
	parts, err := autonomy.Build(m, autonomy.Parts{Rules: []string{"composition"}})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Rules = parts.Rules
	r, err := autonomy.New(m, cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.Cycle(context.Background())
	if s := r.Status(); s.Errors != 1 || !strings.Contains(s.LastError, "recording the review") {
		t.Errorf("errors %d (%q); want the refused annotation counted", s.Errors, s.LastError)
	}
	if c := m.cands[id]; c.Status != types.Deferred || c.Reason != "weakened" {
		t.Errorf("candidate is %v/%q; want it as it was", c.Status, c.Reason)
	}
}

// refusingMap refuses every confirmation, as the map does when the declaration it
// stands for is refused, and every annotation, as the proposer does for a candidate
// that is no longer open.
type refusingMap struct{ *fakeMap }

func (refusingMap) ConfirmCandidate(string, string, types.CandidateReview) error {
	return errors.New("an opposite-sign relationship covers the pair")
}

func (refusingMap) AnnotateCandidate(string, types.CandidateReview) error {
	return errors.New("the operator closed the candidate")
}

// TestStatusDueCountsActiveDueCandidates: Due is the due work a cycle could act on.
// A candidate waiting for an inactive end is not due work, so it is in Waiting only.
func TestStatusDueCountsActiveDueCandidates(t *testing.T) {
	const a, b = "cpu_utilization@pod:a->cpu_pressure", "cpu_utilization@pod:b->cpu_pressure"
	const gone = "cpu_utilization@pod:gone->cpu_pressure"
	m := newFakeMap(t, directed(a, ""), directed(b, ""), directed(gone, ""))
	if err := m.st.RetireProperty("cpu_utilization@pod:gone", "test", "test"); err != nil {
		t.Fatal(err)
	}
	r := run(t, m, autonomy.Config{Level: autonomy.Suggest, Decider: &fixed{actions: map[string]float64{types.ReviewConfirm: 0.6, types.ReviewReject: 0.3, types.ReviewDefer: 0.1}}})
	if s := r.Status(); s.Due != 2 || s.Waiting != 1 {
		t.Errorf("status due=%d waiting=%d; want 2 due and 1 waiting", s.Due, s.Waiting)
	}
}

// TestRulesOnlyDueIsWhatARuleCanSettle: with no model tier, a candidate no rule
// answers cannot be settled by reviewing it again, so it is neither due nor
// reviewed; only what a rule answers is.
func TestRulesOnlyDueIsWhatARuleCanSettle(t *testing.T) {
	const comp, plain = "cpu_utilization@pod:a->cpu_utilization", "cpu_utilization@pod:b->cpu_pressure"
	m := newFakeMap(t, directed(comp, types.CandidateKindComposition), directed(plain, ""))
	r := run(t, m, autonomy.Config{Level: autonomy.Full})
	if s := r.Status(); s.Due != 0 || s.Reviewed != 1 || s.Undecided != 0 {
		t.Errorf("status due=%d reviewed=%d undecided=%d; want the composition reviewed and nothing left due", s.Due, s.Reviewed, s.Undecided)
	}
}

// TestDueLeavesOutWhatTheCycleSettled: Due is read by the pace until the next cycle,
// which at the slow pace is minutes away; a candidate the cycle's rules settled is
// not work left, so it must not count, or the pace returns to base for work done.
func TestDueLeavesOutWhatTheCycleSettled(t *testing.T) {
	const comp = "cpu_utilization@pod:a->cpu_utilization"
	m := newFakeMap(t, directed(comp, types.CandidateKindComposition))
	r := run(t, m, autonomy.Config{Level: autonomy.Full, Decider: &fixed{actions: map[string]float64{types.ReviewConfirm: 0.6, types.ReviewReject: 0.3, types.ReviewDefer: 0.1}}})
	if s := r.Status(); s.Due != 0 || s.Applied != 1 {
		t.Errorf("status due=%d applied=%d; want the composition confirmed by its rule and nothing due", s.Due, s.Applied)
	}
}
