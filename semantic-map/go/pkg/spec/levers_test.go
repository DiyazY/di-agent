package spec

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestLeversDefaults: relief has two levers by default, cpu.limit with today's
// thresholds and memory.high beside it; dry_run follows the runtime; the ledger path is Task 4's rule; the
// policy allows both actions and floors each.
func TestLeversDefaults(t *testing.T) {
	d := Default()
	want := []Lever{{
		Action: "cpu.limit", Enter: 0.25, Exit: 0.10, EnterFor: time.Minute, ExitFor: 3 * time.Minute,
		Step: 0.25, MinEffect: 0.03, VictimShare: 0.05, Settle: 2 * time.Minute, Expiry: time.Hour,
	}, {
		Action: "memory.high", Enter: 0.08, Exit: 0.03, EnterFor: time.Minute, ExitFor: 3 * time.Minute,
		Step: 0.25, MinEffect: 0.01, VictimShare: 0.05, Settle: 2 * time.Minute, Expiry: time.Hour,
	}}
	if !reflect.DeepEqual(d.Loops.Relief.Levers, want) {
		t.Errorf("levers = %+v, want %+v", d.Loops.Relief.Levers, want)
	}
	if d.Loops.Relief.DryRun != nil {
		t.Errorf("dry_run = %v, want unset (follows runtime.kind)", *d.Loops.Relief.DryRun)
	}
	if d.Actuation.Ledger != "" {
		t.Errorf("actuation.ledger = %q, want empty (beside persist.file)", d.Actuation.Ledger)
	}
	if !reflect.DeepEqual(d.Policy.Defaults.Allow, []string{"cpu.limit", "memory.high"}) {
		t.Errorf("allow = %v", d.Policy.Defaults.Allow)
	}
	for action, want := range map[string][2]float64{"cpu.limit": {0.1, 0.5}, "memory.high": {67108864, 0}} {
		if lo, of := d.Policy.Defaults.Floor.Values(action); lo != want[0] || of != want[1] {
			t.Errorf("floor %s = {%v, %v}, want %v", action, lo, of, want)
		}
	}
	if lo, of := d.Policy.Defaults.Floor.Values("nothing"); lo != 0 || of != 0 {
		t.Errorf("floor of an action it does not name = {%v, %v}, want zeros", lo, of)
	}
}

// TestALeverNamesOnlyWhatItChanges: a lever in the file starts from the default
// lever for its action, so naming one threshold keeps the rest.
func TestALeverNamesOnlyWhatItChanges(t *testing.T) {
	got, err := Parse([]byte("loops: {relief: {levers: [{action: cpu.limit, enter: 0.4}]}}"))
	if err != nil {
		t.Fatal(err)
	}
	l := got.Loops.Relief.Levers
	if len(l) != 1 || l[0].Enter != 0.4 || l[0].Exit != 0.10 || l[0].Settle != 2*time.Minute {
		t.Fatalf("levers = %+v; want enter 0.4 over the cpu.limit defaults", l)
	}
}

// TestLeverDuplicateRefused: two levers with one action, or a lever with none,
// cannot both be what was meant.
func TestLeverDuplicateRefused(t *testing.T) {
	_, err := Parse([]byte("loops: {relief: {levers: [{action: cpu.limit}, {action: cpu.limit}]}}"))
	if err == nil || !strings.Contains(err.Error(), "loops.relief.levers[1] cpu.limit") || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("err = %v, want the second lever named as a duplicate", err)
	}
	_, err = Parse([]byte("loops: {relief: {levers: [{enter: 0.3, exit: 0.1, enter_for: 1m, exit_for: 1m, settle: 1m, expiry: 2m}]}}"))
	if err == nil || !strings.Contains(err.Error(), "loops.relief.levers[0].action") {
		t.Fatalf("err = %v, want the empty action named", err)
	}
	_, err = Parse([]byte("loops: {relief: {enabled: true, levers: []}}"))
	if err == nil || !strings.Contains(err.Error(), "loops.relief.levers") {
		t.Fatalf("err = %v, want relief with no lever refused", err)
	}
}

// TestAnUnknownLeverActionIsOneError: a lever whose action has no default lever
// is a misspelling or an actuator this spec knows nothing of; it is refused once,
// naming the actions there are, not as a list of thresholds it never set.
func TestAnUnknownLeverActionIsOneError(t *testing.T) {
	_, err := Parse([]byte("loops: {relief: {levers: [{action: cpu.limit}, {action: cpu.limt}]}}"))
	want := "loops.relief.levers[1] cpu.limt: unknown action (known: cpu.limit, memory.high)"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want %q", err, want)
	}
	for _, other := range []string{".enter", ".exit", ".settle", ".expiry", "policy.defaults.floor"} {
		if strings.Contains(err.Error(), "levers[1]"+other) || strings.Contains(err.Error(), other+" has no") {
			t.Errorf("err = %v; want only the unknown action, not %s", err, other)
		}
	}
}

