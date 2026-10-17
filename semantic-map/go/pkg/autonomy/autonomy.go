// Package autonomy reviews the proposer's candidates on the agent's own authority,
// as far as the operator's autonomy level allows, and records every review as a
// decision in the map.
//
// A candidate goes up the tiers only as far as it has to. Tier 0 is rules in the
// daemon. Tier 1 is a decision model: one forward pass that returns a probability
// per action. Tier 2 is a reasoning model that reads the map through tools and
// writes an analysis, which the decision model then scores again, so the number an
// action is taken on is always the decision model's, never a reasoning model's
// opinion of itself. What no tier is sure enough about is escalated: it stays in the
// operator's queue with the review recorded on it.
//
// The level decides which tier may act and on which actions; below it, a tier's
// verdict is recorded as a suggestion. Every review, applied or not, is a decision
// record holding the candidate, the properties read, the exact text each model was
// given, each model's answer, and the level and thresholds in force, so
// GET /state/decisions/{id} reproduces it after the map has moved on.
package autonomy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DiyazY/di-agent/pkg/statemap"
	"github.com/DiyazY/di-agent/pkg/types"
)

// Level is how much the agent may decide on its own.
type Level int

const (
	// Suggest is today's behaviour: every tier may review and suggest, nothing is
	// applied, and the operator decides everything.
	Suggest Level = iota
	// Rules lets tier 0 act; the model tiers suggest.
	Rules
	// Bounded lets the model tiers confirm and defer above threshold; a model's
	// rejection is escalated instead.
	Bounded
	// Full lets every tier act above threshold; the operator sees the record, and
	// only what no tier was sure of reaches the queue.
	Full
)

var levelNames = []string{"suggest", "rules", "bounded", "full"}

func (l Level) String() string {
	if l < Suggest || l > Full {
		return "level(" + strconv.Itoa(int(l)) + ")"
	}
	return levelNames[l]
}

// ParseLevel accepts a level's number (0–3) or its name.
func ParseLevel(s string) (Level, error) {
	for i, n := range levelNames {
		if s == n || s == strconv.Itoa(i) {
			return Level(i), nil
		}
	}
	return 0, fmt.Errorf("autonomy level %q is not 0–3 or one of %s", s, strings.Join(levelNames, ", "))
}

// permits reports whether a tier may apply an action at this level.
func (l Level) permits(tier int, action string) bool {
	switch l {
	case Rules:
		return tier == 0
	case Bounded:
		return tier == 0 || action != types.ReviewReject
	case Full:
		return true
	}
	return false
}

// Thresholds are the probabilities a model tier's top action needs before it is
// taken. They differ because the actions differ in what undoing them costs: a
// confirmation can be retired with history kept, a deferral costs only time, and a
// rejection hides a pair until its evidence moves.
type Thresholds struct {
	Confirm float64 `json:"confirm"`
	Reject  float64 `json:"reject"`
	Defer   float64 `json:"defer"`
}

// DefaultThresholds are uncalibrated: no labelled set of candidates exists yet to
// calibrate them on, so they are set by what each action costs to undo.
var DefaultThresholds = Thresholds{Confirm: 0.85, Reject: 0.95, Defer: 0.7}

func (t Thresholds) of(action string) float64 {
	switch action {
	case types.ReviewConfirm:
		return t.Confirm
	case types.ReviewReject:
		return t.Reject
	case types.ReviewDefer:
		return t.Defer
	}
	return math.Inf(1)
}

// Map is what the reviewer needs from the semantic map.
type Map interface {
	PendingCandidates() ([]*types.CandidateEdge, error)
	CandidateHistory() ([]*types.CandidateEdge, error)
	ConfirmCandidate(candidateID, from string, rv types.CandidateReview) error
	RejectCandidate(candidateID string, rv types.CandidateReview) error
	DeferCandidate(candidateID string, rv types.CandidateReview) error
	AnnotateCandidate(candidateID string, rv types.CandidateReview) error
	State() *statemap.Map
}

