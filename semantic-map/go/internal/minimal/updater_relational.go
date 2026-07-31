package minimal

import (
	"fmt"
	"math"
	"sync"

	"github.com/DiyazY/di-agent/pkg/types"
)

// RelationalEMAUpdater learns the strength of the relation an edge asserts,
// rather than tracking whichever endpoint a sample happened to observe.
//
// # Why this exists
//
// EMAUpdater incorporates a single construct's value as if it were an
// observation of the edge weight. That is defensible only as a proxy: an edge
// says "RC influences PS with strength w", and the utilization of RC is not a
// measurement of w. Two concrete consequences followed from the proxy:
//
//  1. The blend `effective = (1-c)*prior + c*ema` mixed incomparable scales. The
//     priors in prior_weights.json are calibrated as |Spearman rho| between
//     construct series, so the prior is an association strength while the EMA
//     was a utilization fraction. Confidence then interpolated between two
//     quantities that do not measure the same thing.
//  2. Conflict pairs could not diverge on evidence. P2 and P3 share RC->PS with
//     opposite directions, and the architecture calls them "evidence-
//     distinguishable mechanisms" — but both received the identical observation,
//     so their EMAs moved identically and nothing distinguished them.
//
// This updater takes paired observations of both endpoints and learns |r|, the
// magnitude of the Pearson correlation over a sliding window, which is on the
// same scale as the calibrated prior. A pair whose sign contradicts the edge's
// declared direction is evidence against that proposition, so the observed
// strength is driven toward zero rather than toward |r| — which is what lets a
// conflict pair separate: the same pair stream raises one sibling and lowers the
// other.
//
// # What it does not do
//
// Correlation is not causation and this does not claim otherwise. The edge's
// direction and existence come from the grounded-theory backbone; what is
// learned is the magnitude of the association this cluster exhibits, and whether
// the sign the backbone declares is the sign the cluster shows.
type RelationalEMAUpdater struct {
	*EMAUpdater

	minSupport int // pairs required before a strength is emitted
	window     int // sliding window length in pairs

	pmu     sync.Mutex
	buffers map[string]*pairWindow // keyed by from→to
}

// pairWindow is a fixed-capacity ring of paired observations for one construct
// pair, with the running sums Pearson r needs.
type pairWindow struct {
	xs, ys []float64
	next   int
	filled bool
}

func newPairWindow(capacity int) *pairWindow {
	return &pairWindow{xs: make([]float64, 0, capacity), ys: make([]float64, 0, capacity)}
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
	w.filled = true
}

func (w *pairWindow) n() int { return len(w.xs) }

// pearson returns the correlation over the window, and false when it is
// undefined — fewer than three points, or a constant series on either side.
// A constant series is the common case at idle, where pressure sits at zero for
// long stretches: reporting r = 0 there would be a claim, and "undefined" is
// the honest answer.
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

// NewRelationalEMAUpdater wraps an EMAUpdater with the paired path.
//   - minSupport: pairs required before the first strength is emitted.
//   - window: sliding window length; older pairs fall out.
func NewRelationalEMAUpdater(storage storageWriter, alpha, convergenceThreshold float64,
	minSupport, window int) *RelationalEMAUpdater {
	if window < 3 {
		window = 3
	}
	if minSupport < 3 {
		minSupport = 3
	}
	if minSupport > window {
		minSupport = window
	}
	return &RelationalEMAUpdater{
		EMAUpdater: NewEMAUpdater(storage, alpha, convergenceThreshold),
		minSupport: minSupport,
		window:     window,
		buffers:    make(map[string]*pairWindow),
	}
}

