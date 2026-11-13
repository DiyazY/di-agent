package collectors

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/DiyazY/di-agent/pkg/contracts"
	"github.com/DiyazY/di-agent/pkg/ingest/client"
	"github.com/DiyazY/di-agent/pkg/types"
	"go.yaml.in/yaml/v3"
)

// ProtocolVersion is the wire protocol both transports speak. A plugin answering
// hello with any other version is refused rather than guessed at.
const ProtocolVersion = 1

// Hello is sent once when a plugin is started or reconnected, before any collect.
// IntervalMS is sources.interval in milliseconds, so a sub-second interval is not
// truncated to zero.
type Hello struct {
	Op         string         `json:"op"`
	Protocol   int            `json:"protocol"`
	Name       string         `json:"name"`
	NodeID     string         `json:"node_id"`
	IntervalMS int64          `json:"interval_ms"`
	Config     map[string]any `json:"config"`
}

// newHello is the hello an adapter sends for entry name.
func newHello(env Env, name string, config map[string]any) Hello {
	return Hello{Op: "hello", Protocol: ProtocolVersion, Name: name, NodeID: env.NodeID,
		IntervalMS: env.Interval.Milliseconds(), Config: config}
}

// HelloReply accepts the hello with the plugin's protocol, or refuses it with Error.
// Doctor, optional, is a command the agent runs when the plugin stays unhealthy
// (see Doctor); only an exec plugin's is run, since the agent already runs its
// command.
type HelloReply struct {
	Protocol int      `json:"protocol"`
	Error    string   `json:"error,omitempty"`
	Doctor   []string `json:"doctor,omitempty"`
}

// CollectRequest asks for one batch. Seq is echoed so a late reply to an earlier
// request cannot be taken for this one.
type CollectRequest struct {
	Op      string `json:"op"`
	Seq     uint64 `json:"seq"`
	NowUnix int64  `json:"now_unix"`
}

// nowClock gives each collect of one collector its own now_unix. A sample's
// event id hashes its timestamp in whole seconds, so two collects stamped with
// the same second would make a changed reading look like a duplicate of the
// earlier one and the map would drop it. When a collect falls in the same second
// as the one before (a tick that ran late, or certification calling back to
// back), now_unix moves one second past the previous instead. With
// sources.interval at least 1s it leads the wall clock by at most a second.
type nowClock struct{ last int64 }

func (c *nowClock) next(t time.Time) int64 {
	n := t.Unix()
	if n <= c.last {
		n = c.last + 1
	}
	c.last = n
	return n
}

// CollectReply is one batch as a plugin sends it. Available is three-valued on the
// wire: absent (or null) lets the agent take the set of types in Samples; present,
// even as [], is the full set of types the plugin can produce now. A nil Available
// encodes as absent, a non-nil empty one as []. An empty Samples is "no data right
// now"; Error is a fault of the plugin; Unhealthy says the plugin's source did not
// answer, and why (the collector's Health).
type CollectReply struct {
	Seq       uint64
	Available []types.MetricType
	Samples   []*types.MetricSample
	Error     string
	Unhealthy string
}

// MarshalJSON writes available only when it is non-nil, and samples always.
func (r CollectReply) MarshalJSON() ([]byte, error) {
	type out struct {
		Seq       uint64                `json:"seq"`
		Available *[]types.MetricType   `json:"available,omitempty"`
		Samples   []*types.MetricSample `json:"samples"`
		Error     string                `json:"error,omitempty"`
		Unhealthy string                `json:"unhealthy,omitempty"`
	}
	o := out{Seq: r.Seq, Samples: r.Samples, Error: r.Error, Unhealthy: r.Unhealthy}
	if r.Available != nil {
		o.Available = &r.Available
	}
	if o.Samples == nil {
		o.Samples = []*types.MetricSample{}
	}
	return json.Marshal(o)
}

// rawReply is a collect reply as the agent decodes it: each sample is kept raw so
// one bad sample is dropped on its own rather than failing the whole reply.
type rawReply struct {
	Seq       uint64             `json:"seq"`
	Available []types.MetricType `json:"available"`
	Samples   []json.RawMessage  `json:"samples"`
	Error     string             `json:"error"`
	Unhealthy string             `json:"unhealthy"`
}

// decodeReply parses a collect reply. Only a body that is not a JSON object of
// this shape is an error; what is wrong inside a sample is stampReply's business.
func decodeReply(b []byte) (*rawReply, error) {
	var r rawReply
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("collect reply is not JSON: %w", err)
	}
	return &r, nil
}

// wireSample is a sample as a plugin sends it. Value is a pointer, and shadows the
// embedded one, so an absent or null value is told apart from a zero.
type wireSample struct {
	types.MetricSample
	Value *float64 `json:"value"`
}

// decodeSample decodes one sample strictly: a field types.MetricSample does not
// have (a misspelt "valu") and a missing or null value are errors, so a typo cannot
// turn into a reading of zero.
func decodeSample(raw json.RawMessage) (*types.MetricSample, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var w *wireSample
	if err := dec.Decode(&w); err != nil {
		return nil, err
	}
	if w == nil {
		return nil, errors.New("null sample")
	}
	if w.Value == nil {
		return nil, errors.New("no value")
	}
	s := w.MetricSample
	s.Value = *w.Value
	return &s, nil
}

