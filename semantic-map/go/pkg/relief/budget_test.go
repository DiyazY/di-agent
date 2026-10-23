package relief

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/actuators"
	"github.com/DiyazY/di-agent/pkg/runtime"
	"github.com/DiyazY/di-agent/pkg/spec"
)

func TestBudgetCountsTheLastHour(t *testing.T) {
	b := NewBudget(2)
	if !b.Allow(t0) {
		t.Fatal("a new budget refuses")
	}
	b.Spend(t0)
	b.Spend(t0.Add(10 * time.Minute))
	if b.Allow(t0.Add(30*time.Minute)) || b.Count(t0.Add(30*time.Minute)) != 2 {
		t.Fatalf("two acts within the hour of max 2: allow %v count %d", b.Allow(t0.Add(30*time.Minute)), b.Count(t0.Add(30*time.Minute)))
	}
	if !b.Allow(t0.Add(time.Hour)) || b.Count(t0.Add(time.Hour)) != 1 {
		t.Fatalf("an hour on, the first act no longer counts: count %d", b.Count(t0.Add(time.Hour)))
	}
	// An act spent at its journalled time, older than the latest, still counts in order.
	b.Spend(t0.Add(5 * time.Minute))
	if n := b.Count(t0.Add(time.Hour)); n != 2 {
		t.Fatalf("count %d after spending an earlier act; want 2", n)
	}
	if n := b.Count(t0.Add(time.Hour + 6*time.Minute)); n != 1 {
		t.Fatalf("count %d; want the 5m act gone after 1h5m", n)
	}
}

func TestBudgetReserveIsAtomic(t *testing.T) {
	b := NewBudget(1)
	until := t0.Add(10 * time.Minute)
	if ok, _ := b.reserve("cpu.limit", "pod:a", t0, 1, until); !ok {
		t.Fatal("a new budget refused")
	}
	if ok, why := b.reserve("cpu.limit", "pod:b", t0, 1, until); ok || why != reserveLimit {
		t.Fatalf("reserve let two acts through a limit of one (%v, %q)", ok, why)
	}
	b.refund(t0)
	if ok, _ := b.reserve("cpu.limit", "pod:b", t0, 1, until); !ok {
		t.Fatal("a refunded act still counts")
	}
	// A rule's own limit, above the default, is what reserve is asked against.
	if ok, _ := b.reserve("cpu.limit", "pod:c", t0, 3, until); !ok {
		t.Fatal("reserve ignored the limit it was given")
	}
	// The reservation cooled pod:a for every other lever, and not for its own.
	if ok, why := b.reserve("memory.high", "pod:a", t0, 6, until); ok || why != reserveCooling {
		t.Fatalf("another lever reserved pod:a (%v, %q); want it cooling", ok, why)
	}
	if ok, _ := b.reserve("cpu.limit", "pod:a", t0, 6, until); !ok {
		t.Fatal("the lever that acted was held off its own change")
	}
	// A refused reservation spent nothing: pod:b, pod:c and pod:a again count, the
	// refunded first one does not.
	if n := b.Count(t0); n != 3 {
		t.Fatalf("count %d; want the three reservations that stand", n)
	}
}

func TestBudgetCooldown(t *testing.T) {
	b := NewBudget(6)
	b.Cool("pod:a", t0.Add(10*time.Minute))
	b.Cool("pod:a", t0.Add(5*time.Minute)) // a shorter cooldown does not cut a longer one short
	if !b.Cooling("pod:a", t0.Add(9*time.Minute)) || b.Cooling("pod:a", t0.Add(10*time.Minute)) {
		t.Fatal("cooldown is not until the later time")
	}
	if b.Cooling("pod:b", t0) {
		t.Fatal("pod:b cools without being cooled")
	}
	// After an act, the workload cools for every other lever, not for the one that
	// acted: it may step its own change again.
	b.coolExcept("pod:c", "cpu.limit", t0.Add(10*time.Minute))
	if _, ok := b.coolingFor("cpu.limit", "pod:c", t0); ok {
		t.Fatal("the acting lever is held off its own change")
	}
	if until, ok := b.coolingFor("memory.high", "pod:c", t0); !ok || !until.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("another lever: cooling %v until %v", ok, until)
	}
	if !b.Cooling("pod:c", t0) {
		t.Fatal("Cooling, which names no lever, does not see it")
	}
}

// memory is a second lever for the loop tests: cpu.limit's descriptor under
// another action, so it relieves the same pressure through a change of its own.
var memory = func() actuators.Descriptor { d := cpu; d.Action = "memory.high"; return d }()

// second is a loop on the harness's map, runtime and ledger that acts through
// memory, sharing budget with the harness's own loop.
func (h *harness) second(budget *Budget) *Loop {
	h.t.Helper()
	return h.secondWith(budget, actuators.Static(memory, nil, func(runtime.Workload) (float64, bool) { return 0, false }))
}

