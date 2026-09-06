package statemap

import (
	"fmt"
	"sync"
	"time"
)

// EventKind enumerates what can happen to the map.
type EventKind string

const (
	EventPropertyDeclared   EventKind = "property.declared"
	EventPropertyRedeclared EventKind = "property.redeclared"
	EventPropertyAdmitted   EventKind = "property.admitted"
	EventPropertyStale      EventKind = "property.stale"
	EventPropertyRetired    EventKind = "property.retired"
	EventPropertyConflict   EventKind = "property.conflict"
	EventHookFailed         EventKind = "hook.failed"

	EventRelationshipDeclared EventKind = "relationship.declared"
	EventRelationshipAsserted EventKind = "relationship.asserted"
	EventRelationshipRetired  EventKind = "relationship.retired"

	// EventOperatorIntent records one operator action that spanned several
	// relationships, alongside the individual assertions it produced.
	//
	// It exists because the per-relationship records lose the thing an operator
	// actually did. "prioritize performance" is one act with one reason; it lands as
	// an assertion on each proposition the intent names, in opposite directions,
	// and reading those separately afterwards gives no way to tell a coordinated
	// adjustment from two unrelated ones that happened to be adjacent in the log.
	EventOperatorIntent EventKind = "operator.intent"

	// EventDecision records an answer the agent gave, with the state it read.
	EventDecision EventKind = "decision"
)

// Event is one entry in the journal: a structural change, or a decision.
type Event struct {
	Revision uint64         `json:"revision"`
	At       time.Time      `json:"at"`
	Kind     EventKind      `json:"kind"`
	Target   string         `json:"target"`
	Actor    string         `json:"actor"`
	Detail   map[string]any `json:"detail,omitempty"`

	// Decision is set on EventDecision entries.
	Decision *Decision `json:"decision,omitempty"`
}

// Decision is a complete record of one answer: the question, the answer, and the
// state that produced it.
//
// The inputs are copied in, not referenced. A decision has to stay reconstructible
// after the map moves on, and a reference into live state would silently become a
// record of a later system.
type Decision struct {
	ID       string    `json:"id"`
	At       time.Time `json:"at"`
	Revision uint64    `json:"revision"`

	// Question and Answer are the caller's terms, kept opaque so the journal does
	// not need to know every kind of decision the agent can make.
	Question string         `json:"question"`
	Answer   map[string]any `json:"answer"`

	// PropertiesRead and RelationshipsRead are the state as it stood, so the answer
	// can be re-derived. This is the difference between a log line and an audit
	// trail: not "cost was 0.3" but "cost was 0.3 because these properties held
	// these values and these relationships this strength, at this revision".
	PropertiesRead    []Property     `json:"properties_read"`
	RelationshipsRead []Relationship `json:"relationships_read"`

	// Assumptions are the hypothetical source values an estimate substituted, and
	// Excluded the subjects or properties taken to their floor. Recorded so a
	// counterfactual is re-derivable: what was observed AND what was supposed.
	Assumptions map[string]float64 `json:"assumptions,omitempty"`
	Excluded    []string           `json:"excluded,omitempty"`

	// Rationale is the human-readable form of the same content.
	Rationale string `json:"rationale"`

	// Caveats are the reasons the answer might be weak: stale inputs, low
	// confidence, retired relationships skipped. An agent that reports these is
	// reviewable; one that reports only its conclusion is not.
	Caveats []string `json:"caveats,omitempty"`
}

// Journal is a bounded, append-only record of changes and decisions.
//
// Bounded because an agent on a constrained node cannot grow a log without limit;
// append-only because the record's value is that nothing rewrites it. When the
// bound is reached the oldest entries are dropped and the count of dropped entries
// is reported, so a reader can tell that the record is partial instead of
// assuming they are seeing everything.
type Journal struct {
	mu       sync.RWMutex
	events   []Event
	capacity int
	dropped  uint64

	decisions map[string]*Decision
}

// DefaultJournalCapacity bounds the in-memory record.
const DefaultJournalCapacity = 2000

// NewJournal builds a journal. capacity <= 0 uses DefaultJournalCapacity.
func NewJournal(capacity int) *Journal {
	if capacity <= 0 {
		capacity = DefaultJournalCapacity
	}
	return &Journal{
		capacity:  capacity,
		events:    make([]Event, 0, 64),
		decisions: make(map[string]*Decision),
	}
}

