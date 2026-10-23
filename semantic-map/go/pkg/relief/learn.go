package relief

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/DiyazY/di-agent/pkg/statemap"
	"github.com/DiyazY/di-agent/pkg/types"
)

// Candidates is the discovery queue an intervention can settle: the pending
// candidates, and the confirmation an operator would otherwise give.
type Candidates interface {
	PendingCandidates() ([]*types.CandidateEdge, error)
	ConfirmCandidate(candidateID, from string, rv types.CandidateReview) error
}

const (
	// effectAlpha is how much one outcome moves a workload's learned effect.
	effectAlpha = 0.3
	// effectMin and effectMax bound it: one workload never has its predictions
	// scaled to nothing, nor more than doubled, on its own history.
	effectMin, effectMax = 0.1, 2.0
	// confoundAbs is the smallest absolute change in another workload's share of
	// node CPU that confounds an outcome: below it, a relative change is a small
	// pod's noise.
	confoundAbs = 0.02
	// bindRatio is how close to its cap a throttled workload's usage must be at
	// verify for the cap to have bound: below it, the workload asked for less on
	// its own, and the drop in pressure is its demand falling, not the cap.
	bindRatio = 0.9
)

// effects is, per workload and prediction basis, what acting on it has done
// against what was predicted: an EMA of observed/predicted drop in node pressure,
// starting from 1, clamped to [0.1, 2]. Later predictions for the workload on the
// same basis are scaled by it; a usage-share prediction and a map prediction err
// differently, so one's factor never scales the other. It is held in memory only
// and is lost on restart, when every workload starts again from 1.
type effects struct {
	mu sync.Mutex
	m  map[effectKey]float64
}

type effectKey struct{ subject, basis string }

// Observe folds one outcome's observed/predicted ratio into subject's effect for
// predictions of basis. A ratio that is not a number teaches nothing.
func (e *effects) Observe(subject, basis string, ratio float64) {
	if math.IsNaN(ratio) || math.IsInf(ratio, 0) {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.m == nil {
		e.m = map[effectKey]float64{}
	}
	k := effectKey{subject, basis}
	v, ok := e.m[k]
	if !ok {
		v = 1
	}
	v = (1-effectAlpha)*v + effectAlpha*ratio
	e.m[k] = math.Min(effectMax, math.Max(effectMin, v))
}

// Forget drops subject's effect for predictions of basis, which is 1 again.
func (e *effects) Forget(subject, basis string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.m, effectKey{subject, basis})
}

// Of is subject's learned effect for predictions of basis, 1 when nothing has
// been learned about it.
func (e *effects) Of(subject, basis string) float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if v, ok := e.m[effectKey{subject, basis}]; ok {
		return v
	}
	return 1
}

// workloadShares is every workload's current share of the node's usage, as the
// map holds it: each subject with an active usage@<subject>.
func workloadShares(m *statemap.Map, usage string) map[string]float64 {
	out := map[string]float64{}
	for _, s := range m.Subjects() {
		if v, ok := share(m, usage, s); ok {
			out[s] = v
		}
	}
	return out
}

// movedSince lists, sorted, the workloads other than throttled whose share of
// node CPU has moved since at was taken by more than step relative to their
// share then and by more than 0.02 absolute. A workload live now and absent from
// at arrived since, from a share of nothing. left lists those that were using
// more than 0.02 then and are not live now: their going took their load with
// them.
//
// Rises are the exception. Capping a workload frees CPU, and the contended
// workloads beside it take it up: that is the relief working, not something else
// moving. So workloads that were there at act and rose together by no more than
// throttled gave up (with 0.02 of slack) are returned as absorbed rather than
// moved; a larger rise, a fall, an arrival and a departure always count as moved.
// What throttled gave up is known only while the map holds its share: without
// one nothing counts as freed. Whether an absorbed rise is innocent depends on the
// verdict, which confoundedBy decides.
func movedSince(m *statemap.Map, usage string, at map[string]float64, throttled string, step float64) (moved, left, absorbed []string) {
	live := workloadShares(m, usage)
	var risen []string
	rise := 0.0
	for s, now := range live {
		if s == throttled {
			continue
		}
		then, was := at[s]
		d := math.Abs(now - then)
		if d <= step*then || d <= confoundAbs {
			continue
		}
		if was && now > then {
			risen = append(risen, s)
			rise += now - then
			continue
		}
		moved = append(moved, s)
	}
	freed := 0.0
	if now, ok := live[throttled]; ok {
		freed = math.Max(at[throttled]-now, 0)
	}
	if rise > freed+confoundAbs {
		moved = append(moved, risen...)
	} else {
		absorbed = risen
	}
	for s, then := range at {
		if _, ok := live[s]; !ok && s != throttled && then > confoundAbs {
			left = append(left, s)
		}
	}
	sort.Strings(moved)
	sort.Strings(left)
	sort.Strings(absorbed)
	return moved, left, absorbed
}

