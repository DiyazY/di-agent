package statemap

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Persistence exists because an agent whose value is accumulated ground truth loses
// all of it on restart otherwise: every learned relationship, every confidence, and
// the whole journal. Two consequences follow, and the second is the one that matters
// most for a system meant to be auditable.
//
// A restarted agent that has forgotten everything is back at cold start, reasoning
// from priors on a system it has already watched for a week. And "why did you do that
// yesterday" becomes unanswerable, which makes the audit trail an artefact of one
// process lifetime rather than of the agent.
//
// What is deliberately NOT persisted: the paired-observation windows. They are the
// estimator's short-term memory — the last sixty pairs — and restoring them would
// restore a claim about simultaneity between observations taken before a restart and
// after it, across a gap of unknown length. The learned strengths and their
// confidences survive; the raw pairs behind them do not, so the estimator resumes
// from what it concluded rather than from what it was mid-way through concluding.

// SnapshotVersion identifies the on-disk format. A snapshot from a different version
// is refused rather than guessed at: restoring a misread field would put wrong values
// into a model the agent then reasons from, which is worse than starting cold.
// Version 2 named the property and relationship fields explicitly. A version 1 file
// would half-load under those names — Go's decoder matches "ID" to "id"
// case-insensitively but not "NObservations" to "n_observations" — so the values would
// arrive and the observation counts and timestamps would not. That is the silent
// partial restore this field exists to prevent, so the version moved with the format.
const SnapshotVersion = 2

// Snapshot is the persisted form of a map.
type Snapshot struct {
	Version int `json:"version"`

	// Owner is the system the snapshot describes, so a snapshot moved between hosts
	// can be recognised as foreign instead of adopted as local history.
	Owner string `json:"owner,omitempty"`

	SavedAt  time.Time `json:"saved_at"`
	Revision uint64    `json:"revision"`

	Properties    []Property     `json:"properties"`
	Relationships []Relationship `json:"relationships"`

	// Events carries the journal so the audit trail survives a restart. Decisions are
	// inside their events, which is how they are reconstructed.
	Events []Event `json:"events"`

	// JournalDropped preserves the count of entries the journal had already dropped,
	// so a reader after a restart still knows the record is partial.
	JournalDropped uint64 `json:"journal_dropped"`
}

// Snapshot captures the map's state.
func (m *Map) Snapshot() Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	s := Snapshot{
		Version:  SnapshotVersion,
		Owner:    m.owner,
		SavedAt:  m.now(),
		Revision: m.revision,
	}
	for _, p := range m.properties {
		s.Properties = append(s.Properties, *p)
	}
	for _, r := range m.relationships {
		s.Relationships = append(s.Relationships, *r)
	}
	s.Events = m.journal.Events(0, 0)
	_, dropped, _ := m.journal.Stats()
	s.JournalDropped = dropped
	return s
}

// Save writes a snapshot to path, atomically.
//
// The write goes to a temporary file and is renamed, because a snapshot half-written
// when the process died would be read back as authoritative state on the next start.
// A partially-restored map is worse than an absent one: it looks like knowledge.
func (m *Map) Save(path string) error {
	if path == "" {
		return fmt.Errorf("no snapshot path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating snapshot directory: %w", err)
	}
	snap := m.Snapshot()
	blob, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding snapshot: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o644); err != nil {
		return fmt.Errorf("writing snapshot: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replacing snapshot: %w", err)
	}
	return nil
}

// Load restores a snapshot into an empty map.
//
// Restoring into a non-empty map is refused: merging a snapshot with a seeded model
// would silently pick a winner for every property declared in both, and the choice
// would be invisible. A caller wanting both should load first and seed afterwards,
// where re-declaration rules apply and are journalled.
func (m *Map) Load(path string) (bool, error) {
	blob, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil // nothing saved yet: a first start, not a failure
	}
	if err != nil {
		return false, fmt.Errorf("reading snapshot: %w", err)
	}

	var snap Snapshot
	if err := json.Unmarshal(blob, &snap); err != nil {
		return false, fmt.Errorf("decoding snapshot: %w", err)
	}
	if snap.Version != SnapshotVersion {
		return false, fmt.Errorf(
			"snapshot is version %d, this agent writes version %d: refusing to guess at "+
				"the difference, since a misread field would become state the agent reasons from",
			snap.Version, SnapshotVersion)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// A snapshot from another system is refused. Restoring it would give this agent a
	// week of another machine's observations as its own history, at high confidence,
	// and nothing downstream could tell the difference — the values are plausible and
	// the provenance says "observed". This happens for mundane reasons: a state file
	// copied with a config directory, an image built from a running node.
	if snap.Owner != "" && m.owner != "" && snap.Owner != m.owner {
		return false, fmt.Errorf(
			"snapshot at %s describes %q but this agent models %q: refusing to adopt "+
				"another system's observations as this one's history",
			path, snap.Owner, m.owner)
	}

	if len(m.properties) > 0 || len(m.relationships) > 0 {
		return false, fmt.Errorf("refusing to load a snapshot into a map that already "+
			"holds %d properties and %d relationships: load before seeding, so "+
			"re-declaration rules apply and are journalled",
			len(m.properties), len(m.relationships))
	}

	for i := range snap.Properties {
		p := snap.Properties[i]
		m.properties[p.ID] = &p
	}
	for i := range snap.Relationships {
		r := snap.Relationships[i]
		if r.ID == "" {
			r.ID = RelationshipID(r.From, r.To, r.Label)
		}
		m.relationships[r.ID] = &r
	}
	m.revision = snap.Revision
	m.journal.restore(snap.Events, snap.JournalDropped)

	// The estimator resumes from its conclusions, not its working memory: the pair
	// windows are not restored, so the first pairs after a restart rebuild support
	// rather than continuing a window that spans the gap.
	m.latest = make(map[string]observation)
	m.windows = make(map[string]*pairWindow)

	m.bump(EventPropertyRedeclared, "", "restore", map[string]any{
		"restored_from":  path,
		"saved_at":       snap.SavedAt,
		"properties":     len(snap.Properties),
		"relationships":  len(snap.Relationships),
		"journal_events": len(snap.Events),
		"note": "pair windows are not restored; the estimator resumes from the strengths " +
			"it had concluded, and rebuilds support from the first new pairs",
	}, m.now())
	return true, nil
}

// restore reinstates journal entries from a snapshot.
func (j *Journal) restore(events []Event, dropped uint64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.events = j.events[:0]
	j.decisions = make(map[string]*Decision, len(events))
	for _, e := range events {
		if len(j.events) >= j.capacity {
			j.dropped++
			continue
		}
		j.events = append(j.events, e)
		if e.Decision != nil {
			d := *e.Decision
			j.decisions[d.ID] = &d
		}
	}
	j.dropped += dropped
}