// metricTypeOf reads metric_type out of a sample that did not decode, so its drop
// can still be logged against a type; "" when even that is not there.
func metricTypeOf(raw json.RawMessage) types.MetricType {
	var v struct {
		MetricType types.MetricType `json:"metric_type"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	return v.MetricType
}

// configJSON turns an entry's YAML config block into the JSON object sent in hello.
// An absent block is an empty object, so a plugin can always index config.
func configJSON(n *yaml.Node) (map[string]any, error) {
	m := map[string]any{}
	if n == nil || n.Kind == 0 {
		return m, nil
	}
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config must be a mapping")
	}
	if err := n.Decode(&m); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	// A nested mapping with non-string keys decodes to a map JSON cannot encode;
	// find that now, at startup, rather than at the first hello.
	if _, err := json.Marshal(m); err != nil {
		return nil, fmt.Errorf("config is not representable as JSON: %w", err)
	}
	return m, nil
}

// stamp fills what the agent, not the plugin, is authoritative for: the source is
// the entry name, a missing node id is the agent's, and a missing event id is the
// same deterministic hash an in-process collector or the ingest client would use.
// A foreign node id is left in place so the supervisor counts it as a violation.
func stamp(name, nodeID string, s *types.MetricSample) {
	s.Source = name
	if s.NodeID == "" {
		s.NodeID = nodeID
	}
	if s.EventID == "" {
		s.EventID = client.EventID(name, nodeID, s.Subject, string(s.MetricType), time.Unix(s.TimestampUnix, 0))
	}
}

// stampReply decodes and stamps a successful reply's samples, works out the types
// the plugin declares available, and reports what it dropped on the way:
//   - a sample that does not decode strictly (decodeSample) is DropMalformed;
//   - a sample whose event id repeats an earlier one in the reply is DropDuplicate,
//     and the first is kept. The map would drop the rest as already seen, so
//     passing them on would count samples the map never takes;
//   - an available entry that is not a valid metric type is DropInvalidAvailable,
//     and a repeated one is folded into the first.
//
// The returned samples and available list are never nil, so "no data" stays
// distinguishable from a fault.
//
// A sample with no timestamp takes nowUnix, the now_unix of the request it
// answers, before its event id is computed: hashed at time zero it would carry
// the same event id every tick, and the map would drop every sample after the
// first as a duplicate.
func stampReply(name, nodeID string, nowUnix int64, r *rawReply) ([]*types.MetricSample, []types.MetricType, []contracts.SampleDrop) {
	out := make([]*types.MetricSample, 0, len(r.Samples))
	var drops []contracts.SampleDrop
	seenEvent := make(map[string]bool, len(r.Samples))
	for _, raw := range r.Samples {
		s, err := decodeSample(raw)
		if err != nil {
			drops = append(drops, contracts.SampleDrop{Kind: contracts.DropMalformed, MetricType: metricTypeOf(raw)})
			continue
		}
		if s.TimestampUnix == 0 {
			s.TimestampUnix = nowUnix
		}
		stamp(name, nodeID, s)
		if seenEvent[s.EventID] {
			drops = append(drops, contracts.SampleDrop{Kind: contracts.DropDuplicate, MetricType: s.MetricType})
			continue
		}
		seenEvent[s.EventID] = true
		out = append(out, s)
	}
	avail := []types.MetricType{}
	seen := map[types.MetricType]bool{}
	if r.Available != nil {
		for _, mt := range r.Available {
			if !types.ValidMetricType(string(mt)) {
				drops = append(drops, contracts.SampleDrop{Kind: contracts.DropInvalidAvailable, MetricType: mt})
				continue
			}
			if !seen[mt] {
				seen[mt] = true
				avail = append(avail, mt)
			}
		}
		return out, avail, drops
	}
	for _, s := range out {
		if !seen[s.MetricType] {
			seen[s.MetricType] = true
			avail = append(avail, s.MetricType)
		}
	}
	return out, avail, drops
}

// resolveTimeout is the per-request timeout of an entry: the configured one, or
// min(interval/2, 5s) when it is unset, so both transports default alike.
func resolveTimeout(env Env, timeout time.Duration) time.Duration {
	if timeout > 0 {
		return timeout
	}
	timeout = defaultTimeout
	if half := env.Interval / 2; half > 0 && half < timeout {
		timeout = half
	}
	return timeout
}

// checkHello decides whether a hello reply accepts the session: a refusal or a
// protocol other than ProtocolVersion is a hello failure.
func checkHello(r HelloReply) error {
	if r.Error != "" {
		return fmt.Errorf("hello refused: %s", r.Error)
	}
	if r.Protocol != ProtocolVersion {
		return fmt.Errorf("plugin speaks protocol %d, agent speaks %d", r.Protocol, ProtocolVersion)
	}
	return nil
}

// replyHealth is a successful reply's health: nil, or its unhealthy reason.
func replyHealth(r *rawReply) error {
	if r.Unhealthy == "" {
		return nil
	}
	return errors.New(r.Unhealthy)
}

// checkCollect decides whether a collect reply is a usable answer to request seq:
// an error reply is a fault, and so is a reply to another request.
func checkCollect(r *rawReply, seq uint64) error {
	if r.Error != "" {
		return fmt.Errorf("plugin error: %s", r.Error)
	}
	if r.Seq != seq {
		return fmt.Errorf("reply seq %d, want %d", r.Seq, seq)
	}
	return nil
}
