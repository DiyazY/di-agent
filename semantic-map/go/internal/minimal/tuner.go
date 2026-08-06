package minimal

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/DiyazY/di-agent/pkg/domain"
	"github.com/DiyazY/di-agent/pkg/types"
)

// ── DisabledTuner ─────────────────────────────────────────────────────────────

// DisabledTuner is a no-op TunerContract implementation. ParseIntent always
// returns empty, Validate always returns nil. Used when the tuner is disabled
// via profiles.Config.UseRuleBasedTuner = false.
type DisabledTuner struct{}

func NewDisabledTuner() *DisabledTuner { return &DisabledTuner{} }

func (t *DisabledTuner) ParseIntent(text string) ([]*types.TuneIntent, error) {
	return nil, nil
}

func (t *DisabledTuner) Validate(adjustments []*types.TuneAdjustment) error {
	return nil
}

// ── RuleBasedTuner ────────────────────────────────────────────────────────────

// RuleBasedTuner is the edge-minimal TunerContract implementation.
//
// It maps operator natural-language text to proposition strength adjustments
// using a keyword + direction rule table. Direction words ("prioritize",
// "increase", "more" → positive; "deprioritize", "reduce", "less" → negative)
// modulate a fixed delta (defaultDelta = 0.12) applied to the matching
// proposition set.
//
// The vocabulary and the bounds both come from the domain specification, not from
// this file. An intent names propositions, so a hardcoded rule table would break
// the moment the graph's scope changed — which is exactly what happened when the
// scope narrowed to observable constructs and four of seven intents were left
// pointing at propositions that no longer existed. Bounds live there too, so a
// proposition added at runtime can be given a floor without a rebuild.
//
// This is a deterministic implementation. The same TunerContract interface admits
// a language-model back-end.
type RuleBasedTuner struct {
	spec *domain.Spec
}

// NewRuleBasedTunerFromSpec builds a tuner over the specification's vocabulary.
func NewRuleBasedTunerFromSpec(spec *domain.Spec) *RuleBasedTuner {
	return &RuleBasedTuner{spec: spec}
}

// Direction detection words. These are vocabulary about the *operator's phrasing*
// rather than about the domain — they do not name constructs or propositions — so
// they stay in code while the intent rules live in the specification.
var decreaseWords = []string{"deprioritize", "reduce", "decrease", "lower", "less", "minimize", "avoid"}
var increaseWords = []string{"prioritize", "increase", "focus", "more", "higher", "emphasize", "maximize", "prefer"}

// isNegativeDirection reports whether the operator asked to reduce rather than
// raise. Explicit increase words win a tie so "prefer less overhead" reads as an
// increase in the efficiency claim rather than a decrease.
func isNegativeDirection(lower string) bool {
	neg := matchesAnyKeyword(lower, decreaseWords)
	pos := matchesAnyKeyword(lower, increaseWords)
	return neg && !pos
}

func (t *RuleBasedTuner) ParseIntent(text string) ([]*types.TuneIntent, error) {
	if t.spec == nil {
		return nil, fmt.Errorf("tuner: no domain spec loaded")
	}
	lower := strings.ToLower(text)
	sign := 1.0
	if isNegativeDirection(lower) {
		sign = -1.0
	}

	// Accumulate across every matching intent so "faster and more efficient"
	// applies both. A proposition named by two intents takes the larger
	// magnitude rather than the sum, so overlapping vocabulary cannot compound
	// into an adjustment neither intent asked for.
	byProp := map[string]float64{}
	matched := map[string]string{}
	for _, rule := range t.spec.IntentRules {
		if !matchesAnyKeyword(lower, rule.Keywords) {
			continue
		}
		for pid, delta := range rule.Deltas {
			d := delta * sign
			if prev, ok := byProp[pid]; !ok || math.Abs(d) > math.Abs(prev) {
				byProp[pid] = d
				matched[pid] = rule.Intent
			}
		}
	}
	if len(byProp) == 0 {
		return nil, nil
	}

	pids := make([]string, 0, len(byProp))
	for pid := range byProp {
		pids = append(pids, pid)
	}
	sort.Strings(pids)

	intents := make([]*types.TuneIntent, 0, len(pids))
	for _, pid := range pids {
		intents = append(intents, &types.TuneIntent{
			PropositionID: pid,
			Delta:         byProp[pid],
			Rationale: fmt.Sprintf("intent:%s (matched: %s, direction: %+d)",
				text, matched[pid], int(sign)),
		})
	}
	return intents, nil
}

// matchesAnyKeyword reports whether any keyword appears in the lowered text.
func matchesAnyKeyword(lower string, keywords []string) bool {
	for _, k := range keywords {
		if strings.Contains(lower, strings.ToLower(k)) {
			return true
		}
	}
	return false
}

func (t *RuleBasedTuner) Validate(adjustments []*types.TuneAdjustment) error {
	if t.spec == nil {
		return fmt.Errorf("tuner: no domain spec loaded")
	}
	var errs []string
	for _, a := range adjustments {
		floor := t.spec.FloorFor(a.PropositionID)
		if a.NewStrength < floor {
			errs = append(errs, fmt.Sprintf("%s: %.3f below floor %.3f",
				a.PropositionID, a.NewStrength, floor))
		}
		if a.NewStrength > t.spec.Policy.GlobalCeiling {
			errs = append(errs, fmt.Sprintf("%s: %.3f above ceiling %.3f",
				a.PropositionID, a.NewStrength, t.spec.Policy.GlobalCeiling))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("tuner validation: %s", strings.Join(errs, "; "))
	}
	return nil
}
