package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newCannedServer spins up an httptest.Server whose mux is populated by the
// caller. Returns a ready-to-use *Client and a cleanup func.
func newCannedServer(t *testing.T, h http.Handler) (*Client, func()) {
	t.Helper()
	srv := httptest.NewServer(h)
	c := New(srv.URL)
	c.HTTP.Timeout = 2 * time.Second
	return c, srv.Close
}

// helper: write JSON value to w.
func jsonOK(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encode: %v", err)
	}
}

func TestClient_Graph(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /graph", func(w http.ResponseWriter, r *http.Request) {
		jsonOK(t, w, GraphSnapshot{
			Constructs:   []ConstructDTO{{ConstructID: "RC", Name: "Resource & Cost"}},
			Propositions: []PropositionDTO{{PropositionID: "P3", FromConstruct: "RC", ToConstruct: "PS", Direction: "+", PriorStrength: 0.7}},
			Edges:        []EdgeDTO{{FromID: "RC", ToID: "PS", PropositionID: "P3", Direction: "+", Established: f64(0.7), Effective: f64(0.7), Basis: "established"}},
		})
	})
	c, cleanup := newCannedServer(t, mux)
	defer cleanup()

	snap, err := c.Graph(context.Background())
	if err != nil {
		t.Fatalf("Graph: %v", err)
	}
	if len(snap.Constructs) != 1 || snap.Constructs[0].ConstructID != "RC" {
		t.Errorf("unexpected constructs: %+v", snap.Constructs)
	}
	if len(snap.Edges) != 1 || snap.Edges[0].PropositionID != "P3" {
		t.Errorf("unexpected edges: %+v", snap.Edges)
	}
}

func TestClient_Edges_FilterByPair(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /edges", func(w http.ResponseWriter, r *http.Request) {
		from := r.URL.Query().Get("from")
		to := r.URL.Query().Get("to")
		if from != "RC" || to != "PS" {
			t.Errorf("expected from=RC&to=PS, got from=%q to=%q", from, to)
		}
		jsonOK(t, w, []EdgeDTO{
			{PropositionID: "P2", FromID: "RC", ToID: "PS", Direction: "-"},
			{PropositionID: "P3", FromID: "RC", ToID: "PS", Direction: "+"},
		})
	})
	c, cleanup := newCannedServer(t, mux)
	defer cleanup()

	edges, err := c.Edges(context.Background(), "RC", "PS")
	if err != nil {
		t.Fatalf("Edges: %v", err)
	}
	if len(edges) != 2 {
		t.Fatalf("expected 2 edges (P2,P3), got %d", len(edges))
	}
}

func TestClient_History_PassesSince(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /history", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("since"); got != "1h" {
			t.Errorf("expected since=1h, got %q", got)
		}
		jsonOK(t, w, []OntologyEventDTO{
			{Timestamp: time.Unix(0, 0), Actor: "operator", Kind: "proposition_strength_set", TargetID: "P3"},
		})
	})
	c, cleanup := newCannedServer(t, mux)
	defer cleanup()

	events, err := c.History(context.Background(), "1h")
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(events) != 1 || events[0].TargetID != "P3" {
		t.Errorf("unexpected events: %+v", events)
	}
}

func TestClient_Health(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		jsonOK(t, w, HealthResponse{OK: true})
	})
	c, cleanup := newCannedServer(t, mux)
	defer cleanup()
	h, err := c.Health(context.Background())
	if err != nil || !h.OK {
		t.Fatalf("Health: err=%v h=%+v", err, h)
	}
}

func TestClient_Version(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		jsonOK(t, w, VersionResponse{
			AgentVersion: "0.1.0", GoVersion: "go1.22",
			SemmapConstructs: 7, SemmapPropositions: 15,
		})
	})
	c, cleanup := newCannedServer(t, mux)
	defer cleanup()
	v, err := c.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v.AgentVersion != "0.1.0" || v.SemmapPropositions != 15 {
		t.Errorf("unexpected version: %+v", v)
	}
}

func TestClient_Neighbors(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /neighbors", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("node") != "RC" {
			t.Errorf("expected node=RC, got %q", r.URL.Query().Get("node"))
		}
		jsonOK(t, w, []string{"PS", "RR"})
	})
	c, cleanup := newCannedServer(t, mux)
	defer cleanup()
	out, err := c.Neighbors(context.Background(), "RC")
	if err != nil || len(out) != 2 {
		t.Fatalf("Neighbors: err=%v out=%v", err, out)
	}
}

