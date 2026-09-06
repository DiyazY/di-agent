package scripted

import (
	"crypto/sha256"
	"encoding/hex"
	"log"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/DiyazY/di-agent/pkg/types"
)

// SystemScript is a CollectorContract implementation driven by a Scenario: a
// synthetic system whose subjects and node model have known ground truth. It
// exercises the whole daemon path as a third, simulated instrument, and the runner
// scores the map's answers against NodeValues.
type SystemScript struct {
	// Logf receives the one line the script says for itself: that the scenario is
	// over and it will emit nothing further. nil means the standard logger.
	Logf     func(format string, args ...any)
	finished bool
	sc       *Scenario
	nodeID   string
	sid      string
	start    time.Time

	mu   sync.Mutex
	tick int64
	rng  *rand.Rand

	nodeNames []string
	available []types.MetricType
}

// NewSystemScript builds the collector. Samples at tick n carry the timestamp
// start + n·TickSeconds, so a runner with an injected clock can follow them.
func NewSystemScript(nodeID string, sc *Scenario, start time.Time) (*SystemScript, error) {
	// A literal scenario bypasses LoadScenario; NodeValues cannot compute a truth
	// for a coupling it does not know, so refuse here rather than hand back a
	// script whose truth table is silently incomplete.
	if err := sc.Validate(); err != nil {
		return nil, err
	}
	s := &SystemScript{sc: sc, nodeID: nodeID, sid: "system-script:" + sc.Name, start: start,
		rng: rand.New(rand.NewSource(sc.Seed))}
	seen := map[string]bool{}
	for name := range sc.Node {
		s.nodeNames = append(s.nodeNames, name)
		seen[name] = true
	}
	sort.Strings(s.nodeNames)
	for _, sub := range sc.Subjects {
		for name := range sub.Properties {
			seen[name] = true
		}
	}
	for name := range seen {
		s.available = append(s.available, types.MetricType(name))
	}
	sort.Slice(s.available, func(i, j int) bool { return s.available[i] < s.available[j] })
	return s, nil
}

func (s *SystemScript) SourceID() string { return s.sid }

func (s *SystemScript) log(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

func (s *SystemScript) AvailableMetrics() []types.MetricType {
	out := make([]types.MetricType, len(s.available))
	copy(out, s.available)
	return out
}

// Tick is the last emitted tick. At is the timestamp of a tick.
func (s *SystemScript) Tick() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tick
}

func (s *SystemScript) At(tick int64) time.Time {
	return s.start.Add(time.Duration(tick) * time.Duration(s.sc.TickSeconds) * time.Second)
}

// Active reports whether a subject exists at second sec of the scenario.
func (s *SystemScript) Active(sub SubjectSpec, sec int) bool {
	if sec < sub.Arrive {
		return false
	}
	if sub.Depart == nil || sec < *sub.Depart {
		return true
	}
	return sub.Return != nil && sec >= *sub.Return
}

// subjectValue is the noiseless value of one subject property at sec, honouring an
// override keyed <property>@<subject>. ok is false when the subject is not active.
func (s *SystemScript) subjectValue(sub SubjectSpec, prop string, sec int, override map[string]float64) (float64, bool) {
	if !s.Active(sub, sec) {
		return 0, false
	}
	if v, ok := override[prop+"@"+sub.ID]; ok {
		return v, true
	}
	ps, ok := sub.Properties[prop]
	if !ok {
		return 0, false
	}
	since := sec - sub.Arrive
	if sub.Return != nil && sec >= *sub.Return {
		since = sec - *sub.Return
	}
	return clipRange(evalPattern(ps, since), ps.Range), true
}

// NodeValues is the noiseless truth of every node property at sec, optionally under
// overridden subject values — the ground truth a counterfactual is scored against.
func (s *SystemScript) NodeValues(sec int, override map[string]float64) map[string]float64 {
	sum := func(prop string) float64 {
		var total float64
		for _, sub := range s.sc.Subjects {
			if v, ok := s.subjectValue(sub, prop, sec, override); ok {
				total += v
			}
		}
		return total
	}
	out := map[string]float64{}
	for _, name := range s.nodeNames {
		c := s.sc.Node[name]
		switch c.Coupling {
		case "sum":
			out[name] = clip01(c.Base + sum(c.Of))
		case "none":
			out[name] = clip01(c.Base)
		case "logistic":
			// second pass below, once the sums it may read are in place
		default:
			panic("scripted: coupling " + c.Coupling + " reached NodeValues; Validate refuses it, and NewSystemScript validates")
		}
	}
	for _, name := range s.nodeNames {
		c := s.sc.Node[name]
		if c.Coupling != "logistic" {
			continue
		}
		x, ok := out[c.Of]
		if !ok {
			x = c.Base + sum(c.Of)
		}
		out[name] = clip01(1 / (1 + math.Exp(-(x-c.Theta)/c.K)))
	}
	return out
}

