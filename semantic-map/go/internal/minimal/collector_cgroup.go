package minimal

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DiyazY/di-agent/pkg/types"
)

// CgroupCollector is the edge-minimal CollectorContract implementation.
// It reads cgroups v2 files directly from cgroupRoot (/sys/fs/cgroup in
// production), requiring no external monitoring daemon. Designed for RPi4
// and similarly constrained nodes.
//
// Beyond the node-level aggregate at the root, the collector can walk the tree
// and read the same three files per subject: a pod cgroup (systemd or cgroupfs
// driver, recognised by recogniseKubepods) and, when allowlisted by glob, a
// direct child of system.slice (recognised by recogniseUnits). A container
// scope below a pod is never a subject — the pod is the unit of workload and
// its cgroup survives container restarts, so the walk collects the pod's
// directory and does not descend into it. The walk is depth-limited and
// capped at CgroupOptions.MaxSubjects; beyond the cap the collector logs once
// and skips the rest.
//
// CPU and memory samples — root or subject — are declared as
// "share-of-node-capacity" with Range [0,1]: a fraction of what the whole node
// has, never a fraction of a subject-local limit. When the node's capacity is
// unknown (no MemTotal), memory is not reported at all rather than as a share of
// the subject's own memory.max, which is a different quantity. cpu_throttle_ratio
// is throttled periods over elapsed periods and is declared as "share-of-periods"
// on [0,1]. The node's memory share is the one reading that does not
// come from a cgroup file: a cgroup v2 root has no memory.current / memory.max, so
// it is (MemTotal − MemAvailable) / MemTotal from /proc/meminfo. A subject that has vanished between one Collect() and
// the next simply produces no samples this tick; its cgroup snapshot is
// dropped so a later reappearance (a reused pod UID, say) starts clean. There
// is no "gone" event — absence is silence, not a signal.
//
// CPU metrics (cpu_utilization, cpu_throttle_ratio) require two consecutive
// Collect() calls per subject to establish a measurement window. The first
// call a subject is seen, it contributes only memory_utilization (which is
// instantaneous); CPU samples follow from the second call onward.
//
// Cgroups v1 is not supported — Ubuntu 22.04 on RPi4 uses v2 by default.
// If a directory's files are unreadable, that read is skipped rather than
// treated as an error (transient unavailability per contract).
type CgroupCollector struct {
	nodeID     string
	cgroupRoot string
	sid        string  // stable source identifier
	numCPU     float64 // logical CPUs on this node
	memTotal   uint64  // node memory capacity in bytes; 0 if unknown
	opts       CgroupOptions
	recognise  []recogniser

	mu   sync.Mutex
	prev map[string]*cpuSnapshot // key: subject ("" = root)
	// Each condition below is said once (or, for the cap, whenever the count
	// changes): a collector that goes silent is the failure this project treats as
	// worse than an error, because nothing about an empty map looks wrong.
	rootWarned   bool
	noPodsWarned bool
	lastSkipped  int
}

