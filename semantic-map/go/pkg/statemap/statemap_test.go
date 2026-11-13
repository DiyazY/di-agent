package statemap

import (
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// clock is a controllable time source: lifecycle transitions depend on elapsed
// time, and a test that slept for them would be slow and flaky.
//
// t is the wall clock and m the process's monotonic reading. advance moves both, as
// time passing does; stepWall moves only the wall clock, as an NTP step does.
type clock struct {
	t time.Time
	m time.Duration
}

func (c *clock) now() time.Time           { return c.t }
func (c *clock) mono() time.Duration      { return c.m }
func (c *clock) advance(d time.Duration)  { c.t = c.t.Add(d); c.m += d }
func (c *clock) stepWall(d time.Duration) { c.t = c.t.Add(d) }

// near compares floats that arrive via EMA arithmetic, where an exact literal is
// not reachable: 0.2*0.8 + 0.8*0.8 is 0.8000000000000001.
func near(got, want float64) bool {
	d := got - want
	return d < 1e-9 && d > -1e-9
}

func newTestMap(t *testing.T, cfg Config) (*Map, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), m: time.Second}
	m := New(cfg, NewJournal(0))
	m.SetClock(c.now)
	m.SetMonotonic(c.mono)
	return m, c
}

// ── The map represents the current state of a system ──────────────────────────

func TestObservedPropertyHoldsCurrentValueAndSaysHowSureItIs(t *testing.T) {
	m, c := newTestMap(t, Config{ConvergenceObservations: 10, Alpha: 0.5})

	if err := m.DeclareProperty(Property{
		ID: "cpu_utilization", Kind: Observed, Unit: "fraction",
		Range: [2]float64{0, 1}, Source: "node collector",
	}); err != nil {
		t.Fatal(err)
	}

	// Before anything is observed the map must not pretend to know a value.
	p, _ := m.Property("cpu_utilization")
	if p.Confidence != 0 || p.NObservations != 0 {
		t.Errorf("undisturbed property reports confidence %.3f from %d observations; "+
			"an unobserved property has to report that it is unobserved",
			p.Confidence, p.NObservations)
	}

	// The first observation is the estimate: there is no prior to blend against.
	if err := m.Observe("cpu_utilization", 0.4, c.now()); err != nil {
		t.Fatal(err)
	}
	if p, _ = m.Property("cpu_utilization"); p.Value != 0.4 {
		t.Errorf("first observation gave value %.4f, want 0.4 — blending the first "+
			"reading against a placeholder invents a measurement", p.Value)
	}

	for i := 0; i < 9; i++ {
		c.advance(time.Second)
		if err := m.Observe("cpu_utilization", 0.4, c.now()); err != nil {
			t.Fatal(err)
		}
	}
	p, _ = m.Property("cpu_utilization")
	if p.Confidence != 1.0 {
		t.Errorf("confidence %.3f after reaching the convergence count; want 1.0", p.Confidence)
	}
	if p.NObservations != 10 {
		t.Errorf("n_observations %d, want 10", p.NObservations)
	}
}

func TestDerivedPropertySummarisesLiveMembersOnly(t *testing.T) {
	m, c := newTestMap(t, Config{ConvergenceObservations: 4, StaleAfter: 30 * time.Second})

	for _, id := range []string{"cpu", "mem"} {
		if err := m.DeclareProperty(Property{ID: id, Range: [2]float64{0, 1}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.DeclareProperty(Property{
		ID: "resource_use", Kind: Derived, Members: []string{"cpu", "mem"},
	}); err != nil {
		t.Fatal(err)
	}

	if err := m.Observe("cpu", 0.8, c.now()); err != nil {
		t.Fatal(err)
	}
	if err := m.Observe("mem", 0.2, c.now()); err != nil {
		t.Fatal(err)
	}
	d, _ := m.Property("resource_use")
	if d.Value != 0.5 {
		t.Errorf("derived value %.4f, want the mean 0.5", d.Value)
	}

	// A member that stops reporting must stop contributing. A summary that keeps
	// averaging in a departed member describes a system that no longer exists.
	c.advance(time.Minute)
	if _, _ = m.Sweep(); true {
		if err := m.Observe("cpu", 0.8, c.now()); err != nil {
			t.Fatal(err)
		}
	}
	d, _ = m.Property("resource_use")
	if !near(d.Value, 0.8) {
		t.Errorf("derived value %.4f after mem went stale; want 0.8 from cpu alone", d.Value)
	}
	mem, _ := m.Property("mem")
	if mem.Status != Stale {
		t.Errorf("mem status %s after a minute of silence; want stale", mem.Status)
	}
	d, _ = m.Property("resource_use")
	if d.Status != Active {
		t.Errorf("derived is %s while cpu is still active; want active", d.Status)
	}
}

// ── create / update / remove, at runtime ──────────────────────────────────────

func TestUnknownPropertyIsAdmittedOnFirstObservation(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})

	// This is the case that matters for a system nobody fully described in advance:
	// a collector starts reporting something new and the map has to represent it.
	if err := m.Observe("gpu_utilization", 0.3, c.now()); err != nil {
		t.Fatalf("observation of an undeclared property was rejected: %v", err)
	}
	p, ok := m.Property("gpu_utilization")
	if !ok {
		t.Fatal("property was not created")
	}
	if p.Kind != Observed || p.Status != Active || p.Value != 0.3 {
		t.Errorf("admitted property is %+v; want an active observed property valued 0.3", p)
	}

	// The admission has to be discoverable afterwards, or nobody can find out why
	// the map contains something they never declared.
	var found bool
	for _, e := range m.Journal().Events(0, 0) {
		if e.Kind == EventPropertyAdmitted && e.Target == "gpu_utilization" {
			found = true
		}
	}
	if !found {
		t.Error("no journal entry records the admission")
	}
}

func TestAdmissionCanBeRefused(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: false})
	if err := m.Observe("surprise", 0.5, c.now()); err == nil {
		t.Fatal("an undeclared property was admitted with admission disabled")
	}
}

