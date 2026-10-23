package relief

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/DiyazY/di-agent/pkg/actuators"
	"github.com/DiyazY/di-agent/pkg/policy"
	"github.com/DiyazY/di-agent/pkg/runtime"
	"github.com/DiyazY/di-agent/pkg/spec"
	"github.com/DiyazY/di-agent/pkg/statemap"
)

// Status is the loop at a glance: the action it takes, where the node stands,
// what is in flight, and how often the loop's predictions have held. Cooling
// lists workloads the loop will not act on before the time given, whichever
// lever's doing that is.
type Status struct {
	Action          string               `json:"action"`
	Phase           Phase                `json:"phase"`
	Paused          bool                 `json:"paused"`
	Runtime         string               `json:"runtime"` // what the changes go through: dry-run changes nothing
	InFlight        *Action              `json:"in_flight,omitempty"`
	Actions         int                  `json:"actions"`
	Kept            int                  `json:"kept"`
	Reverted        int                  `json:"reverted"`
	Refused         int                  `json:"refused"`
	PredictionsHeld float64              `json:"predictions_held"` // kept / (kept+reverted)
	Stuck           int                  `json:"stuck,omitempty"`  // changes in force whose undo the runtime has refused
	Cooling         map[string]time.Time `json:"cooling,omitempty"`
}

// Action is the agent's change to one workload, with what it predicted and what
// it then observed. A workload stepped again while its change is in force keeps
// one Action: Plan, Entry and DecisionID describe the latest step, and ID stays
// the first step's act decision, so an operator's handle on the change does not
// move. Entry is the ledger's record of it, whose Original is the setting before
// the agent's first step. Resumed marks one found in the ledger at start rather
// than made by this loop: its prediction is only known if the journal still has the decision that
// made it, and its outcome is never evidence, as its baseline was taken with the
// change already in force. SharesAtAct is every workload's share of node CPU when the action's
// baseline was taken, so its outcome can be told apart from another workload
// moving at the same time. RevertAttempts counts the undos the runtime has
// refused since the latest step, and LastError is the latest refusal: a change
// stuck in force is shown, not only logged.
type Action struct {
	ID              string             `json:"id"`
	Subject         string             `json:"subject"`
	DecisionID      string             `json:"decision_id"`
	Plan            Plan               `json:"plan"`
	Entry           actuators.Entry    `json:"entry"`
	ContentionAtAct float64            `json:"contention_at_act"`
	ActedAt         time.Time          `json:"acted_at"`
	Observed        *float64           `json:"observed,omitempty"` // observed drop, after settle
	Outcome         string             `json:"outcome"`
	VerifyID        string             `json:"verify_id,omitempty"` // the decision that judged the latest step
	Resumed         bool               `json:"resumed,omitempty"`
	SharesAtAct     map[string]float64 `json:"shares_at_act,omitempty"`
	// RevertAttempts and LastError: failed undos of the change in force.
	RevertAttempts int    `json:"revert_attempts,omitempty"`
	LastError      string `json:"last_error,omitempty"`

	cooldown      time.Duration // the workload's policy cooldown, the base of every cooldown after this
	revertPending bool          // judged not to have helped; the revert has not yet succeeded
	needBaseline  bool          // resumed without a fresh reading; ContentionAtAct and ActedAt await one
	origin        *float64      // the setting before the agent's first step, for the of_original floor
	prev          *Action       // the action as it stood before its latest step
	originRefused bool          // a further step was refused for want of origin, and journalled
}

