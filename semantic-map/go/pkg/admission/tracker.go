package admission

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/DiyazY/di-agent/pkg/statemap"
)

const (
	// ArrivalWindow is how long an accept waits for its workload to appear.
	ArrivalWindow = 5 * time.Minute
	// PendingCap bounds the accepts awaiting an outcome.
	PendingCap = 32
	// outcomeCap bounds the outcomes kept for listing.
	outcomeCap = 200
)

// Outcomes of an accept.
const (
	Held       = "held"
	Breached   = "breached"
	Confounded = "confounded"
	NotArrived = "not_arrived"
)

// ErrAlreadyPending is returned for an accept whose name awaits an outcome.
var ErrAlreadyPending = errors.New("already pending")

// Outcome is what became of one accept.
type Outcome struct {
	Name      string `json:"name"`
	AcceptID  string `json:"accept_id"` // the accept's decision, which holds both predictions
	OutcomeID string `json:"outcome_id"`
	Subject   string `json:"subject"`
	Result    string `json:"result"`
	// Reason says why a confounded outcome was not scored.
	Reason string        `json:"reason,omitempty"`
	Lines  []OutcomeLine `json:"lines"`
	At     time.Time     `json:"at"`
}

// OutcomeLine is one domain's check: the observed rise of Relieves over the
// accept-time pressure the predictions were made from, against each basis's
// predicted rise. Rises and errors are set only on scored outcomes (held, and
// breached on a reading), so an unscored line has none rather than a zero; an
// error is predicted minus observed. A domain relief
// acted on in the window is breached with ReliefActed and Peak, relief's reading
// when it acted, and the outcome is not scored: relief has already moved Relieves.
type OutcomeLine struct {
	Domain       string   `json:"domain"`
	ObservedRise *float64 `json:"observed_rise,omitempty"`
	MapRise      *float64 `json:"map_rise,omitempty"`
	UsageRise    *float64 `json:"usage_rise,omitempty"`
	MapErr       *float64 `json:"map_err,omitempty"`
	UsageErr     *float64 `json:"usage_err,omitempty"`
	Breached     bool     `json:"breached"`
	ReliefActed  bool     `json:"relief_acted,omitempty"`
	Peak         *float64 `json:"peak,omitempty"`
}

// PredictionError is the mean absolute error of one admission prediction basis.
type PredictionError struct {
	N   int     `json:"n"`
	MAE float64 `json:"mean_absolute_error"`
}

// PredictionSummary compares the two bases on outcomes still retained by the
// tracker. Paired counts use only outcomes for which both errors were recorded.
type PredictionSummary struct {
	Outcomes int             `json:"outcomes"`
	Paired   int             `json:"paired"`
	Map      PredictionError `json:"map"`
	Usage    PredictionError `json:"usage"`
}

// TrackerConfig wires a tracker to the agent. Workloads, Shares and Moved are
// injected because the runtime and relief packages import peers, which imports
// this one.
type TrackerConfig struct {
	Map *statemap.Map
	// Workloads maps each workload's Name to its subject, as the runtime lists them.
	Workloads func(ctx context.Context) (map[string]string, error)
	// Shares is relief.SharesAt on Map; Moved is relief.Moved on Map at the lever's step.
	Shares func(usage string) map[string]float64
	Moved  func(usage string, at map[string]float64, except string) []string
	// Acted says whether relief acted in the action's domain since the time given,
	// and the highest contention it acted at.
	Acted func(action string, since time.Time) (peak float64, acted bool)
	Now   func() time.Time
	Logf  func(string, ...any)
}

// Tracker checks each accept against what the node observes once the workload
// arrives. It changes nothing: an outcome is recorded and listed, no more.
type Tracker struct {
	c TrackerConfig

	// tick serialises Tick, which alone touches an entry's subject and snapshot.
	tick sync.Mutex

	mu       sync.Mutex
	pending  []*entry // oldest first
	outcomes []Outcome
}

type entry struct {
	name, acceptID string
	at             time.Time
	domains        []Domain
	lines          []Line // the accept's, aligned with domains
	// shares are each domain's subject shares at accept: other workloads are
	// checked against them from accept to judgement.
	shares []map[string]float64

	subject string
	snap    *snapshot
}

// snapshot is the node at arrival, per domain, recorded beside the outcome. The
// rise is measured from the accept's pressure, which the predictions were made
// from; arrival only starts the settle clock.
type snapshot struct {
	at       time.Time
	relieves []float64
	usage    []float64
}

