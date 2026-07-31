package semmap

import (
	"testing"

	"github.com/DiyazY/di-agent/internal/minimal"
	"github.com/DiyazY/di-agent/pkg/types"
)

// TestPairTracker_OnlyPairsWithinTheWindow pins the tolerance that makes pairing
// possible at all. Collectors sample on independent grids — in the dissertation
// testbed system.* lands on 0, 5, 10 … and PSI on 2, 6, 12 …, so the two never
// share a timestamp — but a tolerance wide enough to pair anything is also wide
// enough to relate readings from different operating regimes, so the boundary
// has to be exact.
func TestPairTracker_OnlyPairsWithinTheWindow(t *testing.T) {
	tr := newPairTracker(15)

	tr.record("RC", constructObservation{value: 0.4, ts: 1000, eventID: "rc-1"})

	if fresh := tr.record("PS", constructObservation{value: 0.1, ts: 1012, eventID: "ps-1"}); len(fresh) != 1 {
		t.Errorf("12s apart is inside a 15s window but produced %d counterparts", len(fresh))
	}
	if fresh := tr.record("PS", constructObservation{value: 0.1, ts: 1100, eventID: "ps-2"}); len(fresh) != 0 {
		t.Errorf("88s after the RC reading is outside the window but produced %d counterparts", len(fresh))
	}
	// Order does not matter: an earlier counterpart is as valid as a later one.
	tr.record("CO", constructObservation{value: 0.2, ts: 1105, eventID: "co-1"})
	if fresh := tr.record("PS", constructObservation{value: 0.1, ts: 1095, eventID: "ps-3"}); len(fresh) == 0 {
		t.Error("a counterpart 10s in the future should still pair; the window is symmetric")
	}
}

// TestPairTracker_KeepsOnlyTheLatestPerConstruct guards against inflating support
// without new evidence: if the tracker kept a history, one slow-arriving
// counterpart would pair with every fast arrival and the correlation window would
// fill with repeats of the same point.
func TestPairTracker_KeepsOnlyTheLatestPerConstruct(t *testing.T) {
	tr := newPairTracker(60)
	tr.record("RC", constructObservation{value: 0.1, ts: 1000, eventID: "rc-1"})
	tr.record("RC", constructObservation{value: 0.9, ts: 1005, eventID: "rc-2"})

	fresh := tr.record("PS", constructObservation{value: 0.5, ts: 1010, eventID: "ps-1"})
	if len(fresh) != 1 {
		t.Fatalf("expected one counterpart construct, got %d", len(fresh))
	}
	if got := fresh["RC"]; got.value != 0.9 || got.eventID != "rc-2" {
		t.Errorf("paired against %v (%s); want the most recent RC reading 0.9 (rc-2)",
			got.value, got.eventID)
	}
}

// TestPairEventID_IsOrderIndependent matters for idempotency: the same physical
// pair must get one identity whichever sample arrived first, or a replay in a
// different interleaving would look like new evidence.
func TestPairEventID_IsOrderIndependent(t *testing.T) {
	a := pairEventID("RC", "PS", "cpu-7", "psi-7")
	b := pairEventID("RC", "PS", "psi-7", "cpu-7")
	if a != b {
		t.Errorf("pair identity depends on arrival order: %q vs %q", a, b)
	}
	if same := pairEventID("PS", "RC", "cpu-7", "psi-7"); same == a {
		t.Error("pairs on different edges share an identity; idempotency would suppress one of them")
	}
}

// relStub records the paired calls ingestPaired makes.
type relStub struct {
	calls []relCall
}

type relCall struct {
	from, to           string
	fromValue, toValue float64
	eventID            string
}

