package agentmain

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/collectors"
	"github.com/DiyazY/di-agent/pkg/statemap"
	"github.com/DiyazY/di-agent/pkg/types"
)

// TestSpecReachesConfig guards the class of defect this project has hit three
// times: a setting is declared, parsed, logged, and never passed to the thing it
// configures. Each instance produced a daemon that ran normally while ignoring the
// operator — the tuner flag wired DisabledTuner, the proposer's space-separated value
// discarded every flag after it, and a since-removed machine-class flag seeded
// class-averaged priors while logging that it had applied class-specific ones.
//
// Comparing the spec fields against the Config fields they must populate catches the
// next one without waiting for a measurement to look wrong.
func TestSpecReachesConfig(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	// Each entry is a spec path as main.go reads it and the Config field its value
	// must reach. Extend this when adding a setting that configures the profile.
	wiring := map[string]string{
		"cfg.Node.IngestScope": "AcceptForeignSamples:",
		"cfg.Map.PairWindow":   "PairWindowSeconds:",
		"cfg.Map.PairSupport":  "PairMinSupport:",
		"cfg.Map.PairHistory":  "PairWindow:",
		"cfg.Map.Tuner":        "UseRuleBasedTuner:",
		"prop.Enabled":         "UseProposer:",
		"cfg.Profile.KD":       "KD:",
		"cfg.Profile.Priors":   "PriorWeightsPath:",
		"cfg.Domain.Path":      "DomainSpec:",
	}
	for path, field := range wiring {
		if !strings.Contains(body, path) {
			t.Errorf("%s is no longer read by main.go; update this test or restore it", path)
			continue
		}
		if !strings.Contains(body, field) {
			t.Errorf("%s is read but no Config literal sets %s — the daemon will "+
				"accept the setting and ignore it", path, field)
		}
	}
}

// TestNodeUIReachesTheConsole guards the one line that decides whether a real
// agent serves /ui/; TestStaticUI_FollowsNodeUI covers what registerConsole does
// with the value, and this that main.go still hands it the spec's.
func TestNodeUIReachesTheConsole(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "registerConsole(mux, cfg.Node.UI)") {
		t.Error("main.go no longer passes node.ui to registerConsole: the console would ignore the spec")
	}
}

