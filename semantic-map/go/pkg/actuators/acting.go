package actuators

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/DiyazY/di-agent/pkg/runtime"
)

// Acting applies actions through their actuators and keeps the ledger of what it
// changed: undo is the agent's, so an actuator only reads and sets. It is safe
// for concurrent use; calls are serialised so a read-then-set is never
// interleaved.
type Acting struct {
	nodeID string
	ledger *Ledger

	mu        sync.Mutex
	actuators map[string]Actuator
	real      []Entry // a dry run's view of the on-disk ledger: changes a real run holds
}

// NewActing builds an Acting for nodeID over the given actuators, keyed by the
// action each describes.
func NewActing(nodeID string, l *Ledger, as ...Actuator) *Acting {
	a := &Acting{nodeID: nodeID, ledger: l, actuators: map[string]Actuator{}}
	for _, x := range as {
		a.actuators[x.Describe().Action] = x
	}
	return a
}

// Actuator returns the actuator of action.
func (a *Acting) Actuator(action string) (Actuator, bool) {
	x, ok := a.actuators[action]
	return x, ok
}

// Descriptors lists every action's descriptor, sorted by action.
func (a *Acting) Descriptors() []Descriptor {
	out := make([]Descriptor, 0, len(a.actuators))
	for _, x := range a.actuators {
		out = append(out, x.Describe())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Action < out[j].Action })
	return out
}

// Unmanaged is a change the ledger holds that no loop of this agent gives back,
// with why: another node recorded it, no actuator is built for its action
// (relief off, or its lever removed), or a real run holds it while this is a
// dry run. It stays in force until something that manages it gives it back.
type Unmanaged struct {
	Entry
	Why string `json:"why"`
}

// HeldByRealRun records the entries of the on-disk ledger a dry run read
// without opening it for writing: changes an earlier real run left in force,
// which a dry run shows and never gives back.
func (a *Acting) HeldByRealRun(es []Entry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.real = append([]Entry(nil), es...)
}

// Unmanaged lists the ledger's changes this Acting does not give back, with
// why, sorted by action, then soonest-expiring first: another node's, an action
// it has no actuator for, and those a real run holds while this is a dry run.
func (a *Acting) Unmanaged() []Unmanaged {
	a.mu.Lock()
	real := append([]Entry(nil), a.real...)
	a.mu.Unlock()
	var out []Unmanaged
	add := func(e Entry, why string) {
		switch _, built := a.actuators[e.Action]; {
		case e.NodeID != a.nodeID:
			why = fmt.Sprintf("recorded by node %s; this agent is %s; not managed", e.NodeID, a.nodeID)
		case !built:
			why = "no lever runs for this action; it stays in force until reverted by hand"
		}
		out = append(out, Unmanaged{Entry: e, Why: why})
	}
	for _, e := range a.ledger.All() {
		if _, built := a.actuators[e.Action]; !built || e.NodeID != a.nodeID {
			add(e, "")
		}
	}
	for _, e := range real {
		add(e, "held by a real run and not given back while dry_run is on; a real run's reconcile takes it up")
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Action != out[j].Action {
			return out[i].Action < out[j].Action
		}
		return out[i].Expires.Before(out[j].Expires)
	})
	return out
}

// Entries lists the ledger's entries of action that this Acting manages: this
// node's, of a built action, soonest-expiring first. The rest are Unmanaged.
func (a *Acting) Entries(action string) []Entry {
	if _, built := a.actuators[action]; !built {
		return nil
	}
	var out []Entry
	for _, e := range a.ledger.For(action) {
		if e.NodeID == a.nodeID {
			out = append(out, e)
		}
	}
	return out
}

// readBack bounds the read that follows a Set. It is not the caller's context:
// a Set that ran out the caller's deadline may still have taken effect, and only
// a read can tell.
const readBack = 10 * time.Second

