package semmap_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/internal/minimal"
	"github.com/DiyazY/di-agent/pkg/domain"
	"github.com/DiyazY/di-agent/pkg/profiles"
	"github.com/DiyazY/di-agent/pkg/semmap"
	"github.com/DiyazY/di-agent/pkg/statemap"
	"github.com/DiyazY/di-agent/pkg/types"
)

// TestIngestSample_RejectsForeignSamplesInSelfScope pins the property that makes the
// map node-local. Without it a daemon silently accumulates whatever telemetry arrives,
// and its relationships become means over machines that may be different physical
// systems — the testbed pairs an x86 control-plane host with Cortex-A72 workers, which
// do not share a resource-to-pressure relation. The whole-testbed replay is the
// legitimate exception and has to ask for it.
func TestIngestSample_RejectsForeignSamplesInSelfScope(t *testing.T) {
	sm, _ := newMap(t)
	sm.SetIdentity("node_1", false)

	own := &types.MetricSample{NodeID: "node_1", MetricType: types.CPUUtilization,
		Value: 0.4, TimestampUnix: 1000, EventID: "own-1"}
	if err := sm.IngestSample(own); err != nil {
		t.Fatalf("own sample rejected: %v", err)
	}

	foreign := &types.MetricSample{NodeID: "node_2", MetricType: types.CPUUtilization,
		Value: 0.9, TimestampUnix: 1001, EventID: "foreign-1"}
	err := sm.IngestSample(foreign)
	if err == nil {
		t.Fatal("a sample from another machine was accepted in self scope")
	}
	if !errors.Is(err, semmap.ErrForeignSample) {
		t.Errorf("got %v; want semmap.ErrForeignSample so a caller can tell configuration "+
			"from a transient failure", err)
	}

	// An unlabelled sample is this machine's by default: collectors that do not stamp
	// an ID are not thereby foreign.
	unlabelled := &types.MetricSample{MetricType: types.CPUUtilization,
		Value: 0.4, TimestampUnix: 1002, EventID: "unlabelled-1"}
	if err := sm.IngestSample(unlabelled); err != nil {
		t.Errorf("unlabelled sample rejected: %v", err)
	}
}

// TestIngestSample_AggregatesWhenAsked covers the replay case: one daemon, a whole
// testbed's telemetry, explicitly requested.
func TestIngestSample_AggregatesWhenAsked(t *testing.T) {
	sm, _ := newMap(t)
	sm.SetIdentity("node_1", true)

	for i, host := range []string{"node_1", "node_2", "master"} {
		s := &types.MetricSample{NodeID: host, MetricType: types.CPUUtilization,
			Value: 0.3, TimestampUnix: int64(1000 + i), EventID: "evt-" + host}
		if err := sm.IngestSample(s); err != nil {
			t.Errorf("sample from %s rejected with ingest-scope=any: %v", host, err)
		}
	}
}

// TestIngestSample_RecordsBothLayers pins what one sample does: it becomes an
// observation of the metric's own property, and the construct that summarises that
// metric moves with it.
//
// The second half is the one worth guarding. A construct's value is what a cost answer
// reads, and it is derived rather than stored — recomputed from its members — so a
// sample that reached the metric but left the construct behind would produce an agent
// that sees a busy machine and reports an idle one. This test used to run twice, once
// per updater mode, because whether the construct's magnitude got recorded at all
// depended on which estimator was configured.
func TestIngestSample_RecordsBothLayers(t *testing.T) {
	sm, state := newMap(t)
	sm.SetIdentity("node_1", false)

	spec := mustSpec()
	metric, construct := "", ""
	for _, r := range spec.MetricRouting {
		metric, construct = r.MetricType, r.ConstructID
		break
	}
	if metric == "" {
		t.Skip("spec routes no metrics")
	}

	s := &types.MetricSample{NodeID: "node_1", MetricType: types.MetricType(metric),
		Value: 0.42, TimestampUnix: 1000, EventID: "state-1"}
	if err := sm.IngestSample(s); err != nil {
		t.Fatal(err)
	}

	p, ok := state.Property(metric)
	if !ok {
		t.Fatalf("no property for the ingested metric %s", metric)
	}
	if p.NObservations != 1 || p.Value != 0.42 {
		t.Errorf("metric property is %.4f from %d observations; want 0.42 from 1",
			p.Value, p.NObservations)
	}

	c, ok := state.Property(construct)
	if !ok {
		t.Fatalf("no property for the construct %s that summarises %s", construct, metric)
	}
	if c.Value == 0 {
		t.Errorf("construct %s stayed at zero after its member was observed; a cost "+
			"answer reads the construct, so it would report an idle machine", construct)
	}
}

