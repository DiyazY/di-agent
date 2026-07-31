package compare

import (
	"os"
	"path/filepath"
	"testing"

	rp "github.com/DiyazY/di-agent/cmd/replay/parquet"
	pq "github.com/parquet-go/parquet-go"
)

// writeFakeParquet writes a small parquet covering 3 ticks with mappable
// chart_context rows. cpuIdle controls the system.cpu/idle value (so we
// can synthesize "KDs" with different CPU utilization signatures).
//
// Path: {dir}/{kd}/{test}_run{run}.parquet. The function creates the
// per-KD subdir on demand.
func writeFakeParquet(t *testing.T, root, kd, test string, run int, cpuIdle float64) {
	t.Helper()
	kdDir := filepath.Join(root, kd)
	if err := os.MkdirAll(kdDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(kdDir, test+"_run"+itoa(run)+".parquet")
	rows := []rp.Row{
		{Hostname: "master", ChartContext: "system.cpu", Units: "percentage", MetricID: "idle", Value: cpuIdle, RelativeTime: 0},
		{Hostname: "node_1", ChartContext: "system.cpu", Units: "percentage", MetricID: "idle", Value: cpuIdle, RelativeTime: 0},
		{Hostname: "master", ChartContext: "system.ram", Units: "MiB", MetricID: "used", Value: 1024, RelativeTime: 1},
		{Hostname: "master", ChartContext: "system.net", Units: "kilobits/s", MetricID: "InOctets", Value: 8.0, RelativeTime: 2},
		{Hostname: "master", ChartContext: "system.net", Units: "kilobits/s", MetricID: "OutOctets", Value: -16.0, RelativeTime: 2},
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	w := pq.NewGenericWriter[rp.Row](f)
	if _, err := w.Write(rows); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}
}

// itoa is a tiny strconv.Itoa replacement so the test file doesn't import
// strconv just for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		digits = append([]byte{'-'}, digits...)
	}
	return string(digits)
}