// measuredEffect is what one outcome measured about the relationship it tests: the
// drop in node contention per unit drop in the workload's share of node CPU, both
// fractions of the node, so in the range-normalised units the estimate projects in.
// It is clamped to [0, 1] like every strength, a rise in contention measuring zero.
// A share that fell by no more than confoundAbs gives nothing to divide by, and no
// effect is measured.
func measuredEffect(observed, shareAtAct, shareNow float64) (float64, bool) {
	drop := shareAtAct - shareNow
	if drop <= confoundAbs {
		return 0, false
	}
	return math.Min(1, math.Max(0, observed/drop)), true
}

// confoundedBy is the workloads an outcome cannot be separated from. Moved and left
// always are. An absorbed rise is the relief working only when the outcome agreed:
// when contention did not drop as predicted, the same rise may be a workload that
// grew on its own and hid the cap's effect, and passing it over would turn a
// confound into a null outcome that teaches — two of which retire the relationship.
func confoundedBy(moved, left, absorbed []string, agreed bool) []string {
	out := append(append([]string{}, moved...), left...)
	if !agreed {
		out = append(out, absorbed...)
	}
	sort.Strings(out)
	return out
}

// confounded reports whether another workload moved, arrived or left while the
// throttled one's outcome was being observed, so the change in node pressure
// cannot be put down to the action alone.
func confounded(m *statemap.Map, usage string, at map[string]float64, throttled string, step float64, agreed bool) bool {
	moved, left, absorbed := movedSince(m, usage, at, throttled, step)
	return len(confoundedBy(moved, left, absorbed, agreed)) > 0
}

