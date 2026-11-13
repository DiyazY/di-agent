package compliance

import (
	"fmt"
	"testing"

	"github.com/DiyazY/di-agent/pkg/contracts"
	"github.com/DiyazY/di-agent/pkg/types"
)

type emptyCollector struct{}

func (emptyCollector) SourceID() string                        { return "empty" }
func (emptyCollector) AvailableMetrics() []types.MetricType    { return nil }
func (emptyCollector) Health() error                           { return nil }
func (emptyCollector) Collect() ([]*types.MetricSample, error) { return nil, nil }

// A failing check must be reported by name, and Fatalf must end only the check that
// raised it: the checks after it still run.
func TestRunCollectorChecksOverRecorder(t *testing.T) {
	rec := NewRecorder()
	RunCollectorChecks(rec, func(r Reporter) contracts.CollectorContract { return emptyCollector{} })
	res := rec.Results()
	byName := map[string]Result{}
	for _, x := range res {
		byName[x.Name] = x
	}
	if !byName["SourceIDNonEmpty"].Passed() {
		t.Errorf("SourceIDNonEmpty should pass: %+v", byName["SourceIDNonEmpty"])
	}
	if byName["CollectProducesSamples"].Passed() {
		t.Error("CollectProducesSamples must fail for a collector that emits nothing")
	}
	if _, ran := byName["EventIDDeterministic"]; !ran {
		t.Errorf("checks after a Fatalf must still run; got %v", fmt.Sprint(res))
	}
}

// A zero Recorder works, and a failure raised outside any check is kept as a
// result of its own rather than dropped; a root Fatalf does not panic.
func TestRecorderZeroValueAndRootFailures(t *testing.T) {
	var rec Recorder
	rec.Errorf("before any check: %d", 1)
	rec.Fatalf("fatal at the root")
	rec.Run("ok", func(Reporter) {})
	res := rec.Results()
	if len(res) != 2 || res[0].Name != RootCheck || len(res[0].Failures) != 2 || !res[1].Passed() {
		t.Fatalf("results = %+v", res)
	}
}

// dupCollector emits two samples with one EventID and, as an adapter would,
// reports what it dropped from its source.
type dupCollector struct{ drops []contracts.SampleDrop }

func (*dupCollector) SourceID() string                     { return "dup" }
func (*dupCollector) AvailableMetrics() []types.MetricType { return []types.MetricType{"x"} }
func (*dupCollector) Health() error                        { return nil }
func (*dupCollector) Collect() ([]*types.MetricSample, error) {
	s := &types.MetricSample{NodeID: "n", MetricType: "x", EventID: "e1", Unit: "u", PartOf: "cpu"}
	return []*types.MetricSample{s, s}, nil
}
func (d *dupCollector) TakeDrops() []contracts.SampleDrop {
	return []contracts.SampleDrop{{Kind: contracts.DropMalformed, MetricType: "y"}, {Kind: contracts.DropDuplicate, MetricType: "x"}}
}

func TestChecksCatchDuplicatesDropsAndPartOf(t *testing.T) {
	rec := NewRecorder()
	RunCollectorChecks(rec, func(Reporter) contracts.CollectorContract { return &dupCollector{} })
	byName := map[string]Result{}
	for _, x := range rec.Results() {
		byName[x.Name] = x
	}
	if r := byName["NoDuplicateSamplesInBatch"]; len(r.Failures) != 2*DefaultBatchTicks {
		t.Errorf("NoDuplicateSamplesInBatch = %+v, want the batch duplicate and the reported drop", r)
	}
	if r := byName["NoMalformedSamples"]; len(r.Failures) != DefaultBatchTicks {
		t.Errorf("NoMalformedSamples = %+v, want the malformed drop", r)
	}
	if r := byName["SamplesHaveValidTypeAndSubject"]; r.Passed() {
		t.Error("a part_of on a node-level sample must fail SamplesHaveValidTypeAndSubject")
	}
}
