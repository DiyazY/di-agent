package autonomy_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/internal/minimal"
	"github.com/DiyazY/di-agent/pkg/autonomy"
	"github.com/DiyazY/di-agent/pkg/semmap"
	"github.com/DiyazY/di-agent/pkg/statemap"
	"github.com/DiyazY/di-agent/pkg/types"
)

func TestPersistentCorrelationRule(t *testing.T) {
	for _, tc := range []struct {
		name       string
		windows    int
		undirected bool
		caveats    []string
		level      autonomy.Level
		wantAction string
		applied    bool
	}{
		{"legacy candidate", 0, false, nil, autonomy.Rules, types.ReviewUndecided, false},
		{"two windows", 2, false, nil, autonomy.Rules, types.ReviewUndecided, false},
		{"directed correlation is association-only", 3, false, nil, autonomy.Rules, types.ReviewConfirm, true},
		{"undirected", 3, true, nil, autonomy.Rules, types.ReviewConfirm, true},
		{"caveat", 3, false, []string{"missing range"}, autonomy.Rules, types.ReviewUndecided, false},
		{"suggest only", 3, false, nil, autonomy.Suggest, types.ReviewConfirm, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := directed("cpu_utilization@pod:a->cpu_pressure", "")
			c.PersistentWindows, c.Undirected, c.Caveats = tc.windows, tc.undirected, tc.caveats
			m := newFakeMap(t, c)
			cfg, err := autonomy.Build(m, autonomy.Parts{Rules: []string{"persistent-correlation"}})
			if err != nil {
				t.Fatal(err)
			}
			cfg.Level = tc.level
			run(t, m, cfg)
			if c.Review == nil || c.Review.Action != tc.wantAction || c.Review.Applied != tc.applied {
				t.Fatalf("review %+v; want %s applied=%v", c.Review, tc.wantAction, tc.applied)
			}
			if tc.wantAction == types.ReviewConfirm {
				if !c.Review.Association {
					t.Fatalf("association flag does not match orientation: %+v", c.Review)
				}
				if c.Review.Model != "persistent-correlation" {
					t.Fatalf("wrong rule recorded: %+v", c.Review)
				}
				d, ok := m.st.Journal().Decision(c.Review.DecisionID)
				if !ok || d.Inputs["candidate"].(types.CandidateEdge).PersistentWindows != 3 {
					t.Fatalf("decision lacks persistence evidence: %+v", d)
				}
			}
		})
	}
}

func TestPersistenceProgressOutdatesOnlyAtApprovalThreshold(t *testing.T) {
	review := &types.CandidateReview{
		Direction: types.Positive, Kind: types.CandidateKindSameSubject,
		MIScore: 0.9, NObservations: 36, PersistentWindows: 1,
	}
	candidate := &types.CandidateEdge{
		Direction: types.Positive, Kind: types.CandidateKindSameSubject,
		MIScore: 0.9, NObservations: 36, PersistentWindows: 2,
	}
	if review.Outdated(candidate) {
		t.Fatal("ordinary persistence progress reopened the review")
	}
	candidate.PersistentWindows = types.PersistentWindowsRequired
	if !review.Outdated(candidate) {
		t.Fatal("crossing the persistence approval threshold did not reopen the review")
	}
	review.PersistentWindows = types.PersistentWindowsRequired
	candidate.PersistentWindows++
	if review.Outdated(candidate) {
		t.Fatal("additional windows after approval threshold reopened the review")
	}
	candidate.PersistentWindows = 0
	if !review.Outdated(candidate) {
		t.Fatal("a persistence reset below the approval threshold did not reopen the review")
	}
}

