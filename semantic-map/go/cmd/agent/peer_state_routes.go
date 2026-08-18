package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/DiyazY/di-agent/pkg/peers"
	"github.com/DiyazY/di-agent/pkg/statemap"
)

// This file is where a node stops being alone.
//
// Every agent models its own machine, so any question wider than one machine has to be
// asked of other agents. Until now the only things that crossed a node boundary were a
// cost number, a health probe and an offload request — an agent could not ask a peer
// what properties it has, which of them have gone quiet, or what it believes about a
// relation. Those are the questions a node needs answered before it can reason about
// anything outside itself.
//
// What is deliberately absent: any merge. A peer's properties are held under that
// peer's name, with the time they were collected, and are never folded into the local
// map. A merged map would describe no definite system, and every "this system" answer
// the agent gives would quietly become an answer about some union of machines.

// peerStateFetcher collects peer maps into the store.
//
// It reads the peer registry rather than a startup list, so a peer added at runtime via
// POST /peers is polled without a restart.
type peerStateFetcher struct {
	registry *peers.Registry
	client   *peers.Client
	store    *statemap.PeerStore
}

// fetchAll asks every registered peer for its state, sequentially.
//
// Sequential on purpose: this runs on edge hardware where a fan-out to every peer at
// once competes with the workload the agent is supposed to be measuring. Peer counts
// here are single digits, and a slow peer delays the next poll rather than the node.
func (f *peerStateFetcher) fetchAll(ctx context.Context) (ok, failed int) {
	descriptors, err := f.registry.List()
	if err != nil {
		log.Printf("peer state: listing peers: %v", err)
		return 0, 0
	}
	for _, d := range descriptors {
		if ctx.Err() != nil {
			return ok, failed
		}
		st, err := f.client.State(ctx, d.URL)
		if err != nil {
			f.store.RecordFailure(d.URL, err)
			failed++
			continue
		}
		if err := f.store.Record(*st); err != nil {
			// A refusal is not a transport failure: the peer answered, and what it said
			// could not be attributed. Recording it as unreachable would hide a
			// configuration problem behind a network-shaped symptom.
			log.Printf("peer state: refusing state from %s: %v", d.URL, err)
			failed++
			continue
		}
		_ = f.registry.MarkSeen(d.ID, time.Now())
		ok++
	}
	return ok, failed
}

// startPeerStateLoop polls peers for their state until ctx is cancelled.
//
// A zero interval disables polling, which leaves the cluster view holding only what a
// manual refresh collected. That is a legitimate configuration for a node that should
// not generate periodic traffic, and the flag documentation says what it costs.
func startPeerStateLoop(ctx context.Context, f *peerStateFetcher, every time.Duration) {
	if f == nil || every <= 0 {
		return
	}
	go func() {
		// One immediate pass: an operator restarting an agent should not have to wait a
		// polling interval to see whether its peers are reachable.
		if ok, failed := f.fetchAll(ctx); ok+failed > 0 {
			log.Printf("peer state: %d peers answered, %d did not", ok, failed)
		}
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				f.fetchAll(ctx)
			}
		}
	}()
}

