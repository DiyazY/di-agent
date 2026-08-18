// Package playback drives one parquet file's rows into the daemon's
// /ingest-sample endpoint at a configurable speed.
//
// Playback model:
//
//	A parquet file holds ~300 seconds of Netdata observations, tagged with
//	an integer `relative_time` column (seconds since test start). Playback
//	groups rows by their relative_time tick. At each tick T, every row
//	whose chart_context+metric_id maps to a known MetricType becomes one
//	MetricSample, sent via POST /ingest-sample.
//
//	The cadence between ticks is `1 / Speed` seconds of wall clock:
//	  Speed = 1.0  → real time (300s of data takes 300s of replay)
//	  Speed = 60   → 60× compression (300s in 5s)
//	  Speed = 0    → as fast as possible (no waits between ticks)
//
// EventID determinism:
//
//	EventID = sha256("replay:" + filename + ":" + hostname + ":" +
//	                 chart_context + ":" + metric_id + ":" + relative_time)[:16]
//
//	This makes the replay idempotent through the Updater: replaying the
//	same parquet twice cannot inflate edge n_observations on the second
//	pass — the dedupe set inside UpdateEdge will skip every EventID it
//	has seen. End-to-end idempotency proof is in the acceptance section
//	of the README.
package playback

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"time"

	"github.com/DiyazY/di-agent/cmd/replay/client"
	"github.com/DiyazY/di-agent/cmd/replay/mapping"
	rp "github.com/DiyazY/di-agent/cmd/replay/parquet"
)

// Sender is the HTTP sink for one MetricSample. The Runner takes this
// interface (rather than a concrete *client.Client) so tests can swap in
// an httptest-backed stub.
type Sender interface {
	IngestSample(ctx context.Context, req client.MetricSampleRequest) error
}

// Config drives one replay run. ParquetPath is the only required field;
// everything else has reasonable defaults.
type Config struct {
	ParquetPath string

	// Speed is the time-warp factor. 1.0 = real-time, 60 = 60× faster,
	// 0 = no wait between ticks (max throughput). Negative values are
	// treated as 0.
	Speed float64

	// HostFilter, when non-empty, restricts emitted samples to rows whose
	// hostname is in this set. Useful for "just master" smoke tests.
	HostFilter map[string]struct{}

	// Progress receives one ProgressEvent per replayed tick. When nil,
	// progress is suppressed (callers can still inspect the returned
	// Summary).
	Progress func(ev ProgressEvent)
}

// ProgressEvent is emitted once per replayed tick (relative_time). It is
// the unit the CLI prints as a one-line "T=N → M samples sent" status.
type ProgressEvent struct {
	Tick           int64
	SamplesSent    int
	SamplesSkipped int
}

// Summary is returned by Run after the parquet stream completes.
type Summary struct {
	ParquetPath    string
	TotalRows      int64
	SamplesSent    int
	SamplesSkipped int
	Ticks          int
	Elapsed        time.Duration
}

// String renders a one-line headline suitable for end-of-replay logs.
func (s Summary) String() string {
	return fmt.Sprintf("%s: %d ticks, %d samples sent, %d skipped, elapsed %s",
		filepath.Base(s.ParquetPath), s.Ticks, s.SamplesSent, s.SamplesSkipped, s.Elapsed.Round(time.Millisecond))
}

// EventID returns the deterministic 16-character event identifier for one
// (filename, hostname, chart_context, metric_id, relative_time) tuple. It
// is the contract that makes re-replays idempotent through the Updater.
// Exported so unit tests can assert determinism without reaching into
// internals.
func EventID(parquetFilename, hostname, chartContext, metricID string, relativeTime int64) string {
	h := sha256.Sum256([]byte("replay:" +
		parquetFilename + ":" +
		hostname + ":" +
		chartContext + ":" +
		metricID + ":" +
		strconv.FormatInt(relativeTime, 10)))
	return hex.EncodeToString(h[:8]) // 16 hex chars
}

