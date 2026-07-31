package semmap

import (
	"github.com/DiyazY/di-agent/pkg/contracts"
	"github.com/DiyazY/di-agent/pkg/domain"
	"github.com/DiyazY/di-agent/pkg/types"
)

// MetricRouter resolves a MetricType to the construct it observes. The routing
// table is domain data — it lives in domain_spec.json alongside the constructs
// and propositions — so the Bridge takes a router rather than carrying a table
// of its own. *domain.Spec satisfies this.
//
// This indirection is the whole point: a deployment that declares a new
// construct and routes a metric to it needs no change here, and a reader can
// tell what a running agent routes by reading its specification instead of its
// source.
type MetricRouter interface {
	ConstructForMetric(metricType string) (string, bool)
}

// SpecCarrier is implemented by ontologies built from a domain specification.
// The facade uses it to recover the routing table without threading the spec
// through every constructor.
type SpecCarrier interface {
	Spec() *domain.Spec
}

// edgeUpdater is the slim subset of UpdaterContract the Bridge depends on.
// Defined separately so tests can supply a lightweight stub that counts calls
// without implementing UpdateNode/Reset.
type edgeUpdater interface {
	UpdateEdge(fromID, toID string, observation float64, eventID string) (*types.EdgeDescriptor, error)
}

// Bridge fans a single MetricSample out to every UpdateEdge call its primary
// construct touches. It is stateless: the routing decision is fully determined
// by the metric type and the ontology's current backbone.
//
// Behavior:
//   - The sample's MetricType is mapped to its construct via the router, i.e.
//     the loaded domain specification. Unrouted types are a silent no-op, for
//     forward compatibility with collectors ahead of the specification.
//   - ontology.Relationships(construct) returns every proposition touching
//     that construct (incoming OR outgoing). Bridge calls UpdateEdge once per
//     unique (from, to) endpoint pair — the Updater fans out internally to
//     every proposition sharing that pair (e.g. P2 and P3 on RC→PS).
//   - Per-edge errors do not abort the loop; they are returned (first wins)
//     so callers can log without short-circuiting the rest of the sample.
func Bridge(
	sample *types.MetricSample,
	router MetricRouter,
	ontology contracts.OntologyContract,
	updater edgeUpdater,
) error {
	if sample == nil || router == nil {
		return nil
	}
	construct, ok := router.ConstructForMetric(string(sample.MetricType))
	if !ok {
		// The loaded specification routes no construct for this MetricType.
		// Ignored rather than rejected, so a collector upgraded ahead of the
		// specification still ingests.
		return nil
	}

	props, err := ontology.Relationships(construct)
	if err != nil {
		return err
	}

	// De-duplicate by (from, to). The multigraph storage holds one
	// EdgeDescriptor per proposition, but UpdateEdge already fans out to
	// every proposition sharing the same endpoint pair — calling it once
	// per unique pair is sufficient and avoids double-counting.
	type pair struct{ from, to string }
	seen := make(map[pair]struct{}, len(props))
	var firstErr error
	for _, p := range props {
		if p == nil || p.Deprecated {
			continue
		}
		pr := pair{p.FromConstruct, p.ToConstruct}
		if _, dup := seen[pr]; dup {
			continue
		}
		seen[pr] = struct{}{}
		if _, err := updater.UpdateEdge(p.FromConstruct, p.ToConstruct, sample.Value, sample.EventID); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			// Log-and-continue: keep processing the remaining edges so a
			// single bad pair does not silence the whole sample.
			continue
		}
	}
	return firstErr
}
