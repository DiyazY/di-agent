package semmap

import (
	"sort"
	"sync"

	"github.com/DiyazY/di-agent/pkg/contracts"
	"github.com/DiyazY/di-agent/pkg/types"
)

// DefaultPairWindowSeconds is how far apart two construct observations may be
// and still count as simultaneous. It is not a smoothing parameter: collectors
// sample on independent grids — in the dissertation testbed the system.* series
// land on 0, 5, 10 … while the PSI series land on 2, 6, 12 …, so the two never
// share a timestamp — and without a tolerance no pair would ever form.
//
// The window should be a small multiple of the slowest collector's interval.
// Too tight and pairs never form; too loose and a pair relates a resource
// reading to a pressure reading from a different operating regime.
const DefaultPairWindowSeconds = 15

// constructObservation is the most recent value seen for one construct.
type constructObservation struct {
	value   float64
	ts      int64
	eventID string
}

// pairTracker remembers the latest observation per (node, construct) so the
// ingest path can form paired observations for edges whose two endpoints were
// sampled by different collectors at different times.
//
// Keying by node as well as construct is required, not defensive. A cluster
// streams every node's telemetry into one graph, so a tracker keyed on construct
// alone will pair the master's CPU reading with a worker's pressure reading and
// learn an association between series that have no mechanical relationship. The
// edge itself stays cluster-wide — it aggregates the association observed across
// nodes — but each pair that feeds it has to come from one node.
//
// It holds one entry per (node, construct), not a history: a pair is formed from
// the arriving sample and the freshest counterpart, which is what an agent
// estimating "the relation right now" wants. A longer history would let the same
// counterpart pair with many arrivals and inflate support without new evidence.
type pairTracker struct {
	mu      sync.Mutex
	latest  map[nodeConstruct]constructObservation
	windowS int64
}

// nodeConstruct identifies one series: one construct as observed on one node.
type nodeConstruct struct {
	node      string
	construct string
}

func newPairTracker(windowSeconds int) *pairTracker {
	if windowSeconds <= 0 {
		windowSeconds = DefaultPairWindowSeconds
	}
	return &pairTracker{
		latest:  make(map[nodeConstruct]constructObservation),
		windowS: int64(windowSeconds),
	}
}

// record stores the arriving observation and returns the other constructs
// observed on the SAME node whose latest reading is within the window, so the
// caller can pair against them.
func (p *pairTracker) record(node, construct string, obs constructObservation) map[string]constructObservation {
	p.mu.Lock()
	defer p.mu.Unlock()

	fresh := make(map[string]constructObservation, len(p.latest))
	for k, o := range p.latest {
		if k.node != node || k.construct == construct {
			continue
		}
		age := obs.ts - o.ts
		if age < 0 {
			age = -age
		}
		if age <= p.windowS {
			fresh[k.construct] = o
		}
	}
	p.latest[nodeConstruct{node: node, construct: construct}] = obs
	return fresh
}

// ingestPaired routes one sample through the relational path: it records the
// observation, then for every edge incident to the sample's construct whose
// other endpoint has a fresh value, calls UpdateEdgeRelation with the endpoint
// values in the edge's own (from, to) order.
//
// The paired event ID combines both contributing event IDs in a fixed order, so
// it is deterministic: replaying a batch produces the same pair identities and
// the updater's idempotency rule drops them.
//
// Returns the construct pairs it updated, which is empty — legitimately — whenever
// the counterpart construct has not been observed recently. The caller must not treat
// that as an error: it is the normal state early in a run and whenever one collector
// is slower than the pairing window.
//
// The pairs are returned rather than counted so the caller can propagate the new
// estimates into the state model. Without that the state model's relationships would
// sit at their seeded priors forever, and every sensitivity the reasoner reports
// would be prior-derived no matter how long the agent ran.
func ingestPaired(
	sample *types.MetricSample,
	construct string,
	ontology contracts.OntologyContract,
	updater contracts.RelationalUpdaterContract,
	tracker *pairTracker,
) ([]ConstructPair, error) {
	fresh := tracker.record(sample.NodeID, construct, constructObservation{
		value:   sample.Value,
		ts:      sample.TimestampUnix,
		eventID: sample.EventID,
	})
	if len(fresh) == 0 {
		return nil, nil
	}

	props, err := ontology.Relationships(construct)
	if err != nil {
		return nil, err
	}

	type pair struct{ from, to string }
	seen := make(map[pair]struct{}, len(props))
	var applied []ConstructPair
	var firstErr error

	// Deterministic order so a paired event ID does not depend on map iteration.
	sort.Slice(props, func(i, j int) bool {
		if props[i] == nil || props[j] == nil {
			return props[j] == nil
		}
		return props[i].PropositionID < props[j].PropositionID
	})

	for _, prop := range props {
		if prop == nil || prop.Deprecated {
			continue
		}
		pr := pair{prop.FromConstruct, prop.ToConstruct}
		if _, dup := seen[pr]; dup {
			continue
		}

		// Identify the endpoint that is not the arriving construct. A self-edge
		// has no counterpart and is skipped: correlating a construct with itself
		// would report 1.0 and mean nothing.
		var other string
		switch {
		case prop.FromConstruct == construct && prop.ToConstruct != construct:
			other = prop.ToConstruct
		case prop.ToConstruct == construct && prop.FromConstruct != construct:
			other = prop.FromConstruct
		default:
			continue
		}
		counterpart, ok := fresh[other]
		if !ok {
			continue
		}
		seen[pr] = struct{}{}

		fromValue, toValue := sample.Value, counterpart.value
		if prop.ToConstruct == construct {
			fromValue, toValue = counterpart.value, sample.Value
		}
		pairedID := pairEventID(prop.FromConstruct, prop.ToConstruct, sample.EventID, counterpart.eventID)
		if _, err := updater.UpdateEdgeRelation(prop.FromConstruct, prop.ToConstruct,
			fromValue, toValue, pairedID); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		applied = append(applied, ConstructPair{From: prop.FromConstruct, To: prop.ToConstruct})
	}
	return applied, firstErr
}

// ConstructPair names the endpoints of a relationship that received a paired
// observation.
type ConstructPair struct{ From, To string }

// pairEventID builds a deterministic identity for a paired observation. The two
// contributing IDs are ordered lexicographically so the same physical pair gets
// the same identity regardless of which sample arrived first.
func pairEventID(from, to, a, b string) string {
	if b < a {
		a, b = b, a
	}
	return from + "→" + to + "|" + a + "+" + b
}