// secondWith is second acting through act, whose Describe must be memory.
func (h *harness) secondWith(budget *Budget, act actuators.Actuator) *Loop {
	h.t.Helper()
	lever := spec.Default().Loops.Relief.Levers[0]
	lever.Action = memory.Action
	l, err := New(Config{Interval: tick, Lever: lever, Descriptor: memory, NodeID: "n1", Runtime: h.rt,
		Acting: actuators.NewActing("n1", h.ledger, act), Policy: h.l.cfg.Policy, Map: h.m,
		Now: h.clock, Logf: h.t.Logf, Budget: budget})
	if err != nil {
		h.t.Fatal(err)
	}
	return l
}

// both steps both loops at pressure p for d: the harness's first.
func (h *harness) both(l2 *Loop, d time.Duration, p float64) {
	h.t.Helper()
	for i := time.Duration(0); i < d; i += tick {
		h.step(p)
		l2.Tick(h.ctx)
	}
}

// actsBy is the act decisions the loop for action made.
func (h *harness) actsBy(action string) []string {
	var out []string
	for _, d := range h.decisions("act") {
		if strings.HasPrefix(d.ID, "relief-act-"+action+"-") {
			out = append(out, d.ID)
		}
	}
	return out
}

func leverPolicy(max int) func(*spec.PolicySpec) {
	return func(ps *spec.PolicySpec) {
		ps.Defaults.MaxActionsPerHour = max
		// memory here is in cores, as cpu.limit is.
		ps.Defaults.Floor["memory.high"] = spec.NewBound(0.1, 0.5)
	}
}

// TestBudgetSharedAcrossLevers: max_actions_per_hour is one count for every
// lever on the node. With a limit of one, the cpu.limit loop's act spends it, and
// the memory loop refuses for the rest of the hour.
func TestBudgetSharedAcrossLevers(t *testing.T) {
	budget := NewBudget(1)
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4, "pod:b": 0.3},
		policy: leverPolicy(1), budget: budget})
	l2 := h.second(budget)
	h.both(l2, 50*time.Minute, 0.5)
	if n := len(h.actsBy("cpu.limit")); n != 1 {
		t.Fatalf("cpu.limit acted %d times; want once", n)
	}
	if n := len(h.actsBy("memory.high")); n != 0 {
		t.Fatalf("memory.high acted %d times within the hour cpu.limit spent", n)
	}
	refused := false
	for _, d := range h.decisions("refused") {
		if strings.Contains(d.ID, "memory.high") && d.Answer["refused"] == "max_actions_per_hour 1 reached" {
			refused = true
		}
	}
	if !refused {
		t.Fatal("the memory loop did not journal the shared limit as its reason")
	}
}

// TestLeversShareCooldown: a workload one lever acted on is cooling for the
// other, so two levers do not stack changes on it; the lever that acted may still
// step its own change.
func TestLeversShareCooldown(t *testing.T) {
	budget := NewBudget(6)
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4, "pod:b": 0.3},
		policy: leverPolicy(6), budget: budget})
	l2 := h.second(budget)
	for i := 0; i < 20 && len(h.actsBy("memory.high")) == 0; i++ {
		h.both(l2, tick, 0.5)
	}
	cpuActs, memActs := h.actsBy("cpu.limit"), h.actsBy("memory.high")
	if len(cpuActs) != 1 || len(memActs) != 1 {
		t.Fatalf("acts: cpu.limit %v, memory.high %v; want one each", cpuActs, memActs)
	}
	if a := h.action("pod:a"); a.DecisionID != cpuActs[0] {
		t.Fatalf("cpu.limit acted on %s; want pod:a, the largest share", h.l.Actions()[0].Subject)
	}
	mem := l2.Actions()
	if len(mem) != 1 || mem[0].Subject != "pod:b" {
		t.Fatalf("memory.high acted on %+v; want pod:b, as pod:a is cooling after cpu.limit's act", mem)
	}
	if _, ok := l2.Status().Cooling["pod:a"]; !ok {
		t.Errorf("memory.high's status does not list pod:a cooling: %v", l2.Status().Cooling)
	}
	if _, ok := h.l.Status().Cooling["pod:a"]; ok {
		t.Errorf("cpu.limit's status lists its own change as cooling: %v", h.l.Status().Cooling)
	}
	if st := l2.Status(); st.Action != "memory.high" {
		t.Errorf("status action %q", st.Action)
	}
}

// slowSet is an actuator whose first Set waits for release, after saying so on
// entered: a change on its way through a slow orchestrator.
type slowSet struct {
	actuators.Actuator
	once     sync.Once
	entered  chan struct{}
	release  chan struct{}
	mu       sync.Mutex
	subjects []string // every Set, by subject
}

