package minimal

import (
	"strings"

	"github.com/DiyazY/di-agent/pkg/contracts"
	"github.com/DiyazY/di-agent/pkg/types"
)

// MultiCollector fans out Collect() to multiple CollectorContract instances
// and merges their results into a single slice. The first error from any
// collector stops the fan-out and is returned to the caller.
//
// AvailableMetrics is the deduplicated union of all member collectors' types.
// SourceID is the concatenation of all member SourceIDs joined by commas and
// prefixed with "multi:".
type MultiCollector struct {
	collectors []contracts.CollectorContract
	sid        string
	avail      []types.MetricType
}

// NewMultiCollector constructs a MultiCollector from the given collectors.
// Ordering is preserved; duplicated MetricTypes are deduplicated (first
// occurrence wins in AvailableMetrics output order).
func NewMultiCollector(collectors ...contracts.CollectorContract) *MultiCollector {
	ids := make([]string, len(collectors))
	seen := make(map[types.MetricType]struct{})
	var avail []types.MetricType
	for i, c := range collectors {
		ids[i] = c.SourceID()
		for _, mt := range c.AvailableMetrics() {
			if _, ok := seen[mt]; !ok {
				seen[mt] = struct{}{}
				avail = append(avail, mt)
			}
		}
	}
	return &MultiCollector{
		collectors: collectors,
		sid:        "multi:" + strings.Join(ids, ","),
		avail:      avail,
	}
}

func (m *MultiCollector) SourceID() string                     { return m.sid }
func (m *MultiCollector) AvailableMetrics() []types.MetricType { return m.avail }

// Collect calls every member collector in order and merges results.
// The first non-nil error halts the fan-out and the partial results collected
// so far are returned alongside the error.
func (m *MultiCollector) Collect() ([]*types.MetricSample, error) {
	var out []*types.MetricSample
	for _, c := range m.collectors {
		samples, err := c.Collect()
		if err != nil {
			return out, err
		}
		out = append(out, samples...)
	}
	return out, nil
}
