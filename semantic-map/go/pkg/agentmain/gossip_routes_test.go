package agentmain

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/gossip"
	"github.com/DiyazY/di-agent/pkg/peers"
	"github.com/DiyazY/di-agent/pkg/statemap"
)

func gossipRequest(t *testing.T, method, url, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, data
}

func TestGossipTwoAgentsExchangeObjectsWithoutChangingLocalState(t *testing.T) {
	type node struct {
		url   string
		state *statemap.Map
		store *gossip.Memory
		peers *peers.Registry
	}
	newNode := func(id string) node {
		state := statemap.New(statemap.Config{Owner: id, AdmitUnknown: true}, statemap.NewJournal(0))
		if err := state.Observe("cpu_utilization", 0.2, time.Now()); err != nil {
			t.Fatal(err)
		}
		store, registry := gossip.NewMemory(), peers.NewRegistry()
		mux := http.NewServeMux()
		registerStateRoutes(mux, state, nil, nil)
		registerGossipRoutes(mux, id, registry, store, gossip.NewClient())
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		return node{srv.URL, state, store, registry}
	}
	a, b := newNode("node-a"), newNode("node-b")
	beforeA, beforeB := a.state.State(statemap.Query{}), b.state.State(statemap.Query{})
	code, response := gossipRequest(t, "POST", a.url+"/gossip/objects",
		`{"payload_type":"announcement","payload":{"capability":"compute"}}`)
	var published struct {
		Hash  string `json:"hash"`
		Added bool   `json:"added"`
	}
	if err := json.Unmarshal(response, &published); err != nil || code != 200 || !published.Added {
		t.Fatalf("publish: code=%d body=%s err=%v", code, response, err)
	}
	peer, err := b.peers.Add(a.url, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, wantAdded := range []bool{true, false} {
		code, response = gossipRequest(t, "POST", b.url+"/gossip/objects/"+published.Hash+"/fetch",
			`{"peer_id":"`+peer.ID+`"}`)
		var received struct {
			Hash  string `json:"hash"`
			Added bool   `json:"added"`
		}
		if err := json.Unmarshal(response, &received); err != nil || code != 200 ||
			received.Hash != published.Hash || received.Added != wantAdded {
			t.Fatalf("fetch: code=%d body=%s err=%v", code, response, err)
		}
	}
	code, transferred := gossipRequest(t, "GET", b.url+"/gossip/objects/"+published.Hash, "")
	original, _ := a.store.Get(published.Hash)
	if code != 200 || !bytes.Equal(original, transferred) || gossip.Hash(transferred) != published.Hash {
		t.Fatal("transfer did not preserve the exact hashed bytes")
	}
	var object gossip.Object
	if err := json.Unmarshal(transferred, &object); err != nil || object.OriginID != "node-a" {
		t.Fatalf("origin lost: %+v err=%v", object, err)
	}
	if len(b.store.Inventory().Hashes) != 1 {
		t.Fatal("duplicate delivery created another object")
	}
	if !reflect.DeepEqual(beforeA, a.state.State(statemap.Query{})) ||
		!reflect.DeepEqual(beforeB, b.state.State(statemap.Query{})) {
		t.Fatal("object exchange changed local semantic state")
	}
}

func TestGossipRoutesRefuseInvalidRequestsAndCorruptPeers(t *testing.T) {
	store, registry := gossip.NewMemory(), peers.NewRegistry()
	mux := http.NewServeMux()
	registerGossipRoutes(mux, "node-b", registry, store, gossip.NewClient())
	srv := httptest.NewServer(mux)
	defer srv.Close()
	data, err := gossip.Encode("node-a", "announcement", json.RawMessage(`true`))
	if err != nil {
		t.Fatal(err)
	}
	hash := gossip.Hash(data)
	badPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(append(data, '\n'))
	}))
	defer badPeer.Close()
	peer, err := registry.Add(badPeer.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path, body string
		want             int
	}{
		{"missing payload", "/gossip/objects", `{"payload_type":"x"}`, 400},
		{"forged origin", "/gossip/objects", `{"origin_id":"other","payload_type":"x","payload":true}`, 400},
		{"trailing JSON", "/gossip/objects", `{"payload_type":"x","payload":true}{}`, 400},
		{"oversized", "/gossip/objects", `{"payload_type":"x","payload":"` + strings.Repeat("x", gossip.MaxObjectBytes) + `"}`, 413},
		{"missing peer", "/gossip/objects/" + hash + "/fetch", `{}`, 400},
		{"unknown peer", "/gossip/objects/" + hash + "/fetch", `{"peer_id":"unknown"}`, 404},
		{"arbitrary URL", "/gossip/objects/" + hash + "/fetch", `{"url":"http://example.com"}`, 400},
		{"bad hash", "/gossip/objects/not-a-hash/fetch", `{"peer_id":"` + peer.ID + `"}`, 400},
		{"corrupt peer", "/gossip/objects/" + hash + "/fetch", `{"peer_id":"` + peer.ID + `"}`, 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, body := gossipRequest(t, "POST", srv.URL+tc.path, tc.body); code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", code, tc.want, body)
			}
		})
	}
	if len(store.Inventory().Hashes) != 0 {
		t.Fatal("invalid request admitted content")
	}
	req := httptest.NewRequest("POST", "/gossip/objects", strings.NewReader(`{"payload_type":"x","payload":true}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatal("mutation accepted without JSON Content-Type")
	}
}
