package semmap_test

import (
	"errors"
	"strconv"
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
			EventID: "p" + strconv.Itoa(i), Subject: "pod:a", Unit: "share-of-node-capacity", Range: &[2]float64{0, 1}})
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
	// A scoped reading of a routed metric type is still unrouted: polarity and
	// construct membership are node-level concerns.
	c := &types.MetricSample{MetricType: types.CPUUtilization, Value: 0.4, TimestampUnix: 1001, EventID: "c1", Subject: "pod:abc"}
	if err := sm.IngestSample(c); err != nil {
		t.Fatal(err)
	}
	if p, _ := state.Property("cpu_utilization@pod:abc"); p.Value != 0.4 {
		t.Errorf("scoped cpu value %.3f; want 0.4 untouched by construct polarity normalisation", p.Value)
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
	if err := sm.ConfirmCandidate(cs[0].CandidateID); err != nil {
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
	if err := sm.ConfirmCandidate(cs[0].CandidateID); err == nil {
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
