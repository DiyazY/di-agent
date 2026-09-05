// internal/minimal/tests/scenario_files_test.go
package minimal_test

import (
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/internal/minimal"
	"github.com/DiyazY/di-agent/internal/scripted"
	"github.com/DiyazY/di-agent/pkg/profiles"
	"github.com/DiyazY/di-agent/pkg/semmap"
	"github.com/DiyazY/di-agent/pkg/statemap"
)

// scenarioRun is what driving a scenario through the daemon stack leaves behind.
type scenarioRun struct {
	sc        *scripted.Scenario
	state     *statemap.Map
	sm        *semmap.SemanticMap
	collector *scripted.SystemScript
	ticks     int64

	firstSeen  map[string]int64 // property id → tick first present in the map
	stale      map[string]int64 // property id → tick first reported stale
	retired    map[string]int64 // property id → tick first reported retired
	revived    map[string]int64 // property id → tick first active again after retirement
	candidate  map[string]int64 // "from->to" → tick first pending
	reproposed map[string]int64 // "from->to" → tick first pending again after a confirmation
	confirmed  map[string]bool
}

func (r *scenarioRun) subjectProps() []string {
	var ids []string
	for _, sub := range r.sc.Subjects {
		for name := range sub.Properties {
			ids = append(ids, name+"@"+sub.ID)
		}
	}
	return ids
}

// driveScenario runs the scenario through the same stack the daemon assembles, with an
// injected clock that follows the collector's timestamps, sweeping every tick and
// confirming every candidate the scenario expects as soon as it appears.
func driveScenario(t *testing.T, sc *scripted.Scenario, narrate func(tick int64, r *scenarioRun)) *scenarioRun {
	t.Helper()
	tick := time.Duration(sc.TickSeconds) * time.Second
	staleAfter := time.Duration(sc.Expect.StaleWithinSeconds) * time.Second
	if staleAfter <= 0 {
		staleAfter = 2 * time.Minute
	}
	retireAfter := time.Duration(sc.Expect.RetiredWithinSeconds) * time.Second
	if retireAfter <= 0 {
		retireAfter = 10 * time.Minute
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start
	state := statemap.New(statemap.Config{
		Owner: "sim", StaleAfter: staleAfter, RetireAfter: retireAfter,
		ConvergenceObservations: 50, Alpha: 0.2, AdmitUnknown: true, Learn: true,
		LearnConfig: statemap.LearnConfig{PairWindowSeconds: 2 * sc.TickSeconds, MinSupport: 8, Window: 60},
	}, statemap.NewJournal(0))
	state.SetClock(func() time.Time { return now })
	spec := mustSpec()
	if _, err := profiles.SeedStateMap(state, spec, "", ""); err != nil {
		t.Fatal(err)
	}
	ontology := minimal.NewOntologyFromSpec(spec)
	proposer := minimal.NewMICorrelationProposer(state, 0.7, 30, 120, 2*tick)
	reasoner := minimal.NewRuleEngineReasoner(spec, 0.5, nil, nil)
	reasoner.AttachState(state)
	sm := semmap.New(ontology, reasoner, proposer, minimal.NewDisabledTuner())
	sm.SetIdentity("sim", false)
	sm.AttachState(state)
	collector := scripted.NewSystemScript("sim", sc, start)

	r := &scenarioRun{sc: sc, state: state, sm: sm, collector: collector,
		firstSeen: map[string]int64{}, stale: map[string]int64{}, retired: map[string]int64{},
		revived: map[string]int64{}, candidate: map[string]int64{}, reproposed: map[string]int64{}, confirmed: map[string]bool{}}
	expected := map[string]scripted.ExpectedCandidate{}
	for _, c := range sc.Expect.Candidates {
		expected[c.From+"->"+c.To] = c
	}

	r.ticks = int64(sc.DurationSeconds / sc.TickSeconds)
	for i := int64(1); i <= r.ticks; i++ {
		samples, err := collector.Collect()
		if err != nil {
			t.Fatal(err)
		}
		now = collector.At(i)
		for _, s := range samples {
			if err := sm.IngestSample(s); err != nil {
				t.Fatalf("tick %d: ingest %s: %v", i, s.MetricType, err)
			}
		}
		state.Sweep()
		for _, id := range r.subjectProps() {
			p, ok := state.Property(id)
			if !ok {
				continue
			}
			if _, seen := r.firstSeen[id]; !seen {
				r.firstSeen[id] = i
			}
			switch p.Status {
			case statemap.Stale:
				if _, done := r.stale[id]; !done {
					r.stale[id] = i
				}
			case statemap.Retired:
				if _, done := r.retired[id]; !done {
					r.retired[id] = i
				}
			case statemap.Active:
				if _, wasRetired := r.retired[id]; wasRetired {
					if _, done := r.revived[id]; !done {
						r.revived[id] = i
					}
				}
			}
		}
		cs, _ := sm.PendingCandidates()
		for _, c := range cs {
			key := c.FromID + "->" + c.ToID
			if _, seen := r.candidate[key]; !seen {
				r.candidate[key] = i
			} else if r.confirmed[key] {
				if _, done := r.reproposed[key]; !done {
					r.reproposed[key] = i
				}
			}
			// Confirmed every time it is proposed: after a departure the relationship
			// retired and the candidate was forgotten, so a return proposes it afresh
			// and confirming it again is the revival the lifecycle promises.
			if _, want := expected[key]; want {
				if err := sm.ConfirmCandidate(c.CandidateID); err != nil {
					t.Fatalf("tick %d: confirm %s: %v", i, key, err)
				}
				r.confirmed[key] = true
			}
		}
		if narrate != nil {
			narrate(i, r)
		}
	}
	return r
}

func TestScenarioFiles(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "..", "scenarios")
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(files) == 0 {
		t.Fatalf("no scenario files under %s", dir)
	}
	for _, f := range files {
		f := f
		t.Run(filepath.Base(f), func(t *testing.T) {
			sc, err := scripted.LoadScenario(f)
			if err != nil {
				t.Fatal(err)
			}
			assertScenario(t, driveScenario(t, sc, nil))
		})
	}
}