// UpdateEdgeRelation incorporates one paired observation of (fromID, toID).
//
// fromValue and toValue are the current normalized values of the source and
// target constructs. The pair enters the edge's sliding window; once the window
// holds minSupport pairs, the observed strength is
//
//	strength = |r|          when sign(r) agrees with the edge's direction
//	strength = 0            when it does not
//
// and that strength is EMA-blended into EMAWeight exactly as a direct
// observation would be, so confidence, idempotency and Reset semantics are
// unchanged from EMAUpdater.
//
// Before minSupport is reached the pair is recorded and the edge is left alone:
// n_observations does not advance, so confidence reports that the agent has not
// yet learned anything about this relation. This differs from the endpoint path,
// where the first sample already moves the weight.
func (u *RelationalEMAUpdater) UpdateEdgeRelation(fromID, toID string,
	fromValue, toValue float64, eventID string) (*types.EdgeDescriptor, error) {

	edges, err := u.storage.GetEdgesByPair(fromID, toID)
	if err != nil {
		return nil, err
	}
	if len(edges) == 0 {
		return nil, fmt.Errorf("edge %s→%s not found in storage", fromID, toID)
	}

	r, ok := u.observe(fromID, toID, fromValue, toValue, eventID)
	if !ok {
		// Not enough support yet, a constant series, or a duplicate event.
		return u.storage.GetEdge(fromID, toID)
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	for _, edge := range edges {
		key := fmt.Sprintf("%s→%s:%s:%s", fromID, toID, edge.PropositionID, eventID)
		if _, dup := u.seen[key]; dup {
			continue
		}
		strength := math.Abs(r)
		if !signAgrees(r, edge.Direction) {
			// The cluster shows the opposite sign to the one this proposition
			// asserts. That is evidence against this proposition specifically —
			// its conflict-pair sibling, if one exists, receives |r| instead.
			strength = 0
		}
		updated := *edge
		updated.EMAWeight = u.alpha*strength + (1-u.alpha)*edge.EMAWeight
		updated.NObservations = edge.NObservations + 1
		updated.Confidence = math.Min(float64(updated.NObservations)/u.convergenceThreshold, 1.0)
		if err := u.storage.PutEdge(&updated); err != nil {
			return nil, err
		}
		u.seen[key] = struct{}{}
	}
	return u.storage.GetEdge(fromID, toID)
}

// observe appends the pair and returns the window's correlation when one is
// defined and support has been reached. Duplicate event IDs are dropped before
// they reach the window, so a replayed batch cannot inflate the correlation by
// re-adding the same points.
func (u *RelationalEMAUpdater) observe(fromID, toID string, x, y float64, eventID string) (float64, bool) {
	pairKey := fromID + "→" + toID

	u.pmu.Lock()
	defer u.pmu.Unlock()

	dedupKey := "pair:" + pairKey + ":" + eventID
	u.mu.Lock()
	_, dup := u.seen[dedupKey]
	if !dup {
		u.seen[dedupKey] = struct{}{}
	}
	u.mu.Unlock()
	if dup {
		return 0, false
	}

	w, ok := u.buffers[pairKey]
	if !ok {
		w = newPairWindow(u.window)
		u.buffers[pairKey] = w
	}
	w.add(x, y, u.window)
	if w.n() < u.minSupport {
		return 0, false
	}
	return w.pearson()
}

// PairSupport reports how many pairs the window for (fromID, toID) currently
// holds. Exposed for diagnostics: an edge sitting at zero confidence in
// relational mode is usually short of support rather than broken.
func (u *RelationalEMAUpdater) PairSupport(fromID, toID string) int {
	u.pmu.Lock()
	defer u.pmu.Unlock()
	if w, ok := u.buffers[fromID+"→"+toID]; ok {
		return w.n()
	}
	return 0
}

// Reset clears the paired window along with the edge state, so a reset edge
// re-enters cold start with no residual correlation history.
func (u *RelationalEMAUpdater) Reset(fromID, toID string) error {
	if err := u.EMAUpdater.Reset(fromID, toID); err != nil {
		return err
	}
	u.pmu.Lock()
	delete(u.buffers, fromID+"→"+toID)
	u.pmu.Unlock()

	// Drop the pair dedup keys for this construct pair too; Reset's contract is
	// that future events are processed normally.
	prefix := "pair:" + fromID + "→" + toID + ":"
	u.mu.Lock()
	for k := range u.seen {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(u.seen, k)
		}
	}
	u.mu.Unlock()
	return nil
}

func signAgrees(r float64, dir types.Direction) bool {
	if dir == types.Negative {
		return r < 0
	}
	return r > 0
}
