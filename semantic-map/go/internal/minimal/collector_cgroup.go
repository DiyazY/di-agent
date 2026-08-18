package minimal

import (
	"bufio"
	"crypto/sha256"
	"fmt"
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
// CPU metrics (cpu_utilization, cpu_throttle_ratio) require two consecutive
// Collect() calls to establish a measurement window. The first call stores a
// snapshot and returns only memory_utilization (which is instantaneous). All
// three metrics are available from the second call onward.
//
// Cgroups v1 is not supported — Ubuntu 22.04 on RPi4 uses v2 by default.
// If cgroupRoot does not exist or files are unreadable, Collect() returns an
// empty slice rather than an error (transient unavailability per contract).
type CgroupCollector struct {
	nodeID     string
	cgroupRoot string
	sid        string  // stable source identifier
	numCPU     float64 // logical CPUs on this node

	mu   sync.Mutex
	prev *cpuSnapshot // nil until first successful read
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

// NewCgroupCollector creates a collector reading from cgroupRoot.
//
//	Production:  NewCgroupCollector("node_1", "/sys/fs/cgroup")
//	Testing:     NewCgroupCollector("test-node", t.TempDir()) — populate with fake files
func NewCgroupCollector(nodeID, cgroupRoot string) *CgroupCollector {
	return &CgroupCollector{
		nodeID:     nodeID,
		cgroupRoot: cgroupRoot,
		sid:        "cgroup:" + nodeID,
		numCPU:     float64(runtime.NumCPU()),
	}
}

func (c *CgroupCollector) SourceID() string                     { return c.sid }
func (c *CgroupCollector) AvailableMetrics() []types.MetricType { return cgroupAvailMetrics }

// Collect reads one batch of normalized metric samples from cgroups v2.
// Returns an empty slice (not an error) if cgroup files are unreadable or
// if this is the first call and no delta can yet be computed.
func (c *CgroupCollector) Collect() ([]*types.MetricSample, error) {
	now := time.Now()

	cpu, err := readCPUStat(filepath.Join(c.cgroupRoot, "cpu.stat"))
	if err != nil {
		return nil, nil // transient — cgroup not yet mounted or permission denied
	}
	memCurrent, memMax, err := readMemoryStat(c.cgroupRoot)
	if err != nil {
		return nil, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	prev := c.prev
	c.prev = &cpuSnapshot{
		ts:          now,
		usageUsec:   cpu.usageUsec,
		nrPeriods:   cpu.nrPeriods,
		nrThrottled: cpu.nrThrottled,
	}

	// Always include instantaneous memory utilization.
	var samples []*types.MetricSample
	if memMax > 0 {
		ratio := clamp(float64(memCurrent)/float64(memMax), 0, 1)
		samples = append(samples, c.sample(types.MemoryUtilization, ratio, now, now))
	}

	if prev == nil {
		// First call — no delta yet; return only memory.
		return samples, nil
	}

	elapsedUs := now.Sub(prev.ts).Microseconds()
	if elapsedUs < 1000 {
		// Delta window < 1 ms — too small for meaningful CPU rates.
		return samples, nil
	}

	// CPU utilization: fraction of available CPU time consumed.
	//   delta_usage_usec / (elapsed_us * num_cpus)
	if cpu.usageUsec >= prev.usageUsec {
		cpuUtil := clamp(
			float64(cpu.usageUsec-prev.usageUsec)/(float64(elapsedUs)*c.numCPU),
			0, 1,
		)
		samples = append(samples, c.sample(types.CPUUtilization, cpuUtil, now, prev.ts))
	}

	// CPU throttle ratio: fraction of scheduling periods that were throttled.
	//   delta_nr_throttled / delta_nr_periods
	if cpu.nrPeriods > prev.nrPeriods {
		throttle := clamp(
			float64(cpu.nrThrottled-prev.nrThrottled)/float64(cpu.nrPeriods-prev.nrPeriods),
			0, 1,
		)
		samples = append(samples, c.sample(types.CPUThrottleRatio, throttle, now, prev.ts))
	}

	return samples, nil
}

// sample builds a MetricSample with a deterministic event_id.
// anchorTs identifies the start of the measurement window (prev.ts for delta
// metrics; now for instantaneous). Two observations with the same anchor →
// same event_id, enabling Updater idempotency across replayed telemetry.
func (c *CgroupCollector) sample(mt types.MetricType, value float64, ts, anchorTs time.Time) *types.MetricSample {
	key := fmt.Sprintf("%s:%s:%s:%d", c.sid, c.nodeID, string(mt), anchorTs.Unix())
	h := sha256.Sum256([]byte(key))
	eid := fmt.Sprintf("%x", h[:8])

	return &types.MetricSample{
		NodeID:        c.nodeID,
		MetricType:    mt,
		Value:         value,
		TimestampUnix: ts.Unix(),
		EventID:       eid,
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

// readMemoryStat returns (current bytes, max bytes, error).
// If memory.max contains "max" (no limit), max is returned as 0 — callers
// should skip the utilization ratio rather than divide by zero.
func readMemoryStat(cgroupRoot string) (current, max uint64, err error) {
	cur, err := os.ReadFile(filepath.Join(cgroupRoot, "memory.current"))
	if err != nil {
		return 0, 0, err
	}
	current, err = strconv.ParseUint(strings.TrimSpace(string(cur)), 10, 64)
	if err != nil {
		return 0, 0, err
	}

	lim, err := os.ReadFile(filepath.Join(cgroupRoot, "memory.max"))
	if err != nil {
		return 0, 0, err
	}
	limStr := strings.TrimSpace(string(lim))
	if limStr == "max" {
		return current, 0, nil // no limit configured
	}
	max, err = strconv.ParseUint(limStr, 10, 64)
	return current, max, err
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
