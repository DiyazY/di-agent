package agentmain

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/admission"
	"github.com/DiyazY/di-agent/pkg/peers"
	"github.com/DiyazY/di-agent/pkg/policy"
	"github.com/DiyazY/di-agent/pkg/relief"
	"github.com/DiyazY/di-agent/pkg/runtime"
	"github.com/DiyazY/di-agent/pkg/spec"
	"github.com/DiyazY/di-agent/pkg/statemap"
)

// loopsRig is what main builds for the loops on node n1: the relief loops over a
// dry-run runtime, the admission tracker, and a peer registry, all over m.
type loopsRig struct {
	m     *statemap.Map
	loops []*relief.Loop
	tr    *admission.Tracker
	reg   *peers.Registry
	peer  string
}

func newLoopsRig(t *testing.T) *loopsRig {
	t.Helper()
	s := spec.Default()
	s.Node.ID = "n1"
	s.Loops.Relief.Enabled = true
	rt, err := runtime.Build("dry-run", runtime.Env{NodeID: "n1", Spec: s.Runtime})
	if err != nil {
		t.Fatal(err)
	}
	acting, _, err := buildActing(s, rt, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	pol, err := policy.New(s.Policy)
	if err != nil {
		t.Fatal(err)
	}
	m := statemap.New(statemap.Config{Owner: "n1"}, statemap.NewJournal(0))
	loops, err := buildRelief(s, rt, acting, pol, m, false, nil, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	reg := peers.NewRegistry()
	p, err := reg.Add("http://node_2:8080", "")
	if err != nil {
		t.Fatal(err)
	}
	return &loopsRig{m: m, loops: loops, tr: buildAdmission(s, m, rt, loops, reg).tracker, reg: reg, peer: p.ID}
}

func (r *loopsRig) capture() func() (json.RawMessage, error) {
	return captureLoops("n1", r.loops, r.tr, r.reg)
}

// learned gives the rig state worth keeping: an effect and a tally on the first
// lever, an outcome, and its peer set to 0.7 then penalised once, to 0.65.
func (r *loopsRig) learned(t *testing.T) {
	t.Helper()
	r.loops[0].Restore(relief.Memory{Effects: map[string]map[string]float64{"pod:a": {relief.BasisUsage: 0.6}},
		Acts: 3, Kept: 2, Reverted: 1})
	r.tr.Restore(admission.Memory{Outcomes: []admission.Outcome{{Name: "web", AcceptID: "admit-1.1",
		OutcomeID: "admit-1.2", Subject: "pod:web", Result: admission.Held, At: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)}}})
	if err := r.reg.SetTrust(r.peer, 0.7); err != nil {
		t.Fatal(err)
	}
	if err := r.reg.Penalise(r.peer, time.Date(2026, 11, 5, 12, 5, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureAndRestoreLoops(t *testing.T) {
	a := newLoopsRig(t)
	a.learned(t)
	raw, err := a.capture()()
	if err != nil {
		t.Fatal(err)
	}

	b := newLoopsRig(t)
	if err := restoreLoops(raw, "n1", b.loops, b.tr, b.reg, t.Logf); err != nil {
		t.Fatal(err)
	}
	for i := range a.loops {
		if got, want := b.loops[i].Memory(), a.loops[i].Memory(); !reflect.DeepEqual(got, want) {
			t.Errorf("%s memory %+v; want %+v", a.loops[i].Lever().Action, got, want)
		}
	}
	if got, want := b.tr.Outcomes(), a.tr.Outcomes(); !reflect.DeepEqual(got, want) {
		t.Errorf("outcomes %+v; want %+v", got, want)
	}
	if got, want := b.reg.Trusts(), a.reg.Trusts(); !reflect.DeepEqual(got, want) {
		t.Errorf("peer trust %+v; want %+v, penalty time included", got, want)
	}
	if err := restoreLoops(nil, "n1", b.loops, b.tr, b.reg, t.Logf); err != nil {
		t.Errorf("a cold start: %v", err)
	}
	if err := restoreLoops(json.RawMessage(`{`), "n1", b.loops, b.tr, b.reg, t.Logf); err == nil {
		t.Error("a blob that does not decode was restored")
	}
}

func TestLoopsFromAnotherNodeAreIgnored(t *testing.T) {
	a := newLoopsRig(t)
	a.learned(t)
	raw, err := captureLoops("n2", a.loops, a.tr, a.reg)()
	if err != nil {
		t.Fatal(err)
	}
	b := newLoopsRig(t)
	var logged []string
	logf := func(f string, args ...any) { logged = append(logged, fmt.Sprintf(f, args...)) }
	if err := restoreLoops(raw, "n1", b.loops, b.tr, b.reg, logf); err != nil {
		t.Fatal(err)
	}
	mem := b.loops[0].Memory()
	if len(mem.Effects) != 0 || mem.Acts != 0 || mem.Kept != 0 {
		t.Errorf("memory %+v; another node's learning was adopted", mem)
	}
	if len(b.tr.Outcomes()) != 0 {
		t.Errorf("outcomes %+v; want none", b.tr.Outcomes())
	}
	if d, _ := b.reg.Get(b.peer); d.Trust != 0.5 {
		t.Errorf("peer trust %v; want the default 0.5", d.Trust)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], `"n2"`) || !strings.Contains(logged[0], `"n1"`) {
		t.Errorf("logged %q; want one line naming both nodes", logged)
	}
}

// TestRestoredReliefMemoryIsSavedAgain: the save path carries the loops' blob
// end to end — captured, saved, loaded, restored, and captured the same again.
func TestRestoredReliefMemoryIsSavedAgain(t *testing.T) {
	a := newLoopsRig(t)
	a.learned(t)
	want, err := a.capture()()
	if err != nil {
		t.Fatal(err)
	}
	a.m.LoopsPersistence(a.capture())
	path := filepath.Join(t.TempDir(), "state.json")
	if err := a.m.Save(path); err != nil {
		t.Fatal(err)
	}

	b := newLoopsRig(t)
	if _, err := b.m.Load(path); err != nil {
		t.Fatal(err)
	}
	if err := restoreLoops(b.m.LoopsPersistence(b.capture()), "n1", b.loops, b.tr, b.reg, t.Logf); err != nil {
		t.Fatal(err)
	}
	got, err := b.capture()()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("captured after restore %s; want %s", got, want)
	}
}
