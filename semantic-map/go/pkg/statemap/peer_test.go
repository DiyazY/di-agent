package statemap

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The property these tests exist to protect: an agent can hold another agent's state
// without that state ever becoming an answer about itself.

func TestPeerStateIsHeldSeparatelyFromLocalState(t *testing.T) {
	local, c := newTestMap(t, Config{Owner: "node-1", ConvergenceObservations: 10, Alpha: 0.5})
	if err := local.DeclareProperty(Property{
		ID: "cpu_utilization", Kind: Observed, Range: [2]float64{0, 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := local.Observe("cpu_utilization", 0.2, c.now()); err != nil {
		t.Fatal(err)
	}

	store := NewPeerStore("node-1", time.Minute)
	store.SetClock(c.now)

	// A peer reports the same property at a very different value. This is the case that
	// matters: the IDs collide because both machines run the same collectors.
	err := store.Record(PeerState{
		PeerID: "node-2", URL: "http://node-2:8080", FetchedAt: c.now(), Revision: 99,
		Properties: []Property{{
			ID: "cpu_utilization", Kind: Observed, Value: 0.9, Confidence: 0.8,
			NObservations: 400, Status: Active,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The local map must be untouched: same ID, different system, no interference.
	p, ok := local.Property("cpu_utilization")
	if !ok || p.Value != 0.2 {
		t.Errorf("local cpu_utilization is %.4f after recording a peer's 0.9; peer state "+
			"reached the local map, so an answer about this node can now be a claim "+
			"about another one", p.Value)
	}
	if local.Census().PropertiesTotal != 1 {
		t.Errorf("local map holds %d properties after a peer reported one; peer "+
			"properties were admitted locally", local.Census().PropertiesTotal)
	}

	// And the view keeps them apart while showing both.
	v := store.View(local.State(Query{}))
	if v.Self.Owner != "node-1" {
		t.Errorf("cluster view attributes local state to %q, want node-1: an unattributed "+
			"self view cannot be told apart from a peer's", v.Self.Owner)
	}
	if got := v.Peers["node-2"].Properties[0].Value; got != 0.9 {
		t.Errorf("peer cpu_utilization is %.4f in the cluster view, want 0.9", got)
	}
	if len(v.Self.Properties) != 1 || v.Self.Properties[0].Value != 0.2 {
		t.Errorf("self section of the cluster view does not hold the local value alone")
	}
}

func TestPeerStoreRefusesStateThatCannotBeAttributed(t *testing.T) {
	store := NewPeerStore("node-1", time.Minute)

	// Our own identity coming back at us — a proxy loop, or a peer misconfigured with
	// this node's id. Accepting it would double-count one machine in every cluster view.
	if err := store.Record(PeerState{PeerID: "node-1", URL: "http://self:8080"}); err == nil {
		t.Error("store accepted state claiming to be from this agent; one machine would " +
			"then appear twice in every cluster view")
	}

	// No owner at all: the properties are real but belong to nobody nameable.
	if err := store.Record(PeerState{URL: "http://node-9:8080"}); err == nil {
		t.Error("store accepted state naming no owner; a property whose subject is " +
			"unknown cannot be reasoned with")
	}
}

func TestUnreachablePeerIsReportedRatherThanForgotten(t *testing.T) {
	c := &clock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	store := NewPeerStore("node-1", 30*time.Second)
	store.SetClock(c.now)

	if err := store.Record(PeerState{
		PeerID: "node-2", URL: "http://node-2:8080", FetchedAt: c.now(),
		Properties: []Property{{ID: "cpu_utilization", Value: 0.5, Status: Active}},
	}); err != nil {
		t.Fatal(err)
	}

	c.advance(2 * time.Minute)
	store.RecordFailure("http://node-2:8080", errors.New("dial tcp: connection refused"))

	// What was last known survives the failure: during a partition it is the only thing
	// this agent has about that node.
	st, ok := store.Peer("node-2")
	if !ok || len(st.Properties) != 1 {
		t.Fatalf("a failed fetch discarded what was already known about the peer")
	}
	if st.Err == "" {
		t.Error("failed fetch left no error on the peer; a caller cannot tell 'said " +
			"nothing' from 'could not be asked'")
	}

	v := store.View(StateView{Owner: "node-1"})
	if len(v.Unreachable) != 1 || v.Unreachable[0] != "node-2" {
		t.Errorf("cluster view lists %v as unreachable, want [node-2]: a thin view would "+
			"otherwise read as a small cluster rather than a partitioned one", v.Unreachable)
	}

	// The snapshot is also old enough to be flagged as history.
	if stale := store.StaleView(); len(stale) != 1 || stale[0] != "node-2" {
		t.Errorf("StaleView returned %v; a two-minute-old snapshot under a 30s window "+
			"must be reported as stale rather than offered as current", stale)
	}

	// An address that has never answered is reported apart from a known-but-unreachable
	// peer: there is no state to preserve and no node to attribute it to, and treating
	// the two alike would put a node that may not exist into the cluster view.
	store.RecordFailure("http://node-7:8080", errors.New("no such host"))
	v = store.View(StateView{Owner: "node-1"})
	if _, ok := v.Silent["http://node-7:8080"]; !ok {
		t.Errorf("an address that never identified itself is missing from the silent list: %v",
			v.Silent)
	}
	if len(v.Peers) != 1 {
		t.Errorf("cluster view holds %d peers after a never-contacted address failed; a "+
			"node was invented from a URL", len(v.Peers))
	}

	// And a peer that answers again is no longer unreachable: the view describes the
	// present, not the worst thing that ever happened to that address.
	if err := store.Record(PeerState{
		PeerID: "node-2", URL: "http://node-2:8080", FetchedAt: c.now(),
		Properties: []Property{{ID: "cpu_utilization", Value: 0.6, Status: Active}},
	}); err != nil {
		t.Fatal(err)
	}
	if v = store.View(StateView{Owner: "node-1"}); len(v.Unreachable) != 0 {
		t.Errorf("peer still listed unreachable after answering: %v", v.Unreachable)
	}
}

func TestFindPropertyAnswersPerNodeRatherThanAggregating(t *testing.T) {
	c := &clock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	store := NewPeerStore("node-1", time.Minute)
	store.SetClock(c.now)

	for _, tc := range []struct {
		id    string
		value float64
	}{{"node-2", 0.9}, {"node-3", 0.1}} {
		if err := store.Record(PeerState{
			PeerID: tc.id, URL: "http://" + tc.id + ":8080", FetchedAt: c.now(),
			Properties: []Property{
				{ID: "cpu_utilization", Value: tc.value, Status: Active},
				{ID: "memory_utilization", Value: 0.5, Status: Active},
			},
		}); err != nil {
			t.Fatal(err)
		}
	}

	got := store.FindProperty("cpu_utilization")
	if len(got) != 2 {
		t.Fatalf("FindProperty returned %d answers, want one per peer", len(got))
	}
	// The mean of 0.9 and 0.1 is 0.5, which describes neither machine. Per-node answers
	// are what let a caller pick a node rather than average one out of existence.
	if got["node-2"].Value != 0.9 || got["node-3"].Value != 0.1 {
		t.Errorf("per-node values came back as %v; the point of asking peers is that the "+
			"answers stay attributed", got)
	}
	if len(store.FindProperty("disk_pressure")) != 0 {
		t.Error("FindProperty invented an answer for a property no peer reported")
	}
}

func TestSnapshotFromAnotherSystemIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	// node-2 saves a week of its own observations.
	far, c := newTestMap(t, Config{Owner: "node-2", ConvergenceObservations: 10, Alpha: 0.5})
	if err := far.DeclareProperty(Property{ID: "cpu_utilization", Kind: Observed}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		c.advance(time.Second)
		if err := far.Observe("cpu_utilization", 0.9, c.now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := far.Save(path); err != nil {
		t.Fatal(err)
	}

	// The file lands on node-1 — copied with a config directory, or baked into an image.
	local, _ := newTestMap(t, Config{Owner: "node-1"})
	ok, err := local.Load(path)
	if err == nil {
		t.Fatal("a snapshot describing node-2 loaded into node-1's map; another machine's " +
			"observations would now be this one's history, at full confidence")
	}
	if ok {
		t.Error("Load reported success while returning an error")
	}
	if local.Census().PropertiesTotal != 0 {
		t.Error("the refused load left properties behind: a partial adoption is worse " +
			"than either outcome, because it looks like local knowledge")
	}

	// An unowned snapshot still loads, so existing state files from before owners
	// existed are not orphaned by this check.
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	unowned := filepath.Join(dir, "unowned.json")
	if err := os.WriteFile(unowned, []byte(replaceOwner(string(blob))), 0o644); err != nil {
		t.Fatal(err)
	}
	fresh, _ := newTestMap(t, Config{Owner: "node-1"})
	if ok, err := fresh.Load(unowned); err != nil || !ok {
		t.Errorf("a snapshot written before owners were recorded failed to load: %v", err)
	}
}

// replaceOwner strips the owner field from a snapshot, standing in for a file written
// by an older build.
func replaceOwner(s string) string {
	return strings.Replace(s, `"owner": "node-2",`, "", 1)
}
