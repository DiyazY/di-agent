package statemap

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// admitHeadroom is the share of the node's CPU and of its memory admission leaves
// free. Past it, a pod owed CPU or memory waits on what others hold, which is what
// relief relieves; admitting into it would be admitting work relief must then act on.
const admitHeadroom = 0.10

// Basis says where an admission's demand came from.
type Basis string

const (
	// BasisMeasured: the workload is a pod the map has seen here, and its demand is
	// what was observed of it, or what was declared where that is larger.
	BasisMeasured Basis = "measured"
	// BasisDeclared: anything else, and its demand is what the requester declared.
	BasisDeclared Basis = "declared"
)

// AdmitRequest asks whether this node can take a workload.
type AdmitRequest struct {
	ID string
	// From is the node asking, when one is named; it is recorded, not weighed.
	From string
	// Workload optionally names the subject. Only a pod (pod:<uid>) is measured here:
	// its uid is the cluster's, so pod:<uid> in this map is that workload. A unit or a
	// group (group:<name>, from cluster-wide labels; group:system exists by default)
	// names something each node has its own of, so it is recorded and priced on the
	// declared demand.
	Workload string
	// CPUCores and MemoryBytes are the declared demand, >= 0.
	CPUCores    float64
	MemoryBytes float64
	// CPUEnter and MemoryEnter are the relief levers' enter levels for
	// cpu_contention_ratio and memory_contention_ratio; 0 means no such lever, and
	// that contention is not checked.
	CPUEnter    float64
	MemoryEnter float64
}

// AdmitResult is the answer with its trace.
type AdmitResult struct {
	DecisionID string `json:"decision_id"`
	Revision   uint64 `json:"revision"`
	Accepted   bool   `json:"accepted"`
	Reason     string `json:"reason"`
	// Basis is where the priced demand came from; empty when the answer came before
	// demand was priced.
	Basis Basis `json:"basis,omitempty"`
	// CPUAfter and MemoryAfter are the node shares projected with the workload; nil
	// when the answer came before a projection was made.
	CPUAfter    *float64 `json:"cpu_after,omitempty"`
	MemoryAfter *float64 `json:"memory_after,omitempty"`
	Rationale   string   `json:"rationale"`
	Caveats     []string `json:"caveats,omitempty"`
	// Err is set when the request was refused before anything was decided.
	Err string `json:"error,omitempty"`
}

// contentionReporters names, for each node contention reading, the per-pod property
// the collector derives it from: the node's cpu_contention_ratio is the worst promised
// pod's cpu_contention_ratio@pod, and its memory_contention_ratio the worst promised
// pod's memory stall, memory_pressure_ratio@pod. The node's CPU reading stops when no
// pod is promised CPU, so a quiet node reading on its own is an emptied node, not a
// contended one.
var contentionReporters = map[string]string{
	"cpu_contention_ratio":    "cpu_contention_ratio@pod:",
	"memory_contention_ratio": "memory_pressure_ratio@pod:",
}

// ContentionReporter is the prefix of the per-pod property a node contention reading
// is the worst of, as Admit reads it; ok is false for a reading with no entry.
func ContentionReporter(relieves string) (prefix string, ok bool) {
	prefix, ok = contentionReporters[relieves]
	return prefix, ok
}

