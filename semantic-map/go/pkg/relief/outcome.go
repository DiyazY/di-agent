package relief

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/DiyazY/di-agent/pkg/actuators"
	"github.com/DiyazY/di-agent/pkg/runtime"
)

// What became of an action. An action with no outcome is in flight: applied
// and waiting for Settle to pass so its effect can be judged.
const (
	OutcomeKept     = "kept"     // it helped; the cap stays until release or expiry
	OutcomeReverted = "reverted" // it did not help and was undone
	OutcomeReleased = "released" // pressure ended and the cap was given back
	OutcomeExpired  = "expired"  // it outlived its expiry and was given back
	OutcomeGone     = "gone"     // the workload left; there was nothing to give back
	OutcomeManual   = "manual"   // an operator undid it
)

const (
	// keepRatio is how much of the predicted drop must be observed for an
	// action to be judged to have helped.
	keepRatio = 0.5
	// failedCooldown multiplies the cooldown of a workload whose cap did not
	// help: the map was wrong about it, and acting on it again soon would
	// repeat the mistake.
	failedCooldown = 4
	// backoffWindow is how soon after its release a workload must be capped
	// again for the capping to count as a repeated cycle.
	backoffWindow = time.Hour
	// backoffCap bounds a repeatedly capped workload's cooldown.
	backoffCap = 4 * time.Hour
	// historySize is how many actions Actions keeps.
	historySize = 200
)

// expire gives back every action past its expiry, whatever the phase and
// whether or not the loop is paused: a change the agent can no longer vouch for
// must not outlive the time it was granted. One the actuator will not give back
// stays in force and is tried again next tick, and so is every one when the
// runtime cannot be listed: a change is given back through the workload the
// runtime lists, which is what an actuator finds it by.
func (l *Loop) expire(ctx context.Context, now time.Time, workloads lister) {
	l.mu.Lock()
	var due []*Action
	for _, a := range l.liveLocked() {
		if !now.Before(a.Entry.Expires) {
			due = append(due, a)
		}
	}
	l.mu.Unlock()
	for _, a := range due {
		err := l.giveBack(ctx, now, a, OutcomeExpired, "expired", "the change expired", a.cooldown, workloads)
		switch {
		case errors.Is(err, errNoListing):
			return // logged by the listing; every due change is retried next tick
		case err != nil:
			l.undoFailed(a, "expiring", err)
		}
	}
	l.expireUnread(ctx, now, workloads)
}

// expireUnread gives back, from the ledger's record itself, every change whose
// setting could not be read and whose expiry has passed: its original is set
// through the workload the runtime lists, or through its subject. One the
// actuator says is gone is recorded gone; one it refuses stays unread, in force,
// and is tried again next tick.
func (l *Loop) expireUnread(ctx context.Context, now time.Time, workloads lister) {
	l.mu.Lock()
	var due []Unread
	for _, u := range l.unread {
		if !now.Before(u.Entry.Expires) {
			due = append(due, *u)
		}
	}
	l.mu.Unlock()
	if len(due) == 0 {
		return
	}
	ws, ok := workloads()
	if !ok {
		return
	}
	for _, u := range due {
		e := u.Entry
		w, _ := workload(ws, e.Subject)
		err := l.cfg.Acting.Revert(ctx, w, e)
		if err != nil && !errors.Is(err, actuators.ErrGone) {
			l.mu.Lock()
			if cur := l.unread[e.Subject]; cur != nil {
				cur.Error = err.Error()
				cur.GivenBack = cur.GivenBack || errors.Is(err, actuators.ErrUnconfirmed)
			}
			l.mu.Unlock()
			l.cfg.Logf("relief: giving back the unread change to %s from the ledger: %v (retrying next tick)", e.Subject, err)
			continue
		}
		a := l.taken(e, l.cooldowns(ws)(e.Subject))
		l.mu.Lock()
		delete(l.unread, e.Subject)
		l.history = appendHistory(l.history, a)
		l.mu.Unlock()
		if errors.Is(err, actuators.ErrGone) {
			l.gone(now, a, "the actuator no longer finds the workload")
			continue
		}
		l.expiredFromRecord(now, a, u)
	}
}