// Apply sets action to v on w and records it. A workload the actuator's Check
// refuses is not touched. The setting before the agent's first change to
// (action, subject) is read only when the ledger has no entry for it, and kept as
// Original; a further step keeps it and records the step before as PrevApplied.
//
// The entry is written ahead, Pending with Applied v, before Set, so a change is
// never in force without a record, whatever happens after: a crash, or a Set
// whose call failed after it took effect. After Set the setting is read back
// under a fresh bounded context. On success the entry is committed with what
// reads back, since an actuator may quantise. When Set fails, a setting that
// reads back changed from before this Apply is adopted and committed (and the
// error still returned); one that reads back unchanged restores the entry before
// (or none); one that cannot be read, or that reads back as no setting while it
// differs from before (neither what it was nor what was asked), leaves the entry
// pending, for Held to resolve. A successful Set whose read-back fails is left
// pending too.
func (a *Acting) Apply(ctx context.Context, w runtime.Workload, action string, v float64, expires time.Time, decisionID string) (Entry, error) {
	return a.apply(ctx, w, action, v, expires, decisionID, true)
}

// Restore puts back v, an earlier value of the agent's own change to (action,
// w), as Apply does in every other respect: written ahead, read back, recorded.
// It never runs Check, which gates only cuts: undoing a step that did not help
// must not be refused because that step reclaimed the workload down to where
// Check would refuse a further one.
func (a *Acting) Restore(ctx context.Context, w runtime.Workload, action string, v float64, expires time.Time, decisionID string) (Entry, error) {
	return a.apply(ctx, w, action, v, expires, decisionID, false)
}

func (a *Acting) apply(ctx context.Context, w runtime.Workload, action string, v float64, expires time.Time, decisionID string, check bool) (Entry, error) {
	x, ok := a.actuators[action]
	if !ok {
		return Entry{}, fmt.Errorf("no actuator for action %q", action)
	}
	if !check {
		if _, held := a.entry(action, w.Subject); !held {
			return Entry{}, fmt.Errorf("%s: nothing of %s to restore: the ledger holds no change to it", w.Subject, action)
		}
	} else if why := x.Check(w); why != "" {
		return Entry{}, fmt.Errorf("%s cannot take %s: %s", w.Subject, action, why)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	e := Entry{Action: action, Subject: w.Subject, DecisionID: decisionID, Applied: v, Expires: expires,
		NodeID: a.nodeID, Pending: true}
	prev, held := a.entry(action, w.Subject)
	if held {
		e.Original, e.OriginalSet = prev.Original, prev.OriginalSet
		e.PrevApplied, e.PrevSet = prev.Applied, true
	} else {
		orig, set, err := x.Read(ctx, w)
		if err != nil {
			return Entry{}, fmt.Errorf("%s: read %s: %w", w.Subject, action, err)
		}
		e.Original, e.OriginalSet = orig, set
	}
	beforeV, beforeSet := before(e)
	// restore puts back the ledger as it was before this Apply.
	restore := func() error {
		if held {
			return a.ledger.Put(prev)
		}
		return a.ledger.Delete(action, w.Subject)
	}
	if err := a.ledger.Put(e); err != nil {
		return Entry{}, err // nothing has changed yet
	}
	setErr := x.Set(ctx, w, v, true)
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), readBack)
	got, gotSet, rerr := x.Read(rctx, w)
	cancel()
	if setErr != nil {
		setErr = fmt.Errorf("%s: set %s: %w", w.Subject, action, setErr)
		switch {
		case rerr != nil:
			return Entry{}, fmt.Errorf("%w: %w", ErrLeftPending, setErr) // Held finds out what is in force
		case same(got, gotSet, beforeV, beforeSet):
			if err := restore(); err != nil {
				return Entry{}, errors.Join(setErr, err)
			}
			return Entry{}, setErr
		case !gotSet:
			// Neither what it was nor what was asked: leave it for Held.
			return Entry{}, fmt.Errorf("%w: %w", ErrLeftPending, setErr)
		}
		c := committed(e, got)
		if err := a.ledger.Put(c); err != nil {
			return Entry{}, errors.Join(setErr, err)
		}
		return c, setErr
	}
	if rerr != nil {
		return e, nil // in force as far as Set said; Held settles what reads back
	}
	if same(got, gotSet, beforeV, beforeSet) && !same(got, gotSet, v, true) {
		// Set took the change and it reads back as before: not yet seen to take, as
		// a kubelet that applies an accepted resize a moment later. Committing it
		// would record the original as Applied and drop the cut when it lands.
		return e, nil
	}
	applied := v
	if gotSet {
		applied = got
	}
	c := committed(e, applied)
	if err := a.ledger.Put(c); err != nil {
		// The change is in force but its record is still pending: put back what
		// the setting was before this Apply, and the entry before it.
		if rerr := x.Set(ctx, w, beforeV, beforeSet); rerr != nil {
			return Entry{}, errors.Join(err, fmt.Errorf("%s: roll back %s: %w", w.Subject, action, rerr))
		}
		if rerr := restore(); rerr != nil {
			return Entry{}, errors.Join(err, rerr)
		}
		return Entry{}, err
	}
	return c, nil
}

