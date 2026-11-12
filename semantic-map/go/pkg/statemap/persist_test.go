package statemap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCanSaveCreatesTheDirectoryAndLeavesNothing: the start-up check makes the
// snapshot's directory and leaves no probe file behind.
func TestCanSaveCreatesTheDirectoryAndLeavesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "di-agent")
	if err := CanSave(filepath.Join(dir, "state.json")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("left %v behind", entries)
	}
}

// TestCanSaveRefusesAnUnwritableDirectory: a path the agent cannot write is
// reported at start, not discovered at the first periodic save.
func TestCanSaveRefusesAnUnwritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := CanSave(filepath.Join(dir, "state.json")); err == nil {
		t.Fatal("CanSave succeeded on a read-only directory")
	}
	if err := CanSave(""); err == nil {
		t.Fatal("CanSave succeeded with no path")
	}
}

// TestASaveWaitingItsTurnSavesTheLaterState: a save that starts while another is
// still writing waits for it and then snapshots the map as it is by then, so the
// later state is the one left on disk.
func TestASaveWaitingItsTurnSavesTheLaterState(t *testing.T) {
	m, c := newTestMap(t, Config{ConvergenceObservations: 10, Alpha: 0.5})
	if err := m.DeclareProperty(Property{ID: "cpu", Kind: Observed, Range: [2]float64{0, 1}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "state.json")

	m.saveMu.Lock() // a save in progress
	done := make(chan error, 1)
	go func() { done <- m.Save(path) }()
	// Long enough for a save that did not wait its turn to have taken its snapshot
	// already, so without the turn-taking this test fails; with it, the wait only
	// delays the save.
	time.Sleep(50 * time.Millisecond)
	if err := m.Observe("cpu", 0.7, c.now()); err != nil {
		t.Fatal(err)
	}
	want := m.Revision()
	m.saveMu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snap Snapshot
	if err := json.Unmarshal(blob, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Revision != want {
		t.Fatalf("saved revision %d; want %d, the state when the save got its turn", snap.Revision, want)
	}
}

// savedTwice saves a map at two revisions to path and returns them.
func savedTwice(t *testing.T) (path string, r1, r2 uint64) {
	t.Helper()
	m := New(Config{AdmitUnknown: true}, NewJournal(0))
	path = filepath.Join(t.TempDir(), "state.json")
	now := time.Now()
	if err := m.Observe("cpu", 0.5, now); err != nil {
		t.Fatal(err)
	}
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}
	r1 = m.Revision()
	if err := m.Observe("ram", 0.5, now); err != nil {
		t.Fatal(err)
	}
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}
	return path, r1, m.Revision()
}

func snapRevision(t *testing.T, path string) uint64 {
	t.Helper()
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s Snapshot
	if err := json.Unmarshal(blob, &s); err != nil {
		t.Fatal(err)
	}
	return s.Revision
}

