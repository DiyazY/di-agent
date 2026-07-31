package semmap

import (
	"testing"

	"github.com/DiyazY/di-agent/internal/minimal"
	"github.com/DiyazY/di-agent/pkg/types"
)

// fakeUpdater is a slim test stub satisfying the edgeUpdater interface. It
// records every (from, to, value, eventID) call so tests can assert routing
// without needing a full SemanticMap.
type fakeUpdater struct {
	calls []updateCall
}

type updateCall struct {
	from    string
	to      string
	value   float64
	eventID string
}

func (f *fakeUpdater) UpdateEdge(from, to string, value float64, eventID string) (*types.EdgeDescriptor, error) {
	f.calls = append(f.calls, updateCall{from, to, value, eventID})
	return &types.EdgeDescriptor{FromID: from, ToID: to}, nil
}

// TestBridge_KnownMetricTypeUpdatesRelatedEdges asserts that a sample whose
// primary construct is RC drives UpdateEdge on every unique (from, to)
// endpoint touching RC. In the Di-Select bootstrap that means:
//
//	SC→RC  (P1)        RC→PS  (P2/P3 conflict pair — collapsed)
//	PS→RC  (P10)       MU→RC  (P8)
//	RC→SC  (P14)
//
// Five unique endpoint pairs after de-duplication.
func TestBridge_KnownMetricTypeUpdatesRelatedEdges(t *testing.T) {
	ontology := minimal.NewOntologyFromSpec(mustSpec())
	upd := &fakeUpdater{}

	sample := &types.MetricSample{
		NodeID:        "node_1",
		MetricType:    types.CPUUtilization,
		Value:         0.42,
		TimestampUnix: 1700000000,
		EventID:       "evt-cpu-1",
	}

	if err := Bridge(sample, ontology, upd); err != nil {
		t.Fatalf("Bridge: unexpected error %v", err)
	}

	// Derive the expectation from the spec: a sample routed to a construct must
	// reach every unique endpoint pair incident to it. Hardcoding the pairs made
	// this test assert a graph scope rather than the Bridge's fan-out behaviour.
	spec := mustSpec()
	target, ok := spec.ConstructForMetric("cpu_utilization")
	if !ok {
		t.Fatal("spec routes no construct for cpu_utilization")
	}
	wantPairs := map[string]bool{}
	for _, p := range spec.Propositions {
		if p.FromConstruct == target || p.ToConstruct == target {
			wantPairs[p.FromConstruct+"->"+p.ToConstruct] = false
		}
	}
	if len(wantPairs) == 0 {
		t.Fatalf("no proposition is incident to %s", target)
	}
	for _, c := range upd.calls {
		key := c.from + "->" + c.to
		if _, ok := wantPairs[key]; !ok {
			t.Errorf("unexpected UpdateEdge call for %s (sample only routes through RC)", key)
			continue
		}
		wantPairs[key] = true
		if c.value != 0.42 {
			t.Errorf("UpdateEdge value: got %v, want 0.42", c.value)
		}
		if c.eventID != "evt-cpu-1" {
			t.Errorf("UpdateEdge eventID: got %q, want %q", c.eventID, "evt-cpu-1")
		}
	}
	for pair, ok := range wantPairs {
		if !ok {
			t.Errorf("expected UpdateEdge call for %s but none observed", pair)
		}
	}
}

// TestBridge_UnknownMetricTypeIsSilentlyIgnored verifies the forward-compat
// guarantee: a MetricType not in the routing table produces zero updater
// calls and no error.
func TestBridge_UnknownMetricTypeIsSilentlyIgnored(t *testing.T) {
	ontology := minimal.NewOntologyFromSpec(mustSpec())
	upd := &fakeUpdater{}

	sample := &types.MetricSample{
		NodeID:     "node_1",
		MetricType: types.MetricType("brand_new_metric_that_does_not_exist"),
		Value:      0.5,
		EventID:    "evt-x",
	}

	if err := Bridge(sample, ontology, upd); err != nil {
		t.Fatalf("Bridge: unexpected error %v", err)
	}
	if len(upd.calls) != 0 {
		t.Errorf("expected zero updater calls for unknown MetricType; got %d", len(upd.calls))
	}
}

// TestBridge_ConflictPairIsUpdatedOnce asserts that the RC→PS endpoint —
// which two propositions (P2, P3) share with opposite directions — is called
// EXACTLY ONCE per sample. The Updater handles multigraph fan-out internally;
// the Bridge must not double-count.
func TestBridge_ConflictPairIsUpdatedOnce(t *testing.T) {
	ontology := minimal.NewOntologyFromSpec(mustSpec())
	upd := &fakeUpdater{}

	sample := &types.MetricSample{
		NodeID:     "node_1",
		MetricType: types.CPUUtilization, // routes to RC
		Value:      0.3,
		EventID:    "evt-pair",
	}

	if err := Bridge(sample, ontology, upd); err != nil {
		t.Fatalf("Bridge: unexpected error %v", err)
	}

	rcPsCount := 0
	for _, c := range upd.calls {
		if c.from == "RC" && c.to == "PS" {
			rcPsCount++
		}
	}
	if rcPsCount != 1 {
		t.Errorf("RC→PS UpdateEdge call count: got %d, want 1 (conflict pair P2/P3 must collapse)", rcPsCount)
	}
}
