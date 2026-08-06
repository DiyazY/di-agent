package scripted_test

import (
	"math"
	"testing"

	"github.com/DiyazY/di-agent/compliance"
	"github.com/DiyazY/di-agent/internal/scripted"
	"github.com/DiyazY/di-agent/pkg/contracts"
	"github.com/DiyazY/di-agent/pkg/types"
)

// ── Compliance ────────────────────────────────────────────────────────────────

func TestScriptedCollectorCompliance(t *testing.T) {
	compliance.RunCollectorCompliance(t, func(t *testing.T) contracts.CollectorContract {
		return scripted.New("node_1",
			scripted.ConstantPattern{
				Metric: types.CPUUtilization, Value: 0.42, Node: "node_1",
				StartTick: 0, EndTick: -1,
			},
			scripted.ConstantPattern{
				Metric: types.MemoryUtilization, Value: 0.30, Node: "node_1",
				StartTick: 0, EndTick: -1,
			},
		)
	})
}

// ── ConstantPattern ───────────────────────────────────────────────────────────

func TestConstantPattern_Sample(t *testing.T) {
	p := scripted.ConstantPattern{
		Metric: types.CPUUtilization, Value: 0.6, Node: "n",
		StartTick: 5, EndTick: 10,
	}
	// Before window.
	if _, ok := p.Sample(4); ok {
		t.Error("expected ok=false before StartTick")
	}
	// In window.
	v, ok := p.Sample(7)
	if !ok || v != 0.6 {
		t.Errorf("expected (0.6, true) inside window; got (%v, %v)", v, ok)
	}
	// At EndTick (exclusive).
	if _, ok := p.Sample(10); ok {
		t.Error("expected ok=false at EndTick (exclusive upper bound)")
	}
	// Forever pattern.
	forever := scripted.ConstantPattern{Metric: types.CPUUtilization, Value: 0.1, Node: "n", EndTick: -1}
	if _, ok := forever.Sample(1_000_000); !ok {
		t.Error("forever pattern should remain ok at large tick")
	}
}

// ── RampPattern ───────────────────────────────────────────────────────────────

func TestRampPattern_Sample(t *testing.T) {
	p := scripted.RampPattern{
		Metric: types.CPUUtilization, From: 0.0, To: 1.0, Node: "n",
		StartTick: 0, EndTick: 11, // 11 ticks → spans [0,10]
	}
	v0, ok0 := p.Sample(0)
	v10, ok10 := p.Sample(10)
	if !ok0 || v0 != 0.0 {
		t.Errorf("ramp at tick 0 should be 0.0; got (%v, %v)", v0, ok0)
	}
	if !ok10 || math.Abs(v10-1.0) > 1e-9 {
		t.Errorf("ramp at tick 10 should be 1.0; got (%v, %v)", v10, ok10)
	}
	// Midpoint.
	v5, _ := p.Sample(5)
	if math.Abs(v5-0.5) > 0.01 {
		t.Errorf("ramp midpoint should be ~0.5; got %v", v5)
	}
}

// ── StepPattern ───────────────────────────────────────────────────────────────

func TestStepPattern_Sample(t *testing.T) {
	p := scripted.NewStepPattern(types.CPUUtilization, "n", []scripted.StepPoint{
		{Tick: 0, Value: 0.3},
		{Tick: 100, Value: 0.7},
		{Tick: 250, Value: 0.5},
	})
	cases := []struct {
		tick int64
		want float64
	}{
		{0, 0.3}, {50, 0.3}, {99, 0.3}, {100, 0.7}, {249, 0.7}, {250, 0.5}, {10_000, 0.5},
	}
	for _, c := range cases {
		v, ok := p.Sample(c.tick)
		if !ok {
			t.Errorf("tick=%d: expected ok=true", c.tick)
			continue
		}
		if v != c.want {
			t.Errorf("tick=%d: expected %v, got %v", c.tick, c.want, v)
		}
	}
	// Inactive before first step.
	pNeg := scripted.NewStepPattern(types.CPUUtilization, "n", []scripted.StepPoint{
		{Tick: 5, Value: 0.5},
	})
	if _, ok := pNeg.Sample(2); ok {
		t.Error("expected ok=false before first step")
	}
}