// assertScenario checks the scenario's expect block against what the run recorded.
func assertScenario(t *testing.T, r *scenarioRun) {
	t.Helper()
	sc := r.sc
	tickOf := func(sec int) int64 { return int64(sec / sc.TickSeconds) }
	within := func(sec int) int64 { return tickOf(sec) + 2 } // sweep granularity

	// Lifecycle.
	admitTicks := int64(sc.Expect.AdmittedWithinTicks)
	if admitTicks <= 0 {
		admitTicks = 1
	}
	for _, sub := range sc.Subjects {
		for name := range sub.Properties {
			id := name + "@" + sub.ID
			seen, ok := r.firstSeen[id]
			if !ok {
				t.Errorf("%s was never admitted", id)
				continue
			}
			if arrive := tickOf(sub.Arrive) + 1; seen-arrive > admitTicks {
				t.Errorf("%s admitted at tick %d, %d ticks after arrival; want ≤ %d", id, seen, seen-arrive, admitTicks)
			}
			if sub.Depart != nil {
				dep := tickOf(*sub.Depart)
				if s, ok := r.stale[id]; !ok || s-dep > within(sc.Expect.StaleWithinSeconds) {
					t.Errorf("%s stale at tick %v after departing at %d; want within %ds", id, r.stale[id], dep, sc.Expect.StaleWithinSeconds)
				}
				if rt, ok := r.retired[id]; !ok || rt-dep > within(sc.Expect.RetiredWithinSeconds) {
					t.Errorf("%s retired at tick %v after departing at %d; want within %ds", id, r.retired[id], dep, sc.Expect.RetiredWithinSeconds)
				}
				if sub.Return != nil {
					if rv, ok := r.revived[id]; !ok || rv < tickOf(*sub.Return) {
						t.Errorf("%s not revived after returning at %ds (revived tick %v)", id, *sub.Return, r.revived[id])
					}
				}
			}
		}
	}

	// Discovery.
	for _, c := range sc.Expect.Candidates {
		key := c.From + "->" + c.To
		at, ok := r.candidate[key]
		if !ok || at > tickOf(c.WithinSeconds) {
			t.Errorf("candidate %s: first seen at tick %v; want within %ds", key, r.candidate[key], c.WithinSeconds)
			continue
		}
		rel, ok := r.state.Relationship(statemap.RelationshipID(c.From, c.To, "discovered"))
		if !ok || rel.Sign != c.Sign || rel.Provenance != statemap.Discovered {
			t.Errorf("candidate %s confirmed as %+v; want a Discovered relationship with sign %+d", key, rel, c.Sign)
		}
		if ok && rel.Status != statemap.Active {
			t.Errorf("candidate %s ends the run %s; a confirmed relationship whose endpoints are present must be active", key, rel.Status)
		}
		if c.ReproposedAfterReturn {
			var returnTick int64
			for _, sub := range sc.Subjects {
				if strings.HasSuffix(c.From, "@"+sub.ID) && sub.Return != nil {
					returnTick = tickOf(*sub.Return)
				}
			}
			if rp, ok := r.reproposed[key]; !ok || rp < returnTick {
				t.Errorf("candidate %s was not proposed again after its subject returned at tick %d (re-proposed tick %v); the retired edge must be re-earned, not remembered",
					key, returnTick, r.reproposed[key])
			}
		}
	}
	history, _ := r.sm.CandidateHistory()
	for _, subj := range sc.Expect.NoCandidatesFrom {
		for _, c := range history {
			if strings.HasSuffix(c.FromID, "@"+subj) || strings.HasSuffix(c.ToID, "@"+subj) {
				t.Errorf("subject %s must not be proposed; got candidate %s (%v)", subj, c.CandidateID, c.Status)
			}
		}
	}

	// Counterfactuals, against the model's truth at the end of the run.
	lastSec := int(r.ticks) * sc.TickSeconds
	spec := mustSpec()
	for _, cf := range sc.Expect.Counterfactuals {
		res := r.state.Estimate(statemap.EstimateRequest{Target: cf.Target, Assume: cf.Assume})
		if res.Err != "" || res.Hypothetical == nil {
			t.Errorf("counterfactual on %s: %s (hypothetical=%v)", cf.Target, res.Err, res.Hypothetical)
			continue
		}
		truth := spec.NormalizeForConstruct(cf.Target, r.collector.NodeValues(lastSec, cf.Assume)[cf.Target])
		errAbs := math.Abs(res.Hypothetical.ProjectedLevel - truth)
		var slope bool
		for _, cv := range res.Caveats {
			slope = slope || strings.Contains(cv, "not a fitted slope")
		}
		switch cf.Regime {
		case "linear":
			if errAbs > cf.Tolerance {
				t.Errorf("linear counterfactual on %s under %v: projected %.4f, truth %.4f, error %.4f > tolerance %.4f",
					cf.Target, cf.Assume, res.Hypothetical.ProjectedLevel, truth, errAbs, cf.Tolerance)
			}
		case "saturated":
			if errAbs < cf.MinError {
				t.Errorf("saturated counterfactual on %s under %v: error %.4f < min_error %.4f — the limit this scenario documents did not appear",
					cf.Target, cf.Assume, errAbs, cf.MinError)
			}
		}
		if !slope {
			t.Errorf("counterfactual on %s: the standing slope caveat is missing", cf.Target)
		}
		// Traceability: the decision replays with the same inputs and assumptions.
		var found bool
		for _, e := range r.state.Journal().Events(0, 0) {
			if e.Decision != nil && e.Decision.ID == res.DecisionID {
				found = true
				if len(e.Decision.Assumptions) != len(cf.Assume) || len(e.Decision.PropertiesRead) == 0 {
					t.Errorf("decision %s replays with %d assumptions and %d properties; want %d and >0",
						res.DecisionID, len(e.Decision.Assumptions), len(e.Decision.PropertiesRead), len(cf.Assume))
				}
			}
		}
		if !found {
			t.Errorf("decision %s is not in the journal", res.DecisionID)
		}
	}
}