// expiredFromRecord journals and closes a change given back at its expiry from
// the ledger's record, its setting having been unreadable.
func (l *Loop) expiredFromRecord(now time.Time, a *Action, u Unread) {
	b := l.cfg.Map.Decide(l.id("expired"), l.giveBackAsk(a.Subject, "the change expired"))
	b.Input("act", a.DecisionID)
	b.Input("entry", u.Entry)
	b.Input("unread", u.Error)
	b.Caveat("its setting could not be read; it was given back from the ledger's record of what it replaced")
	b.Commit(map[string]any{OutcomeExpired: a.Subject})
	l.end(now, a, OutcomeExpired, a.cooldown)
}

// lister lists the workloads the runtime runs, at most once per pass and only
// when a step needs them; false when the listing failed.
type lister func() ([]runtime.Workload, bool)

// errNoListing is why nothing is given back in a pass whose listing failed.
var errNoListing = errors.New("the runtime's workloads could not be listed; the change stays in force until they can")

// lister is a lister over the runtime under ctx: the first call lists, the rest
// reuse what it found, and a failed listing is logged once and not retried.
func (l *Loop) lister(ctx context.Context) lister {
	var (
		ws             []runtime.Workload
		listed, failed bool
	)
	return func() ([]runtime.Workload, bool) {
		if !listed && !failed {
			var err error
			if ws, err = l.cfg.Runtime.Workloads(ctx); err != nil {
				l.cfg.Logf("relief: listing workloads: %v", err)
				failed = true
			} else {
				listed = true
			}
		}
		return ws, listed
	}
}

// undo gives a's change back through the workload the runtime lists, so an
// actuator that finds a workload by more than its subject, as memory.high finds
// a pod's cgroup by its UID and QoS class, has all of it. A workload the runtime
// does not list is given back through its subject alone, which an actuator
// finds it by (memory.high by the UID in pod:<uid>): the listing does not decide
// that a workload left, the actuator does. gone reports one the actuator answers
// ErrGone for, with why; its entry has then been deleted. Any other refusal keeps
// the entry, to be retried. errNoListing means nothing was tried: the change
// stays in force for a later pass.
func (l *Loop) undo(ctx context.Context, a *Action, workloads lister) (gone bool, why string, err error) {
	ws, ok := workloads()
	if !ok {
		return false, "", errNoListing
	}
	w, _ := workload(ws, a.Subject)
	err = l.cfg.Acting.Revert(ctx, w, a.Entry)
	switch {
	case errors.Is(err, actuators.ErrGone):
		return true, "the actuator no longer finds the workload", nil
	case err != nil:
		return false, "", err
	}
	return false, "", nil
}

// undoFailed records that the actuator refused to undo a's change: counted and
// kept on the action, so Actions and Status show a change stuck in force, logged
// every time, and journalled only on the first refusal, so an actuator that refuses
// for an hour is one record rather than 240. The change stays in force and the
// undo is retried on the next tick. Called under the pass lock, with mu not held.
func (l *Loop) undoFailed(a *Action, what string, err error) {
	l.mu.Lock()
	a.RevertAttempts++
	a.LastError = err.Error()
	n := a.RevertAttempts
	l.mu.Unlock()
	l.cfg.Logf("relief: %s %s on %s: %v (attempt %d, retrying next tick)", what, a.ID, a.Subject, err, n)
	if n != 1 {
		return
	}
	b := l.cfg.Map.Decide(l.id("stuck"), fmt.Sprintf("give back the change to %s?", a.Subject))
	b.Input("act", a.DecisionID)
	b.Input("entry", a.Entry)
	b.Input("while", what)
	b.Input("error", err.Error())
	b.Caveat("the actuator refused to undo the change: it stays in force and is retried every tick; later refusals are counted on the action, not journalled")
	b.Commit(map[string]any{"stuck": a.Subject})
}

