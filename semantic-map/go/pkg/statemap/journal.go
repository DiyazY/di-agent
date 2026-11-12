package statemap

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
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

	// EventRelationshipRegime records a relationship entering or leaving a regime:
	// its recent layer outside the established one's band for a window of folds.
	// Detail: recent, established, band, started.
	EventRelationshipRegime EventKind = "relationship.regime"

	// EventIntervention records what an action the agent took did to a relationship's
	// evidence, whether or not it changed the structure.
	EventIntervention EventKind = "relationship.intervention"

	// EventOperatorIntent records one operator action that spanned several
	// relationships, alongside the individual assertions it produced.
	//
	// It exists because the per-relationship records lose the thing an operator
	// actually did. "prioritize performance" is one act with one reason; it lands as
	// an assertion on each proposition the intent names, in opposite directions,
	// and reading those separately afterwards gives no way to tell a coordinated
	// adjustment from two unrelated ones that happened to be adjacent in the log.
	EventOperatorIntent EventKind = "operator.intent"

	// EventClockStepped records a sweep that found the wall clock and the process's
	// monotonic clock disagreeing by more than the pair window: the wall clock was
	// stepped, by NTP or an operator. Silence is measured on the monotonic clock, so
	// nothing is retired by it; the record says why a timestamp jumped.
	EventClockStepped EventKind = "clock.stepped"

	// EventDecision records an answer the agent gave, with the state it read.
	EventDecision EventKind = "decision"

	// Events a loop outside this package records through Map.RecordEvent.
	EventReliefPhase      EventKind = "relief.phase"
	EventReliefPaused     EventKind = "relief.paused"
	EventReliefResumed    EventKind = "relief.resumed"
	EventAdmissionArrived EventKind = "admission.arrived"
	EventPeerUnreachable  EventKind = "peer.unreachable"
	EventPeerRecovered    EventKind = "peer.recovered"

	// EventDecisionRefused records a commit under an id the journal already holds:
	// the second answer is not recorded, and this says that it was given.
	EventDecisionRefused EventKind = "decision.refused"
)

// ErrDuplicateDecision is RecordDecision's refusal of an id it already holds.
var ErrDuplicateDecision = errors.New("decision id already recorded")

// Event is one entry in the journal: a structural change, or a decision.
type Event struct {
	// Seq is the event's place in the journal's total order, which survives a
	// restore; with the journal's incarnation it names the event uniquely.
	Seq      uint64         `json:"seq"`
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

	// Kind names what sort of answer this is: estimate, admit, relief.act,
	// relief.verify, admission, review, cost and the rest.
	Kind string `json:"kind"`

	// Cites are the earlier decisions this one rests on, in the order cited: a
	// verdict cites its act, a revert its act and verdict. They make the chain
	// walkable both ways, through Journal.Chain.
	Cites []string `json:"cites,omitempty"`

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

	// Inputs are what the decision read that is not a property or a relationship:
	// a candidate's evidence, the text a model was given, a model's answer. Copied in
	// like the rest.
	Inputs map[string]any `json:"inputs,omitempty"`

	// FactsRead are the machine's facts as they stood, when the decision read them.
	FactsRead []Fact `json:"facts_read,omitempty"`

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

	// seq is the last number handed out, to an event or a decision id; incarnation
	// counts the journal's lives, advanced by every restore. Together they make an
	// id or an event unique across restarts, a crash before a save included.
	seq         uint64
	incarnation uint64

	decisions map[string]*Decision
	// citedBy maps a decision id to the held decisions citing it, in seq order.
	citedBy map[string][]string
	// reserved holds ids claimed by decisions being built and not yet committed:
	// a check of decisions alone leaves the window between choosing an id and
	// recording it, in which a concurrent request can choose the same one.
	reserved map[string]bool
}

// DefaultJournalCapacity bounds the in-memory record.
const DefaultJournalCapacity = 2000

