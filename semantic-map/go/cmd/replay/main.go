// Command replay drives Netdata parquet datasets into the semantic-map
// daemon via its /ingest-sample HTTP endpoint.
//
// Subcommands:
//
//	replay run     --kd K --test T --run N [--speed S] [--addr URL] [--data-dir D] [--node-filter h1,h2]
//	               Replay one parquet (one KD × one test type × one run).
//
//	replay all     --kd K [--speed S] [--addr URL] [--data-dir D] [--node-filter h1,h2]
//	               Replay every test × run for a given KD, sequentially.
//
//	replay probe   --kd K --test T --run N [--data-dir D]
//	               Print unique (chart_context, metric_id, units, sample value)
//	               triples in one parquet — schema-discovery aide.
//
//	replay list    [--data-dir D]
//	               Inventory of every {kd}/{test}_runN.parquet under data-dir.
//
//	replay compare --test T [--run N | --runs-all] [--kds K1,K2,...] [--data-dir D]
//	               [--priors PATH] [--json|--csv] [--node-filter h1,h2]
//	               DEBUG / INSPECTION side-tool. Replays one (or all 5) runs
//	               of T for several KDs in-process and prints a per-edge ×
//	               per-KD inspection table. Use for spotting mapping bugs
//	               and verifying mechanism consistency across recorded
//	               inputs. NOT a production-decision artifact: the parquets
//	               are synthetic benchmark loads, so any "divergence" the
//	               table flags reflects the recorded test harness, not
//	               natural deployment behavior.
//
// All subcommands except `compare` speak HTTP. `compare` runs in-process
// (per-KD inspection needs isolated SemanticMaps; see cmd/replay/compare
// for the rationale). The binary builds standalone
// (no daemon dependency at build time) and imports the wire DTOs in
// cmd/replay/client plus the in-process Build+IngestSample path used by
// compare via pkg/profiles + pkg/semmap.
//
// Default --data-dir is multidimensional-analysis/data/raw resolved by
// walking up from the current working directory until found. Default
// --addr is http://localhost:8080 (the daemon's default).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/DiyazY/di-agent/cmd/replay/client"
	"github.com/DiyazY/di-agent/cmd/replay/compare"
	"github.com/DiyazY/di-agent/cmd/replay/parquet"
	"github.com/DiyazY/di-agent/cmd/replay/playback"
	"github.com/DiyazY/di-agent/pkg/domain"
)

const (
	defaultAddr    = "http://localhost:8080"
	defaultDataDir = "multidimensional-analysis/data/raw"
)

