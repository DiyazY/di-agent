package statemap

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/DiyazY/di-agent/pkg/stats"
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
//
// Save keeps one previous generation, path+".prev". A snapshot that cannot be read
// is then not the end of the model: Load falls back to the previous one, and when
// neither is usable the agent starts cold with restore.failed as the first journal
// event rather than crash-looping. A version or owner mismatch is still refused, with
// no fallback — that is a deliberate refusal, not damage.

// SnapshotVersion identifies the on-disk format. Other versions are refused rather
// than guessed at: restoring a misread field would
// put wrong values into a model the agent then reasons from, which is worse than cold.
// Version 2 named the property and relationship fields explicitly. A version 1 file
// would half-load under those names — Go's decoder matches "ID" to "id"
// case-insensitively but not "NObservations" to "n_observations" — so the values would
// arrive and the observation counts and timestamps would not. That is the silent
// partial restore this field exists to prevent, so the version moved with the format.
// Version 3 renamed a relationship's recent layer from "strength" to "recent".
// Version 4 added the intervention counters. Version 5 distinguishes associations
// from directional relationships. Version 6 persists per-source intervention slopes.
// Version 7 adds the journal's incarnation and sequence. Version 8 adds the
// calibration record of the map's own predictions. Version 9 adds `retired_at` and
// the tombstones of forgotten properties.
const SnapshotVersion = 9

// Snapshot is the persisted form of a map.
type Snapshot struct {
	// Discovery is the proposer's bounded raw evidence, validated separately on
	// restore against its policy and freshness. The state estimator remains cold.
	Discovery json.RawMessage `json:"discovery,omitempty"`

	// Loops is the acting loops' learned state — relief effects and counters,
	// admission outcomes, peer trust — opaque to the map and decoded by the agent
	// that installed the capture.
	Loops   json.RawMessage `json:"loops,omitempty"`
	Version int             `json:"version"`

	// Owner is the system the snapshot describes, so a snapshot moved between hosts
	// can be recognised as foreign instead of adopted as local history.
	Owner string `json:"owner,omitempty"`

	SavedAt  time.Time `json:"saved_at"`
	Revision uint64    `json:"revision"`

	Properties    []Property     `json:"properties"`
	Relationships []Relationship `json:"relationships"`

	// Interventions are the counters by pair "from->to", saved apart from the
	// relationships because they exist before any relationship does.
	Interventions map[string]Interventions `json:"interventions,omitempty"`

	// Forgotten is the tombstones: id to the time the property was retired.
	Forgotten map[string]time.Time `json:"forgotten,omitempty"`

	// Calibration is how the map's own predictions have fared, by kind and basis.
	Calibration []CalibrationEntry `json:"calibration"`

	// Events carries the journal so the audit trail survives a restart. Decisions are
	// inside their events, which is how they are reconstructed.
	Events []Event `json:"events"`

	// Incarnation and Seq are the journal's life count and last number handed
	// out, so a restored agent never reissues an id or an event number.
	Incarnation uint64 `json:"incarnation"`
	Seq         uint64 `json:"seq"`

	// JournalDropped preserves the count of entries the journal had already dropped,
	// so a reader after a restart still knows the record is partial.
	JournalDropped uint64 `json:"journal_dropped"`
}

// Snapshot captures the map's state.
func (m *Map) Snapshot() Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	s := Snapshot{
		Version:   SnapshotVersion,
		Owner:     m.owner,
		SavedAt:   m.now(),
		Revision:  m.revision,
		Discovery: append(json.RawMessage(nil), m.savedDiscovery...),
		Loops:     append(json.RawMessage(nil), m.savedLoops...),
	}
	for _, p := range m.properties {
		s.Properties = append(s.Properties, p.clone()) // Save encodes it after the lock is released
	}
	for _, r := range m.relationships {
		s.Relationships = append(s.Relationships, *r)
	}
	for k, iv := range m.interventions {
		if s.Interventions == nil {
			s.Interventions = map[string]Interventions{}
		}
		s.Interventions[k] = *iv
	}
	if len(m.forgotten) > 0 {
		s.Forgotten = maps.Clone(m.forgotten)
	}
	s.Calibration = m.calibrationListLocked()
	s.Events = m.journal.Events(0, 0)
	_, dropped, _ := m.journal.Stats()
	s.JournalDropped = dropped
	s.Incarnation = m.journal.Incarnation()
	s.Seq = m.journal.Seq()
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
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	snap := m.Snapshot()
	m.mu.RLock()
	capture := m.discoveryCheckpoint
	m.mu.RUnlock()
	if capture != nil {
		// The map and proposer have separate locks, so these captures are not a
		// cross-component transaction. RestoreDiscovery reconciles candidates
		// against persisted relationships before publishing the restored queue.
		raw, err := capture()
		if err != nil {
			return fmt.Errorf("capturing discovery evidence: %w", err)
		}
		snap.Discovery = raw
	}
	m.mu.RLock()
	captureLoops := m.loopsCheckpoint
	m.mu.RUnlock()
	if captureLoops != nil {
		raw, err := captureLoops()
		if err != nil {
			return fmt.Errorf("capturing the loops' state: %w", err)
		}
		snap.Loops = raw
	}
	blob, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("encoding snapshot: %w", err)
	}

	// A uniquely-named temp file, not a fixed path+".tmp". Two saves can be in flight
	// at once — the periodic save loop and the synchronous shutdown save race at
	// SIGINT — and a shared temp path lets them clobber each other's bytes before
	// either renames. os.CreateTemp also creates the file 0o600, which is the right
	// mode for a per-machine private snapshot (journal, decisions, telemetry-derived
	// values) rather than the world-readable 0o644 this used to write.
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("creating snapshot temp file: %w", err)
	}
	tmp := f.Name()
	// Best-effort cleanup: a save that fails before the rename must not leave a
	// litter of half-written temp files beside the real snapshot. Harmless after a
	// successful rename, where tmp no longer exists.
	defer func() { _ = os.Remove(tmp) }()

	if _, err := f.Write(blob); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing snapshot: %w", err)
	}
	// Flushed before the rename: without it a power cut can leave the new name
	// pointing at a file whose bytes never reached the disk, an empty snapshot where
	// a good one stood, and the next start refuses it.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("flushing snapshot: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing snapshot: %w", err)
	}
	// The previous generation is moved aside only now that the new one is fully on
	// disk under the temp name, so a crash between the renames leaves .prev good.
	if err := os.Rename(path, path+".prev"); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("keeping the previous snapshot: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replacing snapshot: %w", err)
	}
	// And the directory after it, so the rename itself survives a power cut. The
	// snapshot is in place by now, so a failure here says so rather than reading as
	// a failed save.
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("snapshot replaced, but flushing its directory failed: %w", err)
	}
	return nil
}

