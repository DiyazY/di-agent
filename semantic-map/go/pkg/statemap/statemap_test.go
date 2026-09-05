package statemap

import (
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// clock is a controllable time source: lifecycle transitions depend on elapsed
// time, and a test that slept for them would be slow and flaky.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

// near compares floats that arrive via EMA arithmetic, where an exact literal is
// not reachable: 0.2*0.8 + 0.8*0.8 is 0.8000000000000001.
func near(got, want float64) bool {
	d := got - want
	return d < 1e-9 && d > -1e-9
}

func newTestMap(t *testing.T, cfg Config) (*Map, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	m := New(cfg, NewJournal(0))
	m.SetClock(c.now)
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
	if r.Strength != 0.9 {
		t.Errorf("assertion overwrote the observed strength (%.3f); the distinction "+
			"between what was observed and what was asserted is what an audit needs",
			r.Strength)
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
		b := m.Decide("dec-1", "how pressured is this system?")
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

	b := m.Decide("dec-2", "cost now?")
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
		if err := m.Observe(id, 0.4, c.now()); err != nil {
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
	if !(raises.Strength > 0.5) {
		t.Errorf("the supported mechanism learned strength %.4f; a stream where pressure "+
			"tracks resource should support it", raises.Strength)
	}
	if relieves.Strength != 0 {
		t.Errorf("the contradicted mechanism learned strength %.4f; evidence of the "+
			"opposite sign is evidence against it, not a weaker version of it",
			relieves.Strength)
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
	if diff := r2.Strength - r1.Strength; diff > 0.05 || diff < -0.05 {
		t.Errorf("replay moved the strength by %.4f (%.4f -> %.4f), more than one "+
			"boundary pair can explain", diff, r1.Strength, r2.Strength)
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
	b := m1.Decide("dec-before-restart", "how is b?")
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
	loaded, err := m2.Load(path)
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
	if got.Strength != learned.Strength || got.NObservations != learned.NObservations {
		t.Errorf("restored strength %.6f/%d, had %.6f/%d — an agent that forgets what it "+
			"learned is back at cold start on a system it has already watched",
			got.Strength, got.NObservations, learned.Strength, learned.NObservations)
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
	loaded, err := m.Load(dir + "/absent.json")
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
	loaded, err := fresh.Load(path)
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

	if backwards.Strength != 0 {
		t.Fatalf("precondition: the contradicted relationship should sit at zero, got %.4f",
			backwards.Strength)
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
}

func newLearningPair(t *testing.T, alphaSlow float64) (*Map, *clock, string) {
	t.Helper()
	m, c := newTestMap(t, Config{
		ConvergenceObservations: 500,
		Alpha:                   0.2,
		AlphaSlow:               alphaSlow,
		Learn:                   true,
		LearnConfig:             LearnConfig{PairWindowSeconds: 15, MinSupport: 8, Window: 60},
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
	if math.Abs(*r.Established-r.Strength) > 0.05 {
		t.Errorf("established %.6f and recent %.6f differ by more than 0.05 this early; "+
			"before history accumulates there is nothing for them to disagree about",
			*r.Established, r.Strength)
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

	recentDrop := afterStrong.Strength - after.Strength
	establishedDrop := establishedBefore - *after.Established
	t.Logf("recent      %.4f -> %.4f  (moved %.4f)", afterStrong.Strength, after.Strength, recentDrop)
	t.Logf("established %.4f -> %.4f  (moved %.4f)", establishedBefore, *after.Established, establishedDrop)

	if recentDrop <= 0 {
		t.Errorf("the recent layer did not fall when the association went away "+
			"(%.4f -> %.4f)", afterStrong.Strength, after.Strength)
	}
	if establishedDrop >= recentDrop {
		t.Errorf("the established layer moved as much as the recent one (%.4f vs %.4f); "+
			"then it is a duplicate rather than a second timescale",
			establishedDrop, recentDrop)
	}
	if math.Abs(after.Strength-*after.Established) < 0.05 {
		t.Errorf("recent %.4f and established %.4f are within 0.05 after a regime "+
			"change; the gap between them is the signal that the present is unusual",
			after.Strength, *after.Established)
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

func TestRedeclaringARangeMakesItDeclared(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	if err := m.Observe("q@pod:1", 5, c.now()); err != nil {
		t.Fatal(err)
	}
	p, _ := m.Property("q@pod:1")
	if p.RangeDeclared {
		t.Fatalf("admitted property already has RangeDeclared=true; want an assumed range")
	}

	if err := m.DeclareProperty(Property{ID: "q@pod:1", Range: [2]float64{0, 100}}); err != nil {
		t.Fatal(err)
	}
	p, _ = m.Property("q@pod:1")
	if !p.RangeDeclared {
		t.Fatalf("RangeDeclared = false after DeclareProperty carried a Range; want true")
	}

	c.advance(time.Second)
	rng := [2]float64{0, 1}
	if err := m.Record(Observation{ID: "q@pod:1", Value: 6, At: c.now(), Range: &rng}); err != nil {
		t.Fatal(err)
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
	if err := m.Observe("x@pod:1", 0.5, c.now()); err != nil {
		t.Fatal(err)
	}

	eventsBefore := len(m.Journal().Events(0, 0))
	c.advance(time.Second)
	rng := [2]float64{0, 10}
	if err := m.Record(Observation{
		ID: "x@pod:1", Value: 0.6, At: c.now(), Unit: "items", Range: &rng,
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
	_ = m.Observe("cpu@pod:1", .3, c.now())
	c.advance(time.Minute)
	m.Sweep()
	if p, _ := m.Property("cpu@pod:1"); p.Status != Retired {
		t.Fatalf("setup: status %s, want retired", p.Status)
	}
	c.advance(time.Second)
	_ = m.Observe("cpu@pod:1", .4, c.now())
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
	b := m.Decide("d1", "test")
	b.RelationshipsInto("dst")
	d := b.Commit(nil)
	var found bool
	for _, cv := range d.Caveats {
		if strings.Contains(cv, "stale endpoint src") {
			found = true
		}
	}
	if !found {
		t.Errorf("caveats %v; want one naming the stale endpoint", d.Caveats)
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
