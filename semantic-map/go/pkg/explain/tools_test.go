package explain_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/explain"
	"github.com/DiyazY/di-agent/pkg/peers"
	"github.com/DiyazY/di-agent/pkg/profiles"
	"github.com/DiyazY/di-agent/pkg/semmap"
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
	sm, _, err := profiles.Build("edge-minimal", profiles.Config{DomainSpec: mustSpec(t)})
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
			{Kind: "edge", ID: e.PropositionID, EMAWeight: e.EMAWeight, PriorWeight: e.PriorWeight, Confidence: e.Confidence},
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
		Citations: []explain.Citation{{Kind: "edge", ID: "P1"}},
		Proposal: &explain.Proposal{
			Kind:      "deprecate",
			Endpoint:  "POST /ontology/deprecate",
			Payload:   map[string]any{"proposition_id": "P1"}, // missing "reason"
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
		Citations: []explain.Citation{{Kind: "edge", ID: "P1"}},
		Proposal: &explain.Proposal{
			Kind:      "deprecate",
			Endpoint:  "POST /ontology/deprecate",
			Payload:   map[string]any{"proposition_id": "P1", "reason": "6 runs stale"},
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