// kds, testTypes are the canonical inventories — used by `list` and
// `all`, and to validate flags.
var (
	kds = []string{"k0s", "k3s", "k8s", "kubeEdge", "openYurt"}

	testTypes = []string{
		"idle",
		"cp_light_1client",
		"cp_heavy_8client",
		"cp_heavy_12client",
		"dp_redis_density",
		"reliability-control",
		"reliability-control-no-pressure-long",
		"reliability-worker",
		"reliability-worker-no-pressure-long",
	}
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var err error
	switch cmd {
	case "run":
		err = cmdRun(ctx, args)
	case "all":
		err = cmdAll(ctx, args)
	case "probe":
		err = cmdProbe(args)
	case "list":
		err = cmdList(args)
	case "compare":
		err = cmdCompare(args)
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `replay — drive Netdata parquets into the semantic-map daemon.

Usage:
  replay run     --kd K --test T --run N [--speed S] [--addr URL] [--data-dir D] [--node-filter h1,h2]
  replay all     --kd K [--speed S] [--addr URL] [--data-dir D] [--node-filter h1,h2]
  replay probe   --kd K --test T --run N [--data-dir D]
  replay list    [--data-dir D]
  replay compare --test T [--run N | --runs-all] [--kds K1,K2,...] [--data-dir D]
                 [--priors PATH] [--json|--csv] [--node-filter h1,h2]
                 (DEBUG / INSPECTION TOOL — not a production-decision artifact;
                  see cmd/replay/compare for the role boundary)

Examples:
  replay run --kd k0s --test idle --run 1 --speed 60
  replay run --kd kubeEdge --test cp_heavy_12client --run 3 --speed 0
  replay all --kd k0s --speed 0
  replay probe --kd k0s --test idle --run 1 | head -40
  replay list
  replay compare --test idle --run 1                       # inspection table
  replay compare --test idle --runs-all --json | jq '.divergence[0:3]'
  replay compare --test cp_heavy_12client --kds k0s,k3s,k8s --csv > compare.csv

Flags (per subcommand):
  --kd            k0s | k3s | k8s | kubeEdge | openYurt
  --kds           comma-separated KDs for compare (default: all five)
  --test          idle | cp_light_1client | cp_heavy_8client | cp_heavy_12client |
                  dp_redis_density | reliability-control | reliability-control-no-pressure-long |
                  reliability-worker | reliability-worker-no-pressure-long
  --run           1..5
  --runs-all      compare only: replay all 5 runs per KD and average snapshots
  --speed         0 = max throughput, 1.0 = real-time, N = N× faster (default 1.0)
  --addr          daemon address (default http://localhost:8080); ignored by compare
  --data-dir      root of {kd}/{test}_runN.parquet (default: multidimensional-analysis/data/raw,
                  resolved by walking up from cwd)
  --priors        path to prior_weights.json (compare only; resolved by walking up from cwd
                  if omitted; comparison falls back to Di-Select defaults when not found)
  --json          compare only: emit JSON instead of table
  --csv           compare only: emit CSV (long format, one row per KD × edge)
  --node-filter   comma-separated hostnames to keep (e.g. master,node_1)
`)
}

// runFlags parses the shared flag set used by run/all/probe.
type runFlags struct {
	kd         string
	test       string
	run        int
	speed      float64
	addr       string
	dataDir    string
	nodeFilter string
}

func parseRunFlags(args []string, defaults runFlags) (runFlags, []string, error) {
	f := defaults
	leftover := []string{}
	i := 0
	for i < len(args) {
		a := args[i]
		consume := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s requires a value", a)
			}
			i++
			return args[i], nil
		}
		switch a {
		case "--kd":
			v, err := consume()
			if err != nil {
				return f, nil, err
			}
			f.kd = v
		case "--test":
			v, err := consume()
			if err != nil {
				return f, nil, err
			}
			f.test = v
		case "--run":
			v, err := consume()
			if err != nil {
				return f, nil, err
			}
			n := 0
			if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
				return f, nil, fmt.Errorf("--run %q: not an int", v)
			}
			f.run = n
		case "--speed":
			v, err := consume()
			if err != nil {
				return f, nil, err
			}
			s := 0.0
			if _, err := fmt.Sscanf(v, "%f", &s); err != nil {
				return f, nil, fmt.Errorf("--speed %q: not a float", v)
			}
			f.speed = s
		case "--addr":
			v, err := consume()
			if err != nil {
				return f, nil, err
			}
			f.addr = v
		case "--data-dir":
			v, err := consume()
			if err != nil {
				return f, nil, err
			}
			f.dataDir = v
		case "--node-filter":
			v, err := consume()
			if err != nil {
				return f, nil, err
			}
			f.nodeFilter = v
		default:
			leftover = append(leftover, a)
		}
		i++
	}
	return f, leftover, nil
}

func validateKD(kd string) error {
	for _, v := range kds {
		if v == kd {
			return nil
		}
	}
	return fmt.Errorf("--kd %q: must be one of %s", kd, strings.Join(kds, ", "))
}

func validateTest(t string) error {
	for _, v := range testTypes {
		if v == t {
			return nil
		}
	}
	return fmt.Errorf("--test %q: must be one of %s", t, strings.Join(testTypes, ", "))
}

