package agentmain

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/DiyazY/di-agent/pkg/actuators"
	"github.com/DiyazY/di-agent/pkg/collectors"
	"github.com/DiyazY/di-agent/pkg/policy"
	"github.com/DiyazY/di-agent/pkg/relief"
	"github.com/DiyazY/di-agent/pkg/runtime"
	"github.com/DiyazY/di-agent/pkg/spec"
	"github.com/DiyazY/di-agent/pkg/statemap"
)

// defaultLedger is where the ledger of the agent's changes is kept when
// persist.file is empty and names no directory to keep it beside.
const defaultLedger = "/var/lib/di-agent/actions.json"

// ledgerPath is where the ledger of the agent's changes is kept: actions.json
// beside the state snapshot, so the two survive a restart together.
func ledgerPath(persistFile string) string {
	if persistFile == "" {
		return defaultLedger
	}
	return filepath.Join(filepath.Dir(persistFile), "actions.json")
}

// buildActing builds the actions the agent can take on rt's workloads, with the
// ledger of what it changed. Under the dry-run runtime every actuator is wrapped
// in actuators.DryRun and the ledger is kept in memory: nothing reaches the
// system, so evidence is false and there is nothing to remember across a
// restart. Otherwise the ledger is a file; one that cannot be opened refuses
// start while relief is enabled, since a change the agent cannot record it could
// never give back, and is kept in memory otherwise, where no change is made.
func buildActing(cfg spec.Spec, rt runtime.Runtime, logf func(string, ...any)) (acting *actuators.Acting, evidence bool, err error) {
	dry := rt.Name() == "dry-run"
	ledger := actuators.MemoryLedger()
	if !dry {
		path := ledgerPath(cfg.Persist.File)
		l, err := actuators.OpenLedger(path)
		switch {
		case err == nil:
			ledger = l
		case cfg.Loops.Relief.Enabled:
			return nil, false, fmt.Errorf("loops.relief needs a writable ledger of its changes (beside persist.file, or %s): %w", defaultLedger, err)
		default:
			logf("actuation: %v; relief is off, so the agent changes nothing and keeps no ledger on disk", err)
		}
	}
	env := actuators.Env{NodeID: cfg.Node.ID, CgroupRoot: collectors.CgroupRoot(cfg.Sources.Collectors),
		Runtime: rt, Logf: logf, Held: ledger.For}
	a, err := actuators.Build("cpu.limit", env)
	if err != nil {
		return nil, false, err
	}
	if dry {
		a = actuators.DryRun(a)
	}
	return actuators.NewActing(cfg.Node.ID, ledger, a), !dry, nil
}

// targetOf is the workload as the policy sees it: its kind is the subject's part
// before the colon, so a rule written for pods never matches a unit.
func targetOf(w runtime.Workload) policy.Target {
	kind, _, _ := strings.Cut(w.Subject, ":")
	return policy.Target{Kind: kind, Name: w.Name, Labels: w.Labels, Self: w.Self}
}

// workloadSubjects is the subjects the map holds a CPU share for: the only ones
// that run as a workload the agent could cap. A core, a disk or an interface is
// a subject of the map too, and is not something to throttle.
func workloadSubjects(m *statemap.Map) func() []string {
	return func() []string {
		var out []string
		for _, s := range m.Subjects() {
			if _, ok := m.Property("cpu_utilization@" + s); ok {
				out = append(out, s)
			}
		}
		return out
	}
}

type resourceView struct {
	Request *float64 `json:"request"`
	Limit   *float64 `json:"limit"`
}

// workloadView is one workload of GET /workloads. Can maps each action the agent
// has an actuator for to why it cannot apply to the workload, "" when it can.
type workloadView struct {
	Subject   string                  `json:"subject"`
	Name      string                  `json:"name"`
	Labels    map[string]string       `json:"labels"`
	Resources map[string]resourceView `json:"resources"`
	Can       map[string]string       `json:"can"`
	Policy    policy.Effective        `json:"policy"`
}

