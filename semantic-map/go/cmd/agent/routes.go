package main

// HTTP surface served by the agent daemon.
//
// Endpoint                          Method  Body / Path                    Status on success
// ─────────────────────────────────────────────────────────────────────────────────────────
// /ingest                           POST    {from_id,to_id,observation,…}  204
//                                                                          400 (unknown construct pair)
// /cost                             GET     ?task=&node=                   200 ActionCost
// /recommend                        POST    OffloadContext                 200 PeerRecommendation
//                                                                          409 (no peer qualifies / none registered)
// /simulate                         POST    {context,target_node_id}       200 OutcomeSimulation
// /candidates                       GET     —                              200 []CandidateEdge
// ─────────────────────────────────────────────────────────────────────────────────────────
// /ingest-sample                    POST    MetricSampleRequest            204
// /graph                            GET     —                              200 GraphSnapshot
// /edges                            GET     ?from=&to=                     200 []EdgeDTO
// /constructs                       GET     —                              200 []ConstructDTO
// /propositions                     GET     —                              200 []PropositionDTO
// /history                          GET     ?since=                        200 []OntologyEventDTO
// /neighbors                        GET     ?node=                         200 []string
// /healthz                          GET     —                              200 HealthResponse
// /version                          GET     —                              200 VersionResponse
// /ontology/strength                POST    SetStrengthRequest             204
// /ontology/deprecate               POST    DeprecateRequest               204
// /ontology/construct               POST    AddConstructRequest            204
// /ontology/proposition             POST    AddPropositionRequest          204
// /agent/reset                      POST    ResetRequest                   204
// /agent/tune                       POST    TuneRequest                    200 TuneResponse
// /candidates/{id}/confirm          POST    —                              204
// /candidates/{id}/reject           POST    —                              204
// /candidates/{id}/defer            POST    —                              204
// /peers                            GET     —                              200 []PeerDTO
// /peers                            POST    AddPeerRequest                 200 PeerDTO
// /peers/{id}                       DELETE  —                              204
// /peers/{id}/trust                 POST    SetTrustRequest                204
// /offload                          POST    OffloadHTTPRequest             200 OffloadHTTPResponse
// /explain                          POST    ExplainHTTPRequest             200 ExplainResponse
//                                                                          422 {error,response} (failed a gate)
//                                                                          501 (provider disabled)
//                                                                          200 x-ndjson (stream:true)
// /ui/...                           GET     —                              200 (embedded HTML)
//
// Endpoints above the divider are pre-existing and keep their original
// plain-text http.Error format. Endpoints below were added in the Phase 1
// HTTP-API expansion and emit JSON errors via writeError; their POST
// handlers gate on requireJSON for CSRF mitigation (path-only candidate
// endpoints excepted).

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/DiyazY/di-agent/pkg/contracts"
	"github.com/DiyazY/di-agent/pkg/explain"
	"github.com/DiyazY/di-agent/pkg/peers"
	"github.com/DiyazY/di-agent/pkg/semmap"
	"github.com/DiyazY/di-agent/pkg/types"
)

// registerRoutes wires every HTTP handler onto mux. It is the single entry
// point for the daemon's URL surface; main only constructs the mux, the
// SemanticMap, and the http.Server.
//
// Convention: the EXISTING endpoints (/ingest, /cost, /recommend, /simulate,
// /candidates) keep their original http.Error plain-text error format to
// minimize diff. Every NEW endpoint added in this expansion uses
// writeError to emit a JSON {"error":"..."} body, and every new POST
// handler calls requireJSON at the top as a lightweight CSRF mitigation.
func registerRoutes(mux *http.ServeMux, sm *semmap.SemanticMap, explainer explain.Explainer) {
	registerExistingRoutes(mux, sm)
	registerReadRoutes(mux, sm)
	registerMutationRoutes(mux, sm)
	registerPeerRoutes(mux, sm)
	registerExplainRoute(mux, explainer)
	registerStaticRoutes(mux)
}

// registerStaticRoutes serves the embedded UI under /ui/.
//
// http.FileServer serves index.html for the directory root automatically
// when the URL ends in "/". An explicit "/ui/{$}" → "/ui/index.html"
// redirect would loop because http.FileServer canonicalizes URLs ending
// in /index.html back to "./" — so we rely on the default behavior.
func registerStaticRoutes(mux *http.ServeMux) {
	mux.Handle("GET /ui/", staticHandler())
}

