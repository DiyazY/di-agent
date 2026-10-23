package actuators

import (
	"context"
	"sync"

	"github.com/DiyazY/di-agent/pkg/runtime"
)

type static struct {
	d       Descriptor
	check   func(runtime.Workload) string
	initial func(runtime.Workload) (float64, bool)
	mu      sync.Mutex
	set     map[string]dryValue // by subject
}

// Static is an actuator that touches nothing outside the process: Read returns
// the last value Set for the subject, else initial(w), and Check is check (nil
// means always actionable). It stands in for a real actuator where the runtime
// is dry-run.
func Static(d Descriptor, check func(runtime.Workload) string, initial func(runtime.Workload) (float64, bool)) Actuator {
	return &static{d: d, check: check, initial: initial, set: map[string]dryValue{}}
}

func (s *static) Describe() Descriptor { return s.d }

func (s *static) Check(w runtime.Workload) string {
	if s.check == nil {
		return ""
	}
	return s.check(w)
}

func (s *static) Read(_ context.Context, w runtime.Workload) (float64, bool, error) {
	s.mu.Lock()
	r, ok := s.set[w.Subject]
	s.mu.Unlock()
	if ok {
		return r.v, r.set, nil
	}
	v, set := s.initial(w)
	return v, set, nil
}

func (s *static) Set(_ context.Context, w runtime.Workload, v float64, set bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.set[w.Subject] = dryValue{v, set}
	return nil
}