// Admit answers whether this node can take a workload, from its own map, and records
// the answer with what it read. It decides; nothing is scheduled.
//
// It does not go through Estimate: the contention relief acts on is a measured
// stall, not a function of demand, so it cannot be projected for a workload that is
// not here yet, and Estimate's assume substitutes a property the map already holds.
// Admission instead asks whether the node keeps its headroom: the node's CPU and
// memory shares plus the workload's must stay at or below 1 - admitHeadroom, and a
// node at or above a lever's enter level declines outright.
//
// The checks run in order: the workload, whether there is anything to price, the
// facts, the node's shares, contention, the projection. The first to fail answers.
func (m *Map) Admit(req AdmitRequest) AdmitResult {
	for _, d := range []struct {
		name string
		v    float64
	}{{"cpu_cores", req.CPUCores}, {"memory_bytes", req.MemoryBytes}} {
		if math.IsNaN(d.v) || math.IsInf(d.v, 0) || d.v < 0 {
			return AdmitResult{Err: d.name + " must be a finite number >= 0"}
		}
	}
	// An explicit id is claimed before any work, and every path from here commits a
	// decision, which releases the claim.
	if req.ID != "" && !m.journal.reserve(req.ID) {
		return AdmitResult{Err: "decision id " + req.ID + " is already used"}
	}
	what := req.Workload
	if what == "" {
		what = "a workload"
	}
	question := fmt.Sprintf("admit %s declaring %g CPU cores and %g bytes of memory", what, req.CPUCores, req.MemoryBytes)
	if req.From != "" {
		question += " for " + req.From
	}
	id := req.ID
	if id == "" {
		id = m.admitID(req)
	}
	b := m.Decide(id, "admit", question)
	b.Input("request", map[string]any{"from": req.From, "workload": req.Workload, "cpu_cores": req.CPUCores, "memory_bytes": req.MemoryBytes})
	b.Input("thresholds", map[string]float64{"cpu_enter": req.CPUEnter, "memory_enter": req.MemoryEnter, "headroom": admitHeadroom})

	res := AdmitResult{}
	answer := func(accepted bool, reason string) AdmitResult {
		res.Accepted, res.Reason = accepted, reason
		b.Note("%s", reason)
		a := map[string]any{"accepted": accepted, "reason": reason, "basis": string(res.Basis),
			"cpu_enter": req.CPUEnter, "memory_enter": req.MemoryEnter, "limit": 1 - admitHeadroom}
		if res.CPUAfter != nil {
			a["cpu_after"], a["memory_after"] = *res.CPUAfter, *res.MemoryAfter
		}
		d := b.Commit(a)
		if d == nil {
			return AdmitResult{Err: "decision " + id + " was not recorded: duplicate id"}
		}
		res.DecisionID, res.Revision, res.Rationale, res.Caveats = d.ID, d.Revision, d.Rationale, d.Caveats
		return res
	}

	// 1. The workload: what the map saw of it here, if anything. A pod with any share
	// still active is already counted in the node's own shares: there is nothing to
	// admit, and it is declined before any other check. A pod seen with both shares is
	// priced on them; otherwise (one share only: the cgroup collector reports memory
	// from its first tick, CPU only from its second; or one retired), on the declared
	// demand, saying why.
	basis := BasisDeclared
	var measured [2]float64
	var declaredWhy string
	switch {
	case req.Workload == "":
		declaredWhy = "no workload was named"
	case !strings.HasPrefix(req.Workload, "pod:"):
		declaredWhy = req.Workload + " is not a pod, so what this map holds under that name is this node's own, not the workload's"
	default:
		ids := [2]string{"cpu_utilization@" + req.Workload, "memory_utilization@" + req.Workload}
		var seen, missing, retired []string
		var last time.Time
		for i, pid := range ids {
			p, ok := b.PropertyNotUsingRetired(pid)
			switch {
			case !ok:
				missing = append(missing, pid)
				continue
			case p.Status == Retired:
				retired = append(retired, pid)
				continue
			case p.Status == Active:
				return answer(false, req.Workload+" is already running here ("+pid+" is active); nothing to admit")
			}
			measured[i] = p.Value
			seen = append(seen, pid)
			if p.LastObserved.After(last) {
				last = p.LastObserved
			}
		}
		switch {
		case len(seen) == 2:
			basis = BasisMeasured
			b.Caveat("demand of %s was measured when it last ran here, %s ago", req.Workload, b.at.Sub(last).Round(time.Second))
		case len(retired) > 0:
			for i, pid := range retired {
				retired[i] = pid + " is retired"
			}
			declaredWhy = strings.Join(retired, " and ")
		case len(seen) == 1:
			declaredWhy = "the map holds " + seen[0] + " but not " + missing[0]
		default:
			declaredWhy = "the map has not seen " + req.Workload + " here"
		}
	}
	if basis == BasisDeclared {
		if req.CPUCores == 0 && req.MemoryBytes == 0 {
			return answer(false, "nothing to price: "+declaredWhy+", and no demand was declared")
		}
		b.Caveat("demand was declared by the requester, not observed: %s", declaredWhy)
		if req.CPUCores == 0 {
			b.Caveat("no cpu demand was declared")
		}
		if req.MemoryBytes == 0 {
			b.Caveat("no memory demand was declared")
		}
	}

	// 2. The machine: how much of it there is turns a declared demand into shares.
	capacity := map[string]float64{}
	for _, f := range b.Facts() {
		if f.Number != nil && *f.Number > 0 {
			capacity[f.Key] = *f.Number
		}
	}
	for _, k := range []string{"cpu_cores", "memory_total"} {
		if capacity[k] == 0 {
			return answer(false, "the map has no fact "+k+", so a demand cannot be read against this machine")
		}
	}

	// 3. What the node uses now. A property seeded from the specification and never
	// observed holds an assumption, not a reading; a stale or retired one, a reading of
	// the past.
	node := map[string]float64{}
	for _, k := range []string{"cpu_utilization", "memory_utilization"} {
		p, ok := b.PropertyNotUsingRetired(k)
		if !ok || p.NObservations == 0 {
			return answer(false, "the map has no reading of node "+k)
		}
		if p.Status != Active {
			return answer(false, "the map's reading of node "+k+" is "+string(p.Status))
		}
		node[k] = p.Value
	}

	// 4. A node already contended is where relief would be acting. A stale or retired
	// contention reading says so only while a pod still reports what it is the worst
	// of: with none, the node has been emptied, which is not contention.
	for _, c := range []struct {
		id, kind string
		enter    float64
	}{{"cpu_contention_ratio", "cpu", req.CPUEnter}, {"memory_contention_ratio", "memory", req.MemoryEnter}} {
		if c.enter <= 0 {
			b.Caveat("no relief lever for %s, so %s was not checked", c.kind, c.id)
			continue
		}
		p, ok := b.PropertyNotUsingRetired(c.id)
		if !ok || p.NObservations == 0 {
			b.Caveat("%s has no reading, so %s contention was not checked", c.id, c.kind)
			continue
		}
		if p.Status != Active {
			// The builder does not wrap a query by metric, so the reporters found are
			// recorded as an input.
			reporters := m.ActiveWithPrefix(contentionReporters[c.id])
			b.Input(c.id+"_reporters", reporters)
			if p.Status == Unobservable {
				// The pods' own readings come from the same down source, so no
				// reporter is also no evidence of calm.
				return answer(false, fmt.Sprintf("the map's reading of %s is unobservable, so the node's contention is unknown", c.id))
			}
			if len(reporters) > 0 {
				return answer(false, fmt.Sprintf("the map's reading of %s is %s while %s still reports it, so the node's contention is unknown",
					c.id, p.Status, reporters[0]))
			}
			b.Caveat("%s is %s and no workload reports it, so the node is taken as not contended", c.id, p.Status)
			continue
		}
		if p.Value >= c.enter {
			return answer(false, fmt.Sprintf("the node is contended: %s %.3f is at or above relief's enter %.3f", c.id, p.Value, c.enter))
		}
	}

	// 5. The demand. A measured pod is priced on the larger of what was measured and
	// what was declared, per resource: the requester may know it has grown.
	declared := [2]float64{req.CPUCores / capacity["cpu_cores"], req.MemoryBytes / capacity["memory_total"]}
	demand := declared
	if basis == BasisMeasured {
		b.Input("demand", map[string]any{"measured": []float64{measured[0], measured[1]}, "declared": []float64{declared[0], declared[1]}})
		for i, kind := range []string{"cpu", "memory"} {
			demand[i] = math.Max(measured[i], declared[i])
			if declared[i] > measured[i] {
				b.Caveat("the declared %s demand %.4f exceeded the measured %.4f and was used", kind, declared[i], measured[i])
			}
		}
	}
	res.Basis = basis
	b.Note("%s demand: %.4f of the node's CPU and %.4f of its memory", basis, demand[0], demand[1])

	// 6. The projection, against the headroom.
	cpuAfter, memAfter := node["cpu_utilization"]+demand[0], node["memory_utilization"]+demand[1]
	res.CPUAfter, res.MemoryAfter = &cpuAfter, &memAfter
	limit := 1 - admitHeadroom
	if cpuAfter > limit {
		return answer(false, fmt.Sprintf("cpu would reach %.3f of the node, past %.2f", cpuAfter, limit))
	}
	if memAfter > limit {
		return answer(false, fmt.Sprintf("memory would reach %.3f of the node, past %.2f", memAfter, limit))
	}
	return answer(true, fmt.Sprintf("cpu would reach %.3f and memory %.3f of the node, within %.2f", cpuAfter, memAfter, limit))
}

// admitID names an admission with a fresh ticket and a digest of its request, so a
// retry of the same request is a second record, never an overwrite of the first.
func (m *Map) admitID(req AdmitRequest) string {
	raw, _ := json.Marshal(struct {
		From, Workload                               string
		CPUCores, MemoryBytes, CPUEnter, MemoryEnter float64
	}{req.From, req.Workload, req.CPUCores, req.MemoryBytes, req.CPUEnter, req.MemoryEnter})
	h := sha256.Sum256(raw)
	return m.NewDecisionID("adm", hex.EncodeToString(h[:])[:8])
}

// ActiveWithPrefix lists the active properties whose id starts with prefix, sorted.
func (m *Map) ActiveWithPrefix(prefix string) []string {
	var out []string
	for _, p := range m.State(Query{Statuses: []Status{Active}}).Properties {
		if strings.HasPrefix(p.ID, prefix) {
			out = append(out, p.ID)
		}
	}
	sort.Strings(out)
	return out
}