// registerPeerStateRoutes exposes what this agent knows beyond its own machine.
func registerPeerStateRoutes(mux *http.ServeMux, sm *statemap.Map, store *statemap.PeerStore,
	f *peerStateFetcher) {
	if sm == nil || store == nil {
		return
	}

	// GET /state/cluster — this node's state and every peer's, side by side.
	//
	// Not a merged cluster map. The self section is what this agent observed; each peer
	// section is what that agent reported and when. A caller comparing them is doing so
	// knowingly, which is the only way the comparison is sound.
	mux.HandleFunc("GET /state/cluster", func(w http.ResponseWriter, r *http.Request) {
		view := store.View(sm.State(statemap.Query{}))
		stale := store.StaleView()
		writeJSON(w, map[string]any{
			"cluster": view,
			// Named explicitly rather than left for the caller to derive from timestamps:
			// a snapshot is only as good as its age, and a reader who has to work that
			// out will sometimes not bother.
			"stale_peers": stale,
			"note": "peer state is reported as received and never merged into local state; " +
				"a property with the same id on two nodes describes two systems",
		})
	})

	// GET /state/peers — one line per peer: who, when, how much, and whether the last
	// attempt worked.
	mux.HandleFunc("GET /state/peers", func(w http.ResponseWriter, r *http.Request) {
		view := store.View(statemap.StateView{Owner: sm.Owner()})
		now := time.Now()
		type summary struct {
			PeerID    string               `json:"peer_id"`
			URL       string               `json:"url"`
			Revision  uint64               `json:"revision"`
			AgeSecond float64              `json:"age_seconds"`
			Counts    statemap.StateCounts `json:"counts"`
			Err       string               `json:"error,omitempty"`
		}
		out := make([]summary, 0, len(view.Peers))
		for _, st := range view.Peers {
			out = append(out, summary{
				PeerID: st.PeerID, URL: st.URL, Revision: st.Revision,
				AgeSecond: st.Age(now).Seconds(), Counts: st.Counts, Err: st.Err,
			})
		}
		writeJSON(w, map[string]any{
			"self":   sm.Owner(),
			"peers":  out,
			"silent": view.Silent,
			"stale":  store.StaleView(),
		})
	})

	// GET /state/peers/{id} — everything received from one peer.
	mux.HandleFunc("GET /state/peers/{id}", func(w http.ResponseWriter, r *http.Request) {
		st, ok := store.Peer(r.PathValue("id"))
		if !ok {
			writeError(w, http.StatusNotFound, "no state held for peer "+r.PathValue("id")+
				"; either it has not been polled or it has never identified itself")
			return
		}
		writeJSON(w, map[string]any{
			"peer":        st,
			"age_seconds": st.Age(time.Now()).Seconds(),
		})
	})

	// POST /state/peers/refresh — poll every peer now.
	//
	// Exposed because the poll is on a timer, and an operator investigating a peer
	// should not have to wait for it, nor be unable to tell a stale snapshot from a
	// peer that has genuinely stopped changing.
	mux.HandleFunc("POST /state/peers/refresh", func(w http.ResponseWriter, r *http.Request) {
		if f == nil {
			writeError(w, http.StatusServiceUnavailable,
				"no peer fetcher is configured, so peer state cannot be refreshed")
			return
		}
		ok, failed := f.fetchAll(r.Context())
		writeJSON(w, map[string]any{
			"answered": ok, "failed": failed,
			"peers": store.View(statemap.StateView{}).Peers,
		})
	})

	// GET /state/where?property=<id> — who has this property, and what do they say it
	// is.
	//
	// The answer is per node, local first, and is not averaged. A mean across machines
	// is a number describing none of them; the reason to ask several nodes is to be
	// able to pick one, and that needs the answers kept apart.
	mux.HandleFunc("GET /state/where", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("property")
		if id == "" {
			writeError(w, http.StatusBadRequest, "property is required: name the property to locate")
			return
		}
		now := time.Now()
		type holder struct {
			Node       string  `json:"node"`
			Local      bool    `json:"local"`
			Value      float64 `json:"value"`
			Confidence float64 `json:"confidence"`
			Status     string  `json:"status"`
			N          int     `json:"n_observations"`
			AgeSeconds float64 `json:"age_seconds,omitempty"`
		}
		var holders []holder
		if p, ok := sm.Property(id); ok {
			holders = append(holders, holder{
				Node: sm.Owner(), Local: true, Value: p.Value, Confidence: p.Confidence,
				Status: string(p.Status), N: p.NObservations,
			})
		}
		for peerID, p := range store.FindProperty(id) {
			h := holder{
				Node: peerID, Value: p.Value, Confidence: p.Confidence,
				Status: string(p.Status), N: p.NObservations,
			}
			if st, ok := store.Peer(peerID); ok {
				h.AgeSeconds = st.Age(now).Seconds()
			}
			holders = append(holders, h)
		}
		writeJSON(w, map[string]any{
			"property": id,
			"holders":  holders,
			"note": "values are per node and deliberately not aggregated; a mean across " +
				"machines describes none of them",
		})
	})
}
