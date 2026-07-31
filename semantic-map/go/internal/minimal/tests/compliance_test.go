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

// The relational updater must satisfy the base suite as well as the paired one:
// it is an UpdaterContract first, and the paired path is an extension rather
// than a replacement.
func TestRelationalEMAUpdaterCompliance(t *testing.T) {
	compliance.RunUpdaterCompliance(t, func(t *testing.T) (contracts.UpdaterContract, contracts.StorageContract) {
		s := minimal.NewInMemoryStorage()
		u := minimal.NewRelationalEMAUpdater(s, 0.2, 500, 8, 60)
		return u, s
	})
	compliance.RunRelationalUpdaterCompliance(t, func(t *testing.T) (contracts.RelationalUpdaterContract, contracts.StorageContract) {
		s := minimal.NewInMemoryStorage()
		u := minimal.NewRelationalEMAUpdater(s, 0.2, 500, 8, 60)
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
		r := minimal.NewRuleEngineReasoner(s, o, 0.5, nil, nil)
		r.AttachState(stateFor(t))
		r.AttachState(stateFor(t))
		return r
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
	s := minimal.NewInMemoryStorage()
	o := minimal.NewOntologyFromSpec(mustSpec())
	seedReasonerState(t, s, o)
	state := stateFor(t)
	r := minimal.NewRuleEngineReasoner(s, o, 0.5, nil, nil)
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

// stateFor builds a state model seeded from the spec, for fixtures that construct a
// reasoner directly. Cost is answered from the map, so a reasoner without one is a
// wiring error rather than a valid configuration — the same thing a real daemon
// guarantees by always attaching one.
// reasonerWithState builds a reasoner with the state model attached, which is what a
// daemon always does: cost is answered from the map, so a reasoner without one cannot
// answer at all.
func reasonerWithState(t *testing.T, s *minimal.InMemoryStorage,
	o *minimal.SpecOntology) *minimal.RuleEngineReasoner {
	t.Helper()
	r := minimal.NewRuleEngineReasoner(s, o, 0.5, nil, nil)
	r.AttachState(stateFor(t))
	return r
}

func stateFor(t *testing.T) *statemap.Map {
	t.Helper()
	spec := mustSpec()
	sm := statemap.New(statemap.Config{ConvergenceObservations: 500}, statemap.NewJournal(0))
	if _, err := profiles.SeedStateMap(sm, spec, "", ""); err != nil {
		t.Fatalf("seeding the state model: %v", err)
	}
	return sm
}