func (c *CgroupCollector) log(format string, args ...any) {
	if c.opts.Logf != nil {
		c.opts.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// CgroupOptions configures subject detection. The zero value walks nothing; the
// daemon's defaults are Subjects on with MaxSubjects 256.
type CgroupOptions struct {
	// Subjects enables the walk over pod cgroups (and allowlisted units).
	Subjects bool
	// UnitGlobs allowlists direct children of system.slice as unit:<name> subjects.
	UnitGlobs []string
	// MaxSubjects bounds the walk; beyond it the collector logs once and skips.
	MaxSubjects int
	// CmdLabel stamps cmd=<argv0> from the subject's first process. Needs /proc
	// visibility (hostPID or privileged), hence off by default.
	CmdLabel bool
	// MemTotalBytes is the node's memory capacity. 0 reads /proc/meminfo; when that
	// is unreadable too, no memory sample is emitted for any subject.
	MemTotalBytes uint64
	// ProcRoot is where /proc is mounted (tests point it at a fake tree).
	ProcRoot string
	// Logf receives the collector's own log lines: an unreadable root, a root with
	// no pod cgroups while subjects are on, and subjects skipped by the cap. nil
	// means the standard logger.
	Logf func(format string, args ...any)
}

type cpuSnapshot struct {
	ts          time.Time
	usageUsec   uint64
	nrPeriods   uint64
	nrThrottled uint64
}

var cgroupAvailMetrics = []types.MetricType{
	types.CPUUtilization,
	types.MemoryUtilization,
	types.CPUThrottleRatio,
}

// NewCgroupCollector creates a collector with the daemon defaults: subjects on,
// 256 at most, no units, no cmd label.
func NewCgroupCollector(nodeID, cgroupRoot string) *CgroupCollector {
	return NewCgroupCollectorWithOptions(nodeID, cgroupRoot, CgroupOptions{Subjects: true, MaxSubjects: 256})
}

// NewCgroupCollectorWithOptions creates a collector reading from cgroupRoot.
//
//	Production:  NewCgroupCollectorWithOptions("node_1", "/sys/fs/cgroup", opts)
//	Testing:     NewCgroupCollectorWithOptions("test-node", t.TempDir(), opts) — fake files
func NewCgroupCollectorWithOptions(nodeID, cgroupRoot string, opts CgroupOptions) *CgroupCollector {
	if opts.MaxSubjects <= 0 {
		opts.MaxSubjects = 256
	}
	if opts.ProcRoot == "" {
		opts.ProcRoot = "/proc"
	}
	c := &CgroupCollector{
		nodeID: nodeID, cgroupRoot: cgroupRoot, sid: "cgroup:" + nodeID,
		numCPU: float64(runtime.NumCPU()), opts: opts,
		prev: make(map[string]*cpuSnapshot),
	}
	c.memTotal = opts.MemTotalBytes
	if c.memTotal == 0 {
		c.memTotal = readMemTotal(filepath.Join(opts.ProcRoot, "meminfo"))
	}
	c.recognise = []recogniser{recogniseKubepods}
	if len(opts.UnitGlobs) > 0 {
		c.recognise = append(c.recognise, recogniseUnits(opts.UnitGlobs))
	}
	return c
}

func (c *CgroupCollector) SourceID() string                     { return c.sid }
func (c *CgroupCollector) AvailableMetrics() []types.MetricType { return cgroupAvailMetrics }

// Collect reads the root and, when enabled, every recognised subject, at one instant.
// A subject exists iff its directory is present, recognised, within the depth bound and
// the subject cap, and readable this tick; nothing is emitted for one that has gone, and
// its snapshot is dropped. Unreadable files are transient: skipped, not errors.
func (c *CgroupCollector) Collect() ([]*types.MetricSample, error) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()

	out, rootErr := c.collectOneLocked("", c.cgroupRoot, nil, now)
	if rootErr != nil && !c.rootWarned {
		c.log("cgroup collector: root %s is unreadable (%v): no node properties will be produced until it is readable — is a cgroup v2 hierarchy mounted at -cgroup-root?",
			c.cgroupRoot, rootErr)
		c.rootWarned = true
	}
	if !c.opts.Subjects {
		return out, nil
	}
	seen := map[string]bool{}
	root := filepath.Clean(c.cgroupRoot)
	var sawPods bool
	var skipped int
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || p == root {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(p, root+string(filepath.Separator)))
		first, _, _ := strings.Cut(rel, "/")
		unitsEnabled := len(c.opts.UnitGlobs) > 0
		if strings.HasPrefix(first, "kubepods") {
			sawPods = true
		}
		if !strings.HasPrefix(first, "kubepods") && !(first == "system.slice" && unitsEnabled) {
			return filepath.SkipDir
		}
		if strings.Count(rel, "/") > 3 {
			return filepath.SkipDir
		}
		for _, r := range c.recognise {
			info, ok := r(rel)
			if !ok {
				continue
			}
			if len(seen) >= c.opts.MaxSubjects {
				skipped++
				return filepath.SkipDir
			}
			seen[info.subject] = true
			labels := info.labels
			if c.opts.CmdLabel {
				if cmd := c.cmdLabel(p); cmd != "" {
					labels["cmd"] = cmd
				}
			}
			samples, _ := c.collectOneLocked(info.subject, p, labels, now) // unreadable: transient, skipped
			out = append(out, samples...)
			return filepath.SkipDir // a subject's children are not subjects
		}
		return nil
	})
	if !sawPods && !c.noPodsWarned {
		c.log("cgroup collector: no kubepods cgroup under %s while -cgroup-subjects is on; if the agent runs in a container this root is its own cgroup, not the node's — mount the host's cgroup root",
			c.cgroupRoot)
		c.noPodsWarned = true
	}
	if skipped != c.lastSkipped {
		if skipped > 0 {
			c.log("cgroup collector: %d subjects beyond -cgroup-max-subjects=%d skipped this tick; the census under-counts by that many",
				skipped, c.opts.MaxSubjects)
		} else {
			c.log("cgroup collector: no subjects skipped by -cgroup-max-subjects=%d any more", c.opts.MaxSubjects)
		}
		c.lastSkipped = skipped
	}
	for subject := range c.prev {
		if subject != "" && !seen[subject] {
			delete(c.prev, subject)
		}
	}
	return out, nil
}

