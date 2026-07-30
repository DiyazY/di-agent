package minimal

import (
	"fmt"
	"math"
	"sort"
	"strings"

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
// Hard bounds are enforced by Validate:
//   - Global ceiling: 0.95 (no proposition can be maximized)
//   - Global floor: 0.10 (no proposition can be zeroed)
//   - SC-related propositions (P1, P4, P11, P14): floor = 0.30
//
// This is a deterministic v1 implementation. The same TunerContract interface
// is used by the cloud-full profile with an SLM back-end.
type RuleBasedTuner struct{}

func NewRuleBasedTuner() *RuleBasedTuner { return &RuleBasedTuner{} }

const defaultDelta = 0.12

type intentRule struct {
	keywords   []string           // any of these triggers the rule (case-insensitive)
	propDeltas map[string]float64 // propID → delta when direction=positive
	// When direction is negative, all deltas are negated before applying.
}

var intentRules = []intentRule{
	{
		keywords: []string{"security", "secure", "compliance", "hardening", "cis"},
		propDeltas: map[string]float64{
			"P1":  +defaultDelta, // SC→RC+: security increases resource use
			"P11": +defaultDelta, // CE→SC+: community helps security patching
		},
	},
	{
		keywords: []string{"performance", "throughput", "latency", "fast", "speed"},
		propDeltas: map[string]float64{
			"P3": +defaultDelta, // RC→PS+: lightweight → better performance
			"P2": -0.10,         // RC→PS-: suppress overhead (conflict pair)
		},
	},
	{
		keywords: []string{"energy", "power", "efficient", "battery", "watt"},
		propDeltas: map[string]float64{
			"P10": +defaultDelta, // PS→RC-: better efficiency → lower cost
			"P8":  +0.08,         // MU→RC-: simpler ops → lower cost
		},
	},
	{
		keywords: []string{"reliability", "resilience", "ha", "availability", "recovery", "fault"},
		propDeltas: map[string]float64{
			"P5":  +defaultDelta, // CO→RR+: offline autonomy helps reliability
			"P15": +defaultDelta, // MU→RR+: automation shortens recovery
		},
	},
	{
		keywords: []string{"maintainability", "maintenance", "simple", "admin", "operations", "setup"},
		propDeltas: map[string]float64{
			"P7": +defaultDelta, // CE→MU+: rich ecosystem lowers effort
			"P8": +0.10,         // MU→RC-: simplicity reduces cost
		},
	},
	{
		keywords: []string{"connectivity", "offline", "disconnected"},
		propDeltas: map[string]float64{
			"P5":  +defaultDelta, // CO→RR+: offline autonomy
			"P13": +0.08,         // CO→PS-: acknowledge the sync overhead
		},
	},
	{
		keywords: []string{"community", "ecosystem", "vendor"},
		propDeltas: map[string]float64{
			"P7":  +defaultDelta, // CE→MU+: ecosystem helps maintainability
			"P11": +0.08,         // CE→SC+: community helps security
		},
	},
}

// Direction detection words.
var decreaseWords = []string{"deprioritize", "reduce", "decrease", "lower", "less", "minimize", "avoid"}
var increaseWords = []string{"prioritize", "increase", "focus", "more", "higher", "emphasize", "maximize", "prefer"}

func (t *RuleBasedTuner) ParseIntent(text string) ([]*types.TuneIntent, error) {
	lower := strings.ToLower(text)

	// Detect direction: negative if any decrease word found; positive if increase
	// word found or no direction word at all (default to positive).
	direction := 1.0
	for _, w := range decreaseWords {
		if strings.Contains(lower, w) {
			direction = -1.0
			break
		}
	}

	// Accumulate deltas per propID — if two rules target the same propID,
	// take the larger absolute value (last-writer-wins if equal).
	deltas := make(map[string]float64)
	rationales := make(map[string]string)

	for _, rule := range intentRules {
		matched := ""
		for _, kw := range rule.keywords {
			if strings.Contains(lower, kw) {
				matched = kw
				break
			}
		}
		if matched == "" {
			continue
		}
		for propID, baseDelta := range rule.propDeltas {
			d := baseDelta * direction
			if existing, ok := deltas[propID]; !ok || math.Abs(d) > math.Abs(existing) {
				deltas[propID] = d
				rationales[propID] = fmt.Sprintf("intent:%s (keyword: %s, direction: %+.0f)", text, matched, direction)
			}
		}
	}

	if len(deltas) == 0 {
		return nil, nil
	}

	out := make([]*types.TuneIntent, 0, len(deltas))
	for propID, delta := range deltas {
		out = append(out, &types.TuneIntent{
			PropositionID: propID,
			Delta:         delta,
			Rationale:     rationales[propID],
		})
	}
	// Stable order: sort by PropositionID so output is deterministic.
	sort.Slice(out, func(i, j int) bool { return out[i].PropositionID < out[j].PropositionID })
	return out, nil
}

// propositionFloor returns the minimum allowed strength for a proposition.
// SC-related propositions have a higher floor (security must stay meaningful).
func propositionFloor(propID string) float64 {
	switch propID {
	case "P1", "P4", "P11", "P14":
		return 0.30
	default:
		return 0.10
	}
}

const strengthCeil = 0.95

func (t *RuleBasedTuner) Validate(adjustments []*types.TuneAdjustment) error {
	var errs []string
	for _, a := range adjustments {
		floor := propositionFloor(a.PropositionID)
		if a.NewStrength < floor {
			errs = append(errs, fmt.Sprintf("%s: %.3f below floor %.3f", a.PropositionID, a.NewStrength, floor))
		}
		if a.NewStrength > strengthCeil {
			errs = append(errs, fmt.Sprintf("%s: %.3f above ceiling %.3f", a.PropositionID, a.NewStrength, strengthCeil))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("tune validation failed: %s", strings.Join(errs, "; "))
	}
	return nil
}
