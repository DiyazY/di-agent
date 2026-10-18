package autonomy_test

import (
	"context"
	"errors"
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

func (m *fakeMap) CandidateHistory() ([]*types.CandidateEdge, error) { return m.PendingCandidates() }

func (m *fakeMap) settle(id string, s types.CandidateStatus, rv types.CandidateReview) error {
	c := m.cands[id]
	rv.Applied = true
	rv.MIScore, rv.NObservations, rv.Direction, rv.Kind = c.MIScore, c.NObservations, c.Direction, c.Kind
	c.Status, c.Review = s, &rv
	return nil
}

func (m *fakeMap) ConfirmCandidate(id, from string, rv types.CandidateReview) error {
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
	rv.MIScore, rv.NObservations, rv.Direction, rv.Kind = c.MIScore, c.NObservations, c.Direction, c.Kind
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

func TestAnUnsureModelGoesUpATierAndThenToTheOperator(t *testing.T) {
	const id = "cpu_utilization@pod:a->cpu_pressure"
	unsure := map[string]float64{types.ReviewConfirm: 0.6, types.ReviewReject: 0.3, types.ReviewDefer: 0.1}

	// With no tier 2, it escalates.
	m := newFakeMap(t, directed(id, ""))
	run(t, m, autonomy.Config{Level: autonomy.Full, Decider: &fixed{actions: unsure}})
	if c := m.cands[id]; c.Status != types.Pending || c.Review.Action != types.ReviewEscalate || c.Review.Tier != 1 {
		t.Errorf("no tier 2: %v review %+v; want escalated from tier 1", c.Status, c.Review)
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

func TestAnUndirectedConfirmationNeedsADriver(t *testing.T) {
	const id = "cpu_pressure<->io_pressure"
	und := func() *types.CandidateEdge {
		return &types.CandidateEdge{CandidateID: id, FromID: "cpu_pressure", ToID: "io_pressure", MIScore: 0.9,
			NObservations: 120, Undirected: true, Status: types.Pending}
	}
	m := newFakeMap(t, und())
	run(t, m, autonomy.Config{Level: autonomy.Full, Decider: &fixed{actions: confident(types.ReviewConfirm),
		orientation: map[string]float64{"FROM": 0.4, "TO": 0.3, "unclear": 0.3}}})
	if c := m.cands[id]; c.Status != types.Pending || c.Review.Action != types.ReviewEscalate {
		t.Errorf("no clear driver: %v review %+v; want escalated", c.Status, c.Review)
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
