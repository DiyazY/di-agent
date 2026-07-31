// Package compare runs the parquet replay for several KDs side-by-side and
// computes per-edge × per-KD divergence.
//
// IN-PROCESS DESIGN (deliberate exception to the cmd/replay convention).
//
// The other replay subcommands (`run`, `all`, `probe`) speak HTTP: they
// stream rows into a live daemon via POST /ingest-sample. That is correct
// for "feed a running agent", but it would be wrong for cross-KD
// comparison — replaying k3s after k0s into the same daemon would leave
// k0s's EMAs contaminated by k3s's observations.
//
// Compare is a meta-analysis tool. It builds N independent SemanticMaps
// in one process (one per KD), each seeded with that KD's calibrated
// priors from prior_weights.json, and feeds each only its own KD's
// parquet rows. After the streams end, it snapshots every map's edges
// and computes per-edge divergence.
//
// For this reason — and only this reason — the compare package imports
// pkg/profiles and pkg/semmap directly. The daemon-streaming replay
// stays HTTP-based and lives in cmd/replay/playback/. If a future
// contributor needs another HTTP-only subcommand, it does not need to
// follow compare's pattern.
//
// Deterministic EventIDs: compare reuses playback.EventID so a future
// `replay run` against the same parquet would yield zero new observations
// against an already-loaded daemon.
package compare

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/DiyazY/di-agent/cmd/replay/mapping"
	rp "github.com/DiyazY/di-agent/cmd/replay/parquet"
	"github.com/DiyazY/di-agent/cmd/replay/playback"
	"github.com/DiyazY/di-agent/pkg/domain"
	"github.com/DiyazY/di-agent/pkg/profiles"
	"github.com/DiyazY/di-agent/pkg/semmap"
	"github.com/DiyazY/di-agent/pkg/types"
)

// Options drives one compare invocation. Run() applies sensible defaults
// for the zero-value tuning fields.
type Options struct {
	// DomainSpec is the domain model the compared agents reason over. Required:
	// profiles.Build refuses a nil spec, since an agent with no constructs has
	// nothing to compare.
	DomainSpec *domain.Spec

	// DataDir is the parquet root (e.g. multidimensional-analysis/data/raw).
	// {DataDir}/{kd}/{test_type}_run{N}.parquet is the file layout.
	DataDir string

	// TestType selects which test (e.g. "idle", "cp_heavy_12client"). The
	// same test type is replayed for every KD.
	TestType string

	// Run selects 1..5 for a single representative replay. Run == 0 means
	// "replay all 5 runs per KD and average the snapshots" — useful for
	// the paper figure where one-run jitter would obscure cross-KD signal.
	Run int

	// KDs is the list of Kubernetes distributions to compare. Index order
	// drives column order in every output formatter.
	KDs []string

	// PriorWeightsPath is the prior_weights.json file to load. Empty means
	// "use Di-Select uncalibrated defaults" — comparison still works but
	// every KD starts from identical priors, so divergence is purely
	// data-driven and weaker than with calibrated seeds.
	PriorWeightsPath string

	// NodeFilter restricts replayed rows to a hostname subset. Empty = all.
	NodeFilter []string

	// EMAAlpha is forwarded to the per-KD SemanticMap. Default 0.2 when 0.
	EMAAlpha float64

	// ConvergenceThreshold is forwarded to the per-KD SemanticMap. Default
	// 500 when 0.
	ConvergenceThreshold float64
}

// PerKDResult bundles one KD's outcome after replaying its parquet(s) and
// snapshotting the final graph state.
type PerKDResult struct {
	KD             string
	SamplesSent    int
	SamplesSkipped int
	// Edges is the final snapshot of every edge in storage, sorted by
	// PropositionID for stable formatter output. When opts.Run == 0 the
	// EMAs / confidences / n_observations are arithmetic means across 5
	// per-run snapshots.
	Edges      []*types.EdgeDescriptor
	DurationMS int64
}

// Result is what Run() returns. PerKD entries are index-aligned with
// Options.KDs; Divergence holds one entry per unique PropositionID seen,
// sorted by Range descending (most discriminative edges first).
type Result struct {
	Options    Options
	PerKD      []*PerKDResult
	Divergence []*EdgeDivergence
}

// Run executes the full compare flow: for each KD it builds a fresh
// SemanticMap with per-KD priors, reads its parquet(s), drives
// sm.IngestSample in-process, and snapshots the final edge set. After
// every KD is done, it computes cross-KD divergence and returns a
// Result ready for any of the output formatters.
//
// The function performs no I/O against the daemon — it is purely local.
// The `dev.sh start` daemon (if running) is irrelevant to compare.
func Run(opts Options) (*Result, error) {
	if err := validate(opts); err != nil {
		return nil, err
	}
	opts = applyDefaults(opts)

	result := &Result{
		Options: opts,
		PerKD:   make([]*PerKDResult, 0, len(opts.KDs)),
	}

	hostFilter := buildHostFilter(opts.NodeFilter)

	for _, kd := range opts.KDs {
		perKD, err := replayOneKD(opts, kd, hostFilter)
		if err != nil {
			return nil, fmt.Errorf("kd %s: %w", kd, err)
		}
		result.PerKD = append(result.PerKD, perKD)
	}

	result.Divergence = Compute(result.PerKD)
	return result, nil
}

