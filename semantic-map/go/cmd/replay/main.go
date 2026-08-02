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
// All subcommands speak HTTP: the binary builds standalone (no daemon dependency
// at build time) and imports the wire DTOs in cmd/replay/client.
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
	"github.com/DiyazY/di-agent/cmd/replay/parquet"
	"github.com/DiyazY/di-agent/cmd/replay/playback"
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

Examples:
  replay run --kd k0s --test idle --run 1 --speed 60
  replay run --kd kubeEdge --test cp_heavy_12client --run 3 --speed 0
  replay all --kd k0s --speed 0
  replay probe --kd k0s --test idle --run 1 | head -40
  replay list

Flags (per subcommand):
  --kd            k0s | k3s | k8s | kubeEdge | openYurt
  --test          idle | cp_light_1client | cp_heavy_8client | cp_heavy_12client |
                  dp_redis_density | reliability-control | reliability-control-no-pressure-long |
                  reliability-worker | reliability-worker-no-pressure-long
  --run           1..5
  --speed         0 = max throughput, 1.0 = real-time, N = N× faster (default 1.0)
  --addr          daemon address (default http://localhost:8080)
  --data-dir      root of {kd}/{test}_runN.parquet (default: multidimensional-analysis/data/raw,
                  resolved by walking up from cwd)
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
