package explain

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/DiyazY/di-agent/pkg/peers"
	"github.com/DiyazY/di-agent/pkg/types"
)

// SemanticMapReader is the read-only subset of *semmap.SemanticMap that the
// Explainer's tools need. Declaring it as an interface here lets tests inject
// a lightweight stub without spinning up a full daemon and keeps this package
// from importing semmap directly (which would create an import cycle if
// semmap ever grew an Explainer field).
type SemanticMapReader interface {
	AllEdges() ([]*types.EdgeDescriptor, error)
	EdgesByPair(from, to string) ([]*types.EdgeDescriptor, error)
	Constructs() ([]*types.Construct, error)
	Propositions() ([]*types.Proposition, error)
	History(since time.Time) ([]*types.OntologyEvent, error)
	CostOfAction(taskType, nodeID string) (*types.ActionCost, error)
	RecommendPeer(ctx *types.OffloadContext) (*types.PeerRecommendation, error)
	Peers() *peers.Registry
}

// ToolSchema is the LLM-visible descriptor for one tool. The wire shape mirrors
// the OpenAI function-tool schema so we can hand it to any OpenAI-compatible
// backend without translation.
type ToolSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// ToolResult is what a tool returns to the reflection loop: a compact JSON
// payload for the LLM and a short digest string for the ToolTrace.
type ToolResult struct {
	Payload json.RawMessage
	Digest  string
}

// ── Schemas ─────────────────────────────────────────────────────────────────
//
// Ordered to encourage the LLM toward targeted queries (get_cost, get_edges)
// before the sledgehammer (get_graph). No tool mutates state.

var toolSchemas = []ToolSchema{
	{
		Name:        "get_cost",
		Description: "Return the live ActionCost for one node + task-type combination: ResourceCost, Confidence, Rationale, GraphPathUsed. Use this first when the operator asks 'why is my cost X'.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"node_id":   map[string]any{"type": "string", "description": "Node ID (usually 'master')."},
				"task_type": map[string]any{"type": "string", "description": "Task type, e.g. 'pod-scheduling'."},
			},
			"required":             []string{"node_id", "task_type"},
			"additionalProperties": false,
		},
	},
	{
		Name:        "get_edges",
		Description: "Return the current EdgeDescriptors, optionally filtered to a construct pair. Each edge carries prior_weight, ema_weight, confidence, n_observations, deprecated, and direction ('+' or '-').",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"from": map[string]any{"type": "string", "description": "From-construct ID (RC/PS/CO/SC/MU/RR/CE). Omit to skip filtering."},
				"to":   map[string]any{"type": "string", "description": "To-construct ID. Omit to skip filtering."},
			},
			"additionalProperties": false,
		},
	},
	{
		Name:        "get_history",
		Description: "Return OntologyEvents (audit log) since the given RFC3339 timestamp. Use to explain WHEN a strength/deprecation change happened. Pass an empty string for full history.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"since": map[string]any{"type": "string", "description": "RFC3339 timestamp. Empty string returns all history."},
			},
			"additionalProperties": false,
		},
	},
	{
		Name:        "get_peers",
		Description: "Return every registered peer with current trust, url, and n_observed. Use to explain WHY a specific peer was picked or excluded.",
		Parameters: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"additionalProperties": false,
		},
	},
	{
		Name:        "get_recommend",
		Description: "Return the peer recommendation this node would produce right now, with expected savings and trust-weighting rationale. Use sparingly — this consumes reasoner time.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"node_id":   map[string]any{"type": "string", "description": "Source node ID (usually 'master')."},
				"task_type": map[string]any{"type": "string", "description": "Task type, e.g. 'pod-scheduling'."},
			},
			"required":             []string{"node_id", "task_type"},
			"additionalProperties": false,
		},
	},
	{
		Name:        "get_graph",
		Description: "Return the full graph snapshot (all constructs, propositions, and edges). Prefer get_edges or get_cost when the question is narrow — get_graph is expensive to reason over.",
		Parameters: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"additionalProperties": false,
		},
	},
}