// Config is what a Loop is built from. Descriptor is the action the loop takes:
// the node property it relieves, the per-workload usage it moves, its unit and
// the capacity fact that turns a share into that unit; Lever is its thresholds,
// for the same action, and Interval how often it looks. Budget is the hourly
// limit and the cooldowns it shares with the node's other levers; nil is one of
// its own, at the policy defaults' max_actions_per_hour. Runtime lists the
// workloads; Acting changes them through the descriptor's actuator and holds the
// ledger of what it changed. Evidence says the changes really reach the system,
// so an action's outcome is evidence about it: only then do outcomes feed the
// map, the learned effects and the discovery queue. A dry run changes nothing,
// and what pressure did after it is the workload's own doing. Candidates is the
// discovery queue interventions settle; nil settles nothing.
type Config struct {
	Interval   time.Duration
	Lever      spec.Lever
	Budget     *Budget
	Descriptor actuators.Descriptor
	NodeID     string
	Runtime    runtime.Runtime
	Acting     *actuators.Acting
	Policy     *policy.Policy
	Map        *statemap.Map
	Now        func() time.Time
	Logf       func(string, ...any)
	Evidence   bool
	Candidates Candidates
}

// Loop relieves one kind of pressure on one node through one action: it notices
// pressure, caps the workload the map says is causing it, checks after Settle
// that the cap helped, undoes it if not, and gives every cap back once pressure
// is gone. Every step is a decision in the map's journal.
//
// Tick, Reconcile and Revert each hold the pass lock for their whole run,
// runtime and actuator calls included, so only one of them changes the system
// at a time and an outcome can be attributed to one action. The state lock is
// never held across such a call: Status, Actions, Pause and Resume take only it, so an
// HTTP handler is never blocked behind a slow orchestrator, and they always see
// state between two steps, never in the middle of one.
type Loop struct {
	cfg    Config
	c      spec.Lever
	d      actuators.Descriptor
	budget *Budget
	act    actuators.Actuator // the descriptor's, from Acting
	epoch  int64

	effects effects       // what acting on each workload has done; safe on its own
	timeout time.Duration // the longest a pass, or a manual revert, waits on the runtime

	pass       sync.Mutex // serialises the passes that change the system
	seq        int        // decision counter; touched only under pass
	reconciled bool       // the ledger has been reconciled; until then nothing is applied. Touched only under pass

	mu          sync.Mutex
	trigger     *Trigger
	phase       Phase
	since       time.Time // when the current phase began
	paused      bool
	inFlight    *Action
	kept        []*Action            // oldest first; release reverts from the end
	history     []*Action            // oldest first, at most historySize
	released    map[string]time.Time // when each workload was last released
	cycles      map[string]int       // throttle/release cycles within backoffWindow
	lastRelease time.Time            // the last release revert, for one per Settle
	noted       string               // the last refusal journalled this episode
	noPlanAt    time.Time            // when attribution last found no plan
	noPlanKey   string               // the eligible workloads it found none among
	nActs       int
	nKept       int
	nReverted   int
	nRefused    int
}

// New builds a loop in Calm. Its decision ids carry the time it was built, so a
// restarted loop cannot reuse an id the journal already holds.
func New(c Config) (*Loop, error) {
	switch {
	case c.Runtime == nil:
		return nil, errors.New("relief: no runtime")
	case c.Acting == nil:
		return nil, errors.New("relief: no acting")
	case c.Descriptor.Action == "":
		return nil, errors.New("relief: no action descriptor")
	case c.Policy == nil:
		return nil, errors.New("relief: no policy")
	case c.Map == nil:
		return nil, errors.New("relief: no state map")
	case c.Interval <= 0:
		return nil, fmt.Errorf("relief: interval %s must be positive", c.Interval)
	case c.Lever.Action != c.Descriptor.Action:
		return nil, fmt.Errorf("relief: lever for %q, descriptor for %q", c.Lever.Action, c.Descriptor.Action)
	}
	act, ok := c.Acting.Actuator(c.Descriptor.Action)
	if !ok {
		return nil, fmt.Errorf("relief: no actuator for action %q", c.Descriptor.Action)
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	if c.Budget == nil {
		c.Budget = NewBudget(c.Policy.Defaults().MaxActionsPerHour)
	}
	now := c.Now()
	return &Loop{
		cfg: c, c: c.Lever, d: c.Descriptor, budget: c.Budget, act: act, epoch: now.Unix(),
		timeout: max(c.Interval, minCallTimeout),
		trigger: NewTrigger(c.Lever), phase: Calm, since: now,
		released: map[string]time.Time{}, cycles: map[string]int{},
	}, nil
}

// minCallTimeout is the least a pass waits on the runtime, however short the
// interval: an orchestrator under the very pressure the loop relieves answers
// slowly.
const minCallTimeout = 10 * time.Second

// Start reconciles with what the ledger says this node changed, then ticks
// every Interval until ctx ends. A failed reconcile is retried by every
// tick until it succeeds, and nothing is applied before then.
func (l *Loop) Start(ctx context.Context) {
	if err := l.Reconcile(ctx); err != nil {
		l.cfg.Logf("relief: %v (no change is made until it succeeds)", err)
	}
	t := time.NewTicker(l.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.Tick(ctx)
		}
	}
}

