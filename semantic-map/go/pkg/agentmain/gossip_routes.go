package agentmain

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/DiyazY/di-agent/pkg/gossip"
	"github.com/DiyazY/di-agent/pkg/peers"
)

func decodeGossipRequest(w http.ResponseWriter, r *http.Request, dst any) error {
	if err := requireJSON(r); err != nil {
		return err
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, gossip.MaxObjectBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		if err != nil {
			return err
		}
		return fmt.Errorf("expected one JSON request")
	}
	return nil
}

func gossipError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	var tooLarge *http.MaxBytesError
	if errors.Is(err, gossip.ErrTooLarge) || errors.As(err, &tooLarge) {
		status = http.StatusRequestEntityTooLarge
	} else if errors.Is(err, gossip.ErrFull) {
		status = http.StatusInsufficientStorage
	}
	writeError(w, status, err.Error())
}

func registerGossipRoutes(mux *http.ServeMux, originID string, registry *peers.Registry,
	store *gossip.Memory, client *gossip.Client) {
	mux.HandleFunc("GET /gossip/objects", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, store.Inventory())
	})
	mux.HandleFunc("POST /gossip/objects", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			PayloadType string          `json:"payload_type"`
			Payload     json.RawMessage `json:"payload"`
		}
		if err := decodeGossipRequest(w, r, &in); err != nil {
			gossipError(w, err)
			return
		}
		data, err := gossip.Encode(originID, in.PayloadType, in.Payload)
		if err != nil {
			gossipError(w, err)
			return
		}
		hash := gossip.Hash(data)
		added, err := store.Put(hash, data)
		if err != nil {
			gossipError(w, err)
			return
		}
		writeJSON(w, map[string]any{"hash": hash, "added": added})
	})
	mux.HandleFunc("GET /gossip/objects/{hash}", func(w http.ResponseWriter, r *http.Request) {
		hash := r.PathValue("hash")
		if err := gossip.ValidateHash(hash); err != nil {
			gossipError(w, err)
			return
		}
		data, ok := store.Get(hash)
		if !ok {
			writeError(w, http.StatusNotFound, "no gossip object "+hash)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Do not use writeJSON: re-encoding or appending a newline changes the hash.
		if _, err := w.Write(data); err != nil {
			log.Printf("gossip: writing object %s: %v", hash, err)
		}
	})
	mux.HandleFunc("POST /gossip/objects/{hash}/fetch", func(w http.ResponseWriter, r *http.Request) {
		hash := r.PathValue("hash")
		if err := gossip.ValidateHash(hash); err != nil {
			gossipError(w, err)
			return
		}
		var in struct {
			PeerID string `json:"peer_id"`
		}
		if err := decodeGossipRequest(w, r, &in); err != nil {
			gossipError(w, err)
			return
		}
		if in.PeerID == "" {
			writeError(w, http.StatusBadRequest, "peer_id is required")
			return
		}
		peer, err := registry.Get(in.PeerID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if peer == nil {
			writeError(w, http.StatusNotFound, "no registered peer "+in.PeerID)
			return
		}
		if _, ok := store.Get(hash); ok {
			writeJSON(w, map[string]any{"hash": hash, "added": false})
			return
		}
		data, err := client.Fetch(r.Context(), peer.URL, hash)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		added, err := store.Put(hash, data)
		if err != nil {
			gossipError(w, err)
			return
		}
		writeJSON(w, map[string]any{"hash": hash, "added": added})
	})
}
