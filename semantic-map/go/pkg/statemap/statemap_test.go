package statemap

import (
	"strings"
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
	if err := m.DeclareRelationship(Relationship{From: "a", To: "b", Sign: 1, Prior: 0.5}); err != nil {
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

func TestRelationshipBlendsPriorAndEvidence(t *testing.T) {
	m, c := newTestMap(t, Config{ConvergenceObservations: 10, Alpha: 1.0})
	for _, id := range []string{"a", "b"} {
		if err := m.DeclareProperty(Property{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.DeclareRelationship(Relationship{
		From: "a", To: "b", Sign: 1, Prior: 0.8, Provenance: Seeded,
	}); err != nil {
		t.Fatal(err)
	}
	id := RelationshipID("a", "b", "")

	r, _ := m.Relationship(id)
	if r.Effective() != 0.8 {
		t.Errorf("unobserved relationship reports effective %.3f, want the prior 0.8", r.Effective())
	}

	for i := 0; i < 10; i++ {
		c.advance(time.Second)
		if err := m.ObserveRelationship(id, 0.2, c.now()); err != nil {
			t.Fatal(err)
		}
	}
	r, _ = m.Relationship(id)
	if r.Effective() != 0.2 {
		t.Errorf("fully observed relationship reports effective %.3f, want the "+
			"observed 0.2", r.Effective())
	}
	if r.Provenance != Learned {
		t.Errorf("provenance %s after observation; a strength that came from this "+
			"system must not still claim to be seeded", r.Provenance)
	}
}

func TestAssertionMovesThePriorAndIsAttributed(t *testing.T) {
	m, c := newTestMap(t, Config{ConvergenceObservations: 4, Alpha: 1.0})
	for _, id := range []string{"a", "b"} {
		if err := m.DeclareProperty(Property{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.DeclareRelationship(Relationship{From: "a", To: "b", Sign: 1, Prior: 0.5}); err != nil {
		t.Fatal(err)
	}
	id := RelationshipID("a", "b", "")
	for i := 0; i < 4; i++ {
		c.advance(time.Second)
		if err := m.ObserveRelationship(id, 0.9, c.now()); err != nil {
			t.Fatal(err)
		}
	}

	if err := m.AssertRelationshipStrength(id, 0.1, "operator:ada", "measured by hand"); err != nil {
		t.Fatal(err)
	}
	r, _ := m.Relationship(id)
	if r.Prior != 0.1 {
		t.Errorf("prior %.3f after assertion, want 0.1", r.Prior)
	}
	if r.Strength != 0.9 {
		t.Errorf("assertion overwrote the observed strength (%.3f); the distinction "+
			"between what was observed and what was asserted is what an audit needs", r.Strength)
	}
	if r.Provenance != Asserted {
		t.Errorf("provenance %s, want asserted", r.Provenance)
	}

	var attributed bool
	for _, e := range m.Journal().Events(0, 0) {
		if e.Kind == EventRelationshipAsserted && e.Actor == "operator:ada" {
			attributed = true
			if e.Detail["reason"] != "measured by hand" {
				t.Errorf("journal lost the reason: %v", e.Detail["reason"])
			}
		}
	}
	if !attributed {
		t.Error("no journal entry attributes the assertion to its actor")
	}
}

// ── Queryable ─────────────────────────────────────────────────────────────────

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
		From: "resource_use", To: "pressure", Sign: 1, Prior: 0.6, Provenance: Seeded,
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
