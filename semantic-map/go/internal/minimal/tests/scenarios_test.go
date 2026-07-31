// Scenario-style integration tests for the Semantic Map.
//
// These exercise the full pipeline — Ontology + Storage + Updater + Reasoner —
// across controlled, narrated timesteps. Compliance tests prove the parts
// satisfy their contracts in isolation; scenarios prove the parts compose
// into the behaviors the architecture promises.
//
// Run with:
//
//	go test -v -run TestScenario ./internal/minimal/tests/...
//
// Every scenario emits a compact narrative via t.Logf so the verbose output
// reads like a story. Hard assertions guard the mechanics that must not
// regress when we refactor.

package minimal_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/internal/minimal"
	"github.com/DiyazY/di-agent/pkg/peers"
	"github.com/DiyazY/di-agent/pkg/profiles"
	"github.com/DiyazY/di-agent/pkg/semmap"
	"github.com/DiyazY/di-agent/pkg/types"
)

// newJSONDecoder / newJSONEncoder are tiny wrappers so the scenario HTTP
// surface mirrors the daemon's wire format without import gymnastics.
func newJSONDecoder(r *http.Request) *json.Decoder { return json.NewDecoder(r.Body) }
func newJSONEncoder(w io.Writer) *json.Encoder     { return json.NewEncoder(w) }

// ── scenarioAgent: SemanticMap + handles for inspection ──────────────────────

type scenarioAgent struct {
	sm       *semmap.SemanticMap
	storage  *minimal.InMemoryStorage
	ontology *minimal.SpecOntology
	updater  *minimal.EMAUpdater
}

// newScenarioAgent wires the same edge-minimal stack a production daemon
// would (InMemoryStorage + SpecOntology + EMAUpdater +
// RuleEngineReasoner + DisabledProposer) and seeds storage from the
// ontology's default Di-Select bootstrap. It returns the SemanticMap plus
// raw handles so scenarios can call ontology mutations and inspect storage
// state directly — operations the facade intentionally hides from agent code.
func newScenarioAgent(t *testing.T) *scenarioAgent {
	t.Helper()
	storage := minimal.NewInMemoryStorage()
	ontology := minimal.NewOntologyFromSpec(mustSpec())
	updater := minimal.NewEMAUpdater(storage, 0.2, 500)
	reasoner := minimal.NewRuleEngineReasoner(storage, ontology, 0.5, nil, nil)
	proposer := minimal.NewDisabledProposer()

	seedReasonerState(t, storage, ontology)

	return &scenarioAgent{
		sm:       semmap.New(storage, ontology, updater, reasoner, proposer, minimal.NewDisabledTuner()),
		storage:  storage,
		ontology: ontology,
		updater:  updater,
	}
}

// edgeSnapshot is a frozen view of one edge's state at a moment in time.
// Used to print "T=N: edge X is here" narrative rows.
type edgeSnapshot struct {
	FromID, ToID, PropID                          string
	PriorWeight, EMAWeight, Effective, Confidence float64
	NObservations                                 int
}

func (s edgeSnapshot) String() string {
	return fmt.Sprintf("%s→%s[%s]  prior=%.3f  ema=%.3f  conf=%.3f  effective=%.3f  n=%d",
		s.FromID, s.ToID, s.PropID, s.PriorWeight, s.EMAWeight, s.Confidence, s.Effective, s.NObservations)
}

func snapEdgeByPropID(t *testing.T, s *minimal.InMemoryStorage, propID string) edgeSnapshot {
	t.Helper()
	edges, _ := s.AllEdges()
	for _, e := range edges {
		if e.PropositionID == propID {
			return edgeSnapshot{
				FromID:        e.FromID,
				ToID:          e.ToID,
				PropID:        e.PropositionID,
				PriorWeight:   e.PriorWeight,
				EMAWeight:     e.EMAWeight,
				Effective:     (1-e.Confidence)*e.PriorWeight + e.Confidence*e.EMAWeight,
				Confidence:    e.Confidence,
				NObservations: e.NObservations,
			}
		}
	}
	t.Fatalf("propID %q not in storage", propID)
	return edgeSnapshot{}
}

// ── Scenario 1: Cold start — every edge defers to its prior ──────────────────

