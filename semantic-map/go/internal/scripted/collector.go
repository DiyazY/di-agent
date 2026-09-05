// Package scripted provides a programmable CollectorContract implementation.
//
// ScriptedCollector is a real Collector — not a test fixture. It powers
// deterministic demos, evolution scenarios, and parquet-replay drivers. Each
// Collect() call advances an internal tick counter by one and returns a sample
// per active pattern at that tick. Patterns are composable: a single collector
// can mix constants, ramps, steps, sines, bursts, and noise wrappers, each
// emitting a distinct MetricType for one node.
//
// EventID is deterministic per (source, node, metric, tick) so two collectors
// with identical patterns produce byte-identical sample streams — matching the
// CollectorContract guarantee and letting the Updater idempotency carry through
// to scenario replay.
package scripted

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/DiyazY/di-agent/pkg/types"
)

// Pattern is one programmable signal scoped to a single (node, metric type).
//
// Sample(tick) returns the value at the given tick along with an `ok` flag
// that is false when the pattern is outside its active window. Patterns are
// pure functions of tick + their own configuration, so two calls at the same
// tick return identical values (the rand-backed NoisyPattern is the explicit
// exception — but its seed is fixed at construction).
type Pattern interface {
	Sample(tick int64) (value float64, ok bool)
	MetricType() types.MetricType
	NodeID() string
}

// ConstantPattern emits the same value on every tick inside [StartTick, EndTick).
// EndTick < 0 means "forever" — the pattern stays active for the lifetime of
// the collector.
type ConstantPattern struct {
	Metric    types.MetricType
	Value     float64
	Node      string
	StartTick int64
	EndTick   int64 // < 0 = no upper bound
}

func (p ConstantPattern) Sample(tick int64) (float64, bool) {
	if tick < p.StartTick {
		return 0, false
	}
	if p.EndTick >= 0 && tick >= p.EndTick {
		return 0, false
	}
	return p.Value, true
}

func (p ConstantPattern) MetricType() types.MetricType { return p.Metric }
func (p ConstantPattern) NodeID() string               { return p.Node }

// RampPattern linearly interpolates from `From` at StartTick to `To` at
// EndTick - 1. Outside [StartTick, EndTick) it is inactive.
type RampPattern struct {
	Metric    types.MetricType
	From      float64
	To        float64
	Node      string
	StartTick int64
	EndTick   int64
}

func (p RampPattern) Sample(tick int64) (float64, bool) {
	if tick < p.StartTick || tick >= p.EndTick {
		return 0, false
	}
	span := float64(p.EndTick - p.StartTick - 1)
	if span <= 0 {
		return p.From, true
	}
	frac := float64(tick-p.StartTick) / span
	return p.From + (p.To-p.From)*frac, true
}

func (p RampPattern) MetricType() types.MetricType { return p.Metric }
func (p RampPattern) NodeID() string               { return p.Node }

// StepPoint is one piece of a piecewise-constant step pattern.
type StepPoint struct {
	Tick  int64
	Value float64
}

// StepPattern emits a piecewise-constant signal. Each step takes effect at
// its declared tick and persists until the next step. Steps with smaller
// Tick must appear first; the constructor (caller responsibility) sorts the
// slice if needed via NewStepPattern.
type StepPattern struct {
	Metric types.MetricType
	Node   string
	Steps  []StepPoint
}

// NewStepPattern returns a StepPattern with Steps sorted by Tick ascending.
func NewStepPattern(metric types.MetricType, node string, steps []StepPoint) StepPattern {
	sorted := append([]StepPoint(nil), steps...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Tick < sorted[j].Tick })
	return StepPattern{Metric: metric, Node: node, Steps: sorted}
}

func (p StepPattern) Sample(tick int64) (float64, bool) {
	if len(p.Steps) == 0 || tick < p.Steps[0].Tick {
		return 0, false
	}
	// Find the latest step whose tick is ≤ requested tick.
	current := p.Steps[0].Value
	for _, s := range p.Steps {
		if s.Tick > tick {
			break
		}
		current = s.Value
	}
	return current, true
}

func (p StepPattern) MetricType() types.MetricType { return p.Metric }
func (p StepPattern) NodeID() string               { return p.Node }

// SineWavePattern oscillates with amplitude Amp around Mid over PeriodTicks.
// Outside [StartTick, EndTick) it is inactive.
type SineWavePattern struct {
	Metric      types.MetricType
	Node        string
	Mid         float64
	Amp         float64
	PeriodTicks float64
	StartTick   int64
	EndTick     int64 // < 0 = forever
}

func (p SineWavePattern) Sample(tick int64) (float64, bool) {
	if tick < p.StartTick {
		return 0, false
	}
	if p.EndTick >= 0 && tick >= p.EndTick {
		return 0, false
	}
	if p.PeriodTicks <= 0 {
		return p.Mid, true
	}
	phase := 2 * math.Pi * float64(tick-p.StartTick) / p.PeriodTicks
	return p.Mid + p.Amp*math.Sin(phase), true
}

func (p SineWavePattern) MetricType() types.MetricType { return p.Metric }
func (p SineWavePattern) NodeID() string               { return p.Node }

// BurstPattern emits Base outside the burst window and Burst inside
// [BurstStart, BurstStart+BurstDur).
type BurstPattern struct {
	Metric     types.MetricType
	Node       string
	Base       float64
	Burst      float64
	BurstStart int64
	BurstDur   int64
}

