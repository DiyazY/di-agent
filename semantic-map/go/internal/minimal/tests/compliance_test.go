package minimal_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/compliance"
	"github.com/DiyazY/di-agent/internal/minimal"
	"github.com/DiyazY/di-agent/pkg/contracts"
	"github.com/DiyazY/di-agent/pkg/profiles"
	"github.com/DiyazY/di-agent/pkg/statemap"
)

func TestCgroupCollectorCompliance(t *testing.T) {
	compliance.RunCollectorCompliance(t, func(t *testing.T) contracts.CollectorContract {
		root := newFakeCgroupRoot(t)
		c := minimal.NewCgroupCollector("test-node", root)
		// Warm up: first Collect() stores the initial snapshot.
		// The second call (from the compliance suite) will have a non-zero
		// delta and return CPU samples alongside memory.
		c.Collect() //nolint:errcheck
		time.Sleep(2 * time.Millisecond)
		return c
	})
}

func TestCgroupCollectorSubjectsCompliance(t *testing.T) {
	compliance.RunCollectorCompliance(t, func(t *testing.T) contracts.CollectorContract {
		root := newFakeCgroupRoot(t)
		pod := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice", "kubepods-burstable-pod8f3c1234_aaaa_bbbb_cccc_1234567890ab.slice")
		if err := os.MkdirAll(pod, 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(pod, "cpu.stat"), "usage_usec 500000\nnr_periods 10\nnr_throttled 1\n")
		mustWrite(t, filepath.Join(pod, "memory.current"), "268435456\n")
		mustWrite(t, filepath.Join(pod, "memory.max"), "max\n")
		c := minimal.NewCgroupCollectorWithOptions("test-node", root, minimal.CgroupOptions{Subjects: true, MaxSubjects: 8, MemTotalBytes: 8 << 30})
		c.Collect() //nolint:errcheck
		time.Sleep(2 * time.Millisecond)
		return c
	})
}

func TestNetdataCollectorCompliance(t *testing.T) {
	// Fake Netdata server responding to all three charts.
	srv := httptest.NewServer(netdataFakeHandler(t))
	defer srv.Close()

	compliance.RunCollectorCompliance(t, func(t *testing.T) contracts.CollectorContract {
		return minimal.NewNetdataCollector("test-node", srv.URL, nil)
	})
}

// netdataFakeHandler returns an http.Handler that responds with canned Netdata JSON.
func netdataFakeHandler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/data", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("chart") {
		case "system.cpu":
			fmt.Fprint(w, `{"result":{"labels":["time","user","system","idle"],"data":[[1703123456,2.5,0.5,96.9]]}}`)
		case "system.ram":
			fmt.Fprint(w, `{"result":{"labels":["time","free","used","cached","buffers"],"data":[[1703123456,4096.0,2048.0,1024.0,512.0]]}}`)
		case "system.net":
			fmt.Fprint(w, `{"result":{"labels":["time","InOctets","OutOctets"],"data":[[1703123456,8.0,-6.0]]}}`)
		default:
			http.NotFound(w, r)
		}
	})
	return mux
}

func TestSpecOntologyCompliance(t *testing.T) {
	compliance.RunOntologyCompliance(t, func(t *testing.T) contracts.OntologyContract {
		return minimal.NewOntologyFromSpec(mustSpec())
	})
}

func TestRuleEngineReasonerCompliance(t *testing.T) {
	compliance.RunReasonerCompliance(t, func(t *testing.T) contracts.ReasonerContract {
		// The reasoner needs the specification (for the cost roles) and a state model
		// to answer from. It needs nothing else: this fixture used to build a storage
		// graph and seed it, which passed and proved nothing, because no cost answer
		// had read from there since the state model became the single source.
		return reasonerWithState(t)
	})
}

func TestDisabledProposerCompliance(t *testing.T) {
	compliance.RunProposerCompliance(t, func(t *testing.T) contracts.ProposerContract {
		return minimal.NewDisabledProposer()
	})
}

func TestMICorrelationProposerCompliance(t *testing.T) {
	compliance.RunProposerCompliance(t, func(t *testing.T) contracts.ProposerContract {
		o := minimal.NewOntologyFromSpec(mustSpec())
		return minimal.NewMICorrelationProposer(minimal.LookupOntology(o), 0.8, 10, 50, 0)
	})
}

func TestRuleBasedTunerCompliance(t *testing.T) {
	compliance.RunTunerCompliance(t, func(t *testing.T) contracts.TunerContract {
		return minimal.NewRuleBasedTunerFromSpec(mustSpec())
	})
}

