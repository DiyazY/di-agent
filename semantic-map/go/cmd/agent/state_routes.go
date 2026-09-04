package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/DiyazY/di-agent/pkg/statemap"
)

// registerStateRoutes exposes the state model: what the system exhibits, how its
// properties relate, and why the agent answered as it did.
//
// The surface is deliberately query-shaped rather than dump-shaped. An operator
// debugging a decision asks "what did it think was going on", and the answer needs
// to be one request naming a property, not a graph export they then have to read.
func registerStateRoutes(mux *http.ServeMux, sm *statemap.Map) {
	if sm == nil {
		return
	}

	// GET /state — the current system. No arguments answers the common question;
	// filters narrow it.
	//
	//   ?kind=observed|derived
	//   ?status=active|stale|retired   (repeatable)
	//   ?min-confidence=0.5
	//   ?related-to=<property>         one-hop neighbourhood
	//   ?id=<property>                 (repeatable)
	mux.HandleFunc("GET /state", func(w http.ResponseWriter, r *http.Request) {
		q := statemap.Query{
			IDs:       r.URL.Query()["id"],
			RelatedTo: r.URL.Query().Get("related-to"),
		}
		for _, k := range r.URL.Query()["kind"] {
			q.Kinds = append(q.Kinds, statemap.Kind(k))
		}
		for _, s := range r.URL.Query()["status"] {
			q.Statuses = append(q.Statuses, statemap.Status(s))
		}
		if v := r.URL.Query().Get("min-confidence"); v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				writeError(w, http.StatusBadRequest, "min-confidence must be a number")
				return
			}
			q.MinConfidence = f
		}
		writeJSON(w, sm.State(q))
	})

	// GET /state/properties/{id} — one property, or its explanation as text when
	// Accept asks for it. The text form exists so a human can read the answer in a
	// terminal without a client to render JSON.
	mux.HandleFunc("GET /state/properties/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if strings.Contains(r.Header.Get("Accept"), "text/plain") {
			out, err := sm.Explain(id)
			if err != nil {
				writeError(w, http.StatusNotFound, err.Error())
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte(out))
			return
		}
		p, ok := sm.Property(id)
		if !ok {
			writeError(w, http.StatusNotFound, "no property "+id)
			return
		}
		writeJSON(w, map[string]any{
			"property":      p,
			"relationships": sm.Relationships("", id),
			"influences":    sm.Relationships(id, ""),
			"revision":      sm.Revision(),
		})
	})

	// POST /state/properties — declare a property. The explicit path for a
	// collector that knows what it will report; observation alone also admits.
	mux.HandleFunc("POST /state/properties", func(w http.ResponseWriter, r *http.Request) {
		if err := requireJSON(r); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		var p statemap.Property
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := sm.DeclareProperty(p); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// DELETE /state/properties/{id}?reason=... — retire a property.
	//
	// A reason is required. Retirement is a claim that something stopped being part
	// of the system, and a claim with no stated basis is not auditable.
	mux.HandleFunc("DELETE /state/properties/{id}", func(w http.ResponseWriter, r *http.Request) {
		reason := r.URL.Query().Get("reason")
		if reason == "" {
			writeError(w, http.StatusBadRequest,
				"reason is required: a retirement with no stated basis cannot be audited")
			return
		}
		actor := r.URL.Query().Get("actor")
		if actor == "" {
			actor = "operator"
		}
		if err := sm.RetireProperty(r.PathValue("id"), reason, actor); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// GET /state/relationships?from=&to=
	mux.HandleFunc("GET /state/relationships", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"relationships": sm.Relationships(r.URL.Query().Get("from"), r.URL.Query().Get("to")),
			"revision":      sm.Revision(),
		})
	})

	// POST /state/relationships — declare a relationship between two properties.
	mux.HandleFunc("POST /state/relationships", func(w http.ResponseWriter, r *http.Request) {
		if err := requireJSON(r); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		var rel statemap.Relationship
		if err := json.NewDecoder(r.Body).Decode(&rel); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := sm.DeclareRelationship(rel); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// DELETE /state/relationships/{id}?reason=...
	mux.HandleFunc("DELETE /state/relationships/{id}", func(w http.ResponseWriter, r *http.Request) {
		reason := r.URL.Query().Get("reason")
		if reason == "" {
			writeError(w, http.StatusBadRequest, "reason is required")
			return
		}
		actor := r.URL.Query().Get("actor")
		if actor == "" {
			actor = "operator"
		}
		if err := sm.RetireRelationship(r.PathValue("id"), reason, actor); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// POST /state/relationships/{id}/strength — the operator assertion path.
	mux.HandleFunc("POST /state/relationships/{id}/strength", func(w http.ResponseWriter, r *http.Request) {
		if err := requireJSON(r); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		var body struct {
			Strength float64 `json:"strength"`
			Actor    string  `json:"actor"`
			Reason   string  `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if body.Reason == "" {
			writeError(w, http.StatusBadRequest,
				"reason is required: an assertion overrides what the system showed, so "+
					"the record needs to say on what basis")
			return
		}
		if body.Actor == "" {
			body.Actor = "operator"
		}
		if err := sm.AssertRelationshipStrength(r.PathValue("id"), body.Strength,
			body.Actor, body.Reason); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		rel, _ := sm.Relationship(r.PathValue("id"))
		eff, known := rel.Effective()
		resp := map[string]any{
			"relationship": rel,
			"basis":        rel.Basis(),
			"note": "an assertion outranks both learned layers and takes effect in " +
				"full; the learned estimates are kept beside it so what was measured " +
				"stays readable",
		}
		if known {
			resp["effective"] = eff
		} else {
			resp["effective"] = nil
		}
		writeJSON(w, resp)
	})

	// GET /state/journal?since=<revision>&limit=N — the change record.
	mux.HandleFunc("GET /state/journal", func(w http.ResponseWriter, r *http.Request) {
		since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		held, dropped, oldest := sm.Journal().Stats()
		writeJSON(w, map[string]any{
			"events":   sm.Journal().Events(since, limit),
			"revision": sm.Revision(),
			// Reporting the window explicitly stops a caller from reading an absence
			// as evidence that nothing happened.
			"held":            held,
			"dropped":         dropped,
			"oldest_revision": oldest,
		})
	})

	// GET /state/decisions?limit=N and GET /state/decisions/{id} — the trace.
	mux.HandleFunc("GET /state/decisions", func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 {
			limit = 20
		}
		writeJSON(w, map[string]any{
			"decisions": sm.Journal().Decisions(limit),
			"revision":  sm.Revision(),
		})
	})

	mux.HandleFunc("GET /state/decisions/{id}", func(w http.ResponseWriter, r *http.Request) {
		d, ok := sm.Journal().Decision(r.PathValue("id"))
		if !ok {
			// 410 rather than 404: the decision may well have happened and been
			// evicted by the journal bound, and a caller has to be able to tell that
			// from a decision that never existed.
			held, dropped, oldest := sm.Journal().Stats()
			if dropped > 0 {
				writeError(w, http.StatusGone, "decision "+r.PathValue("id")+
					" is not held; the journal holds "+strconv.Itoa(held)+
					" entries from revision "+strconv.FormatUint(oldest, 10)+
					" and has dropped "+strconv.FormatUint(dropped, 10))
				return
			}
			writeError(w, http.StatusNotFound, "no decision "+r.PathValue("id"))
			return
		}
		writeJSON(w, d)
	})

	// GET /state/estimate?target=<property>[&id=…][&assume=<property>=<value>]*[&without=<subject|property>]*
	//
	// Answers FROM the map and records the answer with the state that produced it —
	// and, when the question is a counterfactual, with what was supposed. See
	// statemap.Map.Estimate for the arithmetic and the standing caveats.
	mux.HandleFunc("GET /state/estimate", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		target := q.Get("target")
		if target == "" {
			writeError(w, http.StatusBadRequest, "target is required: name the property to estimate")
			return
		}
		req := statemap.EstimateRequest{ID: q.Get("id"), Target: target, Without: q["without"]}
		for _, a := range q["assume"] {
			key, val, ok := strings.Cut(a, "=")
			if !ok || key == "" {
				writeError(w, http.StatusBadRequest, "assume must be <property>=<value>: "+a)
				return
			}
			f, err := strconv.ParseFloat(val, 64)
			if err != nil {
				writeError(w, http.StatusBadRequest, "assume value must be a number: "+a)
				return
			}
			if req.Assume == nil {
				req.Assume = map[string]float64{}
			}
			req.Assume[key] = f
		}
		res := sm.Estimate(req)
		if res.Err != "" {
			writeErrorWithBody(w, http.StatusNotFound, map[string]any{
				"error": res.Err, "decision_id": res.DecisionID, "caveats": res.Caveats,
			})
			return
		}
		writeJSON(w, res)
	})

	// POST /state/sweep — apply lifecycle transitions now.
	//
	// Exposed because staleness is time-based and an operator inspecting a quiet
	// system should not have to wait for a timer to see the map admit that its
	// properties have gone quiet.
	mux.HandleFunc("POST /state/sweep", func(w http.ResponseWriter, r *http.Request) {
		stale, retired := sm.Sweep()
		writeJSON(w, map[string]any{
			"stale": stale, "retired": retired,
			"revision": sm.Revision(), "at": time.Now().UTC(),
		})
	})
}

// writeErrorWithBody emits a JSON error carrying extra fields, used where the
// failure itself is worth tracing.
func writeErrorWithBody(w http.ResponseWriter, status int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
