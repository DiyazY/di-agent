package agentmain

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

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

// ledgerPath is where the ledger of the agent's changes is kept: actuation.ledger
// when set, else actions.json beside the state snapshot, so the two survive a
// restart together.
func ledgerPath(ledger, persistFile string) string {
	switch {
	case ledger != "":
		return ledger
	case persistFile == "":
		return defaultLedger
	}
	return filepath.Join(filepath.Dir(persistFile), "actions.json")
}

// buildActing builds the action of every lever in the spec, on rt's workloads,
// with the ledger of what it changed. A lever whose action no actuator performs
// refuses start. The actuators are built only while relief is enabled: an agent
// that changes nothing needs none, and one that cannot be built on this node (no
// cgroup v2, or the kubelet managing memory.high) then refuses start, naming its
// lever. Each is built after the ledger is opened and given its entries, so it
// can tell a setting the agent made before a restart from one it did not. On a
// dry run (loops.relief.dry_run, by default exactly when
// runtime.kind is dry-run) every actuator is wrapped in actuators.DryRun and the
// ledger is kept in memory: nothing reaches the system, so evidence is false and
// there is nothing to remember across a restart. Otherwise the ledger is a file;
// one that cannot be opened refuses start while relief is enabled, since a change
// the agent cannot record it could never give back, and is kept in memory
// otherwise, where no change is made.
func buildActing(cfg spec.Spec, rt runtime.Runtime, logf func(string, ...any)) (acting *actuators.Acting, evidence bool, err error) {
	registered := actuators.Names()
	for i, l := range cfg.Loops.Relief.Levers {
		if !slices.Contains(registered, l.Action) {
			return nil, false, fmt.Errorf("loops.relief.levers[%d] %s: no such actuator (registered: %s)", i, l.Action, strings.Join(registered, ", "))
		}
	}
	dry := cfg.Loops.Relief.IsDryRun(cfg.Runtime.Kind)
	ledger := actuators.MemoryLedger()
	if !dry {
		path := ledgerPath(cfg.Actuation.Ledger, cfg.Persist.File)
		l, err := actuators.OpenLedger(path)
		switch {
		case err == nil:
			ledger = l
		case cfg.Loops.Relief.Enabled:
			return nil, false, fmt.Errorf("loops.relief needs a writable ledger of its changes (actuation.ledger, else beside persist.file, else %s): %w", defaultLedger, err)
		default:
			logf("actuation: %v; relief is off, so the agent changes nothing and keeps no ledger on disk", err)
		}
	}
	env := actuators.Env{NodeID: cfg.Node.ID, CgroupRoot: collectors.CgroupRoot(cfg.Sources.Collectors),
		Runtime: rt, Logf: logf, Held: ledger.For}
	var as []actuators.Actuator
	levers := cfg.Loops.Relief.Levers
	if !cfg.Loops.Relief.Enabled {
		levers = nil
	}
	for i, l := range levers {
		a, err := actuators.Build(l.Action, env)
		if err != nil {
			return nil, false, fmt.Errorf("loops.relief.levers[%d] %s: %w", i, l.Action, err)
		}
		if dry {
			a = actuators.DryRun(a)
		}
		as = append(as, a)
	}
	acting = actuators.NewActing(cfg.Node.ID, ledger, as...)
	for _, e := range acting.Unbuilt() {
		logf("actuation: warning: %s on %s (applied %s, expires %s) is in the ledger and not given back: no lever runs for this action; "+
			"it stays in force until reverted by hand (OPERATING.md §10)", e.Action, e.Subject, strconv.FormatFloat(e.Applied, 'f', -1, 64), e.Expires.Format(time.RFC3339))
	}
	return acting, !dry, nil
}

// buildRelief builds one relief loop per lever, in lever order, sharing one
// Budget: max_actions_per_hour from the default policy is one count for every
// lever, and a workload one lever acts on cools down for the others.
func buildRelief(cfg spec.Spec, rt runtime.Runtime, acting *actuators.Acting, pol *policy.Policy, m *statemap.Map,
	evidence bool, cands relief.Candidates, logf func(string, ...any)) ([]*relief.Loop, error) {
	budget := relief.NewBudget(pol.Defaults().MaxActionsPerHour)
	var loops []*relief.Loop
	for i, l := range cfg.Loops.Relief.Levers {
		a, ok := acting.Actuator(l.Action)
		if !ok {
			return nil, fmt.Errorf("loops.relief.levers[%d] %s: no actuator built for it", i, l.Action)
		}
		loop, err := relief.New(relief.Config{Interval: cfg.Loops.Relief.Interval, Lever: l, Budget: budget,
			Descriptor: a.Describe(), NodeID: cfg.Node.ID, Runtime: rt, Acting: acting, Policy: pol, Map: m,
			Logf: logf, Evidence: evidence, DryRun: cfg.Loops.Relief.IsDryRun(cfg.Runtime.Kind), Candidates: cands})
		if err != nil {
			return nil, fmt.Errorf("loops.relief.levers[%d] %s: %w", i, l.Action, err)
		}
		loops = append(loops, loop)
	}
	return loops, nil
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
// routes show the loops and are their kill switch; loops is empty when the spec
// leaves relief off, and they then say so rather than not exist.
func registerActingRoutes(mux *http.ServeMux, rt runtime.Runtime, acting *actuators.Acting, pol *policy.Policy, loops []*relief.Loop) {
	registerReliefRoutes(mux, loops)
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

// reliefView is GET /relief: each lever's loop, in lever order.
type reliefView struct {
	Levers []relief.Status `json:"levers"`
}

// registerReliefRoutes wires GET /relief, GET /relief/actions and the three
// operator controls: pause, resume and revert one action. Pause and resume act
// on every lever; pause stops new changes and releases, and does not abandon a
// change in force. A revert finds its action in whichever lever holds it.
func registerReliefRoutes(mux *http.ServeMux, loops []*relief.Loop) {
	status := func() reliefView {
		v := reliefView{Levers: make([]relief.Status, 0, len(loops))}
		for _, l := range loops {
			v.Levers = append(v.Levers, l.Status())
		}
		return v
	}
	handle := func(pattern string, mutates bool, h func(w http.ResponseWriter, r *http.Request)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if len(loops) == 0 {
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
	handle("GET /relief", false, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, status()) })
	handle("GET /relief/actions", false, func(w http.ResponseWriter, r *http.Request) {
		var out []relief.Action
		for _, l := range loops {
			out = append(out, l.Actions()...)
		}
		// Each loop's are newest first; across levers, by when each last acted.
		sort.SliceStable(out, func(i, j int) bool { return out[i].ActedAt.After(out[j].ActedAt) })
		if out == nil {
			out = []relief.Action{}
		}
		writeJSON(w, out)
	})
	handle("POST /relief/pause", true, func(w http.ResponseWriter, r *http.Request) {
		for _, l := range loops {
			l.Pause()
		}
		writeJSON(w, status())
	})
	handle("POST /relief/resume", true, func(w http.ResponseWriter, r *http.Request) {
		for _, l := range loops {
			l.Resume()
		}
		writeJSON(w, status())
	})
	handle("POST /relief/actions/{id}/revert", true, func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		for _, l := range loops {
			err := l.Revert(r.Context(), id)
			switch {
			case errors.Is(err, relief.ErrNoAction):
				continue
			case err != nil:
				writeError(w, http.StatusBadGateway, err.Error())
			default:
				writeJSON(w, status())
			}
			return
		}
		writeError(w, http.StatusNotFound, fmt.Sprintf("%v: %s", relief.ErrNoAction, id))
	})
}
