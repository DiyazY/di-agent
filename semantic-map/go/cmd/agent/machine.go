package main

import (
	"bufio"
	"context"
	"log"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/DiyazY/di-agent/pkg/facts"
	"github.com/DiyazY/di-agent/pkg/statemap"
)

// machineMemory is the memory this agent may use: the node's MemTotal, or its own
// cgroup's memory.max when that is lower, since an agent in a container is bounded by
// its limit and not by the host. 0 when neither can be read (not Linux).
func machineMemory() uint64 {
	total := memTotal("/proc/meminfo")
	if limit := cgroupNumber("/sys/fs/cgroup/memory.max"); limit > 0 && (total == 0 || limit < total) {
		return limit
	}
	return total
}

func memTotal(path string) uint64 {
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

// cgroupNumber reads a cgroup v2 limit file's first field; "max" or anything
// unreadable is 0, meaning no limit known.
func cgroupNumber(path string) uint64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0
	}
	n, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// machineCores is the CPUs this agent may use: the CPUs it can run on, or its own
// cgroup's cpu.max quota when that is lower.
func machineCores() int {
	cores := runtime.NumCPU()
	b, err := os.ReadFile("/sys/fs/cgroup/cpu.max")
	if err != nil {
		return cores
	}
	fields := strings.Fields(string(b))
	if len(fields) != 2 || fields[0] == "max" {
		return cores
	}
	quota, err1 := strconv.ParseFloat(fields[0], 64)
	period, err2 := strconv.ParseFloat(fields[1], 64)
	if err1 != nil || err2 != nil || period <= 0 {
		return cores
	}
	if q := int(quota/period + 0.999); q >= 1 && q < cores {
		return q
	}
	return cores
}

// loadFacts reads the machine's facts from each named source into the map. A source
// that reads nothing is logged, not fatal: a map without facts still models the
// machine, it only lacks the frame. An unknown source name is fatal, since it is a
// mistake in the command line.
func loadFacts(m *statemap.Map, names, root string) error {
	var srcs []facts.Source
	for _, n := range splitList(names) {
		if n == "none" {
			continue
		}
		s, err := facts.Build(n, root)
		if err != nil {
			return err
		}
		srcs = append(srcs, s)
	}
	for _, err := range facts.Load(context.Background(), m, srcs) {
		log.Printf("machine facts: %v", err)
	}
	if fs := m.Facts(); len(fs) > 0 {
		log.Printf("machine facts: %d read (%s)", len(fs), factLine(fs))
	}
	return nil
}

// factLine is the facts an operator reads first, for the startup log.
func factLine(fs []statemap.Fact) string {
	by := map[string]string{}
	for _, f := range fs {
		by[f.Key] = f.Value
	}
	var parts []string
	for _, k := range []string{facts.OS, facts.CPUModel, facts.CPUCores, facts.MemoryTotal} {
		if v := by[k]; v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	return strings.Join(parts, ", ")
}