// registerExistingRoutes preserves the original five endpoints unchanged.
// They are kept in their own function so the diff against pre-expansion
// behavior is obvious to reviewers.
func registerExistingRoutes(mux *http.ServeMux, sm *semmap.SemanticMap) {
	// POST /ingest  {"from_id":"PS","to_id":"RC","observation":0.7,"event_id":"evt-1"}
	mux.HandleFunc("POST /ingest", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			FromID      string  `json:"from_id"`
			ToID        string  `json:"to_id"`
			Observation float64 `json:"observation"`
			EventID     string  `json:"event_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := sm.Ingest(req.FromID, req.ToID, req.Observation, req.EventID); err != nil {
			// An unknown (from_id, to_id) pair is the caller naming a construct
			// pair that carries no edge — a client error, not a server fault.
			// Returning 500 here made a malformed request indistinguishable
			// from a genuine internal failure.
			writeError(w, statusForIngestError(err), err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// GET /cost?task=pod-scheduling&node=node_1
	mux.HandleFunc("GET /cost", func(w http.ResponseWriter, r *http.Request) {
		task := r.URL.Query().Get("task")
		node := r.URL.Query().Get("node")
		result, err := sm.CostOfAction(task, node)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, result)
	})

	// POST /recommend  {"task_type":"...","source_node_id":"...","data_size_bytes":1024,"latency_budget_ms":500}
	mux.HandleFunc("POST /recommend", func(w http.ResponseWriter, r *http.Request) {
		var ctx types.OffloadContext
		if err := json.NewDecoder(r.Body).Decode(&ctx); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		result, err := sm.RecommendPeer(&ctx)
		if err != nil {
			// ErrInsufficientTrust covers "no peer registry", "no peers
			// registered" and "no peer qualifies" — all ordinary states of a
			// single-node or freshly-started deployment, not server faults.
			// 409 says the request was well-formed but current state cannot
			// satisfy it, which is what an operator needs to distinguish.
			if errors.Is(err, contracts.ErrInsufficientTrust) {
				writeError(w, http.StatusConflict, err.Error())
				return
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, result)
	})

	// POST /simulate  {"context":{...},"target_node_id":"node_2"}
	mux.HandleFunc("POST /simulate", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Context      types.OffloadContext `json:"context"`
			TargetNodeID string               `json:"target_node_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		result, err := sm.SimulateOutcome(&req.Context, req.TargetNodeID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, result)
	})

	// GET /candidates  — pending graph extension proposals
	mux.HandleFunc("GET /candidates", func(w http.ResponseWriter, r *http.Request) {
		candidates, err := sm.PendingCandidates()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, candidates)
	})
}

// ── helpers ───────────────────────────────────────────────────────────────────

// writeJSON serializes v as JSON with a 200 OK header. It is shared by both
// old and new endpoints because the success-path content type is the same.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON error: %v", err)
	}
}

// writeError emits a JSON-encoded error response with the given status code.
// Used by every new endpoint added in the HTTP expansion; existing endpoints
// keep plain-text http.Error for diff minimization.
func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(ErrorResponse{Error: msg}); err != nil {
		log.Printf("writeError encoding failure: %v", err)
	}
}

