package statemap

import (
	"math"
	"testing"
)

func TestFisherSEMatchesOutdatedsArithmetic(t *testing.T) {
	if got := fisherSE(120); math.Abs(got-0.09245) > 1e-4 {
		t.Errorf("fisherSE(120) = %.5f, want 0.09245", got)
	}
	if got := fisherSE(4); got != 1 {
		t.Errorf("fisherSE(4) = %v, want 1", got)
	}
	if !math.IsInf(fisherSE(3), 1) {
		t.Errorf("fisherSE(3) = %v, want +Inf", fisherSE(3))
	}
}

func TestStrengthSEIsTheWholeRangeUnderFourPairs(t *testing.T) {
	if strengthSE(0.5, 3) != 1 || strengthSE(0.5, 2) != 1 {
		t.Errorf("under 4 pairs strengthSE must be 1, got %v and %v", strengthSE(0.5, 3), strengthSE(0.5, 2))
	}
	if got := strengthSE(0.5, 120); math.Abs(got-0.0660) > 1e-3 {
		t.Errorf("strengthSE(0.5, 120) = %.4f, want 0.0660", got)
	}
	if got := strengthSE(1, 120); math.IsNaN(got) || math.IsInf(got, 0) || got > 1 {
		t.Errorf("strengthSE(1, 120) = %v, want finite and <= 1", got)
	}
}

func TestFoldSetsStrengthSEFromTheWindow(t *testing.T) {
	m, c := newTestMap(t, Config{
		ConvergenceObservations: 500, Alpha: 0.2, AlphaSlow: 0.001, Learn: true,
		LearnConfig: LearnConfig{PairWindowSeconds: 15, MinSupport: 3, Window: 60},
	})
	for _, id := range []string{"resource", "pressure"} {
		if err := m.DeclareProperty(Property{ID: id, Range: [2]float64{0, 1}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.DeclareRelationship(Relationship{From: "resource", To: "pressure", Label: "raises", Sign: 1}); err != nil {
		t.Fatal(err)
	}
	id := RelationshipID("resource", "pressure", "raises")
	// Observe until the first fold: the window then holds exactly MinSupport pairs.
	k := 0
	for r, _ := m.Relationship(id); r.NObservations == 0; r, _ = m.Relationship(id) {
		observeHalf(t, m, c, k, same, "a")
		k++
	}
	r, _ := m.Relationship(id)
	if r.StrengthSE != 1 || m.windows[id].Len() != 3 {
		t.Fatalf("first fold: window %d, StrengthSE %v, want 3 pairs and 1", m.windows[id].Len(), r.StrengthSE)
	}
	for ; m.windows[id].Len() < 10; k++ {
		observeHalf(t, m, c, k, same, "a")
	}
	r, _ = m.Relationship(id)
	if want := strengthSE(r.Recent, m.windows[id].Len()); math.Abs(r.StrengthSE-want) > 1e-9 || r.StrengthSE >= 1 {
		t.Errorf("at %d pairs StrengthSE %v, want %v below 1", m.windows[id].Len(), r.StrengthSE, want)
	}
}
