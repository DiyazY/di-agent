package minimal_test

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/explain"
	"github.com/DiyazY/di-agent/pkg/statemap"
)

// TestScenario_DecisionOutlivesTheJournalAndExplainsItself narrates the durable
// log: a relief chain is evicted from a four-slot journal, the process restarts,
// and the verdict is still read back from the log and told as one sentence.
func TestScenario_DecisionOutlivesTheJournalAndExplainsItself(t *testing.T) {
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	snap := filepath.Join(dir, "state.json")
	const (
		estID    = "est-1.1-cpu_contention_ratio"
		actID    = "relief-act-cpu.limit-1.2"
		verifyID = "relief-verify-cpu.limit-1.3"
	)

	m := statemap.New(statemap.Config{Owner: "n1"}, statemap.NewJournal(4))
	m.SetClock(func() time.Time { return at })
	if err := m.OpenLog(dir); err != nil {
		t.Fatal(err)
	}
	t.Logf("a node opens its log beside the snapshot; the journal holds only 4 decisions")

	measured := 0.6
	if _, err := m.Journal().RecordDecision(&statemap.Decision{ID: estID, At: at, Kind: "estimate",
		Question: "estimate cpu_contention_ratio",
		RelationshipsRead: []statemap.Relationship{{ID: "usage@pod:a->cpu_contention_ratio", From: "usage@pod:a",
			To: "cpu_contention_ratio", Sign: 1, Provenance: statemap.Intervened, Measured: &measured}},
		Answer: map[string]any{"target": "cpu_contention_ratio", "level": 0.31, "confidence": 0.9, "status": "active",
			"sensitivity": 0.25, "sensitivity_low": 0.2, "sensitivity_high": 0.3, "contributions": 0.1}}); err != nil {
		t.Fatal(err)
	}
	commit := func(id, kind, q string, cites []string, inputs, answer map[string]any) {
		t.Helper()
		b := m.Decide(id, kind, q)
		for _, c := range cites {
			b.Cite(c)
		}
		for k, v := range inputs {
			b.Input(k, v)
		}
		if b.Commit(answer) == nil {
			t.Fatalf("decision %s not recorded", id)
		}
	}
	commit(actID, "relief.act", "cap pod:a's cpu through cpu.limit to relieve cpu_contention_ratio?", []string{estID},
		map[string]any{
			"plan": map[string]any{"subject": "pod:a", "from": 1.0, "basis": "map", "estimate_id": estID, "to": 0.5,
				"unit": "cores", "predicted": 0.08, "rule": 0.0, "effect": 0.0},
			"notice": map[string]any{"relieves": "cpu_contention_ratio", "enter": 0.25, "enter_for": "1m0s",
				"pressure": 0.31, "phase": "relieving", "since": at}},
		map[string]any{"action": "cpu.limit", "subject": "pod:a", "to": 0.5, "unit": "cores", "predicted": 0.08})
	commit(verifyID, "relief.verify", "did capping pod:a's cpu through cpu.limit relieve cpu_contention_ratio?",
		[]string{actID}, map[string]any{"act": actID, "observed": 0.05, "predicted": 0.08},
		map[string]any{"verdict": "keep", "observed": 0.05, "predicted": 0.08})
	for i := 0; i < 5; i++ {
		commit(fmt.Sprintf("est-1.%d-later", 10+i), "estimate", "later estimate", nil, nil, map[string]any{"level": 0.1})
	}
	if _, held := m.Journal().Decision(verifyID); held {
		t.Fatal("the verdict should have been evicted from a four-slot journal")
	}
	t.Logf("a relief chain (estimate, act, verdict) is recorded, then 5 estimates evict it from memory")

	if err := m.Save(snap); err != nil {
		t.Fatal(err)
	}
	if err := m.CloseLog(); err != nil {
		t.Fatal(err)
	}

	r := statemap.New(statemap.Config{Owner: "n1"}, statemap.NewJournal(4))
	r.SetClock(func() time.Time { return at })
	if _, err := r.Load(snap); err != nil {
		t.Fatal(err)
	}
	if err := r.OpenLog(dir); err != nil {
		t.Fatal(err)
	}
	defer r.CloseLog()
	st, ok := r.Journal().LogStats()
	if !ok || st.Indexed < 8 {
		t.Fatalf("LogStats = %+v, %v; want at least 8 indexed", st, ok)
	}
	t.Logf("restart: incarnation %d, log %s, %d decisions indexed, %d corrupt, %d foreign",
		r.Incarnation(), filepath.Base(st.File), st.Indexed, st.CorruptLines, st.ForeignLines)

	// resolveChain-equivalent: walk the cites from the log.
	v, found, err := r.Journal().DecisionFromLog(verifyID)
	if err != nil || !found {
		t.Fatalf("verdict not read back from the log: found=%v err=%v", found, err)
	}
	var cites []statemap.ChainLink
	for stack := append([]string(nil), v.Cites...); len(stack) > 0; {
		id := stack[0]
		stack = stack[1:]
		d, ok, err := r.Journal().DecisionFromLog(id)
		if err != nil || !ok {
			t.Fatalf("cite %s not read back from the log: found=%v err=%v", id, ok, err)
		}
		cites = append(cites, statemap.ChainLink{ID: id, Decision: d})
		stack = append(stack, d.Cites...)
	}
	text := explain.Narrate(*v, cites)
	t.Logf("the verdict explains itself from the log alone:\n%s", text)

	const want = "relief-verify-cpu.limit-1.3 [relief.verify] at 2026-01-01T12:00:00Z: capped pod:a (cpu.limit to 0.5 cores) because cpu_contention_ratio stayed above 0.25 for 1m0s (at 0.31); predicted a drop of 0.08 from the map (usage@pod:a->cpu_contention_ratio, intervened, measured 0.6); observed 0.05; kept"
	first := text
	for i := range text {
		if text[i] == '\n' {
			first = text[:i]
			break
		}
	}
	if first != want {
		t.Fatalf("first line\n%s\nwant\n%s", first, want)
	}
}
