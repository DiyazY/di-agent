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
	m.Decide("explicit", "q").Commit(nil)
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
			m.Decide(id, "q").Commit(nil)
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

func TestRecordDecisionRefusesADuplicate(t *testing.T) {
	j := NewJournal(0)
	if _, err := j.RecordDecision(&Decision{ID: "d"}); err != nil {
		t.Fatal(err)
	}
	if _, err := j.RecordDecision(&Decision{ID: "d"}); !errors.Is(err, ErrDuplicateDecision) {
		t.Fatalf("second record err %v; want ErrDuplicateDecision", err)
	}
	if got := j.Decisions(0); len(got) != 1 {
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
	if d := m.Decide("d", "q").Commit(nil); d == nil {
		t.Fatal("first commit returned nil")
	}
	if d := m.Decide("d", "q").Commit(nil); d != nil {
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
		m1.Decide(id, "q").Commit(nil)
		issued[id] = true
	}
	if err := m1.Save(path); err != nil {
		t.Fatal(err)
	}
	// Decisions after the save that never reach disk: the simulated crash.
	for i := 0; i < 5; i++ {
		id := m1.NewDecisionID("est")
		m1.Decide(id, "q").Commit(nil)
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
	// Load journals the restore itself: that is the next event after the held 20.
	events := m2.Journal().Events(0, 0)
	if got := events[len(events)-1].Seq; got != 21 {
		t.Errorf("next event seq %d; want 21", got)
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
