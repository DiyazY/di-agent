// Package runtime is the agent's inventory of the system it models: it lists the
// workloads the node runs, with what each is allowed to use.
//
// A Runtime is one orchestrator. "dry-run" keeps its workloads in memory, so the
// agent's loops can run and be tested without an orchestrator; Kubernetes
// registers another under its own name and is picked by the spec's runtime kind.
// The loops see only this interface and never learn which orchestrator is behind
// it.
//
// Changing a workload is not the runtime's: an actuator in pkg/actuators sets one
// resource, and the agent's ledger, not the orchestrator, holds what each change
// replaced.
package runtime

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/DiyazY/di-agent/pkg/spec"
)

// Resource is what a workload is allowed to use, in the resource's unit. A nil
// field is not set: no request, or no limit. A limit of zero and no limit are
// different things.
type Resource struct {
	Request *float64 `json:"request"`
	Limit   *float64 `json:"limit"`
}

// Workload is one thing the node runs that the agent may be asked to change.
// Subject is the state map's name for it (`<kind>:<identity>`); Name is the
// orchestrator's bare name for it, without a namespace. Self is true for the
// agent's own workload, which no rule may touch. Whether an action can apply to
// it is each actuator's Check, not the inventory's.
type Workload struct {
	Subject string            `json:"subject"`
	Name    string            `json:"name"`
	Labels  map[string]string `json:"labels"`
	Self    bool              `json:"self"`
	// UID is the orchestrator's identity for the workload; QoS its class, lower
	// case ("guaranteed" | "burstable" | "besteffort"), empty when it has none.
	UID string `json:"uid"`
	QoS string `json:"qos"`
	// Resources by name: "cpu" in cores, "memory" in bytes.
	Resources map[string]Resource `json:"resources"`
}

// Runtime is one orchestrator's inventory of the node. Implementations are safe
// for concurrent use.
type Runtime interface {
	Name() string
	// Workloads lists what the node runs now. The result is the caller's to keep.
	Workloads(ctx context.Context) ([]Workload, error)
}

// Env is what a runtime is built from. Subjects reports the state map's current
// subjects, for a runtime that learns of workloads from the map.
type Env struct {
	NodeID   string
	Spec     spec.RuntimeSpec
	Subjects func() []string
}

// Factory builds a runtime.
type Factory func(Env) (Runtime, error)

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

// Register adds a runtime by name. Registering a name twice is a programming error.
func Register(name string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := factories[name]; dup {
		panic(fmt.Sprintf("runtime: %q registered twice", name))
	}
	factories[name] = f
}

// Names lists the registered runtimes.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(factories))
	for n := range factories {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Build returns the named runtime.
func Build(name string, env Env) (Runtime, error) {
	mu.RLock()
	f, ok := factories[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("no runtime named %q (have %s)", name, strings.Join(Names(), ", "))
	}
	return f(env)
}
