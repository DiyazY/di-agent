package minimal_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/agentmain"
	"github.com/DiyazY/di-agent/pkg/statemap"
)

// TestScenario_Pace narrates the attention pace: ten calm minutes slow the agent,
// relief activity or a newly admitted property returns it to base at once, and every
// decision records the pace it was made at.
func TestScenario_Pace(t *testing.T) {
	t0 := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	now := t0
	clock := func() time.Time { return now }

	m := statemap.New(statemap.Config{AdmitUnknown: true}, statemap.NewJournal(0))
	m.SetClock(clock)

	phase := "calm"
	p := agentmain.NewPace(m, agentmain.PaceInputs{
		LeverPhases: func() []string { return []string{phase} },
	}, clock, func(string, ...any) {})

	elapsed := func() time.Duration { return now.Sub(t0) }
	say := func(note string) {
		pace, _ := p.Current()
		t.Logf("T+%-4s pace=%-4s  %s", elapsed(), pace, note)
	}
	tickTo := func(target time.Duration) {
		for elapsed() < target {
			now = now.Add(time.Minute)
			p.Tick()
		}
	}
	wantPace := func(want string) {
		t.Helper()
		if got, _ := p.Current(); got != want {
			t.Fatalf("T+%s: pace = %s, want %s", elapsed(), got, want)
		}
	}
	lastChange := func() string {
		evs := m.Journal().Events(0, 0)
		for i := len(evs) - 1; i >= 0; i-- {
			if evs[i].Kind == statemap.EventPaceChanged {
				return fmt.Sprintf("%v → %v: %v", evs[i].Detail["from"], evs[i].Detail["to"], evs[i].Detail["reason"])
			}
		}
		return ""
	}

	say("a new map starts at base; one lever, calm")
	wantPace("base")

	for min := 1; min <= 9; min++ {
		tickTo(time.Duration(min) * time.Minute)
		wantPace("base")
	}
	say("nine calm minutes: still base")
	tickTo(10 * time.Minute)
	wantPace("slow")
	say("ten calm minutes: " + lastChange())

	tickTo(12 * time.Minute)
	phase = "relieving"
	p.Tick()
	wantPace("base")
	say("lever starts relieving: " + lastChange())

	tickTo(14 * time.Minute)
	phase = "calm"
	p.Tick()
	wantPace("base")
	say("lever calm again; the count restarts")
	tickTo(23 * time.Minute)
	wantPace("base")
	tickTo(24 * time.Minute)
	wantPace("slow")
	say("ten calm minutes again: " + lastChange())

	tickTo(25 * time.Minute)
	wantPace("slow")
	if err := m.Record(statemap.Observation{ID: "scenario.unknown", Value: 1, At: now}); err != nil {
		t.Fatal(err)
	}
	wantPace("base")
	say("an unknown property is observed: " + lastChange())
	d := m.Decide("pace-scn-1", "estimate", "what is the node doing?").Commit(map[string]any{})
	if d.Inputs["pace"] != "base" {
		t.Fatalf("decision at base recorded pace %v", d.Inputs["pace"])
	}
	say("a decision made now records pace=base")

	tickTo(34 * time.Minute)
	wantPace("base")
	tickTo(35 * time.Minute)
	wantPace("slow")
	say("calm for ten minutes after the admission: " + lastChange())
	d = m.Decide("pace-scn-2", "estimate", "what is the node doing?").Commit(map[string]any{})
	if d.Inputs["pace"] != "slow" {
		t.Fatalf("decision at slow recorded pace %v", d.Inputs["pace"])
	}
	say("a decision made now records pace=slow")

	var got []string
	t.Log("pace.changed events:")
	for _, e := range m.Journal().Events(0, 0) {
		if e.Kind != statemap.EventPaceChanged {
			continue
		}
		got = append(got, fmt.Sprint(e.Detail["to"]))
		t.Logf("  %s  %v → %v  (%v)", e.At.Format("15:04"), e.Detail["from"], e.Detail["to"], e.Detail["reason"])
	}
	want := []string{"slow", "base", "slow", "base", "slow"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("pace changes = %v, want %v", got, want)
	}
}
