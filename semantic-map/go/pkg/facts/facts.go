// Package facts reads what a machine is — its operating system, CPU, memory, disk —
// for the state map to hold beside what it is doing.
//
// A Source is one way of reading them. "host" reads the machine the agent runs on
// from /proc, /sys and /etc/os-release; a deployment that knows its machine better
// (a cloud metadata service, an inventory, the monitoring agent's own report)
// registers another under its own name and picks it by flag. Nothing else changes.
package facts

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/DiyazY/di-agent/pkg/statemap"
)

// Source reads facts about one machine. A fact it cannot read is left out rather
// than reported as empty; an error means the source could read nothing at all.
type Source interface {
	Name() string
	Read(ctx context.Context) ([]statemap.Fact, error)
}

// Factory builds a source. root, when not empty, is where the machine's filesystem
// is mounted, for an agent in a container reading its host (such as /host).
type Factory func(root string) Source

var (
	mu      sync.RWMutex
	sources = map[string]Factory{}
)

// Register adds a source by name. Registering a name twice is a programming error.
func Register(name string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := sources[name]; dup {
		panic(fmt.Sprintf("facts: source %q registered twice", name))
	}
	sources[name] = f
}

// Names lists the registered sources.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(sources))
	for n := range sources {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Build returns the named source.
func Build(name, root string) (Source, error) {
	mu.RLock()
	f, ok := sources[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("no fact source named %q (have %s)", name, strings.Join(Names(), ", "))
	}
	return f(root), nil
}

// Load reads each source and records what it read in the map. A source that fails
// is reported and the rest still load: a machine described in part is better than
// one not described.
func Load(ctx context.Context, m *statemap.Map, srcs []Source) []error {
	var errs []error
	for _, s := range srcs {
		facts, err := s.Read(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s.Name(), err))
			continue
		}
		if err := m.RecordFacts(s.Name(), facts); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// The keys the host source writes, so a reader (the console, a rule, a prompt) can
// name them without a string it has to guess.
const (
	Hostname     = "hostname"
	OS           = "os"
	Kernel       = "kernel"
	Arch         = "arch"
	MachineModel = "machine_model"
	CPUModel     = "cpu_model"
	CPUCores     = "cpu_cores"
	MemoryTotal  = "memory_total"
	SwapTotal    = "swap_total"
	DiskTotal    = "disk_total"
	BootTime     = "boot_time"
)
