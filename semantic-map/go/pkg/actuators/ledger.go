package actuators

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Entry is one change the agent made and still holds: what it set, and what the
// setting was before the agent's first change to (Subject, Action).
type Entry struct {
	Action      string    `json:"action"`
	Subject     string    `json:"subject"`
	DecisionID  string    `json:"decision_id"`
	Original    float64   `json:"original"`
	OriginalSet bool      `json:"original_set"` // false: there was no setting to give back
	Applied     float64   `json:"applied"`
	Expires     time.Time `json:"expires"`
	NodeID      string    `json:"node_id"`
}

type ledgerKey struct{ action, subject string }

type ledgerFile struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
}

// Ledger is the agent's record of its own changes, keyed by (action, subject).
// It is safe for concurrent use.
type Ledger struct {
	path string // "" keeps it in memory

	mu      sync.Mutex
	entries map[ledgerKey]Entry
}

// MemoryLedger is a ledger that never touches disk.
func MemoryLedger() *Ledger { return &Ledger{entries: map[ledgerKey]Entry{}} }

// OpenLedger loads the ledger at path. A missing file is an empty ledger; an
// unreadable or corrupt one is an error naming path. The directory is created if
// missing and must be writable.
func OpenLedger(path string) (*Ledger, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("ledger %s: %w", path, err)
	}
	probe, err := os.CreateTemp(dir, ".ledger-probe-*")
	if err != nil {
		return nil, fmt.Errorf("ledger %s: directory not writable: %w", path, err)
	}
	probe.Close()
	if err := os.Remove(probe.Name()); err != nil {
		return nil, fmt.Errorf("ledger %s: directory not writable: %w", path, err)
	}
	l := &Ledger{path: path, entries: map[ledgerKey]Entry{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ledger %s: %w", path, err)
	}
	var f ledgerFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("ledger %s: corrupt: %w", path, err)
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("ledger %s: unsupported version %d", path, f.Version)
	}
	for _, e := range f.Entries {
		l.entries[ledgerKey{e.Action, e.Subject}] = e
	}
	return l, nil
}

// Put records e. An existing entry for the key keeps its Original: the ledger
// holds what the setting was before the agent's first change, however many
// followed.
func (l *Ledger) Put(e Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := ledgerKey{e.Action, e.Subject}
	prev, had := l.entries[k]
	if had {
		e.Original, e.OriginalSet = prev.Original, prev.OriginalSet
	}
	l.entries[k] = e
	if err := l.saveLocked(); err != nil {
		if had {
			l.entries[k] = prev
		} else {
			delete(l.entries, k)
		}
		return err
	}
	return nil
}

// Delete forgets the entry for (action, subject). Deleting an absent one is not an error.
func (l *Ledger) Delete(action, subject string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := ledgerKey{action, subject}
	prev, had := l.entries[k]
	if !had {
		return nil
	}
	delete(l.entries, k)
	if err := l.saveLocked(); err != nil {
		l.entries[k] = prev
		return err
	}
	return nil
}

// All lists the entries, soonest-expiring first, then by subject, then action.
func (l *Ledger) All() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Entry, 0, len(l.entries))
	for _, e := range l.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Expires.Equal(out[j].Expires) {
			return out[i].Expires.Before(out[j].Expires)
		}
		if out[i].Subject != out[j].Subject {
			return out[i].Subject < out[j].Subject
		}
		return out[i].Action < out[j].Action
	})
	return out
}

// saveLocked writes the ledger to a temp file beside the real one, syncs it and
// renames it over, so a crash leaves the old file or the new, never half of one.
func (l *Ledger) saveLocked() error {
	if l.path == "" {
		return nil
	}
	es := make([]Entry, 0, len(l.entries))
	for _, e := range l.entries {
		es = append(es, e)
	}
	sort.Slice(es, func(i, j int) bool {
		if es[i].Subject != es[j].Subject {
			return es[i].Subject < es[j].Subject
		}
		return es[i].Action < es[j].Action
	})
	b, err := json.MarshalIndent(ledgerFile{Version: 1, Entries: es}, "", "  ")
	if err != nil {
		return fmt.Errorf("ledger %s: %w", l.path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(l.path), ".ledger-*")
	if err != nil {
		return fmt.Errorf("ledger %s: %w", l.path, err)
	}
	fail := func(err error) error {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("ledger %s: %w", l.path, err)
	}
	if _, err := tmp.Write(b); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("ledger %s: %w", l.path, err)
	}
	if err := os.Rename(tmp.Name(), l.path); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("ledger %s: %w", l.path, err)
	}
	return nil
}
