package scripted

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/DiyazY/di-agent/pkg/types"
)

// Scenario is a synthetic system with known ground truth: subjects that arrive, run
// a schedule and leave, and a node model that derives unscoped properties from them
// through a small coupling catalogue. It is data, so a new test case is a new file.
type Scenario struct {
	Name            string              `json:"name"`
	Seed            int64               `json:"seed"`
	TickSeconds     int                 `json:"tick_seconds"`
	DurationSeconds int                 `json:"duration_seconds"`
	Noise           float64             `json:"noise"`
	Node            map[string]Coupling `json:"node"`
	Subjects        []SubjectSpec       `json:"subjects"`
	Expect          Expectations        `json:"expect"`
}

// Coupling says how one node property follows the subjects.
//
//	sum:      base + Σ over active subjects of their property `of`
//	logistic: σ((x − theta)/k) where x is the node property `of` (or, if `of` names
//	          a subject property, base + its sum)
//	none:     base — a node property nothing drives
type Coupling struct {
	Coupling string  `json:"coupling"`
	Base     float64 `json:"base"`
	Theta    float64 `json:"theta"`
	K        float64 `json:"k"`
	Of       string  `json:"of"`
}

// SubjectSpec is one subject's lifetime and schedule. Times are seconds from the
// scenario start; Depart nil means never; Return revives the same subject.
type SubjectSpec struct {
	ID         string                  `json:"id"`
	Arrive     int                     `json:"arrive"`
	Depart     *int                    `json:"depart,omitempty"`
	Return     *int                    `json:"return,omitempty"`
	Properties map[string]PropertySpec `json:"properties"`
}

// PropertySpec is a load schedule, relative to the subject's arrival (or return).
//
//	constant: Value
//	ramp:     Min → Max over Period seconds, then Max
//	sine:     between Min and Max with Period seconds
//	burst:    Min, except Max during [BurstStart, BurstStart+BurstDuration), repeating
//	          every Period seconds when Period > 0
type PropertySpec struct {
	Pattern       string      `json:"pattern"`
	Value         float64     `json:"value"`
	Min           float64     `json:"min"`
	Max           float64     `json:"max"`
	Period        int         `json:"period"`
	BurstStart    int         `json:"burst_start"`
	BurstDuration int         `json:"burst_duration"`
	Unit          string      `json:"unit"`
	Range         *[2]float64 `json:"range,omitempty"`
}

// Expectations are the assertions the runner checks against the scenario's truth.
type Expectations struct {
	AdmittedWithinTicks  int                      `json:"admitted_within_ticks"`
	StaleWithinSeconds   int                      `json:"stale_within_seconds"`
	RetiredWithinSeconds int                      `json:"retired_within_seconds"`
	Candidates           []ExpectedCandidate      `json:"candidates"`
	NoCandidatesFrom     []string                 `json:"no_candidates_from"`
	Counterfactuals      []ExpectedCounterfactual `json:"counterfactuals"`
}

type ExpectedCandidate struct {
	From          string `json:"from"`
	To            string `json:"to"`
	Sign          int    `json:"sign"`
	WithinSeconds int    `json:"within_seconds"`
	// ReproposedAfterReturn asserts the loop the lifecycle exists for: the subject
	// departs, the relationship retires with it, and when the subject returns the
	// pair is proposed again — the map does not remember structure a departed
	// subject has not re-earned. Requires the From subject to have a return time.
	ReproposedAfterReturn bool `json:"reproposed_after_return"`
}

// ExpectedCounterfactual compares the map's projection with the model's truth after
// applying Assume. Regime "linear" asserts |error| ≤ Tolerance; "saturated" asserts
// |error| ≥ MinError — the projection is expected to be wrong there — and that the
// standing slope caveat is present.
type ExpectedCounterfactual struct {
	Target    string             `json:"target"`
	Assume    map[string]float64 `json:"assume"`
	Regime    string             `json:"regime"`
	Tolerance float64            `json:"tolerance"`
	MinError  float64            `json:"min_error"`
}

var patterns = map[string]bool{"constant": true, "ramp": true, "sine": true, "burst": true}
var couplings = map[string]bool{"sum": true, "logistic": true, "none": true}

// LoadScenario reads and validates a scenario file.
func LoadScenario(path string) (*Scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sc Scenario
	if err := json.Unmarshal(data, &sc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := sc.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &sc, nil
}

// Validate checks the scenario is runnable.
func (s *Scenario) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("scenario needs a name")
	}
	if s.TickSeconds <= 0 || s.DurationSeconds <= 0 {
		return fmt.Errorf("tick_seconds and duration_seconds must be positive")
	}
	for name, c := range s.Node {
		if !types.ValidMetricType(name) {
			return fmt.Errorf("node property %q is not a metric type over [A-Za-z0-9._-]", name)
		}
		if !couplings[c.Coupling] {
			return fmt.Errorf("node %s: unknown coupling %q", name, c.Coupling)
		}
		if c.Coupling != "none" && c.Of == "" {
			return fmt.Errorf("node %s: coupling %s needs `of`", name, c.Coupling)
		}
		if c.Coupling == "logistic" && c.K <= 0 {
			return fmt.Errorf("node %s: logistic needs k > 0", name)
		}
	}
	for _, sub := range s.Subjects {
		if sub.ID == "" || !types.ValidSubject(sub.ID) {
			return fmt.Errorf("subject %q is not <kind>:<identity> over [A-Za-z0-9._:-]", sub.ID)
		}
		if len(sub.Properties) == 0 {
			return fmt.Errorf("subject %s has no properties", sub.ID)
		}
		for name, p := range sub.Properties {
			if !types.ValidMetricType(name) {
				return fmt.Errorf("subject %s property %q is not a metric type over [A-Za-z0-9._-]", sub.ID, name)
			}
			if !patterns[p.Pattern] {
				return fmt.Errorf("subject %s property %s: unknown pattern %q", sub.ID, name, p.Pattern)
			}
			if p.Range != nil && p.Range[1] <= p.Range[0] {
				return fmt.Errorf("subject %s property %s: empty range", sub.ID, name)
			}
		}
		if sub.Depart != nil && *sub.Depart <= sub.Arrive {
			return fmt.Errorf("subject %s departs before it arrives", sub.ID)
		}
		if sub.Return != nil && (sub.Depart == nil || *sub.Return <= *sub.Depart) {
			return fmt.Errorf("subject %s returns without departing first", sub.ID)
		}
	}
	for _, c := range s.Expect.Candidates {
		if !c.ReproposedAfterReturn {
			continue
		}
		var returns bool
		for _, sub := range s.Subjects {
			if strings.HasSuffix(c.From, "@"+sub.ID) && sub.Return != nil {
				returns = true
			}
		}
		if !returns {
			return fmt.Errorf("candidate %s->%s expects a re-proposal after return, but its subject never returns", c.From, c.To)
		}
	}
	for _, cf := range s.Expect.Counterfactuals {
		if cf.Regime != "linear" && cf.Regime != "saturated" {
			return fmt.Errorf("counterfactual on %s: regime must be linear or saturated", cf.Target)
		}
	}
	return nil
}