// before is the setting a pending entry's Apply started from: the step before
// it, or the original for a first step.
func before(e Entry) (float64, bool) {
	if e.PrevSet {
		return e.PrevApplied, true
	}
	return e.Original, e.OriginalSet
}

// committed is e no longer pending, with what is in force as Applied.
func committed(e Entry, applied float64) Entry {
	e.Applied, e.Pending, e.PrevApplied, e.PrevSet = applied, false, 0, false
	return e
}

// same reports whether a reading (v, set) is the setting (w, wset).
func same(v float64, set bool, w float64, wset bool) bool {
	if !set || !wset {
		return set == wset
	}
	return closeTo(v, w)
}

// ErrLeftPending is in Apply's error when the step it took was left pending:
// its Set failed and what is in force could not be told, so the change may be
// in force, and Held settles it. Any other error of Apply's left no change of
// this step in force.
var ErrLeftPending = errors.New("the change was left pending")

// ErrUnconfirmed is Revert's answer when the original was set and does not yet
// read back: the entry is kept, since the change may still be in force.
var ErrUnconfirmed = errors.New("given back, not yet confirmed")

// Revert gives back what e replaced and forgets it once the original reads back. A workload that is gone has
// nothing to give back, so ErrGone also forgets the entry; it is still returned,
// so the caller can tell a change given back from a workload that left. Any
// other error keeps the entry: only the actuator's ErrGone ever drops a change.
func (a *Acting) Revert(ctx context.Context, w runtime.Workload, e Entry) error {
	x, ok := a.actuators[e.Action]
	if !ok {
		return fmt.Errorf("no actuator for action %q", e.Action)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	err := x.Set(ctx, w, e.Original, e.OriginalSet)
	if err != nil && !errors.Is(err, ErrGone) {
		return fmt.Errorf("%s: revert %s: %w", e.Subject, e.Action, err)
	}
	if err == nil {
		// The entry goes only once the original reads back: an actuator that
		// applies a moment later (a kubelet resizing) still holds the change.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), readBack)
		got, gotSet, rerr := x.Read(rctx, w)
		cancel()
		switch {
		case errors.Is(rerr, ErrGone):
			err = rerr
		case rerr != nil:
			return fmt.Errorf("%s: revert %s: %w: %w", e.Subject, e.Action, ErrUnconfirmed, rerr)
		case !same(got, gotSet, e.Original, e.OriginalSet):
			return fmt.Errorf("%s: revert %s: %w (reads %v, set %v)", e.Subject, e.Action, ErrUnconfirmed, got, gotSet)
		}
	}
	if derr := a.ledger.Delete(e.Action, e.Subject); derr != nil {
		return derr
	}
	if err != nil {
		return fmt.Errorf("%s: revert %s: %w", e.Subject, e.Action, err)
	}
	return nil
}

// Held reconciles this node's ledger entries of action against the workloads now
// listed. The listing does not decide that a workload left; the actuator does:
// an entry whose workload is not listed is read through a workload named only by
// its subject, which an actuator finds it by (memory.high by the UID in pod:<uid>,
// cpu.limit by the subject itself). An entry the actuator answers ErrGone for, or
// whose setting no longer reads as what was applied, is dropped and deleted from
// the ledger; the rest are held. A listing that holds no workload at all while
// this node holds entries of action is refused, and nothing is changed: an empty
// listing on a node that was changing workloads is a listing problem until proven
// otherwise. A pending entry, one an Apply wrote ahead and never committed, is
// resolved by what reads back: the setting from before that Apply means the
// change never took, and the entry before it is restored (a further step's, now
// held) or none (a first step's, neither held nor dropped); any other setting
// is adopted as Applied and held; none at all is dropped. Entries of other nodes
// and other actions are not touched, and neither is anything when this Acting
// has no actuator for action, as when its lever was removed from the config.
//
// Each entry is reconciled on its own: one whose Read fails with anything but
// ErrGone is returned in failed, with the error, and left in the ledger
// untouched, for the caller to retry; the rest are held or dropped as above. An
// error is returned only when nothing could be reconciled: an empty listing, or a
// ledger that could not be saved; a save that fails partway through the
// deletions returns the entries already dropped with it, for the caller to
// record.
func (a *Acting) Held(ctx context.Context, action string, ws []runtime.Workload) (held, dropped []Entry, failed []Unread, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	x, ok := a.actuators[action]
	if !ok {
		return nil, nil, nil, nil
	}
	bySubject := make(map[string]runtime.Workload, len(ws))
	for _, w := range ws {
		bySubject[w.Subject] = w
	}
	var mine []Entry
	for _, e := range a.ledger.All() {
		if e.NodeID == a.nodeID && e.Action == action {
			mine = append(mine, e)
		}
	}
	if len(ws) == 0 && len(mine) > 0 {
		return nil, nil, nil, fmt.Errorf("the runtime lists no workload while this node holds %d %s change(s): "+
			"taken for a listing problem, nothing is dropped", len(mine), action)
	}
	var puts []Entry // entries rewritten: restored or adopted
	var gone []Entry // entries deleted: dropped, or a first step that never took
	for _, e := range mine {
		w, listed := bySubject[e.Subject]
		if !listed {
			w = runtime.Workload{Subject: e.Subject}
		}
		v, set, rerr := x.Read(ctx, w)
		switch {
		case errors.Is(rerr, ErrGone):
			gone, dropped = append(gone, e), append(dropped, e)
			continue
		case rerr != nil:
			failed = append(failed, Unread{Entry: e, Err: fmt.Errorf("%s: read %s: %w", e.Subject, e.Action, rerr)})
			continue
		}
		if !e.Pending {
			if set && closeTo(v, e.Applied) {
				held = append(held, e)
			} else {
				gone, dropped = append(gone, e), append(dropped, e)
			}
			continue
		}
		bv, bset := before(e)
		switch {
		case same(v, set, bv, bset) && e.PrevSet:
			r := committed(e, e.PrevApplied)
			puts, held = append(puts, r), append(held, r)
		case same(v, set, bv, bset):
			gone = append(gone, e)
		case set:
			r := committed(e, v)
			puts, held = append(puts, r), append(held, r)
		default:
			gone, dropped = append(gone, e), append(dropped, e)
		}
	}
	for _, e := range puts {
		if err := a.ledger.Put(e); err != nil {
			return nil, nil, nil, err
		}
	}
	isDropped := map[string]bool{}
	for _, e := range dropped {
		isDropped[e.Subject] = true
	}
	var done []Entry // dropped and deleted so far
	for _, e := range gone {
		if err := a.ledger.Delete(e.Action, e.Subject); err != nil {
			return nil, done, nil, err // what was dropped is the caller's to record
		}
		if isDropped[e.Subject] {
			done = append(done, e)
		}
	}
	return held, dropped, failed, nil
}

// Unread is a ledger entry Held could not read the setting of, with why.
type Unread struct {
	Entry Entry
	Err   error
}

func (a *Acting) entry(action, subject string) (Entry, bool) {
	for _, e := range a.ledger.All() {
		if e.Action == action && e.Subject == subject {
			return e, true
		}
	}
	return Entry{}, false
}

// closeTo is equality within 1e-9 relative.
func closeTo(a, b float64) bool {
	return math.Abs(a-b) <= 1e-9*math.Max(math.Abs(a), math.Abs(b))
}