// can is each actuator's Check of w, by action.
func can(acting *actuators.Acting, w runtime.Workload) map[string]string {
	out := map[string]string{}
	for _, d := range acting.Descriptors() {
		if a, ok := acting.Actuator(d.Action); ok {
			out[d.Action] = a.Check(w)
		}
	}
	return out
}

// registerActingRoutes shows what the agent could change and what it is allowed
// to: every workload the runtime lists with the policy that governs it and what
// each action says of it, the same answer for one workload with the rule behind
// it, and the actions themselves with the changes the agent holds. The relief
// routes show the loop and are its kill switch; loop is nil when the spec leaves
// relief off, and they then say so rather than not exist.
func registerActingRoutes(mux *http.ServeMux, rt runtime.Runtime, acting *actuators.Acting, pol *policy.Policy, loop *relief.Loop) {
	registerReliefRoutes(mux, loop)
	registerActuatorsRoute(mux, rt, acting)

	mux.HandleFunc("GET /workloads", func(w http.ResponseWriter, r *http.Request) {
		ws, err := rt.Workloads(r.Context())
		if err != nil {
			writeError(w, http.StatusBadGateway, "listing workloads: "+err.Error())
			return
		}
		out := make([]workloadView, 0, len(ws))
		for _, wl := range ws {
			if wl.Labels == nil {
				wl.Labels = map[string]string{}
			}
			res := make(map[string]resourceView, len(wl.Resources))
			for name, r := range wl.Resources {
				res[name] = resourceView{Request: r.Request, Limit: r.Limit}
			}
			out = append(out, workloadView{
				Subject: wl.Subject, Name: wl.Name, Labels: wl.Labels,
				Resources: res,
				Can:       can(acting, wl),
				Policy:    pol.For(targetOf(wl)),
			})
		}
		writeJSON(w, out)
	})

	mux.HandleFunc("GET /policy", func(w http.ResponseWriter, r *http.Request) {
		subject := r.URL.Query().Get("subject")
		if subject == "" {
			writeError(w, http.StatusBadRequest, "subject is required, e.g. ?subject=pod:x")
			return
		}
		ws, err := rt.Workloads(r.Context())
		if err != nil {
			writeError(w, http.StatusBadGateway, "listing workloads: "+err.Error())
			return
		}
		for _, wl := range ws {
			if wl.Subject == subject {
				writeJSON(w, pol.For(targetOf(wl)))
				return
			}
		}
		writeError(w, http.StatusNotFound, "the runtime has no workload "+subject)
	})
}

// registerReliefRoutes wires GET /relief, GET /relief/actions and the three
// operator controls: pause, resume and revert one action. Pause stops new
// changes and releases; it does not abandon a change in force.
func registerReliefRoutes(mux *http.ServeMux, loop *relief.Loop) {
	handle := func(pattern string, mutates bool, h func(w http.ResponseWriter, r *http.Request)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if loop == nil {
				writeError(w, http.StatusNotFound, "relief is not enabled in the spec (loops.relief.enabled)")
				return
			}
			if mutates {
				if err := requireJSON(r); err != nil {
					writeError(w, http.StatusBadRequest, err.Error())
					return
				}
			}
			h(w, r)
		})
	}
	handle("GET /relief", false, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, loop.Status()) })
	handle("GET /relief/actions", false, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, loop.Actions()) })
	handle("POST /relief/pause", true, func(w http.ResponseWriter, r *http.Request) {
		loop.Pause()
		writeJSON(w, loop.Status())
	})
	handle("POST /relief/resume", true, func(w http.ResponseWriter, r *http.Request) {
		loop.Resume()
		writeJSON(w, loop.Status())
	})
	handle("POST /relief/actions/{id}/revert", true, func(w http.ResponseWriter, r *http.Request) {
		err := loop.Revert(r.Context(), r.PathValue("id"))
		switch {
		case errors.Is(err, relief.ErrNoAction):
			writeError(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeError(w, http.StatusBadGateway, err.Error())
		default:
			writeJSON(w, loop.Status())
		}
	})
}