func validate(opts Options) error {
	if opts.DataDir == "" {
		return fmt.Errorf("DataDir is required")
	}
	if opts.TestType == "" {
		return fmt.Errorf("TestType is required")
	}
	if len(opts.KDs) == 0 {
		return fmt.Errorf("at least one KD must be specified")
	}
	if opts.Run < 0 || opts.Run > 5 {
		return fmt.Errorf("Run must be 0..5; got %d", opts.Run)
	}
	return nil
}

func applyDefaults(opts Options) Options {
	if opts.EMAAlpha == 0 {
		opts.EMAAlpha = 0.2
	}
	if opts.ConvergenceThreshold == 0 {
		opts.ConvergenceThreshold = 500
	}
	return opts
}

func buildHostFilter(nodes []string) map[string]struct{} {
	if len(nodes) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(nodes))
	for _, h := range nodes {
		if h != "" {
			out[h] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// replayOneKD is the per-KD branch. It chooses single-run or 5-run-average
// based on opts.Run and returns one PerKDResult either way.
func replayOneKD(opts Options, kd string, hostFilter map[string]struct{}) (*PerKDResult, error) {
	start := time.Now()
	var (
		perKD *PerKDResult
		err   error
	)
	if opts.Run == 0 {
		perKD, err = replayAllRunsAndAverage(opts, kd, hostFilter)
	} else {
		perKD, err = replaySingleRun(opts, kd, opts.Run, hostFilter)
	}
	if err != nil {
		return nil, err
	}
	perKD.DurationMS = time.Since(start).Milliseconds()
	return perKD, nil
}

// replaySingleRun builds one fresh SemanticMap for the given KD, replays
// run N, and snapshots its edges.
func replaySingleRun(opts Options, kd string, run int, hostFilter map[string]struct{}) (*PerKDResult, error) {
	sm, err := buildMap(opts, kd)
	if err != nil {
		return nil, err
	}
	path := parquetPath(opts.DataDir, kd, opts.TestType, run)
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("parquet not found: %s", path)
	}
	sent, skipped, err := streamParquet(sm, path, hostFilter)
	if err != nil {
		return nil, err
	}
	edges, err := sm.AllEdges()
	if err != nil {
		return nil, fmt.Errorf("snapshot edges: %w", err)
	}
	sortEdges(edges)
	return &PerKDResult{
		KD:             kd,
		SamplesSent:    sent,
		SamplesSkipped: skipped,
		Edges:          edges,
	}, nil
}

// replayAllRunsAndAverage runs each of N..5 in a fresh map and averages the
// per-edge EMA / Confidence / n_observations across the snapshots. PriorWeight
// is the same across runs by construction (no mutation), so we keep run 1's
// value.
//
// Missing parquets (some test types only have a few runs in the dataset) are
// skipped with a warning to stderr; the average uses only the runs that did
// complete. If zero runs are available, returns an error.
func replayAllRunsAndAverage(opts Options, kd string, hostFilter map[string]struct{}) (*PerKDResult, error) {
	var (
		snapshots    []*PerKDResult
		totalSent    int
		totalSkipped int
	)
	for run := 1; run <= 5; run++ {
		path := parquetPath(opts.DataDir, kd, opts.TestType, run)
		if _, err := os.Stat(path); err != nil {
			fmt.Fprintf(os.Stderr, "  skip: %s (missing)\n", filepath.Base(path))
			continue
		}
		s, err := replaySingleRun(opts, kd, run, hostFilter)
		if err != nil {
			return nil, fmt.Errorf("run %d: %w", run, err)
		}
		snapshots = append(snapshots, s)
		totalSent += s.SamplesSent
		totalSkipped += s.SamplesSkipped
	}
	if len(snapshots) == 0 {
		return nil, fmt.Errorf("no runs available for KD %s test %s", kd, opts.TestType)
	}

	avgEdges := averageEdges(snapshots)
	return &PerKDResult{
		KD:             kd,
		SamplesSent:    totalSent,
		SamplesSkipped: totalSkipped,
		Edges:          avgEdges,
	}, nil
}

// averageEdges arithmetic-averages EMAWeight, Confidence and NObservations
// across the per-run snapshots, keeping every other field from run 1's
// edge (PriorWeight is identical across runs by construction; PropositionID,
// Direction, FromID, ToID are static). Mu/Sigma are dropped (not relevant
// for v1 compare).
func averageEdges(snaps []*PerKDResult) []*types.EdgeDescriptor {
	if len(snaps) == 0 {
		return nil
	}
	// Index by PropositionID for stable cross-run alignment. Different snapshots
	// always carry the same proposition set since they share the same ontology.
	idx := map[string][]*types.EdgeDescriptor{}
	for _, s := range snaps {
		for _, e := range s.Edges {
			idx[e.PropositionID] = append(idx[e.PropositionID], e)
		}
	}
	out := make([]*types.EdgeDescriptor, 0, len(idx))
	for propID, edges := range idx {
		if len(edges) == 0 {
			continue
		}
		base := edges[0]
		var sumEMA, sumConf float64
		var sumN int
		for _, e := range edges {
			sumEMA += e.EMAWeight
			sumConf += e.Confidence
			sumN += e.NObservations
		}
		n := float64(len(edges))
		out = append(out, &types.EdgeDescriptor{
			FromID:        base.FromID,
			ToID:          base.ToID,
			PropositionID: propID,
			Direction:     base.Direction,
			PriorWeight:   base.PriorWeight,
			EMAWeight:     sumEMA / n,
			Confidence:    sumConf / n,
			NObservations: int(float64(sumN) / n),
		})
	}
	sortEdges(out)
	return out
}

// buildMap constructs a fresh SemanticMap for one KD with that KD's
// calibrated priors seeded. profiles.Build is the only place priors are
// applied, so we must call it once per KD.
func buildMap(opts Options, kd string) (*semmap.SemanticMap, error) {
	sm, _, err := profiles.Build("edge-minimal", profiles.Config{
		EMAAlpha:             opts.EMAAlpha,
		ConvergenceThreshold: opts.ConvergenceThreshold,
		PriorWeightsPath:     opts.PriorWeightsPath,
		KD:                   kd,
		DomainSpec:           opts.DomainSpec,
	})
	if err != nil {
		return nil, fmt.Errorf("profiles.Build %s: %w", kd, err)
	}
	return sm, nil
}

// streamParquet pumps one parquet file through sm.IngestSample. Modeled on
// playback.Run but driving the semmap directly (no HTTP) — and skipping
// the time-warp because compare is always max-speed (in-process; no live
// agent to pace).
//
// Returns (sent, skipped, error).
func streamParquet(sm *semmap.SemanticMap, path string, hostFilter map[string]struct{}) (int, int, error) {
	r, err := rp.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer r.Close()

	parquetName := filepath.Base(path)
	var sent, skipped int

	for {
		row, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return sent, skipped, err
		}
		if hostFilter != nil {
			if _, ok := hostFilter[row.Hostname]; !ok {
				continue
			}
		}
		m := mapping.FromRow(row.ChartContext, row.MetricID, row.Units, row.Hostname, row.Value)
		if !m.Ok {
			skipped++
			continue
		}
		sample := &types.MetricSample{
			NodeID:        row.Hostname,
			MetricType:    m.MetricType,
			Value:         m.Value,
			TimestampUnix: time.Now().Unix(),
			EventID:       playback.EventID(parquetName, row.Hostname, row.ChartContext, row.MetricID, row.RelativeTime),
			Labels: map[string]string{
				"parquet":       parquetName,
				"chart_context": row.ChartContext,
				"metric_id":     row.MetricID,
				"relative_time": strconv.FormatInt(row.RelativeTime, 10),
			},
		}
		if err := sm.IngestSample(sample); err != nil {
			return sent, skipped, fmt.Errorf("ingest %s/%s @ T=%d: %w",
				row.Hostname, row.ChartContext, row.RelativeTime, err)
		}
		sent++
	}
	return sent, skipped, nil
}