func TestSilenceMakesAPropertyStaleThenRetiresIt(t *testing.T) {
	m, c := newTestMap(t, Config{
		StaleAfter: 30 * time.Second, RetireAfter: 2 * time.Minute,
	})
	if err := m.DeclareProperty(Property{ID: "disk_io"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Observe("disk_io", 0.1, c.now()); err != nil {
		t.Fatal(err)
	}

	c.advance(45 * time.Second)
	stale, retired := m.Sweep()
	if len(stale) != 1 || stale[0] != "disk_io" || len(retired) != 0 {
		t.Fatalf("after 45s of silence: stale=%v retired=%v; want disk_io stale only", stale, retired)
	}

	c.advance(3 * time.Minute)
	_, retired = m.Sweep()
	if len(retired) != 1 || retired[0] != "disk_io" {
		t.Fatalf("after 3m more silence: retired=%v; want disk_io", retired)
	}
	p, _ := m.Property("disk_io")
	if p.Status != Retired {
		t.Errorf("status %s, want retired", p.Status)
	}
	if p.Value != 0.1 {
		t.Errorf("retirement discarded the last value (%.3f); a retired property's "+
			"history is what makes an earlier decision reconstructible", p.Value)
	}

	// Sweeping again must not re-report the same transition.
	stale2, retired2 := m.Sweep()
	if len(stale2) != 0 || len(retired2) != 0 {
		t.Errorf("second sweep re-reported transitions: stale=%v retired=%v", stale2, retired2)
	}

	// If the system exhibits it again, the map takes it back.
	if err := m.DeclareProperty(Property{ID: "disk_io"}); err != nil {
		t.Fatal(err)
	}
	if p, _ = m.Property("disk_io"); p.Status != Active {
		t.Errorf("re-declared property has status %s; a property the system exhibits "+
			"again is part of the system again", p.Status)
	}
}

func TestRetiringAPropertyRetiresItsRelationships(t *testing.T) {
	m, _ := newTestMap(t, Config{})
	for _, id := range []string{"a", "b"} {
		if err := m.DeclareProperty(Property{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.DeclareRelationship(Relationship{From: "a", To: "b", Sign: 1}); err != nil {
		t.Fatal(err)
	}

	if err := m.RetireProperty("a", "device removed", "operator:ada"); err != nil {
		t.Fatal(err)
	}
	r, _ := m.Relationship(RelationshipID("a", "b", ""))
	if r.Status != Retired {
		t.Errorf("relationship status %s after its source property was retired; a "+
			"relationship to a property that no longer exists cannot be evaluated", r.Status)
	}
	if !strings.Contains(r.RetiredReason, "endpoint a retired") {
		t.Errorf("retirement reason %q does not point at the cause", r.RetiredReason)
	}
}

func TestRelationshipRejectsSelfLoopAndDirectionReversal(t *testing.T) {
	m, _ := newTestMap(t, Config{})
	for _, id := range []string{"a", "b"} {
		if err := m.DeclareProperty(Property{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.DeclareRelationship(Relationship{From: "a", To: "a", Sign: 1}); err == nil {
		t.Error("a self-loop was accepted; relating a property to itself carries no information")
	}
	if err := m.DeclareRelationship(Relationship{From: "a", To: "b", Sign: 1}); err != nil {
		t.Fatal(err)
	}
	if err := m.DeclareRelationship(Relationship{From: "a", To: "b", Sign: -1}); err == nil {
		t.Error("a sign reversal was accepted as an update; reversing a claim has to " +
			"retire the old one so the history shows the change of mind")
	}
	// Two mechanisms over the same endpoints are legitimate when labelled.
	if err := m.DeclareRelationship(Relationship{
		From: "a", To: "b", Sign: -1, Label: "suppression",
	}); err != nil {
		t.Errorf("a labelled opposing relationship was rejected: %v", err)
	}
}

func TestEffectiveIsUnknownUntilObservedThenReportsTheRecentEstimate(t *testing.T) {
	m, c := newTestMap(t, Config{ConvergenceObservations: 10, Alpha: 1.0})
	for _, id := range []string{"a", "b"} {
		if err := m.DeclareProperty(Property{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.DeclareRelationship(Relationship{
		From: "a", To: "b", Sign: 1, Provenance: Seeded,
	}); err != nil {
		t.Fatal(err)
	}
	id := RelationshipID("a", "b", "")

	// A declared relationship asserts that two properties relate and in which
	// direction. It says nothing about how strongly, and the map must not invent a
	// figure — which is what the seeded prior it replaced did, at weight
	// (1 - confidence), i.e. hardest when the agent knew least.
	r, _ := m.Relationship(id)
	if _, known := r.Effective(); known {
		v, _ := r.Effective()
		t.Errorf("a relationship nothing has been observed about reports effective "+
			"%.3f; declaring that two properties relate is not a measurement of how "+
			"strongly", v)
	}
	if r.Basis() != "unknown" {
		t.Errorf("basis %q before any observation, want unknown", r.Basis())
	}

	for i := 0; i < 10; i++ {
		c.advance(time.Second)
		if err := m.ObserveRelationship(id, 0.2, c.now()); err != nil {
			t.Fatal(err)
		}
	}
	r, _ = m.Relationship(id)
	v, known := r.Effective()
	if !known {
		t.Fatal("still unknown after ten observations")
	}
	if v != 0.2 {
		t.Errorf("effective %.3f, want the observed 0.2", v)
	}
	if r.Basis() != "recent" {
		t.Errorf("basis %q, want recent — nothing has established a long-run value "+
			"and no operator has asserted one", r.Basis())
	}
	if r.Provenance != Learned {
		t.Errorf("provenance %s after observation; a strength that came from this "+
			"system must not still claim to be seeded", r.Provenance)
	}
}

func TestAssertionOutranksObservationAndDoesNotDecay(t *testing.T) {
	m, c := newTestMap(t, Config{ConvergenceObservations: 4, Alpha: 1.0})
	for _, id := range []string{"a", "b"} {
		if err := m.DeclareProperty(Property{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.DeclareRelationship(Relationship{From: "a", To: "b", Sign: 1}); err != nil {
		t.Fatal(err)
	}
	id := RelationshipID("a", "b", "")
	// Drive it to full confidence first. This is the case that used to fail silently:
	// an operator correcting a well-observed relationship wrote a prior that reached
	// the decision scaled by (1 - confidence), so at confidence 1.0 the correction
	// changed the effective strength by exactly nothing.
	for i := 0; i < 4; i++ {
		c.advance(time.Second)
		if err := m.ObserveRelationship(id, 0.9, c.now()); err != nil {
			t.Fatal(err)
		}
	}
	r, _ := m.Relationship(id)
	if r.Confidence != 1.0 {
		t.Fatalf("precondition: confidence %.3f, want 1.0", r.Confidence)
	}

	if err := m.AssertRelationshipStrength(id, 0.1, "operator:ada", "measured by hand"); err != nil {
		t.Fatal(err)
	}
	r, _ = m.Relationship(id)

	v, known := r.Effective()
	if !known {
		t.Fatal("effective unknown after an assertion")
	}
	if v != 0.1 {
		t.Errorf("effective %.3f after asserting 0.1 at full confidence; an operator "+
			"correction must take effect in full, or the better the agent knows its "+
			"machine the less an operator can steer it", v)
	}
	if r.Basis() != "asserted" {
		t.Errorf("basis %q, want asserted", r.Basis())
	}
	if r.Assertion == nil || *r.Assertion != 0.1 {
		t.Errorf("assertion field not set to 0.1")
	}
	if r.Recent != 0.9 {
		t.Errorf("assertion overwrote the observed strength (%.3f); the distinction "+
			"between what was observed and what was asserted is what an audit needs",
			r.Recent)
	}
	if r.Provenance != Asserted {
		t.Errorf("provenance %s, want asserted", r.Provenance)
	}
}
func TestStateQueryAnswersWhatTheSystemIsDoing(t *testing.T) {
	m, c := newTestMap(t, Config{ConvergenceObservations: 2, StaleAfter: time.Minute})
	seed(t, m, c)

	// The default query is the current system: active and stale, never retired.
	// Live: cpu, mem, pressure, unrelated, resource_use. Retired: gone.
	all := m.State(Query{})
	if len(all.Properties) != 5 {
		t.Fatalf("default query returned %d properties, want the 5 live ones", len(all.Properties))
	}
	for _, p := range all.Properties {
		if p.Status == Retired {
			t.Errorf("default query returned retired property %s; withdrawn state must "+
				"not leak into an agent's answers", p.ID)
		}
	}
	if all.Counts.PropertiesRetired != 1 {
		t.Errorf("census reports %d retired, want 1 — a filter must not hide the "+
			"existence of what it excluded", all.Counts.PropertiesRetired)
	}

	if got := m.State(Query{Kinds: []Kind{Derived}}); len(got.Properties) != 1 {
		t.Errorf("kind filter returned %d derived properties, want 1", len(got.Properties))
	}
	if got := m.State(Query{Statuses: []Status{Retired}}); len(got.Properties) != 1 {
		t.Errorf("explicit retired query returned %d, want 1", len(got.Properties))
	}
	if got := m.State(Query{MinConfidence: 0.9}); len(got.Properties) == len(all.Properties) {
		t.Error("confidence filter excluded nothing despite unobserved properties present")
	}

	// The neighbourhood query is what a decision about one property consults.
	n := m.State(Query{RelatedTo: "pressure"})
	ids := map[string]bool{}
	for _, p := range n.Properties {
		ids[p.ID] = true
	}
	if !ids["pressure"] || !ids["resource_use"] {
		t.Errorf("neighbourhood of pressure = %v; want it to include resource_use, "+
			"which is what relates to it", ids)
	}
	if ids["unrelated"] {
		t.Error("neighbourhood included a property nothing relates to")
	}
	// Relationships come back only when both endpoints are in the selection, so a
	// view is never a graph with dangling edges.
	for _, r := range n.Relationships {
		if !ids[r.From] || !ids[r.To] {
			t.Errorf("relationship %s has an endpoint outside the view", r.ID)
		}
	}
}

func TestExplainDescribesAPropertyWithoutAClient(t *testing.T) {
	m, c := newTestMap(t, Config{ConvergenceObservations: 2, StaleAfter: time.Minute})
	seed(t, m, c)

	out, err := m.Explain("resource_use")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"derived", "aggregates", "cpu", "mem", "influences"} {
		if !strings.Contains(out, want) {
			t.Errorf("explanation of a derived property omits %q:\n%s", want, out)
		}
	}

	if out, err = m.Explain("unrelated"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "isolated") {
		t.Errorf("explanation of an unrelated property does not say it is isolated:\n%s", out)
	}
	if _, err = m.Explain("nope"); err == nil {
		t.Error("explaining an absent property did not error")
	}
}

// ── Traceable ─────────────────────────────────────────────────────────────────

func TestDecisionRecordsTheStateThatJustifiedIt(t *testing.T) {
	m, c := newTestMap(t, Config{ConvergenceObservations: 2, StaleAfter: time.Minute})
	seed(t, m, c)

	revAtDecision := m.Revision()
	d := func() *Decision {
		b := m.Decide("dec-1", "estimate", "how pressured is this system?")
		p, _ := b.Property("pressure")
		rels := b.RelationshipsInto("pressure")
		b.Note("read pressure=%.3f from %d incoming relationships", p.Value, len(rels))
		return b.Commit(map[string]any{"pressure": p.Value})
	}()

	if d.Revision != revAtDecision {
		t.Errorf("decision pinned revision %d, map was at %d", d.Revision, revAtDecision)
	}
	if len(d.PropertiesRead) == 0 || len(d.RelationshipsRead) == 0 {
		t.Fatalf("decision recorded %d properties and %d relationships; a record that "+
			"omits its inputs cannot justify its answer",
			len(d.PropertiesRead), len(d.RelationshipsRead))
	}
	if !strings.Contains(d.Rationale, "pressure") {
		t.Errorf("rationale does not name the property read: %q", d.Rationale)
	}

	// The recorded inputs must be a snapshot. If they tracked live state, the
	// record would silently become a description of a later system.
	before := d.PropertiesRead[0].Value
	for i := 0; i < 20; i++ {
		c.advance(time.Second)
		if err := m.Observe("cpu", 0.99, c.now()); err != nil {
			t.Fatal(err)
		}
	}
	again, ok := m.Journal().Decision("dec-1")
	if !ok {
		t.Fatal("decision not retrievable")
	}
	if again.PropertiesRead[0].Value != before {
		t.Errorf("recorded input changed after the fact: %.4f -> %.4f",
			before, again.PropertiesRead[0].Value)
	}
	if again.Revision >= m.Revision() {
		t.Errorf("decision revision %d did not fall behind the map's %d after further "+
			"observations", again.Revision, m.Revision())
	}
}

func TestDecisionReportsWhyItMightBeWrong(t *testing.T) {
	m, c := newTestMap(t, Config{ConvergenceObservations: 100, StaleAfter: 10 * time.Second})
	seed(t, m, c)

	// Let a property go stale, then decide using it.
	c.advance(time.Minute)
	m.Sweep()

	b := m.Decide("dec-2", "estimate", "cost now?")
	b.Property("cpu")       // stale by now
	b.Property("unrelated") // never observed
	b.Property("ghost")     // absent
	d := b.Commit(map[string]any{"cost": 0.0})

	joined := strings.Join(d.Caveats, " | ")
	for _, want := range []string{"stale", "no observations", "not in the map"} {
		if !strings.Contains(joined, want) {
			t.Errorf("caveats omit %q: %v", want, d.Caveats)
		}
	}
}

func TestJournalReportsWhatItDropped(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	// A small journal so the bound is reached quickly.
	m.journal = NewJournal(5)

	for i := 0; i < 20; i++ {
		c.advance(time.Second)
		if err := m.Observe("p"+string(rune('a'+i)), 0.5, c.now()); err != nil {
			t.Fatal(err)
		}
	}
	held, dropped, oldest := m.Journal().Stats()
	if held > 5 {
		t.Errorf("journal holds %d entries, capacity 5", held)
	}
	if dropped == 0 {
		t.Error("journal dropped entries without reporting it; a reader has to be " +
			"able to tell a partial record from a complete one")
	}
	if oldest == 0 {
		t.Error("journal does not report its oldest revision, so a caller cannot tell " +
			"which window it is seeing")
	}
}

// seed builds a small system: two observed properties, a derived summary, an
// isolated property, a retired one, and a relationship.
func seed(t *testing.T, m *Map, c *clock) {
	t.Helper()
	for _, p := range []Property{
		{ID: "cpu", Range: [2]float64{0, 1}, Source: "collector"},
		{ID: "mem", Range: [2]float64{0, 1}, Source: "collector"},
		{ID: "pressure", Range: [2]float64{0, 1}, Source: "collector"},
		{ID: "unrelated", Range: [2]float64{0, 1}},
		{ID: "gone", Range: [2]float64{0, 1}},
		{ID: "resource_use", Kind: Derived, Members: []string{"cpu", "mem"}},
	} {
		if err := m.DeclareProperty(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.DeclareRelationship(Relationship{
		From: "resource_use", To: "pressure", Sign: 1, Provenance: Seeded,
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"cpu", "mem", "pressure"} {
		if err := m.Record(Observation{ID: id, Value: 0.4, At: c.now(), Subject: subjectOf(id)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.RetireProperty("gone", "device removed", "operator:test"); err != nil {
		t.Fatal(err)
	}
}

// TestMapLearnsRelationshipStrengthItself is the point of folding the estimator in:
// the map improves on what it was told without a second model computing the estimate
// and copying it over. Two representations of the same relations, kept in step by a
// propagation call, can disagree — and a reader then has to ask which one an answer
// came from.
func TestMapLearnsRelationshipStrengthItself(t *testing.T) {
	m, c := newTestMap(t, Config{
		ConvergenceObservations: 20,
		Alpha:                   0.5,
		Learn:                   true,
		LearnConfig:             LearnConfig{PairWindowSeconds: 15, MinSupport: 8, Window: 40},
	})
	for _, id := range []string{"resource", "pressure"} {
		if err := m.DeclareProperty(Property{ID: id, Range: [2]float64{0, 1}}); err != nil {
			t.Fatal(err)
		}
	}
	// A conflict pair: two mechanisms over the same endpoints, opposite signs.
	if err := m.DeclareRelationship(Relationship{
		From: "resource", To: "pressure", Label: "raises", Sign: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.DeclareRelationship(Relationship{
		From: "resource", To: "pressure", Label: "relieves", Sign: -1,
	}); err != nil {
		t.Fatal(err)
	}

	// Pressure rises with resource: evidence for "raises", against "relieves".
	for i := 0; i < 40; i++ {
		c.advance(5 * time.Second)
		x := float64(i%10) / 10.0
		if err := m.ObserveEvent("resource", x, c.now(), "r"+itoa(i)); err != nil {
			t.Fatal(err)
		}
		c.advance(2 * time.Second)
		if err := m.ObserveEvent("pressure", 0.9*x+0.02, c.now(), "p"+itoa(i)); err != nil {
			t.Fatal(err)
		}
	}

	raises, _ := m.Relationship(RelationshipID("resource", "pressure", "raises"))
	relieves, _ := m.Relationship(RelationshipID("resource", "pressure", "relieves"))

	if raises.NObservations == 0 {
		t.Fatal("the map learned nothing: no pair was folded into the relationship")
	}
	if raises.Provenance != Learned {
		t.Errorf("provenance %s after learning; a strength from this system must not "+
			"still claim to be seeded", raises.Provenance)
	}
	if !(raises.Recent > 0.5) {
		t.Errorf("the supported mechanism learned strength %.4f; a stream where pressure "+
			"tracks resource should support it", raises.Recent)
	}
	if relieves.Recent != 0 {
		t.Errorf("the contradicted mechanism learned strength %.4f; evidence of the "+
			"opposite sign is evidence against it, not a weaker version of it",
			relieves.Recent)
	}
	rEff, rKnown := raises.Effective()
	vEff, vKnown := relieves.Effective()
	if !rKnown || !vKnown {
		t.Fatalf("both siblings should have an estimate after observation: raises known=%v relieves known=%v",
			rKnown, vKnown)
	}
	if !(rEff > vEff) {
		t.Errorf("the pair did not separate: raises=%.4f relieves=%.4f", rEff, vEff)
	}
}

// TestLearningIsIdempotentUnderReplay guards the estimator against its own inputs:
// replaying a batch must not inflate a strength by folding the same points again.
func TestLearningIsIdempotentUnderReplay(t *testing.T) {
	build := func() (*Map, *clock) {
		m, c := newTestMap(t, Config{
			ConvergenceObservations: 20, Alpha: 0.5, Learn: true,
			LearnConfig: LearnConfig{PairWindowSeconds: 15, MinSupport: 8, Window: 40},
		})
		for _, id := range []string{"a", "b"} {
			if err := m.DeclareProperty(Property{ID: id, Range: [2]float64{0, 1}}); err != nil {
				t.Fatal(err)
			}
		}
		if err := m.DeclareRelationship(Relationship{
			From: "a", To: "b", Sign: 1,
		}); err != nil {
			t.Fatal(err)
		}
		return m, c
	}
	drive := func(m *Map, c *clock, rounds int) {
		for r := 0; r < rounds; r++ {
			for i := 0; i < 20; i++ {
				c.advance(5 * time.Second)
				x := float64(i%10) / 10.0
				if err := m.ObserveEvent("a", x, c.now(), "a"+itoa(i)); err != nil {
					t.Fatal(err)
				}
				c.advance(time.Second)
				if err := m.ObserveEvent("b", 0.8*x, c.now(), "b"+itoa(i)); err != nil {
					t.Fatal(err)
				}
			}
		}
	}

	once, c1 := build()
	drive(once, c1, 1)
	twice, c2 := build()
	drive(twice, c2, 2) // identical event IDs the second time round

	id := RelationshipID("a", "b", "")
	r1, _ := once.Relationship(id)
	r2, _ := twice.Relationship(id)

	// Folding the same batch again must not double the evidence. It is allowed to add
	// at most one pair: concatenating the batch puts its first observation next to the
	// previous batch's last one, and at that moment that really was the freshest
	// counterpart. That boundary pair is new evidence, not a re-fold.
	if r2.NObservations > r1.NObservations+1 {
		t.Errorf("replaying the same telemetry folded %d extra pairs (%d -> %d); "+
			"at most the one boundary pair should be new",
			r2.NObservations-r1.NObservations, r1.NObservations, r2.NObservations)
	}
	if r2.NObservations >= 2*r1.NObservations {
		t.Errorf("observations doubled on replay (%d -> %d): duplicates are not being "+
			"recognised at all", r1.NObservations, r2.NObservations)
	}
	// The strength may shift slightly for the same reason; it must not move as though
	// the whole batch had arrived twice.
	if diff := r2.Recent - r1.Recent; diff > 0.05 || diff < -0.05 {
		t.Errorf("replay moved the strength by %.4f (%.4f -> %.4f), more than one "+
			"boundary pair can explain", diff, r1.Recent, r2.Recent)
	}
}

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}

// TestSnapshotSurvivesRestart is the reliability property: an agent that has watched a
// system for a while must not return to cold start because its process restarted. It
// checks the two things that matter — what it learned, and the record of how.
func TestSnapshotSurvivesRestart(t *testing.T) {
	path := t.TempDir() + "/state.json"

	m1, c1 := newTestMap(t, Config{
		ConvergenceObservations: 20, Alpha: 0.5, Learn: true,
		LearnConfig: LearnConfig{PairWindowSeconds: 15, MinSupport: 6, Window: 30},
	})
	for _, id := range []string{"a", "b"} {
		if err := m1.DeclareProperty(Property{ID: id, Range: [2]float64{0, 1}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m1.DeclareRelationship(Relationship{From: "a", To: "b", Sign: 1}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		c1.advance(5 * time.Second)
		x := float64(i%10) / 10.0
		if err := m1.ObserveEvent("a", x, c1.now(), "a"+itoa(i)); err != nil {
			t.Fatal(err)
		}
		c1.advance(time.Second)
		if err := m1.ObserveEvent("b", 0.9*x, c1.now(), "b"+itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	b := m1.Decide("dec-before-restart", "estimate", "how is b?")
	pb, _ := b.Property("b")
	b.Commit(map[string]any{"b": pb.Value})

	relID := RelationshipID("a", "b", "")
	learned, _ := m1.Relationship(relID)
	if learned.NObservations == 0 {
		t.Fatal("nothing was learned before the snapshot, so the test proves nothing")
	}
	if err := m1.Save(path); err != nil {
		t.Fatal(err)
	}

	m2, _ := newTestMap(t, Config{ConvergenceObservations: 20, Learn: true})
	r, err := m2.Load(path)
	loaded := r.Loaded
	if err != nil {
		t.Fatal(err)
	}
	if !loaded {
		t.Fatal("Load reported nothing restored")
	}

	got, ok := m2.Relationship(relID)
	if !ok {
		t.Fatal("the learned relationship did not survive")
	}
	if got.Recent != learned.Recent || got.NObservations != learned.NObservations {
		t.Errorf("restored strength %.6f/%d, had %.6f/%d — an agent that forgets what it "+
			"learned is back at cold start on a system it has already watched",
			got.Recent, got.NObservations, learned.Recent, learned.NObservations)
	}
	if got.Provenance != learned.Provenance {
		t.Errorf("restored provenance %s, had %s", got.Provenance, learned.Provenance)
	}

	// The audit trail has to survive too, or "why did you do that yesterday" becomes
	// unanswerable after a restart.
	d, ok := m2.Journal().Decision("dec-before-restart")
	if !ok {
		t.Fatal("a decision taken before the restart is no longer retrievable")
	}
	if len(d.PropertiesRead) == 0 {
		t.Error("the restored decision lost its inputs, so it can no longer justify itself")
	}

	// Pair windows are deliberately not restored: simultaneity cannot span a restart
	// of unknown length.
	if n := m2.PairSupport(relID); n != 0 {
		t.Errorf("pair support %d after restore; the estimator should resume from its "+
			"conclusions, not from a window spanning the restart", n)
	}
}

// TestLoadRefusesAmbiguity covers the cases where guessing would put wrong values into
// a model the agent then reasons from.
func TestLoadRefusesAmbiguity(t *testing.T) {
	dir := t.TempDir()

	m, _ := newTestMap(t, Config{})
	r, err := m.Load(dir + "/absent.json")
	loaded := r.Loaded
	if err != nil || loaded {
		t.Errorf("a missing snapshot should be a quiet first start; got loaded=%v err=%v", loaded, err)
	}

	bad := dir + "/future.json"
	if err := os.WriteFile(bad, []byte(`{"version":999,"revision":5}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Load(bad); err == nil {
		t.Error("a snapshot from an unknown version was accepted")
	}

	good := dir + "/good.json"
	if err := m.Save(good); err != nil {
		t.Fatal(err)
	}
	m2, _ := newTestMap(t, Config{})
	if err := m2.DeclareProperty(Property{ID: "already-here"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m2.Load(good); err == nil {
		t.Error("loading into a populated map was accepted; the merge would be invisible")
	}
}

// TestSaveIsAtomicUnderConcurrentWriters guards the shutdown case: the periodic save
// loop and the synchronous shutdown save can both call Save on the same path at once
// when the context is cancelled mid-tick. A shared temp path let them clobber each
// other; a uniquely-named temp per save does not. The file must always be a complete,
// loadable snapshot, and no temp litter may survive.
func TestSaveIsAtomicUnderConcurrentWriters(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/state.json"

	m, _ := newTestMap(t, Config{Owner: "node-x", ConvergenceObservations: 10, Alpha: 0.5})
	if err := m.DeclareProperty(Property{ID: "cpu", Range: [2]float64{0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := m.Observe("cpu", 0.4, time.Now()); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if err := m.Save(path); err != nil {
					t.Errorf("concurrent save: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	// The destination is a complete snapshot, not a half-written one.
	fresh, _ := newTestMap(t, Config{Owner: "node-x"})
	r, err := fresh.Load(path)
	loaded := r.Loaded
	if err != nil || !loaded {
		t.Fatalf("after concurrent saves the snapshot did not load cleanly: loaded=%v err=%v", loaded, err)
	}
	if _, ok := fresh.Property("cpu"); !ok {
		t.Error("restored map is missing a property that was present when saved")
	}

	// No temp files survive: a unique name per save is only safe if each one is also
	// cleaned up, or the dir fills with orphaned .tmp-* beside the real snapshot.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temp litter survived concurrent saves: %s", e.Name())
		}
	}
}

// TestSaveWritesPrivateFileMode pins the snapshot at 0o600: it holds this machine's
// journal, decisions and telemetry-derived state, which is not world-readable material
// on a shared host.
func TestSaveWritesPrivateFileMode(t *testing.T) {
	path := t.TempDir() + "/state.json"
	m, _ := newTestMap(t, Config{Owner: "node-x"})
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("snapshot mode is %o, want 600 — a per-machine private snapshot should "+
			"not be world-readable", perm)
	}
}

// TestSignSuspectSeparatesAWrongDeclarationFromAQuietSystem covers the failure this
// counter exists for. A relationship whose declared sign the machine never shows sits
// at zero strength — which is exactly what a relationship on an idle system looks
// like. The strength cannot tell the two apart, so nothing downstream can either, and
// a graph carrying a backwards claim reports itself as merely unobserved.
func TestSignSuspectSeparatesAWrongDeclarationFromAQuietSystem(t *testing.T) {
	m, c := newTestMap(t, Config{
		ConvergenceObservations: 20,
		Alpha:                   0.5,
		Learn:                   true,
		LearnConfig:             LearnConfig{PairWindowSeconds: 15, MinSupport: 8, Window: 40},
	})
	for _, id := range []string{"resource", "pressure"} {
		if err := m.DeclareProperty(Property{ID: id, Range: [2]float64{0, 1}}); err != nil {
			t.Fatal(err)
		}
	}
	// "backwards" asserts that pressure falls as resource rises. The stream below shows
	// the opposite, on every pair — the shape of a proposition inherited from a
	// framework that phrased its outcome in the opposite polarity.
	if err := m.DeclareRelationship(Relationship{
		From: "resource", To: "pressure", Label: "backwards", Sign: -1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.DeclareRelationship(Relationship{
		From: "resource", To: "pressure", Label: "correct", Sign: 1,
	}); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 40; i++ {
		c.advance(5 * time.Second)
		x := float64(i%10) / 10.0
		if err := m.ObserveEvent("resource", x, c.now(), "r"+itoa(i)); err != nil {
			t.Fatal(err)
		}
		c.advance(2 * time.Second)
		if err := m.ObserveEvent("pressure", 0.9*x+0.02, c.now(), "p"+itoa(i)); err != nil {
			t.Fatal(err)
		}
	}

	backwards, _ := m.Relationship(RelationshipID("resource", "pressure", "backwards"))
	correct, _ := m.Relationship(RelationshipID("resource", "pressure", "correct"))

	if backwards.Recent != 0 {
		t.Fatalf("precondition: the contradicted relationship should sit at zero, got %.4f",
			backwards.Recent)
	}
	if backwards.SignConflicts == 0 {
		t.Error("no sign conflict was counted; the gate zeroed the strength and left no " +
			"trace, which is the state that made this class of defect invisible")
	}
	if backwards.SignAgreements != 0 {
		t.Errorf("counted %d agreements on a stream of the opposite sign",
			backwards.SignAgreements)
	}
	if !backwards.SignSuspect() {
		t.Errorf("a relationship contradicted by every one of %d pairs is not reported "+
			"suspect; it is indistinguishable from unobserved", backwards.SignConflicts)
	}

	if correct.SignAgreements == 0 {
		t.Error("the supported relationship counted no agreements")
	}
	if correct.SignSuspect() {
		t.Error("the supported relationship is reported suspect; a claim the machine " +
			"confirms must never be flagged as a bad declaration")
	}

	// The census is where an aggregate reader would otherwise miss it entirely.
	if got := m.Census().RelationshipsSignSuspect; got != 1 {
		t.Errorf("census reports %d sign-suspect relationships, want 1", got)
	}
}

// TestSignSuspectIsNotTrippedByAQuietSystem is the other half: a relationship that has
// simply not been observed enough must not be accused of declaring the wrong sign.
func TestSignSuspectIsNotTrippedByAQuietSystem(t *testing.T) {
	m, _ := newTestMap(t, Config{ConvergenceObservations: 20, Learn: true})
	for _, id := range []string{"resource", "pressure"} {
		if err := m.DeclareProperty(Property{ID: id, Range: [2]float64{0, 1}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.DeclareRelationship(Relationship{
		From: "resource", To: "pressure", Label: "unobserved", Sign: -1,
	}); err != nil {
		t.Fatal(err)
	}
	r, _ := m.Relationship(RelationshipID("resource", "pressure", "unobserved"))
	if r.SignSuspect() {
		t.Error("an unobserved relationship is reported sign-suspect; silence is not " +
			"evidence that a declaration is wrong")
	}
	if got := m.Census().RelationshipsSignSuspect; got != 0 {
		t.Errorf("census reports %d sign-suspect on an unobserved graph, want 0", got)
	}
}

// TestSignSuspectIgnoresARegimeDependentSign is the false-positive guard. A relationship
// whose sign genuinely changes with the workload collects both agreements and conflicts
// in quantity. That is a fact about the system, not a defect in the declaration, and
// flagging it would train an operator to ignore the flag.
func TestSignSuspectIgnoresARegimeDependentSign(t *testing.T) {
	m, c := newTestMap(t, Config{
		ConvergenceObservations: 20,
		Alpha:                   0.5,
		Learn:                   true,
		LearnConfig:             LearnConfig{PairWindowSeconds: 15, MinSupport: 4, Window: 12},
	})
	for _, id := range []string{"resource", "pressure"} {
		if err := m.DeclareProperty(Property{ID: id, Range: [2]float64{0, 1}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.DeclareRelationship(Relationship{
		From: "resource", To: "pressure", Label: "regime", Sign: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// Alternate the association: blocks where pressure tracks resource, blocks where it
	// opposes it. Neither sign holds throughout.
	for block := 0; block < 12; block++ {
		positive := block%2 == 0
		for i := 0; i < 12; i++ {
			c.advance(5 * time.Second)
			x := float64(i%10) / 10.0
			if err := m.ObserveEvent("resource", x, c.now(),
				"r"+itoa(block)+"_"+itoa(i)); err != nil {
				t.Fatal(err)
			}
			y := 0.9*x + 0.02
			if !positive {
				y = 0.9*(1-x) + 0.02
			}
			c.advance(2 * time.Second)
			if err := m.ObserveEvent("pressure", y, c.now(),
				"p"+itoa(block)+"_"+itoa(i)); err != nil {
				t.Fatal(err)
			}
		}
	}

	r, _ := m.Relationship(RelationshipID("resource", "pressure", "regime"))
	if r.SignAgreements == 0 || r.SignConflicts == 0 {
		t.Fatalf("precondition: expected both outcomes, got agree=%d conflict=%d",
			r.SignAgreements, r.SignConflicts)
	}
	if r.SignSuspect() {
		t.Errorf("a regime-dependent sign was flagged suspect at a conflict share of "+
			"%.2f; a sign that holds in some regimes is a property of the system, not a "+
			"wrong declaration", r.SignConflictShare())
	}
	if got := m.Census().RelationshipsSignSuspect; got != 0 {
		t.Errorf("census reports %d sign-suspect, want 0", got)
	}
}

// ── The established layer ────────────────────────────────────────────────────

// feedPairs drives n paired observations whose correlation is controlled by `assoc`:
// 1.0 gives a perfectly correlated pair stream, 0.0 an uncorrelated one.
func feedPairs(t *testing.T, m *Map, c *clock, n int, assoc float64, tag string) {
	t.Helper()
	for i := 0; i < n; i++ {
		feedPair(t, m, c, i, assoc, tag)
	}
}

// feedPair feeds the i-th pair of feedPairs' sequence.
func feedPair(t *testing.T, m *Map, c *clock, i int, assoc float64, tag string) {
	t.Helper()
	c.advance(time.Second)
	x := float64(i%10) / 10.0
	// Alternating perturbation breaks the correlation when assoc < 1 without
	// changing either series' range, so the two cases differ in association only.
	y := assoc*x + (1-assoc)*float64((i*7)%10)/10.0
	if err := m.ObserveEvent("resource", x, c.now(), tag+"r"+itoa(i)); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Second)
	if err := m.ObserveEvent("pressure", y, c.now(), tag+"p"+itoa(i)); err != nil {
		t.Fatal(err)
	}
}

// observeHalf makes the k-th observation of a pair sequence: resource on even k,
// pressure (y of resource's x) on odd k. Each one folds a pair once the window is
// full, so a test can check what each fold did.
func observeHalf(t *testing.T, m *Map, c *clock, k int, y func(x float64) float64, tag string) {
	t.Helper()
	j := k / 2
	x := float64(j%10) / 10.0
	c.advance(time.Second)
	id, v, ev := "resource", x, tag+"r"+itoa(j)
	if k%2 == 1 {
		id, v, ev = "pressure", y(x), tag+"p"+itoa(j)
	}
	if err := m.ObserveEvent(id, v, c.now(), ev); err != nil {
		t.Fatal(err)
	}
}

func opposed(x float64) float64 { return 1 - x }
func same(x float64) float64    { return x }

// feedOpposed feeds the i-th pair with pressure moving against resource: the declared
// sign contradicted, so the strength that folds is zero. Over a window of 10 an
// assoc-0 stretch still shows |r| near 0.5, inside the band; this one is outside it.
func feedOpposed(t *testing.T, m *Map, c *clock, i int, tag string) {
	t.Helper()
	observeHalf(t, m, c, 2*i, opposed, tag)
	observeHalf(t, m, c, 2*i+1, opposed, tag)
}

func newLearningPair(t *testing.T, alphaSlow float64) (*Map, *clock, string) {
	t.Helper()
	return newLearningPairWindow(t, alphaSlow, 60)
}

func newLearningPairWindow(t *testing.T, alphaSlow float64, window int) (*Map, *clock, string) {
	t.Helper()
	m, c := newTestMap(t, Config{
		ConvergenceObservations: 500,
		Alpha:                   0.2,
		AlphaSlow:               alphaSlow,
		Learn:                   true,
		LearnConfig:             LearnConfig{PairWindowSeconds: 15, MinSupport: 8, Window: window},
	})
	for _, id := range []string{"resource", "pressure"} {
		if err := m.DeclareProperty(Property{ID: id, Range: [2]float64{0, 1}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.DeclareRelationship(Relationship{
		From: "resource", To: "pressure", Label: "raises", Sign: 1,
	}); err != nil {
		t.Fatal(err)
	}
	return m, c, RelationshipID("resource", "pressure", "raises")
}

// TestEstablishedAppearsOnlyWithSupportAndIsNeverSeeded pins the two halves of the
// layer's contract: nothing puts a value there but observation, and it says nothing
// until it has some.
func TestEstablishedAppearsOnlyWithSupportAndIsNeverSeeded(t *testing.T) {
	m, c, id := newLearningPair(t, 0.001)

	r, _ := m.Relationship(id)
	if r.Established != nil {
		t.Errorf("a freshly declared relationship has an established strength %.4f; the "+
			"long-run layer is accumulated from this machine's pairs and nothing seeds it",
			*r.Established)
	}
	if r.Basis() != "unknown" {
		t.Errorf("basis %q before observation, want unknown", r.Basis())
	}

	// Below the support threshold nothing folds at all, so still nothing established.
	feedPairs(t, m, c, 4, 1.0, "a")
	r, _ = m.Relationship(id)
	if r.NObservations != 0 {
		t.Fatalf("precondition: %d folds from 4 pairs, want 0 below MinSupport", r.NObservations)
	}
	if r.Established != nil {
		t.Errorf("established %.4f with no folds", *r.Established)
	}

	feedPairs(t, m, c, 40, 1.0, "b")
	r, _ = m.Relationship(id)
	if r.NObservations == 0 {
		t.Fatal("nothing folded after 44 pairs")
	}
	if r.Established == nil {
		t.Fatal("established is still absent after folding began")
	}
	if r.Basis() != "established" {
		t.Errorf("basis %q once a long-run value exists, want established", r.Basis())
	}
	eff, known := r.Effective()
	if !known || eff != *r.Established {
		t.Errorf("effective %.4f (known=%v) does not report the established value %.4f",
			eff, known, *r.Established)
	}
}

// TestEstablishedIsUnbiasedEarly is the property the bias correction exists for.
//
// Without it a slow layer spends thousands of pairs mostly reporting the zero it
// started from: at alpha = 0.001, ten folds of a constant strength of 1.0 would leave
// an uncorrected accumulator at 1 - 0.999^10 = 0.00996 — a hundredth of the truth, and
// indistinguishable from a relationship the machine has refuted. That initialisation
// dependence is what made the raw slow layer vary across replays by an order of
// magnitude more than the fast one.
func TestEstablishedIsUnbiasedEarly(t *testing.T) {
	m, c, id := newLearningPair(t, 0.001)
	// A perfectly correlated stream, so every fold sees a strength of exactly 1.0 and
	// the long-run mean of what was observed is unambiguous.
	feedPairs(t, m, c, 10, 1.0, "a")

	r, _ := m.Relationship(id)
	if r.NObservations == 0 {
		t.Fatal("nothing folded")
	}
	if r.Established == nil {
		t.Fatal("no established value after folding began")
	}
	uncorrected := 1 - math.Pow(1-0.001, float64(r.NObservations))
	t.Logf("%d folds of strength 1.0: established %.6f (uncorrected would be %.6f)",
		r.NObservations, *r.Established, uncorrected)

	if *r.Established < 0.9 {
		t.Errorf("established %.6f after %d folds of a constant 1.0; the long-run mean "+
			"of a constant is that constant, and a value near %.4f would be the "+
			"initialisation showing through", *r.Established, r.NObservations, uncorrected)
	}
	// The two layers should be close on a short stream — both are near the mean of the
	// handful of folds so far — but not identical. Pairing produces same-tick and
	// one-behind pairs, so fold strengths vary a little, and two time constants over a
	// varying series do not have to coincide. Closeness is the claim; equality is not.
	if math.Abs(*r.Established-r.Recent) > 0.05 {
		t.Errorf("established %.6f and recent %.6f differ by more than 0.05 this early; "+
			"before history accumulates there is nothing for them to disagree about",
			*r.Established, r.Recent)
	}
}

// TestTheTwoLayersSeparateOnARegimeChange is the point of having two. The recent layer
// follows the machine's new behaviour; the established layer holds what the machine has
// mostly done, and the gap between them is how unusual the present is.
func TestTheTwoLayersSeparateOnARegimeChange(t *testing.T) {
	m, c, id := newLearningPair(t, 0.001)

	// A long stretch of strong association, then a stretch of none.
	feedPairs(t, m, c, 300, 1.0, "strong")
	afterStrong, _ := m.Relationship(id)
	if afterStrong.Established == nil {
		t.Fatal("nothing established after 300 pairs")
	}
	establishedBefore := *afterStrong.Established

	feedPairs(t, m, c, 120, 0.0, "weak")
	after, _ := m.Relationship(id)

	recentDrop := afterStrong.Recent - after.Recent
	establishedDrop := establishedBefore - *after.Established
	t.Logf("recent      %.4f -> %.4f  (moved %.4f)", afterStrong.Recent, after.Recent, recentDrop)
	t.Logf("established %.4f -> %.4f  (moved %.4f)", establishedBefore, *after.Established, establishedDrop)

	if recentDrop <= 0 {
		t.Errorf("the recent layer did not fall when the association went away "+
			"(%.4f -> %.4f)", afterStrong.Recent, after.Recent)
	}
	if establishedDrop >= recentDrop {
		t.Errorf("the established layer moved as much as the recent one (%.4f vs %.4f); "+
			"then it is a duplicate rather than a second timescale",
			establishedDrop, recentDrop)
	}
	if math.Abs(after.Recent-*after.Established) < 0.05 {
		t.Errorf("recent %.4f and established %.4f are within 0.05 after a regime "+
			"change; the gap between them is the signal that the present is unusual",
			after.Recent, *after.Established)
	}
}

// TestAssertionOutranksTheEstablishedLayer keeps the precedence intact now that there
// are two learned layers under it.
func TestAssertionOutranksTheEstablishedLayer(t *testing.T) {
	m, c, id := newLearningPair(t, 0.001)
	feedPairs(t, m, c, 60, 1.0, "a")

	r, _ := m.Relationship(id)
	if r.Established == nil || r.Basis() != "established" {
		t.Fatalf("precondition: basis %q, established %v", r.Basis(), r.Established)
	}

	if err := m.AssertRelationshipStrength(id, 0.13, "operator:ada", "measured by hand"); err != nil {
		t.Fatal(err)
	}
	r, _ = m.Relationship(id)
	eff, known := r.Effective()
	if !known || eff != 0.13 {
		t.Errorf("effective %.4f after asserting 0.13; an operator's value outranks both "+
			"learned layers and takes effect in full", eff)
	}
	if r.Basis() != "asserted" {
		t.Errorf("basis %q, want asserted", r.Basis())
	}
	if r.Established == nil {
		t.Error("the assertion discarded the established value; what was measured must " +
			"stay readable beside what was asserted")
	}
}

// TestAlphaSlowDefaultsToTheDerivedConstant guards the number against a silent change,
// and records where it comes from.
func TestAlphaSlowDefaultsToTheDerivedConstant(t *testing.T) {
	got := Config{}.withDefaults().AlphaSlow
	if got != 0.001 {
		t.Errorf("AlphaSlow default is %v, want 0.001 — the point chosen on the "+
			"order-invariance/responsiveness trade-off measured by "+
			"convergence/sweep_alpha_slow.sh. Changing it changes what 'normal for this "+
			"machine' means, so it should not drift by accident.", got)
	}
	fast := Config{}.withDefaults().Alpha
	if got >= fast {
		t.Errorf("AlphaSlow %v is not slower than Alpha %v", got, fast)
	}
}

// ── subjects: a property is observable property × subject ─────────────────────

func TestRecordStampsSubjectUnitRangeAndLabelsAtAdmission(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	rng := [2]float64{0, 100}
	err := m.Record(Observation{
		ID: "queue_depth@pod:abc", Value: 7, At: c.now(), EventID: "e1",
		Subject: "pod:abc", Unit: "items", Range: &rng, Source: "app:ingest",
		Labels: map[string]string{"kind": "pod"},
	})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := m.Property("queue_depth@pod:abc")
	if p.Subject != "pod:abc" || p.Unit != "items" || p.Range != rng || !p.RangeDeclared ||
		p.Source != "app:ingest" || p.Labels["kind"] != "pod" {
		t.Errorf("admitted property %+v; want subject, unit, range, source and labels from the observation", p)
	}
}

func TestRecordWithoutRangeAssumesUnitIntervalAndSaysSo(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	if err := m.Record(Observation{ID: "x@disk:sda", Value: 0.2, At: c.now(), Subject: "disk:sda"}); err != nil {
		t.Fatal(err)
	}
	p, _ := m.Property("x@disk:sda")
	if p.Range != [2]float64{0, 1} || p.RangeDeclared {
		t.Errorf("got range %v declared=%v; want [0,1] assumed and range_declared=false", p.Range, p.RangeDeclared)
	}
}

// Save encodes the snapshot after releasing the map's lock, so a snapshot that shares a
// property's Labels with the map is encoded while observations merge labels into it: a
// fatal concurrent map iteration and map write in the save loop, which ended a case-study
// run on the VM.
func TestSnapshotDoesNotShareLabelsWithTheMap(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	_ = m.Record(Observation{ID: "a@pod:1", Value: 1, At: c.now(), Subject: "pod:1",
		Labels: map[string]string{"qos": "burstable"}})
	snap := m.Snapshot()
	c.advance(time.Second)
	_ = m.Record(Observation{ID: "a@pod:1", Value: 2, At: c.now(), Subject: "pod:1",
		Labels: map[string]string{"name": "redis-0"}})
	if _, ok := snap.Properties[0].Labels["name"]; ok {
		t.Fatal("an observation after the snapshot changed its labels: the snapshot shares the map's")
	}
}

func TestRecordMergesLabelsAndKeepsSubjectImmutable(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	_ = m.Record(Observation{ID: "a@pod:1", Value: 1, At: c.now(), Subject: "pod:1",
		Labels: map[string]string{"qos": "burstable"}})
	c.advance(time.Second)
	_ = m.Record(Observation{ID: "a@pod:1", Value: 2, At: c.now(), Subject: "pod:1",
		Labels: map[string]string{"name": "redis-0", "qos": "guaranteed"}})
	p, _ := m.Property("a@pod:1")
	if p.Labels["name"] != "redis-0" || p.Labels["qos"] != "guaranteed" {
		t.Errorf("labels %v; want merged with later keys winning", p.Labels)
	}
	// A different subject under the same id is a conflict, journaled and not applied.
	c.advance(time.Second)
	_ = m.Record(Observation{ID: "a@pod:1", Value: 3, At: c.now(), Subject: "pod:2"})
	p, _ = m.Property("a@pod:1")
	if p.Subject != "pod:1" {
		t.Errorf("subject changed to %q; it is part of the identity and must not move", p.Subject)
	}
	var conflict bool
	for _, e := range m.Journal().Events(0, 0) {
		if e.Kind == EventPropertyConflict && e.Target == "a@pod:1" {
			conflict = true
		}
	}
	if !conflict {
		t.Error("subject conflict was not journaled")
	}
}

func TestCensusCountsSubjectsAndQueryFiltersBySubject(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	_ = m.Record(Observation{ID: "cpu@pod:1", Value: .1, At: c.now(), Subject: "pod:1"})
	_ = m.Record(Observation{ID: "mem@pod:1", Value: .1, At: c.now(), Subject: "pod:1"})
	_ = m.Record(Observation{ID: "cpu@pod:2", Value: .1, At: c.now(), Subject: "pod:2"})
	_ = m.Record(Observation{ID: "cpu", Value: .1, At: c.now()})
	if got := m.Census().Subjects; got != 2 {
		t.Errorf("census subjects=%d, want 2 (node scope is not a subject)", got)
	}
	v := m.State(Query{Subject: "pod:1"})
	if len(v.Properties) != 2 {
		t.Errorf("Query{Subject: pod:1} returned %d properties, want 2", len(v.Properties))
	}
}

// ── review fix round 1: Record must reject a Derived id before mutating state,
// and a redeclared Range must count as declared ────────────────────────────────

func TestRecordOnDerivedIdIsRejectedWithoutSideEffects(t *testing.T) {
	m, c := newTestMap(t, Config{ConvergenceObservations: 4})
	if err := m.DeclareProperty(Property{ID: "cpu", Range: [2]float64{0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := m.DeclareProperty(Property{
		ID: "RC", Kind: Derived, Members: []string{"cpu"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Observe("cpu", 0.5, c.now()); err != nil {
		t.Fatal(err)
	}

	revBefore := m.Revision()
	eventsBefore := len(m.Journal().Events(0, 0))

	c.advance(time.Second)
	err := m.Record(Observation{
		ID: "RC", Value: 0.5, At: c.now(), Subject: "pod:x",
		Labels: map[string]string{"k": "v"},
	})
	if err == nil {
		t.Fatal("want an error recording onto a derived id, got nil")
	}
	if got := m.Revision(); got != revBefore {
		t.Errorf("revision advanced from %d to %d on a rejected observation", revBefore, got)
	}
	if got := len(m.Journal().Events(0, 0)); got != eventsBefore {
		t.Errorf("journal grew from %d to %d events on a rejected observation", eventsBefore, got)
	}
	rc, _ := m.Property("RC")
	if len(rc.Labels) != 0 {
		t.Errorf("RC.Labels = %v; want untouched by the rejected observation", rc.Labels)
	}
}

// TestRedeclaringTheSameRangeMarksItDeclared covers the range a producer declares that
// happens to equal the range the map had already assumed. Only comparing the numbers
// would leave such a property flagged as assumed forever — and an assumed range is not
// a cosmetic flag: every estimate that reads the property carries a caveat saying its
// contribution was normalised by a range nobody declared, and reconcileDeclarationLocked
// lets a later producer overwrite a range the operator declared while the flag is false.
func TestRedeclaringTheSameRangeMarksItDeclared(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	if err := m.Observe("q", 0.5, c.now()); err != nil {
		t.Fatal(err)
	}
	p, _ := m.Property("q")
	if p.Range != ([2]float64{0, 1}) || p.RangeDeclared {
		t.Fatalf("admitted q is %v declared=%v; want the assumed [0,1]", p.Range, p.RangeDeclared)
	}

	eventsBefore := len(m.Journal().Events(0, 0))
	if err := m.DeclareProperty(Property{ID: "q", Range: [2]float64{0, 1}}); err != nil {
		t.Fatal(err)
	}
	p, _ = m.Property("q")
	if !p.RangeDeclared {
		t.Error("RangeDeclared = false after a declaration carrying [0,1], the same numbers the map had assumed; want true")
	}
	redeclared := 0
	for _, e := range m.Journal().Events(0, 0)[eventsBefore:] {
		if e.Kind == EventPropertyRedeclared && e.Target == "q" {
			redeclared++
			if e.Detail["range_declared"] != true {
				t.Errorf("redeclaration journaled %v; want range_declared recorded on the flip", e.Detail)
			}
		}
	}
	if redeclared != 1 {
		t.Errorf("%d redeclaration events for the flip; want exactly 1", redeclared)
	}

	// Idempotent: declaring it again changes nothing, so the journal stays silent.
	eventsBefore = len(m.Journal().Events(0, 0))
	if err := m.DeclareProperty(Property{ID: "q", Range: [2]float64{0, 1}}); err != nil {
		t.Fatal(err)
	}
	for _, e := range m.Journal().Events(0, 0)[eventsBefore:] {
		if e.Target == "q" {
			t.Errorf("re-declaring an unchanged property journaled %+v; want silence", e)
		}
	}
}

// TestSnapshotRestoredRangesAreDeclaredAgainOnRedeclaration is the upgrade case: a
// snapshot written before range_declared existed restores every property with the flag
// false, and the collectors that re-declare on start mostly declare the same numbers
// the map already holds. Without the fix every seeded property on an upgraded agent
// answers estimates with a false "range was assumed, not declared" caveat.
func TestSnapshotRestoredRangesAreDeclaredAgainOnRedeclaration(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	for _, id := range []string{"a", "b@pod:1"} {
		if err := m.Record(Observation{ID: id, Value: 0.4, At: c.now(), Subject: subjectOf(id)}); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "snapshot.json")
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}

	restored, _ := newTestMap(t, Config{AdmitUnknown: true})
	if r, err := restored.Load(path); err != nil || !r.Loaded {
		t.Fatalf("Load = %v, %v; want a restored snapshot", r.Loaded, err)
	}
	for _, id := range []string{"a", "b@pod:1"} {
		if p, _ := restored.Property(id); p.RangeDeclared {
			t.Fatalf("%s restored with RangeDeclared=true; this test needs the pre-branch shape it models", id)
		}
		if err := restored.DeclareProperty(Property{ID: id, Range: [2]float64{0, 1}, Subject: subjectOf(id)}); err != nil {
			t.Fatal(err)
		}
		if p, _ := restored.Property(id); !p.RangeDeclared {
			t.Errorf("%s: RangeDeclared = false after the producer re-declared its range; want true", id)
		}
	}
}

func TestRedeclaringARangeMakesItDeclared(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	if err := m.Record(Observation{ID: "q@pod:1", Value: 5, At: c.now(), Subject: "pod:1"}); err != nil {
		t.Fatal(err)
	}
	p, _ := m.Property("q@pod:1")
	if p.RangeDeclared {
		t.Fatalf("admitted property already has RangeDeclared=true; want an assumed range")
	}

	if err := m.DeclareProperty(Property{ID: "q@pod:1", Subject: "pod:1", Range: [2]float64{0, 100}}); err != nil {
		t.Fatal(err)
	}
	p, _ = m.Property("q@pod:1")
	if !p.RangeDeclared {
		t.Fatalf("RangeDeclared = false after DeclareProperty carried a Range; want true")
	}

	c.advance(time.Second)
	rng := [2]float64{0, 1}
	if err := m.Record(Observation{ID: "q@pod:1", Value: 6, At: c.now(), Subject: "pod:1", Range: &rng}); err == nil {
		t.Error("an observation contradicting the declared range was applied")
	}
	p, _ = m.Property("q@pod:1")
	if p.Range != ([2]float64{0, 100}) {
		t.Errorf("Range = %v after a conflicting observation; want the declared [0,100] to hold", p.Range)
	}
	var conflict bool
	for _, e := range m.Journal().Events(0, 0) {
		if e.Kind == EventPropertyConflict && e.Target == "q@pod:1" {
			conflict = true
		}
	}
	if !conflict {
		t.Error("range conflict was not journaled")
	}
}

func TestRecordAdoptsRangeAndUnitDeclaredLate(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	if err := m.Record(Observation{ID: "x@pod:1", Value: 0.5, At: c.now(), Subject: "pod:1"}); err != nil {
		t.Fatal(err)
	}

	eventsBefore := len(m.Journal().Events(0, 0))
	c.advance(time.Second)
	rng := [2]float64{0, 10}
	if err := m.Record(Observation{
		ID: "x@pod:1", Value: 0.6, At: c.now(), Subject: "pod:1", Unit: "items", Range: &rng,
	}); err != nil {
		t.Fatal(err)
	}
	p, _ := m.Property("x@pod:1")
	if p.Unit != "items" {
		t.Errorf("Unit = %q, want %q", p.Unit, "items")
	}
	if p.Range != ([2]float64{0, 10}) {
		t.Errorf("Range = %v, want [0,10]", p.Range)
	}
	if !p.RangeDeclared {
		t.Error("RangeDeclared = false after a late Range declaration; want true")
	}
	for _, e := range m.Journal().Events(0, 0)[eventsBefore:] {
		if e.Kind == EventPropertyConflict && e.Target == "x@pod:1" {
			t.Errorf("unexpected property.conflict journaled for a late range/unit declaration: %+v", e)
		}
	}
}

func TestSweepRetirementCascadesToRelationships(t *testing.T) {
	m, c := newTestMap(t, Config{StaleAfter: 30 * time.Second, RetireAfter: time.Minute, AdmitUnknown: true})
	_ = m.DeclareProperty(Property{ID: "pressure"})
	_ = m.Record(Observation{ID: "cpu@pod:1", Value: .5, At: c.now(), Subject: "pod:1"})
	_ = m.Observe("pressure", .2, c.now())
	if err := m.DeclareRelationship(Relationship{From: "cpu@pod:1", To: "pressure", Sign: 1, Label: "discovered"}); err != nil {
		t.Fatal(err)
	}
	c.advance(2 * time.Minute)
	_ = m.Observe("pressure", .2, c.now()) // the node-level property stays alive
	_, retired := m.Sweep()
	if len(retired) != 1 || retired[0] != "cpu@pod:1" {
		t.Fatalf("retired=%v; want cpu@pod:1", retired)
	}
	r, _ := m.Relationship(RelationshipID("cpu@pod:1", "pressure", "discovered"))
	if r.Status != Retired || !strings.Contains(r.RetiredReason, "cpu@pod:1") {
		t.Errorf("relationship %+v; want retired by cascade from its endpoint", r)
	}
}

// TestSweepReleasesTheLockWhenItPanics guards the write lock against a panic raised
// inside Sweep's locked body. net/http recovers a panic raised in a handler and keeps
// serving, so a Sweep that took the lock without `defer Unlock` would leave the daemon
// answering connections while every map operation blocks forever — a wedge with no
// crash to point at. The clock is the injectable thing inside the locked body, so it
// stands in for any panic reachable from there.
func TestSweepReleasesTheLockWhenItPanics(t *testing.T) {
	m, c := newTestMap(t, Config{StaleAfter: 10 * time.Second, RetireAfter: 20 * time.Second, AdmitUnknown: true})
	_ = m.Observe("x", 1, c.now())

	var calls int
	at := c.t.Add(time.Minute)
	m.SetClock(func() time.Time {
		calls++
		if calls == 2 {
			panic("clock failed under the write lock")
		}
		return at
	})
	m.SetMonotonic(func() time.Duration { return c.m + time.Minute })

	m.Sweep() // first call: x falls silent past RetireAfter, so this runs retireLocked
	if p, ok := m.Property("x"); !ok || p.Status != Retired {
		t.Fatalf("x is %+v; the first sweep was meant to retire it through retireLocked", p)
	}

	var panicked bool
	func() {
		defer func() { panicked = recover() != nil }()
		m.Sweep() // second clock call: panics with the write lock held
	}()
	if !panicked {
		t.Fatal("the second sweep did not panic; this test cannot observe a leaked lock")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Property("x")
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Property blocked for a second after a panic inside Sweep: the write lock was never released")
	}
}

func TestRetirePropertyReleasesTheLockWhenItPanics(t *testing.T) {
	m, c := newTestMap(t, Config{StaleAfter: 10 * time.Second, RetireAfter: 20 * time.Second, AdmitUnknown: true})
	_ = m.Observe("x", 1, c.now())
	m.SetClock(func() time.Time { panic("clock failed under the write lock") })

	var panicked bool
	func() {
		defer func() { panicked = recover() != nil }()
		_ = m.RetireProperty("x", "test", "operator")
	}()
	if !panicked {
		t.Fatal("RetireProperty did not panic; this test cannot observe a leaked lock")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Property("x")
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Property blocked for a second after a panic inside RetireProperty: the write lock was never released")
	}
}

func TestRetireHookFiresOnBothPaths(t *testing.T) {
	m, c := newTestMap(t, Config{StaleAfter: 10 * time.Second, RetireAfter: 20 * time.Second, AdmitUnknown: true})
	var got []string
	m.SetRetireHook(func(id string) { got = append(got, id) })
	_ = m.Observe("a", 1, c.now())
	_ = m.Observe("b", 1, c.now())
	if err := m.RetireProperty("a", "test", "operator"); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Minute)
	m.Sweep()
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("hook saw %v; want [a b] (operator path then sweep path)", got)
	}
}

func TestObservationRevivesARetiredProperty(t *testing.T) {
	m, c := newTestMap(t, Config{StaleAfter: 10 * time.Second, RetireAfter: 20 * time.Second, AdmitUnknown: true})
	_ = m.Record(Observation{ID: "cpu@pod:1", Value: .3, At: c.now(), Subject: "pod:1"})
	c.advance(time.Minute)
	m.Sweep()
	if p, _ := m.Property("cpu@pod:1"); p.Status != Retired {
		t.Fatalf("setup: status %s, want retired", p.Status)
	}
	c.advance(time.Second)
	_ = m.Record(Observation{ID: "cpu@pod:1", Value: .4, At: c.now(), Subject: "pod:1"})
	p, _ := m.Property("cpu@pod:1")
	if p.Status != Active || p.RetiredReason != "" {
		t.Errorf("after re-observation: %+v; want active with the retirement reason cleared", p)
	}
	var revived bool
	for _, e := range m.Journal().Events(0, 0) {
		if e.Kind == EventPropertyRedeclared && e.Target == "cpu@pod:1" && e.Detail["revived"] == true {
			revived = true
		}
	}
	if !revived {
		t.Error("revival was not journaled")
	}
}

func TestMarkPropertyStaleAndObservationRestoresIt(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	_ = m.Observe("memory_admission_headroom_ratio", 0.2, c.now())
	if !m.MarkPropertyStale("memory_admission_headroom_ratio", "request listing failed", "requests") {
		t.Fatal("fresh property was not marked stale")
	}
	if p, _ := m.Property("memory_admission_headroom_ratio"); p.Status != Stale {
		t.Fatalf("status %s; want stale", p.Status)
	}
	if m.MarkPropertyStale("memory_admission_headroom_ratio", "still unavailable", "requests") {
		t.Fatal("already stale property reported a new transition")
	}
	c.advance(time.Second)
	if err := m.Record(Observation{ID: "memory_admission_headroom_ratio", Value: 0.3, At: c.now()}); err != nil {
		t.Fatal(err)
	}
	if p, _ := m.Property("memory_admission_headroom_ratio"); p.Status != Active {
		t.Fatalf("status after new sample %s; want active", p.Status)
	}
}

func TestDerivedGoesStaleWhenNoMemberIsActiveAndReturnsWithThem(t *testing.T) {
	m, c := newTestMap(t, Config{StaleAfter: 30 * time.Second, ConvergenceObservations: 2})
	_ = m.DeclareProperty(Property{ID: "cpu", Range: [2]float64{0, 1}})
	_ = m.DeclareProperty(Property{ID: "RC", Kind: Derived, Members: []string{"cpu"}})
	_ = m.Observe("cpu", .6, c.now())
	if d, _ := m.Property("RC"); d.Status != Active || d.Value != .6 {
		t.Fatalf("setup: %+v", d)
	}
	// A quiet node: nothing observes, only the sweep runs.
	c.advance(time.Minute)
	m.Sweep()
	d, _ := m.Property("RC")
	if d.Status != Stale || d.Confidence != 0 {
		t.Errorf("derived after all members went stale: status=%s confidence=%.2f; want stale, 0", d.Status, d.Confidence)
	}
	if d.NObservations == 0 {
		t.Error("going stale must not erase how much the summary had rested on")
	}
	c.advance(time.Second)
	_ = m.Observe("cpu", .7, c.now())
	if d, _ = m.Property("RC"); d.Status != Active {
		t.Errorf("derived did not return to active with its member: %+v", d)
	}
}

func TestResetRelationshipReturnsTheClaimToUnknown(t *testing.T) {
	m, c := newTestMap(t, Config{Learn: true, LearnConfig: LearnConfig{PairWindowSeconds: 5, MinSupport: 3, Window: 10}})
	_ = m.DeclareProperty(Property{ID: "a"})
	_ = m.DeclareProperty(Property{ID: "b"})
	_ = m.DeclareRelationship(Relationship{From: "a", To: "b", Sign: 1, Label: "L"})
	for i := 0; i < 6; i++ {
		x := float64(i) / 10
		_ = m.ObserveEvent("a", x, c.now(), "a"+strconv.Itoa(i))
		_ = m.ObserveEvent("b", x*0.9, c.now(), "b"+strconv.Itoa(i))
		c.advance(time.Second)
	}
	id := RelationshipID("a", "b", "L")
	if r, _ := m.Relationship(id); r.Established == nil {
		t.Fatal("setup: established layer never formed")
	}
	if err := m.ResetRelationship(id, "operator", "test"); err != nil {
		t.Fatal(err)
	}
	r, _ := m.Relationship(id)
	if _, known := r.Effective(); known || r.Established != nil || r.SignAgreements != 0 || r.SignConflicts != 0 {
		t.Errorf("after reset: %+v; want no effective strength, no established layer, no sign tally", r)
	}
}

func TestDecisionCaveatsAStaleEndpoint(t *testing.T) {
	m, c := newTestMap(t, Config{StaleAfter: 10 * time.Second, AdmitUnknown: true})
	_ = m.Observe("src", .5, c.now())
	_ = m.Observe("dst", .5, c.now())
	_ = m.DeclareRelationship(Relationship{From: "src", To: "dst", Sign: 1})
	c.advance(time.Minute)
	_ = m.Observe("dst", .5, c.now())
	m.Sweep()
	b := m.Decide("d1", "estimate", "test")
	b.RelationshipsInto("dst")
	d := b.Commit(nil)
	var found bool
	for _, cv := range d.Caveats {
		if strings.Contains(cv, "has an endpoint src that is stale") {
			found = true
		}
	}
	if !found {
		t.Errorf("caveats %v; want one naming the stale endpoint", d.Caveats)
	}

	// Both endpoints stale: the reader is owed both names, not whichever the map
	// iteration happened to write last.
	c.advance(time.Minute)
	m.Sweep()
	b = m.Decide("d2", "estimate", "test")
	b.RelationshipsInto("dst")
	d = b.Commit(nil)
	var stale []string
	for _, cv := range d.Caveats {
		if strings.Contains(cv, "that is stale") {
			stale = append(stale, cv)
		}
	}
	if len(stale) != 2 {
		t.Fatalf("caveats %v; want one per stale endpoint (2)", d.Caveats)
	}
	var named int
	for _, end := range []string{"src", "dst"} {
		for _, cv := range stale {
			if strings.Contains(cv, "has an endpoint "+end+" that is stale") {
				named++
			}
		}
	}
	if named != 2 {
		t.Errorf("stale caveats %v; want one naming src and one naming dst", stale)
	}
}

func TestCoveredIgnoresRetiredAndOppositeSign(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	_ = m.Observe("a", 1, c.now())
	_ = m.Observe("b", 1, c.now())
	_ = m.DeclareRelationship(Relationship{From: "a", To: "b", Sign: 1, Label: "L"})
	if !m.Covered("a", "b", 1) {
		t.Error("a declared active relationship must count as covered")
	}
	if m.Covered("a", "b", -1) || m.Covered("b", "a", 1) {
		t.Error("the opposite sign and the reverse direction are different claims")
	}
	_ = m.RetireRelationship(RelationshipID("a", "b", "L"), "test", "operator")
	if m.Covered("a", "b", 1) {
		t.Error("a retired relationship does not cover the pair: structure must be able to re-earn its place")
	}
}

// TestDeclareRelationshipCarriesNoEvidence: a declaration asserts that a relationship
// exists and which way it runs. What it is worth is for the machine to say, so any
// strength, support or established value the caller supplies is dropped, and a
// provenance the map does not define is refused — otherwise POST /state/relationships
// can fabricate a learned edge.
func TestDeclareRelationshipCarriesNoEvidence(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	_ = m.Record(Observation{ID: "a", Value: 1, At: c.now()})
	_ = m.Record(Observation{ID: "b", Value: 1, At: c.now()})
	est, asserted := 0.95, 0.7
	err := m.DeclareRelationship(Relationship{From: "a", To: "b", Sign: 1, Recent: 0.9, Confidence: 1,
		NObservations: 500, Established: &est, Assertion: &asserted, SignAgreements: 400, SignConflicts: 3,
		Provenance: Discovered})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := m.Relationship("a->b")
	if _, known := r.Effective(); known {
		t.Errorf("a freshly declared relationship has an effective strength (%+v); a declaration carries no evidence", r)
	}
	if r.NObservations != 0 || r.Established != nil || r.Assertion != nil || r.SignAgreements != 0 ||
		r.SignConflicts != 0 || r.Recent != 0 || r.Confidence != 0 {
		t.Errorf("caller-supplied evidence survived the declaration: %+v", r)
	}
	if err := m.DeclareRelationship(Relationship{From: "b", To: "a", Sign: 1, Provenance: "learned"}); err == nil {
		t.Error("provenance \"learned\" was accepted at declaration; the map decides what is learned")
	}
}

// TestReadPathsCopyLabelsAndMembers: a view, a Property and a committed decision are
// copies. Labels merge in place on a later observation, so a shallow copy would let a
// journaled decision change after the fact and let a caller write into the map.
func TestReadPathsCopyLabelsAndMembers(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	_ = m.Record(Observation{ID: "a@pod:1", Value: 1, At: c.now(), Subject: "pod:1",
		Labels: map[string]string{"qos": "burstable"}})
	b := m.Decide("d1", "estimate", "what was qos when I decided")
	_, _ = b.Property("a@pod:1")
	d := b.Commit(nil)
	c.advance(time.Second)
	_ = m.Record(Observation{ID: "a@pod:1", Value: 2, At: c.now(), Subject: "pod:1",
		Labels: map[string]string{"qos": "guaranteed"}})
	if got := d.PropertiesRead[0].Labels["qos"]; got != "burstable" {
		t.Errorf("the decision's recorded labels read %q after a later observation; the record aliases the live map", got)
	}
	v := m.State(Query{})
	v.Properties[0].Labels["qos"] = "hacked"
	p, _ := m.Property("a@pod:1")
	p.Labels["extra"] = "x"
	live, _ := m.Property("a@pod:1")
	if live.Labels["qos"] == "hacked" || live.Labels["extra"] != "" {
		t.Errorf("writing into a view or a returned Property reached the map: %v", live.Labels)
	}
	members := []string{"a@pod:1"}
	if err := m.DeclareProperty(Property{ID: "d", Kind: Derived, Members: members}); err != nil {
		t.Fatal(err)
	}
	members[0] = "zzz"
	dp, _ := m.Property("d")
	if dp.Members[0] != "a@pod:1" {
		t.Errorf("members alias the caller's slice: %v", dp.Members)
	}
}

// TestRecordRefusesAnInvalidRange: a declared range is what the estimator normalises
// by, so an inverted, empty or non-finite one is refused at the door, as the wire does.
func TestRecordRefusesAnInvalidRange(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	for _, rng := range [][2]float64{{1, 0}, {5, 5}, {math.NaN(), 1}, {0, math.Inf(1)}} {
		r := rng
		if err := m.Record(Observation{ID: "x", Value: 0.5, At: c.now(), Range: &r}); err == nil {
			t.Errorf("range %v was accepted", rng)
		}
	}
	if _, ok := m.Property("x"); ok {
		t.Error("a refused observation admitted the property anyway")
	}
}

// TestDeclarePropertyRefusesAnEmptyOrUndeclaredRange: [5,5] cannot normalise anything,
// and RangeDeclared without a range bypasses normalisation with no "assumed" caveat.
func TestDeclarePropertyRefusesAnEmptyOrUndeclaredRange(t *testing.T) {
	m, _ := newTestMap(t, Config{})
	if err := m.DeclareProperty(Property{ID: "a", Range: [2]float64{5, 5}}); err == nil {
		t.Error("empty range [5,5] was accepted")
	}
	if err := m.DeclareProperty(Property{ID: "b", RangeDeclared: true}); err == nil {
		t.Error("RangeDeclared without a range was accepted")
	}
}

// TestRecordRefusesASubjectThatDoesNotMatchTheId: the subject is part of the id
// (metric@subject), and its charset is what the HTTP surface can address.
func TestRecordRefusesASubjectThatDoesNotMatchTheId(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	for _, o := range []Observation{
		{ID: "cpu@pod:a", Subject: "pod:b"},
		{ID: "cpu", Subject: "pod:a"},
		{ID: "cpu@pod/evil", Subject: "pod/evil"},
	} {
		o.Value, o.At = 0.5, c.now()
		if err := m.Record(o); err == nil {
			t.Errorf("observation id=%q subject=%q was accepted", o.ID, o.Subject)
		}
	}
	if err := m.DeclareProperty(Property{ID: "x@pod/evil", Subject: "pod/evil"}); err == nil {
		t.Error("a declaration with an invalid subject was accepted")
	}
	if err := m.DeclareProperty(Property{ID: "x", Subject: "pod:a"}); err == nil {
		t.Error("a declaration whose subject is not in its id was accepted")
	}
}

// TestConflictingDeclarationIsRefusedNotAveraged: Record's own doc calls a differing
// subject, unit or range "a fault worth seeing rather than a value worth averaging";
// the value was being averaged. An app pushing percent against a share-of-node
// property would have moved the EMA by ~45×.
func TestConflictingDeclarationIsRefusedNotAveraged(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	share, percent := [2]float64{0, 1}, [2]float64{0, 100}
	_ = m.Record(Observation{ID: "cpu@pod:a", Value: 0.1, At: c.now(), Subject: "pod:a", Unit: "share", Range: &share})
	c.advance(time.Second)
	if err := m.Record(Observation{ID: "cpu@pod:a", Value: 50, At: c.now(), Subject: "pod:a", Unit: "percent", Range: &percent}); err == nil {
		t.Error("an observation whose unit and range contradict the property's was accepted")
	}
	p, _ := m.Property("cpu@pod:a")
	if p.NObservations != 1 || p.Value != 0.1 {
		t.Errorf("the contradicting value was averaged in: n=%d value=%v", p.NObservations, p.Value)
	}
	var conflict bool
	for _, e := range m.Journal().Events(0, 0) {
		if e.Kind == EventPropertyConflict && e.Target == "cpu@pod:a" {
			conflict = true
		}
	}
	if !conflict {
		t.Error("the conflict was not journaled")
	}
	// An observation of a scoped id that carries no subject is the same fault: it is
	// how an unscoped push named "cpu@pod:a" would land on the pod's property.
	c.advance(time.Second)
	if err := m.Observe("cpu@pod:a", 0.2, c.now()); err == nil {
		t.Error("an unscoped observation of a scoped property was accepted")
	}
}

// subjectOf returns the subject an id of the form <metric>@<subject> names.
func subjectOf(id string) string {
	_, subject, _ := strings.Cut(id, "@")
	return subject
}

// TestRetireHookRunsOutsideTheLock: the hook is allowed to read the map — the sweep
// hands it to the proposer, and a future hook will look at what was retired. A hook
// that calls back into the map would deadlock if it ran under the write lock, and
// the hook test that only appends to a slice would never notice.
func TestRetireHookRunsOutsideTheLock(t *testing.T) {
	m, c := newTestMap(t, Config{StaleAfter: 10 * time.Second, RetireAfter: 20 * time.Second, AdmitUnknown: true})
	var seen []Status
	m.SetRetireHook(func(id string) {
		p, _ := m.Property(id) // takes the read lock: hangs if the hook ran under the write lock
		seen = append(seen, p.Status)
	})
	_ = m.Observe("a", 1, c.now())
	_ = m.Observe("b", 1, c.now())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = m.RetireProperty("a", "test", "operator")
		c.advance(time.Minute)
		m.Sweep()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("retire hook deadlocked: it ran while the map's write lock was held")
	}
	if len(seen) != 2 || seen[0] != Retired || seen[1] != Retired {
		t.Errorf("hook read %v; want the retired status of both properties", seen)
	}
}

// TestRevivalKeepsCascadeRetiredRelationshipsRetired: a subject that returns is
// exhibited again; the structure that retired with it is not re-asserted by that
// alone — it re-earns its place through the proposer.
func TestRevivalKeepsCascadeRetiredRelationshipsRetired(t *testing.T) {
	m, c := newTestMap(t, Config{StaleAfter: 10 * time.Second, RetireAfter: 20 * time.Second, AdmitUnknown: true})
	_ = m.Record(Observation{ID: "cpu@pod:1", Value: .3, At: c.now(), Subject: "pod:1"})
	_ = m.Observe("pressure", .3, c.now())
	if err := m.DeclareRelationship(Relationship{From: "cpu@pod:1", To: "pressure", Sign: 1, Label: "d"}); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Minute)
	_ = m.Observe("pressure", .3, c.now()) // the node keeps reporting; only the pod goes silent
	m.Sweep()
	if r, _ := m.Relationship(RelationshipID("cpu@pod:1", "pressure", "d")); r.Status != Retired {
		t.Fatalf("setup: relationship %s, want retired by the cascade", r.Status)
	}
	c.advance(time.Second)
	_ = m.Record(Observation{ID: "cpu@pod:1", Value: .4, At: c.now(), Subject: "pod:1"})
	if p, _ := m.Property("cpu@pod:1"); p.Status != Active {
		t.Fatalf("the property did not revive: %s", p.Status)
	}
	if r, _ := m.Relationship(RelationshipID("cpu@pod:1", "pressure", "d")); r.Status != Retired {
		t.Errorf("relationship %s after the property revived; want it to stay retired until re-earned", r.Status)
	}
}

// TestRetireHookPanicDoesNotKillTheSweep: the hook runs outside the lock on the
// sweep goroutine; a panic in it must not take the sweep down or skip the
// properties after it.
func TestRetireHookPanicDoesNotKillTheSweep(t *testing.T) {
	m, c := newTestMap(t, Config{StaleAfter: 10 * time.Second, RetireAfter: 20 * time.Second, AdmitUnknown: true})
	var calls []string
	m.SetRetireHook(func(id string) {
		calls = append(calls, id)
		if id == "a" {
			panic("hook bug")
		}
	})
	_ = m.Observe("a", 1, c.now())
	_ = m.Observe("b", 1, c.now())
	c.advance(time.Minute)
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("a panicking hook escaped Sweep: %v", r)
			}
		}()
		m.Sweep()
	}()
	if len(calls) != 2 {
		t.Errorf("hook calls %v; want both properties despite the panic on the first", calls)
	}
}

// TestDeclarePropertyRefusesAnUnknownStatusAndAScopedDerived: Status is a closed
// set, and a derived property is node-level structure — a summary scoped to one
// subject would be a subject summarising itself.
func TestDeclarePropertyRefusesAnUnknownStatusAndAScopedDerived(t *testing.T) {
	m, _ := newTestMap(t, Config{AdmitUnknown: true})
	if err := m.DeclareProperty(Property{ID: "x", Status: "bogus"}); err == nil {
		t.Error("status \"bogus\" was accepted")
	}
	_ = m.DeclareProperty(Property{ID: "cpu@pod:a", Subject: "pod:a"})
	if err := m.DeclareProperty(Property{ID: "sum@pod:a", Subject: "pod:a", Kind: Derived, Members: []string{"cpu@pod:a"}}); err == nil {
		t.Error("a derived property scoped to a subject was accepted")
	}
}

func TestSubjectsListsTheLiveOnes(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	for _, s := range []string{"pod:b", "pod:a", "pod:gone"} {
		for _, metric := range []string{"cpu", "mem"} {
			if err := m.Record(Observation{ID: metric + "@" + s, Value: 1, At: c.now(), Subject: s}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := m.Record(Observation{ID: "node_cpu", Value: 1, At: c.now()}); err != nil {
		t.Fatal(err)
	}
	for _, metric := range []string{"cpu", "mem"} {
		if err := m.RetireProperty(metric+"@pod:gone", "pod deleted", "operator:ada"); err != nil {
			t.Fatal(err)
		}
	}
	got := m.Subjects()
	if len(got) != 2 || got[0] != "pod:a" || got[1] != "pod:b" {
		t.Errorf("Subjects() = %v, want [pod:a pod:b]: sorted, distinct, no node scope, no retired subject", got)
	}
}

func TestStaleAfterIsTheConfiguredSilence(t *testing.T) {
	if got := New(Config{}, nil).StaleAfter(); got != 2*time.Minute {
		t.Errorf("default %s; want 2m", got)
	}
	if got := New(Config{StaleAfter: 30 * time.Second}, nil).StaleAfter(); got != 30*time.Second {
		t.Errorf("configured %s; want 30s", got)
	}
}

func TestDecisionInputsAndFactsAreCopiedNotAliased(t *testing.T) {
	m, c := newTestMap(t, Config{StaleAfter: time.Minute})
	_ = c
	if err := m.RecordFacts("test", []Fact{NumberFact("cpu_cores", 4, "cores")}); err != nil {
		t.Fatal(err)
	}
	in := map[string]any{"nested": map[string]any{"k": 1.0}, "list": []any{1.0}}
	b := m.Decide("dec-copy", "estimate", "q")
	b.Input("evidence", in)
	b.Facts()
	b.Commit(map[string]any{"ok": true})

	in["nested"].(map[string]any)["k"] = 2.0
	in["list"].([]any)[0] = 9.0

	d, ok := m.Journal().Decision("dec-copy")
	if !ok {
		t.Fatal("decision not found")
	}
	ev := d.Inputs["evidence"].(map[string]any)
	if ev["nested"].(map[string]any)["k"] != 1.0 || ev["list"].([]any)[0] != 1.0 {
		t.Fatalf("caller's later write reached the journal: %v", ev)
	}
	if len(d.FactsRead) == 0 || d.FactsRead[0].Number == nil {
		t.Fatalf("facts not read: %+v", d.FactsRead)
	}
	*d.FactsRead[0].Number = 99
	ev["nested"].(map[string]any)["k"] = 3.0

	again, _ := m.Journal().Decision("dec-copy")
	if *again.FactsRead[0].Number != 4 {
		t.Fatalf("returned fact aliased: %v", *again.FactsRead[0].Number)
	}
	if again.Inputs["evidence"].(map[string]any)["nested"].(map[string]any)["k"] != 1.0 {
		t.Fatal("returned input aliased")
	}
}

// ── A reading must be newer than the last, and not from the future ─────────────

func TestAReadingOlderThanTheLastIsRefusedAndCounted(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	counter := func() int { return m.State(Query{}).Counts.ReadingsOutOfOrder }

	if err := m.Observe("cpu", 0.1, c.now()); err != nil {
		t.Fatal(err)
	}
	rev := m.Revision()
	// Unix-second stamps: a sub-second producer legitimately repeats an instant.
	if err := m.Observe("cpu", 0.9, c.now()); err != nil {
		t.Fatal(err)
	}
	p, _ := m.Property("cpu")
	if p.NObservations != 2 || counter() != 0 || m.Revision() == rev {
		t.Errorf("equal-instant reading: n=%d counter=%d; want 2 and 0, revision advanced", p.NObservations, counter())
	}

	rev = m.Revision()
	if err := m.Observe("cpu", 0.5, c.now().Add(-time.Second)); err != nil {
		t.Fatalf("a refused reading is not an error: %v", err)
	}
	q, _ := m.Property("cpu")
	if q.NObservations != 2 || q.Value != p.Value || !q.LastObserved.Equal(c.now()) || counter() != 1 {
		t.Errorf("older reading changed the property or was not counted: %+v counter=%d", q, counter())
	}
	if m.Revision() != rev {
		t.Error("a refused reading advanced the revision")
	}

	c.advance(time.Second)
	if err := m.Observe("cpu", 0.9, c.now()); err != nil {
		t.Fatal(err)
	}
	q, _ = m.Property("cpu")
	if q.NObservations != 3 || counter() != 1 || m.Revision() == rev {
		t.Errorf("newer reading: n=%d counter=%d; want 3 and 1", q.NObservations, counter())
	}
}

func TestAFutureReadingIsRefusedAndCounted(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})

	if err := m.Observe("gpu", 0.5, c.now().Add(16*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Property("gpu"); ok {
		t.Error("a future-stamped reading admitted a property")
	}
	if got := m.State(Query{}).Counts.ReadingsFuture; got != 1 {
		t.Errorf("ReadingsFuture = %d; want 1", got)
	}

	if err := m.Observe("gpu", 0.5, c.now().Add(15*time.Second)); err != nil {
		t.Fatal(err)
	}
	p, ok := m.Property("gpu")
	if !ok || p.NObservations != 1 {
		t.Errorf("a reading at the tolerance edge was not admitted: %+v ok=%v", p, ok)
	}
	if got := m.State(Query{}).Counts.ReadingsFuture; got != 1 {
		t.Errorf("ReadingsFuture = %d; want still 1", got)
	}
}

func TestARefusedReadingDoesNotConsumeItsEventID(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	t0 := c.now()

	if err := m.ObserveEvent("cpu", 0.1, t0, "e1"); err != nil {
		t.Fatal(err)
	}
	if err := m.ObserveEvent("cpu", 0.5, t0.Add(-time.Second), "e2"); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Second)
	if err := m.ObserveEvent("cpu", 0.5, c.now(), "e2"); err != nil {
		t.Fatal(err)
	}
	p, _ := m.Property("cpu")
	if p.NObservations != 2 {
		t.Errorf("n = %d; want 2: the refused reading consumed event id e2", p.NObservations)
	}
}

// ── Silence runs on the process's monotonic clock ─────────────────────────────

func clockSteps(m *Map) []Event {
	var out []Event
	for _, e := range m.Journal().Events(0, 0) {
		if e.Kind == EventClockStepped {
			out = append(out, e)
		}
	}
	return out
}

func observedDiskIO(t *testing.T) (*Map, *clock) {
	t.Helper()
	m, c := newTestMap(t, Config{StaleAfter: 30 * time.Second, RetireAfter: 2 * time.Minute})
	if err := m.DeclareProperty(Property{ID: "disk_io"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Observe("disk_io", 0.1, c.now()); err != nil {
		t.Fatal(err)
	}
	return m, c
}

func TestAWallClockStepForwardRetiresNothing(t *testing.T) {
	m, c := observedDiskIO(t)
	c.advance(5 * time.Second)
	c.stepWall(2 * time.Hour)

	stale, retired := m.Sweep()
	if len(stale) != 0 || len(retired) != 0 {
		t.Fatalf("a 2h wall step made stale=%v retired=%v; 5s of real silence is neither", stale, retired)
	}
	if p, _ := m.Property("disk_io"); p.Status != Active {
		t.Errorf("status %s after a wall step, want active", p.Status)
	}
	steps := clockSteps(m)
	if len(steps) != 1 {
		t.Fatalf("%d clock.stepped events, want 1", len(steps))
	}
	if w, _ := steps[0].Detail["wall_delta_seconds"].(float64); math.Abs(w-7205) > 1 {
		t.Errorf("wall_delta_seconds %v, want about 7205", steps[0].Detail["wall_delta_seconds"])
	}

	c.advance(time.Second)
	m.Sweep()
	if n := len(clockSteps(m)); n != 1 {
		t.Errorf("%d clock.stepped events after a quiet sweep, want still 1: one event per step", n)
	}
}

func TestAWallClockStepBackwardRetiresNothing(t *testing.T) {
	m, c := observedDiskIO(t)
	c.advance(5 * time.Second)
	c.stepWall(-time.Hour)

	stale, retired := m.Sweep()
	if len(stale) != 0 || len(retired) != 0 {
		t.Fatalf("a backward wall step made stale=%v retired=%v", stale, retired)
	}
	steps := clockSteps(m)
	if len(steps) != 1 {
		t.Fatalf("%d clock.stepped events, want 1", len(steps))
	}
	if w, _ := steps[0].Detail["wall_delta_seconds"].(float64); w >= 0 {
		t.Errorf("wall_delta_seconds %v, want negative", w)
	}
}

func TestAnOldStampedReadingIsAlreadyStale(t *testing.T) {
	m, c := newTestMap(t, Config{StaleAfter: 30 * time.Second, RetireAfter: 2 * time.Minute})
	if err := m.DeclareProperty(Property{ID: "disk_io"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Observe("disk_io", 0.1, c.now().Add(-31*time.Second)); err != nil {
		t.Fatal(err)
	}
	stale, retired := m.Sweep()
	if len(stale) != 1 || len(retired) != 0 {
		t.Fatalf("stale=%v retired=%v; a reading stamped 31s ago is stale on arrival", stale, retired)
	}
	// A wall step does not change that, nor un-stale it.
	c.stepWall(time.Hour)
	if _, retired = m.Sweep(); len(retired) != 0 {
		t.Errorf("retired=%v after a wall step", retired)
	}
}

func TestOrdinarySilenceStillRetires(t *testing.T) {
	m, c := observedDiskIO(t)
	c.advance(3 * time.Minute)
	_, retired := m.Sweep()
	if len(retired) != 1 || retired[0] != "disk_io" {
		t.Fatalf("retired=%v after 3m of silence, want disk_io", retired)
	}
	if n := len(clockSteps(m)); n != 0 {
		t.Errorf("%d clock.stepped events with both clocks agreeing, want 0", n)
	}
}

func TestRelationshipsSurviveAClockStep(t *testing.T) {
	m, c := newTestMap(t, Config{StaleAfter: 30 * time.Second, RetireAfter: 2 * time.Minute})
	for _, id := range []string{"a", "b"} {
		if err := m.DeclareProperty(Property{ID: id}); err != nil {
			t.Fatal(err)
		}
		if err := m.Observe(id, 0.5, c.now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.DeclareRelationship(Relationship{From: "a", To: "b", Sign: 1}); err != nil {
		t.Fatal(err)
	}
	c.advance(5 * time.Second)
	c.stepWall(48 * time.Hour)
	m.Sweep()
	r, _ := m.Relationship(RelationshipID("a", "b", ""))
	if r.Status == Retired {
		t.Errorf("relationship retired by a wall-clock step: %s", r.RetiredReason)
	}
}

// ── A restored map gets retire_after of grace ─────────────────────────────────

// restoredMap saves a map holding two observed properties and a relationship, and
// loads it into a second map whose clock is three days later.
func restoredMap(t *testing.T) (*Map, *clock) {
	t.Helper()
	cfg := Config{Owner: "n", StaleAfter: 30 * time.Second, RetireAfter: 2 * time.Minute}
	a, cA := newTestMap(t, cfg)
	for _, id := range []string{"cpu", "mem"} {
		if err := a.DeclareProperty(Property{ID: id}); err != nil {
			t.Fatal(err)
		}
		if err := a.Observe(id, 0.5, cA.now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.DeclareRelationship(Relationship{From: "cpu", To: "mem", Sign: 1}); err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/state.json"
	if err := a.Save(path); err != nil {
		t.Fatal(err)
	}

	b, cB := newTestMap(t, cfg)
	cB.t = cA.t.Add(72 * time.Hour)
	cB.m = time.Second
	if r, err := b.Load(path); err != nil || !r.Loaded {
		t.Fatalf("load: %v %v", r.Loaded, err)
	}
	return b, cB
}

func TestRestoreGraceMarksStaleButDoesNotRetire(t *testing.T) {
	m, _ := restoredMap(t)
	stale, retired := m.Sweep()
	if len(stale) != 2 || stale[0] != "cpu" || stale[1] != "mem" || len(retired) != 0 {
		t.Fatalf("stale=%v retired=%v; want both stale, none retired during grace", stale, retired)
	}
	r, _ := m.Relationship(RelationshipID("cpu", "mem", ""))
	if r.Status == Retired {
		t.Errorf("relationship retired during grace: %s", r.RetiredReason)
	}
	if n := m.State(Query{}).Counts.RelationshipsLive; n != 1 {
		t.Errorf("RelationshipsLive %d, want 1", n)
	}
}

func TestRestoreGraceEndsAfterRetireAfter(t *testing.T) {
	m, c := restoredMap(t)
	m.Sweep()
	c.advance(2*time.Minute + time.Second)
	_, retired := m.Sweep()
	if len(retired) != 2 || retired[0] != "cpu" || retired[1] != "mem" {
		t.Fatalf("retired=%v after grace, want cpu and mem", retired)
	}
	if r, _ := m.Relationship(RelationshipID("cpu", "mem", "")); r.Status != Retired {
		t.Errorf("relationship %s after grace, want retired", r.Status)
	}
}

func TestAnObservationDuringGraceRevivesNormally(t *testing.T) {
	m, c := restoredMap(t)
	m.Sweep()
	c.advance(10 * time.Second)
	if err := m.Observe("cpu", 0.6, c.now()); err != nil {
		t.Fatal(err)
	}
	if p, _ := m.Property("cpu"); p.Status != Active {
		t.Fatalf("cpu %s after an observation, want active", p.Status)
	}
	c.advance(2 * time.Minute)
	_, retired := m.Sweep()
	if len(retired) != 1 || retired[0] != "mem" {
		t.Fatalf("retired=%v, want mem only", retired)
	}
	if p, _ := m.Property("cpu"); p.Status != Stale {
		t.Errorf("cpu %s, want stale", p.Status)
	}
}

func TestAClockStepDuringGraceRetiresNothing(t *testing.T) {
	m, c := restoredMap(t)
	c.advance(5 * time.Second)
	c.stepWall(6 * time.Hour)
	_, retired := m.Sweep()
	if len(retired) != 0 {
		t.Fatalf("retired=%v after a wall step during grace", retired)
	}
	if n := len(clockSteps(m)); n != 1 {
		t.Errorf("%d clock.stepped events, want 1", n)
	}
	c.advance(2 * time.Minute)
	if _, retired = m.Sweep(); len(retired) != 2 {
		t.Errorf("retired=%v once grace elapsed, want both", retired)
	}
}

// ── Reading order is judged on the monotonic clock ────────────────────────────

func TestReadingsAfterABackwardWallStepKeepThePropertyLive(t *testing.T) {
	m, c := observedDiskIO(t)
	c.advance(10 * time.Second)
	c.stepWall(-time.Hour)
	// The producer stamps with the stepped clock: every reading is an hour older
	// than LastObserved by wall time, yet each is newer than the last.
	for i := 0; i < 20; i++ { // 200 s, past retire_after
		if err := m.Observe("disk_io", 0.2, c.now()); err != nil {
			t.Fatal(err)
		}
		m.Sweep()
		c.advance(10 * time.Second)
	}
	if p, _ := m.Property("disk_io"); p.Status != Active || p.NObservations != 21 {
		t.Errorf("after a backward step: status %s, n=%d; want active and 21", p.Status, p.NObservations)
	}
	if n := m.State(Query{}).Counts.ReadingsOutOfOrder; n != 0 {
		t.Errorf("readings_out_of_order = %d; want 0", n)
	}
}

func TestARestoredPropertyAcceptsReadingsStampedBeforeItsLastObserved(t *testing.T) {
	m, c := restoredMap(t)
	// A board whose RTC came up in 1970: its readings predate the snapshot.
	c.t = time.Unix(1000, 0).UTC()
	if err := m.Observe("cpu", 0.7, c.now()); err != nil {
		t.Fatal(err)
	}
	c.advance(10 * time.Second)
	c.stepWall(56 * 365 * 24 * time.Hour) // NTP fixes the clock
	if err := m.Observe("cpu", 0.8, c.now()); err != nil {
		t.Fatal(err)
	}
	if p, _ := m.Property("cpu"); p.NObservations != 3 || p.Status != Active {
		t.Errorf("cpu n=%d status %s; want 3 and active", p.NObservations, p.Status)
	}
	if n := m.State(Query{}).Counts.ReadingsOutOfOrder; n != 0 {
		t.Errorf("readings_out_of_order = %d; want 0", n)
	}
}

// Two clocks read one after the other never agree to the nanosecond, so a repeated
// stamp lands a hair before the last anchor; it is still the same instant.
func TestARepeatedStampAcrossClockReadJitterIsAccepted(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	at := c.now()
	if err := m.Observe("cpu", 0.1, at); err != nil {
		t.Fatal(err)
	}
	c.stepWall(time.Millisecond) // wall read later than mono, relative to the last record
	if err := m.Observe("cpu", 0.2, at); err != nil {
		t.Fatal(err)
	}
	if p, _ := m.Property("cpu"); p.NObservations != 2 {
		t.Errorf("n=%d; a repeated stamp was refused as out of order", p.NObservations)
	}
}

// signStreamMap builds the sign-suspect setup: two relationships over resource ->
// pressure, "backwards" declared with the wrong sign for a stream that rises together.
func signStreamMap(t *testing.T, rels ...Relationship) (*Map, *clock) {
	t.Helper()
	m, c := newTestMap(t, Config{
		ConvergenceObservations: 20,
		Alpha:                   0.5,
		Learn:                   true,
		LearnConfig:             LearnConfig{PairWindowSeconds: 15, MinSupport: 8, Window: 40},
	})
	for _, id := range []string{"resource", "pressure"} {
		if err := m.DeclareProperty(Property{ID: id, Range: [2]float64{0, 1}}); err != nil {
			t.Fatal(err)
		}
	}
	if rels == nil {
		rels = []Relationship{
			{From: "resource", To: "pressure", Label: "backwards", Sign: -1},
			{From: "resource", To: "pressure", Label: "correct", Sign: 1},
		}
	}
	for _, r := range rels {
		if err := m.DeclareRelationship(r); err != nil {
			t.Fatal(err)
		}
	}
	return m, c
}

// feedSignPairs feeds n pairs; pressure rises with resource, or falls with it if inverse.
func feedSignPairs(t *testing.T, m *Map, c *clock, from, n int, inverse bool) {
	t.Helper()
	for i := from; i < from+n; i++ {
		c.advance(5 * time.Second)
		x := float64(i%10) / 10.0
		if err := m.ObserveEvent("resource", x, c.now(), "r"+itoa(i)); err != nil {
			t.Fatal(err)
		}
		c.advance(2 * time.Second)
		y := 0.9*x + 0.02
		if inverse {
			y = 0.9 * (1 - x)
		}
		if err := m.ObserveEvent("pressure", y, c.now(), "p"+itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
}

func retiredEventsFor(m *Map, id string) []Event {
	var out []Event
	for _, e := range m.Journal().Events(0, 0) {
		if e.Kind == EventRelationshipRetired && e.Target == id {
			out = append(out, e)
		}
	}
	return out
}

func TestSignContradictedSeededRelationshipIsRetired(t *testing.T) {
	m, c := signStreamMap(t)
	feedSignPairs(t, m, c, 0, 120, false)
	id := RelationshipID("resource", "pressure", "backwards")
	backwards, _ := m.Relationship(id)
	correct, _ := m.Relationship(RelationshipID("resource", "pressure", "correct"))
	if backwards.Status != Retired || backwards.RetiredReason != ReasonSignContradicted || !backwards.SignSuspectFlag {
		t.Fatalf("backwards = %v %q flag=%v", backwards.Status, backwards.RetiredReason, backwards.SignSuspectFlag)
	}
	if correct.Status != Active {
		t.Errorf("correct status = %v", correct.Status)
	}
	evs := retiredEventsFor(m, id)
	if len(evs) != 1 {
		t.Fatalf("retired events = %d, want 1", len(evs))
	}
	e := evs[0]
	if e.Actor != "agent" || e.Detail["reason"] != "sign-contradicted" ||
		e.Detail["conflicts"].(int) < 30 || e.Detail["agreements"] != 0 || e.Detail["provenance"] != "learned" {
		t.Errorf("event = %+v", e)
	}
	if got := m.Census().RelationshipsLive; got != 1 {
		t.Errorf("live = %d, want 1", got)
	}
}

func TestSignContradictedAssertedRelationshipIsOnlyFlagged(t *testing.T) {
	m, c := signStreamMap(t)
	id := RelationshipID("resource", "pressure", "backwards")
	if err := m.AssertRelationshipStrength(id, 0.5, "op", "test"); err != nil {
		t.Fatal(err)
	}
	feedSignPairs(t, m, c, 0, 120, false)
	backwards, _ := m.Relationship(id)
	if backwards.Status != Active || !backwards.SignSuspectFlag {
		t.Fatalf("asserted: status=%v flag=%v, want active and flagged", backwards.Status, backwards.SignSuspectFlag)
	}
	if n := len(retiredEventsFor(m, id)); n != 0 {
		t.Errorf("%d retired events for an asserted relationship", n)
	}
}

func TestSignContradictedIntervenedIsRetiredAndKeepsInterventions(t *testing.T) {
	m, c := signStreamMap(t, Relationship{From: "resource", To: "pressure", Label: "backwards", Sign: 1})
	id := RelationshipID("resource", "pressure", "backwards")
	for i := 0; i < 2; i++ {
		if _, _, err := m.RecordIntervention(InterventionOutcome{
			From: "resource", To: "pressure", Agreed: true, HasEffect: true, Effect: 0.6,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if r, _ := m.Relationship(id); r.Provenance != Intervened {
		t.Fatalf("precondition: provenance = %v", r.Provenance)
	}
	feedSignPairs(t, m, c, 0, 120, true)
	r, _ := m.Relationship(id)
	if r.Status != Retired || r.RetiredReason != ReasonSignContradicted {
		t.Fatalf("status=%v reason=%q", r.Status, r.RetiredReason)
	}
	evs := retiredEventsFor(m, id)
	if len(evs) != 1 || evs[0].Detail["provenance"] != "intervened" {
		t.Fatalf("events = %+v", evs)
	}
	if r.Interventions == nil || r.Interventions.EffectN != 2 {
		t.Errorf("interventions = %+v, want EffectN 2", r.Interventions)
	}
}

func TestSignRetiredRelationshipRedeclaredStaysActive(t *testing.T) {
	m, c := signStreamMap(t)
	feedSignPairs(t, m, c, 0, 120, false)
	id := RelationshipID("resource", "pressure", "backwards")
	before, _ := m.Relationship(id)
	if before.Status != Retired {
		t.Fatalf("precondition: status = %v", before.Status)
	}
	if err := m.DeclareRelationship(Relationship{From: "resource", To: "pressure", Label: "backwards", Sign: -1}); err != nil {
		t.Fatal(err)
	}
	r, _ := m.Relationship(id)
	if r.Status != Active || r.SignConflicts != before.SignConflicts || !r.SignSuspectFlag {
		t.Fatalf("redeclared: status=%v conflicts=%d (was %d) flag=%v",
			r.Status, r.SignConflicts, before.SignConflicts, r.SignSuspectFlag)
	}
	feedSignPairs(t, m, c, 120, 10, false)
	if r, _ = m.Relationship(id); r.Status != Active {
		t.Fatalf("re-retired on a repeat: status = %v", r.Status)
	}
	if err := m.ResetRelationship(id, "op", "fresh trial"); err != nil {
		t.Fatal(err)
	}
	feedSignPairs(t, m, c, 130, 120, false)
	if r, _ = m.Relationship(id); r.Status != Retired {
		t.Fatalf("after reset and a fresh trial: status = %v, want retired", r.Status)
	}
	if n := len(retiredEventsFor(m, id)); n != 2 {
		t.Errorf("retired events = %d, want 2", n)
	}
}

// ── A relationship says when the present differs from what is established ─────

// effectiveBand is two standard errors of Fisher's z at the effective window of n
// pairs smoothed with EMA weight alpha: n_eff = n·(1 − φ²)/(1 + φ²), φ = 1 − alpha.
func effectiveBand(n int, alpha float64) float64 {
	phi := 1 - alpha
	nEff := float64(n) * (1 - phi*phi) / (1 + phi*phi)
	return 2 / math.Sqrt(math.Max(nEff-3, 1))
}

// regimeOutside is the regime rule recomputed from a relationship as a reader sees
// it: the recent layer more than effectiveBand away from the established one.
func regimeOutside(m *Map, r Relationship) bool {
	if r.Established == nil {
		return false
	}
	z := func(v float64) float64 { return math.Atanh(math.Min(math.Abs(v), 0.9999)) }
	return math.Abs(z(r.Recent)-z(*r.Established)) > effectiveBand(m.PairSupport(r.ID), m.cfg.Alpha)
}

// driveRegime feeds opposed pairs one at a time until the relationship's Regime
// flips, and returns how many it took.
func driveRegime(t *testing.T, m *Map, c *clock, id string, limit int) int {
	t.Helper()
	r, _ := m.Relationship(id)
	start := r.Regime
	tag := "drive" + itoa(int(c.now().Unix())) + "-" // unique per call: the clock only advances
	for i := 0; i < limit; i++ {
		feedOpposed(t, m, c, i, tag)
		if r, _ := m.Relationship(id); r.Regime != start {
			return i + 1
		}
	}
	t.Fatalf("regime still %v after %d pairs", start, limit)
	return 0
}

func lastEvent(m *Map) Event {
	ev := m.Journal().Events(0, 0)
	return ev[len(ev)-1]
}

// stepRegime makes observations one at a time — each folds one pair — and checks the
// run rule stepwise: Regime flips exactly on the fold that completes a full window of
// consecutive folds on the new side.
func stepRegime(t *testing.T, m *Map, c *clock, id string, y func(float64) float64, tag string) {
	t.Helper()
	r0, _ := m.Relationship(id)
	want := !r0.Regime
	run := 0
	for i := 0; i < 400; i++ {
		observeHalf(t, m, c, i, y, tag)
		r, _ := m.Relationship(id)
		if regimeOutside(m, r) == want {
			run++
		} else {
			run = 0
		}
		window := m.PairSupport(id)
		if run < window {
			if r.Regime != r0.Regime {
				t.Fatalf("observation %d: regime flipped after %d folds on the new side; want %d", i, run, window)
			}
			continue
		}
		if r.Regime != want {
			t.Fatalf("observation %d: %d folds on the new side and regime is still %v", i, run, r.Regime)
		}
		if want && !r.RegimeSince.Equal(c.now()) {
			t.Fatalf("regime since %v; want the fold that started it, %v", r.RegimeSince, c.now())
		}
		if !want && !r.RegimeSince.IsZero() {
			t.Fatalf("regime ended but since is %v", r.RegimeSince)
		}
		e := lastEvent(m)
		if e.Kind != EventRelationshipRegime || e.Target != id || e.Detail["started"] != want {
			t.Fatalf("last event %+v; want relationship.regime started=%v", e, want)
		}
		for _, k := range []string{"recent", "established", "band"} {
			if _, ok := e.Detail[k]; !ok {
				t.Fatalf("regime event lacks %s: %v", k, e.Detail)
			}
		}
		// Window 40 at alpha 0.2: n_eff = 40·0.36/1.64 ≈ 8.78, band 2/sqrt(n_eff − 3).
		if b, _ := e.Detail["band"].(float64); math.Abs(b-2/math.Sqrt(40*0.36/1.64-3)) > 1e-9 {
			t.Fatalf("band %v; want 2/sqrt(n_eff − 3) at window 40, alpha 0.2", e.Detail["band"])
		}
		return
	}
	t.Fatalf("regime never flipped to %v", want)
}

func TestRegimeStartsAfterAWindowOfFoldsOutsideTheBand(t *testing.T) {
	m, c, id := newLearningPairWindow(t, 0.001, 40)
	feedPairs(t, m, c, 300, 1.0, "strong")
	if r, _ := m.Relationship(id); r.Regime {
		t.Fatal("regime set during the stretch that established the layer")
	}
	stepRegime(t, m, c, id, opposed, "opposed")
}

func TestRegimeEndsAfterAWindowOfFoldsInsideTheBand(t *testing.T) {
	m, c, id := newLearningPairWindow(t, 0.001, 40)
	feedPairs(t, m, c, 300, 1.0, "strong")
	stepRegime(t, m, c, id, opposed, "opposed")
	stepRegime(t, m, c, id, same, "back")
}

// TestResetClearsTheRegime: a reset discards the evidence a regime rests on, so it
// clears Regime and RegimeSince, and no regime starts on the pairs right after it.
func TestResetClearsTheRegime(t *testing.T) {
	m, c, id := newLearningPairWindow(t, 0.001, 40)
	feedPairs(t, m, c, 300, 1.0, "strong")
	driveRegime(t, m, c, id, 200)
	if err := m.ResetRelationship(id, "op", "t"); err != nil {
		t.Fatal(err)
	}
	if r, _ := m.Relationship(id); r.Regime || !r.RegimeSince.IsZero() {
		t.Fatalf("a reset relationship keeps its regime: %v since %v", r.Regime, r.RegimeSince)
	}
	since := m.Journal().Seq()
	feedPairs(t, m, c, 20, 0.0, "after")
	if r, _ := m.Relationship(id); r.Regime {
		t.Fatal("regime set on the pairs right after the reset")
	}
	for _, e := range m.Journal().Events(0, 0) {
		if e.Seq > since && e.Kind == EventRelationshipRegime {
			t.Fatalf("regime event after the reset: %+v", e)
		}
	}
}

func TestRegimeSurvivesRestart(t *testing.T) {
	m, c, id := newLearningPairWindow(t, 0.001, 40)
	feedPairs(t, m, c, 300, 1.0, "strong")
	driveRegime(t, m, c, id, 200)
	before, _ := m.Relationship(id)
	path := filepath.Join(t.TempDir(), "state.json")
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}
	restored, _ := newTestMap(t, Config{Learn: true})
	if r, err := restored.Load(path); err != nil || !r.Loaded {
		t.Fatalf("load: %v %v", r.Loaded, err)
	}
	after, _ := restored.Relationship(id)
	if !after.Regime || !after.RegimeSince.Equal(before.RegimeSince) {
		t.Fatalf("restored regime %v since %v; had %v since %v", after.Regime, after.RegimeSince, before.Regime, before.RegimeSince)
	}
}

// ── Default-config regressions: noise is not evidence ──────────────────────────

// noisyMap is a default-config learning map with one relationship resource ->
// pressure declared sign +1. Every learning and smoothing parameter takes its default,
// which is what these regressions are about.
func noisyMap(t *testing.T) (*Map, *clock, string) {
	t.Helper()
	m, c := newTestMap(t, Config{Learn: true})
	for _, id := range []string{"resource", "pressure"} {
		if err := m.DeclareProperty(Property{ID: id, Range: [2]float64{0, 1}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.DeclareRelationship(Relationship{From: "resource", To: "pressure", Label: "noisy", Sign: 1}); err != nil {
		t.Fatal(err)
	}
	return m, c, RelationshipID("resource", "pressure", "noisy")
}

// feedNoisy feeds n pairs of x uniform on [0,1) and y = coef·x + Gaussian noise, from
// a seeded source so the stream is the same on every run.
func feedNoisy(t *testing.T, m *Map, c *clock, rng *rand.Rand, n int, coef float64) {
	t.Helper()
	for i := 0; i < n; i++ {
		x := rng.Float64()
		y := coef*x + 0.3*rng.NormFloat64()
		c.advance(time.Second)
		if err := m.ObserveEvent("resource", x, c.now(), "r"+itoa(i)); err != nil {
			t.Fatal(err)
		}
		c.advance(time.Second)
		if err := m.ObserveEvent("pressure", y, c.now(), "p"+itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
}

// TestSignRetirementIgnoresNoiseOnACorrectSign: a correctly signed but noisy
// relationship is never retired at the defaults. The first folds share all but one
// pair of a still-filling window of smoothed values, so a conflict share over them
// rests on one or two effective draws; retirement must wait for independent evidence.
func TestSignRetirementIgnoresNoiseOnACorrectSign(t *testing.T) {
	for seed := int64(1); seed <= 10; seed++ {
		m, c, id := noisyMap(t)
		feedNoisy(t, m, c, rand.New(rand.NewSource(seed)), 2000, 0.5)
		if r, _ := m.Relationship(id); r.Status != Active {
			t.Errorf("seed %d: a correctly signed noisy relationship is %v (%q) after %d agreements, %d conflicts",
				seed, r.Status, r.RetiredReason, r.SignAgreements, r.SignConflicts)
		}
	}
}

// TestRegimeIgnoresAStationaryNoisyStream: a stream whose association never changes
// produces no regime event at the defaults. Consecutive folds share all but one pair of
// EMA-smoothed values, so the band must be the effective window's, and a run must last
// a window.
func TestRegimeIgnoresAStationaryNoisyStream(t *testing.T) {
	m, c, id := noisyMap(t)
	feedNoisy(t, m, c, rand.New(rand.NewSource(7)), 6000, 0.5)
	n := 0
	for _, e := range m.Journal().Events(0, 0) {
		if e.Kind == EventRelationshipRegime && e.Target == id {
			n++
		}
	}
	if r, _ := m.Relationship(id); n != 0 || r.Status != Active {
		t.Fatalf("stationary stream: %d regime events, status %v (%q); want none, active", n, r.Status, r.RetiredReason)
	}
}

// TestRegimeIsNotUpdatedOnARetiredRelationship: a relationship retired on a fold has
// no present to differ from, so the same fold does not journal a regime for it.
func TestRegimeIsNotUpdatedOnARetiredRelationship(t *testing.T) {
	m, c, id := newLearningPairWindow(t, 0.001, 10)
	r := m.relationships[id]
	// z(0.999) ≈ 3.8 against recent 0: outside the band (2 at window 10, alpha 0.2).
	est := 0.999
	r.Status, r.Established, r.Recent, r.regimeRun = Retired, &est, 0, 9 // one fold short of a run
	since := m.Journal().Seq()
	m.updateRegimeLocked(r, 10, c.now())
	if r.Regime {
		t.Fatal("a retired relationship entered a regime")
	}
	for _, e := range m.Journal().Events(0, 0) {
		if e.Seq > since && e.Kind == EventRelationshipRegime {
			t.Fatalf("regime event for a retired relationship: %+v", e)
		}
	}
}

// ── A down source makes its properties unobservable, not departed ─────────────

func recordFrom(t *testing.T, m *Map, id string, v float64, at time.Time, source string) {
	t.Helper()
	o := Observation{ID: id, Value: v, At: at, Source: source}
	if i := strings.Index(id, "@"); i >= 0 {
		o.Subject = id[i+1:]
	}
	if err := m.Record(o); err != nil {
		t.Fatal(err)
	}
}

func statusOfProp(t *testing.T, m *Map, id string) Status {
	t.Helper()
	p, ok := m.Property(id)
	if !ok {
		t.Fatalf("property %s missing", id)
	}
	return p.Status
}

// outageMap is a map with cpu@pod:a and cpu from "netdata" and a relationship
// between them.
func outageMap(t *testing.T) (*Map, *clock) {
	t.Helper()
	m, c := newTestMap(t, Config{StaleAfter: 30 * time.Second, RetireAfter: 2 * time.Minute, AdmitUnknown: true})
	recordFrom(t, m, "cpu@pod:a", 0.5, c.now(), "netdata")
	recordFrom(t, m, "cpu", 0.5, c.now(), "netdata")
	if err := m.DeclareRelationship(Relationship{From: "cpu@pod:a", To: "cpu", Sign: 1}); err != nil {
		t.Fatal(err)
	}
	return m, c
}

func TestSourceDownMakesPropertiesUnobservableNotRetired(t *testing.T) {
	m, c := outageMap(t)
	m.SetSourceDown("netdata", true, "")
	c.advance(2 * 2 * time.Minute)
	m.Sweep()
	_, retired := m.Sweep()
	for _, id := range []string{"cpu@pod:a", "cpu"} {
		if s := statusOfProp(t, m, id); s != Unobservable {
			t.Errorf("%s is %s; want unobservable", id, s)
		}
	}
	if len(retired) != 0 {
		t.Errorf("retired %v while the source was down", retired)
	}
	rels := m.State(Query{}).Relationships
	if len(rels) != 1 || rels[0].Status != Active {
		t.Errorf("relationships %+v; want the one still active", rels)
	}
	cs := m.Census()
	if cs.PropertiesUnobservable != 2 || cs.PropertiesRetired != 0 {
		t.Errorf("census %+v; want 2 unobservable, 0 retired", cs)
	}
}

func TestRecoveredSourceLeavesPropertiesUnobservableUntilObserved(t *testing.T) {
	m, c := outageMap(t)
	m.SetSourceDown("netdata", true, "")
	c.advance(2 * 2 * time.Minute)
	m.Sweep()

	m.SetSourceDown("netdata", false, "")
	if _, retired := m.Sweep(); len(retired) != 0 {
		t.Fatalf("retired %v at recovery; silence counts from the recovery", retired)
	}
	if s := statusOfProp(t, m, "cpu@pod:a"); s != Unobservable {
		t.Fatalf("cpu@pod:a is %s; want unobservable until observed", s)
	}

	// cpu is observed again within the window; cpu@pod:a is not.
	c.advance(time.Minute)
	recordFrom(t, m, "cpu", 0.6, c.now(), "netdata")
	if p, _ := m.Property("cpu"); p.Status != Active || p.NObservations != 2 {
		t.Fatalf("cpu %s with %d observations; want active, 2", p.Status, p.NObservations)
	}
	c.advance(time.Minute + time.Second)
	_, retired := m.Sweep()
	if len(retired) != 1 || retired[0] != "cpu@pod:a" {
		t.Errorf("retired %v; want only cpu@pod:a, silent RetireAfter since recovery", retired)
	}
}

func TestSetSourceDownJournalsEachTransitionOnce(t *testing.T) {
	m, _ := newTestMap(t, Config{AdmitUnknown: true})
	for _, down := range []bool{true, true, true, false, false, true} {
		m.SetSourceDown("x", down, "")
	}
	var got []EventKind
	for _, e := range m.Journal().Events(0, 0) {
		if e.Kind != EventCollectorDown && e.Kind != EventCollectorUp {
			continue
		}
		got = append(got, e.Kind)
		if e.Target != "x" || e.Detail["member"] != "x" || e.Actor != "collector" {
			t.Errorf("event %+v; want target x, member x, actor collector", e)
		}
	}
	want := []EventKind{EventCollectorDown, EventCollectorUp, EventCollectorDown}
	if !slices.Equal(got, want) {
		t.Errorf("events %v; want %v", got, want)
	}
}

func TestSetSourceDownOnAnUnknownSourceIsRecordedAndHarmless(t *testing.T) {
	m, c := newTestMap(t, Config{StaleAfter: 30 * time.Second, RetireAfter: 2 * time.Minute, AdmitUnknown: true})
	m.SetSourceDown("ghost", false, "")
	if n := len(m.Journal().Events(0, 0)); n != 0 {
		t.Fatalf("%d events for a source that was never down", n)
	}
	m.SetSourceDown("ghost", true, "")
	if n := len(m.Journal().Events(0, 0)); n != 1 {
		t.Fatalf("%d events; want one collector.down", n)
	}
	recordFrom(t, m, "late", 1, c.now(), "ghost")
	m.Sweep()
	if s := statusOfProp(t, m, "late"); s != Unobservable {
		t.Errorf("late is %s; want unobservable", s)
	}
}

func TestOwnerDownMakesTheTypeUnobservableNotTheOtherMember(t *testing.T) {
	m, c := newTestMap(t, Config{StaleAfter: 30 * time.Second, AdmitUnknown: true})
	recordFrom(t, m, "cpu", 0.5, c.now(), "netdata")
	m.SetSourceDown("cgroup", true, "")
	m.Sweep()
	if s := statusOfProp(t, m, "cpu"); s != Active {
		t.Fatalf("cpu is %s after another member went down; want active", s)
	}
	m.SetSourceDown("netdata", true, "")
	m.Sweep()
	if s := statusOfProp(t, m, "cpu"); s != Unobservable {
		t.Errorf("cpu is %s; want unobservable", s)
	}
}

func TestRecordUpdatesSourceOnEveryObservation(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	recordFrom(t, m, "cpu", 0.5, c.now(), "a")
	c.advance(time.Second)
	recordFrom(t, m, "cpu", 0.5, c.now(), "b")
	if p, _ := m.Property("cpu"); p.Source != "b" {
		t.Errorf("source %q; want b", p.Source)
	}
}

func TestStateDefaultViewIncludesUnobservable(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	recordFrom(t, m, "cpu", 0.5, c.now(), "netdata")
	m.SetSourceDown("netdata", true, "")
	m.Sweep()
	if v := m.State(Query{}); len(v.Properties) != 1 || v.Properties[0].Status != Unobservable {
		t.Errorf("default view %+v; want the unobservable property", v.Properties)
	}
	recordFrom(t, m, "mem", 0.5, c.now(), "other")
	if v := m.State(Query{Statuses: []Status{Unobservable}}); len(v.Properties) != 1 || v.Properties[0].ID != "cpu" {
		t.Errorf("unobservable view %+v; want only cpu", v.Properties)
	}
}

func TestDecisionCaveatsAnUnobservableEndpoint(t *testing.T) {
	m, c := newTestMap(t, Config{StaleAfter: 10 * time.Second, AdmitUnknown: true})
	recordFrom(t, m, "cpu@pod:a", .5, c.now(), "netdata")
	recordFrom(t, m, "cpu", .5, c.now(), "other")
	if err := m.DeclareRelationship(Relationship{From: "cpu@pod:a", To: "cpu", Sign: 1}); err != nil {
		t.Fatal(err)
	}
	m.SetSourceDown("netdata", true, "")
	m.Sweep()
	b := m.Decide("d1", "estimate", "test")
	b.RelationshipsInto("cpu")
	d := b.Commit(nil)
	want := "relationship cpu@pod:a->cpu has an endpoint cpu@pod:a that is unobservable"
	if !slices.Contains(d.Caveats, want) {
		t.Errorf("caveats %v; want %q", d.Caveats, want)
	}
}

func TestDecisionCaveatsAnUnobservableProperty(t *testing.T) {
	m, c := newTestMap(t, Config{StaleAfter: 10 * time.Second, AdmitUnknown: true})
	recordFrom(t, m, "cpu", .5, c.now(), "netdata")
	m.SetSourceDown("netdata", true, "")
	m.Sweep()
	b := m.Decide("d1", "estimate", "test")
	if _, ok := b.Property("cpu"); !ok {
		t.Fatal("property missing")
	}
	d := b.Commit(nil)
	var found bool
	for _, cv := range d.Caveats {
		if strings.HasPrefix(cv, "property cpu is unobservable: its collector is down, last observed ") {
			found = true
		}
	}
	if !found {
		t.Errorf("caveats %v; want one naming the unobservable property", d.Caveats)
	}
}

// ── Retired properties are forgotten, leaving a tombstone ─────────────────────

func lastEventOf(m *Map, kind EventKind) (Event, bool) {
	evs := m.Journal().Events(0, 0)
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind == kind {
			return evs[i], true
		}
	}
	return Event{}, false
}

func observeScoped(m *Map, id string, v float64, at time.Time) error {
	_, subject, _ := strings.Cut(id, "@")
	return m.Record(Observation{ID: id, Value: v, At: at, Subject: subject})
}

func forgetCfg() Config {
	return Config{StaleAfter: 30 * time.Second, RetireAfter: 2 * time.Minute, AdmitUnknown: true}
}

func TestRetiredPropertyIsForgottenAfterADayWithATombstone(t *testing.T) {
	m, c := newTestMap(t, forgetCfg())
	_ = m.Observe("cpu", 0.5, c.now())
	_ = observeScoped(m, "cpu@pod:a", 0.5, c.now())
	c.advance(3 * time.Minute)
	m.Sweep()
	p, _ := m.Property("cpu@pod:a")
	if p.Status != Retired || !p.RetiredAt.Equal(c.now()) {
		t.Fatalf("status %s retired_at %v, want retired now", p.Status, p.RetiredAt)
	}
	retiredAt := c.now()
	before := m.Census().PropertiesTotal
	c.advance(forgetAfter - time.Second)
	m.Sweep()
	if _, ok := m.Property("cpu@pod:a"); !ok {
		t.Fatal("forgotten before forgetAfter")
	}
	c.advance(2 * time.Second)
	m.Sweep()
	if _, ok := m.Property("cpu@pod:a"); ok {
		t.Fatal("still present after forgetAfter")
	}
	at, ok := m.Forgotten("cpu@pod:a")
	if !ok || !at.Equal(retiredAt) {
		t.Errorf("tombstone %v %v, want %v", at, ok, retiredAt)
	}
	forgotEvent := false // cpu is forgotten in the same sweep, in no fixed order
	for _, e := range m.Journal().Events(0, 0) {
		forgotEvent = forgotEvent || (e.Kind == EventPropertyForgotten && e.Target == "cpu@pod:a")
	}
	if !forgotEvent {
		t.Errorf("no property.forgotten event for the id")
	}
	if got := m.Census().PropertiesTotal; got != before-2 { // cpu is forgotten too, being silent
		t.Errorf("properties_total %d, want %d", got, before-2)
	}
}

func TestForgettingDropsTheRetiredRelationshipsToo(t *testing.T) {
	m, c := newTestMap(t, forgetCfg())
	_ = m.Observe("cpu", 0.5, c.now())
	_ = observeScoped(m, "cpu@pod:a", 0.5, c.now())
	if err := m.DeclareRelationship(Relationship{From: "cpu@pod:a", To: "cpu", Sign: 1}); err != nil {
		t.Fatal(err)
	}
	c.advance(3 * time.Minute)
	m.Sweep()
	c.advance(forgetAfter + time.Second)
	_ = m.Observe("cpu", 0.5, c.now()) // revived, so only the pod is forgotten
	m.Sweep()
	id := RelationshipID("cpu@pod:a", "cpu", "")
	if _, ok := m.Relationship(id); ok {
		t.Error("the retired relationship outlived its endpoint")
	}
	found := false
	for _, e := range m.Journal().Events(0, 0) {
		if e.Kind == EventPropertyForgotten && e.Target == "cpu@pod:a" {
			rels, _ := e.Detail["relationships"].([]string)
			found = slices.Contains(rels, id)
		}
	}
	if !found {
		t.Error("the forgotten event does not list the relationship it removed")
	}
}

func TestForgetFiresNoRetireHookAndReobservationIsANewAdmission(t *testing.T) {
	m, c := newTestMap(t, forgetCfg())
	n := 0
	m.SetRetireHook(func(string) { n++ })
	_ = observeScoped(m, "cpu@pod:a", 0.5, c.now())
	_ = observeScoped(m, "cpu@pod:a", 0.5, c.now().Add(time.Second))
	c.advance(3 * time.Minute)
	m.Sweep()
	c.advance(forgetAfter + time.Second)
	m.Sweep()
	if n != 1 {
		t.Errorf("retire hook fired %d times, want 1", n)
	}
	if err := observeScoped(m, "cpu@pod:a", 0.4, c.now()); err != nil {
		t.Fatal(err)
	}
	p, ok := m.Property("cpu@pod:a")
	if !ok || p.Status != Active || p.NObservations != 1 {
		t.Errorf("after re-observation: %+v ok=%v, want active with 1 observation", p, ok)
	}
	if _, gone := m.Forgotten("cpu@pod:a"); gone {
		t.Error("tombstone survived re-admission")
	}
	e, _ := lastEventOf(m, EventPropertyAdmitted)
	if e.Detail["after_forgotten"] != true {
		t.Errorf("admission detail %v lacks after_forgotten", e.Detail)
	}
}

func TestTombstoneExpiresAfterTwiceForgetAfter(t *testing.T) {
	m, c := newTestMap(t, forgetCfg())
	_ = observeScoped(m, "cpu@pod:a", 0.5, c.now())
	c.advance(3 * time.Minute)
	m.Sweep()
	c.advance(forgetAfter + time.Second)
	m.Sweep()
	if _, ok := m.Forgotten("cpu@pod:a"); !ok {
		t.Fatal("no tombstone after forgetting")
	}
	c.advance(forgetAfter + time.Second)
	m.Sweep()
	if _, ok := m.Forgotten("cpu@pod:a"); ok {
		t.Error("tombstone outlived twice forgetAfter")
	}
}

func TestDerivedPropertiesAreNeverForgotten(t *testing.T) {
	m, c := newTestMap(t, forgetCfg())
	if err := m.DeclareProperty(Property{ID: "RC", Kind: Derived, Members: []string{"cpu"}}); err != nil {
		t.Fatal(err)
	}
	_ = m.Observe("cpu", 0.5, c.now())
	c.advance(3 * time.Minute)
	m.Sweep()
	c.advance(forgetAfter + time.Second)
	m.Sweep()
	d, ok := m.Property("RC")
	if !ok {
		t.Fatal("derived property was forgotten")
	}
	if _, ok := m.Property("cpu"); ok {
		t.Fatal("the member should have been forgotten")
	}
	if d.Confidence != 0 || !slices.Contains(d.Members, "cpu") {
		t.Errorf("derived %+v: want confidence 0 and the member id kept", d)
	}
}

func TestForgottenSurvivesASnapshot(t *testing.T) {
	m, c := newTestMap(t, forgetCfg())
	_ = observeScoped(m, "cpu@pod:a", 0.5, c.now())
	_ = observeScoped(m, "cpu@pod:b", 0.5, c.now())
	c.advance(3 * time.Minute)
	m.Sweep()
	_ = observeScoped(m, "cpu@pod:c", 0.5, c.now())
	c.advance(forgetAfter - time.Minute)
	_ = observeScoped(m, "cpu@pod:c", 0.5, c.now())
	m.Sweep() // a and b not yet forgotten? they are retired 1 minute short of a day
	c.advance(2 * time.Minute)
	_ = observeScoped(m, "cpu@pod:c", 0.5, c.now())
	m.Sweep()
	if _, ok := m.Forgotten("cpu@pod:a"); !ok {
		t.Fatal("setup: a not forgotten")
	}
	path := filepath.Join(t.TempDir(), "s.json")
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}
	m2, _ := newTestMap(t, forgetCfg())
	if _, err := m2.Load(path); err != nil {
		t.Fatal(err)
	}
	if _, ok := m2.Forgotten("cpu@pod:a"); !ok {
		t.Error("tombstone lost across a restore")
	}
}

func TestRetiredAtSurvivesASnapshot(t *testing.T) {
	m, c := newTestMap(t, forgetCfg())
	_ = observeScoped(m, "cpu@pod:a", 0.5, c.now())
	c.advance(3 * time.Minute)
	m.Sweep()
	want := c.now()
	path := filepath.Join(t.TempDir(), "s.json")
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}
	m2, _ := newTestMap(t, forgetCfg())
	if _, err := m2.Load(path); err != nil {
		t.Fatal(err)
	}
	p, _ := m2.Property("cpu@pod:a")
	if !p.RetiredAt.Equal(want) {
		t.Errorf("retired_at %v, want %v", p.RetiredAt, want)
	}
}

func TestForgettingDropsPerPairStateKeyedByTheProperty(t *testing.T) {
	m, c := newTestMap(t, forgetCfg())
	_ = observeScoped(m, "cpu@pod:a", 0.5, c.now())
	m.mu.Lock()
	m.interventions[interventionKey("cpu@pod:a", "cpu")] = &Interventions{}
	m.interventions[interventionKey("x", "y")] = &Interventions{}
	m.mu.Unlock()
	c.advance(3 * time.Minute)
	m.Sweep()
	c.advance(forgetAfter + time.Second)
	m.Sweep()
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.interventions[interventionKey("cpu@pod:a", "cpu")]; ok {
		t.Error("intervention counters for a forgotten property remain")
	}
	if _, ok := m.interventions[interventionKey("x", "y")]; !ok {
		t.Error("an unrelated pair's counters were dropped")
	}
}

func TestChurnedPodsDoNotGrowTheMap(t *testing.T) {
	m, c := newTestMap(t, forgetCfg())
	for i := 0; i < 100; i++ {
		n := strconv.Itoa(i)
		_ = observeScoped(m, "cpu@pod:"+n, 0.5, c.now())
		_ = observeScoped(m, "memory@pod:"+n, 0.5, c.now())
	}
	live := func() {
		_ = m.Observe("cpu", 0.5, c.now())
		_ = m.Observe("memory", 0.5, c.now())
	}
	tombstones := func() int {
		n := 0
		for i := 0; i < 100; i++ {
			for _, k := range []string{"cpu@pod:", "memory@pod:"} {
				if _, ok := m.Forgotten(k + strconv.Itoa(i)); ok {
					n++
				}
			}
		}
		return n
	}
	step := func(d time.Duration, total time.Duration) {
		for el := time.Duration(0); el < total; el += d {
			c.advance(d)
			live()
			m.Sweep()
		}
	}
	step(time.Minute, 3*time.Minute)
	step(time.Hour, forgetAfter+time.Hour)
	if got := m.Census().PropertiesTotal; got != 2 {
		t.Errorf("properties_total %d after the pods churned away, want 2", got)
	}
	if got := tombstones(); got != 200 {
		t.Errorf("%d tombstones, want 200", got)
	}
	step(time.Hour, 2*forgetAfter)
	if got := tombstones(); got != 0 {
		t.Errorf("%d tombstones after twice forgetAfter, want 0", got)
	}
}

func TestRestoredPropertyOfReturningSourceCountsSilenceFromReturn(t *testing.T) {
	m1, _ := outageMap(t)
	path := t.TempDir() + "/state.json"
	if err := m1.Save(path); err != nil {
		t.Fatal(err)
	}
	m2, c := newTestMap(t, Config{StaleAfter: 30 * time.Second, RetireAfter: 2 * time.Minute, AdmitUnknown: true})
	if _, err := m2.Load(path); err != nil {
		t.Fatal(err)
	}
	m2.SetSourceDown("netdata", true, "")
	c.advance(3 * time.Minute) // past RetireAfter and the restore grace
	if _, retired := m2.Sweep(); len(retired) != 0 {
		t.Fatalf("retired %v while the source is down", retired)
	}
	m2.SetSourceDown("netdata", false, "")
	if _, retired := m2.Sweep(); len(retired) != 0 {
		t.Fatalf("retired %v at return; silence counts from the return", retired)
	}
	c.advance(2*time.Minute + time.Second)
	if _, retired := m2.Sweep(); len(retired) != 2 {
		t.Errorf("retired %v; want both, RetireAfter since the return", retired)
	}
}