// TestStateModelLearnsFromIngestedTelemetry checks the whole ingest path: a sample
// arrives, becomes a property observation, and the map's own estimator moves the
// relationships between the affected properties.
//
// It used to test a propagation step — a second model computed the estimate and copied
// it across — and both the step and the second model are gone.
func TestStateModelLearnsFromIngestedTelemetry(t *testing.T) {
	sm, state := newMap(t)
	spec := mustSpec()

	// Drive correlated observations of two constructs so pairs form.
	rcMetric, psMetric := metricFor(spec, "RC"), metricFor(spec, "PS")
	if rcMetric == "" || psMetric == "" {
		t.Skip("spec routes no metric to RC or PS")
	}
	for i := 0; i < 40; i++ {
		x := float64(i%10) / 10.0
		ts := int64(1700000000 + i*5)
		for _, s := range []*types.MetricSample{
			{NodeID: "n1", MetricType: types.MetricType(rcMetric), Value: x,
				TimestampUnix: ts, EventID: "rc-" + itoa(i)},
			{NodeID: "n1", MetricType: types.MetricType(psMetric), Value: 0.9*x + 0.02,
				TimestampUnix: ts + 2, EventID: "ps-" + itoa(i)},
		} {
			if err := sm.IngestSample(s); err != nil {
				t.Fatal(err)
			}
		}
	}

	var moved int
	for _, r := range state.Relationships("", "") {
		if r.NObservations > 0 {
			moved++
			if r.Provenance != statemap.Learned {
				t.Errorf("%s has %d observations but provenance %s; a strength that came "+
					"from this system must not still claim to be seeded",
					r.ID, r.NObservations, r.Provenance)
			}
			if r.Confidence == 0 {
				t.Errorf("%s observed %d times but reports zero confidence", r.ID, r.NObservations)
			}
		}
	}
	if moved == 0 {
		t.Fatal("no relationship received an observation, so its strength can only ever " +
			"be the seeded prior")
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

// metricFor returns a metric the spec routes to one construct, or "".
func metricFor(spec *domain.Spec, construct string) string {
	for _, r := range spec.MetricRouting {
		if r.ConstructID == construct {
			return r.MetricType
		}
	}
	return ""
}

// TestIngestSampleNormalizesToConstructPolarity pins the reconciliation at the point it
// happens. The value stored must be the one expressed in the construct's polarity, not
// the raw reading, because everything downstream — the derived summary, the paired
// estimator, and the sign a proposition declares — reads the stored value and has no
// way to know which metrics arrived inverted.
func TestIngestSampleNormalizesToConstructPolarity(t *testing.T) {
	spec := mustSpec()

	// Route a higher-is-better metric into a construct that runs higher-is-worse.
	// Every committed construct is higher-is-worse, so this is the opposed case.
	target := spec.Constructs[0].ConstructID
	if err := spec.AddMetricRoute(domain.MetricRoute{
		MetricType:  "synthetic_headroom",
		ConstructID: target,
		Unit:        "fraction",
		Range:       [2]float64{0, 1},
		Polarity:    domain.HigherIsBetter,
	}); err != nil {
		t.Fatalf("adding the opposed route: %v", err)
	}
	if got := spec.NormalizeForConstruct("synthetic_headroom", 0.25); got != 0.75 {
		t.Fatalf("precondition: spec normalised 0.25 to %v, want 0.75", got)
	}

	state := statemap.New(statemap.Config{
		Owner: "test-node", ConvergenceObservations: 10, Alpha: 0.5, AdmitUnknown: true,
	}, statemap.NewJournal(0))
	if _, err := profiles.SeedStateMap(state, spec, "", ""); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	sm := semmap.New(minimal.NewOntologyFromSpec(spec),
		minimal.NewRuleEngineReasoner(spec, 0.5, nil, nil),
		minimal.NewDisabledProposer(), minimal.NewDisabledTuner())
	sm.AttachState(state)

	if err := sm.IngestSample(&types.MetricSample{
		MetricType: "synthetic_headroom", Value: 0.25,
		TimestampUnix: 1000, EventID: "h-1",
	}); err != nil {
		t.Fatalf("ingesting: %v", err)
	}

	p, ok := state.Property("synthetic_headroom")
	if !ok {
		t.Fatal("the property was not declared by the seed")
	}
	if p.Value != 0.75 {
		t.Errorf("stored value %.4f, want 0.75 — a metric opposed to its construct must "+
			"be reflected on the way in, or the construct summarises two quantities that "+
			"cancel", p.Value)
	}
}

// TestIngestSample_FeedsProposerAndForgetsOnRetirement pins the join Task 12 makes:
// every observed property reaches the proposer through ObserveProperty (not just
// routed constructs), and a property's retirement — through the map's retire hook —
// makes the proposer forget it, so a candidate cannot outlive the endpoint it names.
func TestIngestSample_FeedsProposerAndForgetsOnRetirement(t *testing.T) {
	spec := mustSpec()
	ontology := minimal.NewOntologyFromSpec(spec)
	state := statemap.New(statemap.Config{AdmitUnknown: true, Learn: true}, statemap.NewJournal(0))
	if _, err := profiles.SeedStateMap(state, spec, "", ""); err != nil {
		t.Fatal(err)
	}
	proposer := minimal.NewMICorrelationProposer(state, 0.8, 10, 60, 15*time.Second)
	reasoner := minimal.NewRuleEngineReasoner(spec, 0.5, nil, nil)
	reasoner.AttachState(state)
	sm := semmap.New(ontology, reasoner, proposer, minimal.NewDisabledTuner())
	sm.AttachState(state)

	t0 := int64(1_700_000_000)
	for i := 0; i < 40; i++ {
		ts := t0 + int64(i)*10
		x := float64(i%20) / 20
		_ = sm.IngestSample(&types.MetricSample{MetricType: types.CPUUtilization, Value: x, TimestampUnix: ts,
			EventID: "p" + strconv.Itoa(i), Subject: "pod:a", Unit: "fraction", Range: &[2]float64{0, 1}})
		_ = sm.IngestSample(&types.MetricSample{MetricType: types.CPUPressureRatio, Value: 0.9 * x, TimestampUnix: ts + 1,
			EventID: "n" + strconv.Itoa(i)})
	}
	cs, err := sm.PendingCandidates()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, c := range cs {
		if c.FromID == "cpu_utilization@pod:a" && c.ToID == "cpu_pressure_ratio" {
			found = true
		}
	}
	if !found {
		t.Fatalf("ingestion did not reach the proposer; candidates=%v", cs)
	}

	if err := state.RetireProperty("cpu_utilization@pod:a", "test", "operator"); err != nil {
		t.Fatal(err)
	}
	cs, _ = sm.PendingCandidates()
	for _, c := range cs {
		if c.FromID == "cpu_utilization@pod:a" {
			t.Error("candidate survived its endpoint's retirement")
		}
	}
}

func TestIngestSample_AdmissionHeadroomIsRecordedButExcludedFromDiscovery(t *testing.T) {
	spec := mustSpec()
	state := statemap.New(statemap.Config{AdmitUnknown: true, Learn: true}, statemap.NewJournal(0))
	if _, err := profiles.SeedStateMap(state, spec, "", ""); err != nil {
		t.Fatal(err)
	}
	proposer := minimal.NewMICorrelationProposer(state, 0.8, 3, 60, 15*time.Second)
	proposer.SetMaxPairs(1)
	reasoner := minimal.NewRuleEngineReasoner(spec, 0.5, nil, nil)
	reasoner.AttachState(state)
	sm := semmap.New(minimal.NewOntologyFromSpec(spec), reasoner, proposer, minimal.NewDisabledTuner())
	sm.AttachState(state)

	t0 := int64(1_700_000_000)
	rng := [2]float64{0, 1}
	for i := 0; i < 30; i++ {
		x := float64(i%10) / 10
		at := t0 + int64(i)*5
		for _, sample := range []*types.MetricSample{
			{MetricType: types.MemoryAdmissionHeadroomRatio, Value: 1 - x, TimestampUnix: at, EventID: "h" + strconv.Itoa(i)},
			{MetricType: types.MemoryUtilization, Value: x, TimestampUnix: at, EventID: "m" + strconv.Itoa(i)},
			{MetricType: types.MemoryUtilization, Subject: "pod:cache", Value: x, TimestampUnix: at, EventID: "p" + strconv.Itoa(i)},
		} {
			sample.Unit, sample.Range = "fraction", &rng
			if err := sm.IngestSample(sample); err != nil {
				t.Fatalf("ingest %s: %v", sample.MetricType, err)
			}
		}
	}

	if p, ok := state.Property(string(types.MemoryAdmissionHeadroomRatio)); !ok || p.NObservations != 30 {
		t.Fatalf("admission headroom property %+v, ok=%v; want 30 state observations", p, ok)
	}
	d, err := sm.Discovery()
	if err != nil {
		t.Fatal(err)
	}
	if d.Properties != 2 || d.EligiblePairs != 1 || d.TestedPairs != 1 {
		t.Fatalf("discovery %+v; want only the two memory-utilization properties to use the one-pair budget", d)
	}
	candidates, err := sm.PendingCandidates()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range candidates {
		if strings.Contains(c.CandidateID, string(types.MemoryAdmissionHeadroomRatio)) {
			t.Errorf("derived admission metric entered discovery: %s", c.CandidateID)
		}
	}
}

func TestIngestSample_ScopedSampleBecomesMetricAtSubject(t *testing.T) {
	sm, state := newMap(t)
	rng := [2]float64{0, 50}
	s := &types.MetricSample{MetricType: "queue_depth", Value: 3, TimestampUnix: 1000, EventID: "q1",
		Subject: "pod:abc", Unit: "items", Range: &rng, Source: "app"}
	if err := sm.IngestSample(s); err != nil {
		t.Fatal(err)
	}
	if _, ok := state.Property("queue_depth"); ok {
		t.Error("a scoped sample must not land on the node-level id")
	}
	p, ok := state.Property("queue_depth@pod:abc")
	if !ok || p.Subject != "pod:abc" || p.Unit != "items" || p.Range != rng {
		t.Fatalf("scoped property %+v ok=%v; want metric_type@subject with the declared unit and range", p, ok)
	}
}

// TestIngestSample_ScopedReadingOfARoutedTypeIsNeitherReflectedNorRouted: a scoped
// reading of a routed metric type is still unrouted — polarity and construct
// membership are node-level concerns. The route used here is opposed to its
// construct, so a reading that went through NormalizeForConstruct would come out
// reflected (0.25 → 0.75), and one that reached the construct would move its
// derived value; neither may happen.
func TestIngestSample_ScopedReadingOfARoutedTypeIsNeitherReflectedNorRouted(t *testing.T) {
	spec := mustSpec()
	target := spec.Constructs[0].ConstructID
	if err := spec.AddMetricRoute(domain.MetricRoute{MetricType: "synthetic_headroom", ConstructID: target,
		Unit: "fraction", Range: [2]float64{0, 1}, Polarity: domain.HigherIsBetter}); err != nil {
		t.Fatal(err)
	}
	if got := spec.NormalizeForConstruct("synthetic_headroom", 0.25); got != 0.75 {
		t.Fatalf("precondition: the route must be opposed to its construct; normalised 0.25 to %v", got)
	}
	state := statemap.New(statemap.Config{Owner: "test-node", ConvergenceObservations: 10, Alpha: 0.5, AdmitUnknown: true}, statemap.NewJournal(0))
	if _, err := profiles.SeedStateMap(state, spec, "", ""); err != nil {
		t.Fatal(err)
	}
	sm := semmap.New(minimal.NewOntologyFromSpec(spec), minimal.NewRuleEngineReasoner(spec, 0.5, nil, nil),
		minimal.NewDisabledProposer(), minimal.NewDisabledTuner())
	sm.AttachState(state)
	before, _ := state.Property(target)

	rng := [2]float64{0, 1}
	if err := sm.IngestSample(&types.MetricSample{MetricType: "synthetic_headroom", Value: 0.25, TimestampUnix: 1000,
		EventID: "h-scoped", Subject: "pod:abc", Unit: "fraction", Range: &rng}); err != nil {
		t.Fatal(err)
	}
	p, ok := state.Property("synthetic_headroom@pod:abc")
	if !ok || p.Value != 0.25 {
		t.Errorf("scoped value %.3f ok=%v; want 0.25 untouched by the construct's polarity", p.Value, ok)
	}
	// The seed declares a node-level property for every routed type; the scoped
	// reading must not have been recorded against it.
	if node, ok := state.Property("synthetic_headroom"); ok && node.NObservations != 0 {
		t.Errorf("the scoped reading was recorded on the node-level id: %+v", node)
	}
	after, _ := state.Property(target)
	if after.NObservations != before.NObservations || after.Value != before.Value {
		t.Errorf("construct %s moved from %+v to %+v on a scoped reading; scoped samples are never routed", target, before, after)
	}
}

// TestConfirmCandidate_ScopedPairBecomesDiscoveredRelationship pins Task 13's branch:
// a candidate naming a scoped property (a pod↔pressure pair, not two Di-Select
// constructs) must not become a proposition when confirmed. It becomes a state-map
// relationship with Discovered provenance instead — a fact about this machine, not a
// claim in Di-Select's vocabulary.
func TestConfirmCandidate_ScopedPairBecomesDiscoveredRelationship(t *testing.T) {
	spec := mustSpec()
	ontology := minimal.NewOntologyFromSpec(spec)
	state := statemap.New(statemap.Config{AdmitUnknown: true}, statemap.NewJournal(0))
	_, _ = profiles.SeedStateMap(state, spec, "", "")
	proposer := minimal.NewMICorrelationProposer(state, 0.8, 10, 60, 15*time.Second)
	reasoner := minimal.NewRuleEngineReasoner(spec, 0.5, nil, nil)
	reasoner.AttachState(state)
	sm := semmap.New(ontology, reasoner, proposer, minimal.NewDisabledTuner())
	sm.AttachState(state)

	t0 := int64(1_700_000_000)
	for i := 0; i < 40; i++ {
		x := float64(i%20) / 20
		_ = sm.IngestSample(&types.MetricSample{MetricType: types.CPUUtilization, Value: x, TimestampUnix: t0 + int64(i)*10,
			EventID: "p" + strconv.Itoa(i), Subject: "pod:a"})
		_ = sm.IngestSample(&types.MetricSample{MetricType: types.CPUPressureRatio, Value: 0.9 * x, TimestampUnix: t0 + int64(i)*10 + 1,
			EventID: "n" + strconv.Itoa(i)})
	}
	cs, _ := sm.PendingCandidates()
	if len(cs) == 0 {
		t.Fatal("setup: no candidate")
	}
	before, _ := sm.Propositions()
	if err := sm.ConfirmCandidate(cs[0].CandidateID, "", types.CandidateReview{}); err != nil {
		t.Fatal(err)
	}
	after, _ := sm.Propositions()
	if len(after) != len(before) {
		t.Errorf("a scoped candidate must not become a proposition (%d -> %d)", len(before), len(after))
	}
	r, ok := state.Relationship(statemap.RelationshipID("cpu_utilization@pod:a", "cpu_pressure_ratio", "discovered"))
	if !ok || r.Provenance != statemap.Discovered || r.Sign != 1 {
		t.Fatalf("relationship %+v ok=%v; want Discovered, sign +1, label discovered", r, ok)
	}
	if state.Census().Discovered != 1 {
		t.Errorf("census discovered=%d, want 1", state.Census().Discovered)
	}
	// The confirmation stands, so the proposer kept nothing to reopen it with.
	if err := proposer.Reopen(cs[0].CandidateID); err == nil {
		t.Error("an applied confirmation could still be reopened; the proposer kept its prior status")
	}
}

// TestConfirmCandidate_ACoveredPairDeclaresNothingMore: a candidate settled after the
// pair gained a relationship — the agent's interventions created one — is confirmed
// without a second, discovered relationship on the same pair, which would count the
// one dependency twice.
func TestConfirmCandidate_ACoveredPairDeclaresNothingMore(t *testing.T) {
	spec := mustSpec()
	ontology := minimal.NewOntologyFromSpec(spec)
	state := statemap.New(statemap.Config{AdmitUnknown: true}, statemap.NewJournal(0))
	_, _ = profiles.SeedStateMap(state, spec, "", "")
	proposer := minimal.NewMICorrelationProposer(state, 0.8, 10, 60, 15*time.Second)
	reasoner := minimal.NewRuleEngineReasoner(spec, 0.5, nil, nil)
	reasoner.AttachState(state)
	sm := semmap.New(ontology, reasoner, proposer, minimal.NewDisabledTuner())
	sm.AttachState(state)

	t0 := int64(1_700_000_000)
	for i := 0; i < 40; i++ {
		x := float64(i%20) / 20
		_ = sm.IngestSample(&types.MetricSample{MetricType: types.CPUUtilization, Value: x, TimestampUnix: t0 + int64(i)*10,
			EventID: "p" + strconv.Itoa(i), Subject: "pod:a"})
		_ = sm.IngestSample(&types.MetricSample{MetricType: types.CPUPressureRatio, Value: 0.9 * x, TimestampUnix: t0 + int64(i)*10 + 1,
			EventID: "n" + strconv.Itoa(i)})
	}
	cs, _ := sm.PendingCandidates()
	if len(cs) == 0 {
		t.Fatal("setup: no candidate")
	}
	from, to := "cpu_utilization@pod:a", "cpu_pressure_ratio"
	for _, o := range []string{"v1", "v2"} {
		if _, _, err := state.RecordIntervention(statemap.InterventionOutcome{From: from, To: to, Agreed: true, DecisionID: o}); err != nil {
			t.Fatal(err)
		}
	}
	if err := sm.ConfirmCandidate(cs[0].CandidateID, "", types.CandidateReview{By: types.ReviewerIntervention}); err != nil {
		t.Fatal(err)
	}
	if rs := state.Relationships(from, to); len(rs) != 1 || rs[0].Provenance != statemap.Intervened {
		t.Fatalf("relationships %+v; want only the intervened one", rs)
	}
	if left, _ := sm.PendingCandidates(); len(left) != len(cs)-1 {
		t.Errorf("%d pending after confirming; want %d", len(left), len(cs)-1)
	}
}

// TestConfirmCandidate_FailedDeclarationLeavesTheCandidatePending: the proposer marks
// a candidate Confirmed before the facade declares the relationship. When the
// declaration is refused — here an opposite-sign edge already exists — the error must
// not leave a candidate that history calls Confirmed, that no relationship backs, and
// that can never be retried or rejected.
func TestConfirmCandidate_FailedDeclarationLeavesTheCandidatePending(t *testing.T) {
	spec := mustSpec()
	ontology := minimal.NewOntologyFromSpec(spec)
	state := statemap.New(statemap.Config{AdmitUnknown: true}, statemap.NewJournal(0))
	_, _ = profiles.SeedStateMap(state, spec, "", "")
	proposer := minimal.NewMICorrelationProposer(state, 0.8, 10, 60, 15*time.Second)
	reasoner := minimal.NewRuleEngineReasoner(spec, 0.5, nil, nil)
	reasoner.AttachState(state)
	sm := semmap.New(ontology, reasoner, proposer, minimal.NewDisabledTuner())
	sm.AttachState(state)

	t0 := int64(1_700_000_000)
	for i := 0; i < 40; i++ {
		x := float64(i%20) / 20
		_ = sm.IngestSample(&types.MetricSample{MetricType: types.CPUUtilization, Value: x, TimestampUnix: t0 + int64(i)*10,
			EventID: "p" + strconv.Itoa(i), Subject: "pod:a"})
		_ = sm.IngestSample(&types.MetricSample{MetricType: types.CPUPressureRatio, Value: 0.9 * x, TimestampUnix: t0 + int64(i)*10 + 1,
			EventID: "n" + strconv.Itoa(i)})
	}
	cs, _ := sm.PendingCandidates()
	if len(cs) == 0 {
		t.Fatal("setup: no candidate")
	}
	// An earlier claim in the opposite direction makes the declaration fail.
	if err := state.DeclareRelationship(statemap.Relationship{From: "cpu_utilization@pod:a", To: "cpu_pressure_ratio",
		Label: "discovered", Sign: -1, Provenance: statemap.Discovered}); err != nil {
		t.Fatal(err)
	}
	if err := sm.ConfirmCandidate(cs[0].CandidateID, "", types.CandidateReview{}); err == nil {
		t.Fatal("confirming against an opposite-sign edge succeeded")
	}
	after, _ := sm.PendingCandidates()
	if len(after) != 1 || after[0].CandidateID != cs[0].CandidateID {
		t.Errorf("after the failed declaration the candidate is gone from pending: %v; want it back, retryable", after)
	}
	h, _ := proposer.GetHistory()
	for _, c := range h {
		if c.CandidateID == cs[0].CandidateID && c.Status == types.Confirmed {
			t.Error("history calls the candidate Confirmed although no relationship was declared")
		}
	}
}

// TestConfirmCandidate_FailedDeclarationRestoresAProposerDeferral: a candidate the
// proposer deferred is open to confirmation, and a refused declaration puts it back
// as it was — deferred, for the proposer's reason — not into the pending queue the
// proposer took it out of. The deferral is "weakened" and the queue has room, so the
// cap cannot put it back for Reopen.
func TestConfirmCandidate_FailedDeclarationRestoresAProposerDeferral(t *testing.T) {
	state := statemap.New(statemap.Config{AdmitUnknown: true}, statemap.NewJournal(0))
	proposer := minimal.NewMICorrelationProposer(state, 0.8, 10, 60, 15*time.Second)
	proposer.SetLogger(t.Logf)
	sm := semmap.New(minimal.NewOntologyFromSpec(nil), nil, proposer, minimal.NewDisabledTuner())
	sm.AttachState(state)

	t0 := int64(1_700_000_000)
	feed := func(i int, x, y float64) {
		_ = sm.IngestSample(&types.MetricSample{MetricType: types.CPUUtilization, Value: x, TimestampUnix: t0 + int64(i)*10,
			EventID: "p" + strconv.Itoa(i), Subject: "pod:a"})
		_ = sm.IngestSample(&types.MetricSample{MetricType: types.CPUPressureRatio, Value: y, TimestampUnix: t0 + int64(i)*10 + 1,
			EventID: "n" + strconv.Itoa(i)})
	}
	i := 0
	for ; i < 40; i++ {
		x := float64(i%20) / 20
		feed(i, x, 0.9*x)
	}
	cs, _ := sm.PendingCandidates()
	if len(cs) != 1 {
		t.Fatalf("setup: want one pending candidate, got %+v", cs)
	}
	id := cs[0].CandidateID
	// The pair stops holding: the node signal now moves with something else.
	var withdrawn *types.CandidateEdge
	for ; withdrawn == nil; i++ {
		if i > 400 {
			t.Fatal("setup: the candidate never weakened")
		}
		feed(i, float64(i%20)/20, float64((i*7)%20)/20)
		h, _ := proposer.GetHistory()
		for _, c := range h {
			if c.CandidateID == id && c.Status == types.Deferred {
				if c.Reason != "weakened" {
					t.Fatalf("setup: deferred for %q; want weakened", c.Reason)
				}
				withdrawn = c
			}
		}
	}
	if err := state.DeclareRelationship(statemap.Relationship{From: withdrawn.FromID, To: withdrawn.ToID,
		Label: "discovered", Sign: -1, Provenance: statemap.Discovered}); err != nil {
		t.Fatal(err)
	}
	if err := sm.ConfirmCandidate(id, "", types.CandidateReview{}); err == nil {
		t.Fatal("confirming against an opposite-sign edge succeeded")
	}
	h, _ := proposer.GetHistory()
	for _, c := range h {
		if c.CandidateID == id && (c.Status != types.Deferred || c.Reason != "weakened" || c.Review != nil) {
			t.Errorf("after the refused declaration the candidate is %v/%q (review %+v); want it deferred as weakened again",
				c.Status, c.Reason, c.Review)
		}
	}
	if pending, _ := sm.PendingCandidates(); len(pending) != 0 {
		t.Errorf("%d pending after the refusal; want none", len(pending))
	}
}

// TestConfirmCandidate_RefusedConstructDeclarationLeavesTheCandidatePending: the
// construct path refuses too — the ontology rejects a proposition that contradicts one
// already in the backbone — and that refusal must put the candidate back as the
// property-pair path does, with nothing declared.
func TestConfirmCandidate_RefusedConstructDeclarationLeavesTheCandidatePending(t *testing.T) {
	proposer := minimal.NewMICorrelationProposer(nil, 0.8, 10, 60, 15*time.Second)
	sm := semmap.New(minimal.NewOntologyFromSpec(mustSpec()), nil, proposer, minimal.NewDisabledTuner())
	before, _ := sm.Propositions()
	var existing *types.Proposition
	for _, p := range before {
		if p.FromConstruct != p.ToConstruct {
			existing = p
			break
		}
	}
	if existing == nil {
		t.Fatal("setup: the spec has no proposition between two constructs")
	}

	// Feed the pair moving against the backbone's claim.
	sign := 1.0
	if existing.Direction == types.Positive {
		sign = -1
	}
	for i := 0; i < 40; i++ {
		x := float64(i%20) / 20
		if err := proposer.Observe(existing.FromConstruct, existing.ToConstruct, x, 0.5+sign*0.4*x); err != nil {
			t.Fatal(err)
		}
	}
	cs, _ := sm.PendingCandidates()
	if len(cs) != 1 || cs[0].Direction == existing.Direction {
		t.Fatalf("setup: want one candidate contradicting %s, got %+v", existing.PropositionID, cs)
	}
	from := ""
	if cs[0].Undirected {
		from = existing.FromConstruct
	}
	if err := sm.ConfirmCandidate(cs[0].CandidateID, from, types.CandidateReview{}); err == nil {
		t.Fatal("confirming a proposition that contradicts the backbone succeeded")
	}
	if after, _ := sm.Propositions(); len(after) != len(before) {
		t.Errorf("propositions %d -> %d; a refused declaration adds none", len(before), len(after))
	}
	after, _ := sm.PendingCandidates()
	if len(after) != 1 || after[0].CandidateID != cs[0].CandidateID {
		t.Errorf("after the refused declaration pending is %v; want the candidate back, retryable", after)
	}
	h, _ := proposer.GetHistory()
	for _, c := range h {
		if c.CandidateID == cs[0].CandidateID && c.Status == types.Confirmed {
			t.Error("history calls the candidate Confirmed although nothing was declared")
		}
	}
}

func TestAssociationConfirmationGuards(t *testing.T) {
	for _, tc := range []struct {
		name        string
		directed    bool
		withState   bool
		directional bool
	}{
		{name: "operator cannot associate a directed candidate", directed: true, withState: true},
		{name: "association requires state properties"},
		{name: "association cannot specify a direction", withState: true, directional: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := statemap.New(statemap.Config{AdmitUnknown: true}, statemap.NewJournal(0))
			p := minimal.NewMICorrelationProposer(nil, 0.8, 6, 12, 0)
			sm := semmap.New(minimal.NewOntologyFromSpec(nil), nil, p, minimal.NewDisabledTuner())
			sm.AttachState(state)
			base := time.Now().Add(-time.Minute)
			range01 := &[2]float64{0, 1}
			for i := 0; i < 36; i++ {
				at := base.Add(time.Duration(i) * time.Second)
				x := float64(i%12) / 12
				fromID := "load"
				if tc.directed {
					fromID = "load@pod:a"
				}
				readings := []types.PropertyReading{
					{ID: fromID, Subject: "pod:a", Value: x, At: at, Range: range01},
					{ID: "pressure", Value: 0.1 + 0.8*x, At: at, Range: range01},
				}
				if !tc.directed {
					readings[0].Subject = ""
				}
				for _, reading := range readings {
					if tc.withState {
						if err := state.Record(statemap.Observation{
							ID: reading.ID, Subject: reading.Subject, Value: reading.Value, At: at,
						}); err != nil {
							t.Fatal(err)
						}
					}
					if err := p.ObserveProperty(reading); err != nil {
						t.Fatal(err)
					}
				}
			}
			candidates, err := sm.PendingCandidates()
			if err != nil || len(candidates) != 1 {
				t.Fatalf("candidate setup: %v %v", candidates, err)
			}
			from := ""
			if tc.directional {
				from = candidates[0].FromID
			}
			err = sm.ConfirmCandidate(candidates[0].CandidateID, from, types.CandidateReview{
				By: types.ReviewerOperator, Action: types.ReviewConfirm, Association: true,
			})
			if err == nil {
				t.Fatal("invalid association confirmation was accepted")
			}
		})
	}
}

// TestConfirmCandidate_NegativeCorrelationDeclaresSignMinusOne: a candidate with a
// negative direction becomes a relationship with sign −1; with the wrong sign,
// Covered would miss it and the pair would be proposed forever.
func TestConfirmCandidate_NegativeCorrelationDeclaresSignMinusOne(t *testing.T) {
	spec := mustSpec()
	state := statemap.New(statemap.Config{AdmitUnknown: true}, statemap.NewJournal(0))
	_, _ = profiles.SeedStateMap(state, spec, "", "")
	proposer := minimal.NewMICorrelationProposer(state, 0.8, 10, 60, 15*time.Second)
	reasoner := minimal.NewRuleEngineReasoner(spec, 0.5, nil, nil)
	reasoner.AttachState(state)
	sm := semmap.New(minimal.NewOntologyFromSpec(spec), reasoner, proposer, minimal.NewDisabledTuner())
	sm.AttachState(state)
	t0 := int64(1_700_000_000)
	for i := 0; i < 40; i++ {
		x := float64(i%20) / 20
		_ = sm.IngestSample(&types.MetricSample{MetricType: types.CPUUtilization, Value: x, TimestampUnix: t0 + int64(i)*10,
			EventID: "p" + strconv.Itoa(i), Subject: "pod:a"})
		_ = sm.IngestSample(&types.MetricSample{MetricType: types.CPUPressureRatio, Value: 0.9 * (1 - x), TimestampUnix: t0 + int64(i)*10 + 1,
			EventID: "n" + strconv.Itoa(i)})
	}
	cs, _ := sm.PendingCandidates()
	if len(cs) != 1 || cs[0].Direction != types.Negative {
		t.Fatalf("candidates %+v; want one Negative", cs)
	}
	if err := sm.ConfirmCandidate(cs[0].CandidateID, "", types.CandidateReview{}); err != nil {
		t.Fatal(err)
	}
	r, ok := state.Relationship(statemap.RelationshipID("cpu_utilization@pod:a", "cpu_pressure_ratio", "discovered"))
	if !ok || r.Sign != -1 {
		t.Errorf("relationship %+v ok=%v; want sign -1", r, ok)
	}
	if !state.Covered("cpu_utilization@pod:a", "cpu_pressure_ratio", -1) {
		t.Error("the confirmed negative edge is not covered, so it would be proposed again")
	}
}

// spyProposer records which readings reached the proposer.
type spyProposer struct {
	*minimal.DisabledProposer
	seen []types.PropertyReading
}

func (s *spyProposer) ObserveProperty(r types.PropertyReading) error {
	s.seen = append(s.seen, r)
	return nil
}

// TestIngestSample_ARefusedReadingDoesNotReachTheProposer: the map refused it, so
// discovery must not learn from it either — a future-stamped reading would otherwise
// hold the proposer's view of that property in the future.
func TestIngestSample_ARefusedReadingDoesNotReachTheProposer(t *testing.T) {
	state := statemap.New(statemap.Config{AdmitUnknown: true}, statemap.NewJournal(0))
	now := time.Unix(1_700_000_000, 0)
	state.SetClock(func() time.Time { return now })
	spy := &spyProposer{DisabledProposer: minimal.NewDisabledProposer()}
	sm := semmap.New(minimal.NewOntologyFromSpec(mustSpec()), minimal.NewRuleEngineReasoner(mustSpec(), 0.5, nil, nil),
		spy, minimal.NewDisabledTuner())
	sm.AttachState(state)

	for _, s := range []types.MetricSample{
		{MetricType: "gpu", Value: 0.5, TimestampUnix: now.Unix(), EventID: "a"},
		{MetricType: "gpu", Value: 0.5, TimestampUnix: now.Add(time.Hour).Unix(), EventID: "b"},    // future
		{MetricType: "gpu", Value: 0.5, TimestampUnix: now.Add(-time.Minute).Unix(), EventID: "c"}, // older
		{MetricType: "gpu", Value: 0.5, TimestampUnix: now.Unix(), EventID: "a"},                   // duplicate
	} {
		if err := sm.IngestSample(&s); err != nil {
			t.Fatal(err)
		}
	}
	if len(spy.seen) != 1 {
		t.Errorf("%d readings reached the proposer; want 1, the accepted one", len(spy.seen))
	}
}