// collectOneLocked reads one cgroup directory as one subject ("" = the node). The
// error is the cpu.stat read failing, which means no samples at all for this
// directory; the caller decides whether that is worth saying.
func (c *CgroupCollector) collectOneLocked(subject, dir string, labels map[string]string, now time.Time) ([]*types.MetricSample, error) {
	cpu, err := readCPUStat(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		return nil, err
	}
	memCurrent, memErr := readMemoryCurrent(dir)
	prev := c.prev[subject]
	c.prev[subject] = &cpuSnapshot{ts: now, usageUsec: cpu.usageUsec, nrPeriods: cpu.nrPeriods, nrThrottled: cpu.nrThrottled}

	var samples []*types.MetricSample
	switch {
	case memErr == nil && c.memTotal > 0:
		samples = append(samples, c.sample(types.MemoryUtilization, unitNodeShare, clamp(float64(memCurrent)/float64(c.memTotal), 0, 1), now, now, subject, labels))
	case memErr != nil && subject == "":
		// The root of a cgroup v2 hierarchy carries no memory.current / memory.max —
		// the kernel does not create them there — so the node's memory share has to
		// come from /proc/meminfo. Without this the one number an operator asking
		// "how much memory is this machine using" expects was never emitted at all.
		// A subject keeps using its own cgroup files; only the node falls back here,
		// and only when its cgroup cannot answer (a container as root can).
		if share, ok := readMemInUseShare(filepath.Join(c.opts.ProcRoot, "meminfo")); ok {
			samples = append(samples, c.sample(types.MemoryUtilization, unitNodeShare, clamp(share, 0, 1), now, now, subject, labels))
		}
	}
	if prev == nil {
		return samples, nil
	}
	elapsedUs := now.Sub(prev.ts).Microseconds()
	if elapsedUs < 1000 {
		return samples, nil
	}
	if cpu.usageUsec >= prev.usageUsec {
		util := clamp(float64(cpu.usageUsec-prev.usageUsec)/(float64(elapsedUs)*c.numCPU), 0, 1)
		samples = append(samples, c.sample(types.CPUUtilization, unitNodeShare, util, now, prev.ts, subject, labels))
	}
	// Both counters are guarded: a cgroup recreated under the same subject between
	// two ticks restarts them, and an unsigned delta across that wraps to a maximal,
	// plausible-looking ratio.
	if cpu.nrPeriods > prev.nrPeriods && cpu.nrThrottled >= prev.nrThrottled {
		throttle := clamp(float64(cpu.nrThrottled-prev.nrThrottled)/float64(cpu.nrPeriods-prev.nrPeriods), 0, 1)
		samples = append(samples, c.sample(types.CPUThrottleRatio, unitPeriodShare, throttle, now, prev.ts, subject, labels))
	}
	return samples, nil
}

