package autonomy

import (
	"fmt"

	"github.com/DiyazY/di-agent/pkg/statemap"
)

// Tool is something a reasoning model may call. Every tool reads; none writes — the
// reviewer's only writes are the review actions, taken on the decision model's
// number. Call returns a JSON-encodable result and a one-line digest for the trace.
type Tool interface {
	Name() string
	Description() string
	Parameters() map[string]any // JSON Schema of the arguments
	Call(args map[string]string) (result any, digest string, err error)
}

func init() {
	RegisterTool("get_property", func(m Map) Tool { return getProperty{m} })
	RegisterTool("get_state", func(m Map) Tool { return getState{m} })
	RegisterTool("get_candidates", func(m Map) Tool { return getCandidates{m} })
}

// DefaultTools are the tools a reasoning backend gets unless configured otherwise.
var DefaultTools = []string{"get_property", "get_state", "get_candidates"}

func object(props map[string]any, required ...string) map[string]any {
	p := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		p["required"] = required
	}
	return p
}

func stateOf(m Map) (*statemap.Map, error) {
	st := m.State()
	if st == nil {
		return nil, fmt.Errorf("no state model")
	}
	return st, nil
}

type getProperty struct{ m Map }

func (getProperty) Name() string { return "get_property" }
func (getProperty) Description() string {
	return "One property of the machine: its value, range, observations, and the relationships already in the " +
		"model into and out of it."
}
func (getProperty) Parameters() map[string]any {
	return object(map[string]any{"property": map[string]any{"type": "string",
		"description": "the property id, e.g. cpu_utilization@pod:a"}}, "property")
}
func (t getProperty) Call(args map[string]string) (any, string, error) {
	st, err := stateOf(t.m)
	if err != nil {
		return nil, "", err
	}
	id := args["property"]
	p, ok := st.Property(id)
	if !ok {
		return nil, "", fmt.Errorf("property %q is not in the map", id)
	}
	text, _ := st.Explain(id)
	return map[string]any{
		"property": compact(p), "explanation": text,
		"influenced_by": st.Relationships("", id), "influences": st.Relationships(id, ""),
	}, fmt.Sprintf("get_property %s value=%s %s", id, num(p.Value), p.Status), nil
}

type getState struct{ m Map }

func (getState) Name() string { return "get_state" }
func (getState) Description() string {
	return "Every active property, or one subject's (a workload, a device), with its current value."
}
func (getState) Parameters() map[string]any {
	return object(map[string]any{"subject": map[string]any{"type": "string",
		"description": "optional <kind>:<identity>, e.g. pod:<uid> or disk:sda"}})
}
func (t getState) Call(args map[string]string) (any, string, error) {
	st, err := stateOf(t.m)
	if err != nil {
		return nil, "", err
	}
	view := st.State(statemap.Query{Subject: args["subject"], Statuses: []statemap.Status{statemap.Active}})
	props := make([]map[string]any, 0, len(view.Properties))
	for _, p := range view.Properties {
		props = append(props, compact(p))
	}
	return map[string]any{"revision": view.Revision, "properties": props},
		fmt.Sprintf("get_state subject=%q properties=%d", args["subject"], len(props)), nil
}

type getCandidates struct{ m Map }

func (getCandidates) Name() string { return "get_candidates" }
func (getCandidates) Description() string {
	return "Every candidate relationship touching a property, with its correlation, status and who reviewed it."
}
func (getCandidates) Parameters() map[string]any {
	return object(map[string]any{"property": map[string]any{"type": "string"}}, "property")
}
func (t getCandidates) Call(args map[string]string) (any, string, error) {
	id := args["property"]
	hist, err := t.m.CandidateHistory()
	if err != nil {
		return nil, "", err
	}
	var rows []map[string]any
	for _, h := range hist {
		if h.FromID != id && h.ToID != id {
			continue
		}
		row := map[string]any{"candidate": h.CandidateID, "abs_r": h.MIScore, "n": h.NObservations,
			"status": h.Status.String(), "kind": h.Kind, "reason": h.Reason}
		if h.Review != nil {
			row["reviewed_by"], row["review_action"] = h.Review.By, h.Review.Action
		}
		rows = append(rows, row)
	}
	return map[string]any{"property": id, "candidates": rows},
		fmt.Sprintf("get_candidates %s count=%d", id, len(rows)), nil
}

// compact is a property as a tool shows it: the values and lifecycle, not the
// bookkeeping.
func compact(p statemap.Property) map[string]any {
	m := map[string]any{"id": p.ID, "value": p.Value, "n_observations": p.NObservations, "status": p.Status}
	if p.Subject != "" {
		m["subject"] = p.Subject
	}
	if p.Unit != "" {
		m["unit"] = p.Unit
	}
	if p.RangeDeclared {
		m["range"] = p.Range
	}
	if name := p.Labels["name"]; name != "" {
		m["name"] = name
	}
	return m
}
