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
// changed. It replaces the runtime's Apply, Revert and Changed. It is safe for
// concurrent use; calls are serialised so a read-then-set is never interleaved.
type Acting struct {
	nodeID string
	ledger *Ledger

	mu        sync.Mutex
	actuators map[string]Actuator
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

// Apply sets action to v on w and records it. The setting before the agent's
// first change to (action, subject) is read only when the ledger has no entry
// for it, and kept as Original. When Set fails nothing new is recorded.
func (a *Acting) Apply(ctx context.Context, w runtime.Workload, action string, v float64, expires time.Time, decisionID string) (Entry, error) {
	x, ok := a.actuators[action]
	if !ok {
		return Entry{}, fmt.Errorf("no actuator for action %q", action)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	e := Entry{Action: action, Subject: w.Subject, DecisionID: decisionID, Applied: v, Expires: expires, NodeID: a.nodeID}
	prev, held := a.entry(action, w.Subject)
	if held {
		e.Original, e.OriginalSet = prev.Original, prev.OriginalSet
	} else {
		orig, set, err := x.Read(ctx, w)
		if err != nil {
			return Entry{}, fmt.Errorf("%s: read %s: %w", w.Subject, action, err)
		}
		e.Original, e.OriginalSet = orig, set
	}
	if err := x.Set(ctx, w, v, true); err != nil {
		return Entry{}, fmt.Errorf("%s: set %s: %w", w.Subject, action, err)
	}
	// An actuator may quantise what it stores; the ledger holds what is in force.
	if got, set, err := x.Read(ctx, w); err == nil && set {
		e.Applied = got
	}
	if err := a.ledger.Put(e); err != nil {
		// The change is in force but unrecorded, so the agent could never undo
		// it: put back what the setting was before this Apply.
		backV, backSet := e.Original, e.OriginalSet
		if held {
			backV, backSet = prev.Applied, true
		}
		if rerr := x.Set(ctx, w, backV, backSet); rerr != nil {
			return Entry{}, errors.Join(err, fmt.Errorf("%s: roll back %s: %w", w.Subject, action, rerr))
		}
		return Entry{}, err
	}
	return e, nil
}

// Revert gives back what e replaced and forgets it. A workload that is gone has
// nothing to give back, so ErrGone also forgets the entry and is not an error.
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
	return a.ledger.Delete(e.Action, e.Subject)
}

// Held reconciles this node's ledger entries against the workloads now listed. An
// entry whose workload is no longer listed, or whose setting no longer reads as
// what was applied, is dropped and deleted from the ledger; the rest are held.
// Entries of other nodes are neither returned nor dropped.
func (a *Acting) Held(ctx context.Context, ws []runtime.Workload) (held []Entry, dropped []Entry, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	bySubject := make(map[string]runtime.Workload, len(ws))
	for _, w := range ws {
		bySubject[w.Subject] = w
	}
	for _, e := range a.ledger.All() {
		if e.NodeID != a.nodeID {
			continue
		}
		w, listed := bySubject[e.Subject]
		keep := false
		if listed {
			x, ok := a.actuators[e.Action]
			if !ok {
				return held, dropped, fmt.Errorf("no actuator for held action %q", e.Action)
			}
			v, set, rerr := x.Read(ctx, w)
			switch {
			case errors.Is(rerr, ErrGone):
			case rerr != nil:
				return held, dropped, fmt.Errorf("%s: read %s: %w", e.Subject, e.Action, rerr)
			default:
				keep = set && closeTo(v, e.Applied)
			}
		}
		if keep {
			held = append(held, e)
			continue
		}
		if derr := a.ledger.Delete(e.Action, e.Subject); derr != nil {
			return held, dropped, derr
		}
		dropped = append(dropped, e)
	}
	return held, dropped, nil
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
