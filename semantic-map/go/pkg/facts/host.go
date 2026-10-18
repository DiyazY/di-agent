package facts

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/DiyazY/di-agent/pkg/statemap"
)

func init() {
	Register("host", func(root string) Source { return &Host{Root: root} })
}

// Host reads the machine under Root (the agent's own filesystem when empty): the
// Linux files every distribution has, so it needs no tool and no privilege. On a
// system without them it reads what Go itself knows — the hostname, architecture and
// CPU count.
type Host struct{ Root string }

func (h *Host) Name() string { return "host" }

func (h *Host) path(p string) string { return filepath.Join(h.Root, p) }

func (h *Host) Read(context.Context) ([]statemap.Fact, error) {
	var out []statemap.Fact
	add := func(key, value, from string) {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, statemap.Fact{Key: key, Value: value, Source: "host:" + from})
		}
	}
	num := func(key string, v float64, unit, from string) {
		if v > 0 {
			f := statemap.NumberFact(key, v, unit)
			f.Source = "host:" + from
			out = append(out, f)
		}
	}

	if h.Root == "" {
		if n, err := os.Hostname(); err == nil {
			add(Hostname, n, "hostname")
		}
	} else {
		add(Hostname, readFile(h.path("/etc/hostname")), "/etc/hostname")
	}
	add(OS, osRelease(h.path("/etc/os-release")), "/etc/os-release")
	add(Kernel, readFile(h.path("/proc/sys/kernel/osrelease")), "/proc/sys/kernel/osrelease")
	add(Arch, runtime.GOARCH, "go runtime")

	// A Raspberry Pi names itself in the device tree; a PC or VM in DMI.
	if m := strings.TrimRight(readFile(h.path("/sys/firmware/devicetree/base/model")), "\x00"); m != "" {
		add(MachineModel, m, "/sys/firmware/devicetree/base/model")
	} else {
		add(MachineModel, readFile(h.path("/sys/class/dmi/id/product_name")), "/sys/class/dmi/id/product_name")
	}

	model, cores := cpuinfo(h.path("/proc/cpuinfo"))
	add(CPUModel, model, "/proc/cpuinfo")
	if cores > 0 {
		num(CPUCores, float64(cores), "cores", "/proc/cpuinfo")
	} else if h.Root == "" {
		num(CPUCores, float64(runtime.NumCPU()), "cores", "go runtime")
	}

	mem := meminfo(h.path("/proc/meminfo"))
	num(MemoryTotal, float64(mem["MemTotal"]), "bytes", "/proc/meminfo")
	num(SwapTotal, float64(mem["SwapTotal"]), "bytes", "/proc/meminfo")

	if total := diskTotal(h.path("/")); total > 0 {
		num(DiskTotal, float64(total), "bytes", "statfs /")
	}
	if bt := bootTime(h.path("/proc/stat")); !bt.IsZero() {
		add(BootTime, bt.UTC().Format(time.RFC3339), "/proc/stat")
	}

	if len(out) == 0 {
		return nil, errors.New("nothing readable")
	}
	return out, nil
}

func readFile(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// osRelease is PRETTY_NAME, or NAME and VERSION when there is none.
func osRelease(p string) string {
	kv := map[string]string{}
	for _, line := range strings.Split(readFile(p), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			kv[k] = strings.Trim(v, `"'`)
		}
	}
	if kv["PRETTY_NAME"] != "" {
		return kv["PRETTY_NAME"]
	}
	return strings.TrimSpace(kv["NAME"] + " " + kv["VERSION"])
}

// armParts names the Arm cores an edge machine is likely to have, by /proc/cpuinfo's
// "CPU part" (implementer 0x41).
var armParts = map[string]string{
	"0xd03": "Arm Cortex-A53", "0xd04": "Arm Cortex-A35", "0xd05": "Arm Cortex-A55",
	"0xd07": "Arm Cortex-A57", "0xd08": "Arm Cortex-A72", "0xd09": "Arm Cortex-A73",
	"0xd0a": "Arm Cortex-A75", "0xd0b": "Arm Cortex-A76", "0xd0c": "Arm Neoverse-N1",
	"0xd40": "Arm Neoverse-V1", "0xd41": "Arm Cortex-A78", "0xd49": "Arm Neoverse-N2",
}

// armImplementers names /proc/cpuinfo's "CPU implementer", for a core whose part is
// not in armParts: a vendor is still more than nothing.
var armImplementers = map[string]string{
	"0x41": "Arm", "0x42": "Broadcom", "0x43": "Cavium", "0x48": "HiSilicon", "0x4e": "NVIDIA",
	"0x51": "Qualcomm", "0x61": "Apple", "0xc0": "Ampere",
}

// cpuinfo returns the CPU's model name and how many logical CPUs are listed. An Arm
// kernel lists no "model name"; its core is named by part number, and "Hardware"
// (the SoC, and on a Raspberry Pi not even the right one) is the last resort.
func cpuinfo(p string) (string, int) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0
	}
	defer f.Close()
	var model, implementer, part, hardware string
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "processor":
			n++
		case "model name":
			if model == "" {
				model = v
			}
		case "CPU implementer":
			implementer = v
		case "CPU part":
			part = v
		case "Hardware":
			hardware = v
		}
	}
	if model == "" && implementer == "0x41" {
		model = armParts[part]
	}
	if v, ok := armImplementers[implementer]; ok && model == "" {
		model = v + " CPU (part " + part + ")"
	}
	if model == "" {
		model = hardware
	}
	return model, n
}

// meminfo returns /proc/meminfo's fields in bytes.
func meminfo(p string) map[string]uint64 {
	out := map[string]uint64{}
	for _, line := range strings.Split(readFile(p), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		if len(fields) > 2 && fields[2] == "kB" {
			v *= 1024
		}
		out[strings.TrimSuffix(fields[0], ":")] = v
	}
	return out
}

// bootTime is /proc/stat's btime.
func bootTime(p string) time.Time {
	for _, line := range strings.Split(readFile(p), "\n") {
		if rest, ok := strings.CutPrefix(line, "btime "); ok {
			if s, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64); err == nil {
				return time.Unix(s, 0)
			}
		}
	}
	return time.Time{}
}