func TestScenario_ColdStart(t *testing.T) {
	a := newScenarioAgent(t)

	t.Log("Scenario: cold start — ontology bootstrapped, no observations yet.")
	t.Log("Expected: confidence=0 on every edge, effective value equals the prior, EMA equals the prior.")

	edges, err := a.storage.AllEdges()
	if err != nil {
		t.Fatal(err)
	}
	if want := len(mustSpec().Propositions); len(edges) != want {
		t.Fatalf("expected %d seeded edges (one per spec proposition); got %d", want, len(edges))
	}

	sort.Slice(edges, func(i, j int) bool { return edges[i].PropositionID < edges[j].PropositionID })
	for _, e := range edges {
		if e.Confidence != 0.0 {
			t.Errorf("edge %s starts with confidence=%.3f; expected 0.0", e.PropositionID, e.Confidence)
		}
		if e.EMAWeight != e.PriorWeight {
			t.Errorf("edge %s EMA=%.3f != prior=%.3f at cold start",
				e.PropositionID, e.EMAWeight, e.PriorWeight)
		}
		if e.NObservations != 0 {
			t.Errorf("edge %s NObservations=%d at cold start; expected 0", e.PropositionID, e.NObservations)
		}
	}

	cost, err := a.sm.CostOfAction("pod-scheduling", "node_1")
	if err != nil {
		t.Fatal(err)
	}
	if cost.Confidence != 0.0 {
		t.Errorf("aggregate confidence at cold start = %.3f; expected 0.0", cost.Confidence)
	}
	t.Logf("T=0 (cold start)")
	t.Logf("  edges seeded: %d", len(edges))
	t.Logf("  aggregate confidence: %.3f", cost.Confidence)
	t.Logf("  decision latency estimate: %.3f  resource cost: %.3f", cost.LatencyEstimate, cost.ResourceCost)
	t.Logf("  graph path length: %d (all edges contribute via priors)", len(cost.GraphPathUsed))
}

// ── Scenario 2: Convergence on a single edge ─────────────────────────────────

func TestScenario_ConvergenceOnOneEdge(t *testing.T) {
	a := newScenarioAgent(t)
	const target = "P10" // PS→RC negative — performance reducing resource overhead
	const observed = 0.85
	const totalObs = 500

	t.Logf("Scenario: stream %d constant observations at value %.2f on edge %s.", totalObs, observed, target)
	t.Log("Expected: confidence climbs 0 → 1, EMA drifts prior → observed, effective crosses over.")
	t.Log("")

	checkpoints := []int{0, 1, 5, 25, 100, 250, 500}
	cpIdx := 0

	for i := 0; i < totalObs; i++ {
		if cpIdx < len(checkpoints) && i == checkpoints[cpIdx] {
			snap := snapEdgeByPropID(t, a.storage, target)
			t.Logf("  T=%-4d  %s", i, snap)
			cpIdx++
		}
		// Find an edge with target propID to discover its (from, to).
		if i == 0 {
			snap := snapEdgeByPropID(t, a.storage, target)
			a.streamObservation(t, snap.FromID, snap.ToID, observed, i)
			continue
		}
		snap := snapEdgeByPropID(t, a.storage, target)
		a.streamObservation(t, snap.FromID, snap.ToID, observed, i)
	}

	final := snapEdgeByPropID(t, a.storage, target)
	t.Logf("  T=%-4d  %s", totalObs, final)

	// Invariants.
	if final.Confidence < 0.999 {
		t.Errorf("confidence should be ≈1.0 after %d obs (convergence=500); got %.3f", totalObs, final.Confidence)
	}
	if math.Abs(final.EMAWeight-observed) > 0.01 {
		t.Errorf("EMA should converge to %.3f after constant stream; got %.3f", observed, final.EMAWeight)
	}
	if math.Abs(final.Effective-observed) > 0.01 {
		t.Errorf("effective value should equal the EMA at confidence=1.0; got effective=%.3f ema=%.3f",
			final.Effective, final.EMAWeight)
	}
}

// streamObservation is a tiny convenience for the per-iteration ingest with a
// deterministic eventID. Errors fail the test fast.
func (a *scenarioAgent) streamObservation(t *testing.T, fromID, toID string, value float64, i int) {
	t.Helper()
	if err := a.sm.Ingest(fromID, toID, value, fmt.Sprintf("evt-%s→%s-%d", fromID, toID, i)); err != nil {
		t.Fatalf("ingest failed at i=%d: %v", i, err)
	}
}

// ── Scenario 3: Per-KD decisions differ ──────────────────────────────────────

func TestScenario_PerKDDecisionsDiffer(t *testing.T) {
	pwPath := findPriorWeightsFileForScenarios(t)

	smK3s, _, err := profiles.Build("edge-minimal", profiles.Config{
		DomainSpec: mustSpec(),
		EMAAlpha:   0.2, ConvergenceThreshold: 500, MinTrustScore: 0.5,
		PriorWeightsPath: pwPath, KD: "k3s",
	})
	if err != nil {
		t.Fatal(err)
	}
	smK0s, _, err := profiles.Build("edge-minimal", profiles.Config{
		DomainSpec: mustSpec(),
		EMAAlpha:   0.2, ConvergenceThreshold: 500, MinTrustScore: 0.5,
		PriorWeightsPath: pwPath, KD: "k0s",
	})
	if err != nil {
		t.Fatal(err)
	}

	costK3s, _ := smK3s.CostOfAction("pod-scheduling", "node_1")
	costK0s, _ := smK0s.CostOfAction("pod-scheduling", "node_1")

	t.Log("Scenario: two agents, same query, different -kd. Decisions should diverge because the per-KD")
	t.Log("priors land different initial weights on each edge.")
	t.Log("")
	t.Logf("  k3s  →  latency=%.3f  resource_cost=%.3f  confidence=%.3f", costK3s.LatencyEstimate, costK3s.ResourceCost, costK3s.Confidence)
	t.Logf("  k0s  →  latency=%.3f  resource_cost=%.3f  confidence=%.3f", costK0s.LatencyEstimate, costK0s.ResourceCost, costK0s.Confidence)

	if costK3s.LatencyEstimate == costK0s.LatencyEstimate &&
		costK3s.ResourceCost == costK0s.ResourceCost {
		t.Errorf("k3s and k0s agents produced identical cost estimates — per-KD seeding is not steering behavior")
	}
}

