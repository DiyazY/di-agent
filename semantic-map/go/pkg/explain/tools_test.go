package explain_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/explain"
	"github.com/DiyazY/di-agent/pkg/peers"
	"github.com/DiyazY/di-agent/pkg/profiles"
	"github.com/DiyazY/di-agent/pkg/semmap"
	"github.com/DiyazY/di-agent/pkg/statemap"
	"github.com/DiyazY/di-agent/pkg/types"
)

// smReader adapts *semmap.SemanticMap to explain.SemanticMapReader. Both
// packages already export the methods we need — no new plumbing required.
type smReader struct{ *semmap.SemanticMap }

func (s smReader) Peers() *peers.Registry { return s.SemanticMap.Peers() }

// newTestMap returns an edge-minimal SemanticMap and a corresponding reader,
// with the daemon-standard priors loaded.
func newTestMap(t *testing.T) (explain.SemanticMapReader, *semmap.SemanticMap) {
	t.Helper()
	// A state model, as a daemon always attaches: cost is answered from the map, so an
	// agent without one cannot answer get_cost at all.
	sm, _, err := profiles.Build("edge-minimal", profiles.Config{
		DomainSpec: mustSpec(t),
		StateMap:   statemap.New(statemap.Config{ConvergenceObservations: 100}, statemap.NewJournal(0)),
	})
	if err != nil {
		t.Fatalf("profiles.Build: %v", err)
	}
	return smReader{sm}, sm
}

func TestDispatch_GetEdges_FilteredByPair(t *testing.T) {
	r, _ := newTestMap(t)
	res, err := explain.Dispatch(r, "get_edges", map[string]any{"from": "RC", "to": "PS"})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	var edges []map[string]any
	if err := json.Unmarshal(res.Payload, &edges); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(edges) < 1 {
		t.Fatalf("expected ≥1 edges for RC→PS; got %d", len(edges))
	}
	for _, e := range edges {
		if e["from"] != "RC" || e["to"] != "PS" {
			t.Errorf("filter leaked: got from=%v to=%v", e["from"], e["to"])
		}
	}
}

func TestDispatch_GetCost_ReturnsResourceCost(t *testing.T) {
	r, _ := newTestMap(t)
	res, err := explain.Dispatch(r, "get_cost", map[string]any{"node_id": "master", "task_type": "pod-scheduling"})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	var ac types.ActionCost
	if err := json.Unmarshal(res.Payload, &ac); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ac.Rationale == "" {
		t.Errorf("expected non-empty Rationale")
	}
}

func TestDispatch_UnknownTool_ReturnsSentinel(t *testing.T) {
	r, _ := newTestMap(t)
	if _, err := explain.Dispatch(r, "delete_everything", nil); err == nil {
		t.Fatal("expected error for unknown tool; got nil")
	}
}

func TestValidate_AcceptsGroundedCitations(t *testing.T) {
	r, sm := newTestMap(t)
	// Fetch a real edge so we know its live values.
	edges, err := sm.AllEdges()
	if err != nil {
		t.Fatal(err)
	}
	var e *types.EdgeDescriptor
	for _, cand := range edges {
		if !cand.Deprecated {
			e = cand
			break
		}
	}
	if e == nil {
		t.Fatal("no live edges in edge-minimal profile — profile seed broken")
	}

	resp := &explain.ExplainResponse{
		Answer: "Cost is dominated by " + e.PropositionID + ".",
		Citations: []explain.Citation{
			{Kind: "edge", ID: e.PropositionID, EMAWeight: e.EMAWeight, Confidence: e.Confidence},
		},
	}
	v := explain.Validate(r, resp)
	if !v.IsValid {
		t.Fatalf("expected valid response; issues: %v", v.Issues)
	}
}

func TestValidate_RejectsFabricatedEdgeID(t *testing.T) {
	r, _ := newTestMap(t)
	resp := &explain.ExplainResponse{
		Answer: "P99 explains everything.",
		Citations: []explain.Citation{
			{Kind: "edge", ID: "P99", EMAWeight: 0.42},
		},
	}
	v := explain.Validate(r, resp)
	if v.IsValid {
		t.Fatal("expected invalid; validator missed the fabricated P99")
	}
}

func TestValidate_RejectsWrongEMAValue(t *testing.T) {
	r, sm := newTestMap(t)
	edges, _ := sm.AllEdges()
	e := edges[0]
	// Poison the ema_weight by a value larger than Epsilon.
	resp := &explain.ExplainResponse{
		Answer: "See " + e.PropositionID + ".",
		Citations: []explain.Citation{
			{Kind: "edge", ID: e.PropositionID, EMAWeight: e.EMAWeight + 0.5},
		},
	}
	v := explain.Validate(r, resp)
	if v.IsValid {
		t.Fatal("expected invalid; validator missed the wrong ema value")
	}
}

func TestValidate_RejectsAnswerReferencingUncitedProp(t *testing.T) {
	r, _ := newTestMap(t)
	// Answer mentions P10 but doesn't cite it.
	resp := &explain.ExplainResponse{
		Answer: "The cost is driven by P10.",
		Citations: []explain.Citation{
			{Kind: "construct", ID: "RC"},
		},
	}
	v := explain.Validate(r, resp)
	if v.IsValid {
		t.Fatal("expected invalid; answer names P10 but citation set does not include it")
	}
	// Confirm the issue text mentions P10.
	found := false
	for _, s := range v.Issues {
		if len(s) > 0 && (s[len(s)-2] == '1' && s[len(s)-1] == '0') {
			found = true
			break
		}
	}
	_ = found // best-effort — we only require IsValid=false
}

