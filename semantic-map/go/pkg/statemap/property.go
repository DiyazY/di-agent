// Package statemap is the Semantic Map's state model: the properties a system
// exhibits, the relationships between them, and a record of how both came to hold
// their current values.
//
// # What this is for
//
// An agent that decides anything about its own system needs a model of that system
// which is current, inspectable, and answerable. Three requirements follow, and
// they are the reason this package exists rather than a static schema:
//
//   - The system's properties are not known in advance. A collector is upgraded, a
//     workload starts exercising a subsystem nobody instrumented, a device appears.
//     A property observed for the first time is admitted, not discarded.
//   - Properties stop being true. A metric that no longer arrives does not describe
//     the system any more, and a model that keeps reporting its last value is
//     asserting something it cannot support. Properties go stale and can be retired.
//   - Every answer must be attributable. "This is recommended because of these
//     properties, at these values, related this strongly, at this revision" is the
//     output, not a log line.
//
// # Shape
//
// The graph's vertices are Properties. An OBSERVED property is fed by telemetry and
// holds what the system is doing. A DERIVED property aggregates other properties and
// holds a summary the agent reasons over — which is how a framework's evaluation
// constructs live here without the graph being about the framework: a construct is
// a derived property whose members are the metrics that evidence it.
//
// Edges are Relationships between properties, each carrying where it came from:
// seeded from prior knowledge, learned from observation, or asserted by an operator.
// Provenance is part of the data because an agent that cannot say why it believes an
// edge cannot be audited.
//
// Every mutation advances a Revision. A decision records the revision it read, so
// the state that justified it can be reconstructed even after the system moves on.
package statemap

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// Kind distinguishes a property fed by telemetry from one computed from others.
type Kind string

const (
	// Observed properties are fed by a collector. Their value is a measurement.
	Observed Kind = "observed"

	// Derived properties aggregate their members. Their value is a summary, and it
	// is recomputed whenever a member changes rather than stored independently, so
	// a derived property can never disagree with the properties it summarises.
	Derived Kind = "derived"
)

// Status is where a property or relationship sits in its lifecycle.
//
// Retirement is a soft delete throughout. A retired entry keeps its history and
// stays visible to a query that asks for it, because "this property used to exist
// and stopped" is information an operator needs, and because a decision taken
// before the retirement must remain reconstructible.
type Status string

const (
	// Active: observed within its staleness window, or a relationship whose
	// endpoints are.
	Active Status = "active"

	// Stale: nothing has arrived for longer than the staleness window. The last
	// value is retained and marked, because a stale reading is evidence about the
	// past and silence is not evidence about the present.
	Stale Status = "stale"

	// Retired: withdrawn from reasoning. Set by an operator, or automatically for a
	// property whose silence has outlasted the retirement window.
	Retired Status = "retired"
)

// Property is one thing the system exhibits.
//
// The JSON names are explicit because this struct is a wire contract in two
// directions: it is what one agent sends another when asked for its state, and what a
// snapshot holds across a restart. Leaving the names to Go's field-name default made
// them a consequence of the source rather than an agreement, so a rename would have
// silently changed the format both ends parse.
type Property struct {
	ID   string `json:"id"`
	Kind Kind   `json:"kind"`

	// Unit and Range describe the value's meaning. Range is the interval the value
	// is expected to occupy; a value outside it is recorded but flagged, because
	// silently clipping an out-of-range reading hides a broken collector.
	Unit  string     `json:"unit,omitempty"`
	Range [2]float64 `json:"range"`

	// Value is the current estimate: the EMA of observations for an observed
	// property, the aggregate of members for a derived one.
	Value float64 `json:"value"`

	// Confidence is how much observation stands behind Value, in [0,1]: the
	// observation count against the convergence target. A property with no
	// observations reports 0, which is the difference between "idle" and "not yet
	// known", and a reader acting on a value has to be able to tell those apart.
	//
	// Note what it does NOT do, since the relationship field of the same name does
	// exactly that: it gates no blend. A property's Value is a pure EMA of what was
	// observed, so confidence describes the value without adjusting it. A
	// relationship's Effective() really does blend prior and evidence by confidence,
	// because a relationship has a prior worth blending and an unobserved property
	// has no second number to fall back on.
	Confidence float64 `json:"confidence"`

	NObservations int `json:"n_observations"`

	// Source names where an observed property's data comes from — a metric type, a
	// collector — so a reader can find out why a property exists.
	Source string `json:"source,omitempty"`

	// Members are the properties a derived property aggregates.
	Members []string `json:"members,omitempty"`

	Status        Status    `json:"status"`
	FirstObserved time.Time `json:"first_observed,omitzero"`
	LastObserved  time.Time `json:"last_observed,omitzero"`
	RetiredReason string    `json:"retired_reason,omitempty"`

	// OutOfRange counts readings outside Range. Non-zero means the collector and
	// the declared range disagree, which is a configuration fault worth surfacing
	// rather than absorbing.
	OutOfRange int `json:"out_of_range,omitempty"`
}

