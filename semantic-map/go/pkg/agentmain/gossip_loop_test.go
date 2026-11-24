package agentmain

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/gossip"
	"github.com/DiyazY/di-agent/pkg/peers"
	"github.com/DiyazY/di-agent/pkg/statemap"
)

func TestPeriodicGossipPropagatesThroughAnIntermediateAgent(t *testing.T) {
	a, b, c := newGossipNode(t, "node-a"), newGossipNode(t, "node-b"), newGossipNode(t, "node-c")
	before := []statemap.StateView{
		a.state.State(statemap.Query{}), b.state.State(statemap.Query{}), c.state.State(statemap.Query{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	bDone := startGossipLoop(ctx, gossip.NewExchange(b.store, gossip.NewClient()), b.peers, 10*time.Millisecond)
	cDone := startGossipLoop(ctx, gossip.NewExchange(c.store, gossip.NewClient()), c.peers, 10*time.Millisecond)
	t.Cleanup(func() {
		cancel()
		<-bDone
		<-cDone
	})
	// Added after the loops start: membership is read from the live registry.
	if _, err := b.peers.Add(a.url, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.peers.Add(b.url, ""); err != nil {
		t.Fatal(err)
	}
	data, err := gossip.Encode("node-a", "announcement", json.RawMessage(`{"capability":"compute"}`))
	if err != nil {
		t.Fatal(err)
	}
	hash := gossip.Hash(data)
	if _, err := a.store.Put(hash, data); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for !c.store.Has(hash) {
		select {
		case <-deadline.C:
			t.Fatal("object did not propagate A -> B -> C")
		case <-tick.C:
		}
	}
	transferred, _ := c.store.Get(hash)
	if string(transferred) != string(data) {
		t.Fatal("multi-hop propagation changed wire bytes or origin")
	}
	known, err := c.peers.List()
	if err != nil || len(known) != 1 || known[0].URL != b.url {
		t.Fatalf("C must know only B, not A: %+v err=%v", known, err)
	}
	for i, n := range []gossipNode{a, b, c} {
		if !reflect.DeepEqual(before[i], n.state.State(statemap.Query{})) {
			t.Fatalf("gossip changed node %d's semantic state", i)
		}
	}
}

func TestPeriodicGossipRecoversAfterPartition(t *testing.T) {
	a, b, c := newGossipNode(t, "node-a"), newGossipNode(t, "node-b"), newGossipNode(t, "node-c")
	target, err := url.Parse(a.url)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	var partitioned atomic.Bool
	var blocked atomic.Int32
	partitioned.Store(true)
	link := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if partitioned.Load() {
			blocked.Add(1)
			http.Error(w, "partitioned", http.StatusServiceUnavailable)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(link.Close)
	if _, err := b.peers.Add(link.URL, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.peers.Add(b.url, ""); err != nil {
		t.Fatal(err)
	}
	publish := func(node gossipNode, origin, payload string) string {
		t.Helper()
		data, err := gossip.Encode(origin, "announcement", json.RawMessage(payload))
		if err != nil {
			t.Fatal(err)
		}
		hash := gossip.Hash(data)
		if _, err := node.store.Put(hash, data); err != nil {
			t.Fatal(err)
		}
		return hash
	}
	fromA := publish(a, "node-a", `{"during":"partition"}`)
	fromB := publish(b, "node-b", `{"during":"partition"}`)
	ctx, cancel := context.WithCancel(context.Background())
	bDone := startGossipLoop(ctx, gossip.NewExchange(b.store, gossip.NewClient()), b.peers, 10*time.Millisecond)
	cDone := startGossipLoop(ctx, gossip.NewExchange(c.store, gossip.NewClient()), c.peers, 10*time.Millisecond)
	t.Cleanup(func() {
		cancel()
		<-bDone
		<-cDone
	})
	wait := func(condition func() bool, failure string) {
		t.Helper()
		deadline := time.NewTimer(3 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for !condition() {
			select {
			case <-deadline.C:
				t.Fatal(failure)
			case <-tick.C:
			}
		}
	}
	wait(func() bool { return blocked.Load() > 0 && c.store.Has(fromB) }, "partition or B -> C transfer not exercised")
	if b.store.Has(fromA) || c.store.Has(fromA) {
		t.Fatal("object crossed the partition")
	}
	partitioned.Store(false)
	wait(func() bool { return b.store.Has(fromA) && c.store.Has(fromA) }, "A -> B -> C did not recover")
	original, _ := a.store.Get(fromA)
	received, _ := c.store.Get(fromA)
	if string(original) != string(received) || !c.store.Has(fromB) || len(c.store.Inventory().Hashes) != 2 {
		t.Fatal("recovery lost, changed, or duplicated an object")
	}
}

func TestGossipCancellationStopsAnInFlightRound(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()
	registry := peers.NewRegistry()
	if _, err := registry.Add(srv.URL, ""); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startGossipLoop(ctx, gossip.NewExchange(gossip.NewMemory(), gossip.NewClient()), registry, time.Second)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("loop did not start a round")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not stop in-flight exchange")
	}
}
