package agentmain

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/actuators"
	"github.com/DiyazY/di-agent/pkg/policy"
	"github.com/DiyazY/di-agent/pkg/relief"
	"github.com/DiyazY/di-agent/pkg/runtime"
	"github.com/DiyazY/di-agent/pkg/spec"
	"github.com/DiyazY/di-agent/pkg/statemap"
)

func TestActuatorsRoute(t *testing.T) {
	mux, _, dr, acting := actingMuxWith(t, false)
	dr.Put(runtime.Workload{Subject: "pod:me", Name: "me", Self: true})
	b := runtime.Workload{Subject: "pod:b", Name: "b", Resources: map[string]runtime.Resource{"cpu": {Limit: f64(1)}}}
	if _, err := acting.Apply(context.Background(), b, "cpu.limit", 0.5, time.Unix(100, 0), "d1"); err != nil {
		t.Fatal(err)
	}
	type view struct {
		Descriptor actuators.Descriptor `json:"descriptor"`
		Held       []actuators.Entry    `json:"held"`
		Can        *string              `json:"can"`
	}
	read := func(url string) []view {
		t.Helper()
		rec := get(mux, url)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", url, rec.Code, rec.Body)
		}
		var out []view
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	all := read("/actuators")
	if len(all) != 2 || all[1].Descriptor.Action != "memory.high" || len(all[1].Held) != 0 {
		t.Fatalf("actuators %+v; want memory.high beside cpu.limit, holding nothing", all)
	}
	if all[0].Descriptor.Action != "cpu.limit" || all[0].Descriptor.Unit != "cores" ||
		all[0].Descriptor.Relieves != "cpu_contention_ratio" || all[0].Can != nil {
		t.Fatalf("actuators %+v; want cpu.limit's descriptor, with no can without a subject", all)
	}
	if h := all[0].Held; len(h) != 1 || h[0].Subject != "pod:b" || h[0].Applied != 0.5 || h[0].Original != 1 || !h[0].OriginalSet {
		t.Fatalf("held %+v; want the change to pod:b, 1 → 0.5", h)
	}

	one := read("/actuators?subject=pod:me")
	if len(one) != 2 || one[0].Can == nil || *one[0].Can != "the agent itself" || len(one[0].Held) != 0 {
		t.Fatalf("pod:me %+v; want can naming why not, and nothing held on it", one)
	}
	one = read("/actuators?subject=pod:b")
	if len(one) != 2 || one[0].Can == nil || *one[0].Can != "" || len(one[0].Held) != 1 {
		t.Fatalf("pod:b %+v; want can empty (it can) and its held change", one)
	}
	if rec := get(mux, "/actuators?subject=pod:nope"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown subject = %d, want 404", rec.Code)
	}
}

// named is a runtime that is not the dry run, so the agent acts for real.
type named struct{ runtime.Runtime }

func (named) Name() string { return "test" }