// NewJournal builds a journal. capacity <= 0 uses DefaultJournalCapacity.
func NewJournal(capacity int) *Journal {
	if capacity <= 0 {
		capacity = DefaultJournalCapacity
	}
	return &Journal{
		capacity:    capacity,
		events:      make([]Event, 0, 64),
		decisions:   make(map[string]*Decision),
		citedBy:     make(map[string][]string),
		incarnation: 1,
	}
}

func (j *Journal) append(e Event) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.appendLocked(e)
}

func (j *Journal) appendLocked(e Event) {
	j.seq++
	e.Seq = j.seq
	j.events = append(j.events, e)
	if len(j.events) > j.capacity {
		drop := len(j.events) - j.capacity
		// Copy forward rather than reslice: reslicing keeps the dropped entries
		// alive behind the slice header, which on a memory-budgeted node is a leak
		// that only shows up under long uptime.
		for _, old := range j.events[:drop] {
			if old.Decision != nil {
				delete(j.decisions, old.Decision.ID)
			}
		}
		j.events = append(j.events[:0], j.events[drop:]...)
		j.dropped += uint64(drop)
		j.rebuildCitedByLocked()
	}
	if e.Decision != nil {
		j.decisions[e.Decision.ID] = e.Decision
		delete(j.reserved, e.Decision.ID)
		j.indexCitesLocked(e.Decision)
	}
}

// indexCitesLocked adds d to the citedBy lists of the decisions it cites.
func (j *Journal) indexCitesLocked(d *Decision) {
	for _, c := range d.Cites {
		j.citedBy[c] = append(j.citedBy[c], d.ID)
	}
}

// rebuildCitedByLocked rebuilds citedBy from the decisions held, in seq order.
func (j *Journal) rebuildCitedByLocked() {
	j.citedBy = make(map[string][]string)
	for _, e := range j.events {
		if e.Decision != nil {
			j.indexCitesLocked(e.Decision)
		}
	}
}

// reserve claims id for a decision about to be built, and reports whether it was
// free: neither recorded nor claimed by a decision still being built. The check and
// the claim are one step under the lock, so of concurrent callers only one gets it.
// The claim is released when the decision is recorded.
func (j *Journal) reserve(id string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, used := j.decisions[id]; used || j.reserved[id] {
		return false
	}
	if j.reserved == nil {
		j.reserved = map[string]bool{}
	}
	j.reserved[id] = true
	return true
}

// newID issues "<prefix>-<incarnation>.<n>", n a ticket from the event counter.
// It reserves nothing: a ticket is never reissued, so a generated id cannot collide
// with another, and an id whose decision is abandoned leaves nothing behind.
func (j *Journal) newID(prefix string, suffix []string) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.seq++
	id := prefix + "-" + strconv.FormatUint(j.incarnation, 10) + "." + strconv.FormatUint(j.seq, 10)
	if len(suffix) > 0 {
		id += "-" + strings.Join(suffix, "-")
	}
	return id
}

// Seq is the last number handed out, to an event or a decision id.
func (j *Journal) Seq() uint64 {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.seq
}

// Incarnation counts the journal's lives: 1 when fresh, one more on each restore.
func (j *Journal) Incarnation() uint64 {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.incarnation
}