// TestSaveIfChangedSkipsAnUnchangedMap: a quiet node does not rewrite its snapshot
// every tick; the first tick saves once, a moved revision saves again.
func TestSaveIfChangedSkipsAnUnchangedMap(t *testing.T) {
	state := newTestState(t)
	path := filepath.Join(t.TempDir(), "state.json")
	var last saveMark

	if saved, err := saveIfChanged(state, path, &last); !saved || err != nil {
		t.Fatalf("first call = (%v, %v); want (true, nil)", saved, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved, err := saveIfChanged(state, path, &last); saved || err != nil {
		t.Fatalf("unchanged call = (%v, %v); want (false, nil)", saved, err)
	}
	fi2, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fi2.ModTime().Equal(fi.ModTime()) {
		t.Error("an unchanged map rewrote the file")
	}
	if err := state.Observe("cpu", 0.5, time.Now()); err != nil {
		t.Fatal(err)
	}
	if saved, err := saveIfChanged(state, path, &last); !saved || err != nil {
		t.Fatalf("after a change = (%v, %v); want (true, nil)", saved, err)
	}
}

// TestSaveIfChangedSavesADecisionOnlyChange: a decision is journalled without moving
// the map's revision, and it is what a restart must be able to reproduce.
func TestSaveIfChangedSavesADecisionOnlyChange(t *testing.T) {
	state := newTestState(t)
	path := filepath.Join(t.TempDir(), "state.json")
	var last saveMark
	if saved, err := saveIfChanged(state, path, &last); !saved || err != nil {
		t.Fatalf("first call = (%v, %v); want (true, nil)", saved, err)
	}
	rev := state.Revision()
	state.Decide("dec-1", "estimate", "anything?").Commit(map[string]any{"answer": "no"})
	if state.Revision() != rev {
		t.Fatalf("precondition: a decision moved the revision")
	}
	if saved, err := saveIfChanged(state, path, &last); !saved || err != nil {
		t.Fatalf("after a decision = (%v, %v); want (true, nil)", saved, err)
	}
	if saved, _ := saveIfChanged(state, path, &last); saved {
		t.Error("a quiet tick after the decision saved again")
	}
}

// TestShutdownSaveIsUnconditional: the shutdown save is state.Save, not the skipping
// helper, so it writes even when the revision has not moved.
func TestShutdownSaveIsUnconditional(t *testing.T) {
	state := newTestState(t)
	path := filepath.Join(t.TempDir(), "state.json")
	var last saveMark
	if _, err := saveIfChanged(state, path, &last); err != nil {
		t.Fatal(err)
	}
	if saved, _ := saveIfChanged(state, path, &last); saved {
		t.Fatal("expected the second call to skip")
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	if err := state.Save(path); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
		t.Errorf("shutdown save did not rewrite the file: %v, %v", fi, err)
	}
}

// TestServerHasReadTimeouts: a client that opens a connection and sends nothing must
// not hold it forever.
func TestServerHasReadTimeouts(t *testing.T) {
	s := newServer(":0", http.NewServeMux())
	if s.ReadHeaderTimeout != 10*time.Second || s.ReadTimeout != 30*time.Second {
		t.Errorf("ReadHeaderTimeout %v, ReadTimeout %v; want 10s and 30s", s.ReadHeaderTimeout, s.ReadTimeout)
	}
}

// TestSaveLoopFollowsThePaceAndLeavesShutdownToMain: under slow the periodic save
// runs at four times its cadence, and the loop never saves on its way out — the
// shutdown save is main's, done synchronously after cancel.
func TestSaveLoopFollowsThePaceAndLeavesShutdownToMain(t *testing.T) {
	f := newPaceFixture(t)
	// Admitted before the calm: an admission wakes the pace.
	if err := f.m.Observe("cpu_utilization", 0.4, f.now); err != nil {
		t.Fatal(err)
	}
	f.minutes(10)
	f.wantPace(paceSlow)
	path := filepath.Join(t.TempDir(), "state.json")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startSaveLoop(ctx, f.m, path, 25*time.Millisecond, f.p)

	time.Sleep(60 * time.Millisecond)
	if _, err := os.Stat(path); err == nil {
		t.Fatal("saved within 60ms under slow; the slow cadence is 100ms")
	}
	var mtime time.Time
	for deadline := time.Now().Add(140 * time.Millisecond); ; {
		if st, err := os.Stat(path); err == nil {
			mtime = st.ModTime()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no save by 200ms under slow")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Something to save, then the end: a save on exit would rewrite the file.
	if err := f.m.Observe("cpu_utilization", 0.5, f.now); err != nil {
		t.Fatal(err)
	}
	f.wantPace(paceSlow)
	cancel()
	time.Sleep(60 * time.Millisecond)
	if st, err := os.Stat(path); err != nil || !st.ModTime().Equal(mtime) {
		t.Fatalf("the save loop wrote on exit (err=%v); the shutdown save is main's", err)
	}

	b := newPaceFixture(t)
	if err := b.m.Observe("cpu_utilization", 0.4, b.now); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(t.TempDir(), "state.json")
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	startSaveLoop(ctx2, b.m, path, 25*time.Millisecond, b.p)
	for deadline := time.Now().Add(100 * time.Millisecond); ; {
		if _, err := os.Stat(path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no save within 100ms at base; the cadence is 25ms")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// countingCollector counts Collect calls and collects nothing.
type countingCollector struct{ n atomic.Int64 }

func (c *countingCollector) Collect() ([]*types.MetricSample, error) { c.n.Add(1); return nil, nil }
func (*countingCollector) SourceID() string                          { return "counting" }
func (*countingCollector) AvailableMetrics() []types.MetricType      { return nil }
func (*countingCollector) Health() error                             { return nil }

// TestCollectionLoopFollowsThePace: the collection interval is the pace's.
func TestCollectionLoopFollowsThePace(t *testing.T) {
	collects := func(p *Pace) int64 {
		c := &countingCollector{}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			runCollectionLoop(ctx, nil, c, func() []collectors.MemberStatus { return nil },
				statemap.New(statemap.Config{}, statemap.NewJournal(0)), 20*time.Millisecond, &sync.Mutex{}, p)
		}()
		time.Sleep(200 * time.Millisecond)
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("the collection loop did not return after cancel")
		}
		return c.n.Load()
	}
	slow := newPaceFixture(t)
	slow.minutes(10)
	slow.wantPace(paceSlow)
	if n := collects(slow.p); n > 3 {
		t.Errorf("%d collects in 200ms under slow (80ms); want ≤ 3", n)
	}
	if n := collects(newPaceFixture(t).p); n < 5 {
		t.Errorf("%d collects in 200ms at base (20ms); want ≥ 5", n)
	}
}
