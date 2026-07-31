package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/DiyazY/di-agent/pkg/domain"
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
		// Returning the effective strength alongside the asserted prior is the honest
		// answer: on a well-observed relationship the assertion moves the prior a lot
		// and the effective strength barely at all, and an operator should see that
		// rather than assume their number is now in force.
		writeJSON(w, map[string]any{
			"relationship": rel,
			"effective":    rel.Effective(),
			"note": "the assertion set the prior; effective strength blends it with " +
				"observation by confidence, so at high confidence it moves little",
		})
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

	// GET /state/estimate?target=<property>&id=<decision id> — answer a question
	// FROM the map, and record the answer with the state that produced it.
	//
	// This is the surface that makes the map load-bearing rather than descriptive.
	// The answer is assembled by reading the target property and the relationships
	// into it through a DecisionBuilder, so the record of what was read is produced
	// by the reading itself and cannot drift from it. The response carries the
	// decision id, and GET /state/decisions/{id} returns the same inputs afterwards
	// — including after the system has moved on, which is the point.
	mux.HandleFunc("GET /state/estimate", func(w http.ResponseWriter, r *http.Request) {
		target := r.URL.Query().Get("target")
		if target == "" {
			writeError(w, http.StatusBadRequest, "target is required: name the property to estimate")
			return
		}
		id := r.URL.Query().Get("id")
		if id == "" {
			id = "est-" + strconv.FormatUint(sm.Revision(), 10) + "-" + target
		}

		b := sm.Decide(id, "estimate "+target)
		p, ok := b.Property(target)
		if !ok {
			// Still commit: a decision that could not be made is itself a fact worth
			// recording, and its caveats say why.
			d := b.Commit(map[string]any{"error": "unknown property"})
			writeErrorWithBody(w, http.StatusNotFound, map[string]any{
				"error": "no property " + target, "decision_id": d.ID, "caveats": d.Caveats,
			})
			return
		}

		// The influences: each incoming relationship's effective strength applied to
		// its source property's current level. Reading the sources through the builder
		// records them as inputs too, so the trace holds everything the answer used.
		type influence struct {
			Relationship string  `json:"relationship"`
			Source       string  `json:"source"`
			SourceValue  float64 `json:"source_value"`
			Strength     float64 `json:"effective_strength"`
			Sign         int     `json:"sign"`
			Contribution float64 `json:"contribution"`
			Provenance   string  `json:"provenance"`
		}
		var influences []influence
		var sensitivity float64
		for _, rel := range b.RelationshipsInto(target) {
			src, ok := b.Property(rel.From)
			if !ok {
				continue
			}
			contribution := rel.Effective() * float64(rel.Sign) * src.Value
			sensitivity += contribution
			influences = append(influences, influence{
				Relationship: rel.ID, Source: rel.From, SourceValue: src.Value,
				Strength: rel.Effective(), Sign: rel.Sign,
				Contribution: contribution, Provenance: string(rel.Provenance),
			})
		}

		b.Note("level of %s is %.4f at confidence %.3f from %d observations",
			target, p.Value, p.Confidence, p.NObservations)
		b.Note("%d influences sum to %+.4f", len(influences), sensitivity)
		if len(influences) == 0 {
			b.Caveat("nothing relates to %s, so the level is all the map can offer about it", target)
		}

		answer := map[string]any{
			"target":      target,
			"level":       p.Value,
			"confidence":  p.Confidence,
			"status":      string(p.Status),
			"sensitivity": sensitivity,
		}
		d := b.Commit(answer)
		writeJSON(w, map[string]any{
			"decision_id": d.ID,
			"revision":    d.Revision,
			"answer":      answer,
			"influences":  influences,
			"rationale":   d.Rationale,
			"caveats":     d.Caveats,
		})
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

// seedStateFromSpec fills the state model with the properties and relationships the
// domain specification declares, and returns how many properties were created.
//
// Two layers come out of one specification. Each routed metric becomes an OBSERVED
// property — the thing a collector reports. Each construct becomes a DERIVED
// property whose members are the metrics routed to it, so the framework's
// evaluation constructs are summaries over real observations rather than a parallel
// vocabulary. Each proposition becomes a relationship between two derived
// properties, carrying its calibrated strength as a seeded prior.
//
// Seeding is not required for the model to work: an agent started against a
// specification that declares nothing still admits properties as telemetry arrives.
// It exists so that a fresh agent shows the shape it is about to fill in, and so
// that prior knowledge about how constructs relate is present before the system has
// been observed.
func seedStateFromSpec(sm *statemap.Map, spec *domain.Spec) (int, error) {
	if sm == nil || spec == nil {
		return 0, nil
	}

	members := map[string][]string{}
	for _, route := range spec.MetricRouting {
		if err := sm.DeclareProperty(statemap.Property{
			ID:     route.MetricType,
			Kind:   statemap.Observed,
			Unit:   route.Unit,
			Range:  route.Range,
			Source: "domain spec routing → " + route.ConstructID,
		}); err != nil {
			return 0, fmt.Errorf("declaring %s: %w", route.MetricType, err)
		}
		members[route.ConstructID] = append(members[route.ConstructID], route.MetricType)
	}

	for _, c := range spec.Constructs {
		mem := members[c.ConstructID]
		if len(mem) == 0 {
			// A construct with no routed metric would be a summary of nothing. Skipping
			// it keeps the model to what the system can actually exhibit, and the count
			// returned to the caller makes the omission visible.
			continue
		}
		if err := sm.DeclareProperty(statemap.Property{
			ID:      c.ConstructID,
			Kind:    statemap.Derived,
			Members: mem,
			Unit:    "fraction",
			Range:   [2]float64{0, 1},
			Source:  "domain spec construct: " + c.Name,
		}); err != nil {
			return 0, fmt.Errorf("declaring construct %s: %w", c.ConstructID, err)
		}
	}

	for _, prop := range spec.Propositions {
		sign := 1
		if prop.Direction == "negative" {
			sign = -1
		}
		// The proposition ID is the label, which is what lets two mechanisms relate
		// the same pair in opposite directions without one erasing the other.
		if err := sm.DeclareRelationship(statemap.Relationship{
			From: prop.FromConstruct, To: prop.ToConstruct,
			Label: prop.PropositionID, Sign: sign,
			Prior: 0.5, Provenance: statemap.Seeded,
			Note: prop.Description,
		}); err != nil {
			// A proposition whose endpoints are not both present is skipped rather than
			// fatal: the specification may declare a construct this deployment cannot
			// observe, and refusing to start would make an unobservable claim block an
			// otherwise working agent.
			continue
		}
	}

	return sm.Census().PropertiesTotal, nil
}

// writeErrorWithBody emits a JSON error that carries extra fields, used where the
// failure itself is worth tracing.
func writeErrorWithBody(w http.ResponseWriter, status int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