// registerReadRoutes wires the introspection endpoints: /graph, /edges,
// /constructs, /propositions, /history, /neighbors, /healthz, /version.
// These read-only endpoints never mutate state and emit JSON on both
// success and error paths.
func registerReadRoutes(mux *http.ServeMux, sm *semmap.SemanticMap) {
	mux.HandleFunc("GET /graph", func(w http.ResponseWriter, r *http.Request) {
		constructs, err := sm.Constructs()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		propositions, err := sm.Propositions()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		edges, err := sm.AllEdges()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		snap := GraphSnapshot{
			Constructs:   make([]ConstructDTO, 0, len(constructs)),
			Propositions: make([]PropositionDTO, 0, len(propositions)),
			Edges:        make([]EdgeDTO, 0, len(edges)),
		}
		for _, c := range constructs {
			snap.Constructs = append(snap.Constructs, constructToDTO(c))
		}
		for _, p := range propositions {
			snap.Propositions = append(snap.Propositions, propositionToDTO(p))
		}
		for _, e := range edges {
			snap.Edges = append(snap.Edges, edgeToDTO(e))
		}
		writeJSON(w, snap)
	})

	mux.HandleFunc("GET /edges", func(w http.ResponseWriter, r *http.Request) {
		from := r.URL.Query().Get("from")
		to := r.URL.Query().Get("to")
		var (
			edges []*types.EdgeDescriptor
			err   error
		)
		switch {
		case from != "" && to != "":
			edges, err = sm.EdgesByPair(from, to)
		default:
			// Empty filter on either side -> return all and filter in-process
			// for the half-specified case. AllEdges is the common path.
			edges, err = sm.AllEdges()
			if err == nil && (from != "" || to != "") {
				filtered := edges[:0]
				for _, e := range edges {
					if from != "" && e.FromID != from {
						continue
					}
					if to != "" && e.ToID != to {
						continue
					}
					filtered = append(filtered, e)
				}
				edges = filtered
			}
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out := make([]EdgeDTO, 0, len(edges))
		for _, e := range edges {
			out = append(out, edgeToDTO(e))
		}
		writeJSON(w, out)
	})

	mux.HandleFunc("GET /constructs", func(w http.ResponseWriter, r *http.Request) {
		constructs, err := sm.Constructs()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out := make([]ConstructDTO, 0, len(constructs))
		for _, c := range constructs {
			out = append(out, constructToDTO(c))
		}
		writeJSON(w, out)
	})

	mux.HandleFunc("GET /propositions", func(w http.ResponseWriter, r *http.Request) {
		propositions, err := sm.Propositions()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out := make([]PropositionDTO, 0, len(propositions))
		for _, p := range propositions {
			out = append(out, propositionToDTO(p))
		}
		writeJSON(w, out)
	})

	mux.HandleFunc("GET /history", func(w http.ResponseWriter, r *http.Request) {
		since, err := parseSince(r.URL.Query().Get("since"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		events, err := sm.History(since)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out := make([]OntologyEventDTO, 0, len(events))
		for _, e := range events {
			out = append(out, eventToDTO(e))
		}
		writeJSON(w, out)
	})

	mux.HandleFunc("GET /neighbors", func(w http.ResponseWriter, r *http.Request) {
		node := r.URL.Query().Get("node")
		if node == "" {
			writeError(w, http.StatusBadRequest, "missing required query parameter: node")
			return
		}
		neighbors, err := sm.Neighbors(node)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if neighbors == nil {
			neighbors = []string{}
		}
		writeJSON(w, neighbors)
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, HealthResponse{OK: true})
	})

	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		// Count constructs/propositions for operator visibility. Errors here
		// shouldn't fail /version — fall back to zero counts.
		var nC, nP int
		if cs, err := sm.Constructs(); err == nil {
			nC = len(cs)
		}
		if ps, err := sm.Propositions(); err == nil {
			nP = len(ps)
		}
		writeJSON(w, VersionResponse{
			AgentVersion:       Version,
			GoVersion:          runtime.Version(),
			BuildCommit:        BuildCommit,
			SemmapConstructs:   nC,
			SemmapPropositions: nP,
		})
	})
}