// resolveDataDir returns the absolute path to the data root. If override is
// non-empty it is used verbatim. Otherwise we walk up from cwd looking for
// the default subpath.
func resolveDataDir(override string) (string, error) {
	if override != "" {
		abs, err := filepath.Abs(override)
		if err != nil {
			return "", err
		}
		if info, err := os.Stat(abs); err != nil || !info.IsDir() {
			return "", fmt.Errorf("data dir %s does not exist", abs)
		}
		return abs, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	d := cwd
	for {
		cand := filepath.Join(d, defaultDataDir)
		if info, err := os.Stat(cand); err == nil && info.IsDir() {
			return cand, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", fmt.Errorf("could not find %s by walking up from %s", defaultDataDir, cwd)
		}
		d = parent
	}
}

// parquetPath assembles {dataDir}/{kd}/{test}_run{N}.parquet.
func parquetPath(dataDir, kd, test string, run int) string {
	return filepath.Join(dataDir, kd, fmt.Sprintf("%s_run%d.parquet", test, run))
}

func parseHostFilter(s string) map[string]struct{} {
	if s == "" {
		return nil
	}
	out := map[string]struct{}{}
	for _, h := range strings.Split(s, ",") {
		h = strings.TrimSpace(h)
		if h != "" {
			out[h] = struct{}{}
		}
	}
	return out
}

// cmdRun replays one parquet.
func cmdRun(ctx context.Context, args []string) error {
	f, _, err := parseRunFlags(args, runFlags{speed: 1.0, addr: defaultAddr})
	if err != nil {
		return err
	}
	if err := validateKD(f.kd); err != nil {
		return err
	}
	if err := validateTest(f.test); err != nil {
		return err
	}
	if f.run < 1 || f.run > 5 {
		return fmt.Errorf("--run must be 1..5; got %d", f.run)
	}
	dataDir, err := resolveDataDir(f.dataDir)
	if err != nil {
		return err
	}
	path := parquetPath(dataDir, f.kd, f.test, f.run)
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("parquet not found: %s", path)
	}
	return runOne(ctx, f.addr, path, f.speed, parseHostFilter(f.nodeFilter))
}

// cmdAll replays every test × run for one KD.
func cmdAll(ctx context.Context, args []string) error {
	f, _, err := parseRunFlags(args, runFlags{speed: 0, addr: defaultAddr})
	if err != nil {
		return err
	}
	if err := validateKD(f.kd); err != nil {
		return err
	}
	dataDir, err := resolveDataDir(f.dataDir)
	if err != nil {
		return err
	}
	hostFilter := parseHostFilter(f.nodeFilter)
	for _, t := range testTypes {
		for r := 1; r <= 5; r++ {
			p := parquetPath(dataDir, f.kd, t, r)
			if _, err := os.Stat(p); err != nil {
				fmt.Printf("  skip: %s (missing)\n", filepath.Base(p))
				continue
			}
			if err := runOne(ctx, f.addr, p, f.speed, hostFilter); err != nil {
				return err
			}
		}
	}
	return nil
}

// runOne is the shared body of cmdRun and cmdAll: open a sender, kick
// playback, print the header + per-tick progress, and a one-line summary.
func runOne(ctx context.Context, addr, path string, speed float64, hostFilter map[string]struct{}) error {
	sender := client.New(addr)

	r, err := parquet.Open(path)
	if err != nil {
		return err
	}
	totalRows := r.NumRows()
	r.Close()

	hostNote := ""
	if hostFilter != nil {
		hs := make([]string, 0, len(hostFilter))
		for h := range hostFilter {
			hs = append(hs, h)
		}
		sort.Strings(hs)
		hostNote = " hosts=" + strings.Join(hs, ",")
	}
	speedNote := fmt.Sprintf("%.1fx", speed)
	if speed == 0 {
		speedNote = "max"
	}
	fmt.Printf("replay  %s  rows=%d%s  speed=%s\n",
		filepath.Base(path), totalRows, hostNote, speedNote)

	progress := func(ev playback.ProgressEvent) {
		fmt.Printf("  T=%4ds  -> %3d sent  (%d skipped)\n",
			ev.Tick, ev.SamplesSent, ev.SamplesSkipped)
	}

	summary, err := playback.Run(ctx, sender, playback.Config{
		ParquetPath: path,
		Speed:       speed,
		HostFilter:  hostFilter,
		Progress:    progress,
	})
	if summary != nil {
		fmt.Printf("done   %s\n\n", summary)
	}
	return err
}

