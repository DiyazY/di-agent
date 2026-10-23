package relief

import (
	"sort"
	"sync"
	"time"
)

// Budget is what the relief loops of one node share, so that levers do not add
// up to more change than one would make: the actions taken in the last hour,
// one count against max_actions_per_hour whichever lever took them, and each
// workload's cooldown. A workload one lever has acted on cools down for every
// other lever, so two levers never stack changes on it within a cooldown, while
// the lever that acted may still step its own change further. A cooldown after a
// change ends (kept and released, reverted, gone or refused) holds every lever
// off, the one that made it included. A Budget is safe for concurrent use.
type Budget struct {
	mu    sync.Mutex
	max   int
	acted []time.Time                     // oldest first
	cool  map[string]map[string]time.Time // subject → lever exempted ("" none) → until
}

// NewBudget is a budget of maxPerHour actions an hour, the default policy's
// max_actions_per_hour.
func NewBudget(maxPerHour int) *Budget {
	return &Budget{max: maxPerHour, cool: map[string]map[string]time.Time{}}
}

// Allow reports whether fewer than the budget's max actions were taken in the
// hour before now.
func (b *Budget) Allow(now time.Time) bool { return b.Count(now) < b.max }

// Count is the actions taken in the hour before now, on every lever.
func (b *Budget) Count(now time.Time) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.countLocked(now)
}

func (b *Budget) countLocked(now time.Time) int {
	cutoff := now.Add(-time.Hour)
	i := 0
	for i < len(b.acted) && !b.acted[i].After(cutoff) {
		i++
	}
	b.acted = b.acted[i:]
	return len(b.acted)
}

// Spend records an action taken at at, which may be earlier than one already
// recorded: an action taken up after a restart counts from when it was made.
func (b *Budget) Spend(at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.spendLocked(at)
}

func (b *Budget) spendLocked(at time.Time) {
	i := sort.Search(len(b.acted), func(i int) bool { return b.acted[i].After(at) })
	b.acted = append(b.acted, time.Time{})
	copy(b.acted[i+1:], b.acted[i:])
	b.acted[i] = at
}

// Why reserve refused: the hourly count is spent, or another lever holds the
// workload.
const (
	reserveLimit   = "limit"
	reserveCooling = "cooling"
)

// reserve is the one step a lever takes before it changes a workload: under one
// lock it checks that fewer than max actions were taken in the hour before now
// and that no other lever holds subject, then spends an action at now and holds
// every other lever off subject until until. Two levers ticking at once cannot
// both pass, so neither the last action of the hour nor a workload is had twice,
// however long the change then takes to go through. max is the workload's own
// max_actions_per_hour, which a policy rule may set above or below the default.
// why is reserveLimit or reserveCooling when it refuses.
func (b *Budget) reserve(action, subject string, now time.Time, max int, until time.Time) (ok bool, why string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, cooling := b.coolingForLocked(action, subject, now); cooling {
		return false, reserveCooling
	}
	if b.countLocked(now) >= max {
		return false, reserveLimit
	}
	b.spendLocked(now)
	b.coolExceptLocked(subject, action, until)
	return true, ""
}

// refund gives back the action reserve spent at at, for a change that was not
// made. The hold on the workload stands: a failed change still cools it, as a
// refusal does.
func (b *Budget) refund(at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := len(b.acted) - 1; i >= 0; i-- {
		if b.acted[i].Equal(at) {
			b.acted = append(b.acted[:i], b.acted[i+1:]...)
			return
		}
	}
}

// Cool holds every lever off subject until until. A cooldown never shortens one
// already in force.
func (b *Budget) Cool(subject string, until time.Time) { b.coolExcept(subject, "", until) }

// coolExcept holds every lever but except off subject until until: what an act
// leaves, so the lever that acted may step its change again.
func (b *Budget) coolExcept(subject, except string, until time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.coolExceptLocked(subject, except, until)
}

func (b *Budget) coolExceptLocked(subject, except string, until time.Time) {
	c := b.cool[subject]
	if c == nil {
		c = map[string]time.Time{}
		b.cool[subject] = c
	}
	if until.After(c[except]) {
		c[except] = until
	}
}

// Cooling reports whether any lever is held off subject at now.
func (b *Budget) Cooling(subject string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, until := range b.cool[subject] {
		if now.Before(until) {
			return true
		}
	}
	return false
}

// coolingFor is when the lever for action may next act on subject, and whether
// that is after now.
func (b *Budget) coolingFor(action, subject string, now time.Time) (time.Time, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.coolingForLocked(action, subject, now)
}

func (b *Budget) coolingForLocked(action, subject string, now time.Time) (time.Time, bool) {
	var latest time.Time
	for except, until := range b.cool[subject] {
		if except != action && until.After(latest) {
			latest = until
		}
	}
	return latest, now.Before(latest)
}

// coolingAll is every subject the lever for action is held off at now, with
// until when. A subject whose every cooldown has passed is forgotten.
func (b *Budget) coolingAll(action string, now time.Time) map[string]time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]time.Time{}
	for s, c := range b.cool {
		live := false
		for _, until := range c {
			live = live || now.Before(until)
		}
		if !live {
			delete(b.cool, s)
			continue
		}
		if until, ok := b.coolingForLocked(action, s, now); ok {
			out[s] = until
		}
	}
	return out
}