// Pause stops the loop making changes and releasing them. It still observes,
// runs the trigger, verifies what is in flight, and gives back what expires.
func (l *Loop) Pause() { l.mu.Lock(); l.paused = true; l.mu.Unlock() }

// Resume undoes Pause.
func (l *Loop) Resume() { l.mu.Lock(); l.paused = false; l.mu.Unlock() }

// Status is the loop now.
func (l *Loop) Status() Status {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.cfg.Now()
	st := Status{Action: l.d.Action, Phase: l.trigger.Phase(), Paused: l.paused, Runtime: l.cfg.Runtime.Name(),
		Actions: l.nActs, Kept: l.nKept, Reverted: l.nReverted, Refused: l.nRefused}
	if l.inFlight != nil {
		a := copyAction(l.inFlight)
		st.InFlight = &a
	}
	for _, a := range l.liveLocked() {
		if a.RevertAttempts > 0 {
			st.Stuck++
		}
	}
	if judged := l.nKept + l.nReverted; judged > 0 {
		st.PredictionsHeld = float64(l.nKept) / float64(judged)
	}
	if c := l.budget.coolingAll(l.d.Action, now); len(c) > 0 {
		st.Cooling = c
	}
	return st
}

// Actions lists the actions, newest first, at most 200.
func (l *Loop) Actions() []Action {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Action, 0, len(l.history))
	for i := len(l.history) - 1; i >= 0; i-- {
		out = append(out, copyAction(l.history[i]))
	}
	return out
}