// Evidence is what a model tier is told about one candidate.
type Evidence struct {
	Candidate types.CandidateEdge
	From, To  statemap.Property
	// Neighbours are other candidates sharing an end with this one: a property that
	// correlates with many others is more likely following load than driving anything.
	Neighbours []types.CandidateEdge
}

// Verdict is a decision model's answer.
type Verdict struct {
	Model   string `json:"model"`
	Backend string `json:"backend"`
	// Prompt names the question set and the renderer that wrote State.
	Prompt string `json:"prompt"`
	// State is the exact text the model was given.
	State string `json:"state"`
	// Actions holds a probability per action: confirm, reject, defer.
	Actions map[string]float64 `json:"actions"`
	// Orientation holds, for an undirected candidate, a probability per end that
	// the relationship runs from it, and for "unclear".
	Orientation map[string]float64 `json:"orientation,omitempty"`
	// OptionMass is, for a backend that reads probabilities off a general model's
	// tokens, how much of its probability fell on the options at all; 0 when the
	// backend answers over the options by construction.
	OptionMass float64 `json:"option_mass,omitempty"`
}

// MinOptionMass is the share of a general model's probability that has to fall on
// the options for its distribution over them to mean anything. Below it the model
// wanted to say something else, and the verdict escalates.
const MinOptionMass = 0.5

// Decider is tier 1.
type Decider interface {
	Name() string
	// Decide scores a candidate. analysis is a reasoning model's reading of the map,
	// empty on the first pass.
	Decide(ctx context.Context, e Evidence, analysis string) (*Verdict, error)
}

// Analysis is a reasoning model's reading of a candidate.
type Analysis struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Text   string `json:"analysis"`
	// Suggested is the action the model would take. It is recorded and not acted on:
	// the decision model scores the analysis, and its number is what crosses the
	// threshold.
	Suggested string   `json:"suggested_action,omitempty"`
	From      string   `json:"from,omitempty"`
	Trace     []string `json:"tool_trace,omitempty"`
}

// Reasoner is tier 2.
type Reasoner interface {
	Name() string
	Reason(ctx context.Context, e Evidence) (*Analysis, error)
}

// Config configures a Reviewer.
type Config struct {
	Level      Level
	Rules      []Rule        // tier 0, in order; the first that applies decides
	Interval   time.Duration // between review cycles; 0 means one minute
	MaxPer     int           // candidates reviewed per cycle; 0 means 10
	Thresholds Thresholds    // zero means DefaultThresholds
	Decider    Decider       // nil: no model tiers, whatever rules cannot settle escalates
	Reasoner   Reasoner      // nil: no tier 2; needs a Decider to score its analysis
	Logf       func(format string, args ...any)
	Now        func() time.Time
}

// Status is the reviewer as GET /autonomy and GET /state report it.
type Status struct {
	Level          string     `json:"level"`
	Interval       string     `json:"interval"`
	Thresholds     Thresholds `json:"thresholds"`
	Rules          []string   `json:"rules"`
	DecisionModel  string     `json:"decision_model,omitempty"`
	ReasoningModel string     `json:"reasoning_model,omitempty"`
	Cycles         int        `json:"cycles"`
	LastCycle      time.Time  `json:"last_cycle,omitempty"`
	// Cumulative counts of reviews by outcome. Applied and Suggested are actions a
	// tier was sure of, taken or reserved by the level; Escalated is what no tier
	// was sure of. Waiting is the last cycle's count of due candidates passed over
	// because an end is not active, so the evidence is not moving.
	Reviewed  int    `json:"reviewed"`
	Applied   int    `json:"applied"`
	Suggested int    `json:"suggested"`
	Escalated int    `json:"escalated"`
	Waiting   int    `json:"waiting"`
	Errors    int    `json:"errors"`
	LastError string `json:"last_error,omitempty"`
}

// Reviewer runs the review loop.
type Reviewer struct {
	m   Map
	cfg Config

	cycle  sync.Mutex // one cycle at a time
	mu     sync.Mutex
	status Status
	seq    uint64
}

