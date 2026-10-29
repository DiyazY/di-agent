package actuators

import (
	"context"
	"errors"
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
// value, and Read then returns it. Before any Set, Read delegates to a. Whether
// the workload is still there is a's to say: Read and Set both ask a, and a
// workload a answers ErrGone for is gone in the dry run too, its recorded value
// forgotten; any other error of a's is the dry run's only before a Set.
func DryRun(a Actuator) Actuator {
	return &dryRun{Actuator: a, recorded: map[string]dryValue{}}
}

func (d *dryRun) Read(ctx context.Context, w runtime.Workload) (float64, bool, error) {
	v, set, err := d.Actuator.Read(ctx, w)
	d.mu.Lock()
	defer d.mu.Unlock()
	if errors.Is(err, ErrGone) {
		delete(d.recorded, w.Subject)
		return 0, false, err
	}
	if r, ok := d.recorded[w.Subject]; ok {
		return r.v, r.set, nil
	}
	return v, set, err
}

func (d *dryRun) Set(ctx context.Context, w runtime.Workload, v float64, set bool) error {
	if _, _, err := d.Actuator.Read(ctx, w); errors.Is(err, ErrGone) {
		d.mu.Lock()
		delete(d.recorded, w.Subject)
		d.mu.Unlock()
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.recorded[w.Subject] = dryValue{v, set}
	return nil
}
