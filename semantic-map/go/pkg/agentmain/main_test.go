package agentmain

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	state.Decide("dec-1", "anything?").Commit(map[string]any{"answer": "no"})
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