// cmdProbe prints unique (chart_context, metric_id, units, sample value)
// triples found in the parquet. Useful for confirming what's available
// before extending the mapping table.
func cmdProbe(args []string) error {
	f, _, err := parseRunFlags(args, runFlags{})
	if err != nil {
		return err
	}
	if err := validateKD(f.kd); err != nil {
		return err
	}
	if err := validateTest(f.test); err != nil {
		return err
	}
	if f.run < 1 || f.run > 5 {
		return fmt.Errorf("--run must be 1..5; got %d", f.run)
	}
	dataDir, err := resolveDataDir(f.dataDir)
	if err != nil {
		return err
	}
	path := parquetPath(dataDir, f.kd, f.test, f.run)
	r, err := parquet.Open(path)
	if err != nil {
		return err
	}
	defer r.Close()

	type triple struct {
		ctx, mid, units string
	}
	counts := map[triple]int{}
	first := map[triple]float64{}
	for {
		row, err := r.Next()
		if err != nil {
			break
		}
		k := triple{row.ChartContext, row.MetricID, row.Units}
		if _, ok := first[k]; !ok {
			first[k] = row.Value
		}
		counts[k]++
	}
	type entry struct {
		t triple
		n int
		v float64
	}
	entries := make([]entry, 0, len(counts))
	for t, n := range counts {
		entries = append(entries, entry{t, n, first[t]})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].n > entries[j].n })

	fmt.Printf("# %s — %d unique (chart_context, metric_id, units) triples\n",
		filepath.Base(path), len(entries))
	fmt.Printf("# %-9s  %-40s  %-30s  %-15s  %s\n",
		"row_count", "chart_context", "metric_id", "units", "sample_value")
	for _, e := range entries {
		fmt.Printf("  %-9d  %-40s  %-30s  %-15s  %g\n",
			e.n, e.t.ctx, e.t.mid, e.t.units, e.v)
	}
	return nil
}

// cmdList inventories the data directory.
func cmdList(args []string) error {
	f, _, err := parseRunFlags(args, runFlags{})
	if err != nil {
		return err
	}
	dataDir, err := resolveDataDir(f.dataDir)
	if err != nil {
		return err
	}
	fmt.Printf("# data dir: %s\n", dataDir)
	for _, kd := range kds {
		var found []string
		kdDir := filepath.Join(dataDir, kd)
		entries, err := os.ReadDir(kdDir)
		if err != nil {
			fmt.Printf("  %s: (not found)\n", kd)
			continue
		}
		for _, ent := range entries {
			if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".parquet") {
				continue
			}
			found = append(found, ent.Name())
		}
		sort.Strings(found)
		fmt.Printf("  %s: %d parquets\n", kd, len(found))
		for _, name := range found {
			fmt.Printf("    %s\n", name)
		}
	}
	return nil
}

// ── compare ─────────────────────────────────────────────────────────────────

// compareFlags is the parsed shape of `replay compare ...`.
type compareFlags struct {
	test       string
	run        int
	runsAll    bool
	kds        string
	dataDir    string
	priors     string
	nodeFilter string
	asJSON     bool
	asCSV      bool
}

func parseCompareFlags(args []string) (compareFlags, error) {
	f := compareFlags{run: 1}
	i := 0
	for i < len(args) {
		a := args[i]
		consume := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s requires a value", a)
			}
			i++
			return args[i], nil
		}
		switch a {
		case "--test":
			v, err := consume()
			if err != nil {
				return f, err
			}
			f.test = v
		case "--run":
			v, err := consume()
			if err != nil {
				return f, err
			}
			n := 0
			if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
				return f, fmt.Errorf("--run %q: not an int", v)
			}
			f.run = n
		case "--runs-all":
			f.runsAll = true
		case "--kds":
			v, err := consume()
			if err != nil {
				return f, err
			}
			f.kds = v
		case "--data-dir":
			v, err := consume()
			if err != nil {
				return f, err
			}
			f.dataDir = v
		case "--priors":
			v, err := consume()
			if err != nil {
				return f, err
			}
			f.priors = v
		case "--node-filter":
			v, err := consume()
			if err != nil {
				return f, err
			}
			f.nodeFilter = v
		case "--json":
			f.asJSON = true
		case "--csv":
			f.asCSV = true
		case "--addr":
			// Accept and ignore --addr so dev.sh's auto-injected flag
			// doesn't break compare (which is in-process).
			if _, err := consume(); err != nil {
				return f, err
			}
		default:
			return f, fmt.Errorf("unknown flag for compare: %s", a)
		}
		i++
	}
	return f, nil
}