// RecordDecision appends a decision and returns it. An id already recorded is
// refused with ErrDuplicateDecision, since replacing it would rewrite the trace of
// an answer already given; a reservation is not a record.
func (j *Journal) RecordDecision(d *Decision) (*Decision, error) {
	if d == nil {
		return nil, nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, used := j.decisions[d.ID]; used {
		return nil, ErrDuplicateDecision
	}
	j.appendLocked(Event{
		Revision: d.Revision,
		At:       d.At,
		Kind:     EventDecision,
		Target:   d.ID,
		Actor:    "agent",
		Detail:   map[string]any{"kind": d.Kind},
		Decision: d,
	})
	return d, nil
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
	c.Cites = append([]string(nil), d.Cites...)
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
	if d.Inputs != nil {
		c.Inputs = make(map[string]any, len(d.Inputs))
		for k, v := range d.Inputs {
			c.Inputs[k] = copyAny(v)
		}
	}
	if d.FactsRead != nil {
		c.FactsRead = make([]Fact, len(d.FactsRead))
		for i, f := range d.FactsRead {
			if f.Number != nil {
				n := *f.Number
				f.Number = &n
			}
			c.FactsRead[i] = f
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

// Decisions returns the most recent decisions of a kind, newest first; kind ""
// is every kind.
func (j *Journal) Decisions(limit int, kind string) []*Decision {
	j.mu.RLock()
	defer j.mu.RUnlock()
	out := make([]*Decision, 0, len(j.decisions))
	for i := len(j.events) - 1; i >= 0; i-- {
		if d := j.events[i].Decision; d == nil || (kind != "" && d.Kind != kind) {
			continue
		}
		out = append(out, j.events[i].Decision.clone())
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// ChainLink is one ancestor in a chain: the decision when it is still held, or
// Evicted when the journal bound has dropped it.
type ChainLink struct {
	ID       string    `json:"id"`
	Evicted  bool      `json:"evicted,omitempty"`
	Decision *Decision `json:"decision,omitempty"`
}

// Chain is a decision with what it rests on and what rests on it.
type Chain struct {
	Decision *Decision   `json:"decision"`
	Cites    []ChainLink `json:"cites"`
	CitedBy  []string    `json:"cited_by"`
}

// Chain returns a decision's transitive ancestry, depth-first in cite order and
// each id once, and the held decisions citing it directly, in seq order. False
// when id is not held.
func (j *Journal) Chain(id string) (Chain, bool) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	d, ok := j.decisions[id]
	if !ok {
		return Chain{}, false
	}
	c := Chain{Decision: d.clone(), Cites: []ChainLink{},
		CitedBy: append([]string{}, j.citedBy[id]...)}
	seen := map[string]bool{id: true}
	// An explicit stack, not recursion: a full journal can be one long chain.
	stack := reversed(d.Cites)
	for len(stack) > 0 {
		next := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[next] {
			continue
		}
		seen[next] = true
		a, held := j.decisions[next]
		if !held {
			c.Cites = append(c.Cites, ChainLink{ID: next, Evicted: true})
			continue
		}
		c.Cites = append(c.Cites, ChainLink{ID: next, Decision: a.clone()})
		stack = append(stack, reversed(a.Cites)...)
	}
	return c, true
}

func reversed(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[len(ids)-1-i] = id
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
	kind        string
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
	inputs      map[string]any
	facts       []Fact
	cites       []string
}

// Decide starts a decision record of a kind, pinned to the map's current revision.
func (m *Map) Decide(id, kind, question string) *DecisionBuilder {
	m.mu.RLock()
	rev := m.revision
	now := m.now()
	m.mu.RUnlock()
	return &DecisionBuilder{
		m: m, id: id, kind: kind, question: question, at: now, revision: rev,
		seenProp: map[string]bool{}, seenRel: map[string]bool{},
		assumptions: map[string]float64{},
	}
}

// At is the timestamp pinned to this decision.
func (b *DecisionBuilder) At() time.Time { return b.at }

// Property reads a property and records it as an input. A missing or retired
// property is reported as a caveat rather than an error, because an agent should
// answer with what it has and say what it lacked.
func (b *DecisionBuilder) Property(id string) (Property, bool) { return b.read(id, true) }

// PropertyNotUsingRetired reads a property as Property does, for a caller that does
// not use a retired property's last value: the caveat on a retired one says so, and
// the caller records what it did instead.
func (b *DecisionBuilder) PropertyNotUsingRetired(id string) (Property, bool) {
	return b.read(id, false)
}

func (b *DecisionBuilder) read(id string, retiredValueUsed bool) (Property, bool) {
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
		used := "used"
		if !retiredValueUsed {
			used = "not used"
		}
		b.caveats = append(b.caveats,
			fmt.Sprintf("property %s is retired (%s); its last value was %s", id, copied.RetiredReason, used))
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
		if r.To != propertyID || r.Status == Retired || r.Association {
			continue
		}
		out = append(out, b.m.relationshipViewLocked(r))
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
		b.relationshipCaveats(r, staleEnds[r.ID])
	}
	return out
}

// Relationship reads one relationship by id and records it as an input, with the
// same caveats RelationshipsInto gives (stale endpoint, regime).
func (b *DecisionBuilder) Relationship(id string) (Relationship, bool) {
	b.m.mu.RLock()
	r, ok := b.m.relationships[id]
	var out Relationship
	var staleEnds []string
	if ok {
		out = b.m.relationshipViewLocked(r)
		for _, end := range []string{r.From, r.To} {
			if p, ok := b.m.properties[end]; ok && p.Status == Stale {
				staleEnds = append(staleEnds, end)
			}
		}
	}
	b.m.mu.RUnlock()
	if !ok {
		b.caveats = append(b.caveats, fmt.Sprintf("relationship %s is not in the map", id))
		return Relationship{}, false
	}
	if !b.seenRel[id] {
		b.seenRel[id] = true
		b.rels = append(b.rels, out)
	}
	b.relationshipCaveats(out, staleEnds)
	return out, true
}

func (b *DecisionBuilder) relationshipCaveats(r Relationship, staleEnds []string) {
	for _, end := range staleEnds {
		b.caveats = append(b.caveats, fmt.Sprintf("relationship %s has a stale endpoint %s", r.ID, end))
	}
	if r.Regime {
		established := 0.0
		if r.Established != nil {
			established = *r.Established
		}
		b.caveats = append(b.caveats, fmt.Sprintf(
			"relationship %s: present differs from established (recent %.3f, established %.3f)",
			r.ID, r.Recent, established))
	}
}

// Cite records an earlier decision this one rests on. An empty id or one already
// cited is ignored; the order of first citation is kept.
func (b *DecisionBuilder) Cite(id string) {
	if id == "" || slices.Contains(b.cites, id) {
		return
	}
	b.cites = append(b.cites, id)
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

// Input records something else the decision read, under a name. v must be a value
// the caller will not change afterwards: a copy, a string, a number.
func (b *DecisionBuilder) Input(name string, v any) {
	if b.inputs == nil {
		b.inputs = map[string]any{}
	}
	b.inputs[name] = copyAny(v)
}

// copyAny copies the maps and slices nested in v; every other value is assigned.
func copyAny(v any) any {
	switch x := v.(type) {
	case map[string]any:
		c := make(map[string]any, len(x))
		for k, e := range x {
			c[k] = copyAny(e)
		}
		return c
	case []any:
		c := make([]any, len(x))
		for i, e := range x {
			c[i] = copyAny(e)
		}
		return c
	}
	return v
}

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
		Kind:              b.kind,
		Cites:             b.cites,
		Question:          b.question,
		Answer:            answer,
		PropertiesRead:    b.props,
		RelationshipsRead: b.rels,
		Assumptions:       assumptions,
		Excluded:          b.excluded,
		Inputs:            b.inputs,
		FactsRead:         b.facts,
		Rationale:         joinNonEmpty(parts, "; "),
		Caveats:           b.caveats,
	}
	rec, err := b.m.journal.RecordDecision(d)
	if errors.Is(err, ErrDuplicateDecision) {
		b.m.journal.append(Event{
			Revision: b.revision, At: b.at, Kind: EventDecisionRefused,
			Target: b.id, Actor: "agent", Detail: map[string]any{"reason": "duplicate id"},
		})
		return nil
	}
	return rec
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