// Collect advances one tick and emits every active subject's properties and every
// node property, each with noise, in a deterministic order.
func (s *SystemScript) Collect() ([]*types.MetricSample, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tick++
	sec := int(s.tick) * s.sc.TickSeconds
	if sec > s.sc.DurationSeconds {
		if !s.finished {
			// On a live daemon this looks exactly like telemetry that stopped.
			s.finished = true
			s.log("script: scenario %s finished after %d ticks (%ds); emitting nothing further",
				s.sc.Name, s.tick-1, s.sc.DurationSeconds)
		}
		return nil, nil
	}
	at := s.At(s.tick).Unix()
	var out []*types.MetricSample
	for _, sub := range s.sc.Subjects {
		if !s.Active(sub, sec) {
			continue
		}
		names := make([]string, 0, len(sub.Properties))
		for name := range sub.Properties {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			ps := sub.Properties[name]
			v, _ := s.subjectValue(sub, name, sec, nil)
			v = clipRange(v+s.rng.NormFloat64()*s.sc.Noise, ps.Range)
			rng := [2]float64{0, 1}
			if ps.Range != nil {
				rng = *ps.Range
			}
			unit := ps.Unit
			if unit == "" {
				unit = "fraction"
			}
			out = append(out, &types.MetricSample{
				NodeID: s.nodeID, MetricType: types.MetricType(name), Value: v, TimestampUnix: at,
				EventID: s.eventID(sub.ID, name), Subject: sub.ID, Unit: unit, Range: &rng, Source: s.sid,
				Labels: map[string]string{"kind": kindOf(sub.ID)},
			})
		}
	}
	truth := s.NodeValues(sec, nil)
	for _, name := range s.nodeNames {
		v := clip01(truth[name] + s.rng.NormFloat64()*s.sc.Noise)
		rng := [2]float64{0, 1}
		out = append(out, &types.MetricSample{
			NodeID: s.nodeID, MetricType: types.MetricType(name), Value: v, TimestampUnix: at,
			EventID: s.eventID("", name), Unit: "fraction", Range: &rng, Source: s.sid,
		})
	}
	return out, nil
}

func (s *SystemScript) eventID(subject, metric string) string {
	h := sha256.Sum256([]byte(s.sid + "|" + subject + "|" + metric + "|" + strconv.FormatInt(s.tick, 10)))
	return hex.EncodeToString(h[:8])
}

func evalPattern(ps PropertySpec, t int) float64 {
	switch ps.Pattern {
	case "constant":
		return ps.Value
	case "ramp":
		if ps.Period <= 0 {
			return ps.Max
		}
		f := float64(t) / float64(ps.Period)
		if f > 1 {
			f = 1
		}
		return ps.Min + (ps.Max-ps.Min)*f
	case "sine":
		mid, amp := (ps.Min+ps.Max)/2, (ps.Max-ps.Min)/2
		if ps.Period <= 0 {
			return mid
		}
		return mid + amp*math.Sin(2*math.Pi*float64(t)/float64(ps.Period))
	case "burst":
		tt := t
		if ps.Period > 0 {
			tt = t % ps.Period
		}
		if tt >= ps.BurstStart && tt < ps.BurstStart+ps.BurstDuration {
			return ps.Max
		}
		return ps.Min
	}
	return 0
}

func kindOf(subject string) string {
	for i := 0; i < len(subject); i++ {
		if subject[i] == ':' {
			return subject[:i]
		}
	}
	return subject
}

func clip01(v float64) float64 { return clipRange(v, nil) }

func clipRange(v float64, r *[2]float64) float64 {
	lo, hi := 0.0, 1.0
	if r != nil {
		lo, hi = r[0], r[1]
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
