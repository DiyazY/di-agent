package runtime

import (
	"context"
	"sort"
	"sync"
)

func init() {
	Register("dry-run", func(env Env) (Runtime, error) { return NewDryRun(env), nil })
}

// DryRun is a runtime whose workloads live in memory. It is what the agent runs
// against until an orchestrator is configured, and what the loops' tests run
// against.
type DryRun struct {
	env Env

	mu        sync.Mutex
	workloads map[string]Workload
	seen      map[string]bool // subjects ever admitted, so a removed one is not re-admitted
}

// NewDryRun builds a dry-run runtime.
func NewDryRun(env Env) *DryRun {
	return &DryRun{env: env, workloads: map[string]Workload{}, seen: map[string]bool{}}
}

// Name is "dry-run".
func (d *DryRun) Name() string { return "dry-run" }

// Workloads lists the workloads, first admitting any subject in the map it has not
// seen: named for the subject, with no labels and no resources.
func (d *DryRun) Workloads(ctx context.Context) ([]Workload, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.env.Subjects != nil {
		for _, s := range d.env.Subjects() {
			if d.seen[s] {
				continue
			}
			d.seen[s] = true
			d.workloads[s] = Workload{Subject: s, Name: s}
		}
	}
	out := make([]Workload, 0, len(d.workloads))
	for _, w := range d.workloads {
		out = append(out, cloneWorkload(w))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Subject < out[j].Subject })
	return out, nil
}

// Put adds or replaces a workload. For tests.
func (d *DryRun) Put(w Workload) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen[w.Subject] = true
	d.workloads[w.Subject] = cloneWorkload(w)
}

// Remove deletes a workload, as when its pod is gone. It is not admitted again.
func (d *DryRun) Remove(subject string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen[subject] = true
	delete(d.workloads, subject)
}

func cloneFloat(p *float64) *float64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func cloneWorkload(w Workload) Workload {
	if w.Labels != nil {
		l := make(map[string]string, len(w.Labels))
		for k, v := range w.Labels {
			l[k] = v
		}
		w.Labels = l
	}
	if w.Resources != nil {
		r := make(map[string]Resource, len(w.Resources))
		for k, v := range w.Resources {
			r[k] = Resource{Request: cloneFloat(v.Request), Limit: cloneFloat(v.Limit)}
		}
		w.Resources = r
	}
	return w
}
