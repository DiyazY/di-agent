package relief

import (
	"strings"
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

func TestBudgetTakeIsAtomic(t *testing.T) {
	b := NewBudget(1)
	if !b.take(t0, 1) || b.take(t0, 1) {
		t.Fatal("take let two acts through a limit of one")
	}
	b.refund(t0)
	if !b.take(t0, 1) {
		t.Fatal("a refunded act still counts")
	}
	// A rule's own limit, above the default, is what take is asked against.
	if !b.take(t0, 3) {
		t.Fatal("take ignored the limit it was given")
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
	act := actuators.Static(memory, nil, func(runtime.Workload) (float64, bool) { return 0, false })
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