// Provenance says where a relationship's strength came from. It is data, not a
// comment: an agent asked to justify a decision has to distinguish a strength it
// measured from one it was told.
type Provenance string

const (
	// Seeded from prior knowledge, before this system was observed.
	Seeded Provenance = "seeded"

	// Learned from observations of both endpoints on this system.
	Learned Provenance = "learned"

	// Asserted by an operator, overriding what was seeded or learned.
	Asserted Provenance = "asserted"
)

// Relationship is a directed association between two properties.
type Relationship struct {
	ID   string `json:"id"` // stable identity, unique per (From, To, Label)
	From string `json:"from"`
	To   string `json:"to"`

	// Label distinguishes several relationships over the same endpoints. Two
	// mechanisms can relate the same pair in opposite directions, and collapsing
	// them would make the pair unable to represent a disagreement it is observing.
	Label string `json:"label,omitempty"`

	// Sign is +1 when From rising accompanies To rising, -1 for the converse.
	Sign int `json:"sign"`

	// Strength is the current estimate in [0,1], and Confidence how much of it
	// rests on observation of this system.
	Strength   float64 `json:"strength"`
	Prior      float64 `json:"prior"`
	Confidence float64 `json:"confidence"`

	NObservations int        `json:"n_observations"`
	Provenance    Provenance `json:"provenance"`

	Status        Status    `json:"status"`
	FirstObserved time.Time `json:"first_observed,omitzero"`
	LastObserved  time.Time `json:"last_observed,omitzero"`
	RetiredReason string    `json:"retired_reason,omitempty"`
	Note          string    `json:"note,omitempty"`
}

// Effective is the strength the agent should reason with: the prior until this
// system has been observed, the learned estimate once it has, and a continuous
// blend in between.
func (r *Relationship) Effective() float64 {
	c := clamp01(r.Confidence)
	return (1-c)*r.Prior + c*r.Strength
}

// Config bounds the lifecycle. Zero values fall back to the defaults below.
type Config struct {
	// Owner names the system this map models — one machine, since one agent runs per
	// machine and models what it can observe locally.
	//
	// It is not decoration. Once state crosses a node boundary, a property without an
	// owner cannot be attributed: a reader holding two properties with the same ID
	// cannot tell whether they describe two machines or one machine twice. Snapshots
	// carry the owner for the same reason, so a snapshot copied to another host is
	// refused rather than adopted as that host's own history.
	Owner string

	// StaleAfter is the silence that marks a property stale. It should exceed the
	// slowest collector's interval by enough margin that a missed sample is not
	// mistaken for a departed property.
	StaleAfter time.Duration

	// RetireAfter is the silence that retires a property automatically. Zero
	// disables automatic retirement, leaving it to an operator.
	RetireAfter time.Duration

	// ConvergenceObservations is how many observations bring confidence to 1.
	ConvergenceObservations int

	// Alpha is the EMA weight on each new observation.
	Alpha float64

	// AdmitUnknown controls whether an observation of an unknown property creates
	// it. On by default, because a model of a changing system that cannot represent
	// something new is a model of the system as it was when someone wrote it down.
	AdmitUnknown bool

	// Learn enables the paired estimator: relationships learn their strength from
	// simultaneous observations of both endpoints. Off leaves every relationship at
	// its seeded prior with confidence 0 — the honest report when nothing has been
	// learned, but it means the map never improves on what it was told.
	Learn bool

	// LearnConfig tunes the estimator. Ignored when Learn is off.
	LearnConfig LearnConfig
}

