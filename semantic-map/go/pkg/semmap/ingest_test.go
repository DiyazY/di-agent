package semmap_test

import (
	"errors"
	"strconv"
	"testing"

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
