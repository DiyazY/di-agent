package minimal_test

import (
	"testing"

	"github.com/DiyazY/di-agent/pkg/domain"
	"github.com/DiyazY/di-agent/pkg/types"
)

// mustSpec loads the committed domain specification. Tests build their ontology
// from the same data artifact the daemon uses, so a change to the model's scope
// surfaces here rather than in a divergent test fixture.
func mustSpec() *domain.Spec {
	s, err := domain.LoadFound()
	if err != nil {
		panic("test setup: " + err.Error())
	}
	return s
}

var _ = testing.Short

// specPair names a construct pair the spec covers in exactly ONE direction,
// together with that direction. Conflict pairs (the same endpoints carried by
// two propositions of opposite sign) leave no free direction, so a proposer test
// that needs to emit a candidate on the free direction must avoid them.
func specPair() (from, to, dir string) {
	s := mustSpec()
	count := map[string]int{}
	for _, p := range s.Propositions {
		count[p.FromConstruct+"->"+p.ToConstruct]++
	}
	for _, p := range s.Propositions {
		if count[p.FromConstruct+"->"+p.ToConstruct] == 1 {
			return p.FromConstruct, p.ToConstruct, p.Direction
		}
	}
	return "", "", ""
}

// sameDirectionSeries builds a y series whose correlation with x carries the
// sign named by dir, so correlation tests can drive either direction of a pair
// without naming a specific construct or proposition.
func sameDirectionSeries(dir string, x float64) float64 {
	if dir == "negative" {
		return 1.0 - 0.95*x
	}
	return 0.95 * x
}

func flipDir(dir string) string {
	if dir == "negative" {
		return "positive"
	}
	return "negative"
}

// oppositeOf maps a spec direction string to the types.Direction a proposer
// should emit for the free (opposite) direction of the same pair.
func oppositeOf(dir string) types.Direction {
	if dir == "negative" {
		return types.Positive
	}
	return types.Negative
}