func (c Config) withDefaults() Config {
	if c.StaleAfter <= 0 {
		c.StaleAfter = 2 * time.Minute
	}
	if c.ConvergenceObservations <= 0 {
		c.ConvergenceObservations = 500
	}
	if c.Alpha <= 0 || c.Alpha > 1 {
		c.Alpha = 0.2
	}
	return c
}

// Map is the state model: properties, relationships, and the journal recording how
// they changed.
//
// Every read returns copies. Callers hold state that the map keeps mutating, and
// handing out pointers would let a decision's recorded inputs change after the
// decision was made — which would defeat the traceability the journal exists for.
type Map struct {
	mu  sync.RWMutex
	cfg Config

	// owner is the system this map models. Fixed at construction: a map that could be
	// reassigned to another system would carry the first one's history into the second.
	owner string

	properties    map[string]*Property
	relationships map[string]*Relationship

	// latest holds the most recent observation per property, windows the paired
	// observations per relationship. Together they are the estimator's memory: the map
	// learns strengths itself rather than recording an estimate computed elsewhere, so
	// there is one model of the system rather than two kept in step.
	latest   map[string]observation
	windows  map[string]*pairWindow
	learning bool
	learn    LearnConfig

	// seenEvents recognises an observation the map has already applied, so a replayed
	// archive or a retried post does not inflate the confidence behind a value.
	// seenOrder bounds it in insertion order — see admitEventLocked.
	seenEvents map[string]struct{}
	seenOrder  []string

	// revision advances on every mutation. A decision records the revision it read
	// so its inputs can be identified later even though the map has moved on.
	revision uint64

	journal *Journal

	// now is injectable so lifecycle transitions can be tested without sleeping.
	now func() time.Time
}

// New builds an empty map.
func New(cfg Config, journal *Journal) *Map {
	c := cfg.withDefaults()
	if journal == nil {
		journal = NewJournal(0)
	}
	return &Map{
		cfg:           c,
		owner:         c.Owner,
		properties:    make(map[string]*Property),
		relationships: make(map[string]*Relationship),
		latest:        make(map[string]observation),
		windows:       make(map[string]*pairWindow),
		learning:      c.Learn,
		learn:         c.LearnConfig.withDefaults(),
		journal:       journal,
		now:           time.Now,
	}
}

// SetClock overrides the map's clock. For tests.
func (m *Map) SetClock(f func() time.Time) {
	m.mu.Lock()
	m.now = f
	m.mu.Unlock()
}

// Owner returns the system this map models.
func (m *Map) Owner() string { return m.owner }

// Revision returns the current revision.
func (m *Map) Revision() uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.revision
}

// Journal exposes the change and decision record.
func (m *Map) Journal() *Journal { return m.journal }

// ── Property lifecycle ────────────────────────────────────────────────────────

