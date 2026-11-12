package statemap

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestEventsCarrySeqStrictlyIncreasing(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	// A first observation of an unknown property admits it, and the admission is
	// journalled; a repeated observation of a known one need not be.
	for i := 0; i < 10; i++ {
		if err := m.Observe(fmt.Sprintf("p%d", i), 0.5, c.now()); err != nil {
			t.Fatal(err)
		}
	}
	m.Decide("explicit", "estimate", "q").Commit(nil)
	events := m.Journal().Events(0, 0)
	if len(events) < 11 {
		t.Fatalf("journal holds %d events; want at least 11", len(events))
	}
	for i, e := range events {
		if e.Seq != uint64(i+1) {
			t.Fatalf("event %d (%s) has seq %d; want %d", i, e.Kind, e.Seq, i+1)
		}
	}
	if last := events[len(events)-1]; last.Kind != EventDecision {
		t.Fatalf("last event is %s; want the decision", last.Kind)
	}
	if got := m.Journal().Seq(); got != uint64(len(events)) {
		t.Errorf("journal seq %d; want %d", got, len(events))
	}
}

func TestNewDecisionIDCarriesIncarnationAndTicket(t *testing.T) {
	m, _ := newTestMap(t, Config{})
	if got := m.NewDecisionID("est", "x"); got != "est-1.1-x" {
		t.Errorf("first id %q; want est-1.1-x", got)
	}
	if got := m.NewDecisionID("est", "x"); got != "est-1.2-x" {
		t.Errorf("second id %q; want est-1.2-x", got)
	}
	if got := m.NewDecisionID("adm"); got != "adm-1.3" {
		t.Errorf("third id %q; want adm-1.3", got)
	}
}

func TestNewDecisionIDIsUniqueUnderConcurrency(t *testing.T) {
	m, _ := newTestMap(t, Config{})
	ids := make([]string, 100)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := m.NewDecisionID("p", "s")
			m.Decide(id, "estimate", "q").Commit(nil)
			ids[i] = id
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("id %q issued twice", id)
		}
		seen[id] = true
		if _, ok := m.Journal().Decision(id); !ok {
			t.Errorf("decision %q not recorded", id)
		}
	}
}

func TestAnAbandonedGeneratedIdLeavesNothingReserved(t *testing.T) {
	m, _ := newTestMap(t, Config{})
	for i := 0; i < 5; i++ {
		m.NewDecisionID("relief-act-cpu.limit") // issued, never committed
	}
	j := m.Journal()
	j.mu.RLock()
	n := len(j.reserved)
	j.mu.RUnlock()
	if n != 0 {
		t.Errorf("%d ids reserved after issuing without committing; want 0", n)
	}
}

func TestRecordDecisionRefusesADuplicate(t *testing.T) {
	j := NewJournal(0)
	if _, err := j.RecordDecision(&Decision{ID: "d"}); err != nil {
		t.Fatal(err)
	}
	if _, err := j.RecordDecision(&Decision{ID: "d"}); !errors.Is(err, ErrDuplicateDecision) {
		t.Fatalf("second record err %v; want ErrDuplicateDecision", err)
	}
	if got := j.Decisions(0, ""); len(got) != 1 {
		t.Errorf("journal holds %d decisions; want 1", len(got))
	}
	for _, e := range j.Events(0, 0) {
		if e.Kind == EventDecisionRefused {
			t.Error("RecordDecision wrote a decision.refused event; only Commit writes it")
		}
	}
}

func TestCommitOnADuplicateRecordsRefusal(t *testing.T) {
	m, _ := newTestMap(t, Config{})
	if d := m.Decide("d", "estimate", "q").Commit(nil); d == nil {
		t.Fatal("first commit returned nil")
	}
	if d := m.Decide("d", "estimate", "q").Commit(nil); d != nil {
		t.Fatalf("second commit returned %+v; want nil", d)
	}
	events := m.Journal().Events(0, 0)
	last := events[len(events)-1]
	if last.Kind != EventDecisionRefused || last.Target != "d" {
		t.Errorf("last event %s on %q; want decision.refused on d", last.Kind, last.Target)
	}
}

