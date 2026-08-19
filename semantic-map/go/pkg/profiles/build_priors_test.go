package profiles

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/statemap"
)

// TestBuildSeedsStructureAndNoMagnitude exercises the path the daemon actually uses —
// Build() with the same Config literal cmd/agent/main.go constructs — rather
// than calling applyPriorWeights and seedFromOntology directly the way
// TestPerKDSeedingMatchesPriorWeights does.
//
// The distinction matters: the library was always correct, but for a period the
// convergence harness invoked the binary as `-proposer false` (space instead of
// `=`), which makes Go's flag package treat "false" as a positional argument and
// silently drop every flag after it, including -priors and -kd. The daemon then
// started normally on hardcoded ontology defaults. A library-level test could
// not catch that; this test pins the Build-with-full-Config contract, and
// TestRejectsPositionalArgs in cmd/agent covers the CLI side.
//
// What it pins has since inverted. Build used to seed each relationship's strength
// from prior_weights.json, and this test checked the number arrived. Strengths are
// now learned only: a calibration file supplies no magnitude, and a freshly built
// agent must report that it does not yet know what any relationship is worth. The
// failure this guards against is therefore the opposite one — a number appearing
// where nothing has been measured.
func TestBuildSeedsStructureAndNoMagnitude(t *testing.T) {
	pwPath := findPriorWeightsFile(t)

	pw, err := loadPriorWeights(pwPath)
	if err != nil {
		t.Fatalf("loadPriorWeights: %v", err)
	}

	for _, kd := range pw.Distributions {
		kd := kd
		t.Run(kd, func(t *testing.T) {
			cfg := Config{
				DomainSpec:           mustSpec(),
				EMAAlpha:             0.2,
				ConvergenceThreshold: 500,
				MinTrustScore:        0.5,
				PriorWeightsPath:     pwPath,
				KD:                   kd,
				NodeID:               "testnode",
				CgroupRoot:           "/sys/fs/cgroup",
				CollectInterval:      0,
				UseProposer:          false,
			}

			sm, _, err := Build("edge-minimal", cfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}

			edges, err := sm.AllEdges()
			if err != nil {
				t.Fatalf("AllEdges: %v", err)
			}
			if len(edges) == 0 {
				t.Fatal("no edges after Build")
			}

			// The artefact must still parse and must still describe the same structure
			// the map ended up with — otherwise this test would keep passing against a
			// file that had quietly stopped saying anything.
			if len(pw.AgentEdges) == 0 {
				t.Fatalf("artefact declares no agent_edges; nothing pins the structure "+
					"the map was seeded from (kd=%q)", kd)
			}
			declared := map[string]agentEdge{}
			for _, ae := range pw.AgentEdges {
				declared[ae.PropositionID] = ae
			}
			// And it must carry no magnitude to seed from. This is the assertion that
			// would have caught the artefact keeping a prior_weight table long after
			// the daemon stopped reading it.
			raw, err := os.ReadFile(pwPath)
			if err != nil {
				t.Fatalf("re-reading the artefact: %v", err)
			}
			for _, banned := range []string{"prior_strength", "prior_weight", "ema_weight"} {
				if bytes.Contains(raw, []byte(banned)) {
					t.Errorf("artefact still emits %q: a magnitude nothing reads is worse "+
						"than an absent one, because nothing about it looks wrong", banned)
				}
			}

			checked := 0
			for _, e := range edges {
				// Structure arrives: the edge exists, between the declared endpoints,
				// in the declared direction.
				if e.FromID == "" || e.ToID == "" || e.PropositionID == "" {
					t.Errorf("edge %+v is missing structure", e)
				}
				// Magnitude does not.
				if e.Established != nil {
					t.Errorf("%s: Build produced an established strength %.6f; the "+
						"long-run layer is accumulated from this machine's own pairs and "+
						"nothing may seed it", e.PropositionID, *e.Established)
				}
				if e.Assertion != nil {
					t.Errorf("%s: Build produced an assertion %.6f; only an operator "+
						"sets one", e.PropositionID, *e.Assertion)
				}
				if e.Effective != nil {
					t.Errorf("%s: Build produced an effective strength %.6f before any "+
						"observation; a cold-start agent must report that it does not "+
						"know yet", e.PropositionID, *e.Effective)
				}
				if e.Basis != "unknown" {
					t.Errorf("%s: basis %q at cold start, want unknown",
						e.PropositionID, e.Basis)
				}
				if e.NObservations != 0 || e.Confidence != 0 {
					t.Errorf("%s: cold start reports n=%d confidence=%.4f",
						e.PropositionID, e.NObservations, e.Confidence)
				}
				// Structure in the map matches structure in the artefact.
				if ae, ok := declared[e.PropositionID]; ok {
					if ae.FromID != e.FromID || ae.ToID != e.ToID {
						t.Errorf("%s: map has %s→%s, artefact declares %s→%s",
							e.PropositionID, e.FromID, e.ToID, ae.FromID, ae.ToID)
					}
				}
				checked++
			}
			if checked == 0 {
				t.Error("no edges reached the state model")
			}
		})
	}
}

