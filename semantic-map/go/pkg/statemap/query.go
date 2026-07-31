package statemap

import (
	"fmt"
	"sort"
	"strings"
)

// Query selects part of the map. A zero Query returns the active state, which is
// the answer to "what is this system right now" — the question an agent asks most
// often, so it is the one that needs no arguments.
type Query struct {
	// IDs restricts to these properties. Empty means all.
	IDs []string

	// Kinds restricts to observed or derived properties. Empty means both.
	Kinds []Kind

	// Statuses restricts by lifecycle. Empty means active and stale but NOT
	// retired: a retired property is not part of the current system, and returning
	// it by default would put withdrawn state into an agent's answers.
	Statuses []Status

	// MinConfidence drops properties the agent knows too little about.
	MinConfidence float64

	// RelatedTo restricts to properties reachable from this one in one hop, in
	// either direction, which is the neighbourhood a decision about it consults.
	RelatedTo string
}

// StateView is the answer to a Query: properties, the relationships among them,
// and the revision the view was taken at.
//
// The revision is part of the payload rather than a header because a caller
// comparing two views needs to know whether they are looking at one system at two
// times or two answers to the same question.
type StateView struct {
	// Owner is the system these properties describe. It travels with the view because
	// a view is the thing that crosses node boundaries, and a property arriving
	// without a subject is not usable: a reader cannot tell whose CPU it is.
	Owner string `json:"owner,omitempty"`

	Revision      uint64         `json:"revision"`
	Properties    []Property     `json:"properties"`
	Relationships []Relationship `json:"relationships"`

	// Counts summarises the whole map, not just the selection, so a caller can see
	// what the filter excluded.
	Counts StateCounts `json:"counts"`
}

// StateCounts is a census of the map.
type StateCounts struct {
	PropertiesTotal    int `json:"properties_total"`
	PropertiesActive   int `json:"properties_active"`
	PropertiesStale    int `json:"properties_stale"`
	PropertiesRetired  int `json:"properties_retired"`
	Observed           int `json:"observed"`
	Derived            int `json:"derived"`
	RelationshipsTotal int `json:"relationships_total"`
	RelationshipsLive  int `json:"relationships_live"`
	Seeded             int `json:"seeded"`
	Learned            int `json:"learned"`
	Asserted           int `json:"asserted"`
	Unobserved         int `json:"properties_unobserved"`
}

// State answers a Query.
func (m *Map) State(q Query) StateView {
	m.mu.RLock()
	defer m.mu.RUnlock()

	wantStatus := map[Status]bool{}
	if len(q.Statuses) == 0 {
		wantStatus[Active] = true
		wantStatus[Stale] = true
	} else {
		for _, s := range q.Statuses {
			wantStatus[s] = true
		}
	}
	wantKind := map[Kind]bool{}
	for _, k := range q.Kinds {
		wantKind[k] = true
	}
	wantID := map[string]bool{}
	for _, id := range q.IDs {
		wantID[id] = true
	}

	neighbourhood := map[string]bool{}
	if q.RelatedTo != "" {
		neighbourhood[q.RelatedTo] = true
		for _, r := range m.relationships {
			if r.From == q.RelatedTo {
				neighbourhood[r.To] = true
			}
			if r.To == q.RelatedTo {
				neighbourhood[r.From] = true
			}
		}
	}

	view := StateView{Owner: m.owner, Revision: m.revision, Counts: m.censusLocked()}
	selected := map[string]bool{}
	for _, p := range m.properties {
		if !wantStatus[p.Status] {
			continue
		}
		if len(wantKind) > 0 && !wantKind[p.Kind] {
			continue
		}
		if len(wantID) > 0 && !wantID[p.ID] {
			continue
		}
		if q.RelatedTo != "" && !neighbourhood[p.ID] {
			continue
		}
		if p.Confidence < q.MinConfidence {
			continue
		}
		view.Properties = append(view.Properties, *p)
		selected[p.ID] = true
	}

	for _, r := range m.relationships {
		if !selected[r.From] || !selected[r.To] {
			continue
		}
		if r.Status == Retired && !wantStatus[Retired] {
			continue
		}
		view.Relationships = append(view.Relationships, *r)
	}

	sort.Slice(view.Properties, func(i, j int) bool {
		return view.Properties[i].ID < view.Properties[j].ID
	})
	sortRelationships(view.Relationships)
	return view
}

// Property returns one property.
func (m *Map) Property(id string) (Property, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.properties[id]
	if !ok {
		return Property{}, false
	}
	return *p, true
}

