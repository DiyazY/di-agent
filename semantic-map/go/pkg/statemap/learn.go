package statemap

import (
	"math"
	"time"
)

// This file gives the map its own estimator, so a relationship's strength is learned
// here rather than computed in another layer and copied in.
//
// It used to be copied in: a separate construct graph ran the estimator and the map
// recorded whatever it produced. Two representations of the same relations, kept in
// step by a propagation call — which is a standing invitation for them to disagree,
// and for a reader to have to ask which one an answer came from. The estimator is
// small; the second model was not.
//
// What is estimated, and what is not: the map learns the MAGNITUDE of an association
// the backbone already asserts, and whether the sign the backbone declares is the
// sign this system shows. It does not discover relationships and it does not infer
// causation. Direction and existence come from prior knowledge; strength comes from
// here.

// LearnConfig controls the paired estimator. Zero values take the defaults.
type LearnConfig struct {
	// PairWindowSeconds is how far apart two observations may be and still count as
	// simultaneous. Not a smoothing parameter: collectors sample on independent
	// grids, so without a tolerance no pair ever forms. Too wide and a pair relates
	// readings from different operating regimes.
	PairWindowSeconds int

	// MinSupport is the number of pairs a relationship needs before its strength
	// moves at all. Below it the pair is buffered and n_observations does not
	// advance, so confidence keeps reporting that nothing has been learned.
	MinSupport int

	// Window is the number of recent pairs the estimate is computed over. Older
	// pairs fall out, which is what lets a relationship follow a system whose
	// behaviour changes rather than averaging over its whole history.
	Window int
}

func (c LearnConfig) withDefaults() LearnConfig {
	if c.PairWindowSeconds <= 0 {
		c.PairWindowSeconds = 15
	}
	if c.MinSupport < 3 {
		c.MinSupport = 8
	}
	if c.Window < c.MinSupport {
		c.Window = 60
	}
	return c
}

// pairWindow is a fixed-capacity ring of paired observations for one relationship.
type pairWindow struct {
	xs, ys []float64
	next   int
	seen   map[string]struct{} // pair identities already folded in
}

func newPairWindow() *pairWindow {
	return &pairWindow{seen: make(map[string]struct{})}
}

func (w *pairWindow) add(x, y float64, capacity int) {
	if len(w.xs) < capacity {
		w.xs = append(w.xs, x)
		w.ys = append(w.ys, y)
		return
	}
	w.xs[w.next] = x
	w.ys[w.next] = y
	w.next = (w.next + 1) % capacity
}

// pearson returns the correlation over the window, and false when it is undefined:
// fewer than three points, or a constant series on either side. A constant series is
// the common case on a quiet system, where a property sits at zero for long
// stretches. Reporting r = 0 there would be a claim; "undefined" is the honest
// answer, and it leaves the strength where it was.
func (w *pairWindow) pearson() (float64, bool) {
	n := len(w.xs)
	if n < 3 {
		return 0, false
	}
	var sx, sy float64
	for i := 0; i < n; i++ {
		sx += w.xs[i]
		sy += w.ys[i]
	}
	mx, my := sx/float64(n), sy/float64(n)
	var sxy, sxx, syy float64
	for i := 0; i < n; i++ {
		dx, dy := w.xs[i]-mx, w.ys[i]-my
		sxy += dx * dy
		sxx += dx * dx
		syy += dy * dy
	}
	if sxx <= 1e-12 || syy <= 1e-12 {
		return 0, false
	}
	return sxy / math.Sqrt(sxx*syy), true
}

// observation is the last value seen for one property, used to form pairs.
type observation struct {
	value   float64
	at      time.Time
	eventID string
}

// learnFromObservationLocked forms pairs for every relationship incident to the
// property just observed, and folds each into that relationship's estimate.
// Caller holds the write lock.
//
// The pairing uses the freshest counterpart rather than a history, so a slow
// counterpart cannot pair with every fast arrival and inflate support by repeating
// one point.
func (m *Map) learnFromObservationLocked(propertyID string, obs observation) {
	if !m.learning {
		return
	}
	for _, r := range m.relationships {
		if r.Status == Retired {
			continue
		}
		var otherID string
		switch {
		case r.From == propertyID:
			otherID = r.To
		case r.To == propertyID:
			otherID = r.From
		default:
			continue
		}

		other, ok := m.latest[otherID]
		if !ok {
			continue
		}
		gap := obs.at.Sub(other.at)
		if gap < 0 {
			gap = -gap
		}
		if gap > time.Duration(m.learn.PairWindowSeconds)*time.Second {
			continue
		}
		// A retired or stale endpoint is not evidence about the present.
		if p, ok := m.properties[otherID]; !ok || p.Status != Active {
			continue
		}

		// Order the pair the way the relationship reads: source first. The
		// correlation's magnitude is symmetric but the sign check is not meaningful
		// unless the caller knows which way round the values went.
		x, y := obs.value, other.value
		if r.To == propertyID {
			x, y = other.value, obs.value
		}
		m.foldPairLocked(r, x, y, pairIdentity(obs.eventID, other.eventID), obs.at)
	}
}