// ── SineWavePattern ───────────────────────────────────────────────────────────

func TestSineWavePattern_Sample(t *testing.T) {
	p := scripted.SineWavePattern{
		Metric: types.CPUUtilization, Node: "n",
		Mid: 0.5, Amp: 0.3, PeriodTicks: 20,
		StartTick: 0, EndTick: -1,
	}
	v0, _ := p.Sample(0)       // sin(0) = 0
	vQuarter, _ := p.Sample(5) // sin(π/2) = 1
	vHalf, _ := p.Sample(10)   // sin(π) = 0
	vThree, _ := p.Sample(15)  // sin(3π/2) = -1
	if math.Abs(v0-0.5) > 1e-9 {
		t.Errorf("sine at phase 0 should equal Mid (0.5); got %v", v0)
	}
	if math.Abs(vQuarter-0.8) > 1e-9 {
		t.Errorf("sine at quarter-period should equal Mid+Amp (0.8); got %v", vQuarter)
	}
	if math.Abs(vHalf-0.5) > 1e-9 {
		t.Errorf("sine at half-period should equal Mid (0.5); got %v", vHalf)
	}
	if math.Abs(vThree-0.2) > 1e-9 {
		t.Errorf("sine at three-quarter-period should equal Mid-Amp (0.2); got %v", vThree)
	}
}

// ── BurstPattern ──────────────────────────────────────────────────────────────

func TestBurstPattern_Sample(t *testing.T) {
	p := scripted.BurstPattern{
		Metric: types.CPUUtilization, Node: "n",
		Base: 0.1, Burst: 0.9,
		BurstStart: 100, BurstDur: 50,
	}
	cases := []struct {
		tick int64
		want float64
	}{
		{0, 0.1}, {99, 0.1}, {100, 0.9}, {149, 0.9}, {150, 0.1}, {1000, 0.1},
	}
	for _, c := range cases {
		v, ok := p.Sample(c.tick)
		if !ok || v != c.want {
			t.Errorf("tick=%d: expected (%v, true); got (%v, %v)", c.tick, c.want, v, ok)
		}
	}
}

// ── NoisyPattern ──────────────────────────────────────────────────────────────

func TestNoisyPattern_Sample(t *testing.T) {
	base := scripted.ConstantPattern{
		Metric: types.CPUUtilization, Value: 0.5, Node: "n", EndTick: -1,
	}
	// Same seed → same sequence.
	p1 := scripted.NewNoisyPattern(base, 0.05, 42)
	p2 := scripted.NewNoisyPattern(base, 0.05, 42)
	for tick := int64(1); tick <= 20; tick++ {
		v1, _ := p1.Sample(tick)
		v2, _ := p2.Sample(tick)
		if v1 != v2 {
			t.Errorf("tick=%d: noisy patterns with same seed diverged: %v vs %v", tick, v1, v2)
		}
		if v1 < 0 || v1 > 1 {
			t.Errorf("tick=%d: noisy sample outside [0,1]: %v", tick, v1)
		}
	}
	// Mean is approximately the base value over many samples.
	var sum float64
	for tick := int64(1); tick <= 1000; tick++ {
		v, _ := p1.Sample(tick)
		sum += v
	}
	mean := sum / 1000
	if math.Abs(mean-0.5) > 0.02 {
		t.Errorf("noisy mean too far from base 0.5: %v", mean)
	}
}

// ── Collector behaviors ───────────────────────────────────────────────────────