// TestLeverChecksNameTheLever: the checks CPURelief had are made per lever, and
// say which.
func TestLeverChecksNameTheLever(t *testing.T) {
	for doc, want := range map[string]string{
		"loops: {relief: {levers: [{action: cpu.limit, exit: 0.3}]}}":                           "loops.relief.levers[0].exit",
		"loops: {relief: {levers: [{action: cpu.limit, settle: 2m, expiry: 1m}]}}":              "loops.relief.levers[0].expiry",
		"loops: {relief: {levers: [{action: cpu.limit}, {action: memory.high, enter: 1.5}]}}":   "loops.relief.levers[1].enter",
		"loops: {relief: {levers: [{action: cpu.limit}, {action: memory.high, exit_for: 0s}]}}": "loops.relief.levers[1].exit_for",
		"loops: {relief: {levers: [{action: cpu.limit, entre: 0.3}]}}":                          "entre",
		"policy: {defaults: {floor: {cpu.limit: {min: 0.1, of_original: 0.5, x: 1}}}}":          "x",
	} {
		if _, err := Parse([]byte(doc)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want one naming %s", doc, err, want)
		}
	}
}

// TestTheOldReliefKeysAreRefused: there is no shim for loops.relief.cpu or a
// floor in cpu_cores; a file still written that way is refused, not half read.
func TestTheOldReliefKeysAreRefused(t *testing.T) {
	for _, doc := range []string{
		"loops: {relief: {cpu: {enter: 0.4}}}",
		"policy: {defaults: {floor: {cpu_cores: 0.2, of_original: 0.5}}}",
		"policy: {rules: [{match: {name: a}, floor: {cpu_cores: 0.5}}]}",
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", doc)
		}
	}
}

// TestFloorPerActionOverlay: a floor is per action and, within one, per bound: a
// file or a rule naming one bound of one action keeps every other bound and action.
func TestFloorPerActionOverlay(t *testing.T) {
	got, err := Parse([]byte("policy: {defaults: {floor: {cpu.limit: {min: 0.2}}}}"))
	if err != nil {
		t.Fatal(err)
	}
	f := got.Policy.Defaults.Floor
	if lo, of := f.Values("cpu.limit"); lo != 0.2 || of != 0.5 {
		t.Errorf("cpu.limit = {%v, %v}, want {0.2, 0.5}", lo, of)
	}
	if lo, of := f.Values("memory.high"); lo != 67108864 || of != 0 {
		t.Errorf("memory.high = {%v, %v}, want the default", lo, of)
	}

	rule := Floor{"cpu.limit": {OfOriginal: ptr(0.8)}}
	over := rule.Over(Default().Policy.Defaults.Floor)
	if lo, of := over.Values("cpu.limit"); lo != 0.1 || of != 0.8 {
		t.Errorf("rule over defaults, cpu.limit = {%v, %v}, want {0.1, 0.8}", lo, of)
	}
	if lo, _ := over.Values("memory.high"); lo != 67108864 {
		t.Errorf("rule over defaults, memory.high min = %v, want the default's", lo)
	}
	if *rule["cpu.limit"].OfOriginal != 0.8 || rule["cpu.limit"].Min != nil || len(rule) != 1 {
		t.Error("Over changed its receiver")
	}
	*over["cpu.limit"].OfOriginal = 0.1
	if *Default().Policy.Defaults.Floor["cpu.limit"].OfOriginal != 0.5 || *rule["cpu.limit"].OfOriginal != 0.8 {
		t.Error("Over's result shares a bound with an input")
	}

	for name, doc := range map[string]string{
		"not an action":       "policy: {defaults: {floor: {cpu.limt: {min: 0.1, of_original: 0.5}}}}",
		"min negative":        "policy: {rules: [{match: {name: a}, floor: {cpu.limit: {min: -1}}}]}",
		"of_original above":   "policy: {rules: [{match: {name: a}, floor: {memory.high: {of_original: 2}}}]}",
		"half a new action":   "policy: {defaults: {floor: {evict: {min: 1}}}}",
		"no floor for lever":  "loops: {relief: {levers: [{action: cpu.limit}, {action: evict, enter: 0.2, exit: 0.05, enter_for: 1m, exit_for: 3m, step: 0.1, settle: 2m, expiry: 1h}]}}",
		"defaults floor null": "policy: {defaults: {floor: null}}",
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Parse([]byte("policy: {rules: [{match: {name: a}, floor: {memory.high: {of_original: 0.5}}}]}")); err != nil {
		t.Errorf("a rule naming one bound of the second action: %v", err)
	}
}

// TestDryRunFollowsTheRuntime: dry_run unset is the runtime's say; false on the
// dry-run runtime would claim changes that never reach the system.
func TestDryRunFollowsTheRuntime(t *testing.T) {
	for doc, want := range map[string]bool{
		"":                            true,
		"runtime: {kind: kubernetes}": false,
		"runtime: {kind: kubernetes}\nloops: {relief: {dry_run: true}}": true,
		"loops: {relief: {dry_run: true}}":                              true,
	} {
		s, err := Parse([]byte(doc))
		if err != nil {
			t.Fatalf("%q: %v", doc, err)
		}
		if got := s.Loops.Relief.IsDryRun(s.Runtime.Kind); got != want {
			t.Errorf("%q: dry run %v, want %v", doc, got, want)
		}
	}
	if _, err := Parse([]byte("loops: {relief: {dry_run: false}}")); err == nil || !strings.Contains(err.Error(), "dry_run") {
		t.Errorf("err = %v; want dry_run false on the dry-run runtime refused", err)
	}
}

// TestTheLedgerPathIsASetting: actuation.ledger names the ledger file.
func TestTheLedgerPathIsASetting(t *testing.T) {
	s, err := Parse([]byte("actuation: {ledger: /data/ledger.json}"))
	if err != nil || s.Actuation.Ledger != "/data/ledger.json" {
		t.Fatalf("ledger %q, err %v", s.Actuation.Ledger, err)
	}
}

func ptr(v float64) *float64 { return &v }
