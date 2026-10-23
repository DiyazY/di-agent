// Package runtime is the contract through which the agent changes the system it
// models: it lists the workloads the node runs, caps one's CPU, and gives the cap
// back.
//
// A Runtime is one orchestrator. "dry-run" keeps everything in memory, so the
// agent's loops can run and be tested without touching anything; Kubernetes
// registers another under its own name and is picked by the spec's runtime kind.
// The loops see only this interface and never learn which orchestrator is behind
// it.
//
// Every change returns an Undo that carries what the workload looked like before,
// so a change can always be reverted exactly, including to "no limit", and so a
// restarted agent can find, through Changed, the changes it made and has not yet
// given back.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/DiyazY/di-agent/pkg/spec"
)

// ActionCPULimit caps a workload's CPU limit, in cores.
const ActionCPULimit = "cpu.limit"

// Resource is what a workload is allowed to use, in cores. A nil field is not set:
// no request, or no limit. A limit of zero and no limit are different things.
type Resource struct {
	Request *float64 `json:"request"`
	Limit   *float64 `json:"limit"`
}

// Workload is one thing the node runs that the agent may be asked to change.
// Subject is the state map's name for it (`<kind>:<identity>`); Name is the
// orchestrator's bare name for it, without a namespace. Can lists the actions the
// runtime knows for it, each mapped to a reason: empty when the runtime can apply
// it, and otherwise why it cannot. Self is
// true for the agent's own workload, which no rule may touch.
type Workload struct {
	Subject string            `json:"subject"`
	Name    string            `json:"name"`
	Labels  map[string]string `json:"labels"`
	CPU     Resource          `json:"cpu"`
	Can     map[string]string `json:"can"`
	Self    bool              `json:"self"`
	// UID is the orchestrator's identity for the workload; QoS its class, lower
	// case ("guaranteed" | "burstable" | "besteffort"), empty when it has none.
	UID string `json:"uid"`
	QoS string `json:"qos"`
	// Resources by name: "cpu" in cores, "memory" in bytes.
	Resources map[string]Resource `json:"resources"`
}

// Change is one action to apply, with when it lapses and the decision that
// justified it.
type Change struct {
	Action     string    `json:"action"`
	CPULimit   float64   `json:"cpu_limit"`
	Expires    time.Time `json:"expires"`
	DecisionID string    `json:"decision_id"`
}

// Undo is everything needed to give a change back: who made it, to what, what the
// resource was before, and what it was set to.
type Undo struct {
	Runtime    string    `json:"runtime"`
	NodeID     string    `json:"node_id"`
	Subject    string    `json:"subject"`
	Action     string    `json:"action"`
	Original   Resource  `json:"original"`
	Applied    float64   `json:"applied"`
	Expires    time.Time `json:"expires"`
	DecisionID string    `json:"decision_id"`
}

// ErrGone is returned by Apply and Revert for a workload the runtime no longer has.
var ErrGone = errors.New("workload is gone")

// Runtime is one orchestrator the agent can act on. Implementations are safe for
// concurrent use.
type Runtime interface {
	Name() string
	// Workloads lists what the node runs now. The result is the caller's to keep.
	Workloads(ctx context.Context) ([]Workload, error)
	// Apply makes a change and returns what undoes it. An unknown subject is ErrGone.
	// A workload with an outstanding change from this node keeps the Original from
	// before that first change: the outstanding record is updated with the new
	// change, not replaced by one whose Original is the intermediate cap, so one
	// Revert always gives back what the workload had before the agent touched it.
	Apply(ctx context.Context, subject string, c Change) (Undo, error)
	// Revert restores what an Undo recorded. A removed workload is ErrGone.
	Revert(ctx context.Context, u Undo) error
	// Changed lists the changes nodeID made that have not been reverted. Another
	// node's changes are never in it.
	Changed(ctx context.Context, nodeID string) ([]Undo, error)
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