// verify judges the action in flight once Settle has passed: kept if the drop
// observed is at least half the drop predicted, reverted otherwise. A resumed
// action is always kept, as there is no pre-cap baseline to judge it by. The
// judgement needs a fresh reading taken after Settle; one taken before it says
// nothing about the cap's effect. A workload the runtime no longer lists is
// gone, and its slot is freed once the actuator has let go of the change.
func (l *Loop) verify(ctx context.Context, now time.Time, pressure float64, at time.Time, fresh bool, workloads func() ([]runtime.Workload, bool)) {
	l.mu.Lock()
	a := l.inFlight
	l.mu.Unlock()
	if a == nil || a.needBaseline || now.Sub(a.ActedAt) < l.c.Settle {
		return
	}
	ws, ok := workloads()
	if !ok {
		return
	}
	w, listed := workload(ws, a.Subject)
	if !listed {
		// Nothing to judge it against: the change is given back through its
		// subject, and is gone only if the actuator says so.
		if err := l.giveBack(ctx, now, a, OutcomeReleased, "release", "the runtime no longer lists the workload", a.cooldown, workloads); err != nil {
			l.undoFailed(a, "giving back", err)
		}
		return
	}
	if a.revertPending {
		l.revertFailed(ctx, now, w, a, a.VerifyID)
		return
	}
	if !fresh || at.Before(a.ActedAt.Add(l.c.Settle)) {
		return
	}

	observed := a.ContentionAtAct - pressure
	// A resumed action's baseline was read with its cap already in force, so the
	// drop it caused is in neither reading: judging it would revert a cap that
	// works. It is kept, and given back by release or expiry like any other.
	keep := a.Resumed || observed >= keepRatio*a.Plan.Predicted
	verdict := "revert"
	if keep {
		verdict = "keep"
	}
	id := l.id("verify")
	b := l.cfg.Map.Decide(id, fmt.Sprintf("did capping %s's %s through %s relieve %s?", a.Subject, l.d.Resource, l.d.Action, l.d.Relieves))
	b.Property(l.d.Relieves)
	b.Input("act", a.DecisionID)
	b.Input("predicted", a.Plan.Predicted)
	b.Input("observed", observed)
	b.Input("contention_at_act", a.ContentionAtAct)
	b.Note("contention %.4f at act, %.4f now: observed drop %.4f against %.4f predicted; keep needs %.0f%% of it",
		a.ContentionAtAct, pressure, observed, a.Plan.Predicted, keepRatio*100)
	if a.Resumed {
		b.Caveat("resumed after a restart: its baseline was taken with the change already in force, so there is no pre-cap contention to judge it against; kept until release or expiry")
	}
	l.learn(b, id, a, keep, observed, now)
	b.Commit(map[string]any{"verdict": verdict, "observed": observed, "predicted": a.Plan.Predicted})

	l.mu.Lock()
	a.Observed, a.VerifyID = &observed, id
	if keep {
		a.Outcome = OutcomeKept
		l.inFlight = nil
		l.kept = append(l.kept, a)
		if !a.Resumed {
			l.nKept++ // a resumed keep is forced, not a prediction that held
		}
		l.mu.Unlock()
		return
	}
	a.revertPending = true
	l.mu.Unlock()
	l.revertFailed(ctx, now, w, a, id)
}

// revertFailed undoes a step that did not help and puts its workload on a long
// cooldown. A first step is reverted; a further step is undone by putting back
// the cap before it, through Restore, which no Check gates, and that cap stays
// kept. A revert the actuator refuses is retried
// on the next tick without judging the step again.
func (l *Loop) revertFailed(ctx context.Context, now time.Time, w runtime.Workload, a *Action, verifyID string) {
	var (
		entry actuators.Entry
		err   error
	)
	if a.prev != nil {
		p := a.prev
		entry, err = l.cfg.Acting.Restore(ctx, w, l.d.Action, p.Entry.Applied, p.Entry.Expires, p.DecisionID)
		if err != nil {
			l.reconciled = false // the ledger is reconciled again, as after any failed Apply
		}
	} else {
		err = l.cfg.Acting.Revert(ctx, w, a.Entry)
	}
	switch {
	case errors.Is(err, actuators.ErrGone):
		l.gone(now, a, "the actuator no longer finds the workload")
		return
	case err != nil:
		l.undoFailed(a, "reverting", err)
		return
	}
	cooldown := failedCooldown * a.cooldown
	question := l.giveBackAsk(a.Subject, "capping it did not help")
	if a.prev != nil {
		question = fmt.Sprintf("put %s back at its earlier %s: the further step did not help", a.Subject, l.d.Action)
	}
	b := l.cfg.Map.Decide(l.id("revert"), question)
	b.Input("act", a.DecisionID)
	if verifyID != "" {
		b.Input("verify", verifyID)
	}
	b.Input("entry", a.Entry)
	if a.prev != nil {
		b.Input("restored", map[string]any{"act": a.prev.DecisionID, "to": a.prev.Entry.Applied, "unit": l.d.Unit})
	}
	b.Note("attribution failed; cooling %s down for %s", a.Subject, cooldown)
	b.Commit(map[string]any{"reverted": a.Subject})

	l.mu.Lock()
	l.nReverted++
	if p := a.prev; p != nil {
		id, failed := a.ID, a.VerifyID
		*a = *p
		// The verify that caused the revert is the one an operator wants, not the earlier step's.
		a.ID, a.Entry, a.Outcome, a.revertPending, a.VerifyID = id, entry, OutcomeKept, false, failed
		a.RevertAttempts, a.LastError = 0, "" // the undo went through; the earlier cap is in force anew
		if l.inFlight == a {
			l.inFlight = nil
		}
		l.kept = append(l.kept, a)
		l.budget.Cool(a.Subject, now.Add(cooldown))
		l.mu.Unlock()
		return
	}
	l.mu.Unlock()
	l.end(now, a, OutcomeReverted, cooldown)
}