func TestDisabledTunerCompliance(t *testing.T) {
	compliance.RunTunerCompliance(t, func(t *testing.T) contracts.TunerContract {
		return minimal.NewDisabledTuner()
	})
}

// TestReasonerSkipsRetiredRelationships verifies that retirement removes a claim
// from reasoning: the reasoner reads relationships from the state model, so a
// retired one leaves the traversal path and stops contributing to any answer.
//
// The retirement is applied to the state model rather than to the ontology, because
// the state model is what the reasoner reads. Going through the facade
// (SemanticMap.Deprecate) does both; a caller reaching past it into the ontology
// alone changes what Propositions() reports and no decision, which is the failure
// mode the facade exists to prevent.
func TestReasonerSkipsRetiredRelationships(t *testing.T) {
	state := stateFor(t)
	r := minimal.NewRuleEngineReasoner(mustSpec(), 0.5, nil, nil)
	r.AttachState(state)

	before, err := r.CostOfAction("pod-scheduling", "node_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(before.GraphPathUsed) == 0 {
		t.Fatal("expected non-empty graph path before deprecation")
	}

	// Retire one relationship the cost path reads.
	var retired string
	for _, rel := range state.Relationships("", "") {
		if rel.To == mustSpec().CostModel.ResourceConstruct ||
			rel.To == mustSpec().CostModel.PressureConstruct {
			retired = rel.ID
			break
		}
	}
	if retired == "" {
		t.Skip("no relationship terminates at a cost construct in this spec")
	}
	if err := state.RetireRelationship(retired, "spurious in this deployment", "operator:test"); err != nil {
		t.Fatal(err)
	}

	after, err := r.CostOfAction("pod-scheduling", "node_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(after.GraphPathUsed) != len(before.GraphPathUsed)-1 {
		t.Errorf("graph path should shrink by 1 after retirement; got %d → %d",
			len(before.GraphPathUsed), len(after.GraphPathUsed))
	}
	for _, entry := range after.GraphPathUsed {
		if contains(entry, retired) {
			t.Errorf("retired relationship %s still appears in the graph path: %q", retired, entry)
		}
	}

	// Retirement is soft: the relationship stays retrievable so a decision taken
	// before it remains reconstructible.
	rel, ok := state.Relationship(retired)
	if !ok {
		t.Fatalf("retired relationship %s is gone from the map; soft deletion must keep it", retired)
	}
	if rel.RetiredReason == "" {
		t.Error("retired relationship carries no reason")
	}
}

// contains is a small helper to avoid importing "strings" just for one use.
func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// newFakeCgroupRoot creates a temp directory with valid cgroups v2 files
// so CgroupCollector can be exercised without a real kernel cgroup mount.
func newFakeCgroupRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	mustWrite(t, filepath.Join(root, "cpu.stat"),
		"usage_usec 1000000\n"+
			"user_usec 800000\n"+
			"system_usec 200000\n"+
			"nr_periods 1000\n"+
			"nr_throttled 50\n"+
			"throttled_usec 25000\n",
	)
	mustWrite(t, filepath.Join(root, "memory.current"), "2147483648\n") // 2 GB
	mustWrite(t, filepath.Join(root, "memory.max"), "8589934592\n")     // 8 GB

	return root
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("mustWrite %s: %v", path, err)
	}
}

// reasonerWithState builds a reasoner the way a daemon does: the specification, for
// the cost roles, and a state model to answer from. Nothing else — cost is read from
// the map, so a reasoner without one cannot answer at all, and there is no fixture for
// that configuration.
func reasonerWithState(t *testing.T) *minimal.RuleEngineReasoner {
	t.Helper()
	r := minimal.NewRuleEngineReasoner(mustSpec(), 0.5, nil, nil)
	r.AttachState(stateFor(t))
	return r
}

// stateFor builds a state model seeded from the spec, which is what the daemon attaches
// at startup.
func stateFor(t *testing.T) *statemap.Map {
	t.Helper()
	return stateForConvergence(t, 500)
}

// stateForConvergence is the same with a chosen convergence target, so a scenario can
// let confidence saturate inside a short observation window.
func stateForConvergence(t *testing.T, convergence float64) *statemap.Map {
	t.Helper()
	sm := statemap.New(statemap.Config{
		Owner:                   "scenario-node",
		ConvergenceObservations: int(convergence),
		Alpha:                   0.2,
		AdmitUnknown:            true,
		Learn:                   true,
		LearnConfig:             statemap.LearnConfig{PairWindowSeconds: 15, MinSupport: 4, Window: 60},
	}, statemap.NewJournal(0))
	if _, err := profiles.SeedStateMap(sm, mustSpec(), "", ""); err != nil {
		t.Fatalf("seeding the state model: %v", err)
	}
	return sm
}