// foldPairLocked adds one pair to a relationship's window and updates its strength
// once support is reached. Caller holds the write lock.
func (m *Map) foldPairLocked(r *Relationship, x, y float64, identity string, at time.Time) {
	w, ok := m.windows[r.ID]
	if !ok {
		w = newPairWindow()
		m.windows[r.ID] = w
	}
	// Idempotency: the same physical pair must not be folded twice, or replaying a
	// batch of telemetry would inflate the estimate by re-adding its own points.
	if _, dup := w.seen[identity]; dup {
		return
	}
	w.seen[identity] = struct{}{}
	// Bound the dedup set with the window: an identity that can no longer be in the
	// window cannot be a duplicate of anything still in it.
	if len(w.seen) > 4*m.learn.Window {
		w.seen = map[string]struct{}{identity: {}}
	}

	w.add(x, y, m.learn.Window)
	if len(w.xs) < m.learn.MinSupport {
		return
	}
	rr, ok := w.pearson()
	if !ok {
		return
	}

	strength := math.Abs(rr)
	if signAgrees(rr, r.Sign) {
		r.SignAgreements++
	} else {
		// The system shows the opposite sign to the one this relationship asserts.
		// That is evidence against THIS relationship, not a weaker version of it —
		// which is what lets two relationships over the same pair with opposite signs
		// separate instead of moving together.
		//
		// Counted as well as applied. Zeroing the strength is the right arithmetic and
		// a poor report: an estimate driven to zero by a gate that never opens looks
		// exactly like one that was never observed, and the difference is whether the
		// declaration is wrong. SignSuspect reads these two counters.
		r.SignConflicts++
		strength = 0
	}
	r.SignSuspectFlag = r.SignSuspect()

	if r.NObservations == 0 {
		r.Strength = strength
	} else {
		r.Strength = m.cfg.Alpha*strength + (1-m.cfg.Alpha)*r.Strength
	}
	r.NObservations++
	foldEstablished(r, strength, m.cfg.AlphaSlow)
	r.LastObserved = at
	if r.FirstObserved.IsZero() {
		r.FirstObserved = at
	}
	r.Confidence = clamp01(float64(r.NObservations) / float64(m.cfg.ConvergenceObservations))
	if r.Provenance == Seeded {
		r.Provenance = Learned
	}
	m.revision++
}

// pairIdentity names a paired observation from its two contributing event IDs, in a
// fixed order so the same physical pair gets one identity whichever arrived first.
func pairIdentity(a, b string) string {
	if b < a {
		a, b = b, a
	}
	return a + "+" + b
}

func signAgrees(r float64, sign int) bool {
	if sign < 0 {
		return r < 0
	}
	return r > 0
}

// PairSupport reports how many pairs a relationship's window holds. An edge sitting
// at zero confidence is usually short of support rather than broken, and this is how
// a caller tells the difference.
func (m *Map) PairSupport(relationshipID string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if w, ok := m.windows[relationshipID]; ok {
		return len(w.xs)
	}
	return 0
}

// foldEstablished folds one strength reading into the relationship's long-run layer:
// the same input the recent layer smooths, on a much slower clock.
//
// The estimate is bias-corrected — divided by 1 − (1−α)^n — and the correction is the
// substance rather than a refinement. A slow EMA started from its first reading spends
// several time constants dominated by where it began, and measuring that showed the
// consequence plainly: across five replays of one workload on one machine the raw slow
// layer's end value varied by σ ≈ 0.32, against 0.025 for the fast layer, because with
// ~89 pairs per run and a memory of 1000 it never left its transient. A layer whose
// value depends mostly on its own initialisation is not a baseline. Correcting the bias
// makes the estimate the unbiased long-run mean from the first fold, so it is
// meaningful immediately, continuous throughout, and free of any dependence on where it
// started.
//
// Storing only the corrected value is enough: the raw accumulator is recoverable from
// it and n, so the layer costs one field rather than two.
func foldEstablished(r *Relationship, strength, alpha float64) {
	n := r.NObservations // already incremented: the count of folds into this layer
	if n <= 0 {
		return
	}
	var raw float64
	if r.Established != nil && n > 1 {
		raw = *r.Established * biasCorrection(alpha, n-1)
	}
	raw = alpha*strength + (1-alpha)*raw
	corrected := clamp01(raw / biasCorrection(alpha, n))
	r.Established = &corrected
}

// biasCorrection is the weight an n-fold EMA has actually accumulated, 1 − (1−α)^n.
// It rises from α at the first fold toward 1, and dividing by it removes the pull of
// the zero the accumulator started from.
func biasCorrection(alpha float64, n int) float64 {
	c := 1 - math.Pow(1-alpha, float64(n))
	if c < 1e-12 {
		return 1e-12
	}
	return c
}
