package gossip

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

func objectServer(t *testing.T, store *Memory) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /gossip/objects", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(store.Inventory())
	})
	mux.HandleFunc("GET /gossip/objects/{hash}", func(w http.ResponseWriter, r *http.Request) {
		data, ok := store.Get(r.PathValue("hash"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestExchangeBoundsRoundsAndEventuallyFetchesMissingObjects(t *testing.T) {
	remote := NewMemory()
	for i := range MaxFetchesPerRound*2 + 1 {
		data := objectBytes(t, fmt.Sprintf("%d", i))
		if _, err := remote.Put(Hash(data), data); err != nil {
			t.Fatal(err)
		}
	}
	srv := objectServer(t, remote)
	local := NewMemory()
	exchange := NewExchange(local, NewClient())
	for i := range 3 {
		result, err := exchange.Round(context.Background(), []string{srv.URL})
		if err != nil || result.Attempted > MaxFetchesPerRound || result.Added == 0 {
			t.Fatalf("round %d: %+v err=%v", i, result, err)
		}
	}
	if !slices.Equal(local.Inventory().Hashes, remote.Inventory().Hashes) {
		t.Fatal("bounded rounds did not converge")
	}
	result, err := exchange.Round(context.Background(), []string{srv.URL})
	if err != nil || result.Attempted != 0 || result.Added != 0 {
		t.Fatalf("unchanged inventory triggered downloads: %+v err=%v", result, err)
	}
}

func TestExchangeRotatesPeersAndRetiresRemovedCursors(t *testing.T) {
	local := NewMemory()
	exchange := NewExchange(local, NewClient())
	var urls []string
	for i := range MaxPeersPerRound + 1 {
		remote := NewMemory()
		data := objectBytes(t, fmt.Sprintf("%d", i))
		if _, err := remote.Put(Hash(data), data); err != nil {
			t.Fatal(err)
		}
		urls = append(urls, objectServer(t, remote).URL)
	}
	result, err := exchange.Round(context.Background(), urls)
	if err != nil || result.Peers != MaxPeersPerRound || len(local.Inventory().Hashes) != MaxPeersPerRound {
		t.Fatalf("first round exceeded peer budget: %+v err=%v", result, err)
	}
	if _, err := exchange.Round(context.Background(), urls); err != nil {
		t.Fatal(err)
	}
	if len(local.Inventory().Hashes) != len(urls) {
		t.Fatal("rotation omitted a peer")
	}
	if _, err := exchange.Round(context.Background(), urls[:1]); err != nil {
		t.Fatal(err)
	}
	if len(exchange.offsets) != 1 {
		t.Fatal("removed peers retained cursors")
	}
}

func TestCorruptAdvertisementsConsumeBudgetWithoutStarvingOtherObjects(t *testing.T) {
	remote := NewMemory()
	for i := range MaxFetchesPerRound + 1 {
		data := objectBytes(t, fmt.Sprintf("%d", i))
		if _, err := remote.Put(Hash(data), data); err != nil {
			t.Fatal(err)
		}
	}
	inventory := remote.Inventory()
	healthyHash := inventory.Hashes[len(inventory.Hashes)-1]
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gossip/objects" {
			_ = json.NewEncoder(w).Encode(inventory)
			return
		}
		requests.Add(1)
		if r.URL.Path == "/gossip/objects/"+healthyHash {
			data, _ := remote.Get(healthyHash)
			_, _ = w.Write(data)
			return
		}
		_, _ = w.Write([]byte(`{"corrupt":true}`))
	}))
	defer srv.Close()
	local := NewMemory()
	exchange := NewExchange(local, NewClient())
	result, err := exchange.Round(context.Background(), []string{srv.URL})
	if !errors.Is(err, ErrHashMismatch) || result.Attempted != MaxFetchesPerRound ||
		requests.Load() != MaxFetchesPerRound || result.Added != 0 {
		t.Fatalf("corrupt round: %+v err=%v", result, err)
	}
	result, err = exchange.Round(context.Background(), []string{srv.URL})
	if !errors.Is(err, ErrHashMismatch) || result.Attempted > MaxFetchesPerRound ||
		requests.Load() > MaxFetchesPerRound*2 || !local.Has(healthyHash) {
		t.Fatalf("healthy object starved behind corrupt hashes: %+v err=%v", result, err)
	}

}

func TestRoundDeadlineAndPeerRotationDoNotStarveAHealthyPeer(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer slow.Close()
	remote := NewMemory()
	data := objectBytes(t, `true`)
	if _, err := remote.Put(Hash(data), data); err != nil {
		t.Fatal(err)
	}
	healthy := objectServer(t, remote)
	local := NewMemory()
	exchange := NewExchange(local, NewClient())
	urls := []string{slow.URL, healthy.URL}
	ctx, cancel := context.WithTimeout(context.Background(), RoundTimeout+2*time.Second)
	defer cancel()
	started := time.Now()
	result, err := exchange.Round(ctx, urls)
	if !errors.Is(err, context.DeadlineExceeded) || result.Peers != 1 ||
		time.Since(started) > RoundTimeout+time.Second {
		t.Fatalf("active round exceeded its deadline: %+v err=%v elapsed=%s", result, err, time.Since(started))
	}
	// The healthy peer now leads, even though the slow peer prevented its first visit.
	ctx, cancelNext := context.WithTimeout(context.Background(), time.Second)
	defer cancelNext()
	result, err = exchange.Round(ctx, urls)
	if !local.Has(Hash(data)) || result.Added != 1 {
		t.Fatalf("healthy peer starved behind slow peer: %+v err=%v", result, err)
	}
}