func (r *relStub) UpdateEdgeRelation(from, to string, fv, tv float64, eventID string) (*types.EdgeDescriptor, error) {
	r.calls = append(r.calls, relCall{from, to, fv, tv, eventID})
	return &types.EdgeDescriptor{FromID: from, ToID: to}, nil
}
func (r *relStub) UpdateEdge(from, to string, obs float64, eventID string) (*types.EdgeDescriptor, error) {
	return &types.EdgeDescriptor{FromID: from, ToID: to}, nil
}
func (r *relStub) UpdateNode(id string, obs float64, eventID string) (*types.NodeDescriptor, error) {
	return &types.NodeDescriptor{NodeID: id}, nil
}
func (r *relStub) Reset(from, to string) error { return nil }

// TestIngestPaired_OrdersValuesByEdgeDirection is the correctness property that
// makes the learned sign meaningful. The correlation of (x, y) and (y, x) has the
// same sign, but the reasoner reads the edge as "from influences to", and an
// implementation that fed the arriving sample as `fromValue` regardless would
// silently transpose every edge whose target was sampled.
func TestIngestPaired_OrdersValuesByEdgeDirection(t *testing.T) {
	spec := mustSpec()
	ontology := minimal.NewOntologyFromSpec(spec)
	tracker := newPairTracker(15)
	upd := &relStub{}

	// Seed an RC observation, then arrive with a PS one. RC→PS edges must receive
	// (RC value, PS value) in that order even though PS is the arriving sample.
	rc := &types.MetricSample{NodeID: "n1", MetricType: types.CPUUtilization,
		Value: 0.42, TimestampUnix: 1000, EventID: "rc-1"}
	if _, err := ingestPaired(rc, "RC", ontology, upd, tracker); err != nil {
		t.Fatal(err)
	}
	if len(upd.calls) != 0 {
		t.Fatalf("first sample had no counterpart but produced %d paired updates", len(upd.calls))
	}

	ps := &types.MetricSample{NodeID: "n1", MetricType: types.CPUPressureRatio,
		Value: 0.07, TimestampUnix: 1004, EventID: "ps-1"}
	if _, err := ingestPaired(ps, "PS", ontology, upd, tracker); err != nil {
		t.Fatal(err)
	}
	if len(upd.calls) == 0 {
		t.Fatal("a fresh RC counterpart existed but no paired update was made")
	}
	for _, c := range upd.calls {
		switch {
		case c.from == "RC" && c.to == "PS":
			if c.fromValue != 0.42 || c.toValue != 0.07 {
				t.Errorf("RC→PS received (%v, %v); want (0.42, 0.07) — source value first",
					c.fromValue, c.toValue)
			}
		case c.from == "PS" && c.to == "RC":
			if c.fromValue != 0.07 || c.toValue != 0.42 {
				t.Errorf("PS→RC received (%v, %v); want (0.07, 0.42) — source value first",
					c.fromValue, c.toValue)
			}
		default:
			t.Errorf("unexpected edge %s→%s: neither endpoint is the sampled construct", c.from, c.to)
		}
	}
}

// TestIngestPaired_SkipsEdgesWithNoFreshCounterpart records the normal case that
// must not be mistaken for a failure: early in a run, or when one collector is
// slower than the pairing window, an edge simply gets no evidence.
func TestIngestPaired_SkipsEdgesWithNoFreshCounterpart(t *testing.T) {
	spec := mustSpec()
	ontology := minimal.NewOntologyFromSpec(spec)
	tracker := newPairTracker(5)
	upd := &relStub{}

	rc := &types.MetricSample{NodeID: "n1", MetricType: types.CPUUtilization,
		Value: 0.42, TimestampUnix: 1000, EventID: "rc-1"}
	_, _ = ingestPaired(rc, "RC", ontology, upd, tracker)

	stale := &types.MetricSample{NodeID: "n1", MetricType: types.CPUPressureRatio,
		Value: 0.07, TimestampUnix: 1060, EventID: "ps-late"}
	n, err := ingestPaired(stale, "PS", ontology, upd, tracker)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || len(upd.calls) != 0 {
		t.Errorf("a counterpart 60s stale in a 5s window produced %d paired updates", n)
	}
}
