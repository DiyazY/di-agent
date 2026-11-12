package minimal_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/statemap"
)

// TestScenario_TraceSurvivesRestart: an agent answers a question, saves, and
// restarts; the restarted agent answers another resting on the first. The two
// answers carry different incarnations, the journal's numbering runs on across
// the restart instead of starting again, and the second answer's chain still
// reaches the first.
func TestScenario_TraceSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	at := time.Now()

	m1 := statemap.New(statemap.Config{Owner: "n1"}, statemap.NewJournal(0))
	if err := m1.DeclareProperty(statemap.Property{ID: "cpu_utilization", Range: [2]float64{0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := m1.Observe("cpu_utilization", 0.4, at); err != nil {
		t.Fatal(err)
	}
	first := m1.Estimate(statemap.EstimateRequest{Target: "cpu_utilization"})
	if first.Err != "" {
		t.Fatal(first.Err)
	}
	t.Logf("incarnation %d answered %s", m1.Incarnation(), first.DecisionID)
	if err := m1.Save(path); err != nil {
		t.Fatal(err)
	}
	before := m1.Journal().Events(0, 0)

	m2 := statemap.New(statemap.Config{Owner: "n1"}, statemap.NewJournal(0))
	if _, err := m2.Load(path); err != nil {
		t.Fatal(err)
	}
	id := m2.NewDecisionID("est", "cpu_utilization")
	b := m2.Decide(id, "estimate", "is cpu_utilization still where the last incarnation left it?")
	b.Cite(first.DecisionID)
	b.Property("cpu_utilization")
	second := b.Commit(map[string]any{"same": true})
	t.Logf("incarnation %d answered %s, citing %s", m2.Incarnation(), second.ID, first.DecisionID)

	if !strings.HasPrefix(first.DecisionID, "est-1.") || !strings.HasPrefix(second.ID, "est-2.") {
		t.Fatalf("ids %s, %s; want incarnations 1 and 2", first.DecisionID, second.ID)
	}
	var seqs []uint64
	for _, e := range m2.Journal().Events(0, 0) {
		seqs = append(seqs, e.Seq)
	}
	t.Logf("journal seqs %v (%d before the restart)", seqs, len(before))
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("seqs %v do not strictly increase across the restart", seqs)
		}
	}
	if last := before[len(before)-1].Seq; seqs[len(seqs)-1] <= last {
		t.Fatalf("restarted journal ends at seq %d, not past %d", seqs[len(seqs)-1], last)
	}
	c, ok := m2.Journal().Chain(second.ID)
	if !ok || len(c.Cites) != 1 || c.Cites[0].ID != first.DecisionID || c.Cites[0].Decision == nil {
		t.Fatalf("chain %+v (found %v); want it to reach %s across the restart", c, ok, first.DecisionID)
	}
}