// Run streams the parquet at cfg.ParquetPath through the Bridge of the
// daemon behind sender. Each row that maps to a known MetricType becomes
// one POST /ingest-sample call. Rows are grouped by relative_time; the
// wall clock advances by 1/Speed seconds between consecutive groups.
//
// Returns the Summary after the parquet is fully consumed, or the first
// HTTP error if /ingest-sample fails (subsequent rows are not sent — a
// transport-level error means the daemon is gone, and there's no useful
// recovery in v1).
func Run(ctx context.Context, sender Sender, cfg Config) (*Summary, error) {
	r, err := rp.Open(cfg.ParquetPath)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	parquetName := filepath.Base(cfg.ParquetPath)
	start := time.Now()

	// Wait interval between consecutive ticks. Speed <= 0 → no wait.
	var interval time.Duration
	if cfg.Speed > 0 {
		interval = time.Duration(float64(time.Second) / cfg.Speed)
	}

	summary := &Summary{ParquetPath: cfg.ParquetPath, TotalRows: r.NumRows()}

	var (
		currentTick int64 = -1
		tickSent    int
		tickSkipped int
		// nextEmit holds the wall-clock time at which the upcoming tick
		// is allowed to start sending. Initialized to "now" so the first
		// tick fires immediately.
		nextEmit = start
	)

	flushTick := func() {
		if cfg.Progress != nil && currentTick >= 0 {
			cfg.Progress(ProgressEvent{
				Tick:           currentTick,
				SamplesSent:    tickSent,
				SamplesSkipped: tickSkipped,
			})
		}
		tickSent, tickSkipped = 0, 0
	}

	for {
		row, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return summary, err
		}
		// Host filter (if configured).
		if cfg.HostFilter != nil {
			if _, ok := cfg.HostFilter[row.Hostname]; !ok {
				continue
			}
		}
		// Tick boundary: emit progress for previous tick, sleep if needed,
		// advance currentTick.
		if row.RelativeTime != currentTick {
			flushTick()
			currentTick = row.RelativeTime
			summary.Ticks++
			if interval > 0 && summary.Ticks > 1 {
				// Sleep until the next emission window opens. If we
				// fell behind (sending took longer than interval), do
				// not sleep — we run flat-out catching up.
				nextEmit = nextEmit.Add(interval)
				wait := time.Until(nextEmit)
				if wait > 0 {
					select {
					case <-ctx.Done():
						return summary, ctx.Err()
					case <-time.After(wait):
					}
				}
			}
		}
		// Map this row to a MetricType. Unknown triples are skipped
		// silently — the parquets contain hundreds of internal-monitoring
		// contexts that don't fit the v1 MetricType catalogue.
		m := mapping.FromRow(row.ChartContext, row.MetricID, row.Units, row.Hostname, row.Value)
		if !m.Ok {
			tickSkipped++
			summary.SamplesSkipped++
			continue
		}
		req := client.MetricSampleRequest{
			NodeID:        row.Hostname,
			MetricType:    string(m.MetricType),
			Value:         m.Value,
			TimestampUnix: time.Now().Unix(),
			EventID:       EventID(parquetName, row.Hostname, row.ChartContext, row.MetricID, row.RelativeTime),
			Labels: map[string]string{
				"parquet":       parquetName,
				"chart_context": row.ChartContext,
				"metric_id":     row.MetricID,
				"relative_time": strconv.FormatInt(row.RelativeTime, 10),
			},
		}
		if err := sender.IngestSample(ctx, req); err != nil {
			return summary, fmt.Errorf("ingest %s/%s @ T=%d: %w",
				row.Hostname, row.ChartContext, row.RelativeTime, err)
		}
		tickSent++
		summary.SamplesSent++
	}
	flushTick()
	summary.Elapsed = time.Since(start)
	return summary, nil
}