// syncDir flushes a directory. A filesystem that cannot flush one (some network
// and FUSE mounts refuse with EINVAL or ENOTSUP) has nothing to flush, and is not
// a failure.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}

// CanSave reports whether a snapshot can be written at path, by creating its
// directory and a temporary file beside it. An agent checks this at start: a
// path it cannot write would otherwise fail at every save, in the log while the
// operator believes the map is being kept.
func CanSave(path string) error {
	if path == "" {
		return fmt.Errorf("no snapshot path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// Restore reports what Load did.
type Restore struct {
	Loaded       bool   // a snapshot was restored
	From         string // the file it came from: path or path+".prev"
	CurrentError string // why path was not used ("" when it was, or was absent on a cold start)
	PrevError    string // why path+".prev" was not used, when it was tried
}

// ErrSnapshotRefused marks a snapshot that is deliberately not adopted: another
// version, another owner, or a map that already holds state. It is never fallen back
// from, since the previous generation would carry the same refusal or hide it.
var ErrSnapshotRefused = errors.New("snapshot refused")

// Load restores a snapshot into an empty map. It tries path, then path+".prev" (the
// generation Save moved aside) when path is absent or unreadable. A refusal is
// returned as an error without trying the other file. When neither file is usable
// but at least one exists, the map starts cold with restore.failed as its first
// journal event; when neither exists it is a first start and nothing is recorded.
//
// Restoring into a non-empty map is refused: merging a snapshot with a seeded model
// would silently pick a winner for every property declared in both, and the choice
// would be invisible. A caller wanting both should load first and seed afterwards,
// where re-declaration rules apply and are journalled.
func (m *Map) Load(path string) (Restore, error) {
	snap, err := readSnapshot(path)
	from := path
	var curErr, prevErr error
	if err == nil {
		err = m.restoreLocked(snap, from, "")
	}
	if err == nil {
		return m.reserveIncarnation(path, from, snap, "")
	}
	if errors.Is(err, ErrSnapshotRefused) {
		return Restore{}, err
	}
	curErr = err

	prev := path + ".prev"
	snap, err = readSnapshot(prev)
	if err == nil {
		err = m.restoreLocked(snap, prev, curErr.Error())
	}
	if err == nil {
		return m.reserveIncarnation(path, prev, snap, curErr.Error())
	}
	if errors.Is(err, ErrSnapshotRefused) {
		return Restore{}, err
	}
	prevErr = err

	if os.IsNotExist(curErr) && os.IsNotExist(prevErr) {
		return Restore{}, nil // nothing saved yet: a first start, not a failure
	}
	m.mu.Lock()
	m.bump(EventRestoreFailed, path, "restore", map[string]any{
		"path":       path,
		"error":      curErr.Error(),
		"prev_error": prevErr.Error(),
	}, m.now())
	m.mu.Unlock()
	return Restore{CurrentError: curErr.Error(), PrevError: prevErr.Error()}, nil
}

// reserveIncarnation writes the new incarnation at once, so a crash before the next
// save cannot bring this life's number back, and with it ids this life has already
// issued.
func (m *Map) reserveIncarnation(path, from string, snap Snapshot, curErr string) (Restore, error) {
	// After a fallback to .prev this save rotates the corrupt current into .prev; the freshly written current is the good generation.
	if err := m.Save(path); err != nil {
		return Restore{}, fmt.Errorf("reserving incarnation %d: %w", snap.Incarnation+1, err)
	}
	return Restore{Loaded: true, From: from, CurrentError: curErr}, nil
}

// readSnapshot reads, decodes and version-checks one snapshot file. A version
// mismatch wraps ErrSnapshotRefused; a missing file satisfies os.IsNotExist.
func readSnapshot(path string) (Snapshot, error) {
	var snap Snapshot
	blob, err := os.ReadFile(path)
	if err != nil {
		return snap, err
	}
	if err := json.Unmarshal(blob, &snap); err != nil {
		return snap, fmt.Errorf("decoding snapshot %s: %w", path, err)
	}
	if snap.Version != SnapshotVersion {
		return snap, fmt.Errorf(
			"%w: snapshot %s is version %d, this agent writes version %d: refusing to guess at "+
				"the difference, since a misread field would become state the agent reasons from",
			ErrSnapshotRefused, path, snap.Version, SnapshotVersion)
	}
	return snap, nil
}

// restoreLocked restores snap under the write lock. from is the file it was read
// from; currentError, when non-empty, says why the current file was passed over.
func (m *Map) restoreLocked(snap Snapshot, from, currentError string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// A snapshot from another system is refused. Restoring it would give this agent a
	// week of another machine's observations as its own history, at high confidence,
	// and nothing downstream could tell the difference — the values are plausible and
	// the provenance says "observed". This happens for mundane reasons: a state file
	// copied with a config directory, an image built from a running node.
	if snap.Owner != "" && m.owner != "" && snap.Owner != m.owner {
		return fmt.Errorf(
			"%w: snapshot at %s describes %q but this agent models %q: refusing to adopt "+
				"another system's observations as this one's history",
			ErrSnapshotRefused, from, snap.Owner, m.owner)
	}

	if len(m.properties) > 0 || len(m.relationships) > 0 {
		return fmt.Errorf("%w: refusing to load a snapshot into a map that already "+
			"holds %d properties and %d relationships: load before seeding, so "+
			"re-declaration rules apply and are journalled",
			ErrSnapshotRefused, len(m.properties), len(m.relationships))
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
	for k, iv := range snap.Interventions {
		iv := iv
		m.interventions[k] = &iv
	}
	maps.Copy(m.forgotten, snap.Forgotten)
	for _, e := range snap.Calibration {
		m.calibration[CalibrationKey{e.Kind, e.Basis}] = e.Calibration
	}
	m.revision = snap.Revision
	if m.cfg.RetireAfter > 0 {
		m.graceUntil = m.mono() + m.cfg.RetireAfter
	}
	m.baselineSweepLocked() // a restore is a first reading of both clocks, so a step before the first sweep is seen
	m.savedDiscovery = append(json.RawMessage(nil), snap.Discovery...)
	m.savedLoops = append(json.RawMessage(nil), snap.Loops...)
	inc := snap.Incarnation + 1
	m.journal.restore(snap.Events, snap.JournalDropped, snap.Seq, inc)

	// The estimator resumes from its conclusions, not its working memory: the pair
	// windows are not restored, so the first pairs after a restart rebuild support
	// rather than continuing a window that spans the gap.
	m.latest = make(map[string]observation)
	m.windows = make(map[string]*stats.PairWindow)

	detail := map[string]any{
		"restored_from":  from,
		"saved_at":       snap.SavedAt,
		"properties":     len(snap.Properties),
		"relationships":  len(snap.Relationships),
		"journal_events": len(snap.Events),
		"incarnation":    inc,
		"note": "pair windows are not restored; the estimator resumes from the strengths " +
			"it had concluded, and rebuilds support from the first new pairs",
	}
	if currentError != "" {
		detail["current_error"] = currentError
	}
	m.bump(EventPropertyRedeclared, "", "restore", detail, m.now())
	return nil
}

// DiscoveryPersistence installs the proposer checkpoint callback and returns any
// saved evidence. The callback runs outside the state lock to avoid lock inversion.
func (m *Map) DiscoveryPersistence(capture func() (json.RawMessage, error)) json.RawMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.discoveryCheckpoint = capture
	return append(json.RawMessage(nil), m.savedDiscovery...)
}

// LoopsPersistence installs the loops' capture callback, run by Save outside the
// state lock as the discovery capture is, and returns the blob Load restored — nil
// on a cold start.
func (m *Map) LoopsPersistence(capture func() (json.RawMessage, error)) json.RawMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loopsCheckpoint = capture
	return append(json.RawMessage(nil), m.savedLoops...)
}

// DiscardDiscoveryCheckpoint forgets a checkpoint that failed validation while
// retaining the restored map, journal, and relationships.
func (m *Map) DiscardDiscoveryCheckpoint() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.savedDiscovery = nil
}

// restore reinstates journal entries from a snapshot. Each event keeps its own
// seq; the counter resumes past both the saved one and the highest held.
func (j *Journal) restore(events []Event, dropped, seq, incarnation uint64) {
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
	j.rebuildCitedByLocked()
	j.dropped += dropped
	j.seq = seq
	for _, e := range j.events {
		if e.Seq > j.seq {
			j.seq = e.Seq
		}
	}
	j.incarnation = incarnation
}