// Schemas returns the tool schemas the Explainer exposes to the LLM. The slice
// is defensively copied so callers can't mutate the package state.
func Schemas() []ToolSchema {
	out := make([]ToolSchema, len(toolSchemas))
	copy(out, toolSchemas)
	return out
}

// Dispatch runs one named tool call with the given JSON arguments against the
// reader, returning a compact result the LLM can consume plus a short digest
// for the audit trace. Unknown tool names return ErrUnknownTool.
func Dispatch(reader SemanticMapReader, name string, args map[string]any) (*ToolResult, error) {
	switch name {
	case "get_cost":
		return dispatchGetCost(reader, args)
	case "get_edges":
		return dispatchGetEdges(reader, args)
	case "get_history":
		return dispatchGetHistory(reader, args)
	case "get_peers":
		return dispatchGetPeers(reader)
	case "get_recommend":
		return dispatchGetRecommend(reader, args)
	case "get_graph":
		return dispatchGetGraph(reader)
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnknownTool, name)
	}
}

// ErrUnknownTool is returned when the LLM asks for a tool that isn't
// registered. The reflection loop surfaces this back to the LLM as an
// invocation error so it can retry with a valid name.
var ErrUnknownTool = errors.New("explain: unknown tool")

// ── Dispatchers ─────────────────────────────────────────────────────────────

func dispatchGetCost(r SemanticMapReader, args map[string]any) (*ToolResult, error) {
	node, _ := args["node_id"].(string)
	task, _ := args["task_type"].(string)
	if node == "" || task == "" {
		return nil, errors.New("get_cost: node_id and task_type are required")
	}
	ac, err := r.CostOfAction(task, node)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(ac)
	if err != nil {
		return nil, err
	}
	return &ToolResult{
		Payload: payload,
		Digest:  fmt.Sprintf("cost node=%s task=%s rc=%.4f conf=%.3f", node, task, ac.ResourceCost, ac.Confidence),
	}, nil
}

func dispatchGetEdges(r SemanticMapReader, args map[string]any) (*ToolResult, error) {
	from, _ := args["from"].(string)
	to, _ := args["to"].(string)
	var edges []*types.EdgeDescriptor
	var err error
	if from != "" && to != "" {
		edges, err = r.EdgesByPair(from, to)
	} else {
		var all []*types.EdgeDescriptor
		all, err = r.AllEdges()
		if err == nil {
			for _, e := range all {
				if from != "" && e.FromID != from {
					continue
				}
				if to != "" && e.ToID != to {
					continue
				}
				edges = append(edges, e)
			}
		}
	}
	if err != nil {
		return nil, err
	}
	// Trim: give the LLM just what it needs, not the internal fields.
	type edgeOut struct {
		From          string  `json:"from"`
		To            string  `json:"to"`
		PropositionID string  `json:"proposition_id"`
		Direction     string  `json:"direction"`
		PriorWeight   float64 `json:"prior_weight"`
		EMAWeight     float64 `json:"ema_weight"`
		Confidence    float64 `json:"confidence"`
		NObservations int     `json:"n_observations"`
		Deprecated    bool    `json:"deprecated"`
	}
	out := make([]edgeOut, 0, len(edges))
	for _, e := range edges {
		dir := "+"
		if e.Direction == types.Negative {
			dir = "-"
		}
		out = append(out, edgeOut{
			From: e.FromID, To: e.ToID, PropositionID: e.PropositionID, Direction: dir,
			PriorWeight: e.PriorWeight, EMAWeight: e.EMAWeight, Confidence: e.Confidence,
			NObservations: e.NObservations, Deprecated: e.Deprecated,
		})
	}
	payload, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return &ToolResult{
		Payload: payload,
		Digest:  fmt.Sprintf("edges from=%s to=%s count=%d", from, to, len(out)),
	}, nil
}

