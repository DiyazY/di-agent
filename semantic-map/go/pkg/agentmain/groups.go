package agentmain

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/DiyazY/di-agent/pkg/groups"
	"github.com/DiyazY/di-agent/pkg/runtime"
	"github.com/DiyazY/di-agent/pkg/statemap"
)

// grouper keeps the node's topology: which workloads form the groups operators named,
// which subjects are its system and hardware, and which pod each container and virtual
// interface is part of. Each tick recomputes it from what the node runs now, declares
// every group whose members use the machine as a group: subject of the map, and retires
// a group nothing belongs to any more.
type grouper struct {
	state     *statemap.Map
	cfg       groups.Config
	linux     groups.Linux
	workloads func(context.Context) ([]runtime.Workload, error)
	logf      func(string, ...any)

	mu       sync.Mutex
	top      groups.Topology
	declared map[string]bool
}

func (g *grouper) tick(ctx context.Context) {
	in := groups.Input{Physical: g.linux.Physical(), VethPods: g.linux.VethPods(), ContainerPods: map[string]string{}}
	if g.workloads != nil {
		ws, err := g.workloads(ctx)
		if err != nil {
			g.logf("groups: listing workloads: %v", err)
		}
		in.Workloads = ws
	}
	seen := map[string]bool{}
	for _, p := range g.state.State(statemap.Query{}).Properties {
		if p.Subject == "" || statemap.IsGroup(p.Subject) || seen[p.Subject] {
			continue
		}
		seen[p.Subject] = true
		in.Subjects = append(in.Subjects, p.Subject)
		if p.Labels["kind"] == "container" && p.Labels["parent"] != "" {
			in.ContainerPods[p.Subject] = p.Labels["parent"]
		}
	}
	top := groups.Compute(g.cfg, in)

	now := map[string]bool{}
	for _, gr := range top.Groups {
		if !gr.Consumers {
			continue
		}
		if err := g.state.DeclareGroup(gr.ID, gr.Members, map[string]string{"group": gr.Name, "source": gr.Source}); err != nil {
			g.logf("groups: %s: %v", gr.ID, err)
			continue
		}
		now[gr.ID] = true
	}
	g.mu.Lock()
	gone := []string{}
	for id := range g.declared {
		if !now[id] {
			gone = append(gone, id)
		}
	}
	g.top, g.declared = top, now
	g.mu.Unlock()
	for _, id := range gone {
		for _, metric := range statemap.GroupMetrics {
			_ = g.state.RetireProperty(metric+"@"+id, "no subject belongs to the group any more", "groups")
		}
	}
}

func (g *grouper) topology() groups.Topology {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.top
}

// run refreshes the topology, then waits every at the pace, until ctx ends.
func (g *grouper) run(ctx context.Context, every time.Duration, p *Pace) {
	for {
		g.tick(ctx)
		if !p.wait(ctx, every) {
			return
		}
	}
}

// registerGroupsRoute serves the node's topology: its groups, each with its members and
// whether the map holds its sums, and each part's whole.
func registerGroupsRoute(mux *http.ServeMux, g *grouper) {
	mux.HandleFunc("GET /groups", func(w http.ResponseWriter, _ *http.Request) {
		top := g.topology()
		if top.Groups == nil {
			top.Groups = []groups.Group{}
		}
		if top.Parents == nil {
			top.Parents = map[string]string{}
		}
		writeJSON(w, top)
	})
}