func TestPersistentCorrelationIsReviewedWhenWindowsAdvance(t *testing.T) {
	c := directed("cpu_utilization@pod:a->cpu_pressure", "")
	c.PersistentWindows = 1
	m := newFakeMap(t, c)
	cfg, err := autonomy.Build(m, autonomy.Parts{Rules: []string{"persistent-correlation"}})
	if err != nil {
		t.Fatal(err)
	}

	cfg.Level = autonomy.Rules
	r := run(t, m, cfg)
	first := c.Review.DecisionID
	for i := 0; i < 10; i++ {
		r.Cycle(context.Background())
	}
	if c.Review.DecisionID != first || c.Status != types.Pending {
		t.Fatal("repeated review cycles changed unchanged evidence")
	}
	c.PersistentWindows = 2
	r.Cycle(context.Background())
	if c.Review.DecisionID != first || c.Status != types.Pending {
		t.Fatal("ordinary persistence progress reopened the review or approved too soon")
	}
	c.PersistentWindows = 3
	r.Cycle(context.Background())
	if c.Status != types.Confirmed || c.Review.PersistentWindows != 3 || c.Review.DecisionID == first {
		t.Fatalf("unchanged r with new windows was not confirmed: %+v", c)
	}
}

func TestPersistentAssociationSurvivesRestartWithoutEnteringEstimates(t *testing.T) {
	const from, to = "load@engine:test", "speed@engine:test"
	build := func(state *statemap.Map) (*semmap.SemanticMap, *minimal.MICorrelationProposer) {
		p := minimal.NewMICorrelationProposer(state, 0.9, 6, 12, 0)
		m := semmap.New(minimal.NewOntologyFromSpec(nil), nil, p, minimal.NewDisabledTuner())
		m.AttachState(state)
		if _, err := m.RestoreDiscovery(time.Now(), 2*time.Minute); err != nil {
			t.Fatal(err)
		}
		return m, p
	}
	state := statemap.New(statemap.Config{AdmitUnknown: true}, statemap.NewJournal(0))
	m, p := build(state)
	base := time.Now().Add(-time.Minute)
	feed := func(i int) {
		at := base.Add(time.Duration(i) * time.Second)
		x := float64(i%12) / 12
		for _, reading := range []types.PropertyReading{
			{ID: from, Subject: "engine:test", At: at, Value: x, Range: &[2]float64{0, 1}},
			{ID: to, Subject: "engine:test", At: at, Value: 0.1 + 0.8*x, Range: &[2]float64{0, 1}},
		} {
			if err := state.Record(statemap.Observation{ID: reading.ID, Subject: reading.Subject, Value: reading.Value, At: at}); err != nil {
				t.Fatal(err)
			}
			if err := p.ObserveProperty(reading); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := 0; i < 29; i++ {
		feed(i)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := state.Save(path); err != nil {
		t.Fatal(err)
	}
	state = statemap.New(statemap.Config{AdmitUnknown: true}, statemap.NewJournal(0))
	if r, err := state.Load(path); err != nil || !r.Loaded {
		t.Fatalf("load: %v %v", r.Loaded, err)
	}
	m, p = build(state)
	cs, err := m.PendingCandidates()
	if err != nil || len(cs) != 1 || cs[0].PersistentWindows != 2 || cs[0].PersistencePairs != 5 {
		t.Fatalf("progress not restored: %+v %v", cs, err)
	}
	cfg, err := autonomy.Build(m, autonomy.Parts{Rules: []string{"persistent-correlation"}})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Level, cfg.Logf = autonomy.Rules, t.Logf
	reviewer, err := autonomy.New(m, cfg)
	if err != nil {
		t.Fatal(err)
	}
	reviewer.Cycle(context.Background())
	for i := 29; i < 36; i++ {
		if state.Associated(from, to, 1) {
			t.Fatal("association accepted before three complete windows")
		}
		feed(i)
		reviewer.Cycle(context.Background())
	}
	if !state.Associated(from, to, 1) || state.Covered(from, to, 1) || state.Covered(to, from, 1) {
		t.Fatal("association was not accepted independently of direction")
	}
	cs, err = m.CandidateHistory()
	if err != nil || len(cs) != 1 || cs[0].Status != types.Confirmed || cs[0].Review == nil || !cs[0].Review.Association {
		t.Fatalf("missing association review: %+v %v", cs, err)
	}
	d, ok := state.Journal().Decision(cs[0].Review.DecisionID)
	if !ok || d.Answer["association"] != true || d.Answer["applied"] != true {
		t.Fatalf("association decision is not auditable: %+v", d)
	}
	res := state.Estimate(statemap.EstimateRequest{Target: to, Assume: map[string]float64{from: 1}})
	if len(res.Influences) != 0 || res.Answer.Sensitivity != 0 {
		t.Fatalf("association entered estimate: %+v", res)
	}
	if err := state.Save(path); err != nil {
		t.Fatal(err)
	}
	state = statemap.New(statemap.Config{AdmitUnknown: true}, statemap.NewJournal(0))
	if r, err := state.Load(path); err != nil || !r.Loaded {
		t.Fatalf("load confirmed association: %v %v", r.Loaded, err)
	}
	m, p = build(state)
	if cs, err := m.PendingCandidates(); err != nil || len(cs) != 0 || !state.Associated(from, to, 1) {
		t.Fatalf("accepted association became pending on restart: %+v %v", cs, err)
	}
}

func TestPersistentCorrelationCreatesATracedAssociationForDirectedPairs(t *testing.T) {
	const from, to = "cpu_utilization@pod:a", "cpu_pressure"
	state := statemap.New(statemap.Config{AdmitUnknown: true}, statemap.NewJournal(0))
	p := minimal.NewMICorrelationProposer(state, 0.9, 6, 12, 0)
	m := semmap.New(minimal.NewOntologyFromSpec(nil), nil, p, minimal.NewDisabledTuner())
	m.AttachState(state)
	cfg, err := autonomy.Build(m, autonomy.Parts{Rules: []string{"composition", "persistent-correlation"}})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Level, cfg.Logf = autonomy.Rules, t.Logf
	r, err := autonomy.New(m, cfg)
	if err != nil {
		t.Fatal(err)
	}
	rng := &[2]float64{0, 1}
	for i := 0; i < 36; i++ {
		at := time.Now().Add(time.Duration(i) * 10 * time.Second)
		x := float64(i%12) / 12
		for _, reading := range []types.PropertyReading{
			{ID: from, Subject: "pod:a", At: at, Value: x, Range: rng},
			{ID: to, At: at, Value: 0.8*x + 0.1, Range: rng},
		} {
			if err := state.Record(statemap.Observation{ID: reading.ID, Subject: reading.Subject, Value: reading.Value, At: at}); err != nil {
				t.Fatal(err)
			}
			if err := p.ObserveProperty(reading); err != nil {
				t.Fatal(err)
			}
			// A replay of the same timestamp must not supply another observation.
			if err := p.ObserveProperty(reading); err != nil {
				t.Fatal(err)
			}
		}
		r.Cycle(context.Background())
		if i < 35 && state.Covered(from, to, 1) {
			t.Fatalf("confirmed after %d pairs, before three full windows", i+1)
		}
	}
	var rel statemap.Relationship
	ok := false
	for _, relationship := range state.Relationships("", "") {
		if relationship.Association && relationship.Label == "association" {
			rel, ok = relationship, true
			break
		}
	}
	if !ok || !rel.Association || rel.Provenance != statemap.Discovered ||
		!strings.Contains(rel.Note, "persistent-correlation") || state.Covered(from, to, 1) ||
		!state.Associated(from, to, 1) {
		t.Fatalf("missing traced association-only relationship: %+v", rel)
	}
	h, err := m.CandidateHistory()
	if err != nil || len(h) != 1 {
		t.Fatalf("history %v: %v", h, err)
	}
	c := h[0]
	if c.Status != types.Confirmed || c.Review == nil || c.Review.PersistentWindows != 3 ||
		!c.Review.Association {
		t.Fatalf("candidate not confirmed with persistence evidence: %+v", c)
	}
	d, ok := state.Journal().Decision(c.Review.DecisionID)
	if !ok || d.Answer["model"] != "persistent-correlation" || d.Answer["applied"] != true ||
		d.Answer["association"] != true {
		t.Fatalf("missing applied decision: %+v", d)
	}
}

// A composition candidate the proposer withdrew before any review cycle reached it —
// its |r| weakened within the 30 s it was pending, as the mem-squeeze pair's did in
// the case study — is still a part against its whole, and the rule settles it at the
// next cycle.
func TestTheRuleSettlesACompositionTheProposerWithdrewBeforeAReview(t *testing.T) {
	state, m := withdrawnComposition(t)
	cfg, err := autonomy.Build(m, autonomy.Parts{Rules: []string{"composition"}})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Level, cfg.Logf = autonomy.Suggest, t.Logf
	r, err := autonomy.New(m, cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.Cycle(context.Background())
	if s := r.Status(); s.Reviewed != 0 || s.Errors != 0 {
		t.Fatalf("at suggest a withdrawn candidate has no queue to suggest on; reviewed %d, errors %d", s.Reviewed, s.Errors)
	}
	if h, _ := m.CandidateHistory(); h[0].Status != types.Deferred || h[0].Reason != "weakened" || h[0].Review != nil {
		t.Fatalf("at suggest the withdrawn candidate was touched: %v/%q review %+v", h[0].Status, h[0].Reason, h[0].Review)
	}
	if ds := state.Journal().Decisions(0, ""); len(ds) != 0 {
		t.Fatalf("at suggest %d decisions were journaled for a withdrawn candidate", len(ds))
	}
	cfg.Level = autonomy.Full
	if r, err = autonomy.New(m, cfg); err != nil {
		t.Fatal(err)
	}
	r.Cycle(context.Background())
	h, err := m.CandidateHistory()
	if err != nil || len(h) != 1 || h[0].Status != types.Confirmed || h[0].Review == nil ||
		h[0].Review.By != types.ReviewerRules {
		t.Fatalf("withdrawn composition candidate was not settled by the rule: %+v %v", h[0], err)
	}
	if !state.Covered(withdrawnFrom, withdrawnTo, 1) {
		t.Fatal("the confirmation declared no relationship")
	}
}

const withdrawnFrom, withdrawnTo = "memory_pressure_ratio@pod:squeeze", "memory_pressure_ratio"

// withdrawnComposition builds a map whose one candidate is a composition pair the
// proposer raised and then deferred as weakened.
func withdrawnComposition(t *testing.T) (*statemap.Map, *semmap.SemanticMap) {
	t.Helper()
	const from, to = withdrawnFrom, withdrawnTo
	state := statemap.New(statemap.Config{AdmitUnknown: true}, statemap.NewJournal(0))
	p := minimal.NewMICorrelationProposer(state, 0.9, 6, 12, 0)
	m := semmap.New(minimal.NewOntologyFromSpec(nil), nil, p, minimal.NewDisabledTuner())
	m.AttachState(state)
	rng := &[2]float64{0, 1}
	base := time.Now().Add(-time.Hour)
	feed := func(i int, x, y float64) {
		at := base.Add(time.Duration(i) * 10 * time.Second)
		for _, reading := range []types.PropertyReading{
			{ID: from, Subject: "pod:squeeze", At: at, Value: x, Range: rng},
			{ID: to, At: at, Value: y, Range: rng},
		} {
			if err := state.Record(statemap.Observation{ID: reading.ID, Subject: reading.Subject, Value: reading.Value, At: at}); err != nil {
				t.Fatal(err)
			}
			if err := p.ObserveProperty(reading); err != nil {
				t.Fatal(err)
			}
		}
	}
	i := 0
	for ; i < 12; i++ {
		x := float64(i) / 12
		feed(i, x, 0.8*x+0.1)
	}
	if cs, _ := m.PendingCandidates(); len(cs) != 1 || cs[0].Kind != types.CandidateKindComposition {
		t.Fatalf("want one pending composition candidate, got %+v", cs)
	}
	// The pair stops holding before the reviewer's next cycle: the node total now moves
	// with something else.
	for ; ; i++ {
		feed(i, float64(i%12)/12, float64((i*7)%12)/12)
		if h, _ := m.CandidateHistory(); h[0].Status == types.Deferred {
			if h[0].Reason != "weakened" {
				t.Fatalf("deferred for %q; want weakened", h[0].Reason)
			}
			break
		}
		if i > 200 {
			t.Fatal("the candidate never weakened")
		}
	}
	return state, m
}

// A withdrawn candidate whose confirmation the map refuses — an opposite-sign edge
// already holds the pair — keeps the review that settled on it, so it is reviewed
// once: the next cycle does not confirm it again, fail again and journal another
// decision while its evidence stands still.
func TestARefusedWithdrawnCandidateIsReviewedOnce(t *testing.T) {
	state, m := withdrawnComposition(t)
	if err := state.DeclareRelationship(statemap.Relationship{From: withdrawnFrom, To: withdrawnTo,
		Label: "discovered", Sign: -1, Provenance: statemap.Discovered}); err != nil {
		t.Fatal(err)
	}
	cfg, err := autonomy.Build(m, autonomy.Parts{Rules: []string{"composition"}})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Level, cfg.Logf = autonomy.Full, t.Logf
	r, err := autonomy.New(m, cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.Cycle(context.Background())
	h, _ := m.CandidateHistory()
	if h[0].Status != types.Deferred || h[0].Reason != "weakened" || h[0].Review == nil ||
		h[0].Review.Action != types.ReviewUndecided {
		t.Fatalf("after the refusal the candidate is %v/%q review %+v; want deferred as weakened, its undecided review kept",
			h[0].Status, h[0].Reason, h[0].Review)
	}
	decisions, errs := len(state.Journal().Decisions(0, "")), r.Status().Errors
	if decisions == 0 {
		t.Fatal("the refused confirmation journaled no decision")
	}
	r.Cycle(context.Background())
	if n := len(state.Journal().Decisions(0, "")); n != decisions {
		t.Errorf("the second cycle journaled %d more decisions for a candidate whose evidence did not move", n-decisions)
	}
	if e := r.Status().Errors; e != errs {
		t.Errorf("the second cycle failed %d more times; want the candidate left alone", e-errs)
	}
}

// The persistent-correlation rule reads a cap-deferred candidate's evidence as it is:
// the cap is a choice about the queue, the pair's windows still count, and the map
// finds the candidate for the association in history, not only in the queue.
func TestPersistentCorrelationAssociatesACapDeferredCandidate(t *testing.T) {
	const node = "cpu_pressure"
	state := statemap.New(statemap.Config{AdmitUnknown: true}, statemap.NewJournal(0))
	p := minimal.NewMICorrelationProposer(state, 0.9, 6, 12, 0)
	p.SetMaxPending(1)
	p.SetLogger(t.Logf)
	m := semmap.New(minimal.NewOntologyFromSpec(nil), nil, p, minimal.NewDisabledTuner())
	m.AttachState(state)
	rng := &[2]float64{0, 1}
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 36; i++ {
		at := base.Add(time.Duration(i) * 10 * time.Second)
		x := float64(i%12) / 12
		for _, reading := range []types.PropertyReading{
			{ID: "cpu_utilization@pod:a", Subject: "pod:a", At: at, Value: x, Range: rng},
			{ID: "cpu_utilization@pod:b", Subject: "pod:b", At: at, Value: 0.5*x + 0.2, Range: rng},
			{ID: node, At: at, Value: 0.8*x + 0.1, Range: rng},
		} {
			if err := state.Record(statemap.Observation{ID: reading.ID, Subject: reading.Subject, Value: reading.Value, At: at}); err != nil {
				t.Fatal(err)
			}
			if err := p.ObserveProperty(reading); err != nil {
				t.Fatal(err)
			}
		}
	}
	h, _ := m.CandidateHistory()
	var capped *types.CandidateEdge
	for _, c := range h {
		if c.Status == types.Deferred && c.Reason == "cap" && c.PersistentWindows >= types.PersistentWindowsRequired && len(c.Caveats) == 0 {
			capped = c
			break
		}
	}
	if capped == nil {
		t.Fatalf("setup: no persistent cap-deferred candidate in %+v", h)
	}
	cfg, err := autonomy.Build(m, autonomy.Parts{Rules: []string{"persistent-correlation"}})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Level, cfg.Logf = autonomy.Rules, t.Logf
	r, err := autonomy.New(m, cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.Cycle(context.Background())
	if s := r.Status(); s.Errors != 0 {
		t.Fatalf("errors %d: %s", s.Errors, s.LastError)
	}
	h, _ = m.CandidateHistory()
	for _, c := range h {
		if c.CandidateID == capped.CandidateID && (c.Status != types.Confirmed || c.Review == nil || !c.Review.Association) {
			t.Fatalf("cap-deferred persistent candidate was not associated: %v/%q review %+v", c.Status, c.Reason, c.Review)
		}
	}
	if !state.Associated(capped.FromID, capped.ToID, 1) {
		t.Fatal("no association declared for the cap-deferred candidate")
	}
}