func TestClient_SetStrength_SendsJSON(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ontology/strength", func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("expected application/json, got %q", ct)
		}
		var req SetStrengthRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if req.PropositionID != "P3" || req.Strength != 0.77 {
			t.Errorf("unexpected body: %+v", req)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	c, cleanup := newCannedServer(t, mux)
	defer cleanup()
	if err := c.SetStrength(context.Background(), "P3", 0.77); err != nil {
		t.Fatalf("SetStrength: %v", err)
	}
}

func TestClient_Deprecate(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ontology/deprecate", func(w http.ResponseWriter, r *http.Request) {
		var req DeprecateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if req.PropositionID != "P1" || req.Reason != "stale" {
			t.Errorf("unexpected body: %+v", req)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	c, cleanup := newCannedServer(t, mux)
	defer cleanup()
	if err := c.Deprecate(context.Background(), "P1", "stale"); err != nil {
		t.Fatalf("Deprecate: %v", err)
	}
}

func TestClient_ResetEdge(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /agent/reset", func(w http.ResponseWriter, r *http.Request) {
		var req ResetRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if req.From != "RC" || req.To != "PS" {
			t.Errorf("unexpected body: %+v", req)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	c, cleanup := newCannedServer(t, mux)
	defer cleanup()
	if err := c.ResetEdge(context.Background(), "RC", "PS"); err != nil {
		t.Fatalf("ResetEdge: %v", err)
	}
}

func TestClient_AddConstruct(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ontology/construct", func(w http.ResponseWriter, r *http.Request) {
		var req AddConstructRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if req.ConstructID != "XX" {
			t.Errorf("unexpected body: %+v", req)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	c, cleanup := newCannedServer(t, mux)
	defer cleanup()
	if err := c.AddConstruct(context.Background(), "XX", "Test", "desc"); err != nil {
		t.Fatalf("AddConstruct: %v", err)
	}
}

func TestClient_AddProposition(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ontology/proposition", func(w http.ResponseWriter, r *http.Request) {
		var req AddPropositionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if req.PropositionID != "P99" || req.Direction != "+" {
			t.Errorf("unexpected body: %+v", req)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	c, cleanup := newCannedServer(t, mux)
	defer cleanup()
	if err := c.AddProposition(context.Background(), "P99", "A", "B", "+", 0.5); err != nil {
		t.Fatalf("AddProposition: %v", err)
	}
}

func TestClient_CandidateActions(t *testing.T) {
	calls := map[string]int{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /candidates/{id}/confirm", func(w http.ResponseWriter, r *http.Request) {
		calls["confirm:"+r.PathValue("id")]++
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /candidates/{id}/reject", func(w http.ResponseWriter, r *http.Request) {
		calls["reject:"+r.PathValue("id")]++
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /candidates/{id}/defer", func(w http.ResponseWriter, r *http.Request) {
		calls["defer:"+r.PathValue("id")]++
		w.WriteHeader(http.StatusNoContent)
	})
	c, cleanup := newCannedServer(t, mux)
	defer cleanup()
	if err := c.ConfirmCandidate(context.Background(), "cand-1"); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if err := c.RejectCandidate(context.Background(), "cand-2"); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if err := c.DeferCandidate(context.Background(), "cand-3"); err != nil {
		t.Fatalf("Defer: %v", err)
	}
	if calls["confirm:cand-1"] != 1 || calls["reject:cand-2"] != 1 || calls["defer:cand-3"] != 1 {
		t.Errorf("unexpected call counts: %+v", calls)
	}
}

func TestClient_ErrorResponse_DecodesErrorField(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ontology/strength", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(ErrorResponse{Error: "proposition not found"})
	})
	c, cleanup := newCannedServer(t, mux)
	defer cleanup()
	err := c.SetStrength(context.Background(), "P_BAD", 0.5)
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "proposition not found") {
		t.Errorf("expected error to include server message, got %q", err.Error())
	}
}

// f64 returns a pointer to a float literal. Established, Assertion and Effective are
// pointers because absence is a distinct state from zero, so a test fixture needs a
// one-liner to express "this one has a value".
func f64(v float64) *float64 { return &v }