// resolvePriorsPath returns an absolute path to prior_weights.json. If
// override is set, it's used verbatim. Otherwise we walk up from cwd
// looking for `semantic-map/prior_weights.json` or `prior_weights.json`.
// Returns "" with no error if nothing was found — compare's caller will
// warn and fall back to uncalibrated defaults.
func resolvePriorsPath(override string) (string, error) {
	if override != "" {
		abs, err := filepath.Abs(override)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(abs); err != nil {
			return "", fmt.Errorf("priors file %s does not exist", abs)
		}
		return abs, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	d := cwd
	for i := 0; i < 8; i++ {
		for _, sub := range []string{"prior_weights.json", "semantic-map/prior_weights.json"} {
			cand := filepath.Join(d, sub)
			if _, err := os.Stat(cand); err == nil {
				return cand, nil
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return "", nil
}

// cmdCompare drives replay/compare.Run() and emits the requested format.
func cmdCompare(args []string) error {
	f, err := parseCompareFlags(args)
	if err != nil {
		return err
	}
	if f.test == "" {
		return fmt.Errorf("--test is required")
	}
	if err := validateTest(f.test); err != nil {
		return err
	}
	if f.asJSON && f.asCSV {
		return fmt.Errorf("--json and --csv are mutually exclusive")
	}
	if f.runsAll {
		f.run = 0
	}
	if !f.runsAll && (f.run < 1 || f.run > 5) {
		return fmt.Errorf("--run must be 1..5; got %d", f.run)
	}

	// Resolve --kds (default = all five).
	selectedKDs := append([]string(nil), kds...)
	if strings.TrimSpace(f.kds) != "" {
		selectedKDs = nil
		for _, k := range strings.Split(f.kds, ",") {
			k = strings.TrimSpace(k)
			if k == "" {
				continue
			}
			if err := validateKD(k); err != nil {
				return err
			}
			selectedKDs = append(selectedKDs, k)
		}
		if len(selectedKDs) == 0 {
			return fmt.Errorf("--kds must list at least one KD")
		}
	}

	dataDir, err := resolveDataDir(f.dataDir)
	if err != nil {
		return err
	}
	priors, err := resolvePriorsPath(f.priors)
	if err != nil {
		return err
	}
	if priors == "" {
		fmt.Fprintln(os.Stderr,
			"warning: prior_weights.json not found — compare will use uncalibrated Di-Select defaults; cross-KD divergence will be data-driven only.")
	}

	var nodeFilter []string
	if s := strings.TrimSpace(f.nodeFilter); s != "" {
		for _, h := range strings.Split(s, ",") {
			h = strings.TrimSpace(h)
			if h != "" {
				nodeFilter = append(nodeFilter, h)
			}
		}
	}

	spec, err := domain.LoadFound()
	if err != nil {
		return fmt.Errorf("domain spec: %w (pass -domain, or run from within the repo)", err)
	}
	opts := compare.Options{
		DomainSpec:       spec,
		DataDir:          dataDir,
		TestType:         f.test,
		Run:              f.run,
		KDs:              selectedKDs,
		PriorWeightsPath: priors,
		NodeFilter:       nodeFilter,
	}
	result, err := compare.Run(opts)
	if err != nil {
		return err
	}

	switch {
	case f.asJSON:
		return compare.FormatJSON(os.Stdout, result)
	case f.asCSV:
		return compare.FormatCSV(os.Stdout, result)
	default:
		return compare.FormatTable(os.Stdout, result)
	}
}
