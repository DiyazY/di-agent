package statemap

import (
	"bytes"
	"encoding/json"
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

// TestSaveLeavesOnlyTheSnapshot: a saved snapshot replaces the last, and no
// temporary file is left beside it.
func TestSaveLeavesOnlyTheSnapshot(t *testing.T) {
	m, _ := newTestMap(t, Config{})
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	for i := 0; i < 2; i++ {
		if err := m.Save(path); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		t.Fatalf("directory holds %v; want only state.json", entries)
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
