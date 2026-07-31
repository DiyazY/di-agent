package profiles

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/DiyazY/di-agent/internal/minimal"
)

// TestPerKDSeedingMatchesPriorWeights is the numerical verification that
// `-kd <name>` applies the per-distribution edge weights from
// prior_weights.json. For each (proposition, KD) pair in the file, the seeded
// EdgeDescriptor.PriorWeight must equal
// distribution_edge_weights[kd][edge_key].prior_weight.
//
// The storage backbone is a multigraph: Di-Select has three "conflict-pair"
// propositions (P2/P3 on RC→PS, P5/P6 on CO→RR, P7/P9 on CE→MU) sharing
// endpoints but with distinct PropositionIDs and opposite directions. Storage
// must hold all 15 propositions as independent EdgeDescriptors.
func TestPerKDSeedingMatchesPriorWeights(t *testing.T) {
	pwPath := findPriorWeightsFile(t)

	// Parse the file directly to get expected values.
	raw, err := os.ReadFile(pwPath)
	if err != nil {
		t.Fatalf("read prior_weights.json: %v", err)
	}
	var expected priorWeightsFile
	if err := json.Unmarshal(raw, &expected); err != nil {
		t.Fatalf("parse prior_weights.json: %v", err)
	}
	if len(expected.Distributions) == 0 {
		t.Fatal("prior_weights.json has no distributions")
	}

	for _, kd := range expected.Distributions {
		kd := kd
		t.Run(kd, func(t *testing.T) {
			storage := minimal.NewInMemoryStorage()
			ontology := minimal.NewOntologyFromSpec(mustSpec())
			applyPriorWeights(ontology, &expected)
			seedFromOntology(storage, ontology, &expected, kd)

			perKD := expected.DistributionEdgeWeights[kd]
			if len(perKD) == 0 {
				t.Fatalf("no edge weights for KD=%q in file", kd)
			}

			edges, err := storage.AllEdges()
			if err != nil {
				t.Fatal(err)
			}
			if len(edges) == 0 {
				t.Fatal("storage has no edges after seeding")
			}

			matched := 0
			for _, e := range edges {
				key := edgeKey(e.FromID, e.ToID, e.PropositionID)
				want, ok := perKD[key]
				if !ok {
					t.Errorf("seeded edge %q has no entry in prior_weights for KD=%q", key, kd)
					continue
				}
				if !almostEqual(e.PriorWeight, want.PriorWeight, 1e-6) {
					t.Errorf("edge %q PriorWeight: got %.6f, want %.6f", key, e.PriorWeight, want.PriorWeight)
				}
				if !almostEqual(e.EMAWeight, want.PriorWeight, 1e-6) {
					t.Errorf("edge %q EMAWeight: got %.6f, want %.6f (should start equal to prior)",
						key, e.EMAWeight, want.PriorWeight)
				}
				matched++
			}
			// All 15 Di-Select propositions (P1–P15) must be present after
			// seeding — the multigraph storage holds one descriptor per
			// proposition, including the three conflict pairs.
			if want := len(mustSpec().Propositions); matched < want {
				t.Errorf("expected %d matched edges (one per proposition in the domain spec) after multigraph seeding; got %d", want, matched)
			}
		})
	}
}

// TestGlobalSeedingWhenKDIsEmpty verifies that when KD is unset, edges are
// seeded from the global proposition strengths in prior_weights.json (NOT
// the per-KD weights).
func TestGlobalSeedingWhenKDIsEmpty(t *testing.T) {
	pwPath := findPriorWeightsFile(t)
	raw, _ := os.ReadFile(pwPath)
	var pw priorWeightsFile
	_ = json.Unmarshal(raw, &pw)

	storage := minimal.NewInMemoryStorage()
	ontology := minimal.NewOntologyFromSpec(mustSpec())
	applyPriorWeights(ontology, &pw)
	seedFromOntology(storage, ontology, &pw, "") // no KD

	edges, _ := storage.AllEdges()
	for _, e := range edges {
		want := pw.Propositions[e.PropositionID].PriorStrength
		if !almostEqual(e.PriorWeight, want, 1e-6) {
			t.Errorf("edge %s→%s (prop %s): PriorWeight=%.6f; expected global %.6f",
				e.FromID, e.ToID, e.PropositionID, e.PriorWeight, want)
		}
	}
}

// TestValidateKD checks that unknown distribution names are rejected.
func TestValidateKD(t *testing.T) {
	pwPath := findPriorWeightsFile(t)
	raw, _ := os.ReadFile(pwPath)
	var pw priorWeightsFile
	_ = json.Unmarshal(raw, &pw)

	if err := validateKD(&pw, "k0s"); err != nil {
		t.Errorf("k0s should be valid; got %v", err)
	}
	if err := validateKD(&pw, ""); err != nil {
		t.Errorf("empty KD should be valid (skip per-KD seeding); got %v", err)
	}
	if err := validateKD(&pw, "nonexistent-distro"); err == nil {
		t.Error("expected error for unknown KD")
	}
	if err := validateKD(nil, "k0s"); err != nil {
		t.Errorf("nil priorWeights should make KD a no-op; got %v", err)
	}
}

// findPriorWeightsFile walks up from this package to locate prior_weights.json
// in the semantic-map directory. Keeps the test independent of the test
// runner's working directory.
func findPriorWeightsFile(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// Walk up at most 6 levels looking for semantic-map/prior_weights.json.
	dir := wd
	for i := 0; i < 6; i++ {
		candidate := filepath.Join(dir, "prior_weights.json")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		candidate = filepath.Join(dir, "semantic-map", "prior_weights.json")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Skipf("prior_weights.json not found from %q — skipping numerical verification", wd)
	return ""
}

func almostEqual(a, b, eps float64) bool {
	return math.Abs(a-b) <= eps
}
