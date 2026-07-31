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
	"github.com/DiyazY/di-agent/pkg/types"
)

func TestInMemoryStorageCompliance(t *testing.T) {
	compliance.RunStorageCompliance(t, func(t *testing.T) contracts.StorageContract {
		return minimal.NewInMemoryStorage()
	})
}

func TestEMAUpdaterCompliance(t *testing.T) {
	compliance.RunUpdaterCompliance(t, func(t *testing.T) (contracts.UpdaterContract, contracts.StorageContract) {
		s := minimal.NewInMemoryStorage()
		u := minimal.NewEMAUpdater(s, 0.2, 500)
		return u, s
	})
}

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
		// The reasoner reads from storage that the ontology has seeded. Build
		// the same wiring the edge-minimal profile uses so the compliance suite
		// exercises a realistic configuration.
		s := minimal.NewInMemoryStorage()
		o := minimal.NewOntologyFromSpec(mustSpec())
		seedReasonerState(t, s, o)
		return minimal.NewRuleEngineReasoner(s, o, 0.5, nil, nil)
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
		return minimal.NewMICorrelationProposer(o, 0.8, 10, 50)
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

// TestReasonerSkipsDeprecatedPropositions verifies the live-ontology
// behavior end-to-end: when the Ontology deprecates a proposition, the
// Reasoner must exclude its edge from cost computation. The graph path
// length drops by one, and the underlying edge stays in storage (preserved
// for audit / replay).
func TestReasonerSkipsDeprecatedPropositions(t *testing.T) {
	s := minimal.NewInMemoryStorage()
	o := minimal.NewOntologyFromSpec(mustSpec())
	seedReasonerState(t, s, o)
	r := minimal.NewRuleEngineReasoner(s, o, 0.5, nil, nil)

	before, err := r.CostOfAction("pod-scheduling", "node_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(before.GraphPathUsed) == 0 {
		t.Fatal("expected non-empty graph path before deprecation")
	}

	// Deprecate one proposition (P1: SC→RC positive — the first one in
	// diSelectPropositions order).
	if err := o.Deprecate(firstSpecProp(), "spurious in this deployment"); err != nil {
		t.Fatal(err)
	}

	after, err := r.CostOfAction("pod-scheduling", "node_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(after.GraphPathUsed) != len(before.GraphPathUsed)-1 {
		t.Errorf("graph path should shrink by 1 after deprecation; got %d → %d",
			len(before.GraphPathUsed), len(after.GraphPathUsed))
	}
	for _, entry := range after.GraphPathUsed {
		if contains(entry, "[P1]") {
			t.Errorf("deprecated proposition P1 still appears in graph path: %q", entry)
		}
	}

	// The edge must still be in storage — soft-delete, not removal.
	edges, _ := s.AllEdges()
	stillPresent := false
	for _, e := range edges {
		if e.PropositionID == firstSpecProp() {
			stillPresent = true
			break
		}
	}
	if !stillPresent {
		t.Error("deprecated proposition's edge was removed from storage — soft-delete must preserve it")
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

// seedReasonerState seeds storage with one node per construct and one edge per
// proposition, mirroring what profiles.seedFromOntology does at daemon startup.
// Without seeding, the reasoner has nothing to traverse and GraphPathUsed
// would be empty.
func seedReasonerState(t *testing.T, s *minimal.InMemoryStorage, o *minimal.SpecOntology) {
	t.Helper()
	cs, err := o.Constructs()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if err := s.PutNode(&types.NodeDescriptor{
			NodeID:        c.ConstructID,
			ConstructType: c.Name,
			PriorValue:    0.5,
			EMAValue:      0.5,
		}); err != nil {
			t.Fatal(err)
		}
	}
	ps, err := o.Propositions()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if err := s.PutEdge(&types.EdgeDescriptor{
			FromID:        p.FromConstruct,
			ToID:          p.ToConstruct,
			PropositionID: p.PropositionID,
			Direction:     p.Direction,
			PriorWeight:   p.PriorStrength,
			EMAWeight:     p.PriorStrength,
		}); err != nil {
			t.Fatal(err)
		}
	}
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
