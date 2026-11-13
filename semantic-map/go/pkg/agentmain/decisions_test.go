package agentmain

import (
	"slices"
	"testing"

	"github.com/DiyazY/di-agent/pkg/statemap"
)

// A chain whose every link the bounded journal has evicted is still walkable:
// each hop resolves through the log, and an id neither holds is marked evicted.
func TestResolveChainWalksThroughTheLog(t *testing.T) {
	m := statemap.New(statemap.Config{Owner: "n1"}, statemap.NewJournal(3))
	if err := m.OpenLog(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.CloseLog() })
	act := m.NewDecisionID("act")
	m.Decide(act, "relief.act", "q").Commit(nil)
	verify := m.NewDecisionID("verify")
	b := m.Decide(verify, "relief.verify", "q")
	b.Cite(act)
	b.Commit(nil)
	revert := m.NewDecisionID("revert")
	b = m.Decide(revert, "relief.revert", "q")
	b.Cite(verify)
	b.Cite(act)
	b.Cite("ghost")
	b.Commit(nil)
	for i := 0; i < 3; i++ {
		m.Decide(m.NewDecisionID("est"), "estimate", "q").Commit(nil)
	}
	j := m.Journal()
	if _, held := j.Decision(revert); held {
		t.Fatal("revert should be evicted")
	}

	c, from, ok := resolveChain(j, revert)
	if !ok || from != "log" {
		t.Fatalf("resolveChain = ok %v, from %q", ok, from)
	}
	if c.Decision == nil || c.Decision.ID != revert {
		t.Fatalf("chain decision = %+v", c.Decision)
	}
	var got []string
	for _, l := range c.Cites {
		got = append(got, l.ID)
	}
	if want := []string{verify, act, "ghost"}; !slices.Equal(got, want) {
		t.Fatalf("cites = %v, want %v", got, want)
	}
	for _, l := range c.Cites[:2] {
		if l.Evicted || l.Decision == nil || l.Decision.ID != l.ID {
			t.Fatalf("link %+v should carry its decision from the log", l)
		}
	}
	if g := c.Cites[2]; !g.Evicted || g.Decision != nil {
		t.Fatalf("ghost link = %+v, want evicted", g)
	}
	if c.CitedBy != nil {
		t.Fatalf("cited_by = %v, want nil from the log", c.CitedBy)
	}

	// A held decision resolves from memory, as Journal.Chain does.
	held := j.Decisions(1, "")[0].ID
	if _, from, ok := resolveChain(j, held); !ok || from != "journal" {
		t.Fatalf("held chain from %q, ok %v", from, ok)
	}
	if _, _, ok := resolveChain(j, "never"); ok {
		t.Fatal("an id neither holds resolved")
	}
}

// A root still held keeps its memory chain; an ancestor the journal evicted is
// filled from the log.
func TestResolveChainFillsAnEvictedAncestorFromTheLog(t *testing.T) {
	m := statemap.New(statemap.Config{Owner: "n1"}, statemap.NewJournal(3))
	if err := m.OpenLog(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.CloseLog() })
	act := m.NewDecisionID("act")
	m.Decide(act, "relief.act", "q").Commit(nil)
	verify := m.NewDecisionID("verify")
	b := m.Decide(verify, "relief.verify", "q")
	b.Cite(act)
	b.Cite("ghost")
	b.Commit(nil)
	for i := 0; i < 2; i++ {
		m.Decide(m.NewDecisionID("est"), "estimate", "q").Commit(nil)
	}
	j := m.Journal()
	if _, held := j.Decision(act); held {
		t.Fatal("act should be evicted")
	}
	c, from, ok := resolveChain(j, verify)
	if !ok || from != "journal" || len(c.Cites) != 2 {
		t.Fatalf("resolveChain = %+v, from %q, ok %v", c, from, ok)
	}
	if l := c.Cites[0]; l.ID != act || l.Evicted || l.Decision == nil || l.Decision.ID != act {
		t.Fatalf("act link = %+v, want held from the log", l)
	}
	if l := c.Cites[1]; l.ID != "ghost" || !l.Evicted {
		t.Fatalf("ghost link = %+v, want evicted", l)
	}
}

// A held root's evicted ancestors are walked through the log hop by hop: an act
// filled from the log brings the estimate it cites, also from the log.
func TestResolveChainWalksAnEvictedAncestorsOwnCites(t *testing.T) {
	m := statemap.New(statemap.Config{Owner: "n1"}, statemap.NewJournal(3))
	if err := m.OpenLog(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.CloseLog() })
	est := m.NewDecisionID("est")
	m.Decide(est, "estimate", "q").Commit(nil)
	act := m.NewDecisionID("act")
	b := m.Decide(act, "relief.act", "q")
	b.Cite(est)
	b.Commit(nil)
	verify := m.NewDecisionID("verify")
	b = m.Decide(verify, "relief.verify", "q")
	b.Cite(act)
	b.Commit(nil)
	for i := 0; i < 2; i++ {
		m.Decide(m.NewDecisionID("est"), "estimate", "q").Commit(nil)
	}
	j := m.Journal()
	if _, held := j.Decision(act); held {
		t.Fatal("act should be evicted")
	}
	if _, held := j.Decision(verify); !held {
		t.Fatal("verify should be held")
	}
	c, from, ok := resolveChain(j, verify)
	if !ok || from != "journal" || c.CitedBy == nil {
		t.Fatalf("resolveChain = %+v, from %q, ok %v; want a held root with cited_by", c, from, ok)
	}
	var got []string
	for _, l := range c.Cites {
		if l.Evicted || l.Decision == nil || l.Decision.ID != l.ID {
			t.Fatalf("link %+v should carry its decision from the log", l)
		}
		got = append(got, l.ID)
	}
	if want := []string{act, est}; !slices.Equal(got, want) {
		t.Fatalf("cites = %v, want %v", got, want)
	}
}