func TestBuildActing(t *testing.T) {
	dry, err := runtime.Build("dry-run", runtime.Env{NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	s := spec.Default()
	s.Node.ID = "n1"
	s.Loops.Relief.Enabled = true
	// The runtime the agent is told it has; named{dry} below answers as a real one.
	// cpu.limit alone: memory.high on a real runtime needs a cgroup v2 tree.
	real := s
	real.Runtime.Kind = "kubernetes"
	real.Loops.Relief.Levers = s.Loops.Relief.Levers[:1]

	t.Run("dry-run changes nothing and keeps the ledger in memory", func(t *testing.T) {
		s := s
		s.Persist.File = filepath.Join(t.TempDir(), "state.json")
		acting, evidence, err := buildActing(s, dry, t.Logf)
		if err != nil || evidence {
			t.Fatalf("evidence %v err %v; want no evidence from a dry run", evidence, err)
		}
		w := runtime.Workload{Subject: "pod:x", Resources: map[string]runtime.Resource{"cpu": {Limit: f64(1)}}}
		if _, err := acting.Apply(context.Background(), w, "cpu.limit", 0.5, time.Time{}, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(s.Persist.File), "actions.json")); !os.IsNotExist(err) {
			t.Fatalf("stat %v; a dry run writes no ledger", err)
		}
	})

	t.Run("a real runtime keeps the ledger beside persist.file", func(t *testing.T) {
		s := real
		s.Persist.File = filepath.Join(t.TempDir(), "state.json")
		acting, evidence, err := buildActing(s, named{dry}, t.Logf)
		if err != nil || !evidence {
			t.Fatalf("evidence %v err %v", evidence, err)
		}
		w := runtime.Workload{Subject: "pod:x", Resources: map[string]runtime.Resource{"cpu": {Limit: f64(1)}}}
		if _, err := acting.Apply(context.Background(), w, "cpu.limit", 0.5, time.Time{}, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(s.Persist.File), "actions.json")); err != nil {
			t.Fatalf("ledger: %v", err)
		}
	})

	t.Run("an unwritable ledger refuses relief", func(t *testing.T) {
		s := real
		blocker := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(blocker, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		s.Persist.File = filepath.Join(blocker, "state.json")
		s.Loops.Relief.Enabled = true
		want := filepath.Join(blocker, "actions.json")
		if _, _, err := buildActing(s, named{dry}, t.Logf); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("err %v; want a refusal naming %s", err, want)
		}
		s.Loops.Relief.Enabled = false
		if _, _, err := buildActing(s, named{dry}, t.Logf); err != nil {
			t.Fatalf("err %v; with relief off nothing is changed, so no ledger is needed", err)
		}
	})
}

func TestLedgerPath(t *testing.T) {
	if got := ledgerPath("", "/data/state.json"); got != "/data/actions.json" {
		t.Errorf("ledgerPath = %s", got)
	}
	if got := ledgerPath("", ""); got != "/var/lib/di-agent/actions.json" {
		t.Errorf("ledgerPath(\"\", \"\") = %s", got)
	}
	if got := ledgerPath("/l/ledger.json", "/data/state.json"); got != "/l/ledger.json" {
		t.Errorf("ledgerPath with actuation.ledger = %s", got)
	}
}

