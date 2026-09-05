// Evolution scenarios — narrated end-to-end demonstrations of how the
// Semantic Map adapts to live telemetry. Each scenario constructs its own
// scaffolded agent (evolutionAgent), drives observations through a
// ScriptedCollector + IngestSample (or, in scenario 6, the Proposer directly),
// and prints checkpoint tables + an EVOLUTION SUMMARY block via t.Logf so
// `go test -v -run TestEvolution` reads like a paper results section.
//
// Hard invariants assert the mechanics that must not regress; the printed
// tables are the human-readable demonstration of the convergence story.

package minimal_test

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"testing"

	"github.com/DiyazY/di-agent/internal/minimal"
	"github.com/DiyazY/di-agent/internal/scripted"
	"github.com/DiyazY/di-agent/pkg/contracts"
	"github.com/DiyazY/di-agent/pkg/semmap"
	"github.com/DiyazY/di-agent/pkg/statemap"
	"github.com/DiyazY/di-agent/pkg/types"
)

// ── Scaffolding ──────────────────────────────────────────────────────────────

type evolutionAgent struct {
	sm        *semmap.SemanticMap
	state     *statemap.Map
	ontology  *minimal.SpecOntology
	proposer  contracts.ProposerContract
	collector *scripted.ScriptedCollector
}

// newEvolutionAgent wires the same edge-minimal stack a production daemon would — a
// state model seeded from the specification, the ontology as the declaration layer, a
// reasoner reading the model, and a proposer — and drives it with a scripted collector.
// If proposer is nil, wires DisabledProposer.
//
// The stack used to include a storage graph and an EMA updater, and the scenarios below
// watched edge weights converge there. They watch the state model's relationships now:
// same story, one model. The visible difference is that a relationship advances on a
// PAIRED observation of both its endpoints rather than on any single sample, so a
// scenario driving only one endpoint moves properties and leaves relations at their
// priors — which is the honest reading, and was the reason for the change.
func newEvolutionAgent(t *testing.T, collector *scripted.ScriptedCollector, proposer contracts.ProposerContract) *evolutionAgent {
	return newEvolutionAgentWithConvergence(t, collector, proposer, 500)
}

// newEvolutionAgentWithConvergence is the same as newEvolutionAgent but lets
// a scenario tighten the convergence threshold so confidence saturates inside
// a shorter observation window (used by the deprecation scenario).
func newEvolutionAgentWithConvergence(t *testing.T, collector *scripted.ScriptedCollector, proposer contracts.ProposerContract, convergence float64) *evolutionAgent {
	t.Helper()
	ontology := minimal.NewOntologyFromSpec(mustSpec())
	// ONE state model, shared by the facade and the reasoner. Two would let a
	// facade-level retirement land in a model the reasoner never reads, which is the
	// bug this fixture had: cost is answered from the reasoner's map and mutated
	// through the facade's.
	state := stateForConvergence(t, convergence)
	reasoner := minimal.NewRuleEngineReasoner(mustSpec(), 0.5, nil, nil)
	reasoner.AttachState(state)
	if proposer == nil {
		proposer = minimal.NewDisabledProposer()
	}

	sm := semmap.New(ontology, reasoner, proposer, minimal.NewDisabledTuner())
	sm.AttachState(state)
	return &evolutionAgent{
		sm:        sm,
		state:     state,
		ontology:  ontology,
		proposer:  proposer,
		collector: collector,
	}
}

// runTicks calls collector.Collect() n times and feeds every sample through
// sm.IngestSample. Returns the total number of samples processed.
func (a *evolutionAgent) runTicks(t *testing.T, n int) int {
	t.Helper()
	total := 0
	for i := 0; i < n; i++ {
		samples, err := a.collector.Collect()
		if err != nil {
			t.Fatalf("collector error at tick %d: %v", i+1, err)
		}
		for _, s := range samples {
			if err := a.sm.IngestSample(s); err != nil {
				t.Fatalf("IngestSample error tick=%d: %v", i+1, err)
			}
			total++
		}
	}
	return total
}

// ── Snapshot helpers ─────────────────────────────────────────────────────────

type edgeSnap struct {
	PropID, From, To, Direction string
	Prior, EMA, Effective       float64
	Confidence                  float64
	NObservations               int
	Delta                       float64 // effective - prior
	Deprecated                  bool
}

func (s edgeSnap) String() string {
	return fmt.Sprintf("%-4s %s→%s(%s)  prior=%.3f ema=%.3f conf=%.3f eff=%.3f Δ=%+0.3f n=%d",
		s.PropID, s.From, s.To, s.Direction, s.Prior, s.EMA, s.Confidence, s.Effective, s.Delta, s.NObservations)
}