// Relationship returns one relationship.
func (m *Map) Relationship(id string) (Relationship, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.relationships[id]
	if !ok {
		return Relationship{}, false
	}
	return *r, true
}

// Relationships returns every relationship, optionally filtered by endpoint.
func (m *Map) Relationships(from, to string) []Relationship {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Relationship
	for _, r := range m.relationships {
		if from != "" && r.From != from {
			continue
		}
		if to != "" && r.To != to {
			continue
		}
		out = append(out, *r)
	}
	sortRelationships(out)
	return out
}

// Census summarises the map.
func (m *Map) Census() StateCounts {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.censusLocked()
}

func (m *Map) censusLocked() StateCounts {
	var c StateCounts
	for _, p := range m.properties {
		c.PropertiesTotal++
		switch p.Status {
		case Active:
			c.PropertiesActive++
		case Stale:
			c.PropertiesStale++
		case Retired:
			c.PropertiesRetired++
		}
		switch p.Kind {
		case Observed:
			c.Observed++
		case Derived:
			c.Derived++
		}
		if p.NObservations == 0 {
			c.Unobserved++
		}
	}
	for _, r := range m.relationships {
		c.RelationshipsTotal++
		if r.Status != Retired {
			c.RelationshipsLive++
		}
		switch r.Provenance {
		case Seeded:
			c.Seeded++
		case Learned:
			c.Learned++
		case Asserted:
			c.Asserted++
		}
	}
	return c
}

// Explain renders the neighbourhood of one property as text: its value and
// lifecycle, what relates to it, and what it relates to.
//
// This is the map answering for itself. An agent's rationale can quote it, and an
// operator can read it without a client, which matters because the first question
// after a surprising decision is always "what did it think was going on".
func (m *Map) Explain(id string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	p, ok := m.properties[id]
	if !ok {
		return "", fmt.Errorf("property %q not found", id)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s", p.ID, p.Kind)
	if p.Unit != "" {
		fmt.Fprintf(&b, ", %s", p.Unit)
	}
	fmt.Fprintf(&b, ") = %.4f\n", p.Value)
	fmt.Fprintf(&b, "  status      %s", p.Status)
	if p.RetiredReason != "" {
		fmt.Fprintf(&b, " (%s)", p.RetiredReason)
	}
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "  confidence  %.3f from %d observations\n", p.Confidence, p.NObservations)
	if p.NObservations == 0 {
		fmt.Fprintf(&b, "              no observations yet: the value is an assumption, not a measurement\n")
	}
	if !p.LastObserved.IsZero() {
		fmt.Fprintf(&b, "  observed    first %s, last %s\n",
			p.FirstObserved.UTC().Format("15:04:05"), p.LastObserved.UTC().Format("15:04:05"))
	}
	if p.Source != "" {
		fmt.Fprintf(&b, "  source      %s\n", p.Source)
	}
	if p.OutOfRange > 0 {
		fmt.Fprintf(&b, "  WARNING     %d readings outside declared range %v\n", p.OutOfRange, p.Range)
	}
	if p.Kind == Derived {
		fmt.Fprintf(&b, "  aggregates  %s\n", describeIDs(p.Members))
		for _, mid := range p.Members {
			if mem, ok := m.properties[mid]; ok {
				fmt.Fprintf(&b, "                %s\n", mem.String())
			} else {
				fmt.Fprintf(&b, "                %s (absent from the map)\n", mid)
			}
		}
	}

	var incoming, outgoing []Relationship
	for _, r := range m.relationships {
		if r.To == id {
			incoming = append(incoming, *r)
		}
		if r.From == id {
			outgoing = append(outgoing, *r)
		}
	}
	sortRelationships(incoming)
	sortRelationships(outgoing)

	if len(incoming) > 0 {
		fmt.Fprintln(&b, "  influenced by")
		for i := range incoming {
			fmt.Fprintf(&b, "                %s\n", incoming[i].String())
		}
	}
	if len(outgoing) > 0 {
		fmt.Fprintln(&b, "  influences")
		for i := range outgoing {
			fmt.Fprintf(&b, "                %s\n", outgoing[i].String())
		}
	}
	if len(incoming) == 0 && len(outgoing) == 0 {
		fmt.Fprintln(&b, "  isolated: no relationship touches this property, so nothing about "+
			"it propagates to any other part of the model")
	}
	return b.String(), nil
}

func sortRelationships(rs []Relationship) {
	sort.Slice(rs, func(i, j int) bool { return rs[i].ID < rs[j].ID })
}
