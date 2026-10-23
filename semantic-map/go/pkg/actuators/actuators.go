// Package actuators is the contract an action is plugged in under: what it
// changes on a workload, in which unit, and how to read and set it. Like
// pkg/collectors for sensing, a registry maps an action name to a Factory.
package actuators

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/DiyazY/di-agent/pkg/runtime"
)

// ErrGone is returned by Read and Set when the workload no longer exists, so a
// caller can tell a vanished workload from a failing actuator.
var ErrGone = errors.New("workload is gone")

// Descriptor says what an action does, in terms the agent can reason about
// without knowing the actuator.
type Descriptor struct {
	Action   string   `json:"action"`
	Kinds    []string `json:"kinds"`
	Resource string   `json:"resource"` // "cpu" | "memory": key into Workload.Resources (request = floor, limit = ceiling)
	Unit     string   `json:"unit"`     // "cores" | "bytes"
	Capacity string   `json:"capacity"` // fact turning a node share into Unit: "cpu_cores" | "memory_total"
	Min      float64  `json:"min"`
	Relieves string   `json:"relieves"` // node metric_type
	Usage    string   `json:"usage"`    // metric_type; Usage@subject is the workload's share, Usage the node's
	Describe string   `json:"describe"`
}

// Actuator changes one resource setting of a workload.
type Actuator interface {
	// Describe returns the action's descriptor.
	Describe() Descriptor
	// Check returns why the action cannot apply to w, or "" when it can.
	Check(w runtime.Workload) string
	// Read returns the setting now in force; set is false when none is set.
	Read(ctx context.Context, w runtime.Workload) (v float64, set bool, err error)
	// Set applies v, or removes the setting when set is false.
	Set(ctx context.Context, w runtime.Workload, v float64, set bool) error
}

// Env is what every actuator is built with: the node it acts on, the cgroup
// root, the workload runtime, and where it logs. Logf may be nil, which discards.
type Env struct {
	NodeID     string
	CgroupRoot string
	Runtime    runtime.Runtime
	Logf       func(format string, args ...any)
}

// Factory builds the actuator of one registered action.
type Factory func(Env) (Actuator, error)

var (
	registryMu sync.RWMutex
	factories  = map[string]Factory{}
)

// Register makes action available to Build. It is meant for init(); an empty or
// already registered action is a programming error and panics.
func Register(action string, f Factory) {
	if action == "" {
		panic("actuators: Register with an empty action")
	}
	if f == nil {
		panic("actuators: Register " + action + " with a nil factory")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := factories[action]; dup {
		panic("actuators: action " + action + " registered twice")
	}
	factories[action] = f
}

// Build makes the actuator registered under action. An unknown action is an
// error naming the registered ones.
func Build(action string, env Env) (Actuator, error) {
	registryMu.RLock()
	f, ok := factories[action]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown action %q (registered: %s)", action, strings.Join(Names(), ", "))
	}
	return f(env)
}

// Names lists the registered actions, sorted.
func Names() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(factories))
	for n := range factories {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// resetForTest empties the registry.
func resetForTest() {
	registryMu.Lock()
	defer registryMu.Unlock()
	factories = map[string]Factory{}
}
