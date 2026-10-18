package statemap

import "testing"

// Reading the same machine twice is not a change; reading it with more memory is,
// and the journal says from what to what.
func TestFactsJournalChangesOnly(t *testing.T) {
	m := New(Config{}, nil)
	read := func(mem float64) {
		if err := m.RecordFacts("host", []Fact{NumberFact("memory_total", mem, "bytes"), {Key: "os", Value: "Ubuntu"}}); err != nil {
			t.Fatal(err)
		}
	}
	read(8e9)
	rev := m.Revision()
	read(8e9)
	if m.Revision() != rev {
		t.Fatalf("an unchanged reading advanced the revision %d -> %d", rev, m.Revision())
	}
	read(16e9)
	var kinds []EventKind
	for _, e := range m.Journal().Events(0, 0) {
		kinds = append(kinds, e.Kind)
	}
	if len(kinds) != 3 || kinds[2] != EventFactChanged {
		t.Fatalf("journal = %v, want two recorded and one changed", kinds)
	}
	if f, _ := m.Fact("memory_total"); *f.Number != 16e9 || f.Source != "host" {
		t.Fatalf("memory_total = %+v", f)
	}
	if v := m.State(Query{}); len(v.Facts) != 2 || v.Facts[0].Key != "memory_total" {
		t.Fatalf("view facts = %+v", v.Facts)
	}
}

// A decision that read the facts keeps its own copy.
func TestDecisionKeepsTheFactsItRead(t *testing.T) {
	m := New(Config{}, nil)
	_ = m.RecordFacts("host", []Fact{NumberFact("cpu_cores", 4, "cores")})
	b := m.Decide("d1", "q")
	b.Facts()
	d := b.Commit(nil)
	_ = m.RecordFacts("host", []Fact{NumberFact("cpu_cores", 8, "cores")})
	if len(d.FactsRead) != 1 || *d.FactsRead[0].Number != 4 {
		t.Fatalf("decision facts = %+v", d.FactsRead)
	}
}