func TestIncarnationAdvancesOnEveryRestore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap.json")
	m1, _ := newTestMap(t, Config{})
	if err := m1.Save(path); err != nil {
		t.Fatal(err)
	}
	if got := m1.Incarnation(); got != 1 {
		t.Errorf("fresh map incarnation %d; want 1", got)
	}
	m2, _ := newTestMap(t, Config{})
	if _, err := m2.Load(path); err != nil {
		t.Fatal(err)
	}
	if got := m2.Incarnation(); got != 2 {
		t.Errorf("first restore incarnation %d; want 2", got)
	}
	m3, _ := newTestMap(t, Config{})
	if _, err := m3.Load(path); err != nil {
		t.Fatal(err)
	}
	if got := m3.Incarnation(); got != 3 {
		t.Errorf("second restore incarnation %d; want 3", got)
	}
	if id := m2.NewDecisionID("est"); !strings.HasPrefix(id, "est-2.") {
		t.Errorf("restored map's id %q; want prefix est-2.", id)
	}
}

func TestRestoreOfAnOlderSnapshotStillGivesFreshIds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap.json")
	m1, _ := newTestMap(t, Config{})
	issued := map[string]bool{}
	for i := 0; i < 3; i++ {
		id := m1.NewDecisionID("est")
		m1.Decide(id, "estimate", "q").Commit(nil)
		issued[id] = true
	}
	if err := m1.Save(path); err != nil {
		t.Fatal(err)
	}
	// Decisions after the save that never reach disk: the simulated crash.
	for i := 0; i < 5; i++ {
		id := m1.NewDecisionID("est")
		m1.Decide(id, "estimate", "q").Commit(nil)
		issued[id] = true
	}
	m2, _ := newTestMap(t, Config{})
	if _, err := m2.Load(path); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if id := m2.NewDecisionID("est"); issued[id] {
			t.Fatalf("restored map reissued %q", id)
		}
	}
}

func TestRestoreSeqNeverFallsBelowHeldEvents(t *testing.T) {
	m1, c := newTestMap(t, Config{AdmitUnknown: true})
	for i := 0; m1.Journal().Seq() < 20; i++ {
		if err := m1.Observe(fmt.Sprintf("p%d", i), 0.5, c.now()); err != nil {
			t.Fatal(err)
		}
	}
	snap := m1.Snapshot()
	snap.Seq = 1
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "snap.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	m2, _ := newTestMap(t, Config{})
	if _, err := m2.Load(path); err != nil {
		t.Fatal(err)
	}
	held := m1.Journal().Events(0, 0)
	highest := held[len(held)-1].Seq
	// Load journals the restore itself: that is the next event after the held ones.
	events := m2.Journal().Events(0, 0)
	if got := events[len(events)-1].Seq; got != highest+1 {
		t.Errorf("next event seq %d; want %d", got, highest+1)
	}
}

func TestLoadReservesTheIncarnationAtOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap.json")
	m1, _ := newTestMap(t, Config{})
	if err := m1.Save(path); err != nil {
		t.Fatal(err)
	}
	m2, _ := newTestMap(t, Config{})
	if _, err := m2.Load(path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Incarnation != 2 {
		t.Errorf("file incarnation %d after Load; want 2", snap.Incarnation)
	}
}

