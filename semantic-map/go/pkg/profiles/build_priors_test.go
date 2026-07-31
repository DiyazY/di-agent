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