// parquetPath assembles {dataDir}/{kd}/{test}_run{N}.parquet — same as the
// main replay CLI.
func parquetPath(dataDir, kd, test string, run int) string {
	return filepath.Join(dataDir, kd, fmt.Sprintf("%s_run%d.parquet", test, run))
}

// sortEdges keeps every per-KD snapshot in the same order (by PropositionID)
// so output formatters can iterate index-aligned across columns.
func sortEdges(edges []*types.EdgeDescriptor) {
	sort.Slice(edges, func(i, j int) bool {
		// P1, P2, …, P15 — natural sort on the numeric tail.
		return propIDLess(edges[i].PropositionID, edges[j].PropositionID)
	})
}

// propIDLess compares "P1" < "P2" < "P10" < "P15" by numeric tail. Falls back
// to lexicographic for any non-conforming ID (e.g. proposer-discovered
// candidates with prefixes other than "P").
func propIDLess(a, b string) bool {
	na, oka := propIDNum(a)
	nb, okb := propIDNum(b)
	if oka && okb {
		return na < nb
	}
	return a < b
}

func propIDNum(s string) (int, bool) {
	if len(s) < 2 || s[0] != 'P' {
		return 0, false
	}
	n, err := strconv.Atoi(s[1:])
	if err != nil {
		return 0, false
	}
	return n, true
}