// TestADryRunReliefLoopActsOnAMapWorkload: under runtime.kind dry-run the loop
// takes a step on a workload the map holds, with no limit of its own, and the
// step is recorded in memory only.
func TestADryRunReliefLoopActsOnAMapWorkload(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }
	m := statemap.New(statemap.Config{Owner: "n1", AdmitUnknown: true, Alpha: 1}, statemap.NewJournal(0))
	m.SetClock(clock)
	unit := [2]float64{0, 1}
	read := func() {
		t.Helper()
		for _, o := range []statemap.Observation{
			{ID: "cpu_utilization", Value: 0.8},
			{ID: "cpu_contention_ratio", Value: 0.5},
			{ID: "cpu_utilization@pod:a", Subject: "pod:a", Value: 0.4},
		} {
			o.At, o.Range = now, &unit
			if err := m.Record(o); err != nil {
				t.Fatal(err)
			}
		}
	}
	read()
	if err := m.RecordFacts("test", []statemap.Fact{statemap.NumberFact("cpu_cores", 4, "cores")}); err != nil {
		t.Fatal(err)
	}
	s := spec.Default()
	s.Node.ID = "n1"
	s.Loops.Relief.Enabled = true
	rt, err := runtime.Build("dry-run", runtime.Env{NodeID: "n1", Spec: s.Runtime, Subjects: workloadSubjects(m)})
	if err != nil {
		t.Fatal(err)
	}
	acting, evidence, err := buildActing(s, rt, t.Logf)
	if err != nil || evidence {
		t.Fatalf("evidence %v err %v", evidence, err)
	}
	pol, err := policy.New(s.Policy)
	if err != nil {
		t.Fatal(err)
	}
	loop, err := relief.New(relief.Config{Interval: s.Loops.Relief.Interval, Lever: s.Loops.Relief.Levers[0],
		NodeID: "n1", Runtime: rt, Acting: acting, Policy: pol, Map: m, Now: clock, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 12 && len(acting.Entries("cpu.limit")) == 0; i++ {
		now = now.Add(15 * time.Second)
		read()
		loop.Tick(ctx)
	}
	held := acting.Entries("cpu.limit")
	if len(held) != 1 || held[0].Subject != "pod:a" || held[0].OriginalSet || held[0].Applied != 1.2000000000000002 {
		t.Fatalf("held %+v; want pod:a stepped from 1.6 to 1.2 cores, with no limit before", held)
	}
}

// TestALedgerEntryWithNoLeverIsWarnedAndShown: a change in the ledger whose
// action no built actuator performs, as after relief was turned off or its lever
// removed, is not given back by anything. Start says so, naming the change, and
// GET /actuators shows it, so it is not in force unseen.
func TestALedgerEntryWithNoLeverIsWarnedAndShown(t *testing.T) {
	dry, err := runtime.Build("dry-run", runtime.Env{NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(t.TempDir(), "actions.json")
	l, err := actuators.OpenLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	held := actuators.Entry{Action: "memory.high", Subject: "pod:x", NodeID: "n1", Applied: 314572800, Expires: expires, DecisionID: "d1"}
	if err := l.Put(held); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*spec.Spec){
		"relief off":    func(s *spec.Spec) { s.Loops.Relief.Enabled = false },
		"lever removed": func(s *spec.Spec) { s.Loops.Relief.Levers = s.Loops.Relief.Levers[:1] },
	} {
		t.Run(name, func(t *testing.T) {
			s := spec.Default()
			s.Node.ID = "n1"
			s.Runtime.Kind = "kubernetes"
			s.Loops.Relief.Enabled = true
			s.Actuation.Ledger = ledgerPath
			edit(&s)
			var logs []string
			logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
			acting, _, err := buildActing(s, named{dry}, logf)
			if err != nil {
				t.Fatal(err)
			}
			warned := false
			for _, m := range logs {
				if strings.Contains(m, "memory.high") && strings.Contains(m, "pod:x") && strings.Contains(m, "314572800") &&
					strings.Contains(m, expires.Format(time.RFC3339)) && strings.Contains(m, "not given back: no lever runs for this action") {
					warned = true
				}
			}
			if !warned {
				t.Errorf("logs %q; want a warning naming the change and that no lever gives it back", logs)
			}
			mux := http.NewServeMux()
			registerActuatorsRoute(mux, dry, acting)
			rec := get(mux, "/actuators")
			out := actuatorRows(t, rec.Body.Bytes())
			var found bool
			for _, v := range out {
				if v.Action == "memory.high" {
					found = true
					if v.Built || v.Descriptor != nil || len(v.Held) != 0 || len(v.Unmanaged) != 1 || v.Unmanaged[0].Subject != "pod:x" ||
						!strings.Contains(v.Unmanaged[0].Why, "no lever runs for this action") {
						t.Errorf("memory.high %+v; want its change unmanaged, saying why, not built, no descriptor", v)
					}
				} else if !v.Built || v.Descriptor == nil || v.Descriptor.Action != v.Action {
					t.Errorf("%+v; want a built action with its descriptor", v)
				}
			}
			if !found {
				t.Errorf("GET /actuators %s; want the memory.high change shown", rec.Body)
			}
		})
	}
}

type actuatorRow struct {
	Action     string                `json:"action"`
	Descriptor *actuators.Descriptor `json:"descriptor"`
	Built      bool                  `json:"built"`
	Held       []actuators.Entry     `json:"held"`
	Unmanaged  []actuators.Unmanaged `json:"unmanaged"`
}

func actuatorRows(t *testing.T, b []byte) []actuatorRow {
	t.Helper()
	var out []actuatorRow
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return out
}

func rowOf(rows []actuatorRow, action string) actuatorRow {
	for _, r := range rows {
		if r.Action == action {
			return r
		}
	}
	return actuatorRow{}
}

func warnedOf(logs []string, parts ...string) bool {
	for _, m := range logs {
		ok := strings.Contains(m, "warning")
		for _, p := range parts {
			ok = ok && strings.Contains(m, p)
		}
		if ok {
			return true
		}
	}
	return false
}

// TestAnotherNodesEntryIsWarnedAndShown: a ledger entry another node recorded
// is never adopted; start warns of it, naming both nodes, and GET /actuators
// shows it as unmanaged, apart from what this agent holds.
func TestAnotherNodesEntryIsWarnedAndShown(t *testing.T) {
	dry, err := runtime.Build("dry-run", runtime.Env{NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(t.TempDir(), "actions.json")
	l, err := actuators.OpenLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	theirs := actuators.Entry{Action: "cpu.limit", Subject: "pod:x", NodeID: "n2", Applied: 0.5, Expires: time.Unix(100, 0).UTC(), DecisionID: "d1"}
	if err := l.Put(theirs); err != nil {
		t.Fatal(err)
	}
	s := spec.Default()
	s.Node.ID, s.Runtime.Kind = "n1", "kubernetes"
	s.Loops.Relief.Enabled = true
	s.Loops.Relief.Levers = s.Loops.Relief.Levers[:1]
	s.Actuation.Ledger = ledgerPath
	var logs []string
	acting, _, err := buildActing(s, named{dry}, func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) })
	if err != nil {
		t.Fatal(err)
	}
	if !warnedOf(logs, "pod:x", "recorded by node n2; this agent is n1; not managed") {
		t.Errorf("logs %q; want a warning naming both nodes", logs)
	}
	mux := http.NewServeMux()
	registerActuatorsRoute(mux, dry, acting)
	row := rowOf(actuatorRows(t, get(mux, "/actuators").Body.Bytes()), "cpu.limit")
	if !row.Built || len(row.Held) != 0 || len(row.Unmanaged) != 1 || row.Unmanaged[0].NodeID != "n2" ||
		!strings.Contains(row.Unmanaged[0].Why, "recorded by node n2") {
		t.Fatalf("cpu.limit %+v; want n2's change unmanaged, nothing held", row)
	}
}

// TestADryRunShowsTheRealRunsChanges: a dry run on kubernetes opens the on-disk
// ledger read-only: a cap a real run left in force is not taken for the
// kubelet's, is warned of at start and shown by GET /actuators, and a dry-run
// step never writes the file.
func TestADryRunShowsTheRealRunsChanges(t *testing.T) {
	dry, err := runtime.Build("dry-run", runtime.Env{NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	s, _, pod, ledger := memTree(t, "500000000", ", dry_run: true")
	l, err := actuators.OpenLedger(ledger)
	if err != nil {
		t.Fatal(err)
	}
	const subject = "pod:1234abcd-0000-1111-2222-333344445555"
	if err := l.Put(actuators.Entry{Action: "memory.high", Subject: subject, Applied: 500000000, NodeID: "n1",
		Expires: time.Unix(100, 0).UTC(), DecisionID: "d1"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	var logs []string
	acting, evidence, err := buildActing(s, named{dry}, func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) })
	if err != nil {
		t.Fatalf("dry run refused: %v; want the real run's cap recognised from the ledger", err)
	}
	if evidence {
		t.Error("a dry run is evidence")
	}
	if !warnedOf(logs, subject, "held by a real run and not given back while dry_run is on") {
		t.Errorf("logs %q; want a warning that the real run's change stays", logs)
	}
	mux := http.NewServeMux()
	registerActuatorsRoute(mux, dry, acting)
	row := rowOf(actuatorRows(t, get(mux, "/actuators").Body.Bytes()), "memory.high")
	if !row.Built || len(row.Held) != 0 || len(row.Unmanaged) != 1 || row.Unmanaged[0].Subject != subject {
		t.Fatalf("memory.high %+v; want the real run's change shown unmanaged", row)
	}
	w := runtime.Workload{Subject: "pod:other", Name: "other"}
	if _, err := acting.Apply(context.Background(), w, "cpu.limit", 0.5, time.Now().Add(time.Hour), "dry-1"); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(ledger)
	if err != nil || string(after) != string(before) {
		t.Fatalf("ledger file changed by a dry run (%v):\n%s\n→\n%s", err, before, after)
	}
	if b, _ := os.ReadFile(filepath.Join(pod, "memory.high")); strings.TrimSpace(string(b)) != "500000000" {
		t.Fatalf("memory.high %q; want the real run's cap untouched", b)
	}
}