func (j *Journal) append(e Event) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.events = append(j.events, e)
	if len(j.events) > j.capacity {
		drop := len(j.events) - j.capacity
		// Copy forward rather than reslice: reslicing keeps the dropped entries
		// alive behind the slice header, which on a memory-budgeted node is a leak
		// that only shows up under long uptime.
		j.events = append(j.events[:0], j.events[drop:]...)
		j.dropped += uint64(drop)
		for id, d := range j.decisions {
			if d.Revision < j.events[0].Revision {
				delete(j.decisions, id)
			}
		}
	}
	if e.Decision != nil {
		j.decisions[e.Decision.ID] = e.Decision
	}
}

// RecordDecision appends a decision and returns it.
func (j *Journal) RecordDecision(d *Decision) *Decision {
	if d == nil {
		return nil
	}
	j.append(Event{
		Revision: d.Revision,
		At:       d.At,
		Kind:     EventDecision,
		Target:   d.ID,
		Actor:    "agent",
		Decision: d,
	})
	return d
}

// Events returns entries at or after sinceRevision, oldest first.
func (j *Journal) Events(sinceRevision uint64, limit int) []Event {
	j.mu.RLock()
	defer j.mu.RUnlock()
	out := make([]Event, 0, len(j.events))
	for _, e := range j.events {
		if e.Revision >= sinceRevision {
			out = append(out, e)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// Decision returns one decision by ID, and whether it is still held. A decision
// evicted by the capacity bound reports false rather than an empty record, so a
// caller cannot mistake "dropped" for "never happened".
func (j *Journal) Decision(id string) (*Decision, bool) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	d, ok := j.decisions[id]
	if !ok {
		return nil, false
	}
	return d.clone(), true
}

// clone is a deep copy of the audit record: the maps and slices a caller could
// write into are copied, so what the journal remembers cannot change through a
// returned decision.
func (d *Decision) clone() *Decision {
	c := *d
	if d.Assumptions != nil {
		c.Assumptions = make(map[string]float64, len(d.Assumptions))
		for k, v := range d.Assumptions {
			c.Assumptions[k] = v
		}
	}
	c.Excluded = append([]string(nil), d.Excluded...)
	c.Caveats = append([]string(nil), d.Caveats...)
	if d.PropertiesRead != nil {
		c.PropertiesRead = make([]Property, len(d.PropertiesRead))
		for i := range d.PropertiesRead {
			c.PropertiesRead[i] = d.PropertiesRead[i].clone()
		}
	}
	if d.RelationshipsRead != nil {
		c.RelationshipsRead = make([]Relationship, len(d.RelationshipsRead))
		for i, r := range d.RelationshipsRead {
			if r.Established != nil {
				v := *r.Established
				r.Established = &v
			}
			if r.Assertion != nil {
				v := *r.Assertion
				r.Assertion = &v
			}
			c.RelationshipsRead[i] = r
		}
	}
	if d.Answer != nil {
		c.Answer = make(map[string]any, len(d.Answer))
		for k, v := range d.Answer {
			c.Answer[k] = v
		}
	}
	return &c
}

// Decisions returns the most recent decisions, newest first.
func (j *Journal) Decisions(limit int) []*Decision {
	j.mu.RLock()
	defer j.mu.RUnlock()
	out := make([]*Decision, 0, len(j.decisions))
	for i := len(j.events) - 1; i >= 0; i-- {
		if j.events[i].Decision == nil {
			continue
		}
		out = append(out, j.events[i].Decision.clone())
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// Stats reports the journal's coverage, including how many entries were dropped.
func (j *Journal) Stats() (held int, dropped uint64, oldest uint64) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	if len(j.events) > 0 {
		oldest = j.events[0].Revision
	}
	return len(j.events), j.dropped, oldest
}

// ── Decision construction ─────────────────────────────────────────────────────

// DecisionBuilder accumulates the state a decision reads, so the reading and the
// recording cannot drift apart. A caller that reads a property through the builder
// has, by construction, recorded that it read it.
type DecisionBuilder struct {
	m           *Map
	id          string
	question    string
	at          time.Time
	revision    uint64
	props       []Property
	rels        []Relationship
	caveats     []string
	seenProp    map[string]bool
	seenRel     map[string]bool
	rationale   []string
	assumptions map[string]float64
	excluded    []string
}

// Decide starts a decision record pinned to the map's current revision.
func (m *Map) Decide(id, question string) *DecisionBuilder {
	m.mu.RLock()
	rev := m.revision
	now := m.now()
	m.mu.RUnlock()
	return &DecisionBuilder{
		m: m, id: id, question: question, at: now, revision: rev,
		seenProp: map[string]bool{}, seenRel: map[string]bool{},
		assumptions: map[string]float64{},
	}
}

// Property reads a property and records it as an input. A missing or retired
// property is reported as a caveat rather than an error, because an agent should
// answer with what it has and say what it lacked.
func (b *DecisionBuilder) Property(id string) (Property, bool) {
	b.m.mu.RLock()
	p, ok := b.m.properties[id]
	var copied Property
	if ok {
		copied = p.clone()
	}
	b.m.mu.RUnlock()

	if !ok {
		b.caveats = append(b.caveats, fmt.Sprintf("property %s is not in the map", id))
		return Property{}, false
	}
	if !b.seenProp[id] {
		b.seenProp[id] = true
		b.props = append(b.props, copied)
	}
	switch copied.Status {
	case Retired:
		b.caveats = append(b.caveats,
			fmt.Sprintf("property %s is retired (%s); its last value was used", id, copied.RetiredReason))
	case Stale:
		b.caveats = append(b.caveats,
			fmt.Sprintf("property %s is stale: last observed %s", id, copied.LastObserved.UTC().Format(time.RFC3339)))
	}
	if copied.NObservations == 0 {
		b.caveats = append(b.caveats,
			fmt.Sprintf("property %s has no observations; its value is an assumption", id))
	}
	if copied.OutOfRange > 0 {
		b.caveats = append(b.caveats, fmt.Sprintf(
			"property %s has taken %d readings outside its declared range %v — collector and declaration disagree",
			id, copied.OutOfRange, copied.Range))
	}
	return copied, true
}

// RelationshipsInto reads the active relationships terminating at a property and
// records them as inputs. A relationship has no stored staleness; it is as fresh as
// its endpoints, so the caveat is derived from them here.
func (b *DecisionBuilder) RelationshipsInto(propertyID string) []Relationship {
	b.m.mu.RLock()
	var out []Relationship
	// Both endpoints can be stale at once, and both are the reader's business: a
	// single slot per relationship silently kept whichever was checked last.
	staleEnds := map[string][]string{}
	for _, r := range b.m.relationships {
		if r.To != propertyID || r.Status == Retired {
			continue
		}
		out = append(out, *r)
		for _, end := range []string{r.From, r.To} {
			if p, ok := b.m.properties[end]; ok && p.Status == Stale {
				staleEnds[r.ID] = append(staleEnds[r.ID], end)
			}
		}
	}
	b.m.mu.RUnlock()

	sortRelationships(out)
	for _, r := range out {
		if !b.seenRel[r.ID] {
			b.seenRel[r.ID] = true
			b.rels = append(b.rels, r)
		}
		for _, end := range staleEnds[r.ID] {
			b.caveats = append(b.caveats, fmt.Sprintf("relationship %s has a stale endpoint %s", r.ID, end))
		}
	}
	return out
}

// Note adds a rationale line.
func (b *DecisionBuilder) Note(format string, args ...any) {
	b.rationale = append(b.rationale, fmt.Sprintf(format, args...))
}

// Caveat records a reason the answer may be weak.
func (b *DecisionBuilder) Caveat(format string, args ...any) {
	b.caveats = append(b.caveats, fmt.Sprintf(format, args...))
}

// Assume records a hypothetical value for a property, as an input to the decision.
func (b *DecisionBuilder) Assume(id string, v float64) {
	b.assumptions[id] = v
	b.rationale = append(b.rationale, fmt.Sprintf("assumed %s = %.4f", id, v))
}

// Exclude records a subject or property taken to its floor.
func (b *DecisionBuilder) Exclude(x string) { b.excluded = append(b.excluded, x) }

// Commit records the decision in the journal and returns it.
func (b *DecisionBuilder) Commit(answer map[string]any) *Decision {
	parts := make([]string, 0, len(b.rationale)+2)
	parts = append(parts, b.rationale...)
	if len(b.props) > 0 {
		ids := make([]string, 0, len(b.props))
		for _, p := range b.props {
			ids = append(ids, p.String())
		}
		parts = append(parts, "properties: "+describeIDs(ids))
	}
	if len(b.rels) > 0 {
		ids := make([]string, 0, len(b.rels))
		for i := range b.rels {
			ids = append(ids, b.rels[i].String())
		}
		parts = append(parts, "relationships: "+describeIDs(ids))
	}
	var assumptions map[string]float64
	if len(b.assumptions) > 0 {
		assumptions = b.assumptions
	}
	d := &Decision{
		ID:                b.id,
		At:                b.at,
		Revision:          b.revision,
		Question:          b.question,
		Answer:            answer,
		PropertiesRead:    b.props,
		RelationshipsRead: b.rels,
		Assumptions:       assumptions,
		Excluded:          b.excluded,
		Rationale:         joinNonEmpty(parts, "; "),
		Caveats:           b.caveats,
	}
	return b.m.journal.RecordDecision(d)
}

func joinNonEmpty(parts []string, sep string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	s := ""
	for i, p := range out {
		if i > 0 {
			s += sep
		}
		s += p
	}
	return s
}