// registerMutationRoutes wires the ontology and edge mutation endpoints.
//
// Every body-bearing handler:
//  1. Calls requireJSON (CSRF mitigation: rejects non-application/json bodies)
//  2. Decodes the typed DTO from the body
//  3. Calls the facade and returns 204 No Content on success
//  4. Emits writeError on any failure
//
// The path-only candidate endpoints intentionally skip requireJSON because
// they take no body — the CSRF concern there is satisfied by the path
// parameter being non-guessable in practice (UUID-shaped candidate IDs).
func registerMutationRoutes(mux *http.ServeMux, sm *semmap.SemanticMap) {
	mux.HandleFunc("POST /ontology/strength", func(w http.ResponseWriter, r *http.Request) {
		if err := requireJSON(r); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		var req SetStrengthRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := sm.SetPropositionStrength(req.PropositionID, req.Strength); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /ontology/deprecate", func(w http.ResponseWriter, r *http.Request) {
		if err := requireJSON(r); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		var req DeprecateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := sm.Deprecate(req.PropositionID, req.Reason); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /ontology/construct", func(w http.ResponseWriter, r *http.Request) {
		if err := requireJSON(r); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		var req AddConstructRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		c := &types.Construct{
			ConstructID: req.ConstructID,
			Name:        req.Name,
			Description: req.Description,
		}
		if err := sm.AddConstruct(c); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /ontology/proposition", func(w http.ResponseWriter, r *http.Request) {
		if err := requireJSON(r); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		var req AddPropositionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		dir, ok := directionFromString(req.Direction)
		if !ok {
			writeError(w, http.StatusBadRequest, "direction must be \"+\" or \"-\"")
			return
		}
		p := &types.Proposition{
			PropositionID: req.PropositionID,
			FromConstruct: req.From,
			ToConstruct:   req.To,
			Direction:     dir,
			PriorStrength: req.PriorStrength,
		}
		if err := sm.AddValidatedProposition(p); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /agent/reset", func(w http.ResponseWriter, r *http.Request) {
		if err := requireJSON(r); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		var req ResetRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := sm.ResetEdge(req.From, req.To); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// POST /agent/tune — natural-language operator intent → proposition adjustments
	mux.HandleFunc("POST /agent/tune", func(w http.ResponseWriter, r *http.Request) {
		if err := requireJSON(r); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		var req TuneRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if req.Intent == "" {
			writeError(w, http.StatusBadRequest, "intent must not be empty")
			return
		}
		applied, err := sm.Tune(req.Intent, req.Operator)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		dtos := make([]TuneAdjustmentDTO, len(applied))
		for i, a := range applied {
			dtos[i] = tuneAdjToDTO(a)
		}
		writeJSON(w, TuneResponse{Applied: dtos, Intent: req.Intent})
	})

	// POST /ingest-sample — Bridge-routed telemetry from external collectors.
	//
	// Where POST /ingest takes a fully pre-routed (from, to, value, event_id)
	// tuple and bypasses the Bridge, /ingest-sample carries a typed MetricSample
	// and runs the Bridge server-side. This is the public-API entry point for
	// out-of-tree collectors (e.g. the parquet replay tool) that cannot
	// import internal Go packages. Bridge silently ignores unmapped metric
	// types; this handler additionally rejects values outside the closed
	// catalogue with 400 so misconfigured callers fail loudly.
	mux.HandleFunc("POST /ingest-sample", func(w http.ResponseWriter, r *http.Request) {
		if err := requireJSON(r); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		var req MetricSampleRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		sample, err := sampleRequestToTypes(&req, sm)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := sm.IngestSample(sample); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// Path-only candidate review actions. No body, no requireJSON.
	mux.HandleFunc("POST /candidates/{id}/confirm", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := sm.ConfirmCandidate(id); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /candidates/{id}/reject", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := sm.RejectCandidate(id); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /candidates/{id}/defer", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := sm.DeferCandidate(id); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// parseSince accepts an empty string (returns zero time), an RFC3339
// timestamp, or a Go duration (subtracted from time.Now). It is the shared
// parser for the ?since= query parameter on /history.
func parseSince(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d), nil
	}
	return time.Time{}, errors.New("since must be RFC3339 timestamp or Go duration (e.g. 1h, 30m)")
}

// requireJSON returns an error when the request's Content-Type is not
// application/json. Called at the top of every new POST handler as a CSRF
// mitigation: browsers will not send Content-Type: application/json on
// simple cross-origin form submissions, so requiring it blocks naive CSRF.
// Path-only mutation endpoints (e.g. /candidates/{id}/confirm) skip this.
func requireJSON(r *http.Request) error {
	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		return errors.New("Content-Type must be application/json")
	}
	return nil
}

// registerPeerRoutes wires the multi-agent coordination endpoints:
//
//	GET    /peers           list every registered peer
//	POST   /peers           register a new peer (idempotent on URL)
//	DELETE /peers/{id}      unregister a peer
//	POST   /peers/{id}/trust override a peer's trust (test + operator path)
//	POST   /offload         the peer-side receiver of an offload proposal
//
// /offload is the *server* side of the protocol: an inbound request from
// another agent that wants to know if this agent can run their task within
// the requested latency/energy budgets. The handler computes its own
// CostOfAction, compares against the budgets, and returns accept/reject
// with the expected latency/energy so the source agent can update trust.
// It does NOT execute any task — execution is a future (P7) concern.
func registerPeerRoutes(mux *http.ServeMux, sm *semmap.SemanticMap) {
	mux.HandleFunc("GET /peers", func(w http.ResponseWriter, r *http.Request) {
		list, err := sm.Peers().List()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out := make([]PeerDTO, 0, len(list))
		for _, p := range list {
			out = append(out, peerToDTO(p))
		}
		writeJSON(w, out)
	})

	mux.HandleFunc("POST /peers", func(w http.ResponseWriter, r *http.Request) {
		if err := requireJSON(r); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		var req AddPeerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if strings.TrimSpace(req.URL) == "" {
			writeError(w, http.StatusBadRequest, "url is required")
			return
		}
		d, err := sm.Peers().Add(req.URL, req.Note)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(peerToDTO(d))
	})

	mux.HandleFunc("DELETE /peers/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := sm.Peers().Remove(id); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /peers/{id}/trust", func(w http.ResponseWriter, r *http.Request) {
		if err := requireJSON(r); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		id := r.PathValue("id")
		var req SetTrustRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := sm.Peers().SetTrust(id, req.Value); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /offload", func(w http.ResponseWriter, r *http.Request) {
		if err := requireJSON(r); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		var req OffloadHTTPRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp := decideOffload(sm, &req)
		writeJSON(w, resp)
	})
}

// peerToDTO converts an internal peers.Descriptor to its wire form.
func peerToDTO(d *peers.Descriptor) PeerDTO {
	return PeerDTO{
		ID:        d.ID,
		URL:       d.URL,
		Trust:     d.Trust,
		NObserved: d.NObserved,
		LastSeen:  d.LastSeen,
		Note:      d.Note,
	}
}

// decideOffload is the peer-side accept/reject logic. It calls CostOfAction
// once to estimate the cost of this task on the local agent, then compares
// against the requested budgets:
//
//   - If LatencyBudgetMs > 0 and the local LatencyEstimate exceeds it → reject.
//   - If EnergyBudgetJoules is set and the local ResourceCost exceeds it → reject.
//   - Otherwise → accept.
//
// The decision is informational only — we do not schedule anything. Execution
// is a P7 concern; this handler exists so the source agent can record an
// outcome and adjust trust accordingly.
func decideOffload(sm *semmap.SemanticMap, req *OffloadHTTPRequest) OffloadHTTPResponse {
	cost, err := sm.CostOfAction(req.TaskType, req.SourceNodeID)
	if err != nil {
		return OffloadHTTPResponse{
			Accepted: false,
			Reason:   "cost evaluation failed: " + err.Error(),
		}
	}
	if req.LatencyBudgetMs > 0 && cost.LatencyEstimate > req.LatencyBudgetMs {
		return OffloadHTTPResponse{
			Accepted:             false,
			Reason:               fmt.Sprintf("latency %.2fms exceeds budget %.2fms", cost.LatencyEstimate, req.LatencyBudgetMs),
			ExpectedLatency:      cost.LatencyEstimate,
			ExpectedResourceCost: cost.ResourceCost,
		}
	}
	if req.EnergyBudgetJoules != nil && cost.ResourceCost > *req.EnergyBudgetJoules {
		return OffloadHTTPResponse{
			Accepted:             false,
			Reason:               fmt.Sprintf("resource cost %.3f exceeds energy budget %.3fJ", cost.ResourceCost, *req.EnergyBudgetJoules),
			ExpectedLatency:      cost.LatencyEstimate,
			ExpectedResourceCost: cost.ResourceCost,
		}
	}
	return OffloadHTTPResponse{
		Accepted:             true,
		Reason:               "within budget",
		ExpectedLatency:      cost.LatencyEstimate,
		ExpectedResourceCost: cost.ResourceCost,
	}
}

// registerExplainRoute wires POST /explain. The endpoint is always registered
// so operators get a helpful 501 with remediation instructions when the
// provider is disabled — better than a bare 404 they'd have to hunt down.
func registerExplainRoute(mux *http.ServeMux, explainer explain.Explainer) {
	mux.HandleFunc("POST /explain", func(w http.ResponseWriter, r *http.Request) {
		if err := requireJSON(r); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		var req ExplainHTTPRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if strings.TrimSpace(req.Question) == "" {
			writeError(w, http.StatusBadRequest, "question is required")
			return
		}
		explainReq := explain.ExplainRequest{
			Question:   req.Question,
			SessionID:  req.SessionID,
			UsePlanner: req.UsePlanner,
			UseCritic:  req.UseCritic,
			Stream:     req.Stream,
			Budget: explain.ExplainBudget{
				MaxIterations: req.MaxIterations,
				MaxToolCalls:  req.MaxToolCalls,
			},
		}

		// Streaming path: NDJSON over chunked encoding. Requires both the
		// caller to ask for it and the Explainer to support it.
		if req.Stream {
			streamer, ok := explainer.(explain.StreamingExplainer)
			if !ok {
				writeError(w, http.StatusNotImplemented, "this explainer does not support streaming")
				return
			}
			serveExplainStream(w, r, streamer, explainReq)
			return
		}

		resp, err := explainer.Explain(r.Context(), explainReq)
		if err != nil {
			switch {
			case errors.Is(err, explain.ErrNotEnabled):
				writeError(w, http.StatusNotImplemented, err.Error())
			case errors.Is(err, explain.ErrSessionNotFound):
				// The caller sent a session_id we do not have — their fault,
				// not ours. A 500 here would send an operator hunting for a
				// server failure over an expired or mistyped session.
				writeError(w, http.StatusBadRequest, err.Error())
			case resp != nil:
				// Partial response: the answer parsed but failed a gate after
				// MaxIterations. Surface both so the operator sees the attempt
				// and the reason it was rejected.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnprocessableEntity)
				if encErr := json.NewEncoder(w).Encode(map[string]any{
					"error":    err.Error(),
					"response": resp,
				}); encErr != nil {
					log.Printf("explain: encoding 422 body failed: %v", encErr)
				}
			default:
				writeError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Header is already committed by the first Write, so an encode failure
		// here cannot change the status — but it must not vanish either, or a
		// truncated 200 looks like a success to everyone involved.
		if encErr := json.NewEncoder(w).Encode(resp); encErr != nil {
			log.Printf("explain: encoding response failed after 200 was sent: %v", encErr)
		}
	})
}

// serveExplainStream runs a streaming /explain call, writing one compact JSON
// object per line (NDJSON) as progress arrives.
//
// Why NDJSON rather than SSE: the consumer here is a CLI or a script, not a
// browser EventSource. NDJSON is trivially parseable with a line reader in
// every language, needs no `data:` prefix stripping, and composes with `jq`
// on the command line. SSE would buy browser auto-reconnect, which nothing in
// this deployment wants.
//
// The stream always terminates in exactly one `final` or `error` event, so a
// client can loop until it sees one of those two kinds.
func serveExplainStream(
	w http.ResponseWriter,
	r *http.Request,
	streamer explain.StreamingExplainer,
	req explain.ExplainRequest,
) {
	flusher, canFlush := w.(http.Flusher)

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	enc := json.NewEncoder(w)
	emit := func(ev explain.Event) {
		if err := enc.Encode(ev); err != nil {
			// The client hung up. Nothing useful to do — the explain loop
			// will notice via ctx cancellation on its next check.
			return
		}
		if canFlush {
			flusher.Flush()
		}
	}

	// ExplainStream emits the terminal final/error event itself, so we must
	// not write anything to the body after it returns. The error is still
	// worth logging: the client sees it in the stream, but without this the
	// server side has no record of streaming failures to correlate against.
	if _, err := streamer.ExplainStream(r.Context(), req, emit); err != nil {
		log.Printf("explain: stream ended with error: %v", err)
	}
}

// statusForIngestError maps an Ingest failure to an HTTP status. A missing edge
// means the caller named a construct pair the backbone does not carry, which is
// a client error; anything else is treated as internal.
func statusForIngestError(err error) int {
	if err == nil {
		return http.StatusNoContent
	}
	if strings.Contains(err.Error(), "not found in storage") {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}