// release gives back the most recent kept change, one per Settle, whenever the
// node is not relieving: after pressure has eased, and after a restart that
// found caps on a calm node. One the actuator will not give back does not hold up
// the rest: the next most recent is tried in the same tick, and the refused one
// is tried again at the next release, a Settle later. A pass that cannot list
// the workloads gives back nothing and tries again next tick. In Releasing, with
// nothing left, it ends the phase. An action in flight is judged first, so it is
// either kept and released in turn or reverted.
func (l *Loop) release(ctx context.Context, now time.Time, pressure float64, phase Phase, workloads lister) {
	l.mu.Lock()
	if l.inFlight != nil {
		l.mu.Unlock()
		return
	}
	if len(l.kept) == 0 {
		if phase == Releasing {
			l.trigger.Released()
			l.phase, l.since = l.trigger.Phase(), now
		}
		l.mu.Unlock()
		return
	}
	if !l.lastRelease.IsZero() && now.Sub(l.lastRelease) < l.c.Settle {
		l.mu.Unlock()
		return
	}
	kept := make([]*Action, len(l.kept))
	cooldowns := make([]time.Duration, len(l.kept))
	for i, a := range l.kept {
		kept[len(kept)-1-i] = a // newest first
		cooldowns[len(kept)-1-i] = backoff(a.cooldown, l.cycles[a.Subject])
	}
	l.mu.Unlock()

	var skipped []string
	for i, a := range kept {
		gone, why, err := l.undo(ctx, a, workloads)
		switch {
		case errors.Is(err, errNoListing):
			return
		case gone:
			l.gone(now, a, why)
			return
		case err != nil:
			l.undoFailed(a, "releasing", err)
			skipped = append(skipped, a.ID)
			continue
		}
		l.recordRelease(now, a, pressure, phase, cooldowns[i], skipped)
		return
	}
}

// recordRelease journals and records the release of a, whose revert went
// through. skipped lists the newer actions the actuator would not give back first.
func (l *Loop) recordRelease(now time.Time, a *Action, pressure float64, phase Phase, cooldown time.Duration, skipped []string) {
	b := l.cfg.Map.Decide(l.id("release"), l.giveBackAsk(a.Subject, l.d.Relieves+" has eased"))
	b.Property(l.d.Relieves)
	b.Input("act", a.DecisionID)
	b.Input("entry", a.Entry)
	b.Input("pressure", pressure)
	b.Input("phase", string(phase))
	b.Note("released most recent first; cooling %s down for %s", a.Subject, cooldown)
	if len(skipped) > 0 {
		b.Input("skipped", skipped)
		b.Note("newer changes the actuator would not give back were passed over, to be retried: %s", strings.Join(skipped, ", "))
	}
	b.Commit(map[string]any{"released": a.Subject, "cooldown": cooldown.String()})
	l.end(now, a, OutcomeReleased, cooldown)

	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastRelease = now
	l.released[a.Subject] = now
	if len(l.kept) == 0 && l.inFlight == nil && l.trigger.Phase() == Releasing {
		l.trigger.Released()
		l.phase, l.since = l.trigger.Phase(), now
	}
}