func NewTracker(c TrackerConfig) *Tracker {
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	return &Tracker{c: c}
}

// Accepted registers an accept for its outcome check.
func (t *Tracker) Accepted(o Offer, d Decision, domains []Domain) error {
	lines := make([]Line, len(domains))
	for i, dom := range domains {
		for _, l := range d.Lines {
			if l.Domain == dom.Relieves {
				lines[i] = l
			}
		}
	}
	shares := make([]map[string]float64, len(domains))
	for i, dom := range domains {
		shares[i] = t.c.Shares(dom.Usage)
	}
	e := &entry{name: o.Name, acceptID: d.ID, at: t.c.Now(), domains: slices.Clone(domains), lines: lines, shares: shares}

	// One critical section for the check and the insert, so two offers for one
	// name cannot both be registered.
	t.mu.Lock()
	defer t.mu.Unlock()
	if slices.ContainsFunc(t.pending, func(p *entry) bool { return p.name == o.Name }) {
		return ErrAlreadyPending
	}
	if len(t.pending) >= PendingCap {
		// The oldest is dropped with an outcome of its own, so GET /tasks shows it.
		t.c.Logf("admission: pending cap %d reached, dropped %s", PendingCap, t.pending[0].name)
		t.outcomes = append(t.outcomes, t.record(t.pending[0], Confounded, "dropped at the pending cap", nil, nil, t.c.Now()))
		t.trimOutcomes()
		t.pending = t.pending[1:]
	}
	t.pending = append(t.pending, e)
	return nil
}

// Pending says whether an accept for name awaits its outcome.
func (t *Tracker) Pending(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.ContainsFunc(t.pending, func(p *entry) bool { return p.name == name })
}

// PendingAccept is an accept awaiting its outcome.
type PendingAccept struct {
	Name       string    `json:"name"`
	AcceptID   string    `json:"accept_id"`
	AcceptedAt time.Time `json:"accepted_at"`
	// Arrived says the workload has appeared and been snapshotted; its settle runs.
	Arrived bool   `json:"arrived"`
	Subject string `json:"subject,omitempty"`
}

// PendingList lists the accepts awaiting an outcome, oldest first.
func (t *Tracker) PendingList() []PendingAccept {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pendingLocked()
}

// pendingLocked is PendingList; the caller holds mu.
func (t *Tracker) pendingLocked() []PendingAccept {
	out := make([]PendingAccept, len(t.pending))
	for i, e := range t.pending {
		out[i] = PendingAccept{Name: e.name, AcceptID: e.acceptID, AcceptedAt: e.at, Arrived: e.snap != nil, Subject: e.subject}
	}
	return out
}

// Outcomes lists recent outcomes, newest first.
func (t *Tracker) Outcomes() []Outcome {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := slices.Clone(t.outcomes)
	slices.Reverse(out)
	return out
}

// Memory is the tracker's state for the snapshot: outcomes oldest first, and
// the accepts still awaiting one.
type Memory struct {
	Outcomes []Outcome       `json:"outcomes"`
	Pending  []PendingAccept `json:"pending"`
}

// Memory is the tracker's state, for the snapshot.
// Both are read under one lock, so an accept judged meanwhile is in one or the
// other, never both.
func (t *Tracker) Memory() Memory {
	t.mu.Lock()
	defer t.mu.Unlock()
	return Memory{Outcomes: slices.Clone(t.outcomes), Pending: t.pendingLocked()}
}

// Restore replaces the outcomes with mem's and closes each accept that was
// pending as confounded: its arrival and settle clocks ran in a process that is
// gone, so nothing re-enters pending on this one's clock. Called before the
// tracker ticks.
func (t *Tracker) Restore(mem Memory) {
	now := t.c.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.outcomes = slices.Clone(mem.Outcomes)
	t.pending = nil
	for _, p := range mem.Pending {
		e := &entry{name: p.Name, acceptID: p.AcceptID, at: p.AcceptedAt, subject: p.Subject}
		t.outcomes = append(t.outcomes, t.record(e, Confounded, "agent restarted", nil, nil, now))
	}
	t.trimOutcomes()
}

