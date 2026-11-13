package statemap

import (
	"math"
	"time"

	"github.com/DiyazY/di-agent/pkg/stats"
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
		w = stats.NewPairWindow(m.learn.Window)
		m.windows[r.ID] = w
	}
	// Idempotency: the same physical pair must not be folded twice, or replaying a
	// batch of telemetry would inflate the estimate by re-adding its own points.
	if !w.Fold(identity, x, y) {
		return
	}
	if w.Len() < m.learn.MinSupport {
		return
	}
	rr, ok := w.Pearson()
	if !ok {
		return
	}

	strength := math.Abs(rr)
	wasEligible := m.signRetireEligible(r)
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
	if !wasEligible && m.signRetireEligible(r) && r.Provenance != Asserted {
		// On the transition only: a relationship an operator re-declares keeps its
		// counters and its flag, and is not retired again until the predicate drops
		// and rises once more. An asserted one is the operator's claim to overrule; it
		// stays flagged.
		m.retireSignContradictedLocked(r, at)
	}

	if r.NObservations == 0 {
		r.Recent = strength
	} else {
		r.Recent = m.cfg.Alpha*strength + (1-m.cfg.Alpha)*r.Recent
	}
	r.StrengthSE = strengthSE(r.Recent, w.Len())
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
	m.updateRegimeLocked(r, w.Len(), at)
	m.revision++
}

// fisherSE is one standard error of Fisher's z over n pairs, 1/sqrt(n−3) — the same
// quantity pkg/types/types.go Outdated compares against (2/sqrt(n−3) there, two SEs).
func fisherSE(n int) float64 {
	if n < 4 {
		return math.Inf(1)
	}
	return 1 / math.Sqrt(float64(n-3))
}

// strengthSE maps one z-SE above r back to the r scale:
// max(0, tanh(atanh(min(r, 0.9999)) + fisherSE(n)) − r), never negative at r → 1.
// Under 4 pairs it is 1, the whole range: no interval can be stated.
func strengthSE(r float64, n int) float64 {
	if n < 4 {
		return 1
	}
	return max(0, math.Tanh(math.Atanh(math.Min(r, 0.9999))+fisherSE(n))-r)
}

// regimeBand is two standard errors of Fisher's z, the bar Outdated uses, at the
// window's effective size rather than its count: 2 / sqrt(max(n_eff − 3, 1)).
//
// The paired values are EMA-smoothed with weight alpha, an AR(1) with φ = 1 − alpha,
// so n pairs carry n_eff = n·(1 − φ²)/(1 + φ²) independent ones. The independent-pair
// band is far too narrow for them: on a stationary stream at the defaults it put the
// relationship in and out of regime dozens of times per thousand pairs. At alpha 1
// nothing is smoothed and n_eff = n.
func regimeBand(nWindow int, alpha float64) float64 {
	phi2 := (1 - alpha) * (1 - alpha)
	nEff := float64(nWindow) * (1 - phi2) / (1 + phi2)
	return 2 / math.Sqrt(math.Max(nEff-3, 1))
}

// updateRegimeLocked moves a relationship into or out of a regime: the recent layer
// outside the established one's band, in Fisher's z, for a full window of folds
// running — consecutive folds share all but one pair, so a shorter run is one draw
// seen several times. Caller holds the write lock.
func (m *Map) updateRegimeLocked(r *Relationship, nWindow int, at time.Time) {
	if r.Status == Retired { // retired on this fold: it has no present to differ
		return
	}
	if r.Established == nil { // a fold always seeds Established first; this guards direct callers only
		r.regimeRun = 0
		return
	}
	z := func(v float64) float64 { return math.Atanh(math.Min(math.Abs(v), 0.9999)) }
	band := regimeBand(nWindow, m.cfg.Alpha)
	outside := math.Abs(z(r.Recent)-z(*r.Established)) > band
	if outside == r.Regime {
		r.regimeRun = 0
		return
	}
	r.regimeRun++
	if r.regimeRun < nWindow {
		return
	}
	r.regimeRun = 0
	r.Regime = outside
	r.RegimeSince = time.Time{}
	if outside {
		r.RegimeSince = at
	}
	m.bump(EventRelationshipRegime, r.ID, "agent", map[string]any{
		"recent": r.Recent, "established": *r.Established, "band": band, "started": outside,
	}, at)
}

// signRetireWindows is how many windows of pairs the sign counters must cover before
// the agent retires a sign-suspect relationship. Folds share all but one pair of a
// window of smoothed values, so the first folds rest on one or two effective draws: a
// conflict share over them is noise, and retirement is permanent. Three windows is
// enough independent evidence for the share to mean what it says.
const signRetireWindows = 3

// signRetireEligible is the retirement predicate: sign-suspect, over at least
// signRetireWindows windows of pairs. SignSuspect alone stays the advisory flag.
func (m *Map) signRetireEligible(r *Relationship) bool {
	return r.SignSuspect() && r.SignAgreements+r.SignConflicts >= signRetireWindows*m.learn.Window
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
		return w.Len()
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
