package agentmain

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
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

func TestDiscoveredPeersPropagateObjects(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var members []*gossip.Membership
	var workers []<-chan struct{}
	var servers []*httptest.Server
	t.Cleanup(func() {
		cancel()
		for _, worker := range workers {
			<-worker
		}
		for _, member := range members {
			if err := member.Close(); err != nil {
				t.Error(err)
			}
		}
		for _, server := range servers {
			server.Close()
		}
	})
	var seed string
	var stores []*gossip.Memory
	for i := range 2 {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		mux := http.NewServeMux()
		server := httptest.NewUnstartedServer(mux)
		servers = append(servers, server)
		cfg := gossip.MembershipConfig{Enabled: true, Site: "lab", BindAddr: "127.0.0.1",
			BindPort: port, AllowedCIDRs: []string{"127.0.0.0/8"}, MaxActivePeers: 1}
		if i == 0 {
			seed = net.JoinHostPort(cfg.BindAddr, fmt.Sprint(port))
		} else {
			cfg.Seeds = []string{seed}
		}
		member, err := gossip.NewMembership(cfg, fmt.Sprintf("agent-%d", i), server.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		members = append(members, member)
		store, registry := gossip.NewMemory(), peers.NewRegistry()
		stores = append(stores, store)
		client := gossip.NewMembershipClient(member)
		registerGossipRoutes(mux, fmt.Sprintf("agent-%d", i), registry, store, client, member)
		server.Start()
		workers = append(workers, member.Run(ctx))
		workers = append(workers, startGossipLoop(ctx, gossip.NewExchange(store, client), registry, 20*time.Millisecond, member))
	}
	data, err := gossip.Encode("agent-0", "announcement", json.RawMessage(`{"value":1}`))
	if err != nil {
		t.Fatal(err)
	}
	hash := gossip.Hash(data)
	if _, err := stores[0].Put(hash, data); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for !stores[1].Has(hash) {
		select {
		case <-deadline.C:
			t.Fatal("object did not propagate through discovered membership")
		case <-tick.C:
		}
	}
	received, _ := stores[1].Get(hash)
	if string(received) != string(data) {
		t.Fatal("discovered exchange changed object bytes")
	}
	for _, path := range []string{"/gossip/objects", "/gossip/objects/" + hash} {
		for _, tc := range []struct {
			remote, site string
			want         int
		}{
			{"127.0.0.1:1234", "lab", http.StatusOK},
			{"127.0.0.1:1234", "", http.StatusForbidden},
			{"127.0.0.1:1234", "other", http.StatusForbidden},
			{"192.0.2.1:1234", "lab", http.StatusForbidden},
		} {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.RemoteAddr = tc.remote
			request.Header.Set("X-Di-Gossip-Site", tc.site)
			response := httptest.NewRecorder()
			servers[0].Config.Handler.ServeHTTP(response, request)
			if response.Code != tc.want {
				t.Fatalf("%s from %s site %q: got %d, want %d", path, tc.remote, tc.site, response.Code, tc.want)
			}
		}
	}
}

func TestPeriodicGossipPropagatesThroughAnIntermediateAgent(t *testing.T) {
	a, b, c := newGossipNode(t, "node-a"), newGossipNode(t, "node-b"), newGossipNode(t, "node-c")
	before := []statemap.StateView{
		a.state.State(statemap.Query{}), b.state.State(statemap.Query{}), c.state.State(statemap.Query{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	bDone := startGossipLoop(ctx, gossip.NewExchange(b.store, gossip.NewClient()), b.peers, 10*time.Millisecond, nil)
	cDone := startGossipLoop(ctx, gossip.NewExchange(c.store, gossip.NewClient()), c.peers, 10*time.Millisecond, nil)
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
	bDone := startGossipLoop(ctx, gossip.NewExchange(b.store, gossip.NewClient()), b.peers, 10*time.Millisecond, nil)
	cDone := startGossipLoop(ctx, gossip.NewExchange(c.store, gossip.NewClient()), c.peers, 10*time.Millisecond, nil)
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
	done := startGossipLoop(ctx, gossip.NewExchange(gossip.NewMemory(), gossip.NewClient()), registry, time.Second, nil)
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
