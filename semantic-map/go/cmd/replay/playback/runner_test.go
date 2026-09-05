package playback

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/cmd/replay/client"
	rp "github.com/DiyazY/di-agent/cmd/replay/parquet"
	pq "github.com/parquet-go/parquet-go"
)

// writeFixture writes a parquet whose rows span three ticks, contain a
// known-mappable triple per tick, and contain at least one row that should
// be silently dropped by the mapping layer.
func writeFixture(t *testing.T, path string) {
	t.Helper()
	rows := []rp.Row{
		// tick 0
		{Hostname: "master", ChartContext: "system.cpu", Units: "percentage", MetricID: "idle", Value: 99.0, RelativeTime: 0},
		{Hostname: "node_1", ChartContext: "system.cpu", Units: "percentage", MetricID: "idle", Value: 95.0, RelativeTime: 0},
		// non-mappable (chart_context unknown)
		{Hostname: "master", ChartContext: "netdata.workers.cpu", Units: "percentage", MetricID: "idle", Value: 0.5, RelativeTime: 0},
		// tick 1
		{Hostname: "master", ChartContext: "system.ram", Units: "MiB", MetricID: "used", Value: 820, RelativeTime: 1},
		// tick 2
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

// recorder is an httptest.Server-backed Sender that captures every
// MetricSampleRequest it receives. Used by every test below to assert
// what the runner actually emitted.
type recorder struct {
	mu       sync.Mutex
	received []client.MetricSampleRequest
	srv      *httptest.Server
}

func newRecorder(t *testing.T) *recorder {
	t.Helper()
	rec := &recorder{}
	mux := http.NewServeMux()
	mux.HandleFunc("/ingest-sample", func(w http.ResponseWriter, r *http.Request) {
		var req client.MetricSampleRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		rec.mu.Lock()
		rec.received = append(rec.received, req)
		rec.mu.Unlock()
		w.WriteHeader(204)
	})
	rec.srv = httptest.NewServer(mux)
	t.Cleanup(rec.srv.Close)
	return rec
}

func (r *recorder) sender() *client.Client { return client.New(r.srv.URL) }

func TestRunner_StreamsTicksWithMappableRows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.parquet")
	writeFixture(t, path)

	rec := newRecorder(t)
	cfg := Config{ParquetPath: path, Speed: 0} // max speed
	summary, err := Run(context.Background(), rec.sender(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 6 rows total, 5 mappable (1 dropped: netdata.workers.cpu).
	if summary.SamplesSent != 5 {
		t.Errorf("SamplesSent: got %d, want 5", summary.SamplesSent)
	}
	if summary.SamplesSkipped != 1 {
		t.Errorf("SamplesSkipped: got %d, want 1", summary.SamplesSkipped)
	}
	if summary.Ticks != 3 {
		t.Errorf("Ticks: got %d, want 3 (0,1,2)", summary.Ticks)
	}
	if len(rec.received) != 5 {
		t.Fatalf("HTTP receives: got %d, want 5", len(rec.received))
	}
	// All requests use the real hostname as node_id; spot-check the first.
	if rec.received[0].NodeID != "master" {
		t.Errorf("first NodeID: got %q, want master", rec.received[0].NodeID)
	}
	if rec.received[0].MetricType != "cpu_utilization" {
		t.Errorf("first MetricType: got %q, want cpu_utilization", rec.received[0].MetricType)
	}
	// 99% idle → 0.01 utilization. Allow small float wiggle.
	if v := rec.received[0].Value; v < 0.0099 || v > 0.0101 {
		t.Errorf("first Value: got %.4f, want ~0.01", v)
	}
}

func TestRunner_EventIDsAreDeterministicAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.parquet")
	writeFixture(t, path)

	first := newRecorder(t)
	if _, err := Run(context.Background(), first.sender(), Config{ParquetPath: path, Speed: 0}); err != nil {
		t.Fatal(err)
	}

	second := newRecorder(t)
	if _, err := Run(context.Background(), second.sender(), Config{ParquetPath: path, Speed: 0}); err != nil {
		t.Fatal(err)
	}

	if len(first.received) != len(second.received) {
		t.Fatalf("send counts differ: first=%d second=%d", len(first.received), len(second.received))
	}
	// Determinism is the contract end-to-end idempotency depends on.
	for i := range first.received {
		if first.received[i].EventID != second.received[i].EventID {
			t.Errorf("EventID[%d] differs: %q vs %q",
				i, first.received[i].EventID, second.received[i].EventID)
		}
	}
}

func TestRunner_HostFilterRestrictsEmission(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.parquet")
	writeFixture(t, path)

	rec := newRecorder(t)
	cfg := Config{
		ParquetPath: path, Speed: 0,
		HostFilter: map[string]struct{}{"node_1": {}},
	}
	if _, err := Run(context.Background(), rec.sender(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(rec.received) != 1 {
		t.Fatalf("host filter to node_1 alone: got %d sends, want 1", len(rec.received))
	}
	if rec.received[0].NodeID != "node_1" {
		t.Errorf("filtered NodeID: got %q, want node_1", rec.received[0].NodeID)
	}
}

func TestRunner_ProgressCallbackFires(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.parquet")
	writeFixture(t, path)

	rec := newRecorder(t)
	var events []ProgressEvent
	cfg := Config{
		ParquetPath: path, Speed: 0,
		Progress: func(ev ProgressEvent) { events = append(events, ev) },
	}
	if _, err := Run(context.Background(), rec.sender(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Errorf("progress events: got %d, want 3 (one per tick)", len(events))
	}
}

func TestRunner_SpeedThrottlesEmission(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.parquet")
	writeFixture(t, path)

	rec := newRecorder(t)
	start := time.Now()
	// 3 ticks at 30x means ~2 inter-tick gaps of 1/30s ≈ 33 ms each
	// → minimum ~66ms. Use a relaxed lower bound to avoid CI flakiness.
	cfg := Config{ParquetPath: path, Speed: 30}
	if _, err := Run(context.Background(), rec.sender(), cfg); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 30*time.Millisecond {
		t.Errorf("expected >=30ms elapsed at Speed=30; got %v", time.Since(start))
	}
}

func TestEventID_Deterministic(t *testing.T) {
	a := EventID("idle_run1.parquet", "master", "system.cpu", "idle", 42)
	b := EventID("idle_run1.parquet", "master", "system.cpu", "idle", 42)
	if a != b {
		t.Fatalf("not deterministic: %q vs %q", a, b)
	}
	if len(a) != 16 {
		t.Errorf("len: got %d, want 16", len(a))
	}
	// One field flip should change the digest.
	c := EventID("idle_run1.parquet", "master", "system.cpu", "idle", 43)
	if a == c {
		t.Error("EventID collides across distinct relative_time")
	}
}

// TestRunner_DeclaresUnitAndRangeOnEverySample covers the Collector contract's MUST at
// the replay boundary: every sample carries the unit and range its value is expressed
// in. mapping.FromRow normalises each supported triple to a fraction before it gets
// here, and a sample that does not say so leaves the map assuming [0,1] and caveating
// every answer that reads the property for the assumption.
func TestRunner_DeclaresUnitAndRangeOnEverySample(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.parquet")
	writeFixture(t, path)

	rec := newRecorder(t)
	if _, err := Run(context.Background(), rec.sender(), Config{ParquetPath: path, Speed: 0}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rec.received) == 0 {
		t.Fatal("no samples were sent; this test cannot see what they declare")
	}
	for i, req := range rec.received {
		if req.Unit != "fraction" {
			t.Errorf("sample %d Unit = %q; want \"fraction\"", i, req.Unit)
		}
		if req.Range == nil || *req.Range != [2]float64{0, 1} {
			t.Errorf("sample %d Range = %v; want [0 1]", i, req.Range)
		}
		if req.Source != SourceID("fixture.parquet") {
			t.Errorf("sample %d Source = %q; want %q", i, req.Source, SourceID("fixture.parquet"))
		}
	}
}