// PredictionSummary describes prediction errors over the outcomes currently
// retained by the tracker. Confounded and otherwise unscored outcomes are omitted.
func (t *Tracker) PredictionSummary() PredictionSummary {
	outcomes := t.Outcomes()
	var summary PredictionSummary
	mapAbs, usageAbs := 0.0, 0.0
	for _, outcome := range outcomes {
		paired := false
		for _, line := range outcome.Lines {
			if line.MapErr == nil || line.UsageErr == nil {
				continue
			}
			paired = true
			summary.Paired++
			summary.Map.N++
			summary.Usage.N++
			mapAbs += math.Abs(*line.MapErr)
			usageAbs += math.Abs(*line.UsageErr)
		}
		if paired {
			summary.Outcomes++
		}
	}
	if summary.Map.N > 0 {
		summary.Map.MAE = mapAbs / float64(summary.Map.N)
		summary.Usage.MAE = usageAbs / float64(summary.Usage.N)
	}
	return summary
}

// Tick snapshots accepts whose workload has arrived, judges those whose settle
// has elapsed, and expires those whose workload never came.
func (t *Tracker) Tick(ctx context.Context) {
	t.tick.Lock()
	defer t.tick.Unlock()

	t.mu.Lock()
	work := slices.Clone(t.pending)
	t.mu.Unlock()
	if len(work) == 0 {
		return
	}

	// Without the list, arrivals cannot be seen, so nothing is expired either.
	names, err := t.c.Workloads(ctx)
	if err != nil {
		t.c.Logf("admission: listing workloads: %v", err)
	}
	now := t.c.Now()
	var done []Outcome
	var finished []*entry
	for _, e := range work {
		if e.snap == nil && err == nil {
			if s, ok := names[e.name]; ok && t.reporting(e, s) {
				snap := t.snapshot(e, now)
				// Under mu, so PendingList can read arrival while Tick runs.
				t.mu.Lock()
				e.subject, e.snap = s, snap
				t.mu.Unlock()
				t.c.Map.RecordEvent(statemap.EventAdmissionArrived, e.name, "agent",
					map[string]any{"name": e.name, "subject": s, "accept": e.acceptID})
			} else if now.Sub(e.at) > ArrivalWindow {
				done = append(done, t.record(e, NotArrived, "", nil, nil, now))
				finished = append(finished, e)
			}
			continue
		}
		if e.snap != nil && now.Sub(e.snap.at) >= settle(e.domains) {
			done = append(done, t.judge(e, now))
			finished = append(finished, e)
		}
	}
	if len(finished) == 0 {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	// An entry dropped at the cap while this tick ran already has its outcome.
	for i, e := range finished {
		if slices.Contains(t.pending, e) {
			t.outcomes = append(t.outcomes, done[i])
		}
	}
	t.pending = slices.DeleteFunc(t.pending, func(p *entry) bool { return slices.Contains(finished, p) })
	t.trimOutcomes()
}

// trimOutcomes keeps the newest outcomeCap outcomes; the caller holds mu.
func (t *Tracker) trimOutcomes() {
	if n := len(t.outcomes) - outcomeCap; n > 0 {
		t.outcomes = slices.Delete(t.outcomes, 0, n)
	}
}

// reporting says whether subject's usage is in the map on every domain: before
// that, the snapshot would miss part of the workload.
func (t *Tracker) reporting(e *entry, subject string) bool {
	for _, d := range e.domains {
		p, ok := t.c.Map.Property(d.Usage + "@" + subject)
		if !ok || p.Status == statemap.Retired {
			return false
		}
	}
	return true
}

func (t *Tracker) snapshot(e *entry, now time.Time) *snapshot {
	s := &snapshot{at: now}
	for _, d := range e.domains {
		s.relieves = append(s.relieves, value(t.c.Map, d.Relieves))
		s.usage = append(s.usage, value(t.c.Map, d.Usage))
	}
	return s
}

// judge checks a settled accept, in order: breached when relief acted in a domain
// in the window, since relief acts only past Enter and its action has already
// lowered Relieves (the workloads it capped free capacity the others absorb, so a
// move then is relief's doing and is recorded, not a confound); confounded when
// another workload moved since accept, which says nothing about the prediction;
// confounded when the admitted workload no longer reports, or on a stale reading;
// then breached past Enter, else held.
func (t *Tracker) judge(e *entry, now time.Time) Outcome {
	lines := make([]OutcomeLine, len(e.domains))
	for i, d := range e.domains {
		lines[i].Domain = d.Relieves
	}
	inputs := map[string]any{"relieves_at_arrival": slices.Clone(e.snap.relieves),
		"usage_at_arrival": slices.Clone(e.snap.usage)}

	var moved []string
	for i, d := range e.domains {
		moved = append(moved, t.c.Moved(d.Usage, e.shares[i], e.subject)...)
	}
	slices.Sort(moved)
	moved = slices.Compact(moved)
	if len(moved) > 0 {
		inputs["moved"] = moved
	}

	peaks := map[string]float64{}
	for i, d := range e.domains {
		if peak, acted := t.c.Acted(d.Action, e.at); acted {
			pk := peak
			lines[i].Breached, lines[i].ReliefActed, lines[i].Peak = true, true, &pk
			peaks[d.Relieves] = peak
		}
	}
	if len(peaks) > 0 {
		inputs["relief_acted"], inputs["peak"] = true, peaks
		return t.record(e, Breached, "", lines, inputs, now)
	}
	if len(moved) > 0 {
		return t.record(e, Confounded, "another workload moved: "+strings.Join(moved, ", "), lines, inputs, now)
	}
	// Relieves read without the workload says nothing about the prediction.
	if !t.reporting(e, e.subject) {
		return t.record(e, Confounded, "admitted workload no longer reporting", lines, inputs, now)
	}

	var stale []string
	for _, d := range e.domains {
		if p, ok := t.c.Map.Property(d.Relieves); ok && p.Status == statemap.Stale {
			stale = append(stale, d.Relieves)
		}
	}
	if len(stale) > 0 {
		inputs["stale"] = stale
		return t.record(e, Confounded, "stale reading: "+strings.Join(stale, ", "), lines, inputs, now)
	}

	result := Held
	settled := make([]float64, len(e.domains))
	baseline := make([]float64, len(e.domains))
	fed := map[string]statemap.Calibration{}
	for i, d := range e.domains {
		l, a := &lines[i], e.lines[i]
		v := value(t.c.Map, d.Relieves)
		settled[i], baseline[i] = v, a.Pressure
		observed := v - a.Pressure
		l.ObservedRise = &observed
		// Relief's trigger is strictly above Enter.
		if v > d.Enter {
			l.Breached, result = true, Breached
		}
		l.MapRise, l.MapErr = rise(a.MapPred, a.Pressure, observed)
		l.UsageRise, l.UsageErr = rise(a.UsagePred, a.Pressure, observed)

		// Each basis is judged on whether its predicted level fell on the same side of
		// the exit as the settled one.
		kind := "admission." + d.Relieves
		for basis, pred := range map[string]*float64{"map": a.MapPred, "usage": a.UsagePred} {
			if pred == nil {
				continue
			}
			t.c.Map.RecordPrediction(kind, basis, *pred, v, (*pred <= d.Exit) == (v <= d.Exit))
			fed[kind+"/"+basis] = t.c.Map.Calibration()[statemap.CalibrationKey{Kind: kind, Basis: basis}]
		}
	}
	if len(fed) > 0 {
		inputs["calibration"] = fed
	}
	inputs["relieves_at_accept"], inputs["relieves_now"] = baseline, settled
	return t.record(e, result, "", lines, inputs, now)
}

// record commits the outcome decision, linked to the accept.
func (t *Tracker) record(e *entry, result, reason string, lines []OutcomeLine, inputs map[string]any, now time.Time) Outcome {
	o := Outcome{Name: e.name, AcceptID: e.acceptID, OutcomeID: t.c.Map.NewDecisionID("admit"), Subject: e.subject,
		Result: result, Reason: reason, Lines: lines, At: now}
	b := t.c.Map.Decide(o.OutcomeID, "admission.outcome", "outcome of "+e.name+"?")
	b.Input("accept", e.acceptID)
	b.Cite(e.acceptID)
	b.Input("subject", e.subject)
	b.Input("lines", slices.Clone(lines))
	for k, v := range inputs {
		b.Input(k, v)
	}
	answer := map[string]any{"outcome": result}
	if reason != "" {
		answer["reason"] = reason
	}
	b.Commit(answer)
	return o
}

// rise is a basis's predicted rise over the pressure it was made from, and its
// error against the observed rise; nil when the basis made no prediction.
func rise(pred *float64, pressure, observed float64) (*float64, *float64) {
	if pred == nil {
		return nil, nil
	}
	r := *pred - pressure
	e := r - observed
	return &r, &e
}

// value reads a property as Decide does: absent, seeded or retired reads 0, the
// calm a node with no reporting subject is in.
func value(m *statemap.Map, id string) float64 {
	p, ok := m.Property(id)
	if !ok || p.NObservations == 0 || p.Status == statemap.Retired {
		return 0
	}
	return p.Value
}

func settle(domains []Domain) time.Duration {
	var s time.Duration
	for _, d := range domains {
		s = max(s, d.Settle)
	}
	return s
}