// giveBack reverts an action for a reason other than its verdict or a release
// with pressure eased: expiry, an operator, or a workload the runtime no longer
// lists, through undo. A workload that is gone is recorded as such; one that
// cannot be listed is an error wrapping errNoListing, and its change stays in
// force. reason is why it is given back, as the decision asks it.
func (l *Loop) giveBack(ctx context.Context, now time.Time, a *Action, outcome, kind, reason string, cooldown time.Duration, workloads lister) error {
	gone, why, err := l.undo(ctx, a, workloads)
	switch {
	case err != nil:
		return fmt.Errorf("reverting %s on %s (%s): %w", a.ID, a.Subject, outcome, err)
	case gone:
		l.gone(now, a, why)
		return nil
	}
	b := l.cfg.Map.Decide(l.id(kind), l.giveBackAsk(a.Subject, reason))
	b.Input("act", a.DecisionID)
	b.Input("entry", a.Entry)
	b.Note("cooling %s down for %s", a.Subject, cooldown)
	b.Commit(map[string]any{outcome: a.Subject})
	l.end(now, a, outcome, cooldown)
	return nil
}

// gone records that an action's workload left and frees its slot. There is
// nothing to give back and nothing to retry: the workload, if it returns, is a
// new one.
func (l *Loop) gone(now time.Time, a *Action, why string) {
	b := l.cfg.Map.Decide(l.id("gone"), fmt.Sprintf("what became of the change to %s?", a.Subject))
	b.Input("act", a.DecisionID)
	b.Input("entry", a.Entry)
	b.Note("%s; the change is dropped", why)
	b.Commit(map[string]any{"gone": a.Subject})
	l.end(now, a, OutcomeGone, 0)
}

// end closes an action: it leaves the slot it held and its workload cools down
// from now. A closed action has no undo pending, so no undo failing either.
func (l *Loop) end(now time.Time, a *Action, outcome string, cooldown time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a.Outcome, a.revertPending = outcome, false
	a.RevertAttempts, a.LastError = 0, ""
	if l.inFlight == a {
		l.inFlight = nil
	}
	l.kept = removeAction(l.kept, a)
	if cooldown > 0 {
		l.budget.Cool(a.Subject, now.Add(cooldown))
	}
}

// backoff is a workload's cooldown after its k-th cap/release cycle within
// backoffWindow: doubled each cycle, capped at backoffCap.
func backoff(base time.Duration, k int) time.Duration {
	d := base
	for i := 0; i < k && d < backoffCap; i++ {
		d *= 2
	}
	return min(d, backoffCap)
}

func hasSubject(ws []runtime.Workload, subject string) bool {
	_, ok := workload(ws, subject)
	return ok
}

// workload is the listed workload with subject, or one named only by it when ws
// does not list it, which an actuator finds it by.
func workload(ws []runtime.Workload, subject string) (runtime.Workload, bool) {
	for _, w := range ws {
		if w.Subject == subject {
			return w, true
		}
	}
	return bySubject(subject), false
}

// bySubject is a workload named only by its subject, for giving back a change
// to one the runtime does not list: the actuator finds it by the subject, or
// answers ErrGone, or, when the subject is not enough, ErrInvalidIdentity, which
// keeps the entry.
func bySubject(subject string) runtime.Workload { return runtime.Workload{Subject: subject} }

// removeAction drops a from as without disturbing the order of the rest.
func removeAction(as []*Action, a *Action) []*Action {
	for i, x := range as {
		if x == a {
			return append(as[:i:i], as[i+1:]...)
		}
	}
	return as
}

func appendHistory(h []*Action, a *Action) []*Action {
	h = append(h, a)
	if len(h) > historySize {
		h = h[len(h)-historySize:]
	}
	return h
}

// copyAction is an action the caller cannot change the loop's record through.
func copyAction(a *Action) Action {
	c := *a
	c.Observed = cloneFloat(a.Observed)
	if a.SharesAtAct != nil {
		c.SharesAtAct = make(map[string]float64, len(a.SharesAtAct))
		for s, v := range a.SharesAtAct {
			c.SharesAtAct[s] = v
		}
	}
	c.prev, c.origin = nil, nil
	return c
}

func cloneFloat(p *float64) *float64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
