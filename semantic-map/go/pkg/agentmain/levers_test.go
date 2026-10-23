package agentmain

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/actuators"
	"github.com/DiyazY/di-agent/pkg/policy"
	"github.com/DiyazY/di-agent/pkg/runtime"
	"github.com/DiyazY/di-agent/pkg/spec"
	"github.com/DiyazY/di-agent/pkg/statemap"
)

// TestLeverUnknownActionRefused: a lever whose action no actuator performs
// refuses start, naming the lever and what is registered.
func TestLeverUnknownActionRefused(t *testing.T) {
	rt, err := runtime.Build("dry-run", runtime.Env{NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	s := spec.Default()
	s.Node.ID = "n1"
	l := s.Loops.Relief.Levers[0]
	l.Action = "memory.hgh"
	s.Loops.Relief.Levers = []spec.Lever{s.Loops.Relief.Levers[0], l}
	_, _, err = buildActing(s, rt, t.Logf)
	want := "loops.relief.levers[1] memory.hgh: no such actuator (registered: " + strings.Join(actuators.Names(), ", ") + ")"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v; want %q", err, want)
	}
}

// TestTheLedgerIsWhereTheSpecSays: actuation.ledger, when set, is the ledger
// file whatever persist.file is.
func TestTheLedgerIsWhereTheSpecSays(t *testing.T) {
	dry, err := runtime.Build("dry-run", runtime.Env{NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	s := spec.Default()
	s.Node.ID, s.Runtime.Kind = "n1", "kubernetes"
	s.Loops.Relief.Enabled, s.Loops.Relief.Levers = true, s.Loops.Relief.Levers[:1]
	s.Actuation.Ledger = filepath.Join(t.TempDir(), "elsewhere.json")
	acting, evidence, err := buildActing(s, named{dry}, t.Logf)
	if err != nil || !evidence {
		t.Fatalf("evidence %v err %v", evidence, err)
	}
	w := runtime.Workload{Subject: "pod:x", Resources: map[string]runtime.Resource{"cpu": {Limit: f64(1)}}}
	if _, err := acting.Apply(context.Background(), w, "cpu.limit", 0.5, time.Time{}, ""); err != nil {
		t.Fatal(err)
	}
	l, err := actuators.OpenLedger(s.Actuation.Ledger)
	if err != nil || len(l.All()) != 1 {
		t.Fatalf("ledger at actuation.ledger holds %v (err %v); want the change", l, err)
	}
}

// TestDryRunOnARealRuntime: loops.relief.dry_run on a runtime that changes the
// system keeps every change in memory and none of it is evidence.
func TestDryRunOnARealRuntime(t *testing.T) {
	dry, err := runtime.Build("dry-run", runtime.Env{NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	s := spec.Default()
	s.Node.ID, s.Runtime.Kind = "n1", "kubernetes"
	s.Loops.Relief.Enabled, s.Loops.Relief.Levers = true, s.Loops.Relief.Levers[:1]
	yes := true
	s.Loops.Relief.DryRun = &yes
	s.Actuation.Ledger = filepath.Join(t.TempDir(), "ledger.json")
	acting, evidence, err := buildActing(s, named{dry}, t.Logf)
	if err != nil || evidence {
		t.Fatalf("evidence %v err %v; want a dry run's", evidence, err)
	}
	w := runtime.Workload{Subject: "pod:x", Resources: map[string]runtime.Resource{"cpu": {Limit: f64(1)}}}
	if _, err := acting.Apply(context.Background(), w, "cpu.limit", 0.5, time.Time{}, ""); err != nil {
		t.Fatal(err)
	}
	if l, err := actuators.OpenLedger(s.Actuation.Ledger); err != nil || len(l.All()) != 0 {
		t.Fatalf("on-disk ledger %v (err %v); a dry run writes none", l, err)
	}
}

// TestReliefRouteListsLevers: GET /relief is every lever's status in lever
// order, each naming its action; pause and resume act on all of them.
func TestReliefRouteListsLevers(t *testing.T) {
	s := spec.Default()
	s.Node.ID = "n1"
	second := s.Loops.Relief.Levers[0]
	second.Action = "cpu.limit.second"
	s.Loops.Relief.Levers = []spec.Lever{s.Loops.Relief.Levers[0], second}
	rt, err := runtime.Build("dry-run", runtime.Env{NodeID: "n1", Spec: s.Runtime})
	if err != nil {
		t.Fatal(err)
	}
	pol, err := policy.New(s.Policy)
	if err != nil {
		t.Fatal(err)
	}
	cpu, err := actuators.Build("cpu.limit", actuators.Env{NodeID: "n1", Runtime: rt})
	if err != nil {
		t.Fatal(err)
	}
	d := cpu.Describe()
	d.Action = second.Action
	other := actuators.Static(d, nil, func(runtime.Workload) (float64, bool) { return 0, false })
	acting := actuators.NewActing("n1", actuators.MemoryLedger(), actuators.DryRun(cpu), other)
	loops, err := buildRelief(s, rt, acting, pol, statemap.New(statemap.Config{Owner: "n1"}, nil), false, nil, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerActingRoutes(mux, rt, acting, pol, loops)

	levers := func() []struct {
		Action string `json:"action"`
		Paused bool   `json:"paused"`
	} {
		t.Helper()
		rec := get(mux, "/relief")
		var body struct {
			Levers []struct {
				Action string `json:"action"`
				Paused bool   `json:"paused"`
			} `json:"levers"`
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /relief = %d: %s", rec.Code, rec.Body)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Levers
	}
	got := levers()
	if len(got) != 2 || got[0].Action != "cpu.limit" || got[1].Action != "cpu.limit.second" {
		t.Fatalf("levers = %+v; want cpu.limit then cpu.limit.second", got)
	}
	if rec := post(mux, "/relief/pause", true); rec.Code != http.StatusOK {
		t.Fatalf("pause = %d", rec.Code)
	}
	for _, l := range levers() {
		if !l.Paused {
			t.Errorf("%s not paused", l.Action)
		}
	}
	post(mux, "/relief/resume", true)
	for _, l := range levers() {
		if l.Paused {
			t.Errorf("%s still paused", l.Action)
		}
	}

	// A revert finds its action in whichever loop holds it.
	c := runtime.Workload{Subject: "pod:c", Name: "c"}
	rt.(*runtime.DryRun).Put(c)
	if _, err := acting.Apply(context.Background(), c, second.Action, 0.25, time.Now().Add(time.Hour), "act-2"); err != nil {
		t.Fatal(err)
	}
	for _, l := range loops {
		if err := l.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	var acts []struct {
		ID    string `json:"id"`
		Entry struct {
			Action string `json:"action"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(get(mux, "/relief/actions").Body.Bytes(), &acts); err != nil || len(acts) != 1 || acts[0].Entry.Action != second.Action {
		t.Fatalf("actions = %+v, err %v; want the second lever's", acts, err)
	}
	if rec := post(mux, "/relief/actions/"+acts[0].ID+"/revert", true); rec.Code != http.StatusOK {
		t.Fatalf("revert = %d: %s", rec.Code, rec.Body)
	}
	if rec := post(mux, "/relief/actions/nope/revert", true); rec.Code != http.StatusNotFound {
		t.Errorf("unknown id = %d, want 404", rec.Code)
	}
}