func (p BurstPattern) Sample(tick int64) (float64, bool) {
	if p.BurstDur > 0 && tick >= p.BurstStart && tick < p.BurstStart+p.BurstDur {
		return p.Burst, true
	}
	return p.Base, true
}

func (p BurstPattern) MetricType() types.MetricType { return p.Metric }
func (p BurstPattern) NodeID() string               { return p.Node }

// NoisyPattern wraps another Pattern with additive Gaussian noise. The value
// is clipped to [0, 1] so downstream consumers (Updater idempotency, blending)
// never see out-of-range samples. The wrapped pattern's `ok` flag is honored.
type NoisyPattern struct {
	Base  Pattern
	Sigma float64
	rng   *rand.Rand
}

// NewNoisyPattern wraps base with Gaussian noise N(0, sigma). seed makes the
// noise reproducible — two scenarios with the same seed produce identical
// jitter sequences.
func NewNoisyPattern(base Pattern, sigma float64, seed int64) *NoisyPattern {
	return &NoisyPattern{
		Base:  base,
		Sigma: sigma,
		rng:   rand.New(rand.NewSource(seed)),
	}
}

func (p *NoisyPattern) Sample(tick int64) (float64, bool) {
	v, ok := p.Base.Sample(tick)
	if !ok {
		return 0, false
	}
	v += p.rng.NormFloat64() * p.Sigma
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	return v, true
}

func (p *NoisyPattern) MetricType() types.MetricType { return p.Base.MetricType() }
func (p *NoisyPattern) NodeID() string               { return p.Base.NodeID() }

// ScriptedCollector is a CollectorContract implementation backed by a fixed
// list of Patterns. Each Collect() call advances `tick` by 1 and emits a
// MetricSample per pattern that is active (ok=true) at the new tick.
type ScriptedCollector struct {
	nodeID    string
	sourceID  string
	mu        sync.Mutex
	patterns  []Pattern
	available []types.MetricType
	tick      int64
}

// New builds a ScriptedCollector for the given node, seeded with `patterns`.
// More patterns can be added later via Add. AvailableMetrics is recomputed
// (deduped, sorted) each time the pattern list changes.
func New(nodeID string, patterns ...Pattern) *ScriptedCollector {
	c := &ScriptedCollector{
		nodeID:   nodeID,
		sourceID: "scripted:" + nodeID,
	}
	for _, p := range patterns {
		c.patterns = append(c.patterns, p)
	}
	c.recomputeAvailable()
	return c
}

// Add appends a pattern to the collector's program.
func (c *ScriptedCollector) Add(p Pattern) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.patterns = append(c.patterns, p)
	c.recomputeAvailable()
}

// Tick returns the most recently emitted tick value. Useful for scenarios
// that want to log "T=N" lines that match the collector's internal counter.
func (c *ScriptedCollector) Tick() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tick
}

// Reset rewinds the internal tick counter so a scenario can replay from the
// start. The pattern list is unchanged.
func (c *ScriptedCollector) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tick = 0
}

// SourceID returns the collector's stable source identifier.
func (c *ScriptedCollector) SourceID() string { return c.sourceID }

// AvailableMetrics returns the deduped union of MetricTypes declared by
// the patterns, sorted for stable iteration.
func (c *ScriptedCollector) AvailableMetrics() []types.MetricType {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]types.MetricType, len(c.available))
	copy(out, c.available)
	return out
}

// Collect advances the tick by one and returns the samples produced by every
// active pattern at that tick. EventIDs are deterministic per
// (source, node, metric, tick) so the same tick produces the same EventID
// across collectors with the same configuration.
func (c *ScriptedCollector) Collect() ([]*types.MetricSample, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tick++
	now := time.Now()
	out := make([]*types.MetricSample, 0, len(c.patterns))
	for _, p := range c.patterns {
		v, ok := p.Sample(c.tick)
		if !ok {
			continue
		}
		// Clip to [0, 1] — the metric types the v1 scenarios emit are all
		// fraction-typed. Out-of-range bytes/sec or ms metrics belong to a
		// future extension and would skip this clip in their own collector.
		if v < 0 {
			v = 0
		}
		if v > 1 {
			v = 1
		}
		rng := scriptedRange
		out = append(out, &types.MetricSample{
			NodeID:        p.NodeID(),
			MetricType:    p.MetricType(),
			Value:         v,
			TimestampUnix: now.Unix(),
			EventID:       eventID(c.sourceID, p.NodeID(), p.MetricType(), c.tick),
			Unit:          "fraction",
			Range:         &rng,
			Source:        c.sourceID,
		})
	}
	return out, nil
}

// scriptedRange matches the clip above: every value this collector emits is a
// fraction in [0,1], and the contract requires it to say so on every sample.
var scriptedRange = [2]float64{0, 1}

func (c *ScriptedCollector) recomputeAvailable() {
	seen := make(map[types.MetricType]bool, len(c.patterns))
	for _, p := range c.patterns {
		seen[p.MetricType()] = true
	}
	out := make([]types.MetricType, 0, len(seen))
	for mt := range seen {
		out = append(out, mt)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	c.available = out
}

// eventID derives a stable 16-character event identifier from
// (source, node, metric, tick). Two collectors with identical configuration
// produce the same EventID stream — required for idempotent replay.
func eventID(sourceID, nodeID string, mt types.MetricType, tick int64) string {
	h := sha256.Sum256([]byte(sourceID + "|" + nodeID + "|" + string(mt) + ":" + strconv.FormatInt(tick, 10)))
	return hex.EncodeToString(h[:8]) // 16 hex chars
}