// DeclareProperty creates or updates a property's definition — not its value.
// Idempotent on identical input, so a startup seed can run repeatedly.
func (m *Map) DeclareProperty(p Property) error {
	if p.ID == "" {
		return fmt.Errorf("property needs an id")
	}
	if p.Kind == "" {
		p.Kind = Observed
	}
	if p.Kind == Derived && len(p.Members) == 0 {
		return fmt.Errorf("derived property %q has no members: it would have nothing to summarise", p.ID)
	}
	if p.Kind == Observed && len(p.Members) > 0 {
		return fmt.Errorf("observed property %q lists members: a property is fed by telemetry or computed from others, not both", p.ID)
	}
	if p.Range[1] < p.Range[0] {
		return fmt.Errorf("property %q has inverted range [%v, %v]", p.ID, p.Range[0], p.Range[1])
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	existing, ok := m.properties[p.ID]
	if !ok {
		np := p
		if np.Status == "" {
			np.Status = Active
		}
		np.FirstObserved = time.Time{}
		np.LastObserved = time.Time{}
		m.properties[p.ID] = &np
		m.bump(EventPropertyDeclared, np.ID, "system", map[string]any{
			"kind": string(np.Kind), "unit": np.Unit, "source": np.Source,
			"members": np.Members,
		}, now)
		return nil
	}

	// Redeclaring must not silently discard accumulated evidence: definition and
	// state are different things, and a collector restart re-declares.
	changed := map[string]any{}
	if existing.Kind != p.Kind && p.Kind != "" {
		changed["kind"] = string(p.Kind)
		existing.Kind = p.Kind
	}
	if p.Unit != "" && existing.Unit != p.Unit {
		changed["unit"] = p.Unit
		existing.Unit = p.Unit
	}
	if p.Source != "" && existing.Source != p.Source {
		changed["source"] = p.Source
		existing.Source = p.Source
	}
	if p.Range != ([2]float64{}) && existing.Range != p.Range {
		changed["range"] = p.Range
		existing.Range = p.Range
	}
	if len(p.Members) > 0 && !sameStrings(existing.Members, p.Members) {
		changed["members"] = p.Members
		existing.Members = append([]string(nil), p.Members...)
	}
	if existing.Status == Retired {
		// Re-declaring a retired property revives it: the system is exhibiting it
		// again, which is exactly the event retirement was recording the absence of.
		changed["status"] = string(Active)
		existing.Status = Active
		existing.RetiredReason = ""
	}
	if len(changed) > 0 {
		m.bump(EventPropertyRedeclared, existing.ID, "system", changed, now)
	}
	return nil
}

// Observe records a measurement of a property.
//
// An unknown property is admitted when Config.AdmitUnknown is set, which is the
// mechanism by which the map follows a system that changes rather than a schema
// someone wrote down. The admission is journalled, so a property that appears in
// production is discoverable afterwards.
func (m *Map) Observe(id string, value float64, at time.Time) error {
	return m.ObserveEvent(id, value, at, "")
}

// ObserveEvent is Observe with the observation's event identity.
//
// The identity does two jobs. The paired estimator uses it to recognise a pair it has
// already folded in, and this method uses it to recognise the observation itself: an
// event already seen is a no-op. Both matter for the same reason — a caller replaying an
// archive, or a collector that re-sends after a timeout, would otherwise inflate
// confidence, which is a claim about how much observation stands behind a value. The
// value would barely move (the same number re-averaged is the same number) while the
// count behind it doubled, so the map would report certainty it had not earned.
//
// An observation with no event identity cannot be recognised and is always applied.
func (m *Map) ObserveEvent(id string, value float64, at time.Time, eventID string) error {
	if id == "" {
		return fmt.Errorf("observation needs a property id")
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return fmt.Errorf("property %q observed with non-finite value", id)
	}
	if at.IsZero() {
		at = m.now()
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Keyed by property as well as event, because an event identity is only unique
	// within the reading it names: one collector tick can carry several metrics, and
	// some collectors number them from a shared counter.
	if eventID != "" && !m.admitEventLocked(id+"@"+eventID) {
		return nil
	}

	p, ok := m.properties[id]
	if !ok {
		if !m.cfg.AdmitUnknown {
			return fmt.Errorf("property %q is not declared and admission is disabled", id)
		}
		p = &Property{
			ID: id, Kind: Observed, Status: Active,
			Range: [2]float64{0, 1}, Source: "admitted-on-observation",
		}
		m.properties[id] = p
		m.bump(EventPropertyAdmitted, id, "system", map[string]any{
			"value": value, "reason": "first observation of an undeclared property",
		}, at)
	}
	if p.Kind == Derived {
		return fmt.Errorf("property %q is derived: it is computed from %v, not observed directly",
			id, p.Members)
	}

	if p.Range != ([2]float64{}) && (value < p.Range[0] || value > p.Range[1]) {
		p.OutOfRange++
	}
	if p.NObservations == 0 {
		p.FirstObserved = at
		p.Value = value // the first observation IS the estimate; no prior to blend
	} else {
		p.Value = m.cfg.Alpha*value + (1-m.cfg.Alpha)*p.Value
	}
	p.NObservations++
	p.LastObserved = at
	p.Confidence = clamp01(float64(p.NObservations) / float64(m.cfg.ConvergenceObservations))
	if p.Status != Retired {
		p.Status = Active
	}
	m.revision++
	m.recomputeDerivedLocked(at)

	// Record for pairing, then learn from whatever this can pair with. Derived
	// properties are recomputed above first, so a relationship between two summaries
	// pairs two fresh values rather than one fresh and one from the previous tick.
	obs := observation{value: p.Value, at: at, eventID: eventID}
	m.latest[id] = obs
	m.learnFromObservationLocked(id, obs)
	for _, d := range m.properties {
		if d.Kind != Derived || d.Status != Active || d.NObservations == 0 {
			continue
		}
		if !containsString(d.Members, id) {
			continue
		}
		dobs := observation{value: d.Value, at: at, eventID: eventID + ":" + d.ID}
		m.latest[d.ID] = dobs
		m.learnFromObservationLocked(d.ID, dobs)
	}
	return nil
}

func containsString(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// RetireProperty withdraws a property from reasoning, keeping its record.
func (m *Map) RetireProperty(id, reason, actor string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	p, ok := m.properties[id]
	if !ok {
		return fmt.Errorf("property %q not found", id)
	}
	if p.Status == Retired {
		return nil
	}
	p.Status = Retired
	p.RetiredReason = reason
	now := m.now()

	// A relationship whose endpoint is gone cannot be evaluated. Retiring it with a
	// reason that points at the cause keeps the graph consistent and the audit
	// trail readable, rather than leaving edges that silently never fire.
	for _, r := range m.relationships {
		if r.Status == Retired {
			continue
		}
		if r.From == id || r.To == id {
			r.Status = Retired
			r.RetiredReason = fmt.Sprintf("endpoint %s retired: %s", id, reason)
			m.bump(EventRelationshipRetired, r.ID, actor, map[string]any{
				"reason": r.RetiredReason, "cascade_from": id,
			}, now)
		}
	}
	m.bump(EventPropertyRetired, id, actor, map[string]any{"reason": reason}, now)
	return nil
}

// Sweep applies time-based lifecycle transitions and returns what changed. It is
// idempotent, so a caller may run it on a timer or before a query without
// producing duplicate journal entries for the same transition.
func (m *Map) Sweep() (stale, retired []string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	for _, p := range m.properties {
		if p.Kind == Derived || p.Status == Retired || p.NObservations == 0 {
			continue
		}
		silence := now.Sub(p.LastObserved)
		switch {
		case m.cfg.RetireAfter > 0 && silence > m.cfg.RetireAfter:
			p.Status = Retired
			p.RetiredReason = fmt.Sprintf("no observation for %s", silence.Round(time.Second))
			retired = append(retired, p.ID)
			m.bump(EventPropertyRetired, p.ID, "sweep",
				map[string]any{"reason": p.RetiredReason, "silence_s": silence.Seconds()}, now)
		case silence > m.cfg.StaleAfter && p.Status != Stale:
			p.Status = Stale
			stale = append(stale, p.ID)
			m.bump(EventPropertyStale, p.ID, "sweep",
				map[string]any{"silence_s": silence.Seconds()}, now)
		}
	}
	sort.Strings(stale)
	sort.Strings(retired)
	return stale, retired
}

// recomputeDerivedLocked refreshes derived properties from their members. Caller
// holds the write lock.
//
// A derived property is recomputed rather than stored so it cannot drift from the
// properties it summarises. Retired and stale members are excluded: a summary must
// describe what the system is doing now, and its confidence is the mean of the
// members that actually contributed, so a construct backed by one live metric out
// of four does not claim the confidence of four.
func (m *Map) recomputeDerivedLocked(at time.Time) {
	for _, d := range m.properties {
		if d.Kind != Derived || d.Status == Retired {
			continue
		}
		var sum, conf float64
		var n, obs int
		for _, id := range d.Members {
			mem, ok := m.properties[id]
			if !ok || mem.Status != Active || mem.NObservations == 0 {
				continue
			}
			sum += mem.Value
			conf += mem.Confidence
			obs += mem.NObservations
			n++
		}
		if n == 0 {
			d.Confidence = 0
			d.NObservations = 0
			continue
		}
		d.Value = sum / float64(n)
		d.Confidence = conf / float64(n)
		d.NObservations = obs
		d.LastObserved = at
		if d.FirstObserved.IsZero() {
			d.FirstObserved = at
		}
	}
}

// ── Relationship lifecycle ────────────────────────────────────────────────────

// RelationshipID is the stable identity for an edge over (from, to, label).
func RelationshipID(from, to, label string) string {
	if label == "" {
		return from + "->" + to
	}
	return from + "->" + to + ":" + label
}

// DeclareRelationship creates or updates a relationship's definition.
func (m *Map) DeclareRelationship(r Relationship) error {
	if r.From == "" || r.To == "" {
		return fmt.Errorf("relationship needs both endpoints")
	}
	if r.From == r.To {
		return fmt.Errorf("relationship from %q to itself carries no information", r.From)
	}
	if r.Sign != 1 && r.Sign != -1 {
		return fmt.Errorf("relationship %s->%s has sign %d, want +1 or -1", r.From, r.To, r.Sign)
	}
	if r.Provenance == "" {
		r.Provenance = Seeded
	}
	r.ID = RelationshipID(r.From, r.To, r.Label)

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, id := range []string{r.From, r.To} {
		if _, ok := m.properties[id]; !ok {
			return fmt.Errorf("relationship %s references undeclared property %q", r.ID, id)
		}
	}

	now := m.now()
	if existing, ok := m.relationships[r.ID]; ok {
		if existing.Sign != r.Sign {
			// A direction reversal is a different claim, not an update. Rejecting it
			// keeps the record honest: the old claim must be retired and the new one
			// added, so the history shows that somebody changed their mind.
			return fmt.Errorf("relationship %s already asserts sign %+d; retire it before asserting %+d",
				r.ID, existing.Sign, r.Sign)
		}
		detail := map[string]any{}
		// A re-declaration carrying a different prior updates it, unless an operator has
		// asserted one. Ignoring the new value silently kept a stale number whenever a
		// calibration was reloaded — the declaration looked accepted and changed
		// nothing. An assertion outranks a seed, so it is not overwritten.
		if r.Prior != 0 && r.Prior != existing.Prior && existing.Provenance != Asserted {
			detail["prior_old"] = existing.Prior
			detail["prior_new"] = r.Prior
			existing.Prior = r.Prior
			if existing.NObservations == 0 {
				// Nothing observed yet, so the strength IS the prior; leaving the old one
				// would make Effective() blend toward a number nobody supplied.
				existing.Strength = r.Prior
			}
		}
		if r.Note != "" && r.Note != existing.Note {
			existing.Note = r.Note
		}
		if existing.Status == Retired && r.Status != Retired {
			existing.Status = Active
			existing.RetiredReason = ""
			detail["revived"] = true
		}
		if len(detail) > 0 {
			m.bump(EventRelationshipDeclared, r.ID, "system", detail, now)
		}
		return nil
	}

	nr := r
	if nr.Status == "" {
		nr.Status = Active
	}
	if nr.Strength == 0 {
		nr.Strength = nr.Prior
	}
	m.relationships[nr.ID] = &nr
	m.bump(EventRelationshipDeclared, nr.ID, "system", map[string]any{
		"from": nr.From, "to": nr.To, "label": nr.Label, "sign": nr.Sign,
		"prior": nr.Prior, "provenance": string(nr.Provenance),
	}, now)
	return nil
}

// ObserveRelationship records evidence about a relationship's strength: an
// estimate in [0,1] computed by whatever the caller uses to relate the endpoints.
//
// The map does not compute the estimate itself. Which estimator is appropriate
// depends on what the properties mean, and hard-coding one here would make the
// state model responsible for a modelling choice that belongs to its caller.
func (m *Map) ObserveRelationship(id string, strength float64, at time.Time) error {
	return m.ObserveRelationshipEvent(id, strength, at, "")
}

// ObserveRelationshipEvent is ObserveRelationship with the observation's event
// identity, so a replay of the same evidence is a no-op rather than a second vote.
func (m *Map) ObserveRelationshipEvent(id string, strength float64, at time.Time, eventID string) error {
	if math.IsNaN(strength) || math.IsInf(strength, 0) {
		return fmt.Errorf("relationship %q observed with non-finite strength", id)
	}
	if at.IsZero() {
		at = m.now()
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if eventID != "" && !m.admitEventLocked(id+"@"+eventID) {
		return nil
	}

	r, ok := m.relationships[id]
	if !ok {
		return fmt.Errorf("relationship %q not found", id)
	}
	if r.Status == Retired {
		return fmt.Errorf("relationship %q is retired", id)
	}
	s := clamp01(strength)
	if r.NObservations == 0 {
		r.FirstObserved = at
		r.Strength = s
	} else {
		r.Strength = m.cfg.Alpha*s + (1-m.cfg.Alpha)*r.Strength
	}
	r.NObservations++
	r.LastObserved = at
	r.Confidence = clamp01(float64(r.NObservations) / float64(m.cfg.ConvergenceObservations))
	if r.Provenance == Seeded {
		r.Provenance = Learned
	}
	r.Status = Active
	m.revision++
	return nil
}

// AssertRelationshipStrength is the operator path: it overrides the strength and
// records who did it.
//
// It writes the prior rather than the learned estimate, and says so in the
// journal. Writing the estimate would erase the distinction between what was
// observed and what was asserted — the distinction an audit exists to preserve.
func (m *Map) AssertRelationshipStrength(id string, strength float64, actor, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.relationships[id]
	if !ok {
		return fmt.Errorf("relationship %q not found", id)
	}
	old := r.Prior
	r.Prior = clamp01(strength)
	r.Provenance = Asserted
	m.bump(EventRelationshipAsserted, id, actor, map[string]any{
		"prior_old": old, "prior_new": r.Prior, "reason": reason,
		"confidence_at_assertion": r.Confidence,
		"effective_after":         r.Effective(),
		"note": "an assertion moves the prior; at high confidence the effective " +
			"strength barely moves, which is the blend behaving as designed",
	}, m.now())
	return nil
}

// RecordOperatorIntent journals one operator action that spanned several
// relationships, naming the intent, the actor, and what it touched.
//
// It records nothing about the map's contents — the assertions it produced did that
// individually. What it adds is the act: which of them were one decision, whose, and
// on what stated basis. Without it a coordinated adjustment is indistinguishable
// afterwards from unrelated changes that happened to land together.
func (m *Map) RecordOperatorIntent(intent, actor string, targets []string) {
	if actor == "" {
		actor = "operator"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bump(EventOperatorIntent, "", actor, map[string]any{
		"intent":  intent,
		"targets": append([]string(nil), targets...),
	}, m.now())
}

// ResetRelationship discards what was learned about a relationship on this system,
// returning it to its prior with no observations behind it.
//
// The operation an operator wants when a relationship learned something from a period
// they now know to have been unrepresentative — a load test, a broken collector, a
// misconfigured neighbour. It is deliberately not a delete: the relationship is still
// asserted to exist, and the prior is still the best available estimate of its
// strength. What goes is the evidence, and with it the confidence that rested on it.
//
// The reset is journalled with what it discarded, because "this edge has no
// observations" and "this edge's observations were thrown away at 14:20 by an operator
// who said the collector was broken" are different states of knowledge, and only one of
// them is reconstructible from a count of zero.
func (m *Map) ResetRelationship(id, actor, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.relationships[id]
	if !ok {
		return fmt.Errorf("relationship %q not found", id)
	}
	discarded := r.NObservations
	oldStrength, oldConfidence := r.Strength, r.Confidence

	r.Strength = 0
	r.Confidence = 0
	r.NObservations = 0
	r.FirstObserved = time.Time{}
	r.LastObserved = time.Time{}
	if r.Provenance == Learned {
		// Back to seeded: the strength in force is the prior again, and provenance has to
		// say so or the next reader will treat an unobserved edge as a measured one.
		r.Provenance = Seeded
	}
	// The estimator's window goes too. Leaving it would let the next single pair
	// complete a window built from the very observations that were just discarded.
	delete(m.windows, id)

	m.bump(EventRelationshipAsserted, id, actor, map[string]any{
		"reset": true, "reason": reason,
		"discarded_observations": discarded,
		"strength_before":        oldStrength,
		"confidence_before":      oldConfidence,
		"prior":                  r.Prior,
		"note": "evidence discarded; the prior stands as the estimate and the pair " +
			"window was cleared so nothing from before the reset can complete it",
	}, m.now())
	return nil
}

// RetireRelationship withdraws a relationship from reasoning, keeping its record.
func (m *Map) RetireRelationship(id, reason, actor string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.relationships[id]
	if !ok {
		return fmt.Errorf("relationship %q not found", id)
	}
	if r.Status == Retired {
		return nil
	}
	r.Status = Retired
	r.RetiredReason = reason
	m.bump(EventRelationshipRetired, id, actor, map[string]any{"reason": reason}, m.now())
	return nil
}

// admitEventLocked reports whether an event identity is new, recording it if so.
// Caller holds the write lock.
//
// The set is bounded and evicted in insertion order. An unbounded one would grow with
// every sample for the lifetime of the process, which on an edge node is the wrong
// trade: what matters is recognising the duplicates that arrive close together — a
// retried post, a replayed batch — not remembering last week. Eviction is stated here
// rather than hidden because it means idempotency is guaranteed over a window, not
// forever, and a caller replaying an archive larger than the window will re-apply its
// oldest observations.
func (m *Map) admitEventLocked(key string) bool {
	if m.seenEvents == nil {
		m.seenEvents = make(map[string]struct{}, seenEventCapacity)
	}
	if _, dup := m.seenEvents[key]; dup {
		return false
	}
	if len(m.seenOrder) >= seenEventCapacity {
		delete(m.seenEvents, m.seenOrder[0])
		m.seenOrder = m.seenOrder[1:]
	}
	m.seenEvents[key] = struct{}{}
	m.seenOrder = append(m.seenOrder, key)
	return true
}

// seenEventCapacity is how many recent observations the map can recognise as repeats.
// Sized for a few thousand samples: comfortably more than any retry or batch replay a
// collector produces in one burst, and small enough to be invisible in an edge node's
// memory budget.
const seenEventCapacity = 8192

// bump advances the revision and appends a journal entry. Caller holds the lock.
func (m *Map) bump(kind EventKind, target, actor string, detail map[string]any, at time.Time) {
	m.revision++
	m.journal.append(Event{
		Revision: m.revision,
		At:       at,
		Kind:     kind,
		Target:   target,
		Actor:    actor,
		Detail:   detail,
	})
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

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// String renders a property for a rationale line.
func (p *Property) String() string {
	return fmt.Sprintf("%s=%.4f(c=%.2f,%s)", p.ID, p.Value, p.Confidence, p.Status)
}

// String renders a relationship for a rationale line.
func (r *Relationship) String() string {
	sign := "+"
	if r.Sign < 0 {
		sign = "-"
	}
	label := r.Label
	if label != "" {
		label = "[" + label + "]"
	}
	return fmt.Sprintf("%s%s->%s(%s%.3f,c=%.2f,%s)",
		r.From, label, r.To, sign, r.Effective(), r.Confidence, r.Provenance)
}

// describe is used by query rendering to keep property listings stable.
func describeIDs(ids []string) string {
	s := append([]string(nil), ids...)
	sort.Strings(s)
	return strings.Join(s, ",")
}