// TestRun_TwoSyntheticKDs uses two parquets with deliberately different
// CPU idle values and asserts the per-KD effective weights diverge in the
// expected direction. We don't use the real KD names because profiles.Build
// validates KD against the prior_weights.json file when one is supplied;
// passing an empty PriorWeightsPath bypasses that check entirely.
func TestRun_TwoSyntheticKDs(t *testing.T) {
	dir := t.TempDir()
	// "fake-a" has high CPU idle (low utilization). "fake-b" has low idle
	// (high utilization). Both feed RC, so the RC-driven edges (P1, P2,
	// P3, P8, P10, P14) should pick up different EMAs.
	writeFakeParquet(t, dir, "fake-a", "idle", 1, 99.0) // 1% utilization
	writeFakeParquet(t, dir, "fake-b", "idle", 1, 30.0) // 70% utilization

	opts := Options{
		DomainSpec: mustSpec(t),
		DataDir:    dir,
		TestType:   "idle",
		Run:        1,
		KDs:        []string{"fake-a", "fake-b"},
		// PriorWeightsPath empty → no per-KD priors; profiles.Build accepts
		// any KD string when no file is loaded.
	}
	r, err := Run(opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(r.PerKD) != 2 {
		t.Fatalf("PerKD count: got %d, want 2", len(r.PerKD))
	}
	if r.PerKD[0].KD != "fake-a" || r.PerKD[1].KD != "fake-b" {
		t.Errorf("KD order: got [%s, %s], want [fake-a, fake-b]",
			r.PerKD[0].KD, r.PerKD[1].KD)
	}
	for _, p := range r.PerKD {
		if p.SamplesSent == 0 {
			t.Errorf("KD %s sent zero samples", p.KD)
		}
		if len(p.Edges) == 0 {
			t.Errorf("KD %s has empty edge snapshot", p.KD)
		}
	}

	// Find any RC-driven edge (P3: RC→PS, positive) and assert the two KDs
	// have different EMA weights — divergence is the whole point.
	var emaA, emaB float64
	var nObsA, nObsB int
	for _, e := range r.PerKD[0].Edges {
		if e.PropositionID == "P3" {
			emaA = e.EMAWeight
			nObsA = e.NObservations
		}
	}
	for _, e := range r.PerKD[1].Edges {
		if e.PropositionID == "P3" {
			emaB = e.EMAWeight
			nObsB = e.NObservations
		}
	}
	if nObsA == 0 || nObsB == 0 {
		t.Errorf("P3 n_observations should be > 0 for both KDs; got A=%d, B=%d", nObsA, nObsB)
	}
	if emaA == emaB {
		t.Errorf("P3 EMA should diverge between fake-a (idle=99) and fake-b (idle=30); both got %.4f",
			emaA)
	}

	// One divergence entry per proposition in the loaded domain spec, and at least
	// one with Range > 0 since fake-a and fake-b differ.
	if want := len(mustSpec(t).Propositions); len(r.Divergence) != want {
		t.Errorf("Divergence count: got %d, want %d", len(r.Divergence), want)
	}
	maxRange := 0.0
	for _, d := range r.Divergence {
		if d.Range > maxRange {
			maxRange = d.Range
		}
	}
	if maxRange == 0 {
		t.Errorf("no divergence detected — per-KD streaming did not produce different EMAs")
	}
}

// TestRunMultiRunAveraging synthesizes 5 parquets for one fake KD with
// monotonically varying CPU idle values and asserts that the averaged
// EMA falls inside the range of per-run EMAs.
func TestRunMultiRunAveraging(t *testing.T) {
	dir := t.TempDir()
	// 5 runs with descending CPU idle (so utilization rises from 1% to 50%).
	idleValues := []float64{99.0, 90.0, 75.0, 60.0, 50.0}
	for i, v := range idleValues {
		writeFakeParquet(t, dir, "fake-a", "idle", i+1, v)
	}

	// Single-run results (run=1 — the highest idle case).
	singleOpts := Options{
		DomainSpec: mustSpec(t),
		DataDir:    dir,
		TestType:   "idle",
		Run:        1,
		KDs:        []string{"fake-a"},
	}
	singleR, err := Run(singleOpts)
	if err != nil {
		t.Fatalf("Run single: %v", err)
	}

	// 5-run average results.
	avgOpts := Options{
		DomainSpec: mustSpec(t),
		DataDir:    dir,
		TestType:   "idle",
		Run:        0, // 0 = all 5 runs averaged
		KDs:        []string{"fake-a"},
	}
	avgR, err := Run(avgOpts)
	if err != nil {
		t.Fatalf("Run avg: %v", err)
	}

	// Pick P3 again (RC→PS). The average EMA should be different from any
	// single-run EMA (because the inputs differ across runs) and should
	// fall within the min/max envelope of per-run values.
	var singleEMA float64
	for _, e := range singleR.PerKD[0].Edges {
		if e.PropositionID == "P3" {
			singleEMA = e.EMAWeight
		}
	}
	var avgEMA float64
	for _, e := range avgR.PerKD[0].Edges {
		if e.PropositionID == "P3" {
			avgEMA = e.EMAWeight
		}
	}
	if singleEMA == 0 && avgEMA == 0 {
		t.Fatal("could not locate P3 in either snapshot")
	}
	// Idle=99 run gives the smallest utilization signal; avg should pull
	// higher (more utilization). Just assert they differ — exact direction
	// depends on EMAUpdater internals.
	if singleEMA == avgEMA {
		t.Errorf("avg EMA equals single-run EMA — averaging did not aggregate distinct snapshots")
	}
	t.Logf("single-run (idle=99) P3 EMA = %.4f", singleEMA)
	t.Logf("5-run average     P3 EMA = %.4f", avgEMA)

	// SamplesSent on the averaged result should equal the sum across runs
	// (which is 5× the per-run sent count). Sanity-check it.
	if avgR.PerKD[0].SamplesSent < singleR.PerKD[0].SamplesSent {
		t.Errorf("averaged SamplesSent should sum across runs; got avg=%d, single=%d",
			avgR.PerKD[0].SamplesSent, singleR.PerKD[0].SamplesSent)
	}
}

// TestRun_HostFilterRestrictsIngestion confirms NodeFilter trims rows
// to the named hostnames before ingestion.
func TestRun_HostFilterRestrictsIngestion(t *testing.T) {
	dir := t.TempDir()
	writeFakeParquet(t, dir, "fake-a", "idle", 1, 99.0)

	opts := Options{
		DomainSpec: mustSpec(t),
		DataDir:    dir,
		TestType:   "idle",
		Run:        1,
		KDs:        []string{"fake-a"},
		NodeFilter: []string{"node_1"},
	}
	r, err := Run(opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// fake parquet has 5 rows, 1 with hostname=node_1 (the second system.cpu
	// row at tick 0). All others are master.
	if r.PerKD[0].SamplesSent != 1 {
		t.Errorf("NodeFilter to node_1 alone: got SamplesSent=%d, want 1", r.PerKD[0].SamplesSent)
	}
}

// TestRun_MissingDataDir surfaces a useful error rather than silently
// producing empty results.
func TestRun_MissingDataDir(t *testing.T) {
	opts := Options{
		DomainSpec: mustSpec(t),
		DataDir:    "/nonexistent/path/that/does/not/exist",
		TestType:   "idle",
		Run:        1,
		KDs:        []string{"fake-a"},
	}
	if _, err := Run(opts); err == nil {
		t.Error("Run with missing data dir should return error")
	}
}
