package statemap

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoveryCheckpointSharesAtomicStateSnapshot(t *testing.T) {
	m, _ := newTestMap(t, Config{})
	path := filepath.Join(t.TempDir(), "state.json")
	raw := json.RawMessage(`{"Version":1,"progress":2}`)
	m.DiscoveryPersistence(func() (json.RawMessage, error) {
		m.Census() // Capture must not run under the state mutex.
		return raw, nil
	})
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m.DiscoveryPersistence(func() (json.RawMessage, error) {
		return nil, fmt.Errorf("checkpoint failed")
	})
	if err := m.Save(path); err == nil || !strings.Contains(err.Error(), "checkpoint failed") {
		t.Fatalf("checkpoint failure hidden: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("failed capture changed the durable snapshot: %v", err)
	}
	q, _ := newTestMap(t, Config{})
	if r, err := q.Load(path); err != nil || !r.Loaded {
		t.Fatalf("load: %v %v", r.Loaded, err)
	}
	var checkpoint map[string]int
	if err := json.Unmarshal(q.DiscoveryPersistence(nil), &checkpoint); err != nil || checkpoint["progress"] != 2 {
		t.Fatalf("checkpoint did not survive atomic save/load: %v %v", checkpoint, err)
	}
}

func TestVersionFourSnapshotIsRejected(t *testing.T) {
	m, _ := newTestMap(t, Config{})
	path := filepath.Join(t.TempDir(), "v4.json")
	if err := os.WriteFile(path, []byte(`{"version":4,"revision":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if r, err := m.Load(path); err == nil || r.Loaded {
		t.Fatalf("version 4 should be rejected, got loaded=%v err=%v", r.Loaded, err)
	}
}