func TestScriptedCollector_CollectAdvancesTick(t *testing.T) {
	c := scripted.New("n",
		scripted.ConstantPattern{Metric: types.CPUUtilization, Value: 0.42, Node: "n", EndTick: -1},
	)
	if c.Tick() != 0 {
		t.Fatalf("initial tick should be 0; got %d", c.Tick())
	}
	for i := 1; i <= 5; i++ {
		samples, err := c.Collect()
		if err != nil {
			t.Fatal(err)
		}
		if len(samples) != 1 {
			t.Fatalf("expected 1 sample; got %d", len(samples))
		}
		if c.Tick() != int64(i) {
			t.Errorf("after %d Collect(): tick=%d; want %d", i, c.Tick(), i)
		}
	}
}

func TestScriptedCollector_DeterministicEventID(t *testing.T) {
	mk := func() *scripted.ScriptedCollector {
		return scripted.New("n",
			scripted.ConstantPattern{Metric: types.CPUUtilization, Value: 0.42, Node: "n", EndTick: -1},
			scripted.ConstantPattern{Metric: types.MemoryUtilization, Value: 0.30, Node: "n", EndTick: -1},
		)
	}
	a := mk()
	b := mk()
	for i := 0; i < 10; i++ {
		sa, _ := a.Collect()
		sb, _ := b.Collect()
		if len(sa) != len(sb) {
			t.Fatalf("tick %d: sample count mismatch %d vs %d", i, len(sa), len(sb))
		}
		for j := range sa {
			if sa[j].EventID != sb[j].EventID {
				t.Errorf("tick %d sample[%d]: EventID mismatch %q vs %q",
					i, j, sa[j].EventID, sb[j].EventID)
			}
			if sa[j].Value != sb[j].Value {
				t.Errorf("tick %d sample[%d]: Value mismatch %v vs %v",
					i, j, sa[j].Value, sb[j].Value)
			}
		}
	}
}

func TestScriptedCollector_ResetRewinds(t *testing.T) {
	c := scripted.New("n",
		scripted.ConstantPattern{Metric: types.CPUUtilization, Value: 0.42, Node: "n", EndTick: -1},
	)
	for i := 0; i < 7; i++ {
		_, _ = c.Collect()
	}
	if c.Tick() != 7 {
		t.Fatalf("expected tick=7; got %d", c.Tick())
	}
	c.Reset()
	if c.Tick() != 0 {
		t.Errorf("Reset() did not rewind to 0; got %d", c.Tick())
	}
	// Next Collect() should land at tick 1 again with the original EventID.
	first, _ := c.Collect()
	c.Reset()
	again, _ := c.Collect()
	if len(first) != len(again) || first[0].EventID != again[0].EventID {
		t.Error("Reset() then Collect() did not reproduce the first tick's EventID")
	}
}

func TestScriptedCollector_AvailableMetricsDeduped(t *testing.T) {
	c := scripted.New("n",
		scripted.ConstantPattern{Metric: types.CPUUtilization, Value: 0.4, Node: "n", EndTick: -1},
		scripted.ConstantPattern{Metric: types.CPUUtilization, Value: 0.5, Node: "n", EndTick: -1},
		scripted.ConstantPattern{Metric: types.MemoryUtilization, Value: 0.6, Node: "n", EndTick: -1},
	)
	got := c.AvailableMetrics()
	if len(got) != 2 {
		t.Errorf("expected 2 distinct metric types; got %d (%v)", len(got), got)
	}
}

func TestScriptedCollector_InactivePatternsSkipped(t *testing.T) {
	c := scripted.New("n",
		scripted.ConstantPattern{Metric: types.CPUUtilization, Value: 0.4, Node: "n", StartTick: 5, EndTick: -1},
	)
	// Ticks 1..4 should yield zero samples; ticks 5+ should yield one.
	for tick := 1; tick <= 4; tick++ {
		s, _ := c.Collect()
		if len(s) != 0 {
			t.Errorf("tick=%d expected 0 samples; got %d", tick, len(s))
		}
	}
	for tick := 5; tick <= 8; tick++ {
		s, _ := c.Collect()
		if len(s) != 1 {
			t.Errorf("tick=%d expected 1 sample; got %d", tick, len(s))
		}
	}
}