// ── Scenario 4: Deprecation shrinks the graph (but preserves the edge) ───────

func TestScenario_DeprecationShrinksGraph(t *testing.T) {
	a := newScenarioAgent(t)

	before, _ := a.sm.CostOfAction("pod-scheduling", "node_1")
	t.Log("Scenario: deprecate P1. Reasoner must skip it; storage must retain the EdgeDescriptor.")
	t.Log("")
	t.Logf("  before deprecation  graph path length = %d", len(before.GraphPathUsed))
	t.Logf("                       latency=%.3f  resource_cost=%.3f  confidence=%.3f",
		before.LatencyEstimate, before.ResourceCost, before.Confidence)

	if err := a.ontology.Deprecate(firstSpecProp(), "scenario test"); err != nil {
		t.Fatal(err)
	}

	after, _ := a.sm.CostOfAction("pod-scheduling", "node_1")
	t.Logf("  after deprecation   graph path length = %d", len(after.GraphPathUsed))
	t.Logf("                       latency=%.3f  resource_cost=%.3f  confidence=%.3f",
		after.LatencyEstimate, after.ResourceCost, after.Confidence)

	if len(after.GraphPathUsed) != len(before.GraphPathUsed)-1 {
		t.Errorf("graph path should shrink by exactly 1 after deprecating one proposition; got %d → %d",
			len(before.GraphPathUsed), len(after.GraphPathUsed))
	}

	// Storage must still hold the deprecated edge — soft delete only.
	all, _ := a.storage.AllEdges()
	stillPresent := false
	for _, e := range all {
		if e.PropositionID == firstSpecProp() {
			stillPresent = true
			break
		}
	}
	if !stillPresent {
		t.Error("deprecated edge P1 disappeared from storage — Deprecate must be soft-delete, not removal")
	}

	// Ontology surface still includes the deprecated proposition (flagged).
	props, _ := a.ontology.Propositions()
	foundDeprecated := false
	for _, p := range props {
		if p.PropositionID == firstSpecProp() {
			if !p.Deprecated {
				t.Error("P1 not flagged Deprecated in ontology after Deprecate()")
			}
			foundDeprecated = true
		}
	}
	if !foundDeprecated {
		t.Error("P1 vanished from Propositions() after Deprecate() — should remain visible with Deprecated=true")
	}
}

// ── Scenario 5: Idempotent replay ────────────────────────────────────────────

func TestScenario_IdempotentReplay(t *testing.T) {
	a := newScenarioAgent(t)

	t.Log("Scenario: stream 200 observations, capture state, replay them all with identical eventIDs.")
	t.Log("Expected: state byte-identical after replay. Then re-stream with NEW eventIDs — state changes.")
	t.Log("")

	const target = "P10" // PS→RC
	startSnap := snapEdgeByPropID(t, a.storage, target)

	// First pass.
	for i := 0; i < 200; i++ {
		a.streamObservation(t, startSnap.FromID, startSnap.ToID, 0.7, i)
	}
	afterFirst := snapEdgeByPropID(t, a.storage, target)
	t.Logf("  after first pass (200 obs):   %s", afterFirst)

	// Replay — same eventIDs.
	for i := 0; i < 200; i++ {
		a.streamObservation(t, startSnap.FromID, startSnap.ToID, 0.7, i)
	}
	afterReplay := snapEdgeByPropID(t, a.storage, target)
	t.Logf("  after replay (same evtIDs):   %s", afterReplay)

	if afterReplay.NObservations != afterFirst.NObservations ||
		afterReplay.EMAWeight != afterFirst.EMAWeight ||
		afterReplay.Confidence != afterFirst.Confidence {
		t.Error("replay with identical eventIDs changed state — idempotency violated")
	}

	// Now stream with new eventIDs starting from offset 1000 — state should advance.
	for i := 1000; i < 1200; i++ {
		a.streamObservation(t, startSnap.FromID, startSnap.ToID, 0.7, i)
	}
	afterNew := snapEdgeByPropID(t, a.storage, target)
	t.Logf("  after 200 new evtIDs:         %s", afterNew)

	if afterNew.NObservations == afterFirst.NObservations {
		t.Error("new eventIDs did not accumulate — idempotency is over-broad (suppressing legitimate updates)")
	}
}

