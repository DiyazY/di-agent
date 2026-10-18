package statemap

import (
	"fmt"
	"sort"
	"strconv"
	"time"
)

// Fact is something about the machine that holds for as long as the agent runs: its
// operating system, kernel, CPU model and core count, memory, disk. A property is a
// signal the map follows; a fact is the frame those signals are read in — 0.3 of a
// core means something different on a 4-core VM than on a 64-core server.
//
// Facts are read, not learned: they carry no confidence and no history, only where
// they came from and when. A fact read again with a different value (memory hot-added,
// a disk grown) replaces the old one and the change is journalled.
type Fact struct {
	Key   string `json:"key"`
	Value string `json:"value"`

	// Number is the value as a number, for a fact that is one; Unit says what of.
	Number *float64 `json:"number,omitempty"`
	Unit   string   `json:"unit,omitempty"`

	// Source is what read it, such as "host:/proc/meminfo"; ReadAt when.
	Source string    `json:"source"`
	ReadAt time.Time `json:"read_at"`
}

const (
	EventFactRecorded EventKind = "fact.recorded"
	EventFactChanged  EventKind = "fact.changed"
)

// NumberFact builds a numeric fact; Value is the number as text.
func NumberFact(key string, v float64, unit string) Fact {
	return Fact{Key: key, Value: strconv.FormatFloat(v, 'f', -1, 64), Number: &v, Unit: unit}
}

// RecordFacts records facts read by source. A new key is journalled as recorded, a
// changed value as changed, and an unchanged one only refreshes ReadAt, without a
// revision: reading the same machine twice is not a change to the model.
func (m *Map) RecordFacts(source string, facts []Fact) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if m.facts == nil {
		m.facts = map[string]Fact{}
	}
	for _, f := range facts {
		if f.Key == "" {
			return fmt.Errorf("a fact from %s has no key", source)
		}
		if f.Source == "" {
			f.Source = source
		}
		if f.ReadAt.IsZero() {
			f.ReadAt = now
		}
		was, ok := m.facts[f.Key]
		m.facts[f.Key] = f.clone()
		switch {
		case !ok:
			m.bump(EventFactRecorded, f.Key, source, map[string]any{"value": f.Value, "unit": f.Unit}, now)
		case was.Value != f.Value || was.Unit != f.Unit:
			m.bump(EventFactChanged, f.Key, source, map[string]any{"from": was.Value, "to": f.Value, "unit": f.Unit}, now)
		}
	}
	return nil
}

// Facts returns the machine's facts, by key.
func (m *Map) Facts() []Fact {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.factsLocked()
}

// Fact returns one fact.
func (m *Map) Fact(key string) (Fact, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	f, ok := m.facts[key]
	return f.clone(), ok
}

func (m *Map) factsLocked() []Fact {
	out := make([]Fact, 0, len(m.facts))
	for _, f := range m.facts {
		out = append(out, f.clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func (f Fact) clone() Fact {
	if f.Number != nil {
		v := *f.Number
		f.Number = &v
	}
	return f
}

// Facts reads every fact and records them as inputs: the frame the decision read its
// properties in.
func (b *DecisionBuilder) Facts() []Fact {
	facts := b.m.Facts()
	b.facts = facts
	return facts
}