// cmdLabel returns the basename of argv0 of the first process in the cgroup, or "".
func (c *CgroupCollector) cmdLabel(dir string) string {
	procs, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if err != nil {
		return ""
	}
	pid, _, _ := strings.Cut(strings.TrimSpace(string(procs)), "\n")
	if pid == "" {
		return ""
	}
	cmdline, err := os.ReadFile(filepath.Join(c.opts.ProcRoot, pid, "cmdline"))
	if err != nil {
		return ""
	}
	argv0, _, _ := strings.Cut(string(cmdline), "\x00")
	if argv0 == "" {
		return ""
	}
	return filepath.Base(argv0)
}

// readMemInUseShare parses MemTotal and MemAvailable from /proc/meminfo and returns
// (MemTotal − MemAvailable) / MemTotal — memory the node cannot hand to a new
// workload without reclaiming, as a share of what it has. Reports false when either
// key is missing, malformed or zero: a node memory share nobody can compute is better
// left unsaid than guessed at.
//
// This counts page cache differently from cgroup accounting — reclaimable cache is
// excluded here, whereas memory.current includes a cgroup's own page cache — so the
// node figure and the sum of its subjects' figures are not the same quantity and
// should not be expected to add up.
func readMemInUseShare(path string) (float64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	var total, available uint64
	var haveTotal, haveAvailable bool
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total, haveTotal = kb, true
		case "MemAvailable:":
			available, haveAvailable = kb, true
		}
	}
	if !haveTotal || !haveAvailable || total == 0 || available > total {
		return 0, false
	}
	return float64(total-available) / float64(total), true
}

// readMemTotal parses MemTotal from /proc/meminfo (kB). 0 when unavailable.
func readMemTotal(path string) uint64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0
			}
			return kb * 1024
		}
	}
	return 0
}

var shareRange = [2]float64{0, 1}

const (
	// unitNodeShare: a fraction of what the whole node has (CPU, memory).
	unitNodeShare = "share-of-node-capacity"
	// unitPeriodShare: throttled CFS periods over elapsed periods, a fraction of
	// the subject's own scheduling periods rather than of the node.
	unitPeriodShare = "share-of-periods"
)

// sample builds a MetricSample with a deterministic event_id over
// (source, node, subject, metric, anchor), declared in the unit the caller names
// on [0,1] — this collector's meaning, not the map's rule.
func (c *CgroupCollector) sample(mt types.MetricType, unit string, value float64, ts, anchorTs time.Time, subject string, labels map[string]string) *types.MetricSample {
	key := fmt.Sprintf("%s:%s:%s:%s:%d", c.sid, c.nodeID, subject, string(mt), anchorTs.Unix())
	h := sha256.Sum256([]byte(key))
	rng := shareRange
	return &types.MetricSample{
		NodeID: c.nodeID, MetricType: mt, Value: value, TimestampUnix: ts.Unix(),
		EventID: fmt.Sprintf("%x", h[:8]),
		Subject: subject, Unit: unit, Range: &rng, Source: c.sid, Labels: labels,
	}
}

// ── cgroup v2 file readers ────────────────────────────────────────────────────

type rawCPUStat struct {
	usageUsec   uint64
	nrPeriods   uint64
	nrThrottled uint64
}

// readCPUStat parses the cgroups v2 cpu.stat key-value file.
// Unknown keys are silently ignored — the format may include additional fields
// on newer kernels.
func readCPUStat(path string) (*rawCPUStat, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	stat := &rawCPUStat{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			continue
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "usage_usec":
			stat.usageUsec = v
		case "nr_periods":
			stat.nrPeriods = v
		case "nr_throttled":
			stat.nrThrottled = v
		}
	}
	return stat, scanner.Err()
}

// readMemoryCurrent returns the cgroup's memory.current in bytes. memory.max is not
// read: a share of a subject's own limit is not a quantity this collector reports.
func readMemoryCurrent(cgroupRoot string) (uint64, error) {
	cur, err := os.ReadFile(filepath.Join(cgroupRoot, "memory.current"))
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(cur)), 10, 64)
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