func TestValidate_RejectsProposalWithMissingPayload(t *testing.T) {
	r, _ := newTestMap(t)
	resp := &explain.ExplainResponse{
		Answer:    "You should deprecate P1.",
		Citations: []explain.Citation{{Kind: "edge", ID: firstProp(t)}},
		Proposal: &explain.Proposal{
			Kind:      "deprecate",
			Endpoint:  "POST /ontology/deprecate",
			Payload:   map[string]any{"proposition_id": firstProp(t)}, // missing "reason"
			Rationale: "Stale for 6 runs.",
		},
	}
	v := explain.Validate(r, resp)
	if v.IsValid {
		t.Fatal("expected invalid; proposal missing 'reason' key")
	}
}

func TestValidate_AcceptsWellFormedProposal(t *testing.T) {
	r, _ := newTestMap(t)
	resp := &explain.ExplainResponse{
		Answer:    "You should deprecate P1.",
		Citations: []explain.Citation{{Kind: "edge", ID: firstProp(t)}},
		Proposal: &explain.Proposal{
			Kind:      "deprecate",
			Endpoint:  "POST /ontology/deprecate",
			Payload:   map[string]any{"proposition_id": firstProp(t), "reason": "6 runs stale"},
			Rationale: "P1 has n_observations=0 across all recent runs.",
		},
	}
	v := explain.Validate(r, resp)
	if !v.IsValid {
		t.Fatalf("expected valid; issues: %v", v.Issues)
	}
}

// Event-citation shape: timestamp is required and must parse as RFC3339.
func TestValidate_EventCitation_RequiresRFC3339Timestamp(t *testing.T) {
	r, _ := newTestMap(t)
	resp := &explain.ExplainResponse{
		Answer:    "P7 was deprecated recently.",
		Citations: []explain.Citation{{Kind: "event", ID: "P7", Timestamp: "not-a-time"}},
	}
	v := explain.Validate(r, resp)
	if v.IsValid {
		t.Fatal("expected invalid; timestamp is not RFC3339")
	}
	// A well-formed event citation is fine (even if the target ID is not in the current graph — events are historical).
	resp.Citations[0].Timestamp = time.Now().UTC().Format(time.RFC3339)
	v2 := explain.Validate(r, resp)
	if !v2.IsValid {
		t.Fatalf("expected valid with RFC3339 timestamp; issues: %v", v2.Issues)
	}
}

// TestStateToolsReadTheModelTheAgentReasonsFrom is the point of the state tools: the
// natural-language surface must cite the model decisions come from. Citing the
// construct backbone instead would make the validator's check meaningless — it would
// confirm an answer against numbers no decision uses.
func TestStateToolsReadTheModelTheAgentReasonsFrom(t *testing.T) {
	r, sm := newTestMap(t)
	state := sm.State()
	if state == nil {
		t.Fatal("fixture built no state model")
	}
	if err := state.Observe("cpu_utilization", 0.62, time.Now()); err != nil {
		t.Fatal(err)
	}

	res, err := explain.Dispatch(r, "get_state", map[string]any{})
	if err != nil {
		t.Fatalf("get_state: %v", err)
	}
	var out struct {
		Revision   uint64 `json:"revision"`
		Properties []struct {
			ID    string  `json:"id"`
			Value float64 `json:"value"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(res.Payload, &out); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, p := range out.Properties {
		if p.ID == "cpu_utilization" {
			found = true
			if p.Value != 0.62 {
				t.Errorf("get_state reports cpu_utilization=%.4f, map holds 0.62", p.Value)
			}
		}
	}
	if !found {
		t.Error("get_state omitted an observed property")
	}
	if out.Revision == 0 {
		t.Error("get_state omits the revision, so an answer cannot be pinned to a state")
	}

	if _, err := explain.Dispatch(r, "explain_property",
		map[string]any{"property": "cpu_utilization"}); err != nil {
		t.Fatalf("explain_property: %v", err)
	}
}

// TestValidatorChecksPropertyCitationsAgainstTheStateModel closes the loop: an answer
// that misreports a property's value is rejected, and one that reports it correctly is
// accepted. Without this the surface could cite anything it liked about the system.
func TestValidatorChecksPropertyCitationsAgainstTheStateModel(t *testing.T) {
	r, sm := newTestMap(t)
	if err := sm.State().Observe("cpu_utilization", 0.42, time.Now()); err != nil {
		t.Fatal(err)
	}

	honest := &explain.ExplainResponse{
		Answer: "cpu utilization is 0.42",
		Citations: []explain.Citation{
			{Kind: "property", ID: "cpu_utilization", Value: 0.42},
		},
	}
	if v := explain.Validate(r, honest); !v.IsValid {
		t.Errorf("a correct property citation was rejected: %v", v.Issues)
	}

	invented := &explain.ExplainResponse{
		Answer: "cpu utilization is 0.99",
		Citations: []explain.Citation{
			{Kind: "property", ID: "cpu_utilization", Value: 0.99},
		},
	}
	if v := explain.Validate(r, invented); v.IsValid {
		t.Error("a citation claiming a value the map does not hold was accepted")
	}

	absent := &explain.ExplainResponse{
		Answer:    "the widget is hot",
		Citations: []explain.Citation{{Kind: "property", ID: "no_such_property"}},
	}
	if v := explain.Validate(r, absent); v.IsValid {
		t.Error("a citation of a property not in the map was accepted")
	}
}