func newSlowSet(a actuators.Actuator) *slowSet {
	return &slowSet{Actuator: a, entered: make(chan struct{}), release: make(chan struct{})}
}

func (b *slowSet) Set(ctx context.Context, w runtime.Workload, v float64, set bool) error {
	b.mu.Lock()
	b.subjects = append(b.subjects, w.Subject)
	b.mu.Unlock()
	first := false
	b.once.Do(func() { first = true })
	if first {
		close(b.entered)
		<-b.release
	}
	return b.Actuator.Set(ctx, w, v, set)
}

func (b *slowSet) sets() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.subjects...)
}

// TestLeversDoNotStackWhileOneIsApplying: the shared cooldown is reserved with
// the hourly count before the change, not after it lands. While cpu.limit's change
// to pod:a is still inside the orchestrator, memory.high, ticking at the same
// moment with pod:a its only candidate, finds pod:a cooling and changes nothing.
func TestLeversDoNotStackWhileOneIsApplying(t *testing.T) {
	budget := NewBudget(6)
	var cpuSlow *slowSet
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4}, policy: leverPolicy(6), budget: budget,
		wrap: func(rt *runtime.DryRun, a actuators.Actuator) (runtime.Runtime, actuators.Actuator) {
			cpuSlow = newSlowSet(a)
			return rt, cpuSlow
		}})
	mem := newSlowSet(actuators.Static(memory, nil, func(runtime.Workload) (float64, bool) { return 0, false }))
	close(mem.release) // memory.high's Set, if it came, would not wait
	l2 := h.secondWith(budget, mem)

	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		h.advance(tick)
		record(t, h.m, contention, "", 0.5, h.now)
		tickDone := make(chan struct{})
		go func() { h.l.Tick(h.ctx); close(tickDone) }()
		select {
		case <-tickDone:
			l2.Tick(h.ctx)
			continue
		case <-cpuSlow.entered:
		}
		// cpu.limit is inside Set on pod:a; memory.high ticks now, in Relieving too.
		l2.Tick(h.ctx)
		close(cpuSlow.release)
		<-tickDone
		close(done)
		break
	}
	select {
	case <-done:
	default:
		t.Fatal("cpu.limit never reached its Set")
	}
	if got := cpuSlow.sets(); len(got) != 1 || got[0] != "pod:a" {
		t.Fatalf("cpu.limit set %v; want pod:a once", got)
	}
	if got := mem.sets(); len(got) != 0 {
		t.Fatalf("memory.high set %v while cpu.limit's change to pod:a was in flight; want nothing", got)
	}
	if n := len(h.actsBy("memory.high")); n != 0 {
		t.Fatalf("memory.high journalled %d acts", n)
	}
	if n := budget.Count(h.now); n != 1 {
		t.Fatalf("budget count %d; want only cpu.limit's act", n)
	}
}

// TestDecisionsAreWordedFromTheDescriptor: what a decision asks names the lever's
// action and the pressure it relieves, so a memory lever's journal does not read
// as CPU relief. The wording changes; the numbers do not.
func TestDecisionsAreWordedFromTheDescriptor(t *testing.T) {
	budget := NewBudget(6)
	h := newHarness(t, fixture{shares: map[string]float64{"pod:a": 0.4, "pod:b": 0.3},
		policy: leverPolicy(6), budget: budget})
	l2 := h.second(budget)
	for i := 0; i < 20 && len(h.actsBy("memory.high")) == 0; i++ {
		h.both(l2, tick, 0.5)
	}
	h.both(l2, 3*time.Minute, 0.5) // neither helped: both are reverted
	byAction := map[string]int{}
	for _, d := range h.m.Journal().Decisions(0) {
		if !strings.HasPrefix(d.ID, "relief-") {
			continue
		}
		if strings.Contains(d.Question, "CPU") {
			t.Errorf("%s asks %q; want it worded from the descriptor", d.ID, d.Question)
		}
		for _, action := range []string{"cpu.limit", "memory.high"} {
			if strings.Contains(d.Question, action) {
				byAction[action]++
			}
		}
		if strings.HasPrefix(d.ID, "relief-act-") && !strings.Contains(d.Question, cpu.Relieves) {
			t.Errorf("%s asks %q; want the pressure it relieves, %s", d.ID, d.Question, cpu.Relieves)
		}
	}
	if byAction["cpu.limit"] == 0 || byAction["memory.high"] == 0 {
		t.Errorf("questions naming each action: %v; want both levers' decisions named by their action", byAction)
	}
	if n := len(h.decisions("revert")); n == 0 {
		t.Errorf("no revert decision to check the wording of")
	}
}
