package compliance

import (
	"testing"

	"github.com/DiyazY/di-agent/pkg/contracts"
	"github.com/DiyazY/di-agent/pkg/types"
)

type CollectorFactory func(t *testing.T) contracts.CollectorContract

func RunCollectorCompliance(t *testing.T, factory CollectorFactory) {
	t.Helper()

	// ── SourceID ──────────────────────────────────────────────────────────────

	t.Run("SourceIDNonEmpty", func(t *testing.T) {
		c := factory(t)
		if c.SourceID() == "" {
			t.Error("SourceID() must return a non-empty string")
		}
	})

	t.Run("SourceIDStable", func(t *testing.T) {
		c := factory(t)
		if c.SourceID() != c.SourceID() {
			t.Error("SourceID() must return the same value on every call")
		}
	})

	// ── AvailableMetrics ──────────────────────────────────────────────────────

	t.Run("AvailableMetricsNonEmpty", func(t *testing.T) {
		c := factory(t)
		if len(c.AvailableMetrics()) == 0 {
			t.Error("AvailableMetrics() must list at least one MetricType")
		}
	})

	t.Run("AvailableMetricsStable", func(t *testing.T) {
		c := factory(t)
		first := metricSet(c.AvailableMetrics())
		second := metricSet(c.AvailableMetrics())
		for mt := range first {
			if !second[mt] {
				t.Errorf("AvailableMetrics() changed: %q present first call but not second", mt)
			}
		}
		for mt := range second {
			if !first[mt] {
				t.Errorf("AvailableMetrics() changed: %q present second call but not first", mt)
			}
		}
	})

	// ── Collect ───────────────────────────────────────────────────────────────

	t.Run("CollectDoesNotError", func(t *testing.T) {
		c := factory(t)
		_, err := c.Collect()
		if err != nil {
			t.Fatalf("Collect() returned unexpected error: %v", err)
		}
	})

	t.Run("CollectTwiceNoError", func(t *testing.T) {
		c := factory(t)
		if _, err := c.Collect(); err != nil {
			t.Fatalf("first Collect() error: %v", err)
		}
		if _, err := c.Collect(); err != nil {
			t.Fatalf("second Collect() error: %v", err)
		}
	})

	t.Run("SamplesHaveNonEmptyNodeID", func(t *testing.T) {
		c := factory(t)
		samples, _ := c.Collect()
		for _, s := range samples {
			if s.NodeID == "" {
				t.Errorf("MetricSample.NodeID must be non-empty; got %+v", s)
			}
		}
	})

	t.Run("SamplesHaveNonEmptyEventID", func(t *testing.T) {
		c := factory(t)
		samples, _ := c.Collect()
		for _, s := range samples {
			if s.EventID == "" {
				t.Errorf("MetricSample.EventID must be non-empty; got %+v", s)
			}
		}
	})

	// A sample whose unit and range are not declared cannot be reasoned about: the
	// map normalises a contribution by the property's range, so an undeclared range
	// means the map assumes [0,1] and every answer that reads the property carries a
	// caveat saying so. The contract makes declaring them a MUST; this asserts it.
	t.Run("SamplesDeclareUnitAndRange", func(t *testing.T) {
		c := factory(t)
		samples, _ := c.Collect()
		for _, s := range samples {
			if s.Unit == "" {
				t.Errorf("MetricSample.Unit must be non-empty; got %+v", s)
			}
			if s.Range == nil {
				t.Errorf("MetricSample.Range must be declared; got %+v", s)
				continue
			}
			if s.Range[1] <= s.Range[0] {
				t.Errorf("MetricSample.Range %v is empty; hi must exceed lo", *s.Range)
			}
		}
	})

	t.Run("SamplesMetricTypeInAvailable", func(t *testing.T) {
		c := factory(t)
		available := metricSet(c.AvailableMetrics())
		samples, _ := c.Collect()
		for _, s := range samples {
			if !available[s.MetricType] {
				t.Errorf("sample MetricType %q not declared in AvailableMetrics()", s.MetricType)
			}
		}
	})

	t.Run("EventIDDeterministic", func(t *testing.T) {
		// Collect twice; any overlapping EventIDs must refer to identical samples.
		// Implementations with advancing windows produce disjoint batches —
		// in that case the test trivially passes.
		c := factory(t)
		first := sampleMap(mustCollect(t, c))
		second := sampleMap(mustCollect(t, c))
		for eid, s1 := range first {
			s2, ok := second[eid]
			if !ok {
				continue
			}
			if s1.NodeID != s2.NodeID || s1.MetricType != s2.MetricType ||
				s1.Value != s2.Value || s1.TimestampUnix != s2.TimestampUnix {
				t.Errorf("EventID %q maps to different samples:\n  first:  %+v\n  second: %+v", eid, s1, s2)
			}
		}
	})
}

// ── helpers ───────────────────────────────────────────────────────────────────

func metricSet(ms []types.MetricType) map[types.MetricType]bool {
	out := make(map[types.MetricType]bool, len(ms))
	for _, m := range ms {
		out[m] = true
	}
	return out
}

func mustCollect(t *testing.T, c contracts.CollectorContract) []*types.MetricSample {
	t.Helper()
	samples, err := c.Collect()
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	return samples
}

func sampleMap(samples []*types.MetricSample) map[string]*types.MetricSample {
	out := make(map[string]*types.MetricSample, len(samples))
	for _, s := range samples {
		out[s.EventID] = s
	}
	return out
}