// New builds a reviewer.
func New(m Map, cfg Config) (*Reviewer, error) {
	if cfg.Reasoner != nil && cfg.Decider == nil {
		return nil, fmt.Errorf("a reasoning model needs a decision model to score its analysis")
	}
	if cfg.Level < Suggest || cfg.Level > Full {
		return nil, fmt.Errorf("autonomy level %d is not 0–3", cfg.Level)
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Minute
	}
	if cfg.MaxPer <= 0 {
		cfg.MaxPer = 10
	}
	if cfg.Thresholds == (Thresholds{}) {
		cfg.Thresholds = DefaultThresholds
	}
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	r := &Reviewer{m: m, cfg: cfg}
	r.status = Status{Level: cfg.Level.String(), Interval: cfg.Interval.String(), Thresholds: cfg.Thresholds,
		Rules: []string{}}
	for _, rule := range cfg.Rules {
		r.status.Rules = append(r.status.Rules, rule.Name())
	}
	if cfg.Decider != nil {
		r.status.DecisionModel = cfg.Decider.Name()
	}
	if cfg.Reasoner != nil {
		r.status.ReasoningModel = cfg.Reasoner.Name()
	}
	return r, nil
}

// Status reports the reviewer's configuration and counts.
func (r *Reviewer) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

// Run reviews every Interval until ctx ends.
func (r *Reviewer) Run(ctx context.Context) {
	r.cfg.Logf("autonomy: reviewing candidates every %s at level %s", r.cfg.Interval, r.cfg.Level)
	t := time.NewTicker(r.cfg.Interval)
	defer t.Stop()
	for {
		r.Cycle(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Cycle reviews up to MaxPer candidates that are due: pending, and never reviewed or
// with evidence that has moved since their last review. Strongest first, so a full
// queue spends the models on the candidates most likely to matter.
func (r *Reviewer) Cycle(ctx context.Context) {
	r.cycle.Lock()
	defer r.cycle.Unlock()
	pending, err := r.m.PendingCandidates()
	if err != nil {
		r.failed(err)
		return
	}
	var due []*types.CandidateEdge
	for _, c := range pending {
		if c.Review.Outdated(c) {
			due = append(due, c)
		}
	}
	sort.SliceStable(due, func(i, j int) bool {
		return due[i].MIScore*float64(due[i].NObservations) > due[j].MIScore*float64(due[j].NObservations)
	})
	reviewed, waiting := 0, 0
	for _, c := range due {
		if reviewed >= r.cfg.MaxPer || ctx.Err() != nil {
			break
		}
		if !r.endsActive(c) {
			waiting++
			continue
		}
		if r.review(ctx, c, pending) {
			reviewed++
		}
	}
	r.mu.Lock()
	r.status.Waiting = waiting
	r.status.Cycles++
	r.status.LastCycle = r.cfg.Now()
	r.mu.Unlock()
}

// outcome is what a review settled on.
type outcome struct {
	tier   int
	by     string
	model  string
	action string // confirm, reject, defer, or escalate
	from   string // the end an undirected confirmation runs from
	why    string
}

// review takes one candidate up the tiers. It reports false when the candidate was
// passed over without a decision.
func (r *Reviewer) review(ctx context.Context, c *types.CandidateEdge, pending []*types.CandidateEdge) bool {
	st := r.m.State()
	if st == nil {
		r.failed(fmt.Errorf("the map has no state model to record decisions in"))
		return false
	}
	id := r.decisionID(c)
	b := st.Decide(id, "review candidate "+c.CandidateID)
	from, _ := b.Property(c.FromID)
	to, _ := b.Property(c.ToID)
	ev := Evidence{Candidate: *c, From: from, To: to, Neighbours: neighbours(c, pending)}
	ev.Candidate.Caveats = append([]string(nil), c.Caveats...)
	ev.Candidate.Review = nil
	b.Input("candidate", ev.Candidate)
	b.Input("neighbours", ev.Neighbours)
	b.Input("autonomy", map[string]any{"level": r.cfg.Level.String(), "thresholds": r.cfg.Thresholds})

	o := r.decide(ctx, b, ev)
	applied, err := r.apply(c, o, id)
	if err != nil {
		b.Caveat("applying %s failed: %v; the candidate was escalated instead", o.action, err)
		o.action, applied = types.ReviewEscalate, false
		_ = r.m.AnnotateCandidate(c.CandidateID, r.reviewOf(o, id))
	}
	b.Note("tier %d (%s): %s", o.tier, o.by, o.why)
	answer := map[string]any{
		"action": o.action, "applied": applied, "tier": o.tier, "by": o.by,
		"level": r.cfg.Level.String(),
	}
	if o.model != "" {
		answer["model"] = o.model
	}
	if o.from != "" {
		answer["from"] = o.from
	}
	b.Commit(answer)

	r.count(func(s *Status) {
		s.Reviewed++
		switch {
		case o.action == types.ReviewEscalate:
			s.Escalated++
		case applied:
			s.Applied++
		default:
			s.Suggested++
		}
	})
	verb := "suggests"
	if applied {
		verb = "applied"
	}
	r.cfg.Logf("autonomy: %s %s %s (tier %d, %s; decision %s)", verb, o.action, c.CandidateID, o.tier, o.by, id)
	return true
}

// endsActive reports whether both ends of c are active. An end that is stale or gone
// is not moving, and nor is the pair's evidence: a decision now would be on a frozen
// window, so the candidate waits until the end returns.
func (r *Reviewer) endsActive(c *types.CandidateEdge) bool {
	st := r.m.State()
	if st == nil {
		return false
	}
	for _, end := range []string{c.FromID, c.ToID} {
		if p, ok := st.Property(end); !ok || p.Status != statemap.Active {
			return false
		}
	}
	return true
}

// decide runs the tiers, recording what each read and answered.
func (r *Reviewer) decide(ctx context.Context, b *statemap.DecisionBuilder, ev Evidence) outcome {
	if o, ok := r.rules(ev); ok {
		return o
	}
	if r.cfg.Decider == nil {
		return outcome{tier: 0, by: types.ReviewerRules, action: types.ReviewEscalate,
			why: "no rule applies and no decision model is configured"}
	}
	v, err := r.cfg.Decider.Decide(ctx, ev, "")
	if err != nil {
		b.Caveat("decision model: %v", err)
		r.failed(err)
		return outcome{tier: 1, by: types.ReviewerDecisionModel, model: r.cfg.Decider.Name(),
			action: types.ReviewEscalate, why: "the decision model could not be reached"}
	}
	b.Input("tier1", v)
	o := r.choose(v, ev, 1, types.ReviewerDecisionModel, r.cfg.Decider.Name())
	if o.action != types.ReviewEscalate || r.cfg.Reasoner == nil {
		return o
	}
	a, err := r.cfg.Reasoner.Reason(ctx, ev)
	if err != nil {
		b.Caveat("reasoning model: %v", err)
		r.failed(err)
		o.why += "; the reasoning model failed"
		return o
	}
	b.Input("tier2_analysis", a)
	b.Caveat("the tier 2 analysis is the reasoning model's reading of the map; its tool calls are recorded with it")
	v2, err := r.cfg.Decider.Decide(ctx, ev, a.Text)
	if err != nil {
		b.Caveat("decision model on the analysis: %v", err)
		r.failed(err)
		o.why += "; the decision model could not score the analysis"
		return o
	}
	b.Input("tier2", v2)
	return r.choose(v2, ev, 2, types.ReviewerReasoningAgent, r.cfg.Reasoner.Name()+" + "+r.cfg.Decider.Name())
}

// choose turns a verdict into an outcome: the top action when its probability
// reaches the action's threshold, and for an undirected confirmation an end the
// relationship runs from at the same bar; otherwise escalation.
func (r *Reviewer) choose(v *Verdict, ev Evidence, tier int, by, model string) outcome {
	o := outcome{tier: tier, by: by, model: model, action: types.ReviewEscalate}
	top, p := argmax(v.Actions)
	if top == "" {
		o.why = "the model returned no probabilities"
		return o
	}
	if v.OptionMass > 0 && v.OptionMass < MinOptionMass {
		o.why = fmt.Sprintf("only %.2f of the model's probability fell on the options", v.OptionMass)
		return o
	}
	if p < r.cfg.Thresholds.of(top) {
		o.why = fmt.Sprintf("its top action %s had p=%.3f, under the %.2f threshold", top, p, r.cfg.Thresholds.of(top))
		return o
	}
	if top == types.ReviewConfirm && ev.Candidate.Undirected {
		end, pe := argmax(v.Orientation)
		if end != ev.Candidate.FromID && end != ev.Candidate.ToID || pe < r.cfg.Thresholds.Confirm {
			o.why = fmt.Sprintf("confirm at p=%.3f, but no end was the driver at the %.2f threshold (%s, p=%.3f)",
				p, r.cfg.Thresholds.Confirm, orNone(end), pe)
			return o
		}
		o.from = end
		o.why = fmt.Sprintf("confirm at p=%.3f, running from %s at p=%.3f", p, end, pe)
		o.action = top
		return o
	}
	o.action = top
	o.why = fmt.Sprintf("%s at p=%.3f (threshold %.2f)", top, p, r.cfg.Thresholds.of(top))
	return o
}

// apply takes the outcome, or records it as a suggestion when the level reserves
// the action. It reports whether the action was applied.
func (r *Reviewer) apply(c *types.CandidateEdge, o outcome, id string) (bool, error) {
	rv := r.reviewOf(o, id)
	if o.action == types.ReviewEscalate || !r.cfg.Level.permits(o.tier, o.action) {
		return false, r.m.AnnotateCandidate(c.CandidateID, rv)
	}
	switch o.action {
	case types.ReviewConfirm:
		return true, r.m.ConfirmCandidate(c.CandidateID, o.from, rv)
	case types.ReviewReject:
		return true, r.m.RejectCandidate(c.CandidateID, rv)
	case types.ReviewDefer:
		return true, r.m.DeferCandidate(c.CandidateID, rv)
	}
	return false, fmt.Errorf("unknown action %q", o.action)
}

func (r *Reviewer) reviewOf(o outcome, id string) types.CandidateReview {
	return types.CandidateReview{By: o.by, Model: o.model, Tier: o.tier, Action: o.action,
		DecisionID: id, At: r.cfg.Now()}
}

// decisionID names a review's decision record: the candidate's hash, so a reader can
// group a candidate's reviews, and a sequence number, so two never share an id.
func (r *Reviewer) decisionID(c *types.CandidateEdge) string {
	r.mu.Lock()
	r.seq++
	n := r.seq
	r.mu.Unlock()
	h := sha256.Sum256([]byte(c.CandidateID))
	return fmt.Sprintf("review-%s-%d-%d", hex.EncodeToString(h[:4]), r.cfg.Now().Unix(), n)
}

func (r *Reviewer) failed(err error) {
	r.count(func(s *Status) { s.Errors++; s.LastError = err.Error() })
	r.cfg.Logf("autonomy: %v", err)
}

func (r *Reviewer) count(f func(*Status)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f(&r.status)
}

// neighbours are the other pending candidates sharing an end with c, strongest
// first, at most eight.
func neighbours(c *types.CandidateEdge, pending []*types.CandidateEdge) []types.CandidateEdge {
	var out []types.CandidateEdge
	for _, o := range pending {
		if o.CandidateID == c.CandidateID {
			continue
		}
		if o.FromID == c.FromID || o.FromID == c.ToID || o.ToID == c.FromID || o.ToID == c.ToID {
			n := *o
			n.Caveats, n.Review = nil, nil
			out = append(out, n)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].MIScore > out[j].MIScore })
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}

func argmax(m map[string]float64) (string, float64) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys) // a tie goes the same way every time
	best, bp := "", -1.0
	for _, k := range keys {
		if m[k] > bp {
			best, bp = k, m[k]
		}
	}
	return best, math.Max(bp, 0)
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