// The global-prior fallback test that stood here is gone with the thing it tested.
// Seeding resolved a magnitude per relationship — per-cluster calibration, else the
// global proposition strength, else a neutral 0.5 — and this test pinned the middle
// rung. There is no chain now: a declaration carries no magnitude at all, which
// TestBuildSeedsStructureAndNoMagnitude asserts directly.

// TestBuildKeepsOntologyAndStorageInAgreement pins the invariant that the prior
// a caller reads from GET /propositions is the one the Reasoner traverses. Per-KD
// seeding writes storage edges; before this was fixed it left the ontology's
// proposition strengths at the global values, so the two disagreed until the
// first tune and the audit trail recorded a transition from an unused number.
func TestBuildKeepsOntologyAndStorageInAgreement(t *testing.T) {
	pwPath := findPriorWeightsFile(t)
	pw, err := loadPriorWeights(pwPath)
	if err != nil {
		t.Fatalf("loadPriorWeights: %v", err)
	}

	for _, kd := range pw.Distributions {
		kd := kd
		t.Run(kd, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.DomainSpec = mustSpec()
			cfg.PriorWeightsPath = pwPath
			cfg.KD = kd

			sm, _, err := Build("edge-minimal", cfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			edges, err := sm.AllEdges()
			if err != nil {
				t.Fatalf("AllEdges: %v", err)
			}
			props, err := sm.Propositions()
			if err != nil {
				t.Fatalf("Propositions: %v", err)
			}
			strength := make(map[string]float64, len(props))
			for _, p := range props {
				strength[p.PropositionID] = p.PriorStrength
			}
			for _, e := range edges {
				got, ok := strength[e.PropositionID]
				if !ok {
					t.Errorf("edge %s has no matching proposition", e.PropositionID)
					continue
				}
				// The two surfaces must report the same number, whatever it is. At cold
				// start that number is nothing on both sides: the edge has no effective
				// strength and the declaration layer reports 0 for it.
				var want float64
				if e.Effective != nil {
					want = *e.Effective
				}
				if diff := got - want; diff > 1e-9 || diff < -1e-9 {
					t.Errorf("%s: proposition reports %.6f, edge effective %.6f — a "+
						"caller reading GET /propositions must see what the Reasoner "+
						"traverses", e.PropositionID, got, want)
				}
			}
		})
	}
}

// TestBuildMakesCostAnswersTraceable pins the property that makes the map
// load-bearing: an answer the agent gives must be re-derivable from the state it
// read. Without it the rationale is prose that nothing can be checked against.
func TestBuildMakesCostAnswersTraceable(t *testing.T) {
	state := statemap.New(statemap.Config{
		ConvergenceObservations: 4,
		AdmitUnknown:            true,
	}, statemap.NewJournal(0))

	cfg := DefaultConfig()
	cfg.DomainSpec = mustSpec()
	cfg.StateMap = state

	sm, _, err := Build("edge-minimal", cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// A property the cost path reads, so the answer has something to stand on.
	spec := mustSpec()
	target := spec.CostModel.ResourceConstruct
	var metric string
	for _, route := range spec.MetricRouting {
		if route.ConstructID == target {
			metric = route.MetricType
			break
		}
	}
	if metric == "" {
		t.Fatalf("no metric routes to the resource construct %q", target)
	}
	if err := state.DeclareProperty(statemap.Property{
		ID: target, Kind: statemap.Derived, Members: []string{metric},
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := state.Observe(metric, 0.6, time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	cost, err := sm.CostOfAction("placement", "self")
	if err != nil {
		t.Fatal(err)
	}
	if cost.DecisionID == "" {
		t.Fatal("cost answer carries no DecisionID: it was computed without the state " +
			"model, so nothing can reproduce it")
	}
	d, ok := state.Journal().Decision(cost.DecisionID)
	if !ok {
		t.Fatalf("decision %s is not in the journal", cost.DecisionID)
	}
	if len(d.PropertiesRead) == 0 {
		t.Error("the recorded decision lists no properties, so the answer cannot be re-derived")
	}
	// The answer in the record must match the answer returned, or the trace is of a
	// different computation than the one the caller saw.
	if rc, okv := d.Answer["resource_cost"].(float64); !okv || rc != cost.ResourceCost {
		t.Errorf("recorded resource_cost %v does not match the returned %.6f",
			d.Answer["resource_cost"], cost.ResourceCost)
	}
	var sawTarget bool
	for _, p := range d.PropertiesRead {
		if p.ID == target {
			sawTarget = true
			if p.Value != cost.ResourceCost {
				t.Errorf("recorded %s=%.6f but the answer reported %.6f",
					target, p.Value, cost.ResourceCost)
			}
		}
	}
	if !sawTarget {
		t.Errorf("the decision does not record reading %s, the property it answered about", target)
	}
}