func TestLoadRefusesAVersion6Snapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap.json")
	if err := os.WriteFile(path, []byte(`{"version":6,"owner":"","events":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m, _ := newTestMap(t, Config{})
	if _, err := m.Load(path); err == nil || !strings.Contains(err.Error(), "version 6") {
		t.Fatalf("load err %v; want a refusal naming version 6", err)
	}
}

func TestDecisionCarriesItsKindInTheEvent(t *testing.T) {
	m, _ := newTestMap(t, Config{})
	d := m.Decide("e", "estimate", "q").Commit(nil)
	if d == nil || d.Kind != "estimate" {
		t.Fatalf("decision %+v; want kind estimate", d)
	}
	events := m.Journal().Events(0, 0)
	last := events[len(events)-1]
	if last.Kind != EventDecision || last.Detail["kind"] != "estimate" {
		t.Errorf("last event %s detail %v; want decision with kind estimate", last.Kind, last.Detail)
	}
}

func TestCiteIsOrderedAndDeduplicated(t *testing.T) {
	m, _ := newTestMap(t, Config{})
	b := m.Decide("d", "review", "q")
	b.Cite("b")
	b.Cite("")
	b.Cite("a")
	b.Cite("b")
	d := b.Commit(nil)
	if got := strings.Join(d.Cites, ","); got != "b,a" {
		t.Errorf("cites %q; want b,a", got)
	}
}

// chainIDs is the ids of a chain's cites, in order.
func chainIDs(c Chain) string {
	ids := make([]string, len(c.Cites))
	for i, l := range c.Cites {
		ids[i] = l.ID
	}
	return strings.Join(ids, ",")
}

// actVerifyRevert records the three decisions of a reverted relief action.
func actVerifyRevert(m *Map) {
	m.Decide("act", "relief.act", "q").Commit(nil)
	v := m.Decide("verify", "relief.verify", "q")
	v.Cite("act")
	v.Commit(nil)
	r := m.Decide("revert", "relief.revert", "q")
	r.Cite("verify")
	r.Cite("act")
	r.Commit(nil)
}

func TestChainWalksAncestryInCiteOrder(t *testing.T) {
	m, _ := newTestMap(t, Config{})
	actVerifyRevert(m)
	c, ok := m.Journal().Chain("revert")
	if !ok {
		t.Fatal("revert not held")
	}
	if got := chainIDs(c); got != "verify,act" {
		t.Errorf("revert cites %q; want verify,act", got)
	}
	if c.Cites[0].Decision == nil || c.Cites[0].Evicted {
		t.Errorf("held ancestor %+v; want its decision", c.Cites[0])
	}
	a, _ := m.Journal().Chain("act")
	if got := strings.Join(a.CitedBy, ","); got != "verify,revert" {
		t.Errorf("act cited by %q; want verify,revert", got)
	}
	if _, ok := m.Journal().Chain("nope"); ok {
		t.Error("Chain of an id never held reported ok")
	}
}

func TestChainIsCycleSafeOnAFullJournal(t *testing.T) {
	j := NewJournal(2000)
	n := 2000
	id := func(i int) string { return fmt.Sprintf("d%d", i) }
	for i := 0; i < n; i++ {
		d := &Decision{ID: id(i), Kind: "review"}
		if i > 0 {
			d.Cites = []string{id(i - 1)}
		} else {
			d.Cites = []string{id(n - 1)}
		}
		if i == n-1 {
			d.Cites = append(d.Cites, id(i))
		}
		if _, err := j.RecordDecision(d); err != nil {
			t.Fatal(err)
		}
	}
	c, ok := j.Chain(id(n - 1))
	if !ok {
		t.Fatal("last not held")
	}
	if len(c.Cites) != n-1 {
		t.Fatalf("%d links; want %d", len(c.Cites), n-1)
	}
	seen := map[string]bool{}
	for _, l := range c.Cites {
		if seen[l.ID] || l.ID == id(n-1) {
			t.Fatalf("link %s repeated or the decision itself", l.ID)
		}
		seen[l.ID] = true
	}
}

func TestChainFlagsAnEvictedAncestor(t *testing.T) {
	j := NewJournal(3)
	rec := func(id string, rev uint64, cites ...string) {
		if _, err := j.RecordDecision(&Decision{ID: id, Revision: rev, Cites: cites}); err != nil {
			t.Fatal(err)
		}
	}
	rec("a", 1)
	rec("b", 2, "a")
	rec("c", 3)
	rec("d", 4)
	c, ok := j.Chain("b")
	if !ok {
		t.Fatal("b not held")
	}
	if len(c.Cites) != 1 || c.Cites[0].ID != "a" || !c.Cites[0].Evicted || c.Cites[0].Decision != nil {
		t.Errorf("b cites %+v; want [{a evicted}]", c.Cites)
	}
	if _, ok := j.Chain("a"); ok {
		t.Error("evicted a still reports a chain")
	}
}

func TestDecisionsFilterByKind(t *testing.T) {
	m, _ := newTestMap(t, Config{})
	m.Decide("e1", "estimate", "q").Commit(nil)
	m.Decide("a1", "admit", "q").Commit(nil)
	m.Decide("e2", "estimate", "q").Commit(nil)
	m.Decide("a2", "admit", "q").Commit(nil)
	got := m.Journal().Decisions(0, "admit")
	if len(got) != 2 || got[0].ID != "a2" || got[1].ID != "a1" {
		t.Errorf("admit decisions %v; want a2, a1", got)
	}
	if all := m.Journal().Decisions(0, ""); len(all) != 4 {
		t.Errorf("%d decisions with no kind filter; want 4", len(all))
	}
}

func TestRestoreRebuildsCitedBy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap.json")
	m1, _ := newTestMap(t, Config{})
	actVerifyRevert(m1)
	if err := m1.Save(path); err != nil {
		t.Fatal(err)
	}
	m2, _ := newTestMap(t, Config{})
	if _, err := m2.Load(path); err != nil {
		t.Fatal(err)
	}
	c, ok := m2.Journal().Chain("act")
	if !ok || strings.Join(c.CitedBy, ",") != "verify,revert" {
		t.Errorf("restored act cited by %v (held %v); want verify,revert", c.CitedBy, ok)
	}
}