// directionString renders a Direction for the narrated tables.
func directionString(d types.Direction) string {
	if d == types.Positive {
		return "+"
	}
	return "-"
}

func snap(t *testing.T, state *statemap.Map, propID string) edgeSnap {
	t.Helper()
	for _, r := range state.Relationships("", "") {
		if r.Label != propID {
			continue
		}
		dir := "+"
		if r.Sign < 0 {
			dir = "-"
		}
		return edgeSnap{
			PropID:        r.Label,
			From:          r.From,
			To:            r.To,
			Direction:     dir,
			Prior:         established(r),
			EMA:           r.Strength,
			Effective:     r.EffectiveOrZero(),
			Confidence:    r.Confidence,
			NObservations: r.NObservations,
			Delta:         r.EffectiveOrZero() - established(r),
			Deprecated:    r.Status == statemap.Retired,
		}
	}
	t.Fatalf("no relationship carries proposition %q", propID)
	return edgeSnap{}
}

// allSnaps returns every edge's snapshot, sorted by PropositionID
// (P1, P2, …, P10, P11, …).
func allSnaps(t *testing.T, state *statemap.Map) []edgeSnap {
	t.Helper()
	rels := state.Relationships("", "")
	out := make([]edgeSnap, 0, len(rels))
	for _, r := range rels {
		if r.Label == "" {
			continue
		}
		out = append(out, snap(t, state, r.Label))
	}
	sort.Slice(out, func(i, j int) bool {
		return propLessNumeric(out[i].PropID, out[j].PropID)
	})
	return out
}

// propLessNumeric sorts proposition IDs by their numeric tail.
func propLessNumeric(a, b string) bool {
	return propNum(a) < propNum(b)
}

