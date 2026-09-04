// Package stats holds the one Pearson estimator, the one Fisher p-value and the one
// paired-observation window the map and the proposer share. Two copies of an
// estimator are two chances for the same pair of readings to be scored differently.
package stats

import "math"

// PairWindow is a fixed-capacity ring of paired observations with a dedup set keyed
// by pair identity, so a replayed batch cannot fold the same physical pair twice.
type PairWindow struct {
	xs, ys []float64
	next   int
	seen   map[string]struct{}
}

func NewPairWindow() *PairWindow { return &PairWindow{seen: make(map[string]struct{})} }

// Fold adds (x, y) under identity unless that identity was already folded. capacity
// bounds the ring; the dedup set is trimmed when it exceeds 4*capacity, because an
// identity that can no longer be in the window cannot be a duplicate of anything in
// it. Returns false when the pair was a duplicate.
func (w *PairWindow) Fold(identity string, x, y float64, capacity int) bool {
	if _, dup := w.seen[identity]; dup {
		return false
	}
	w.seen[identity] = struct{}{}
	if capacity > 0 && len(w.seen) > 4*capacity {
		w.seen = map[string]struct{}{identity: {}}
	}
	if capacity <= 0 || len(w.xs) < capacity {
		w.xs = append(w.xs, x)
		w.ys = append(w.ys, y)
		return true
	}
	w.xs[w.next] = x
	w.ys[w.next] = y
	w.next = (w.next + 1) % capacity
	return true
}

func (w *PairWindow) Len() int { return len(w.xs) }

// Pearson returns the correlation over the window, and false when it is undefined.
func (w *PairWindow) Pearson() (float64, bool) { return Pearson(w.xs, w.ys) }

// Pearson returns the correlation coefficient, and false when it is undefined: fewer
// than three points, mismatched lengths, or a constant series on either side. A
// constant series is the common case on a quiet system; reporting r = 0 there would
// be a claim, and "undefined" is the honest answer.
func Pearson(xs, ys []float64) (float64, bool) {
	n := len(xs)
	if n < 3 || n != len(ys) {
		return 0, false
	}
	var sx, sy float64
	for i := 0; i < n; i++ {
		sx += xs[i]
		sy += ys[i]
	}
	mx, my := sx/float64(n), sy/float64(n)
	var sxy, sxx, syy float64
	for i := 0; i < n; i++ {
		dx, dy := xs[i]-mx, ys[i]-my
		sxy += dx * dy
		sxx += dx * dx
		syy += dy * dy
	}
	if sxx <= 1e-12 || syy <= 1e-12 {
		return 0, false
	}
	return sxy / math.Sqrt(sxx*syy), true
}

// FisherPValue is the two-tailed p-value for Pearson r using the Fisher z-transform:
// z = atanh(r)·sqrt(n−3), P = erfc(|z|/sqrt 2). Returns 1.0 when n < 4.
func FisherPValue(r float64, n int) float64 {
	if n < 4 {
		return 1.0
	}
	if r >= 1 {
		r = 1 - 1e-12
	}
	if r <= -1 {
		r = -1 + 1e-12
	}
	z := math.Atanh(r) * math.Sqrt(float64(n-3))
	return math.Erfc(math.Abs(z) / math.Sqrt2)
}
