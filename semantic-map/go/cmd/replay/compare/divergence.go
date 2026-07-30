package compare

import (
	"math"
	"sort"

	"github.com/DiyazY/di-agent/pkg/types"
)

// EdgeDivergence summarizes how one proposition's effective weight varies
// across KDs after replay. Effective[i] = (1 - confidence[i]) * prior +
// confidence[i] * ema for KD i — that is, the value the Reasoner would
// actually use.
//
// Range is max(Effective) - min(Effective). StdDev is the sample standard
// deviation (n-1 denominator) so the ordering matches what a research
// notebook would compute. Both metrics ignore KDs whose snapshot lacked
// the proposition (defensive — every KD shares the Di-Select ontology in
// v1, so this should never trigger).
type EdgeDivergence struct {
	PropositionID string
	From          string
	To            string
	Direction     string // "+" / "-"
	PriorWeight   float64

	// Per-KD slices; index-aligned with Result.PerKD.
	Effective     []float64
	EMA           []float64
	Confidence    []float64
	NObservations []int

	Range  float64
	StdDev float64
}

// Compute returns one EdgeDivergence per unique PropositionID seen across
// every PerKDResult's edge set, sorted by Range descending (most
// discriminative first). Within equal-range entries the order is by
// PropositionID for determinism.
//
// Per-KD priors are taken from the first KD that carries the proposition
// — they are guaranteed identical across KDs in v1 because the global
// prior_strength is what seeds the EdgeDescriptor when no per-KD edge
// override fires; the per-KD weight goes into prior_weight directly.
// Compute therefore prints the prior_weight from the per-KD edge of the
// first listed KD, matching what the table column "Prior" reflects.
func Compute(perKD []*PerKDResult) []*EdgeDivergence {
	if len(perKD) == 0 {
		return nil
	}

	// Union of proposition IDs across every KD's snapshot.
	type slot struct {
		from, to string
		dir      types.Direction
		prior    float64
		eff      []float64
		ema      []float64
		conf     []float64
		nobs     []int
		hasPrior bool
	}
	byProp := map[string]*slot{}

	for i, kd := range perKD {
		index := map[string]*types.EdgeDescriptor{}
		for _, e := range kd.Edges {
			index[e.PropositionID] = e
		}
		for propID, e := range index {
			s, ok := byProp[propID]
			if !ok {
				s = &slot{
					from:     e.FromID,
					to:       e.ToID,
					dir:      e.Direction,
					prior:    e.PriorWeight,
					eff:      make([]float64, len(perKD)),
					ema:      make([]float64, len(perKD)),
					conf:     make([]float64, len(perKD)),
					nobs:     make([]int, len(perKD)),
					hasPrior: true,
				}
				byProp[propID] = s
			}
			// Effective = (1-c)*prior + c*ema — what the Reasoner uses.
			eff := (1.0-e.Confidence)*e.PriorWeight + e.Confidence*e.EMAWeight
			s.eff[i] = eff
			s.ema[i] = e.EMAWeight
			s.conf[i] = e.Confidence
			s.nobs[i] = e.NObservations
		}
	}

	out := make([]*EdgeDivergence, 0, len(byProp))
	for propID, s := range byProp {
		div := &EdgeDivergence{
			PropositionID: propID,
			From:          s.from,
			To:            s.to,
			Direction:     directionString(s.dir),
			PriorWeight:   s.prior,
			Effective:     s.eff,
			EMA:           s.ema,
			Confidence:    s.conf,
			NObservations: s.nobs,
		}
		div.Range = floatRange(s.eff)
		div.StdDev = sampleStdDev(s.eff)
		out = append(out, div)
	}

	// Sort by Range desc, then by PropositionID asc for stable ordering.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Range != out[j].Range {
			return out[i].Range > out[j].Range
		}
		return propIDLess(out[i].PropositionID, out[j].PropositionID)
	})
	return out
}

// TopN returns the first n entries (or all, if n is larger). Convenience
// wrapper used by the table formatter.
func TopN(div []*EdgeDivergence, n int) []*EdgeDivergence {
	if n <= 0 || n > len(div) {
		return div
	}
	return div[:n]
}

func directionString(d types.Direction) string {
	if d == types.Negative {
		return "-"
	}
	return "+"
}

// floatRange returns max - min across the slice. Empty slices return 0.
func floatRange(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	lo, hi := xs[0], xs[0]
	for _, v := range xs[1:] {
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	return hi - lo
}

// sampleStdDev returns the sample (Bessel-corrected) standard deviation.
// For n <= 1 returns 0 — a single KD has no spread.
func sampleStdDev(xs []float64) float64 {
	n := len(xs)
	if n <= 1 {
		return 0
	}
	var sum float64
	for _, v := range xs {
		sum += v
	}
	mean := sum / float64(n)
	var ss float64
	for _, v := range xs {
		d := v - mean
		ss += d * d
	}
	return math.Sqrt(ss / float64(n-1))
}