func propNum(p string) int {
	n := 0
	for i := 1; i < len(p); i++ {
		c := p[i]
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// emitAdvisories scans edges and prints "ADVISORY" lines via t.Logf when
// (a) confidence > 0.7 AND |Δeff| > 0.25 (suggests deprecation review), or
// (b) confidence > 0.95 (suggests promotion). Returns the count emitted.
func emitAdvisories(t *testing.T, state *statemap.Map, tag string) int {
	t.Helper()
	count := 0
	for _, e := range allSnaps(t, state) {
		if e.Deprecated {
			continue
		}
		if e.Confidence > 0.7 && math.Abs(e.Delta) > 0.25 {
			t.Logf("  ADVISORY [%s]: %s — confidence=%.3f and |Δeff|=%.3f → review for deprecation",
				tag, e.PropID, e.Confidence, math.Abs(e.Delta))
			count++
		} else if e.Confidence > 0.95 {
			t.Logf("  ADVISORY [%s]: %s — confidence=%.3f → promote (high agreement with prior)",
				tag, e.PropID, e.Confidence)
			count++
		}
	}
	return count
}

// printSummary prints the EVOLUTION SUMMARY block at the end of each scenario.
func printSummary(t *testing.T, name string, state *statemap.Map) {
	t.Helper()
	all := allSnaps(t, state)

	adapted := 0
	converged := 0
	var totalAbsDelta float64
	type edgeRow struct {
		PropID string
		Delta  float64
	}
	rows := make([]edgeRow, 0, len(all))
	for _, e := range all {
		if math.Abs(e.Delta) > 0.1 {
			adapted++
		}
		if e.Confidence > 0.9 {
			converged++
		}
		totalAbsDelta += math.Abs(e.EMA - e.Prior)
		rows = append(rows, edgeRow{PropID: e.PropID, Delta: e.Delta})
	}
	avgAbsDelta := totalAbsDelta / float64(len(all))
	advisories := emitAdvisories(t, state, name)

	sort.Slice(rows, func(i, j int) bool { return math.Abs(rows[i].Delta) > math.Abs(rows[j].Delta) })

	t.Logf("=== EVOLUTION SUMMARY: %s ===", name)
	t.Logf("Edges that adapted (|Δeff| > 0.1):   %d of %d", adapted, len(all))
	t.Logf("Edges that converged (conf > 0.9):   %d of %d", converged, len(all))
	t.Logf("Average final |EMA - prior|:         %.3f", avgAbsDelta)
	t.Logf("Edges advisory-flagged:              %d", advisories)
	t.Logf("Top 3 most-changed edges:")
	limit := 3
	if len(rows) < 3 {
		limit = len(rows)
	}
	for i := 0; i < limit; i++ {
		e := snap(t, state, rows[i].PropID)
		t.Logf("  %-4s %s→%s(%s)  Δ=%+0.3f", e.PropID, e.From, e.To, e.Direction, e.Delta)
	}
}

// printRows is a small helper to dump a focused set of edges as a checkpoint
// table.
func printRows(t *testing.T, label string, state *statemap.Map, propIDs []string) {
	t.Helper()
	t.Logf("  %s", label)
	for _, id := range propIDs {
		t.Logf("    %s", snap(t, state, id).String())
	}
}

// ── Scenario 1: cold-to-warm convergence ─────────────────────────────────────

// Scenario 1 is now about what ONE metric can and cannot establish.
//
// It used to assert that 500 constant CPU samples converged every relationship touching
// the construct CPU routes to. That was the endpoint-EMA reading: a single construct's
// magnitude was folded into each incident edge as though it were evidence about the
// association. It is not, and the scenario now demonstrates the distinction — the
// property converges, and the relationships stay exactly where prior knowledge put them,
// because nothing has been observed about any pair.
func TestEvolution_ColdToWarmConvergence(t *testing.T) {
	col := scripted.New("node_1",
		scripted.ConstantPattern{
			Metric: types.CPUUtilization, Value: 0.8, Node: "node_1", StartTick: 0, EndTick: -1,
		},
	)
	a := newEvolutionAgent(t, col, nil)

	construct, routed := a.sm.RoutedConstruct(string(types.CPUUtilization))
	if !routed {
		t.Skip("the loaded spec routes cpu_utilization nowhere")
	}

	t.Log("Scenario 1: cold-to-warm — constant CPU=0.8 for 500 ticks.")
	t.Logf("Tracks the metric property, the construct %s that summarises it, and the", construct)
	t.Log("relationships incident to that construct — which must NOT move: one endpoint")
	t.Log("observed is not an observation of any association.")
	t.Log("")

	focus := allSpecProps()
	checkpoints := []int{0, 20, 100, 250, 500}
	cursor := 0
	for tick := 0; tick < 500; tick++ {
		if cursor < len(checkpoints) && tick == checkpoints[cursor] {
			p, _ := a.state.Property(construct)
			t.Logf("  T=%-4d %s = %.3f (conf %.3f from %d observations)",
				tick, construct, p.Value, p.Confidence, p.NObservations)
			printRows(t, "        relations", a.state, focus)
			cursor++
		}
		a.runTicks(t, 1)
	}

	// The property converged: it holds what the system is doing, at full confidence.
	p, ok := a.state.Property(construct)
	if !ok {
		t.Fatalf("no property for the routed construct %s", construct)
	}
	t.Logf("  T=500 (final) %s = %.3f (conf %.3f from %d observations)",
		construct, p.Value, p.Confidence, p.NObservations)
	if math.Abs(p.Value-0.8) > 0.01 {
		t.Errorf("%s should converge to the observed 0.8; got %.3f", construct, p.Value)
	}
	if p.Confidence < 0.999 {
		t.Errorf("%s confidence should be ≈1.0 after 500 observations; got %.3f",
			construct, p.Confidence)
	}

	// The relationships did not: their strength is still what was seeded, and their
	// confidence still reports that nothing has been learned here.
	for _, id := range focus {
		s := snap(t, a.state, id)
		if s.NObservations != 0 || s.Confidence != 0 {
			t.Errorf("%s advanced to n=%d conf=%.3f on one endpoint's observations; a "+
				"strength must wait for pairs", id, s.NObservations, s.Confidence)
		}
		if s.Effective != s.Prior {
			t.Errorf("%s effective %.3f drifted from its prior %.3f with no evidence",
				id, s.Effective, s.Prior)
		}
	}

	printSummary(t, "cold-to-warm", a.state)
}

// ── Scenario 2: regime change ────────────────────────────────────────────────

func TestEvolution_RegimeChange(t *testing.T) {
	// Both endpoints are driven, and driven differently. A relationship advances on a
	// PAIR, so stepping one construct alone would leave every relationship at zero
	// observations — which is Scenario 1's subject, not this one. The pressure series
	// steps with resource but on a different amplitude, so the association has
	// something to estimate rather than being a copy of one signal.
	col := scripted.New("node_1",
		scripted.NewStepPattern(types.CPUUtilization, "node_1", []scripted.StepPoint{
			{Tick: 0, Value: 0.3},
			{Tick: 300, Value: 0.85},
			{Tick: 600, Value: 0.3},
		}),
		scripted.SineWavePattern{Metric: types.CPUPressureRatio, Node: "node_1",
			Mid: 0.4, Amp: 0.3, PeriodTicks: 60, EndTick: -1},
	)
	a := newEvolutionAgent(t, col, nil)

	t.Log("Scenario 2: regime change — step pattern 0.3 → 0.85 → 0.3 over 800 ticks.")
	t.Log("Expected: EMA tracks each regime; the third regime drags EMA back toward 0.3.")
	t.Log("")

	focus := allSpecProps()
	checkpoints := []int{0, 100, 300, 400, 600, 800}
	cursor := 0
	for tick := 0; tick <= 800; tick++ {
		if cursor < len(checkpoints) && tick == checkpoints[cursor] {
			printRows(t, fmt.Sprintf("T=%d  regime-target=%.2f", tick, regimeAt(tick)), a.state, focus)
			cursor++
		}
		if tick < 800 {
			a.runTicks(t, 1)
		}
	}

	// Invariants, read from the CONSTRUCT rather than from a relationship.
	//
	// This scenario is about a value following its regime, and a relationship's
	// strength is not that value: since the endpoint estimator was removed a
	// relationship holds |r| over a window of pairs — an association, dimensionless and
	// with no reason to approach 0.3 whatever the resource series does. The assertions
	// below used to read a relationship and pass only because its strength was then an
	// EMA of one endpoint's magnitude.
	rc := mustSpec().CostModel.ResourceConstruct
	final, ok := a.state.Property(rc)
	if !ok {
		t.Fatalf("construct %s absent", rc)
	}
	if final.NObservations == 0 {
		t.Fatalf("construct %s was never observed", rc)
	}
	if final.Value > 0.55 {
		t.Errorf("at T=800 %s should be moving back toward the third regime's 0.3; got %.3f",
			rc, final.Value)
	}
	if final.Value < 0.25 {
		t.Errorf("at T=800 %s undershot the third regime's 0.3; got %.3f", rc, final.Value)
	}

	printSummary(t, "regime-change", a.state)
}

func regimeAt(tick int) float64 {
	switch {
	case tick < 300:
		return 0.3
	case tick < 600:
		return 0.85
	default:
		return 0.3
	}
}

// ── Scenario 3: conflict-pair coupling ───────────────────────────────────────

func TestEvolution_ConflictPairCoupling(t *testing.T) {
	col := scripted.New("node_1",
		scripted.ConstantPattern{
			Metric: types.CPUUtilization, Value: 0.7, Node: "node_1", StartTick: 0, EndTick: -1,
		},
	)
	a := newEvolutionAgent(t, col, nil)

	idA, idB, ok := conflictPair()
	if !ok {
		t.Skip("the loaded specification declares no conflict pair — two propositions " +
			"over one pair with opposite signs. The pair that used to serve here was " +
			"not one: both stated the same mechanism, against outcome measures of " +
			"opposite polarity, so one of them asserted a sign its machine never showed.")
	}
	t.Logf("Scenario 3: conflict-pair coupling — %s and %s on the same pair.", idA, idB)
	t.Log("Both must receive identical EMA updates from one observation but contribute opposite signs in CostOfAction.")
	t.Log("")

	// Each half starts with its own prior (P2=0.4, P3=0.5). The EMA formula
	// converges geometrically: after k updates with alpha=0.2, the gap shrinks
	// by 0.8^k. At T=20 the gap is already ~1e-2; by T=50 it's ~1e-5.
	checkpoints := []int{0, 1, 50, 200, 500}
	cursor := 0
	for tick := 0; tick <= 500; tick++ {
		if cursor < len(checkpoints) && tick == checkpoints[cursor] {
			p2 := snap(t, a.state, idA)
			p3 := snap(t, a.state, idB)
			t.Logf("  T=%d", tick)
			t.Logf("    %s", p2)
			t.Logf("    %s", p3)
			// Confidence and NObservations must match exactly — they are
			// driven by event counting, not by the observation value.
			if p2.Confidence != p3.Confidence {
				t.Errorf("  T=%d: P2.conf=%.6f != P3.conf=%.6f", tick, p2.Confidence, p3.Confidence)
			}
			if p2.NObservations != p3.NObservations {
				t.Errorf("  T=%d: P2.n=%d != P3.n=%d", tick, p2.NObservations, p3.NObservations)
			}
			// EMAs converge geometrically. At T=0 they equal their respective
			// priors; from T=50 onward they are indistinguishable to 4 dp.
			if tick >= 50 && math.Abs(p2.EMA-p3.EMA) > 1e-4 {
				t.Errorf("  T=%d: P2.EMA=%.6f and P3.EMA=%.6f should have converged",
					tick, p2.EMA, p3.EMA)
			}
			cursor++
		}
		if tick < 500 {
			a.runTicks(t, 1)
		}
	}

	// The coupling above is a property of the construct graph's edges, and that is
	// where this scenario ends. Cost is answered from the state model, which this
	// fixture does not attach, so asserting a cost here would be asserting on a model
	// nothing in this test feeds. What a conflict pair does inside the state model —
	// separating on evidence rather than moving together — is covered by
	// TestRelationalObservationsReachTheStateModel in pkg/semmap.

	printSummary(t, "conflict-pair", a.state)
}

// ── Scenario 4: multi-construct stress ───────────────────────────────────────

// Scenario 4 asks what a system with several moving parts lets the map learn.
//
// The patterns vary, and that is the point: the four metrics used to be constants,
// which under endpoint EMA still advanced every incident edge — a strength climbing to
// full confidence on a signal that never changed. A constant series carries no
// association, so the estimator now declines to move on it, and a scenario that wants
// relationships to learn has to drive something that actually varies.
func TestEvolution_MultiConstructStress(t *testing.T) {
	// Drive one varying pattern per metric the specification actually routes, phase-
	// shifted so different constructs move against one another. Naming metrics
	// literally is what broke this scenario when the graph was scoped down: it fed
	// pod-startup and network receive, both of which stopped being routed, so nothing
	// reached the pressure construct and no relationship could form a pair.
	routes := mustSpec().MetricRouting
	if len(routes) < 2 {
		t.Skipf("specification routes %d metrics; this scenario needs at least two", len(routes))
	}
	patterns := make([]scripted.Pattern, 0, len(routes))
	for i, r := range routes {
		patterns = append(patterns, scripted.SineWavePattern{
			Metric: types.MetricType(r.MetricType), Node: "node_1",
			Mid: 0.5, Amp: 0.3, PeriodTicks: 40, StartTick: int64(i * 3), EndTick: -1,
		})
	}
	col := scripted.New("node_1", patterns...)
	a := newEvolutionAgent(t, col, nil)

	t.Log("Scenario 4: multi-construct stress — four varying patterns.")
	t.Log("A relationship can only learn when BOTH its endpoints are observed, so what")
	t.Log("moves is bounded by the specification's routing.")
	t.Log("")

	a.runTicks(t, 500)

	// Which constructs the specification can actually observe here.
	observed := map[string]bool{}
	for _, r := range mustSpec().MetricRouting {
		if p, ok := a.state.Property(r.MetricType); ok && p.NObservations > 0 {
			observed[r.ConstructID] = true
		}
	}
	t.Logf("  observed constructs: %v", sortedKeys(observed))

	all := allSnaps(t, a.state)
	t.Log("  Final state of every relationship:")
	for _, e := range all {
		t.Logf("    %s", e)
	}

	// A relationship whose endpoints were both observed had the chance to learn; one
	// that reaches an unobserved construct must not have moved, because nothing about
	// it was measured.
	learned := 0
	for _, e := range all {
		bothObserved := observed[e.From] && observed[e.To]
		if bothObserved && e.NObservations > 0 {
			learned++
		}
		if !bothObserved && e.NObservations != 0 {
			t.Errorf("%s reaches an unobserved construct but recorded %d observations — "+
				"evidence appeared for a pair that was never both observed",
				e.PropID, e.NObservations)
		}
	}
	if learned == 0 {
		t.Error("no relationship between two observed constructs learned anything from 500 " +
			"ticks of varying telemetry")
	}
	t.Logf("  relationships that learned: %d of %d", learned, len(all))

	printSummary(t, "multi-construct", a.state)
}

// sortedKeys returns a map's keys in order, for stable narration.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ── Scenario 5: deprecation from contradiction ───────────────────────────────

func TestEvolution_DeprecationFromContradiction(t *testing.T) {
	// A contradiction now means what it says: the two endpoints move together in the
	// direction OPPOSITE to the one the proposition declares. The scenario used to drive
	// a single low signal and call the resulting drift a contradiction, which it was not
	// — one endpoint sitting low says nothing about whether the pair is related.
	spec := mustSpec()
	target, aMetric, bMetric := contradictablePair(t)
	rel := relFor(t, stateFor(t), target)
	t.Logf("Scenario 5: deprecation from contradiction (%s: %s→%s, declared %+d).",
		target, rel.From, rel.To, rel.Sign)

	// Anti-correlate the two endpoints relative to the declared sign, so the evidence is
	// evidence AGAINST this proposition rather than a weaker version of it.
	amp := 0.3
	if rel.Sign < 0 {
		amp = -0.3 // a negative claim is contradicted by the endpoints moving together
	}
	col := scripted.New("node_1",
		scripted.SineWavePattern{Metric: types.MetricType(aMetric), Node: "node_1",
			Mid: 0.5, Amp: 0.3, PeriodTicks: 40, EndTick: -1},
		scripted.SineWavePattern{Metric: types.MetricType(bMetric), Node: "node_1",
			Mid: 0.5, Amp: -amp, PeriodTicks: 40, EndTick: -1},
	)
	// Tight convergence threshold so 200 pairs saturate confidence and the
	// |Δ|-plus-confidence advisor threshold can fire within the scenario.
	a := newEvolutionAgentWithConvergence(t, col, nil, 150)
	_ = spec

	t.Log("Evidence contradicts the declared direction, so the learned strength falls to")
	t.Log("zero while confidence rises — which is what makes the advisory meaningful.")
	t.Log("")

	before, _ := a.sm.CostOfAction("pod-scheduling", "node_1")
	t.Logf("  before: graph path length = %d, resource_cost=%.3f, confidence=%.3f",
		len(before.GraphPathUsed), before.ResourceCost, before.Confidence)

	advisoryAt := -1
	for tick := 0; tick < 200; tick++ {
		a.runTicks(t, 1)
		if (tick+1)%50 == 0 {
			n := emitAdvisories(t, a.state, fmt.Sprintf("T=%d", tick+1))
			if n > 0 && advisoryAt < 0 {
				advisoryAt = tick + 1
			}
		}
	}

	contradicted := snap(t, a.state, target)
	t.Logf("  %s before deprecation: %s", target, contradicted)
	if contradicted.NObservations == 0 {
		t.Fatalf("%s never paired, so nothing could contradict it", target)
	}
	if contradicted.EMA > 0.1 {
		t.Errorf("%s learned strength %.3f from evidence of the opposite sign; evidence "+
			"against a claim is not a weaker version of it", target, contradicted.EMA)
	}
	if advisoryAt < 0 {
		t.Error("expected at least one advisory line to fire before deprecation")
	}

	// Operator action: deprecate through the FACADE, not the ontology. The facade
	// retires the relationship in the state model as well, which is what every answer
	// reads; reaching past it changes what Propositions() reports and no decision.
	if err := a.sm.Deprecate(target, "evidence contradicts the declared direction"); err != nil {
		t.Fatalf("deprecate failed: %v", err)
	}
	t.Logf("  Operator deprecates %s.", target)

	after, _ := a.sm.CostOfAction("pod-scheduling", "node_1")
	t.Logf("  after:  graph path length = %d, resource_cost=%.3f, confidence=%.3f",
		len(after.GraphPathUsed), after.ResourceCost, after.Confidence)

	if len(after.GraphPathUsed) > len(before.GraphPathUsed) {
		t.Errorf("graph path grew after a deprecation: before=%d after=%d",
			len(before.GraphPathUsed), len(after.GraphPathUsed))
	}
	// Read through the facade, not the declaration layer. The ontology holds no
	// deprecation flag of its own — the flag on /propositions is overlaid from whether
	// the relationship is retired — so reading it directly is how a caller would
	// convince themselves nothing had happened.
	props, _ := a.sm.Propositions()
	foundDeprecated := false
	for _, p := range props {
		if p.PropositionID == target && p.Deprecated {
			foundDeprecated = true
		}
	}
	if !foundDeprecated {
		t.Errorf("%s not reported deprecated after Deprecate()", target)
	}
	// The relationship must still be retrievable: a soft delete withdraws a claim from
	// reasoning and keeps its record, so a decision taken before it stays reconstructible.
	stillThere := false
	for _, r := range a.state.Relationships("", "") {
		if r.Label == target {
			stillThere = true
			if r.Status != statemap.Retired {
				t.Errorf("%s is still %s after deprecation; it would keep contributing to "+
					"every answer", r.Label, r.Status)
			}
			break
		}
	}
	if !stillThere {
		t.Error("the relationship vanished from the model — a soft delete must preserve it")
	}

	printSummary(t, "deprecation", a.state)
}

// contradictablePair finds a proposition whose two endpoint constructs both have routed
// metrics, so evidence can be produced for or against it, and returns the proposition
// with those two metrics. A proposition whose endpoints cannot both be observed is not a
// claim this deployment can contradict.
func contradictablePair(t *testing.T) (propID, aMetric, bMetric string) {
	t.Helper()
	spec := mustSpec()
	routed := map[string]string{}
	for _, r := range spec.MetricRouting {
		if _, seen := routed[r.ConstructID]; !seen {
			routed[r.ConstructID] = r.MetricType
		}
	}
	for _, p := range spec.Propositions {
		am, aok := routed[p.FromConstruct]
		bm, bok := routed[p.ToConstruct]
		if aok && bok {
			return p.PropositionID, am, bm
		}
	}
	t.Skip("no proposition in this spec has both endpoints routed to metrics")
	return "", "", ""
}

// relFor returns the relationship carrying a proposition ID.
func relFor(t *testing.T, state *statemap.Map, propID string) statemap.Relationship {
	t.Helper()
	for _, r := range state.Relationships("", "") {
		if r.Label == propID {
			return r
		}
	}
	t.Fatalf("no relationship carries proposition %q", propID)
	return statemap.Relationship{}
}

// ── Scenario 6: propose-then-confirm ─────────────────────────────────────────

func TestEvolution_NewEdgeProposeConfirm(t *testing.T) {
	col := scripted.New("node_1") // collector unused — the proposer is driven directly
	ontology := minimal.NewOntologyFromSpec(mustSpec())
	proposer := minimal.NewMICorrelationProposer(minimal.LookupOntology(ontology), 0.8, 30, 200, 0)

	// Verify MU→PS is a free pair (no proposition in the bootstrap).
	props, _ := ontology.Propositions()
	for _, p := range props {
		if p.FromConstruct == "MU" && p.ToConstruct == "PS" {
			t.Fatalf("MU→PS is already in the bootstrap (P%s); pick another pair", p.PropositionID)
		}
	}
	before := len(props)

	a := newEvolutionAgent(t, col, proposer)
	// Swap in the proposer-aware ontology so AddValidatedProposition routes
	// through the same instance the proposer sees.
	a.ontology = ontology
	state := stateFor(t)
	r := minimal.NewRuleEngineReasoner(mustSpec(), 0.5, nil, nil)
	r.AttachState(state)
	a.sm = semmap.New(ontology, r, proposer, minimal.NewDisabledTuner())
	a.sm.AttachState(state)
	a.state = state

	t.Log("Scenario 6: propose-then-confirm.")
	t.Log("Drive 150 strongly correlated MU↔PS observations directly to the proposer;")
	t.Log("verify a pending candidate is emitted, confirm it, see backbone grow by 1.")
	t.Log("")

	rng := rand.New(rand.NewSource(42))
	for tick := 0; tick < 150; tick++ {
		base := 0.5 + 0.3*math.Sin(float64(tick)/20)
		noise := rng.NormFloat64() * 0.03
		valueMU := base
		valuePS := base + noise // ~95%+ correlated
		if err := proposer.Observe("MU", "PS", valueMU, valuePS); err != nil {
			t.Fatal(err)
		}
	}

	cands, err := a.sm.PendingCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("expected 1 pending candidate; got %d", len(cands))
	}
	cand := cands[0]
	t.Logf("  candidate detected: %s %s→%s(%s)  |r|=%.3f  n=%d",
		cand.CandidateID, cand.FromID, cand.ToID, directionString(cand.Direction), cand.MIScore, cand.NObservations)
	if cand.CandidateID != "MU->PS" {
		t.Errorf("unexpected candidate id: %s", cand.CandidateID)
	}
	if cand.Direction != types.Positive {
		t.Errorf("expected positive direction; got %v", cand.Direction)
	}

	// Confirm.
	if err := a.sm.ConfirmCandidate(cand.CandidateID); err != nil {
		t.Fatalf("Confirm error: %v", err)
	}

	afterProps, _ := a.sm.Propositions()
	if len(afterProps) != before+1 {
		t.Errorf("propositions: before=%d after=%d (expected before+1)", before, len(afterProps))
	}

	// New proposition is visible with the proposer-mi evidence tag.
	var newProp *types.Proposition
	for _, p := range afterProps {
		if p.FromConstruct == "MU" && p.ToConstruct == "PS" {
			newProp = p
			break
		}
	}
	if newProp == nil {
		t.Fatal("could not locate new MU→PS proposition")
	}
	if len(newProp.EvidenceSources) != 1 || newProp.EvidenceSources[0] != "proposer-mi" {
		t.Errorf("evidence sources should be [proposer-mi]; got %v", newProp.EvidenceSources)
	}

	// Post-confirm: no more pending candidates.
	postCands, _ := a.sm.PendingCandidates()
	if len(postCands) != 0 {
		t.Errorf("expected 0 pending candidates after Confirm; got %d", len(postCands))
	}

	// History contains the candidate with Confirmed status.
	hist, _ := proposer.GetHistory()
	var confirmed bool
	for _, h := range hist {
		if h.CandidateID == cand.CandidateID && h.Status == types.Confirmed {
			confirmed = true
		}
	}
	if !confirmed {
		t.Error("proposer history does not record the candidate as Confirmed")
	}

	t.Logf("  Confirmed → proposition added: %s %s→%s(%s) prior=%.3f",
		newProp.PropositionID, newProp.FromConstruct, newProp.ToConstruct,
		directionString(newProp.Direction), newProp.PriorStrength)
	t.Logf("  Propositions: %d → %d", before, len(afterProps))

	t.Logf("=== EVOLUTION SUMMARY: propose-confirm ===")
	t.Logf("Candidates emitted:    1 (%s, |r|=%.3f, n=%d)", cand.CandidateID, cand.MIScore, cand.NObservations)
	t.Logf("Candidates confirmed:  1")
	t.Logf("Backbone size change:  %d → %d (+1)", before, len(afterProps))
	t.Logf("New proposition:       %s  evidence=%v", newProp.PropositionID, newProp.EvidenceSources)
}

// established reports the long-run layer as a plain float for snapshot comparison,
// zero when the machine has not established one yet.
func established(r statemap.Relationship) float64 {
	if r.Established == nil {
		return 0
	}
	return *r.Established
}

// ── Scenario: a subject's whole life ──────────────────────────────────────────
//
// A subject arrives, is admitted, correlates with node pressure, is proposed,
// confirmed, asked about counterfactually, leaves, goes stale, retires — taking its
// relationship with it — and returns. Printed as checkpoints so `go test -v -run
// TestEvolution_SubjectLifecycle` reads like the demonstration it is.
func TestEvolution_SubjectLifecycle(t *testing.T) {
	depart, ret := 2400, 4200
	sc := &scripted.Scenario{
		Name: "lifecycle", Seed: 11, TickSeconds: 10, DurationSeconds: 5400, Noise: 0.01,
		Node: map[string]scripted.Coupling{
			"node_cpu": {Coupling: "sum", Base: 0.10, Of: "cpu_utilization"},
			"pressure": {Coupling: "sum", Base: 0.05, Of: "cpu_utilization"},
		},
		Subjects: []scripted.SubjectSpec{{
			ID: "pod:a", Arrive: 0, Depart: &depart, Return: &ret,
			Properties: map[string]scripted.PropertySpec{"cpu_utilization": {Pattern: "sine", Min: 0.1, Max: 0.6, Period: 600}},
		}},
		Expect: scripted.Expectations{
			AdmittedWithinTicks: 1, StaleWithinSeconds: 120, RetiredWithinSeconds: 600,
			Candidates: []scripted.ExpectedCandidate{{From: "cpu_utilization@pod:a", To: "pressure", Sign: 1, WithinSeconds: 1200}},
		},
	}
	if err := sc.Validate(); err != nil {
		t.Fatal(err)
	}

	checkpoints := map[int64]string{60: "learning", 120: "discovered", 200: "asked", 250: "departed", 300: "retired", 430: "returned"}
	var decisionID string
	r := driveScenario(t, sc, func(tick int64, r *scenarioRun) {
		label, ok := checkpoints[tick]
		if !ok {
			return
		}
		c := r.state.Census()
		p, _ := r.state.Property("cpu_utilization@pod:a")
		rel, _ := r.state.Relationship(statemap.RelationshipID("cpu_utilization@pod:a", "pressure", "discovered"))
		eff, _ := rel.Effective()
		t.Logf("T=%4d %-10s subjects=%d props(active/stale/retired)=%d/%d/%d discovered=%d | pod:a cpu=%.3f %s | edge %s eff=%.3f",
			tick, label, c.Subjects, c.PropertiesActive, c.PropertiesStale, c.PropertiesRetired, c.Discovered,
			p.Value, p.Status, rel.Status, eff)
		if label == "asked" {
			res := r.state.Estimate(statemap.EstimateRequest{Target: "pressure", Assume: map[string]float64{"cpu_utilization@pod:a": 0.6}})
			decisionID = res.DecisionID
			if res.Hypothetical == nil {
				t.Fatalf("T=%d: no hypothetical — the candidate was not confirmed by the time the question was asked; caveats=%v", tick, res.Caveats)
			}
			t.Logf("      estimate pressure if pod:a cpu=0.6 → projected %.3f (level %.3f, delta %+.3f); caveats=%d; decision %s",
				res.Hypothetical.ProjectedLevel, res.Answer.Level, res.Hypothetical.Delta, len(res.Caveats), res.DecisionID)
		}
	})
	assertScenario(t, r)

	// The decision made while the subject was alive replays after it retired and returned.
	var replayed bool
	for _, e := range r.state.Journal().Events(0, 0) {
		if e.Decision != nil && e.Decision.ID == decisionID {
			replayed = true
			if e.Decision.Assumptions["cpu_utilization@pod:a"] != 0.6 {
				t.Errorf("replayed decision lost its assumption: %v", e.Decision.Assumptions)
			}
		}
	}
	if !replayed {
		t.Errorf("decision %s did not survive the subject's retirement and return", decisionID)
	}
	rel, _ := r.state.Relationship(statemap.RelationshipID("cpu_utilization@pod:a", "pressure", "discovered"))
	t.Logf("EVOLUTION SUMMARY: admitted@%d stale@%d retired@%d revived@%d candidate@%d; edge now %s (%s)",
		r.firstSeen["cpu_utilization@pod:a"], r.stale["cpu_utilization@pod:a"], r.retired["cpu_utilization@pod:a"],
		r.revived["cpu_utilization@pod:a"], r.candidate["cpu_utilization@pod:a->pressure"], rel.Status, rel.Provenance)
}
