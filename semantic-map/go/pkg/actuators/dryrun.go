package actuators

import (
	"context"
	"sync"

	"github.com/DiyazY/di-agent/pkg/runtime"
)

type dryValue struct {
	v   float64
	set bool
}

type dryRun struct {
	Actuator
	mu       sync.Mutex
	recorded map[string]dryValue // by subject
}

// DryRun wraps a so that Set changes nothing outside the process: it records the
// value, and Read then returns it. Before any Set, Read delegates to a.
func DryRun(a Actuator) Actuator {
	return &dryRun{Actuator: a, recorded: map[string]dryValue{}}
}

func (d *dryRun) Read(ctx context.Context, w runtime.Workload) (float64, bool, error) {
	d.mu.Lock()
	r, ok := d.recorded[w.Subject]
	d.mu.Unlock()
	if ok {
		return r.v, r.set, nil
	}
	return d.Actuator.Read(ctx, w)
}

func (d *dryRun) Set(ctx context.Context, w runtime.Workload, v float64, set bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.recorded[w.Subject] = dryValue{v, set}
	return nil
}
