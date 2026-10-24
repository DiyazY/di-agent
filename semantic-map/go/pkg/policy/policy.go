// Package policy says what the agent may do to a workload. Rules are read in
// order: the first one that matches supplies the settings, any rule that
// matches and freezes makes the workload untouchable, and the agent itself is
// always frozen.
package policy

import (
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/DiyazY/di-agent/pkg/spec"
)

// selfFrozen is why the agent never acts on itself.
const selfFrozen = "built-in: the agent itself"

// Target is a workload as the policy sees it. Kind is the part of the subject
// before the colon (pod, unit).
type Target struct {
	Kind   string
	Name   string
	Labels map[string]string
	Self   bool
}

// Effective is what the policy permits for one target.
type Effective struct {
	Frozen   bool          `json:"frozen"`
	FrozenBy string        `json:"frozen_by,omitempty"`
	Rule     int           `json:"rule"` // first matching rule; -1 = defaults
	Settings spec.Settings `json:"settings"`
}

// Policy is a validated PolicySpec.
type Policy struct {
	spec spec.PolicySpec
}

// New refuses a rule whose name glob does not compile.
func New(p spec.PolicySpec) (*Policy, error) {
	for i, r := range p.Rules {
		if _, err := path.Match(r.Match.Name, ""); err != nil {
			return nil, fmt.Errorf("policy rule %d: name %q: %w", i, r.Match.Name, err)
		}
	}
	return &Policy{spec: p}, nil
}

// For resolves the policy for one target. The first matching rule supplies the
// settings, field by field, with defaults for what it leaves unset.
func (p *Policy) For(t Target) Effective {
	e := Effective{Rule: -1, Settings: copySettings(p.spec.Defaults)}
	if t.Self {
		e.Frozen, e.FrozenBy = true, selfFrozen
	}
	for i, r := range p.spec.Rules {
		if !matches(r.Match, t) {
			continue
		}
		if e.Rule < 0 {
			e.Rule = i
			overlay(&e.Settings, r.Settings)
		}
		if r.Freeze && !e.Frozen {
			e.Frozen, e.FrozenBy = true, fmt.Sprintf("rule %d", i)
		}
	}
	// Every bound of every action named is set, so a reader never meets a nil one.
	for a := range e.Settings.Floor {
		e.Settings.Floor[a] = spec.NewBound(e.Settings.Floor.Values(a))
	}
	return e
}

// Defaults is the settings of a workload no rule matches, as a copy.
func (p *Policy) Defaults() spec.Settings { return copySettings(p.spec.Defaults) }

// Allows reports whether the action may be taken: not frozen, allowed, and not
// denied. Deny beats allow.
func (e Effective) Allows(action string) bool {
	return !e.Frozen && contains(e.Settings.Allow, action) && !contains(e.Settings.Deny, action)
}

func matches(m spec.Match, t Target) bool {
	if m.Kind != "" && m.Kind != t.Kind {
		return false
	}
	if m.Name != "" {
		if ok, err := path.Match(m.Name, t.Name); err != nil || !ok {
			return false
		}
	}
	for k, want := range m.Labels {
		got, ok := t.Labels[k]
		if !ok || !contains(want, got) {
			return false
		}
	}
	return true
}

// overlay sets onto s every field r leaves set; a floor action by action and
// bound by bound.
func overlay(s *spec.Settings, r spec.Settings) {
	if r.Allow != nil {
		s.Allow = append([]string(nil), r.Allow...)
	}
	if r.Deny != nil {
		s.Deny = append([]string(nil), r.Deny...)
	}
	if r.Floor != nil {
		s.Floor = r.Floor.Over(s.Floor)
	}
	if r.MaxActionsPerHour != 0 {
		s.MaxActionsPerHour = r.MaxActionsPerHour
	}
	if r.Cooldown != 0 {
		s.Cooldown = r.Cooldown
	}
}

// copySettings keeps a caller from altering the policy through an Effective.
func copySettings(s spec.Settings) spec.Settings {
	var out spec.Settings
	out.MaxActionsPerHour, out.Cooldown = s.MaxActionsPerHour, s.Cooldown
	overlay(&out, spec.Settings{Allow: s.Allow, Deny: s.Deny, Floor: s.Floor})
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// MissingDefaultFreezes names each freeze rule of the default policy that p has
// no rule for. policy.rules in a file replaces the default rules, so a file that
// adds a rule without copying them unfreezes what they protected.
func MissingDefaultFreezes(p spec.PolicySpec) []string {
	var out []string
	for _, d := range spec.Default().Policy.Rules {
		if !d.Freeze {
			continue
		}
		if !slices.ContainsFunc(p.Rules, func(r spec.Rule) bool { return r.Freeze && covers(r.Match, d.Match) }) {
			out = append(out, describe(d.Match))
		}
	}
	return out
}

// covers reports whether a rule matching m freezes at least what one matching d
// does: no narrower kind or name, and for every label of d, the same label with
// at least d's values and nothing more to hold.
func covers(m, d spec.Match) bool {
	if (m.Kind != "" && m.Kind != d.Kind) || (m.Name != "" && m.Name != d.Name) || len(m.Labels) > len(d.Labels) {
		return false
	}
	for k, want := range d.Labels {
		got, ok := m.Labels[k]
		if !ok {
			return false
		}
		for _, v := range want {
			if !slices.Contains(got, v) {
				return false
			}
		}
	}
	return true
}

// describe is a match as one line, for a warning.
func describe(m spec.Match) string {
	keys := make([]string, 0, len(m.Labels))
	for k := range m.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s in [%s]", k, strings.Join(m.Labels[k], ", ")))
	}
	return "freeze where " + strings.Join(parts, " and ")
}