func dispatchGetHistory(r SemanticMapReader, args map[string]any) (*ToolResult, error) {
	sinceStr, _ := args["since"].(string)
	var since time.Time
	if sinceStr != "" {
		var err error
		since, err = time.Parse(time.RFC3339, sinceStr)
		if err != nil {
			return nil, fmt.Errorf("get_history: since must be RFC3339: %w", err)
		}
	}
	events, err := r.History(since)
	if err != nil {
		return nil, err
	}
	type eventOut struct {
		Timestamp string         `json:"timestamp"`
		Actor     string         `json:"actor"`
		Kind      string         `json:"kind"`
		TargetID  string         `json:"target_id"`
		Detail    map[string]any `json:"detail"`
	}
	out := make([]eventOut, 0, len(events))
	for _, e := range events {
		out = append(out, eventOut{
			Timestamp: e.Timestamp.UTC().Format(time.RFC3339),
			Actor:     e.Actor,
			Kind:      string(e.Kind),
			TargetID:  e.TargetID,
			Detail:    e.Detail,
		})
	}
	payload, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return &ToolResult{
		Payload: payload,
		Digest:  fmt.Sprintf("history since=%s count=%d", sinceStr, len(out)),
	}, nil
}

func dispatchGetPeers(r SemanticMapReader) (*ToolResult, error) {
	reg := r.Peers()
	if reg == nil {
		payload, _ := json.Marshal([]any{})
		return &ToolResult{Payload: payload, Digest: "peers count=0 (registry disabled)"}, nil
	}
	descs, err := reg.List()
	if err != nil {
		return nil, err
	}
	type peerOut struct {
		ID        string  `json:"id"`
		URL       string  `json:"url"`
		Trust     float64 `json:"trust"`
		NObserved int     `json:"n_observed"`
		LastSeen  string  `json:"last_seen,omitempty"`
	}
	out := make([]peerOut, 0, len(descs))
	for _, d := range descs {
		p := peerOut{ID: d.ID, URL: d.URL, Trust: d.Trust, NObserved: d.NObserved}
		if !d.LastSeen.IsZero() {
			p.LastSeen = d.LastSeen.UTC().Format(time.RFC3339)
		}
		out = append(out, p)
	}
	payload, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return &ToolResult{Payload: payload, Digest: fmt.Sprintf("peers count=%d", len(out))}, nil
}

func dispatchGetRecommend(r SemanticMapReader, args map[string]any) (*ToolResult, error) {
	node, _ := args["node_id"].(string)
	task, _ := args["task_type"].(string)
	if node == "" || task == "" {
		return nil, errors.New("get_recommend: node_id and task_type are required")
	}
	rec, err := r.RecommendPeer(&types.OffloadContext{TaskType: task, SourceNodeID: node})
	if err != nil {
		// Not necessarily fatal: no peers / insufficient trust is a legitimate answer.
		payload, _ := json.Marshal(map[string]string{"error": err.Error()})
		return &ToolResult{Payload: payload, Digest: "recommend error=" + err.Error()}, nil
	}
	payload, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	return &ToolResult{
		Payload: payload,
		Digest:  fmt.Sprintf("recommend peer=%s savings=%.4f", rec.PeerID, rec.ExpectedSavings),
	}, nil
}

func dispatchGetGraph(r SemanticMapReader) (*ToolResult, error) {
	constructs, err := r.Constructs()
	if err != nil {
		return nil, err
	}
	propositions, err := r.Propositions()
	if err != nil {
		return nil, err
	}
	edgesResult, err := dispatchGetEdges(r, nil)
	if err != nil {
		return nil, err
	}
	var edgesRaw json.RawMessage = edgesResult.Payload
	payload, err := json.Marshal(map[string]any{
		"constructs":   constructs,
		"propositions": propositions,
		"edges":        edgesRaw,
	})
	if err != nil {
		return nil, err
	}
	return &ToolResult{
		Payload: payload,
		Digest:  fmt.Sprintf("graph constructs=%d propositions=%d", len(constructs), len(propositions)),
	}, nil
}