// Tick is one pass: observe pressure, give back what expired, judge what has
// settled, then act or release as the phase asks. A reading counts only while
// it is fresh: present, active, and younger than the map's StaleAfter, judged
// here rather than left to the next sweep. The trigger is fed the reading's own
// time, so a reading that has stopped changing cannot complete a run by being
// read again. Without a fresh reading the trigger holds and no new change is
// made and nothing is released, because a decision on a reading that is not
// current is a decision about a system that may no longer exist. What needs no
// reading still happens: expiry, a pending revert retried, and the dropping of a
// change whose workload the runtime no longer lists.
//
// The whole pass, every runtime call in it, is bounded by the loop's timeout
// (Interval, at least 10s), so an orchestrator that stops answering
// cannot hold the pass lock and with it every manual revert.
func (l *Loop) Tick(ctx context.Context) {
	l.pass.Lock()
	defer l.pass.Unlock()
	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	now := l.cfg.Now()
	if !l.reconciled {
		if err := l.reconcile(ctx, now); err != nil {
			l.cfg.Logf("relief: %v (no change is made until it succeeds)", err)
		}
	}
	pressure, at, fresh := l.pressure(now)
	var shares map[string]float64
	if fresh {
		// For a resumed action's baseline, taken below; read outside the state lock,
		// as the loop's other reads of the map are.
		shares = workloadShares(l.cfg.Map, l.d.Usage)
	}

	l.mu.Lock()
	seen := now
	if fresh {
		seen = at
	}
	prev := l.phase
	l.phase = l.trigger.Observe(seen, pressure, fresh)
	if l.phase != prev {
		l.since = now
		switch l.phase {
		case Relieving:
			l.noted, l.noPlanAt = "", time.Time{}
		case Releasing:
			l.lastRelease = time.Time{}
		}
	}
	if a := l.inFlight; a != nil && a.needBaseline && fresh {
		a.ContentionAtAct, a.ActedAt, a.SharesAtAct, a.needBaseline = pressure, now, shares, false
	}
	phase, paused := l.phase, l.paused
	l.mu.Unlock()

	l.expire(ctx, now)

	var (
		ws     []runtime.Workload
		listed bool
		failed bool
	)
	workloads := func() ([]runtime.Workload, bool) {
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

	l.verify(ctx, now, pressure, at, fresh, workloads)
	if paused || !fresh {
		return
	}
	if phase == Relieving {
		if l.reconciled {
			l.relieve(ctx, now, pressure, workloads)
		}
		return
	}
	l.release(ctx, now, pressure, phase)
}

// Reconcile takes up the changes the ledger says this node made with this
// loop's action and has not given back, as after a restart. Until it has
// succeeded once the loop applies nothing, because a change made on top of caps
// it does not know of would record one of them as the workload's original.
func (l *Loop) Reconcile(ctx context.Context) error {
	l.pass.Lock()
	defer l.pass.Unlock()
	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	now := l.cfg.Now()
	if err := l.reconcile(ctx, now); err != nil {
		return err
	}
	l.expire(ctx, now)
	return nil
}

// reconcile is Reconcile under the pass lock. Acting reads each held change back
// against the workloads the runtime lists: one whose workload is no longer
// listed, or whose setting no longer reads as what the agent applied, as after a
// pod restart or another hand, is no longer the agent's and is journalled gone.
// Of the rest, the most recent unexpired change becomes the action in flight,
// with its prediction taken from the decision that made it if the journal still
// holds it. Verify keeps it either way: its baseline is read with the change in
// force, so there is no drop to judge. Its baseline is the pressure now if that
// is fresh, else the first fresh reading after. Older unexpired changes are
// kept, for release. Expired ones are kept in force for the caller's expire to
// give back, so one the actuator will not yet give back is retried every tick.
// Another node's changes, and another action's, are never touched here. Each
// unexpired change counts toward the shared hourly limit from the time the
// journal says it was made, or from now when the journal no longer has it, so a
// restart does not hand the loops a fresh budget, and holds the other levers off
// its workload for its cooldown from then.
func (l *Loop) reconcile(ctx context.Context, now time.Time) error {
	ws, err := l.cfg.Runtime.Workloads(ctx)
	if err != nil {
		return fmt.Errorf("relief: listing workloads: %w", err)
	}
	held, dropped, err := l.cfg.Acting.Held(ctx, l.d.Action, ws)
	if err != nil {
		return fmt.Errorf("relief: reconciling this node's changes: %w", err)
	}
	pressure, _, fresh := l.pressure(now)
	cooldowns := l.cooldowns(ws)
	shares := workloadShares(l.cfg.Map, l.d.Usage)

	l.mu.Lock()
	live := map[string]*Action{}
	for _, a := range l.liveLocked() {
		live[a.Subject] = a
	}
	busy := l.inFlight != nil
	l.mu.Unlock()

	// taken builds the action for a change found in the ledger rather than made here.
	taken := func(e actuators.Entry) *Action {
		a := &Action{ID: e.DecisionID, Subject: e.Subject, DecisionID: e.DecisionID, Entry: e,
			Resumed: true, cooldown: cooldowns(e.Subject), Plan: Plan{Subject: e.Subject, To: e.Applied, Unit: l.d.Unit}}
		if p, first, ok := l.journalledAct(e.DecisionID); ok {
			a.Plan = p
			if first != "" {
				a.ID = first
			}
		}
		if a.ID == "" {
			a.ID = l.id("act")
		}
		return a
	}

	for _, e := range dropped {
		why := "the runtime no longer lists the workload"
		if hasSubject(ws, e.Subject) {
			why = "its setting no longer reads as what the agent applied: changed underneath, as by a restart or another hand"
		}
		a := live[e.Subject]
		if a == nil {
			a = taken(e)
			l.mu.Lock()
			l.history = appendHistory(l.history, a)
			l.mu.Unlock()
		}
		l.gone(now, a, why)
	}

	var expired, resumed []*Action
	for _, e := range held {
		if live[e.Subject] != nil {
			continue
		}
		a := taken(e)
		a.origin = l.originOf(a)
		if fresh {
			a.ContentionAtAct, a.ActedAt, a.SharesAtAct = pressure, now, shares
		} else {
			a.needBaseline = true
		}
		if !now.Before(e.Expires) {
			expired = append(expired, a)
		} else {
			resumed = append(resumed, a)
			at := now
			if d, ok := l.cfg.Map.Journal().Decision(e.DecisionID); ok && !d.At.IsZero() && d.At.Before(now) {
				at = d.At
			}
			l.budget.Spend(at)
			l.budget.coolExcept(a.Subject, l.d.Action, at.Add(a.cooldown))
		}
	}
	sort.SliceStable(resumed, func(i, j int) bool { return resumed[i].Entry.Expires.Before(resumed[j].Entry.Expires) })

	l.mu.Lock()
	for _, a := range expired {
		a.Outcome = OutcomeKept
		l.history = appendHistory(l.history, a)
		l.kept = append(l.kept, a)
	}
	for i, a := range resumed {
		l.history = appendHistory(l.history, a)
		if i == len(resumed)-1 && !busy {
			l.inFlight = a
			continue
		}
		a.Outcome = OutcomeKept
		l.kept = append(l.kept, a)
	}
	l.mu.Unlock()
	l.reconciled = true
	for _, a := range resumed {
		l.cfg.Logf("relief: resumed %s on %s (expires %s)", a.ID, a.Subject, a.Entry.Expires.Format(time.RFC3339))
	}
	return nil
}

// ErrNoAction is Revert's answer for an id that is not an action in force.
var ErrNoAction = errors.New("no such action in force")

// Revert undoes an action by hand and cools its workload down, as an operator
// overriding the loop. Only an action still in force can be reverted. An
// actuator that does not answer within the loop's timeout is an error, and the
// action stays in force.
func (l *Loop) Revert(ctx context.Context, id string) error {
	l.pass.Lock()
	defer l.pass.Unlock()
	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	l.mu.Lock()
	var a *Action
	for _, c := range l.liveLocked() {
		if c.ID == id {
			a = c
		}
	}
	l.mu.Unlock()
	if a == nil {
		return fmt.Errorf("%w: %s", ErrNoAction, id)
	}
	return l.giveBack(ctx, l.cfg.Now(), a, OutcomeManual, "manual", a.cooldown)
}

// pressure is node pressure, when it was read, and whether it is fresh. The
// sweep marks a silent property stale only when it next runs, so the reading's
// age is checked here as well: a source that has just gone quiet leaves a value
// that is active for up to a sweep interval longer and describes nothing now.
func (l *Loop) pressure(now time.Time) (float64, time.Time, bool) {
	p, ok := l.cfg.Map.Property(l.d.Relieves)
	fresh := ok && p.Status == statemap.Active && !p.LastObserved.IsZero() &&
		now.Sub(p.LastObserved) <= l.cfg.Map.StaleAfter()
	return p.Value, p.LastObserved, fresh
}

// relieve takes one step when nothing is in flight. The eligible workloads are
// the actionable ones not cooling down for this lever, including one already
// capped, which is stepped again from its current cap. The hourly limit is
// checked before anything is estimated: the count is every lever's, and the
// limit is each workload's own max_actions_per_hour, so a rule's override holds
// for its workloads against the one shared count. It is checked again, and spent,
// in one step just before the change, since another lever may have spent it
// since. Attribution that found no plan is not repeated
// until Settle has passed or the eligible workloads change, because each
// estimate is a journalled decision and asking the same question every tick
// would bury the journal in identical answers.
func (l *Loop) relieve(ctx context.Context, now time.Time, pressure float64, workloads func() ([]runtime.Workload, bool)) {
	l.mu.Lock()
	busy := l.inFlight != nil
	kept := make(map[string]*Action, len(l.kept))
	for _, a := range l.kept {
		kept[a.Subject] = a
	}
	since, noPlanAt, noPlanKey := l.since, l.noPlanAt, l.noPlanKey
	l.mu.Unlock()
	if busy {
		return
	}
	inHour := l.budget.Count(now)
	cooling := l.budget.coolingAll(l.d.Action, now)
	ws, ok := workloads()
	if !ok {
		return
	}
	pol := l.cfg.Policy
	var open []runtime.Workload
	var limitedBy *policy.Effective
	var limitedSubject string
	var unanchored []*Action
	for _, w := range ws {
		if _, ok := cooling[w.Subject]; ok || !actionable(w, l.act, pol) {
			continue
		}
		if k := kept[w.Subject]; k != nil && k.origin == nil {
			unanchored = append(unanchored, k)
			continue
		}
		eff := pol.For(targetOf(w))
		if inHour >= eff.Settings.MaxActionsPerHour {
			if limitedBy == nil {
				limitedBy, limitedSubject = &eff, w.Subject
			}
			continue
		}
		open = append(open, w)
	}
	for _, a := range unanchored {
		l.refuseUnanchored(a)
	}
	if len(open) == 0 {
		if limitedBy != nil {
			l.refuse(fmt.Sprintf("max_actions_per_hour %d reached", limitedBy.Settings.MaxActionsPerHour),
				map[string]any{"rule": limitedBy.Rule, "policy": *limitedBy, "subject": limitedSubject,
					"actions_last_hour": inHour})
		}
		return
	}
	key := eligibleKey(open)
	if !noPlanAt.IsZero() && key == noPlanKey && now.Sub(noPlanAt) < l.c.Settle {
		return
	}
	if _, ok := capacity(l.cfg.Map, l.d); !ok {
		l.refuse(noCapacity(l.d), nil)
		return
	}

	originals := map[string]float64{}
	for s, a := range kept {
		if a.origin != nil {
			originals[s] = *a.origin
		}
	}
	cands := Attribute(l.cfg.Map, l.act, open, pol, l.c)
	plan, ok, why := makePlan(ctx, l.cfg.Map, l.act, cands, open, pol, l.c, l.effects.Of, originals)
	if !ok {
		l.mu.Lock()
		l.noPlanAt, l.noPlanKey = now, key
		l.mu.Unlock()
		l.refuse(why, map[string]any{"attribution": cands})
		return
	}
	var w runtime.Workload
	for _, c := range open {
		if c.Subject == plan.Subject {
			w = c
		}
	}
	eff := pol.For(targetOf(w))
	k := kept[plan.Subject]
	if !l.budget.take(now, eff.Settings.MaxActionsPerHour) {
		// Another lever spent the last of the hour since the check above.
		l.refuse(fmt.Sprintf("max_actions_per_hour %d reached", eff.Settings.MaxActionsPerHour),
			map[string]any{"rule": eff.Rule, "policy": eff, "subject": plan.Subject,
				"actions_last_hour": l.budget.Count(now)})
		return
	}

	id := l.id("act")
	b := l.cfg.Map.Decide(id, fmt.Sprintf("cap %s's CPU to relieve node CPU contention?", plan.Subject))
	b.Property(l.d.Relieves)
	b.Property(l.d.Usage + "@" + plan.Subject)
	b.Facts()
	shares := workloadShares(l.cfg.Map, l.d.Usage)
	entry, err := l.cfg.Acting.Apply(ctx, w, l.d.Action, plan.To, now.Add(l.c.Expiry), id)
	if err != nil {
		l.budget.refund(now)
		r := l.cfg.Map.Decide(l.id("refused"), fmt.Sprintf("cap %s's CPU to relieve node CPU contention?", plan.Subject))
		r.Input("plan", plan)
		r.Input("policy", eff)
		r.Input("rule", eff.Rule)
		r.Input("error", err.Error())
		r.Note("the actuator refused the change; cooling %s down for %s", plan.Subject, eff.Settings.Cooldown)
		r.Note("the change may have taken effect regardless; the next pass reconciles the ledger with what the workload reads as")
		r.Commit(map[string]any{"refused": "actuator: " + err.Error()})
		l.budget.Cool(plan.Subject, now.Add(eff.Settings.Cooldown))
		l.mu.Lock()
		l.nRefused++
		l.mu.Unlock()
		// A failed Apply can still have taken effect, as a resize whose call timed
		// out: Acting records one that reads back changed, and reconciling again
		// adopts it, so expiry and release give it back.
		l.reconciled = false
		return
	}

	b.Input("notice", map[string]any{"pressure": pressure, "phase": string(Relieving), "since": since})
	b.Input("attribution", cands)
	b.Input("plan", plan)
	b.Input("policy", eff)
	if k != nil {
		b.Input("previous", map[string]any{"act": k.DecisionID, "plan": k.Plan, "action": k.ID})
		b.Note("a further step on the change %s already in force", k.ID)
	}
	b.Note("capping %s from %.3f to %.3f %s; predicted drop in node CPU contention %.4f", plan.Subject,
		plan.From, plan.To, plan.Unit, plan.Predicted)
	if plan.Basis == BasisUsage {
		b.Caveat("%s", usageCaveat)
	}
	b.Commit(map[string]any{"action": l.d.Action, "subject": plan.Subject,
		"to": plan.To, "unit": plan.Unit, "predicted": plan.Predicted, "expires": entry.Expires})

	l.mu.Lock()
	defer l.mu.Unlock()
	a := k
	if a != nil {
		before := *a
		a.prev = &before
		l.kept = removeAction(l.kept, a)
		l.history = appendHistory(removeAction(l.history, a), a)
		a.DecisionID, a.Plan, a.Entry, a.Observed, a.Outcome, a.VerifyID = id, plan, entry, nil, "", ""
		a.RevertAttempts, a.LastError = 0, ""
		a.ContentionAtAct, a.ActedAt, a.cooldown, a.Resumed, a.needBaseline = pressure, now, eff.Settings.Cooldown, false, false
		a.SharesAtAct = shares
	} else {
		origin := plan.From
		if entry.OriginalSet {
			origin = entry.Original
		}
		a = &Action{ID: id, Subject: plan.Subject, DecisionID: id, Plan: plan, Entry: entry,
			ContentionAtAct: pressure, ActedAt: now, cooldown: eff.Settings.Cooldown, origin: &origin, SharesAtAct: shares}
		if last, ok := l.released[plan.Subject]; ok && now.Sub(last) < backoffWindow {
			l.cycles[plan.Subject]++
		} else {
			l.cycles[plan.Subject] = 0
		}
		l.history = appendHistory(l.history, a)
	}
	l.inFlight = a
	l.budget.coolExcept(plan.Subject, l.d.Action, now.Add(eff.Settings.Cooldown))
	l.nActs++
	l.noted, l.noPlanAt = "", time.Time{}
}

// eligibleKey names a set of workloads, so attribution can tell whether the
// set it last found no plan among has changed.
func eligibleKey(ws []runtime.Workload) string {
	ids := make([]string, len(ws))
	for i, w := range ws {
		ids[i] = w.Subject
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// refuse journals why no action was taken, once per reason per relieving
// episode, so a reason that holds for an hour is one record rather than 240.
func (l *Loop) refuse(why string, inputs map[string]any) {
	l.mu.Lock()
	if why == l.noted {
		l.mu.Unlock()
		return
	}
	l.noted = why
	l.nRefused++
	l.mu.Unlock()
	b := l.cfg.Map.Decide(l.id("refused"), "relieve node CPU contention?")
	b.Property(l.d.Relieves)
	for k, v := range inputs {
		b.Input(k, v)
	}
	b.Caveat("%s", why)
	b.Commit(map[string]any{"refused": why})
}

// id names the next decision: relief-<kind>-<action>-<loop start>-<n>. The
// action keeps two levers' loops, started in the same second, from naming two
// decisions alike.
func (l *Loop) id(kind string) string {
	l.seq++
	return fmt.Sprintf("relief-%s-%s-%d-%d", kind, l.d.Action, l.epoch, l.seq)
}

// liveLocked is every action still in force. Caller holds mu.
func (l *Loop) liveLocked() []*Action {
	out := append([]*Action(nil), l.kept...)
	if l.inFlight != nil {
		out = append(out, l.inFlight)
	}
	return out
}

// liveSubjects is the workloads with a change in force. Caller holds mu.
func (l *Loop) liveSubjects() map[string]bool {
	out := map[string]bool{}
	for _, a := range l.liveLocked() {
		out[a.Subject] = true
	}
	return out
}

// cooldowns returns each workload's policy cooldown, for actions found in the
// ledger rather than made here. A workload ws does not list gets the policy for
// its kind alone.
func (l *Loop) cooldowns(ws []runtime.Workload) func(subject string) time.Duration {
	return func(subject string) time.Duration {
		for _, w := range ws {
			if w.Subject == subject {
				return l.cfg.Policy.For(targetOf(w)).Settings.Cooldown
			}
		}
		return l.cfg.Policy.For(targetOf(runtime.Workload{Subject: subject})).Settings.Cooldown
	}
}

// refuseUnanchored journals, once per action, that a workload whose
// pre-agent setting is not known will not be stepped again: the of_original floor
// would be a share of the agent's own cap, and would slide down with every step
// and every restart. The change itself stays, for verify, release and expiry.
func (l *Loop) refuseUnanchored(a *Action) {
	l.mu.Lock()
	done := a.originRefused
	a.originRefused = true
	if !done {
		l.nRefused++
	}
	l.mu.Unlock()
	if done {
		return
	}
	why := fmt.Sprintf("origin of %s's change is unknown: no original setting was recorded and the journal does not hold its first act %s", a.Subject, a.ID)
	b := l.cfg.Map.Decide(l.id("refused"), fmt.Sprintf("cap %s's CPU further to relieve node CPU contention?", a.Subject))
	b.Input("action", a.ID)
	b.Input("entry", a.Entry)
	b.Caveat("%s", why)
	b.Commit(map[string]any{"refused": why})
}

// originOf is the setting a resumed action's workload had before the agent
// first touched it, or nil when that is not known for certain. It is the
// ledger's Original when one was set; otherwise the usage the first act
// recorded, read from that act and never from a further step, whose usage is
// already the agent's doing.
func (l *Loop) originOf(a *Action) *float64 {
	if a.Entry.OriginalSet {
		v := a.Entry.Original
		return &v
	}
	if a.ID == "" {
		return nil
	}
	p, first, ok := l.journalledAct(a.ID)
	if !ok || first != "" || p.From <= 0 {
		return nil
	}
	return &p.From
}

// journalledAct reads an act decision: the plan it recorded, so a resumed action
// is judged against what was predicted, and, for a further step, the id of the
// action's first act, which it cites. A snapshot restores inputs as decoded JSON
// rather than as Go values, so both are read.
func (l *Loop) journalledAct(id string) (p Plan, first string, ok bool) {
	if id == "" {
		return Plan{}, "", false
	}
	d, ok := l.cfg.Map.Journal().Decision(id)
	if !ok {
		return Plan{}, "", false
	}
	switch v := d.Inputs["plan"].(type) {
	case Plan:
		p = v
	case map[string]any:
		p.Subject, _ = v["subject"].(string)
		p.Predicted, _ = v["predicted"].(float64)
		p.From, _ = v["from"].(float64)
		p.To, _ = v["to"].(float64)
		p.Unit, _ = v["unit"].(string)
		p.EstimateID, _ = v["estimate_id"].(string)
		p.Basis, _ = v["basis"].(string)
		p.Effect, _ = v["effect"].(float64)
	}
	if prev, isMap := d.Inputs["previous"].(map[string]any); isMap {
		first, _ = prev["action"].(string)
	}
	return p, first, true
}
