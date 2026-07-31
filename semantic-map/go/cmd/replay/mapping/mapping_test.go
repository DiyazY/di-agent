package mapping

import (
	"math"
	"testing"

	"github.com/DiyazY/di-agent/pkg/types"
)

// TestFromRow_Table covers every triple in the v1 mapping table plus a
// handful of negative cases (unknown context, unknown metric id, wrong
// units). The test asserts both the MetricType routing and the numeric
// normalization — the latter being the actual "is this a fraction or a
// rate?" contract.
func TestFromRow_Table(t *testing.T) {
	cases := []struct {
		name  string
		ctx   string
		mid   string
		units string
		host  string
		value float64
		want  Mapping
	}{
		// ── system.cpu / idle → cpu_utilization (inverted) ────────────────
		{
			name: "cpu idle 99.5 on master -> util 0.005",
			ctx:  "system.cpu", mid: "idle", units: "percentage", host: "master", value: 99.5,
			want: Mapping{MetricType: types.CPUUtilization, Value: 0.005, Ok: true},
		},
		{
			name: "cpu idle 0 on node_1 -> util 1.0",
			ctx:  "system.cpu", mid: "idle", units: "percentage", host: "node_1", value: 0.0,
			want: Mapping{MetricType: types.CPUUtilization, Value: 1.0, Ok: true},
		},
		{
			name: "cpu idle 105 (out-of-range) clipped to util 0",
			ctx:  "system.cpu", mid: "idle", units: "percentage", host: "master", value: 105.0,
			want: Mapping{MetricType: types.CPUUtilization, Value: 0.0, Ok: true},
		},

		// ── system.ram / used → memory_utilization ───────────────────────
		{
			name: "ram used 820 MiB on master (64 GiB host) -> ~0.0125",
			ctx:  "system.ram", mid: "used", units: "MiB", host: "master", value: 820.0,
			want: Mapping{MetricType: types.MemoryUtilization, Value: 820.0 / 65536.0, Ok: true},
		},
		{
			name: "ram used 800 MiB on node_1 (8 GiB RPi4) -> ~0.0977",
			ctx:  "system.ram", mid: "used", units: "MiB", host: "node_1", value: 800.0,
			want: Mapping{MetricType: types.MemoryUtilization, Value: 800.0 / 8192.0, Ok: true},
		},

		// ── system.net / InOctets → network_rx_bps normalized to [0,1] ──────
		// Normalized by 1 Gbps = 125,000,000 bytes/s (RPi4 NIC capacity).
		// 8 kbits/s * 125 = 1000 bytes/s; 1000 / 125,000,000 = 0.000008
		{
			name: "net InOctets 8 kilobits/s -> 0.000008 (normalized to 1 Gbps ref)",
			ctx:  "system.net", mid: "InOctets", units: "kilobits/s", host: "master", value: 8.0,
			want: Mapping{MetricType: types.NetworkRxBps, Value: 8.0 * 125.0 / 125_000_000.0, Ok: true},
		},
		{
			name: "net InOctets 1000000 kilobits/s (1 Gbps) -> 1.0 (clamped)",
			ctx:  "system.net", mid: "InOctets", units: "kilobits/s", host: "master", value: 1_000_000.0,
			want: Mapping{MetricType: types.NetworkRxBps, Value: 1.0, Ok: true},
		},

		// ── PSI → PS constructs ─────────────────────────────────────────
		// Netdata reports the fraction of the window during which at least one
		// task stalled, as a percentage; /100 puts it on the shared [0,1] scale.
		{
			name: "cpu_some_pressure some 10 at 7.5% -> 0.075",
			ctx:  "system.cpu_some_pressure", mid: "some 10", units: "percentage",
			host: "node_1", value: 7.5,
			want: Mapping{MetricType: types.CPUPressureRatio, Value: 0.075, Ok: true},
		},
		{
			name: "io_some_pressure some 10 at 0% -> 0.0 (idle clusters sit here)",
			ctx:  "system.io_some_pressure", mid: "some 10", units: "percentage",
			host: "node_1", value: 0.0,
			want: Mapping{MetricType: types.IOPressureRatio, Value: 0.0, Ok: true},
		},
		{
			// The 60s and 300s windows average over intervals longer than most
			// state transitions in these workloads, so mapping them would smooth
			// away the variation the evidence layer exists to track. Unmapped is
			// deliberate, not an omission.
			name: "cpu_some_pressure some 60 is deliberately unmapped",
			ctx:  "system.cpu_some_pressure", mid: "some 60", units: "percentage",
			host: "node_1", value: 7.5,
			want: Mapping{},
		},

		// ── system.net / OutOctets → network_tx_bps (signed, abs first) ──
		{
			name: "net OutOctets -8 kilobits/s -> 0.000008 (abs, normalized)",
			ctx:  "system.net", mid: "OutOctets", units: "kilobits/s", host: "master", value: -8.0,
			want: Mapping{MetricType: types.NetworkTxBps, Value: 8.0 * 125.0 / 125_000_000.0, Ok: true},
		},
		{
			name: "net OutOctets 16 kilobits/s positive -> 0.000016 (normalized)",
			ctx:  "system.net", mid: "OutOctets", units: "kilobits/s", host: "master", value: 16.0,
			want: Mapping{MetricType: types.NetworkTxBps, Value: 16.0 * 125.0 / 125_000_000.0, Ok: true},
		},

		// ── Negative cases — should return {Ok: false} ────────────────────
		{
			name: "unknown chart_context -> Ok=false",
			ctx:  "netdata.workers.cpu", mid: "idle", units: "percentage", host: "master", value: 1.0,
			want: Mapping{},
		},
		{
			name: "system.cpu but wrong metric id -> Ok=false",
			ctx:  "system.cpu", mid: "user", units: "percentage", host: "master", value: 5.0,
			want: Mapping{},
		},
		{
			name: "system.cpu but wrong units -> Ok=false",
			ctx:  "system.cpu", mid: "idle", units: "fraction", host: "master", value: 0.99,
			want: Mapping{},
		},
		{
			name: "system.ram wrong metric_id (free) -> Ok=false",
			ctx:  "system.ram", mid: "free", units: "MiB", host: "master", value: 60000,
			want: Mapping{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FromRow(tc.ctx, tc.mid, tc.units, tc.host, tc.value)
			if got.Ok != tc.want.Ok {
				t.Fatalf("Ok: got %v, want %v", got.Ok, tc.want.Ok)
			}
			if !tc.want.Ok {
				return
			}
			if got.MetricType != tc.want.MetricType {
				t.Errorf("MetricType: got %q, want %q", got.MetricType, tc.want.MetricType)
			}
			if math.Abs(got.Value-tc.want.Value) > 1e-9 {
				t.Errorf("Value: got %.9f, want %.9f", got.Value, tc.want.Value)
			}
		})
	}
}

// TestHostRAMMiB asserts the host-specific memory total table used to
// normalize system.ram/used.
func TestHostRAMMiB(t *testing.T) {
	cases := []struct {
		host string
		want float64
	}{
		{"master", 65536.0},
		{"node_1", 8192.0},
		{"node_2", 8192.0},
		{"node_3", 8192.0},
		{"node_99", 8192.0}, // prefix fallback
		{"unknown", 8192.0}, // ultimate fallback
	}
	for _, tc := range cases {
		got := hostRAMMiB(tc.host)
		if got != tc.want {
			t.Errorf("hostRAMMiB(%q): got %.0f, want %.0f", tc.host, got, tc.want)
		}
	}
}
