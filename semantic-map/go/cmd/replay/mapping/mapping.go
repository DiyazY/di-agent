// Package mapping converts Netdata parquet rows into the
// types.MetricSample shape expected by the semantic-map daemon.
//
// The mapping table below was derived by probing one parquet from each KD
// (k0s/idle, k0s/cp_heavy_12client, kubeEdge/idle) against
// multidimensional-analysis/data/raw/. The triples we keep are precisely
// those that:
//
//  1. exist on all five KDs (k0s, k3s, k8s, kubeEdge, openYurt) so the
//     replay narrative is cross-KD;
//  2. translate to one of the pkg/types.MetricType constants the Bridge
//     already routes (CPUUtilization, MemoryUtilization, NetworkRxBps,
//     NetworkTxBps);
//  3. normalize cleanly to the unit declared in pkg/types/types.go's
//     MetricType catalogue.
//
// Observed schema (cross-KD, idle_run1):
//
//	chart_context     metric_id     units         notes
//	────────────────  ────────────  ────────────  ───────────────────────────────
//	system.cpu        idle          percentage    inverted: util = 1 - idle/100
//	system.ram        used          MiB           host-known total: master=64G,
//	                                              node_*=8G (RPi4)
//	system.net        InOctets      kilobits/s    Netdata reports kilobits/s;
//	                                              wire unit is bytes/s; *125
//	system.net        OutOctets     kilobits/s    signed-negative in source for
//	                                              outbound; abs() before *125
//
// Many richer signals exist in the parquets (cpu.cpu per-core, system.io,
// mem.available, k8s.cgroup.*, …) but none map to a v1 MetricType without
// host-aware normalization or k0s-only availability. Future versions can
// extend the table without touching the playback runner.
package mapping

import (
	"strings"

	"github.com/DiyazY/di-agent/pkg/types"
)

// Mapping is the result of mapping one parquet row. Value is the normalized
// observation in the unit declared for the MetricType. When the row does
// not match any known triple, Ok is false and the other fields are zero.
type Mapping struct {
	MetricType types.MetricType
	Value      float64
	Ok         bool
}

// hostRAMMiB returns the assumed total physical memory for the given
// hostname, in MiB. The mapping reflects the experimental testbed described
// in CLAUDE.md / publications:
//
//   - master = Intel NUC (64 GiB DDR4) → 65536 MiB
//   - node_1 / node_2 / node_3 = Raspberry Pi 4 (8 GiB SDRAM) → 8192 MiB
//
// Unknown hostnames fall through to 8192 MiB (conservative). This is the
// best per-row normalizer available given a single value column; computing
// used/(used+free+cached+buffers) on the fly would require multi-row
// buffering and is left to a future revision.
func hostRAMMiB(hostname string) float64 {
	switch hostname {
	case "master":
		return 65536.0
	case "node_1", "node_2", "node_3":
		return 8192.0
	}
	// Heuristic for anything else: pi-class.
	if strings.HasPrefix(hostname, "node") {
		return 8192.0
	}
	return 8192.0
}

// FromRow maps one parquet row (identified by its chart_context + metric_id
// + numeric value + hostname) to a normalized MetricSample value.
//
// Returns Ok=false if the row does not match any known triple — callers
// should silently skip those rows (the parquets contain hundreds of
// chart_contexts beyond the v1 mappings, e.g. Netdata's own internal
// monitoring under netdata.workers.*).
func FromRow(chartContext, metricID, units, hostname string, value float64) Mapping {
	switch {
	case chartContext == "system.cpu" && metricID == "idle" && units == "percentage":
		// CPU utilization = 1.0 - idle/100. Clip to [0, 1] in case of
		// pathological values from the source.
		util := 1.0 - value/100.0
		if util < 0 {
			util = 0
		}
		if util > 1 {
			util = 1
		}
		return Mapping{MetricType: types.CPUUtilization, Value: util, Ok: true}

	case chartContext == "system.ram" && metricID == "used" && units == "MiB":
		// Memory utilization = used / known-host-total. Hosts that are
		// not in the testbed fall back to the conservative pi-class total
		// (see hostRAMMiB). Clip to [0, 1] for safety.
		total := hostRAMMiB(hostname)
		if total <= 0 {
			return Mapping{}
		}
		util := value / total
		if util < 0 {
			util = 0
		}
		if util > 1 {
			util = 1
		}
		return Mapping{MetricType: types.MemoryUtilization, Value: util, Ok: true}

	case chartContext == "system.net" && metricID == "InOctets" && units == "kilobits/s":
		// Netdata reports kilobits/s; convert to bytes/s (*125), then
		// normalize to [0,1] using 1 Gbps (RPi4 NIC capacity) as the
		// reference. Without normalization the EMA grows to tens of thousands,
		// which inverts latency cost in the Reasoner via negative-direction
		// edges (P13: CO→PS direction=−).
		const refBps = 125_000_000.0 // 1 Gbps in bytes/s
		v := value * 125.0 / refBps
		if v < 0 {
			v = 0
		}
		if v > 1 {
			v = 1
		}
		return Mapping{MetricType: types.NetworkRxBps, Value: v, Ok: true}

	case chartContext == "system.net" && metricID == "OutOctets" && units == "kilobits/s":
		// Outbound is reported signed-negative in Netdata's system.net chart.
		// Take the magnitude, convert, and normalize by the same 1 Gbps ref.
		const refBps = 125_000_000.0
		v := value
		if v < 0 {
			v = -v
		}
		v = v * 125.0 / refBps
		if v > 1 {
			v = 1
		}
		return Mapping{MetricType: types.NetworkTxBps, Value: v, Ok: true}

	case chartContext == "system.cpu_some_pressure" && metricID == "some 10" && units == "percentage":
		// PSI: the percentage of the last 10 s during which at least one task was
		// stalled waiting for CPU. /100 puts it on the [0,1] scale every other
		// route uses.
		//
		// The "some 10" window is the right one of the three Netdata reports.
		// "some 60" and "some 300" average over intervals longer than most of the
		// state transitions these workloads produce, so they would smooth away the
		// variation an evidence layer exists to track.
		return Mapping{MetricType: types.CPUPressureRatio, Value: clip01(value / 100.0), Ok: true}

	case chartContext == "system.io_some_pressure" && metricID == "some 10" && units == "percentage":
		return Mapping{MetricType: types.IOPressureRatio, Value: clip01(value / 100.0), Ok: true}
	}
	return Mapping{}
}

// clip01 confines a normalized value to the domain edge weights live on. Every
// mapping needs this: a value outside [0,1] is clipped by the divergence measure
// downstream, at which point the affected edge stops responding to evidence while
// aggregate curves stay smooth.
func clip01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
