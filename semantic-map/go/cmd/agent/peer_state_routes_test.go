package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/peers"
	"github.com/DiyazY/di-agent/pkg/statemap"
)

// nodeFixture is one agent: a map that models a single machine, and the HTTP surface
// that other agents reach it through.
func nodeFixture(t *testing.T, owner string) (*statemap.Map, *httptest.Server) {
	t.Helper()
	sm := statemap.New(statemap.Config{
		Owner:                   owner,
		ConvergenceObservations: 4,
		AdmitUnknown:            true,
	}, statemap.NewJournal(0))
	mux := http.NewServeMux()
	registerStateRoutes(mux, sm)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return sm, srv
}

// TestAgentAsksAPeerForItsStateAndKeepsItSeparate is the end-to-end shape of the
// decentralised claim: node-1 learns what node-2 knows without either map becoming a
// description of both machines.
func TestAgentAsksAPeerForItsStateAndKeepsItSeparate(t *testing.T) {
	// Two agents, same collectors, genuinely different systems.
	remote, remoteSrv := nodeFixture(t, "node-2")
	local, _ := nodeFixture(t, "node-1")

	now := time.Now()
	for i, m := range []struct {
		id           string
		hereAndThere [2]float64
	}{
		{"cpu_utilization", [2]float64{0.15, 0.88}},
		{"memory_utilization", [2]float64{0.40, 0.41}},
	} {
		if err := local.Observe(m.id, m.hereAndThere[0], now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := remote.Observe(m.id, m.hereAndThere[1], now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	// Something only the remote node has: a property that exists on one machine and not
	// the other is the ordinary case, and the exchange has to carry it.
	if err := remote.Observe("thermal_throttle_events", 3, now); err != nil {
		t.Fatal(err)
	}

	registry := peers.NewRegistry()
	if _, err := registry.Add(remoteSrv.URL, "node-2"); err != nil {
		t.Fatal(err)
	}
	store := statemap.NewPeerStore("node-1", time.Minute)
	fetcher := &peerStateFetcher{registry: registry, client: peers.NewClient(2 * time.Second), store: store}

	if ok, failed := fetcher.fetchAll(context.Background()); ok != 1 || failed != 0 {
		t.Fatalf("fetching one peer's state gave ok=%d failed=%d", ok, failed)
	}

	// The peer's state arrived under the peer's own name, taken from what it reported
	// rather than from the address it was reached at.
	st, ok := store.Peer("node-2")
	if !ok {
		t.Fatalf("no state held under node-2 after a successful fetch; the snapshot was " +
			"filed under something other than the owner the peer reported")
	}
	if st.Counts.PropertiesTotal != 3 {
		t.Errorf("peer reported %d properties, want 3: the exchange dropped part of the map",
			st.Counts.PropertiesTotal)
	}

	// The local map is untouched — the point of the whole arrangement.
	if p, _ := local.Property("cpu_utilization"); p.Value != 0.15 {
		t.Errorf("local cpu_utilization became %.4f after fetching a peer that reports 0.88; "+
			"remote state reached the local model", p.Value)
	}
	if _, exists := local.Property("thermal_throttle_events"); exists {
		t.Error("a property only the peer has appeared in the local map; the local map " +
			"now claims something about this machine that was never observed on it")
	}

	// And the operator surface answers the cross-node question per node.
	mux := http.NewServeMux()
	registerPeerStateRoutes(mux, local, store, fetcher)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	var where struct {
		Property string `json:"property"`
		Holders  []struct {
			Node       string  `json:"node"`
			Local      bool    `json:"local"`
			Value      float64 `json:"value"`
			AgeSeconds float64 `json:"age_seconds"`
		} `json:"holders"`
	}
	if code := getState(t, srv.URL+"/state/where?property=cpu_utilization", &where); code != 200 {
		t.Fatalf("GET /state/where returned %d", code)
	}
	if len(where.Holders) != 2 {
		t.Fatalf("/state/where returned %d holders, want this node and the peer", len(where.Holders))
	}
	byNode := map[string]float64{}
	for _, h := range where.Holders {
		byNode[h.Node] = h.Value
		if h.Node == "node-2" && h.Local {
			t.Error("a peer's answer is labelled local; the labelling is what stops a " +
				"remote value being read as an observation of this machine")
		}
		if h.Node == "node-2" && h.AgeSeconds == 0 {
			t.Error("a peer's answer carries no age; a snapshot's worth depends on it")
		}
	}
	if byNode["node-1"] != 0.15 || byNode["node-2"] != 0.88 {
		t.Errorf("per-node values came back as %v; the two machines' answers were mixed", byNode)
	}

	// A property nobody has is an empty answer, not a zero.
	var missing struct {
		Holders []struct{} `json:"holders"`
	}
	if code := getState(t, srv.URL+"/state/where?property=nothing_reports_this", &missing); code != 200 {
		t.Fatalf("GET /state/where for an unheld property returned %d", code)
	}
	if len(missing.Holders) != 0 {
		t.Error("/state/where invented a holder for a property no node reports")
	}
}

// TestClusterViewSaysWhoIsMissing covers the case an operator actually debugs: a peer
// that stops answering. A thin cluster view has to be distinguishable from a small
// cluster.
func TestClusterViewSaysWhoIsMissing(t *testing.T) {
	remote, remoteSrv := nodeFixture(t, "node-2")
	local, _ := nodeFixture(t, "node-1")
	if err := remote.Observe("cpu_utilization", 0.7, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := local.Observe("cpu_utilization", 0.2, time.Now()); err != nil {
		t.Fatal(err)
	}

	registry := peers.NewRegistry()
	if _, err := registry.Add(remoteSrv.URL, "node-2"); err != nil {
		t.Fatal(err)
	}
	// A second peer at an address nothing serves.
	if _, err := registry.Add("http://127.0.0.1:1", "node-3"); err != nil {
		t.Fatal(err)
	}

	store := statemap.NewPeerStore("node-1", time.Minute)
	fetcher := &peerStateFetcher{registry: registry, client: peers.NewClient(time.Second), store: store}
	if ok, failed := fetcher.fetchAll(context.Background()); ok != 1 || failed != 1 {
		t.Fatalf("one live and one dead peer gave ok=%d failed=%d", ok, failed)
	}

	mux := http.NewServeMux()
	registerPeerStateRoutes(mux, local, store, fetcher)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	var body struct {
		Cluster struct {
			SelfID string                        `json:"self_id"`
			Self   statemap.StateView            `json:"self"`
			Peers  map[string]statemap.PeerState `json:"peers"`
			Silent map[string]string             `json:"silent"`
		} `json:"cluster"`
	}
	if code := getState(t, srv.URL+"/state/cluster", &body); code != 200 {
		t.Fatalf("GET /state/cluster returned %d", code)
	}
	if body.Cluster.Self.Owner != "node-1" || body.Cluster.SelfID != "node-1" {
		t.Errorf("cluster view attributes local state to %q/%q, want node-1",
			body.Cluster.Self.Owner, body.Cluster.SelfID)
	}
	if len(body.Cluster.Peers) != 1 {
		t.Errorf("cluster view holds %d peers; the unreachable address must not become a "+
			"node, since nothing has ever identified itself there", len(body.Cluster.Peers))
	}
	if len(body.Cluster.Silent) != 1 {
		t.Errorf("cluster view reports %d silent addresses, want 1: an operator reading a "+
			"two-node view of a three-address cluster has to be told which one never "+
			"answered", len(body.Cluster.Silent))
	}
}

// TestPeerStateWithoutAnOwnerIsRefused covers an agent started without a node
// identity. Its state is real but cannot be attributed, and unattributed properties
// are the one kind that could quietly be read as somebody else's.
func TestPeerStateWithoutAnOwnerIsRefused(t *testing.T) {
	anon, anonSrv := nodeFixture(t, "") // no -node-id
	if err := anon.Observe("cpu_utilization", 0.5, time.Now()); err != nil {
		t.Fatal(err)
	}

	client := peers.NewClient(time.Second)
	if _, err := client.State(context.Background(), anonSrv.URL); err == nil {
		t.Fatal("state from an agent with no identity was accepted; its properties would " +
			"then sit in the cluster view belonging to nobody")
	}

	registry := peers.NewRegistry()
	if _, err := registry.Add(anonSrv.URL, "anonymous"); err != nil {
		t.Fatal(err)
	}
	store := statemap.NewPeerStore("node-1", time.Minute)
	fetcher := &peerStateFetcher{registry: registry, client: client, store: store}
	if ok, failed := fetcher.fetchAll(context.Background()); ok != 0 || failed != 1 {
		t.Fatalf("fetching from an unidentified agent gave ok=%d failed=%d", ok, failed)
	}
	if len(store.View(statemap.StateView{}).Peers) != 0 {
		t.Error("unattributed state was stored anyway")
	}
}
