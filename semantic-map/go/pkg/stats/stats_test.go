package stats

import (
	"math"
	"testing"
)

func TestPearsonPerfectPositive(t *testing.T) {
	xs := []float64{0, 1, 2, 3, 4}
	ys := []float64{0, 2, 4, 6, 8}
	r, ok := Pearson(xs, ys)
	if !ok || math.Abs(r-1) > 1e-12 {
		t.Fatalf("r=%.6f ok=%v; want 1, true", r, ok)
	}
}

func TestPearsonUndefinedOnConstantOrShort(t *testing.T) {
	if _, ok := Pearson([]float64{1, 1, 1}, []float64{1, 2, 3}); ok {
		t.Error("constant series must be undefined, not r=0")
	}
	if _, ok := Pearson([]float64{1, 2}, []float64{1, 2}); ok {
		t.Error("fewer than three points must be undefined")
	}
}

func TestFisherPValue(t *testing.T) {
	if p := FisherPValue(0.9, 3); p != 1.0 {
		t.Errorf("n<4 must return 1.0, got %v", p)
	}
	if p := FisherPValue(0.95, 100); p >= 0.001 {
		t.Errorf("strong correlation over 100 points must be significant, got %v", p)
	}
}

func TestPairWindowDedupsAndBounds(t *testing.T) {
	w := NewPairWindow()
	if !w.Fold("a", 1, 2, 3) || w.Fold("a", 1, 2, 3) {
		t.Fatal("first fold of an identity must succeed and the second must be rejected")
	}
	w.Fold("b", 2, 4, 3)
	w.Fold("c", 3, 6, 3)
	w.Fold("d", 4, 8, 3) // evicts the oldest; capacity 3
	if w.Len() != 3 {
		t.Fatalf("len=%d, want 3", w.Len())
	}
	r, ok := w.Pearson()
	if !ok || math.Abs(r-1) > 1e-12 {
		t.Fatalf("r=%.6f ok=%v; want 1, true", r, ok)
	}
}