// TestSaveKeepsOnePreviousGeneration: each save moves the last one to path.prev,
// and nothing else is left beside them.
func TestSaveKeepsOnePreviousGeneration(t *testing.T) {
	m := New(Config{AdmitUnknown: true}, NewJournal(0))
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	var revs []uint64
	for _, id := range []string{"a", "b", "c"} {
		if err := m.Observe(id, 0.5, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := m.Save(path); err != nil {
			t.Fatal(err)
		}
		revs = append(revs, m.Revision())
	}
	if got := snapRevision(t, path); got != revs[2] {
		t.Errorf("current is revision %d; want %d", got, revs[2])
	}
	if got := snapRevision(t, path+".prev"); got != revs[1] {
		t.Errorf("previous is revision %d; want %d", got, revs[1])
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name() != "state.json" || entries[1].Name() != "state.json.prev" {
		t.Fatalf("directory holds %v; want state.json and state.json.prev", entries)
	}
}

func TestLoadFallsBackToPrevOnACorruptCurrent(t *testing.T) {
	path, _, _ := savedTwice(t)
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, blob[:len(blob)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(Config{AdmitUnknown: true}, NewJournal(0))
	r, err := m.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Loaded || r.From != path+".prev" || r.CurrentError == "" {
		t.Fatalf("Restore = %+v; want loaded from .prev with a CurrentError", r)
	}
	found := false
	for _, e := range m.Journal().Events(0, 0) {
		if e.Kind == EventPropertyRedeclared && e.Actor == "restore" {
			found = true
			if e.Detail["current_error"] == nil || e.Detail["restored_from"] != path+".prev" {
				t.Errorf("restore detail %v lacks current_error / restored_from", e.Detail)
			}
		}
	}
	if !found {
		t.Error("no restore event journalled")
	}
}

func TestLoadUsesPrevWhenCurrentIsAbsent(t *testing.T) {
	path, _, _ := savedTwice(t)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	m := New(Config{AdmitUnknown: true}, NewJournal(0))
	r, err := m.Load(path)
	if err != nil || !r.Loaded || r.From != path+".prev" {
		t.Fatalf("Restore = %+v, err %v; want loaded from .prev", r, err)
	}
}

func TestLoadStartsColdWithRestoreFailedWhenBothAreCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	for _, p := range []string{path, path + ".prev"} {
		if err := os.WriteFile(p, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m := New(Config{AdmitUnknown: true}, NewJournal(0))
	r, err := m.Load(path)
	if err != nil || r.Loaded {
		t.Fatalf("Restore = %+v, err %v; want a cold start without error", r, err)
	}
	if r.CurrentError == "" || r.PrevError == "" {
		t.Errorf("Restore = %+v; want both errors named", r)
	}
	ev := m.Journal().Events(0, 0)
	if len(ev) == 0 || ev[0].Kind != "restore.failed" {
		t.Fatalf("first event %v; want restore.failed", ev)
	}
	if ev[0].Detail["error"] == "" || ev[0].Detail["prev_error"] == "" {
		t.Errorf("detail %v lacks error / prev_error", ev[0].Detail)
	}
}

func TestLoadStillRefusesAVersionMismatchWithoutFallback(t *testing.T) {
	path, _, _ := savedTwice(t)
	if err := os.WriteFile(path, []byte(`{"version":4}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(Config{AdmitUnknown: true}, NewJournal(0))
	r, err := m.Load(path)
	if err == nil || !errors.Is(err, ErrSnapshotRefused) || r.Loaded {
		t.Fatalf("Restore = %+v, err %v; want a refusal", r, err)
	}
	if m.Census().PropertiesTotal != 0 {
		t.Error("the previous generation was restored behind a refusal")
	}
}

func TestLoadStillRefusesAnotherOwnerWithoutFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	far := New(Config{Owner: "node-2", AdmitUnknown: true}, NewJournal(0))
	for i := 0; i < 2; i++ {
		if err := far.Observe("cpu", 0.5, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := far.Save(path); err != nil {
			t.Fatal(err)
		}
	}
	local := New(Config{Owner: "node-1", AdmitUnknown: true}, NewJournal(0))
	r, err := local.Load(path)
	if err == nil || !errors.Is(err, ErrSnapshotRefused) || r.Loaded {
		t.Fatalf("Restore = %+v, err %v; want a refusal", r, err)
	}
	if local.Census().PropertiesTotal != 0 {
		t.Error("another system's snapshot was adopted")
	}
}

func TestFirstStartWithNeitherFileIsSilent(t *testing.T) {
	m := New(Config{AdmitUnknown: true}, NewJournal(0))
	r, err := m.Load(filepath.Join(t.TempDir(), "state.json"))
	if err != nil || r.Loaded || r.CurrentError != "" {
		t.Fatalf("Restore = %+v, err %v; want a quiet first start", r, err)
	}
	if ev := m.Journal().Events(0, 0); len(ev) != 0 {
		t.Errorf("first start journalled %v", ev)
	}
}

// TestSnapshotIsCompact: the snapshot is one line of JSON; indentation is bytes a
// periodic write pays for and nobody reads.
func TestSnapshotIsCompact(t *testing.T) {
	m := New(Config{AdmitUnknown: true}, NewJournal(0))
	now := time.Now()
	for _, id := range []string{"cpu", "ram"} {
		if err := m.Observe(id, 0.5, now); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(blob, []byte("\n")); n != 0 {
		t.Errorf("snapshot has %d newlines; want compact JSON", n)
	}
	if !json.Valid(blob) {
		t.Error("snapshot is not valid JSON")
	}
}

// TestLoopsBlobRoundTrips: the loops' learned state is saved with the map and
// handed back, unread, to whoever installs the callback after a load.
func TestLoopsBlobRoundTrips(t *testing.T) {
	m, _ := newTestMap(t, Config{})
	if got := m.LoopsPersistence(func() (json.RawMessage, error) { return json.RawMessage(`{"a":1}`), nil }); got != nil {
		t.Fatalf("cold start returned %s", got)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}
	m2, _ := newTestMap(t, Config{})
	if _, err := m2.Load(path); err != nil {
		t.Fatal(err)
	}
	if got := m2.LoopsPersistence(nil); string(got) != `{"a":1}` {
		t.Fatalf("restored %s, want {\"a\":1}", got)
	}
}

// TestLoopsCaptureRunsOutsideTheLock: the capture may read the map, as the
// loops do, without deadlocking the save that called it.
func TestLoopsCaptureRunsOutsideTheLock(t *testing.T) {
	m, _ := newTestMap(t, Config{})
	m.LoopsPersistence(func() (json.RawMessage, error) {
		_ = m.Census()
		return json.RawMessage(`{}`), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Save(filepath.Join(t.TempDir(), "state.json")) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("Save deadlocked on a capture that reads the map")
	}
}
