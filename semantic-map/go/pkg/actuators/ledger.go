package actuators

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/DiyazY/di-agent/internal/testhooks"
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
	// Pending marks an entry written ahead of its Set and not yet committed:
	// Applied is the value asked for, and PrevApplied the step before it, when
	// PrevSet; Held settles what is in force.
	Pending     bool    `json:"pending,omitempty"`
	PrevApplied float64 `json:"prev_applied,omitempty"`
	PrevSet     bool    `json:"prev_set,omitempty"`
	// PrevDecisionID and PrevExpires are the step before's decision and expiry,
	// with PrevSet: what a step that never took goes back to.
	PrevDecisionID string    `json:"prev_decision_id,omitempty"`
	PrevExpires    time.Time `json:"prev_expires,omitempty"`
}

// validate is why e cannot be in the ledger, or nil: it names an action, a
// subject and a node; its values are finite; a committed entry has no step
// before it, and a pending one has a step before it only when PrevSet; and an
// entry with no original setting records none.
func (e Entry) validate() error {
	bad := func(why string) error {
		return fmt.Errorf("invalid ledger entry %s on %s (node %q): %s", e.Action, e.Subject, e.NodeID, why)
	}
	finite := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
	switch {
	case e.Action == "":
		return bad("no action")
	case e.Subject == "":
		return bad("no subject")
	case e.NodeID == "":
		return bad("no node id")
	case !finite(e.Applied) || !finite(e.Original) || !finite(e.PrevApplied):
		return bad(fmt.Sprintf("a value is not finite (applied %v, original %v, prev_applied %v)", e.Applied, e.Original, e.PrevApplied))
	case !e.Pending && (e.PrevSet || e.PrevApplied != 0):
		return bad("committed, yet it records a step before it")
	case !e.PrevSet && (e.PrevApplied != 0 || e.PrevDecisionID != "" || !e.PrevExpires.IsZero()):
		return bad("a step before (prev_applied, prev_decision_id or prev_expires) without prev_set")
	case !e.OriginalSet && e.Original != 0:
		return bad("an original value with original_set false")
	}
	return nil
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
	saveErr error // when set, saving fails with it; for tests
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
	es, err := ReadLedger(path)
	if err != nil {
		return nil, err
	}
	l := &Ledger{path: path, entries: map[ledgerKey]Entry{}}
	for _, e := range es {
		l.entries[ledgerKey{e.Action, e.Subject}] = e
	}
	return l, nil
}

// ReadLedger loads the entries of the ledger at path without ever writing it or
// its directory, as a dry run does to show what a real run left in force. A
// missing file is no entries; an unreadable, corrupt or invalid one is an error
// naming path.
func ReadLedger(path string) ([]Entry, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
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
		if err := e.validate(); err != nil {
			return nil, fmt.Errorf("ledger %s: %w", path, err)
		}
	}
	return f.Entries, nil
}

// ReadLedgerLenient is ReadLedger that does not refuse the whole file for an
// invalid entry: it returns the valid entries and, for each invalid one, its
// index in the file and why. An unreadable or corrupt file is still an error.
// It is for an agent that changes nothing and only shows what the ledger holds.
func ReadLedgerLenient(path string) (valid []Entry, invalid []string, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("ledger %s: %w", path, err)
	}
	var f ledgerFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, nil, fmt.Errorf("ledger %s: corrupt: %w", path, err)
	}
	if f.Version != 1 {
		return nil, nil, fmt.Errorf("ledger %s: unsupported version %d", path, f.Version)
	}
	for i, e := range f.Entries {
		if err := e.validate(); err != nil {
			invalid = append(invalid, fmt.Sprintf("entry %d: %v", i, err))
			continue
		}
		valid = append(valid, e)
	}
	return valid, invalid, nil
}

// Put records e, which must be valid. An existing entry for the key must be
// given its own Original back: the ledger holds what the setting was before the
// agent's first change, however many followed, and a Put that would change it is
// a caller's bug, refused.
func (l *Ledger) Put(e Entry) error {
	if err := e.validate(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	k := ledgerKey{e.Action, e.Subject}
	prev, had := l.entries[k]
	if had && (e.OriginalSet != prev.OriginalSet || e.Original != prev.Original) {
		return fmt.Errorf("ledger entry %s on %s: original %v (set %v) would replace the recorded original %v (set %v)",
			e.Action, e.Subject, e.Original, e.OriginalSet, prev.Original, prev.OriginalSet)
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

// For lists the entries of action, in All's order. It has the shape of Env.Held.
func (l *Ledger) For(action string) []Entry {
	var out []Entry
	for _, e := range l.All() {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

// saveLocked writes the ledger to a temp file beside the real one, syncs it,
// renames it over and syncs the directory, so a crash leaves the old file or the
// new, never half of one, and a change recorded stays recorded.
func (l *Ledger) saveLocked() error {
	if l.saveErr != nil {
		return l.saveErr
	}
	if h := testhooks.LedgerSave; h != nil {
		if err := h(); err != nil {
			return err
		}
	}
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
	if err := syncDir(filepath.Dir(l.path)); err != nil {
		return fmt.Errorf("ledger %s: syncing its directory: %w", l.path, err)
	}
	return nil
}

// syncDir makes a rename into dir durable: until the directory is synced, a
// crash may leave the old name pointing at the old file. A platform that cannot
// sync a directory (EINVAL, or Windows, which cannot open one to flush) has
// nothing more to give.
func syncDir(dir string) error {
	if goruntime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	if errors.Is(err, syscall.EINVAL) {
		return nil
	}
	return err
}
