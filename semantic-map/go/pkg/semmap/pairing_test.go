package semmap

import (
	"errors"
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

	tr.record("node_1", "RC", constructObservation{value: 0.4, ts: 1000, eventID: "rc-1"})

	if fresh := tr.record("node_1", "PS", constructObservation{value: 0.1, ts: 1012, eventID: "ps-1"}); len(fresh) != 1 {
		t.Errorf("12s apart is inside a 15s window but produced %d counterparts", len(fresh))
	}
	if fresh := tr.record("node_1", "PS", constructObservation{value: 0.1, ts: 1100, eventID: "ps-2"}); len(fresh) != 0 {
		t.Errorf("88s after the RC reading is outside the window but produced %d counterparts", len(fresh))
	}
	// Order does not matter: an earlier counterpart is as valid as a later one.
	tr.record("node_1", "CO", constructObservation{value: 0.2, ts: 1105, eventID: "co-1"})
	if fresh := tr.record("node_1", "PS", constructObservation{value: 0.1, ts: 1095, eventID: "ps-3"}); len(fresh) == 0 {
		t.Error("a counterpart 10s in the future should still pair; the window is symmetric")
	}
}

// TestPairTracker_KeepsOnlyTheLatestPerConstruct guards against inflating support
// without new evidence: if the tracker kept a history, one slow-arriving
// counterpart would pair with every fast arrival and the correlation window would
// fill with repeats of the same point.
func TestPairTracker_KeepsOnlyTheLatestPerConstruct(t *testing.T) {
	tr := newPairTracker(60)
	tr.record("node_1", "RC", constructObservation{value: 0.1, ts: 1000, eventID: "rc-1"})
	tr.record("node_1", "RC", constructObservation{value: 0.9, ts: 1005, eventID: "rc-2"})

	fresh := tr.record("node_1", "PS", constructObservation{value: 0.5, ts: 1010, eventID: "ps-1"})
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

// TestPairTracker_DoesNotPairAcrossNodes pins the property that keeps a learned
// association mechanical. A cluster streams every node's telemetry into one
// graph, so a tracker keyed on construct alone pairs the master's CPU reading
// with a worker's pressure reading — two series with no causal connection — and
// the edge learns from noise. The edge stays cluster-wide; each pair does not.
func TestPairTracker_DoesNotPairAcrossNodes(t *testing.T) {
	tr := newPairTracker(15)

	tr.record("master", "RC", constructObservation{value: 0.9, ts: 1000, eventID: "m-rc"})
	if fresh := tr.record("node_1", "PS", constructObservation{value: 0.1, ts: 1002, eventID: "n1-ps"}); len(fresh) != 0 {
		t.Errorf("paired a node_1 reading against master's: %v", fresh)
	}

	tr.record("node_1", "RC", constructObservation{value: 0.2, ts: 1004, eventID: "n1-rc"})
	fresh := tr.record("node_1", "PS", constructObservation{value: 0.1, ts: 1006, eventID: "n1-ps2"})
	if len(fresh) != 1 {
		t.Fatalf("same-node counterpart not offered: %v", fresh)
	}
	if got := fresh["RC"]; got.eventID != "n1-rc" {
		t.Errorf("paired against %q; want node_1's own RC reading", got.eventID)
	}
}

// TestIngestSample_RejectsForeignSamplesInSelfScope pins the boundary that makes
// the map node-local. Without it, a daemon silently accumulates whatever telemetry
// arrives, and its edge weights become means over machines that may be different
// physical systems — the testbed pairs an x86 control-plane host with Cortex-A72
// workers, which do not share a resource-to-pressure relation. The whole-testbed
// replay is the legitimate exception and has to ask for it.
func TestIngestSample_RejectsForeignSamplesInSelfScope(t *testing.T) {
	sm, _ := newIdentityMap(t)
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
	if !errors.Is(err, ErrForeignSample) {
		t.Errorf("got %v; want ErrForeignSample so a caller can tell configuration "+
			"from a transient failure", err)
	}

	// An unlabelled sample is this machine's by default: collectors that do not
	// stamp an ID are not thereby foreign.
	unlabelled := &types.MetricSample{MetricType: types.CPUUtilization,
		Value: 0.4, TimestampUnix: 1002, EventID: "unlabelled-1"}
	if err := sm.IngestSample(unlabelled); err != nil {
		t.Errorf("unlabelled sample rejected: %v", err)
	}
}

// TestIngestSample_AggregatesWhenAsked covers the replay case: one daemon, a whole
// testbed's telemetry, explicitly requested.
func TestIngestSample_AggregatesWhenAsked(t *testing.T) {
	sm, _ := newIdentityMap(t)
	sm.SetIdentity("node_1", true)

	for i, host := range []string{"node_1", "node_2", "master"} {
		s := &types.MetricSample{NodeID: host, MetricType: types.CPUUtilization,
			Value: 0.3, TimestampUnix: int64(1000 + i), EventID: "evt-" + host}
		if err := sm.IngestSample(s); err != nil {
			t.Errorf("sample from %s rejected with ingest-scope=any: %v", host, err)
		}
	}
}

// TestIngestSample_RecordsConstructStateInBothModes pins the state RQ4 identified
// as missing from the cost path. A construct's observed magnitude belongs on its
// own descriptor — which, on a node-local map, is this machine's current value for
// that construct — and it was previously written only when the updater happened to
// be relational.
func TestIngestSample_RecordsConstructStateInBothModes(t *testing.T) {
	for _, mode := range []string{"endpoint", "relational"} {
		t.Run(mode, func(t *testing.T) {
			sm, storage := newIdentityMap(t)
			if mode == "relational" {
				sm2, st2 := newRelationalMap(t)
				sm, storage = sm2, st2
			}
			sm.SetIdentity("node_1", false)

			s := &types.MetricSample{NodeID: "node_1", MetricType: types.CPUUtilization,
				Value: 0.42, TimestampUnix: 1000, EventID: "state-1"}
			if err := sm.IngestSample(s); err != nil {
				t.Fatal(err)
			}
			node, err := storage.GetNode("RC")
			if err != nil {
				t.Fatal(err)
			}
			if node == nil {
				t.Fatal("no descriptor for the routed construct")
			}
			if node.NObservations != 1 {
				t.Errorf("construct RC recorded %d observations; want 1 — without this the "+
					"reasoner cannot read the state it is estimating", node.NObservations)
			}
		})
	}
}