// ── Scenario 6: Audit trail records every mutation ───────────────────────────

func TestScenario_AuditTrailRecordsEverything(t *testing.T) {
	a := newScenarioAgent(t)

	t.Log("Scenario: trigger one of each ontology mutation. GetHistory must contain exactly four events.")
	t.Log("")

	if err := a.ontology.SetPropositionStrength("P3", 0.77); err != nil {
		t.Fatal(err)
	}
	if err := a.ontology.AddConstruct(&types.Construct{
		ConstructID: "PR",
		Name:        "Privacy & Data Sovereignty",
		Description: "Sample new construct added by scenario test",
	}); err != nil {
		t.Fatal(err)
	}
	// Pick a fresh proposition pair that doesn't conflict with anything
	// existing — PS↔SC is not in the Di-Select bootstrap propositions.
	if err := a.ontology.AddValidatedProposition(&types.Proposition{
		PropositionID: "P-scenario",
		FromConstruct: "PS",
		ToConstruct:   "SC",
		Direction:     types.Positive,
		PriorStrength: 0.42,
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.ontology.Deprecate(firstSpecProp(), "scenario test deprecation"); err != nil {
		t.Fatal(err)
	}

	events, err := a.ontology.GetHistory(time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("  recorded %d events in audit log:", len(events))
	for i, e := range events {
		t.Logf("    [%d] %s  kind=%s  target=%s  actor=%s",
			i, e.Timestamp.Format(time.RFC3339Nano), e.Kind, e.TargetID, e.Actor)
	}

	if len(events) != 4 {
		t.Errorf("expected exactly 4 events after 4 mutations; got %d", len(events))
	}

	want := []types.OntologyEventKind{
		types.EventPropositionStrengthSet,
		types.EventConstructAdded,
		types.EventPropositionAdded,
		types.EventPropositionDeprecated,
	}
	for i, w := range want {
		if i >= len(events) {
			break
		}
		if events[i].Kind != w {
			t.Errorf("event[%d] kind=%s; expected %s", i, events[i].Kind, w)
		}
	}

	// Chronological insertion order: each timestamp must be ≥ the previous.
	for i := 1; i < len(events); i++ {
		if events[i].Timestamp.Before(events[i-1].Timestamp) {
			t.Errorf("events not in chronological order: events[%d].Timestamp=%v < events[%d].Timestamp=%v",
				i, events[i].Timestamp, i-1, events[i-1].Timestamp)
		}
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

// ── Scenario 7: Multi-agent coordination — the headline ──────────────────────
//
// Wires three in-process agents (A, B, C), each behind its own httptest
// server with the minimum HTTP surface RecommendPeer needs (/cost, /healthz,
// /offload). Each agent is biased so its CostOfAction returns a different
// ResourceCost: A low (attractive offload target), B high (the agent that
// wants to offload), C medium. Cross-registers A, B, and C in every other
// agent's peer registry.
//
// The scenario walks through:
//  1. Pre-flight: each agent's local CostOfAction.
//  2. B.RecommendPeer — must pick A (the lowest-cost peer, highest savings).
//  3. B → A: POST /offload through peers.Client with realistic budgets.
//  4. A accepts; B updates A's trust by +0.1.
//  5. Final summary table: per-agent self-cost, B's trust scores for A/C,
//     and the final decision narrative.

// coordinationAgent is one node in the multi-agent scenario. Pairs the
// edge-minimal SemanticMap with the httptest.Server and the peer registry so
// the test code can assert on both the wire and the in-memory state.
type coordinationAgent struct {
	name string
	sm   *semmap.SemanticMap
	srv  *httptest.Server
}

// newCoordinationAgent builds one agent and starts its HTTP server. peerURLs
// is the list of OTHER agents this agent should know about — registered with
// the default trust of 0.5.
func newCoordinationAgent(t *testing.T, name string, peerURLs ...string) *coordinationAgent {
	t.Helper()
	sm, _, err := profiles.Build("edge-minimal", profiles.Config{
		DomainSpec:           mustSpec(),
		EMAAlpha:             0.5,
		ConvergenceThreshold: 100,
		MinTrustScore:        0.5,
		PeerURLs:             peerURLs,
	})
	if err != nil {
		t.Fatalf("profiles.Build %s: %v", name, err)
	}
	mux := http.NewServeMux()
	registerScenarioHTTP(mux, sm)
	srv := httptest.NewServer(mux)
	return &coordinationAgent{name: name, sm: sm, srv: srv}
}

// registerScenarioHTTP wires the minimum HTTP surface RecommendPeer needs to
// talk to a remote peer: GET /cost, GET /healthz, POST /offload. The agent
// daemon's full route set lives in cmd/agent (package main) and cannot be
// imported by tests in this package — so we re-wire just what the scenario
// requires. The wire shapes match the daemon exactly so peers.Client works
// against either implementation.
func registerScenarioHTTP(mux *http.ServeMux, sm *semmap.SemanticMap) {
	mux.HandleFunc("GET /cost", func(w http.ResponseWriter, r *http.Request) {
		task := r.URL.Query().Get("task")
		node := r.URL.Query().Get("node")
		cost, err := sm.CostOfAction(task, node)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		writeJSONBody(w, cost)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("POST /offload", func(w http.ResponseWriter, r *http.Request) {
		var req peers.OffloadRequest
		if err := decodeJSON(r, &req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		cost, err := sm.CostOfAction(req.TaskType, req.SourceNodeID)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		resp := peers.OffloadResponse{
			Accepted:             true,
			Reason:               "within budget",
			ExpectedLatency:      cost.LatencyEstimate,
			ExpectedResourceCost: cost.ResourceCost,
		}
		if req.LatencyBudgetMs > 0 && cost.LatencyEstimate > req.LatencyBudgetMs {
			resp.Accepted = false
			resp.Reason = fmt.Sprintf("latency %.2f > budget %.2f", cost.LatencyEstimate, req.LatencyBudgetMs)
		} else if req.EnergyBudgetJoules != nil && cost.ResourceCost > *req.EnergyBudgetJoules {
			resp.Accepted = false
			resp.Reason = fmt.Sprintf("resource cost %.2f > energy budget %.2f", cost.ResourceCost, *req.EnergyBudgetJoules)
		}
		w.Header().Set("Content-Type", "application/json")
		writeJSONBody(w, resp)
	})
}

// biasEnergyTo drives the agent's CostOfAction ResourceCost by pushing every
// RC-terminating edge away from its prior in the cost-increasing direction:
// the reasoner scores resource cost as (effective − prior) × sign(direction),
// so a positive edge climbing above its prior and a negative edge falling below
// it both inflate cost, and an edge sitting at its prior contributes nothing.
//
// `frac` is that deviation as a fraction of the headroom the edge actually has
// (0 = sit at the prior, 1 = ride the cost-increasing bound), so the helper
// cannot ask for an observation outside [0,1] no matter what the priors are.
// The edges are read from live storage rather than named here, so the helper
// follows the domain spec instead of pinning a graph scope. Confidence blending
// scales the realised deviation below the requested one, which is why the
// scenario asserts the ordering of costs and not their magnitudes.
func biasEnergyTo(t *testing.T, sm *semmap.SemanticMap, label string, frac float64) {
	t.Helper()

	edges, err := sm.AllEdges()
	if err != nil {
		t.Fatal(err)
	}
	type target struct {
		from, to string
		obs      float64
	}
	var targets []target
	for _, e := range edges {
		if e.ToID != "RC" {
			continue
		}
		// Headroom differs per direction: a positive edge can climb to 1.0, a
		// negative edge can fall to 0.0.
		obs := e.PriorWeight + frac*(1.0-e.PriorWeight)
		if e.Direction == types.Negative {
			obs = e.PriorWeight * (1.0 - frac)
		}
		targets = append(targets, target{e.FromID, e.ToID, clamp01(obs)})
	}
	if len(targets) == 0 {
		t.Fatal("no RC-terminating edge in storage: the offload scenario has no cost lever")
	}

	const ticks = 200
	for i := 0; i < ticks; i++ {
		for _, tg := range targets {
			ev := fmt.Sprintf("%s-%s-%s-%d", label, tg.from, tg.to, i)
			if err := sm.Ingest(tg.from, tg.to, tg.obs, ev); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// decodeJSON / writeJSONBody are tiny helpers so registerScenarioHTTP does not
// depend on the cmd/agent package. The encodings match the daemon's
// application/json convention.
func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := newJSONDecoder(r)
	return dec.Decode(v)
}

func writeJSONBody(w http.ResponseWriter, v any) {
	enc := newJSONEncoder(w)
	_ = enc.Encode(v)
}

func TestScenario_CoordinationOffload(t *testing.T) {
	t.Log("Scenario 7: three in-process agents coordinate via /peers + /offload.")
	t.Log("")
	t.Log("Setup:")
	t.Log("  Agent A — idle (low energy cost, attractive offload destination)")
	t.Log("  Agent B — loaded (high energy cost, wants to offload)")
	t.Log("  Agent C — medium (in the middle)")
	t.Log("  Each agent runs its own httptest server with /cost, /healthz, /offload.")
	t.Log("")

	// ── Wire each agent (peers are populated in a second pass so we have URLs) ──

	a := newCoordinationAgent(t, "A")
	defer a.srv.Close()
	b := newCoordinationAgent(t, "B")
	defer b.srv.Close()
	c := newCoordinationAgent(t, "C")
	defer c.srv.Close()

	// Cross-register: each agent knows about the other two.
	for _, p := range []struct {
		who *coordinationAgent
		url string
	}{
		{a, b.srv.URL}, {a, c.srv.URL},
		{b, a.srv.URL}, {b, c.srv.URL},
		{c, a.srv.URL}, {c, b.srv.URL},
	} {
		if _, err := p.who.sm.Peers().Add(p.url, ""); err != nil {
			t.Fatalf("register peer on %s: %v", p.who.name, err)
		}
	}

	// ── Bias each agent so CostOfAction returns a distinguishable energy cost ──

	biasEnergyTo(t, a.sm, "A", 0.10) // barely off its prior — cheapest
	biasEnergyTo(t, b.sm, "B", 0.90) // near the cost-increasing bound — priciest
	biasEnergyTo(t, c.sm, "C", 0.50) // in between

	// ── Pre-flight: print each agent's self-cost ────────────────────────────────

	costA, _ := a.sm.CostOfAction("pod-scheduling", "node_self")
	costB, _ := b.sm.CostOfAction("pod-scheduling", "node_self")
	costC, _ := c.sm.CostOfAction("pod-scheduling", "node_self")

	t.Log("Pre-flight self-costs (pod-scheduling):")
	t.Logf("  A:  resource_cost=%.3f  latency=%.3f  conf=%.3f", costA.ResourceCost, costA.LatencyEstimate, costA.Confidence)
	t.Logf("  B:  resource_cost=%.3f  latency=%.3f  conf=%.3f", costB.ResourceCost, costB.LatencyEstimate, costB.Confidence)
	t.Logf("  C:  resource_cost=%.3f  latency=%.3f  conf=%.3f", costC.ResourceCost, costC.LatencyEstimate, costC.Confidence)
	t.Log("")

	// Mandatory ordering for the recommendation to land on A:
	// resource_cost(A) < resource_cost(C) < resource_cost(B).
	if !(costA.ResourceCost < costC.ResourceCost && costC.ResourceCost < costB.ResourceCost) {
		t.Fatalf("scenario precondition failed: need A<C<B, got A=%.3f C=%.3f B=%.3f",
			costA.ResourceCost, costC.ResourceCost, costB.ResourceCost)
	}

	// ── Phase 1: B asks the reasoner for the best peer to offload to ───────────

	t.Log("Phase 1: B.RecommendPeer({task=pod-scheduling, source=node_self}).")
	rec, err := b.sm.RecommendPeer(&types.OffloadContext{
		TaskType:        "pod-scheduling",
		SourceNodeID:    "node_self",
		DataSizeBytes:   1024,
		LatencyBudgetMs: 5000,
	})
	if err != nil {
		t.Fatalf("RecommendPeer: %v", err)
	}

	// Resolve the recommended peer ID back to a name so the log reads cleanly.
	winner := identifyWinner(t, b, a, c, rec.PeerID)
	t.Logf("  → recommended peer: %s (ID=%s)", winner, rec.PeerID)
	t.Logf("  → expected savings: %.3f", rec.ExpectedSavings)
	t.Logf("  → rationale: %s", rec.Rationale)
	t.Log("")

	if winner != "A" {
		t.Fatalf("expected B to recommend A (lowest cost); got %s", winner)
	}
	if rec.ExpectedSavings <= 0 {
		t.Errorf("ExpectedSavings should be positive; got %.3f", rec.ExpectedSavings)
	}

	// ── Phase 2: B actually offloads to A via the wire ─────────────────────────

	t.Log("Phase 2: B → A POST /offload with budgets {latency=5000ms, energy=∞}.")
	preTrustForA := peerByURL(t, b.sm, a.srv.URL).Trust
	resp, err := b.sm.PeerClient().Offload(context.Background(), a.srv.URL, &peers.OffloadRequest{
		TaskType:        "pod-scheduling",
		SourceNodeID:    "node_B",
		DataSizeBytes:   1024,
		LatencyBudgetMs: 5000,
	})
	if err != nil {
		t.Fatalf("Offload call: %v", err)
	}
	t.Logf("  → A.Accepted=%t reason=%q expected_latency=%.3f expected_resource_cost=%.3f",
		resp.Accepted, resp.Reason, resp.ExpectedLatency, resp.ExpectedResourceCost)
	if !resp.Accepted {
		t.Fatalf("expected A to accept; got rejection reason=%q", resp.Reason)
	}

	// Trust update on a successful offload: nudge upward.
	const acceptDelta = 0.10
	aDescOnB := peerByURL(t, b.sm, a.srv.URL)
	if err := b.sm.Peers().UpdateTrust(aDescOnB.ID, acceptDelta); err != nil {
		t.Fatal(err)
	}
	postTrustForA := peerByURL(t, b.sm, a.srv.URL).Trust
	t.Logf("  → B's trust in A: %.3f → %.3f (delta=+%.2f on accept)", preTrustForA, postTrustForA, acceptDelta)
	if postTrustForA <= preTrustForA {
		t.Errorf("trust for A should increase after accept; got %.3f → %.3f", preTrustForA, postTrustForA)
	}
	t.Log("")

	// ── Final summary table ────────────────────────────────────────────────────

	t.Log("Final state:")
	t.Log("  ┌──────┬──────────────┬──────────────────────────────────────────┐")
	t.Log("  │ Node │ Self-energy  │ Role in this scenario                    │")
	t.Log("  ├──────┼──────────────┼──────────────────────────────────────────┤")
	t.Logf("  │  A   │   %.3f      │ idle — accepted offload from B           │", costA.ResourceCost)
	t.Logf("  │  B   │   %.3f      │ loaded — chose A; trust(A) +%.2f         │", costB.ResourceCost, acceptDelta)
	t.Logf("  │  C   │   %.3f      │ medium — eligible but not picked         │", costC.ResourceCost)
	t.Log("  └──────┴──────────────┴──────────────────────────────────────────┘")
	t.Log("")
	t.Logf("B's peer table (post-offload):")
	listB, _ := b.sm.Peers().List()
	for _, p := range listB {
		who := identifyURL(p.URL, a, c)
		t.Logf("  %s  url=%s  trust=%.3f  n_observed=%d", who, p.URL, p.Trust, p.NObserved)
	}
}

// identifyWinner looks up a peer ID against B's registry entries pointing at
// A's and C's URLs so the test log can say "winner=A" instead of "winner=<hex>".
func identifyWinner(t *testing.T, b, a, c *coordinationAgent, peerID string) string {
	t.Helper()
	for _, p := range mustList(t, b.sm.Peers()) {
		if p.ID == peerID {
			switch p.URL {
			case a.srv.URL:
				return "A"
			case c.srv.URL:
				return "C"
			default:
				return "?"
			}
		}
	}
	return "<unknown>"
}

// identifyURL returns the human-friendly label for a peer URL when we know
// which agent (A, C) it points to. B's own entry isn't in B's registry.
func identifyURL(url string, a, c *coordinationAgent) string {
	switch url {
	case a.srv.URL:
		return "A"
	case c.srv.URL:
		return "C"
	default:
		return "?"
	}
}

func mustList(t *testing.T, r *peers.Registry) []*peers.Descriptor {
	t.Helper()
	list, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func peerByURL(t *testing.T, sm *semmap.SemanticMap, url string) *peers.Descriptor {
	t.Helper()
	d, err := sm.Peers().GetByURL(url)
	if err != nil {
		t.Fatal(err)
	}
	if d == nil {
		t.Fatalf("peer with URL %s not registered", url)
	}
	return d
}

// ── Scenario 8: ProposerNaturalDiscovery — full propose→confirm loop ─────────
//
// Verifies the full propose-then-confirm loop driven by natural telemetry
// ingestion:
//  1. Seed the SM with the edge-minimal profile (15 backbone propositions).
//  2. Feed 60 observations that drive a strong CE↔RC correlation that does NOT
//     currently exist in the backbone. (CE→RC is not one of P1-P15.)
//  3. The proposer generates a PendingCandidate for that pair.
//  4. Operator Confirms the candidate → backbone grows to 16 propositions.
//  5. Verified: a new non-deprecated proposition covering CE↔RC exists.
func TestEvolution_ProposerNaturalDiscovery(t *testing.T) {
	s := minimal.NewInMemoryStorage()
	o := minimal.NewOntologyFromSpec(mustSpec())
	seedReasonerState(t, s, o)

	u := minimal.NewEMAUpdater(s, 0.2, 500)
	proposer := minimal.NewMICorrelationProposer(o, 0.7, 20, 80)
	r := minimal.NewRuleEngineReasoner(s, o, 0.5, nil, nil)

	sm := semmap.New(s, o, u, r, proposer, minimal.NewDisabledTuner())
	_ = sm // used indirectly; proposer is wired into sm but we call proposer directly

	// Ingest 60 samples driving CE↔RC correlation:
	// CE (community ecosystem) and RC (resource constraints) are not linked
	// in the bootstrap — a strong correlation should generate a candidate.
	for i := 0; i < 60; i++ {
		x := float64(i) / 60.0
		y := x*0.95 + 0.02 // strong positive linear correlation
		_ = proposer.ObserveConstruct("CE", x)
		_ = proposer.ObserveConstruct("RC", y)
	}

	candidates, err := proposer.GetCandidates()
	if err != nil {
		t.Fatal(err)
	}
	var cand *types.CandidateEdge
	for _, c := range candidates {
		if (c.FromID == "CE" && c.ToID == "RC") || (c.FromID == "RC" && c.ToID == "CE") {
			cand = c
			break
		}
	}
	if cand == nil {
		t.Fatal("expected proposer to generate a CE↔RC candidate after 60 correlated observations")
	}
	if cand.PValue >= 0.05 {
		t.Errorf("expected significant p-value (< 0.05); got %.4f", cand.PValue)
	}

	// Confirm the candidate — backbone grows.
	if err := proposer.Confirm(cand.CandidateID); err != nil {
		t.Fatal(err)
	}

	props, err := o.Propositions()
	if err != nil {
		t.Fatal(err)
	}
	if want := len(mustSpec().Propositions) + 1; len(props) != want {
		t.Errorf("expected %d propositions after confirm; got %d", want, len(props))
	}

	// Verified: a new proposition covering CE↔RC exists and is not deprecated.
	var found bool
	for _, p := range props {
		if !p.Deprecated && ((p.FromConstruct == "CE" && p.ToConstruct == "RC") ||
			(p.FromConstruct == "RC" && p.ToConstruct == "CE")) {
			found = true
			break
		}
	}
	if !found {
		t.Error("confirmed CE↔RC proposition not found in backbone")
	}
}

// ── Scenario 9: Operator Tune and Audit Trail ─────────────────────────────────
//
// Verifies the full operator-tuning pipeline:
//  1. Build an SM with the RuleBasedTuner.
//  2. Tune on the spec's first intent rule — expect every proposition that rule
//     names to move in the direction the rule's delta declares.
//  3. Verify the history contains an "operator-tune" event with the intent text.
//  4. Verify that CostOfAction after tuning is traversable without error.
func TestEvolution_OperatorTuneAndAuditTrail(t *testing.T) {
	s := minimal.NewInMemoryStorage()
	o := minimal.NewOntologyFromSpec(mustSpec())
	seedReasonerState(t, s, o)

	u := minimal.NewEMAUpdater(s, 0.2, 500)
	r := minimal.NewRuleEngineReasoner(s, o, 0.5, nil, nil)
	proposer := minimal.NewDisabledProposer()
	tuner := minimal.NewRuleBasedTunerFromSpec(mustSpec())

	sm := semmap.New(s, o, u, r, proposer, tuner)

	costBefore, err := r.CostOfAction("pod-scheduling", "node_1")
	if err != nil {
		t.Fatal(err)
	}

	applied, err := sm.Tune("prioritize "+firstSpecKeyword(), "test-operator")
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) == 0 {
		t.Fatal("expected at least one adjustment for the spec's first intent")
	}

	// Every proposition the rule names must move in the direction its delta
	// declares — unless the floor or ceiling already pinned it there, in which
	// case standing still is the correct outcome.
	rule := mustSpec().IntentRules[0]
	seen := map[string]bool{}
	for _, a := range applied {
		delta, named := rule.Deltas[a.PropositionID]
		if !named {
			t.Errorf("%s adjusted but intent %q does not name it", a.PropositionID, rule.Intent)
			continue
		}
		seen[a.PropositionID] = true
		moved := a.NewStrength - a.OldStrength
		switch {
		case delta > 0 && moved < 0:
			t.Errorf("%s: intent raises it by %.2f but strength fell %.3f → %.3f",
				a.PropositionID, delta, a.OldStrength, a.NewStrength)
		case delta < 0 && moved > 0:
			t.Errorf("%s: intent lowers it by %.2f but strength rose %.3f → %.3f",
				a.PropositionID, delta, a.OldStrength, a.NewStrength)
		}
	}
	for propID := range rule.Deltas {
		if !seen[propID] {
			t.Errorf("intent %q names %s but no adjustment was applied to it", rule.Intent, propID)
		}
	}

	// Audit trail: history must contain "operator-tune" event.
	events, err := o.GetHistory(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var tuneEvent *types.OntologyEvent
	for _, e := range events {
		if string(e.Kind) == "operator-tune" {
			tuneEvent = e
			break
		}
	}
	if tuneEvent == nil {
		t.Fatal("expected 'operator-tune' event in history")
	}
	if tuneEvent.Detail["intent"] != "prioritize "+firstSpecKeyword() {
		t.Errorf("operator-tune event has wrong intent: %v", tuneEvent.Detail["intent"])
	}
	if tuneEvent.Actor != "test-operator" {
		t.Errorf("operator-tune event has wrong actor: %q", tuneEvent.Actor)
	}

	// After tuning, cost is still computable (graph still traversable).
	costAfter, err := r.CostOfAction("pod-scheduling", "node_1")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("cost before tune: resource_cost=%.4f; cost after: resource_cost=%.4f",
		costBefore.ResourceCost, costAfter.ResourceCost)
}

// findPriorWeightsFileForScenarios walks up to locate prior_weights.json so
// scenarios are runnable from any working directory the test runner picks.
func findPriorWeightsFileForScenarios(t *testing.T) string {
	t.Helper()
	dir, _ := filepath.Abs(".")
	for i := 0; i < 6; i++ {
		candidates := []string{
			filepath.Join(dir, "prior_weights.json"),
			filepath.Join(dir, "semantic-map", "prior_weights.json"),
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				return c
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Skip("prior_weights.json not found — per-KD scenario skipped")
	return ""
}