// learn makes a judged action evidence, when it is: an intervention recorded on
// Usage@<subject> → Relieves (cpu_utilization@<subject> → cpu_contention_ratio
// for cpu.limit), the workload's effect updated
// when nothing else moved, and a pending candidate on that pair settled when the
// interventions created or promoted the relationship. What it fed, and why not
// when it did not, goes into the verify decision b. Called under the pass lock,
// with mu not held.
//
// An outcome is not evidence when the runtime changes nothing (a dry run), when a
// resumed action's baseline was taken with its change already in force, when there
// is no prediction, or when the cap did not bind: a workload using well under its
// new limit at verify asked for less on its own, so the drop is not the cap's.
func (l *Loop) learn(b *statemap.DecisionBuilder, verifyID string, a *Action, agreed bool, observed float64, now time.Time) {
	moved, left, absorbed := movedSince(l.cfg.Map, l.d.Usage, a.SharesAtAct, a.Subject, l.c.Step)
	by := confoundedBy(moved, left, absorbed, agreed)
	conf := len(by) > 0
	b.Input("confounded", map[string]any{"confounded": conf, "moved": moved, "left": left, "absorbed": absorbed})

	ss := map[string]any{"at_act": nil, "at_verify": nil}
	if v, ok := a.SharesAtAct[a.Subject]; ok {
		ss["at_act"] = v
	}
	nowShare, haveShare := share(l.cfg.Map, l.d.Usage, a.Subject)
	if haveShare {
		ss["at_verify"] = nowShare
	}
	b.Input("subject_share", ss)

	why := ""
	switch {
	case !l.cfg.Evidence:
		why = "the runtime does not change the system (" + l.cfg.Runtime.Name() + "): contention moved on its own"
	case a.Resumed:
		why = "resumed: its baseline was taken with the change already in force"
	case a.Plan.Predicted <= 0:
		why = "no prediction to judge the outcome against"
	default:
		why = l.unbound(a, nowShare, haveShare)
	}
	if why != "" {
		b.Input("evidence", map[string]any{"fed": false, "why": why})
		return
	}
	b.Input("evidence", map[string]any{"fed": true, "confounded": conf})
	if conf {
		b.Note("other workloads moved while the outcome was observed (%s): recorded, teaches nothing", strings.Join(by, ", "))
	}

	from := l.d.Usage + "@" + a.Subject
	o := statemap.InterventionOutcome{From: from, To: l.d.Relieves, Agreed: agreed, Confounded: conf, DecisionID: verifyID}
	if then, ok := a.SharesAtAct[a.Subject]; ok && haveShare {
		o.Effect, o.HasEffect = measuredEffect(observed, then, nowShare)
	}
	rel, changed, err := l.cfg.Map.RecordIntervention(o)
	iv := map[string]any{"relationship": rel, "changed": changed}
	if o.HasEffect {
		iv["effect"] = o.Effect
	}
	if err != nil {
		iv["error"] = err.Error()
		b.Caveat("recording the intervention: %v", err)
	}
	b.Input("intervention", iv)

	if !conf {
		effect := a.Plan.Effect
		if effect <= 0 {
			effect = 1
		}
		// Against the map's own prediction, before this workload's effect scaled it,
		// so the effect converges on the workload's real response rather than on 1.
		ratio := observed * effect / a.Plan.Predicted
		l.effects.Observe(a.Subject, a.Plan.Basis, ratio)
		if o.HasEffect && err == nil {
			// The relationship's strength is now what the interventions measured, so a
			// map prediction carries it; a factor learned against the old strength
			// would correct it twice.
			l.effects.Forget(a.Subject, BasisMap)
		}
		b.Input("effect", map[string]any{"basis": a.Plan.Basis, "ratio": ratio, "now": l.effects.Of(a.Subject, a.Plan.Basis)})
	}

	if changed == "created" || changed == "promoted" {
		l.settle(b, verifyID, from, now)
	}
}

// unbound says why the cap could not have caused the outcome, or "" when it
// bound: the workload's usage at verify, in the action's unit, is at least 0.9
// of its new setting. With no current usage or no capacity fact, binding cannot
// be shown.
func (l *Loop) unbound(a *Action, nowShare float64, haveShare bool) string {
	n, ok := capacity(l.cfg.Map, l.d)
	switch {
	case !haveShare:
		return "the cap's effect cannot be shown: " + a.Subject + " has no current usage"
	case !ok:
		return "the cap's effect cannot be shown: " + noCapacity(l.d)
	}
	if used := nowShare * n; used < bindRatio*a.Plan.To {
		return fmt.Sprintf("the cap did not bind: usage %.3f %s under the %.3f-%s limit", used, l.d.Unit, a.Plan.To,
			strings.TrimSuffix(l.d.Unit, "s"))
	}
	return ""
}

// settle confirms the pending candidate on from → the node's Relieves, if there is
// one: directed that way, or undirected in either order. What it settled, or why
// it could not, goes into the verify decision.
func (l *Loop) settle(b *statemap.DecisionBuilder, verifyID, from string, now time.Time) {
	if l.cfg.Candidates == nil {
		return
	}
	pending, err := l.cfg.Candidates.PendingCandidates()
	if err != nil {
		b.Caveat("listing pending candidates: %v", err)
		return
	}
	for _, c := range pending {
		to := l.d.Relieves
		directed := !c.Undirected && c.FromID == from && c.ToID == to
		undirected := c.Undirected && ((c.FromID == from && c.ToID == to) || (c.FromID == to && c.ToID == from))
		if !directed && !undirected || c.Direction == types.Negative {
			continue
		}
		rv := types.CandidateReview{By: types.ReviewerIntervention, Tier: 0, Action: types.ReviewConfirm,
			DecisionID: verifyID, At: now}
		if err := l.cfg.Candidates.ConfirmCandidate(c.CandidateID, from, rv); err != nil {
			b.Caveat("settling candidate %s: %v", c.CandidateID, err)
			return
		}
		b.Input("candidate", c.CandidateID)
		b.Note("settled candidate %s: interventions showed what it proposed", c.CandidateID)
		return
	}
}
