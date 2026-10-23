package agentmain

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/actuators"
	"github.com/DiyazY/di-agent/pkg/runtime"
	"github.com/DiyazY/di-agent/pkg/spec"
)

func TestActuatorsRoute(t *testing.T) {
	mux, _, _, acting := actingMuxWith(t, false)
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
	if len(all) != 1 || all[0].Descriptor.Action != "cpu.limit" || all[0].Descriptor.Unit != "cores" ||
		all[0].Descriptor.Relieves != "cpu_contention_ratio" || all[0].Can != nil {
		t.Fatalf("actuators %+v; want cpu.limit's descriptor, with no can without a subject", all)
	}
	if h := all[0].Held; len(h) != 1 || h[0].Subject != "pod:b" || h[0].Applied != 0.5 || h[0].Original != 1 || !h[0].OriginalSet {
		t.Fatalf("held %+v; want the change to pod:b, 1 → 0.5", h)
	}

	one := read("/actuators?subject=pod:a")
	if len(one) != 1 || one[0].Can == nil || *one[0].Can != "no cpu limit" || len(one[0].Held) != 0 {
		t.Fatalf("pod:a %+v; want can naming why not, and nothing held on it", one)
	}
	one = read("/actuators?subject=pod:b")
	if len(one) != 1 || one[0].Can == nil || *one[0].Can != "" || len(one[0].Held) != 1 {
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
		s := s
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
		s := s
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
	if got := ledgerPath("/data/state.json"); got != "/data/actions.json" {
		t.Errorf("ledgerPath = %s", got)
	}
	if got := ledgerPath(""); got != "/var/lib/di-agent/actions.json" {
		t.Errorf("ledgerPath(\"\") = %s", got)
	}
}
