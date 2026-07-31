package profiles

import (
	"testing"
)

// TestBuildAppliesPerKDPriors exercises the path the daemon actually uses —
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
func TestBuildAppliesPerKDPriors(t *testing.T) {
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

			perKD := pw.DistributionEdgeWeights[kd]
			if len(perKD) == 0 {
				t.Fatalf("no per-KD edge weights for %q", kd)
			}

			checked := 0
			for _, e := range edges {
				key := edgeKey(e.FromID, e.ToID, e.PropositionID)
				want, ok := perKD[key]
				if !ok {
					t.Errorf("edge key %q absent from prior_weights.json", key)
					continue
				}
				if diff := e.PriorWeight - want.PriorWeight; diff > 1e-6 || diff < -1e-6 {
					t.Errorf("%s: Build seeded PriorWeight=%.6f, want %.6f",
						e.PropositionID, e.PriorWeight, want.PriorWeight)
				}
				// EMA starts equal to the prior, so a mis-seeded prior would
				// also corrupt the cold-start effective weight.
				if diff := e.EMAWeight - want.PriorWeight; diff > 1e-6 || diff < -1e-6 {
					t.Errorf("%s: Build seeded EMAWeight=%.6f, want %.6f (cold start)",
						e.PropositionID, e.EMAWeight, want.PriorWeight)
				}
				checked++
			}
			if want := len(pw.Propositions); checked != want {
				t.Errorf("checked %d edges, want %d", checked, want)
			}
		})
	}
}

// TestBuildWithoutKDUsesGlobalPriors guards the fallback: with -priors but no
// -kd, edges take the global calibrated proposition strengths, not the
// ontology's hardcoded defaults.
func TestBuildWithoutKDUsesGlobalPriors(t *testing.T) {
	pwPath := findPriorWeightsFile(t)
	pw, err := loadPriorWeights(pwPath)
	if err != nil {
		t.Fatalf("loadPriorWeights: %v", err)
	}

	cfg := DefaultConfig()
	cfg.DomainSpec = mustSpec()
	cfg.PriorWeightsPath = pwPath
	cfg.KD = ""

	sm, _, err := Build("edge-minimal", cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	edges, err := sm.AllEdges()
	if err != nil {
		t.Fatalf("AllEdges: %v", err)
	}
	for _, e := range edges {
		want, ok := pw.Propositions[e.PropositionID]
		if !ok {
			continue
		}
		if diff := e.PriorWeight - want.PriorStrength; diff > 1e-6 || diff < -1e-6 {
			t.Errorf("%s: PriorWeight=%.6f, want global %.6f",
				e.PropositionID, e.PriorWeight, want.PriorStrength)
		}
	}
}

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
				if diff := got - e.PriorWeight; diff > 1e-9 || diff < -1e-9 {
					t.Errorf("%s: proposition strength %.6f, edge PriorWeight %.6f",
						e.PropositionID, got, e.PriorWeight)
				}
			}
		})
	}
}

// TestBuildAppliesMachineClassPriors pins the precedence that makes per-class
// calibration meaningful: with a class named, every seeded edge must match that
// class's entry in prior_weights.json, not the class-averaged per-cluster one.
//
// The two tables agree on most edges by construction — the pipeline distinguishes
// classes only where a source constant was actually measured per class — so the
// test also asserts that at least one edge DIFFERS somewhere, otherwise it would
// pass just as happily against a file with no class information at all.
func TestBuildAppliesMachineClassPriors(t *testing.T) {
	pwPath := findPriorWeightsFile(t)
	pw, err := loadPriorWeights(pwPath)
	if err != nil {
		t.Fatalf("loadPriorWeights: %v", err)
	}
	if len(pw.MachineClasses.Classes) == 0 {
		t.Skip("prior_weights.json declares no machine classes")
	}

	differsSomewhere := false
	for _, kd := range pw.Distributions {
		for _, class := range pw.MachineClasses.Classes {
			kd, class := kd, class
			t.Run(kd+"/"+class, func(t *testing.T) {
				cfg := DefaultConfig()
				cfg.DomainSpec = mustSpec()
				cfg.PriorWeightsPath = pwPath
				cfg.KD = kd
				cfg.MachineClass = class

				sm, _, err := Build("edge-minimal", cfg)
				if err != nil {
					t.Fatalf("Build: %v", err)
				}
				edges, err := sm.AllEdges()
				if err != nil {
					t.Fatalf("AllEdges: %v", err)
				}
				want := pw.MachineClassEdgeWeights[kd][class]
				if len(want) == 0 {
					t.Fatalf("no class edge weights for %s/%s", kd, class)
				}
				perKD := pw.DistributionEdgeWeights[kd]
				for _, e := range edges {
					key := edgeKey(e.FromID, e.ToID, e.PropositionID)
					w, ok := want[key]
					if !ok {
						t.Errorf("edge %q absent from the %s table", key, class)
						continue
					}
					if diff := e.PriorWeight - w.PriorWeight; diff > 1e-6 || diff < -1e-6 {
						t.Errorf("%s: seeded %.6f, want the %s prior %.6f",
							e.PropositionID, e.PriorWeight, class, w.PriorWeight)
					}
					if o, ok := perKD[key]; ok && o.PriorWeight != w.PriorWeight {
						differsSomewhere = true
					}
				}
			})
		}
	}
	if !differsSomewhere {
		t.Error("no edge's class prior differs from its class-averaged prior anywhere; " +
			"this test would pass against a file carrying no class information")
	}
}

// TestBuildRejectsUnknownMachineClass keeps a typo loud. Silently falling back to
// the class-averaged priors would leave an operator believing their agent was
// calibrated for its hardware when it was not.
func TestBuildRejectsUnknownMachineClass(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DomainSpec = mustSpec()
	cfg.PriorWeightsPath = findPriorWeightsFile(t)
	cfg.KD = "k0s"
	cfg.MachineClass = "definitely-not-a-class"

	if _, _, err := Build("edge-minimal", cfg); err == nil {
		t.Fatal("an undeclared machine class was accepted")
	}
}
