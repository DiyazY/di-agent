// Package profiles wires contract implementations into a ready SemanticMap.
// Agent code calls Build("edge-minimal") — it never imports internal packages.
package profiles

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/DiyazY/di-agent/internal/minimal"
	"github.com/DiyazY/di-agent/pkg/contracts"
	"github.com/DiyazY/di-agent/pkg/domain"
	"github.com/DiyazY/di-agent/pkg/peers"
	"github.com/DiyazY/di-agent/pkg/semmap"
	"github.com/DiyazY/di-agent/pkg/statemap"
	"github.com/DiyazY/di-agent/pkg/types"
)

// defaultPeerTimeout caps a single peer HTTP call in v1. LAN-local peers
// resolve in single-digit milliseconds; 2s gives an order of magnitude of
// headroom for a stalled peer without making the reasoner block forever.
const defaultPeerTimeout = 2 * time.Second

// Config holds profile-specific tuning parameters.
type Config struct {
	// EMAAlpha is the decay factor for the EMA updater (0 < alpha < 1).
	// Higher = reacts faster to change; lower = more stable. Default: 0.2.
	EMAAlpha float64

	// ConvergenceThreshold is the number of observations at which an edge's
	// confidence reaches 1.0. Default: 500.
	ConvergenceThreshold float64

	// MinTrustScore is the minimum trust score a peer must have to be
	// considered for task offloading. Default: 0.5.
	MinTrustScore float64

	// PriorWeightsPath is an optional path to a prior_weights.json file
	// produced by the Python prior initialization pipeline. When set, the
	// proposition PriorStrength values loaded from the file override the
	// hardcoded Di-Select defaults.  Empty string = use hardcoded defaults.
	PriorWeightsPath string

	// KD is the Kubernetes distribution this agent is running on (e.g.
	// "k3s", "k0s", "k8s", "kubeEdge", "openYurt"). When set together with
	// PriorWeightsPath, the per-distribution edge weights from the file are
	// used to seed storage instead of the global proposition strengths.
	// Empty string = no per-distribution adjustment (global priors used).
	KD string

	// NodeID identifies this agent within the cluster (used by the Collector
	// when generating event IDs). Empty string → callers (typically main)
	// fall back to os.Hostname().
	NodeID string

	// CgroupRoot is the directory the CgroupCollector reads from.
	// Production: /sys/fs/cgroup; tests use t.TempDir() with fake files.
	// Empty string disables the cgroup collector (Build returns nil for the
	// CollectorContract handle).
	CgroupRoot string

	// NetdataURL is the base URL of a Netdata daemon to poll for live metrics
	// (e.g. "http://localhost:19999"). Empty string disables the Netdata collector.
	// When set together with CgroupRoot, both collectors run as a MultiCollector.
	NetdataURL string

	// CollectInterval is how often the collection scheduler ticks. Zero
	// disables the loop entirely. Callers (main.go) default to 10s.
	CollectInterval time.Duration

	// PeerURLs is the static list of known remote agent URLs to register at
	// startup (e.g. ["http://node_1:8080", "http://node_2:8080"]). Each URL
	// is added to the in-memory peer registry with default trust 0.5; the
	// reasoner discovers them via SemanticMap.Peers(). Empty or nil → the
	// daemon starts with no peers and RecommendPeer returns
	// ErrInsufficientTrust until /peers POSTs populate the registry.
	PeerURLs []string

	// PeerTimeout overrides the per-call HTTP timeout for outbound peer
	// queries. Zero → defaultPeerTimeout (2s).
	PeerTimeout time.Duration

	// UseProposer enables the MICorrelationProposer instead of the DisabledProposer.
	// When false (default in tests), the proposer is a silent no-op.
	UseProposer bool
	// ProposerThreshold is the |Pearson r| threshold to emit a candidate.
	// Defaults to 0.85 when UseProposer is true and the field is zero.
	ProposerThreshold float64
	// ProposerMinPairs is the minimum number of paired observations required
	// before correlation is evaluated. Defaults to 30.
	ProposerMinPairs int
	// ProposerBufSize is the ring buffer capacity per construct pair. Defaults to 120.
	ProposerBufSize int

	// DomainSpec is the loaded domain model: constructs, metric routing,
	// propositions, adjustment policy and operator vocabulary. Required — a
	// profile without one has no graph to reason over. See pkg/domain.
	DomainSpec *domain.Spec

	// UseRuleBasedTuner enables the RuleBasedTuner instead of the DisabledTuner.
	// When true (default for edge-minimal), natural-language operator intent can be
	// mapped to proposition strength adjustments via SemanticMap.Tune / POST /agent/tune.
	// Set false to disable operator tuning entirely.
	UseRuleBasedTuner bool

	// Relational selects the RelationalEMAUpdater, which learns each edge's
	// weight from paired observations of its two endpoints instead of from
	// whichever endpoint a sample observed. The two modes answer different
	// questions and are not interchangeable:
	//
	//   endpoint (default) — the weight tracks the magnitude of one construct.
	//                        Every sample moves an edge, so confidence grows with
	//                        the raw sample rate.
	//   relational        — the weight tracks the strength of the association
	//                        between the endpoints, on the same scale as the
	//                        calibrated prior. An edge advances only when both
	//                        endpoints have been observed within the pairing
	//                        window, so confidence grows with paired coverage.
	//
	// Default stays endpoint so previously reported measurements remain
	// reproducible from the same flags.
	Relational bool

	// PairWindowSeconds is how far apart two construct observations may be and
	// still form a pair. Only used when Relational is set. Zero means
	// semmap.DefaultPairWindowSeconds.
	PairWindowSeconds int

	// PairMinSupport is the number of pairs an edge needs before the relational
	// updater emits a strength; PairWindow is the sliding window length. Zero
	// means the built-in defaults (8 and 60).
	PairMinSupport int
	PairWindow     int

	// StateMap is the live state model this agent reasons from. When set, the
	// reasoner answers cost queries from it and records each answer in its journal,
	// so every answer carries a DecisionID that reproduces its inputs. Nil leaves the
	// reasoner reading the storage graph, whose answers are not traceable.
	StateMap *statemap.Map

	// AcceptForeignSamples lets this agent ingest telemetry labelled with other
	// machines' IDs into its own graph. The map is node-local by design (one agent
	// per machine, its graph holding that machine's evidence), so the default is
	// off: an aggregate over machines that are different physical systems produces
	// edge weights that are means over incomparable mechanisms. A whole-testbed
	// replay is the legitimate exception and sets this explicitly.
	AcceptForeignSamples bool
}

func DefaultConfig() Config {
	return Config{
		EMAAlpha:             0.2,
		ConvergenceThreshold: 500,
		MinTrustScore:        0.5,
		PriorWeightsPath:     "",
		KD:                   "",
		UseRuleBasedTuner:    true,
	}
}

// ── prior_weights.json schema ─────────────────────────────────────────────────

// priorWeightsFile mirrors the top-level structure of prior_weights.json
// produced by semantic_map.prior_init.pipeline.
type priorWeightsFile struct {
	Version                 string                          `json:"version"`
	GeneratedAt             string                          `json:"generated_at"`
	Distributions           []string                        `json:"distributions"`
	Propositions            map[string]propositionPrior     `json:"propositions"`
	DistributionEdgeWeights map[string]map[string]edgePrior `json:"distribution_edge_weights"`
}

type propositionPrior struct {
	PriorStrength float64 `json:"prior_strength"`
	Direction     string  `json:"direction"`
	Method        string  `json:"method"`
}

// edgePrior is one entry in distribution_edge_weights[kd][edge_key].
// edge_key has the form "fromID→toID:propositionID".
type edgePrior struct {
	FromID        string  `json:"from_id"`
	ToID          string  `json:"to_id"`
	PropositionID string  `json:"proposition_id"`
	Direction     string  `json:"direction"`
	PriorWeight   float64 `json:"prior_weight"`
	EMAWeight     float64 `json:"ema_weight"`
}

// loadPriorWeights reads prior_weights.json from the given path.
// Returns nil (no error) if the path is empty — caller uses hardcoded defaults.
func loadPriorWeights(path string) (*priorWeightsFile, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("prior_weights: cannot read %q: %w", path, err)
	}
	var pw priorWeightsFile
	if err := json.Unmarshal(data, &pw); err != nil {
		return nil, fmt.Errorf("prior_weights: malformed JSON in %q: %w", path, err)
	}
	return &pw, nil
}

// ── Build ─────────────────────────────────────────────────────────────────────

// Build constructs and returns a fully wired SemanticMap for the named
// profile along with the profile's collector (if any). The returned
// CollectorContract may be nil — the daemon must treat nil as "no
// autonomous collection loop in this profile / configuration" and skip
// scheduling.
func Build(profileName string, cfg Config) (*semmap.SemanticMap, contracts.CollectorContract, error) {
	pw, err := loadPriorWeights(cfg.PriorWeightsPath)
	if err != nil {
		return nil, nil, err
	}
	if err := validateKD(pw, cfg.KD); err != nil {
		return nil, nil, err
	}
	// A profile without a domain model has no graph to reason over. Fail here
	// rather than constructing an agent whose /graph is empty, which would look
	// identical to an agent whose telemetry has not arrived yet.
	if cfg.DomainSpec == nil {
		return nil, nil, fmt.Errorf("no domain spec: pass -domain <path> to load one")
	}
	switch profileName {
	case "edge-minimal":
		sm, coll := buildEdgeMinimal(cfg, pw)
		return sm, coll, nil
	default:
		return nil, nil, fmt.Errorf("unknown profile %q", profileName)
	}
}

// validateKD checks that the configured KD is one of the distributions the
// prior_weights.json file knows about. An empty KD is always allowed (means
// "use global priors, no per-distribution adjustment").
func validateKD(pw *priorWeightsFile, kd string) error {
	if kd == "" || pw == nil || len(pw.Distributions) == 0 {
		return nil
	}
	for _, d := range pw.Distributions {
		if d == kd {
			return nil
		}
	}
	return fmt.Errorf("KD %q not found in prior_weights.json distributions %v", kd, pw.Distributions)
}

func buildEdgeMinimal(cfg Config, pw *priorWeightsFile) (*semmap.SemanticMap, contracts.CollectorContract) {
	storage := minimal.NewInMemoryStorage()
	ontology := minimal.NewOntologyFromSpec(cfg.DomainSpec)

	var updater contracts.UpdaterContract = minimal.NewEMAUpdater(storage, cfg.EMAAlpha, cfg.ConvergenceThreshold)
	if cfg.Relational {
		minSupport, window := cfg.PairMinSupport, cfg.PairWindow
		if minSupport <= 0 {
			minSupport = 8
		}
		if window <= 0 {
			window = 60
		}
		updater = minimal.NewRelationalEMAUpdater(storage, cfg.EMAAlpha,
			cfg.ConvergenceThreshold, minSupport, window)
	}

	// Peer registry + outbound HTTP client. Always constructed (cheap, no
	// network I/O) so the reasoner has a place to look up peers even when
	// the daemon was started without -peers; the /peers POST endpoint can
	// then populate it at runtime.
	peerRegistry := peers.NewRegistry()
	timeout := cfg.PeerTimeout
	if timeout <= 0 {
		timeout = defaultPeerTimeout
	}
	peerClient := peers.NewClient(timeout)
	for _, url := range cfg.PeerURLs {
		if url == "" {
			continue
		}
		// Best-effort: an invalid URL surfaces as ErrEmptyURL (already
		// filtered above) — any other error is impossible from Add today.
		_, _ = peerRegistry.Add(url, "")
	}

	reasoner := minimal.NewRuleEngineReasoner(storage, ontology, cfg.MinTrustScore, peerRegistry, peerClient)
	var proposer contracts.ProposerContract
	if cfg.UseProposer {
		thresh := cfg.ProposerThreshold
		if thresh == 0 {
			thresh = 0.85
		}
		minPairs := cfg.ProposerMinPairs
		if minPairs == 0 {
			minPairs = 30
		}
		bufSize := cfg.ProposerBufSize
		if bufSize == 0 {
			bufSize = 120
		}
		proposer = minimal.NewMICorrelationProposer(ontology, thresh, minPairs, bufSize)
	} else {
		proposer = minimal.NewDisabledProposer()
	}

	var tuner contracts.TunerContract
	if cfg.UseRuleBasedTuner {
		tuner = minimal.NewRuleBasedTunerFromSpec(cfg.DomainSpec)
	} else {
		tuner = minimal.NewDisabledTuner()
	}

	// Apply calibrated proposition strengths from the pipeline before seeding
	// storage. Per-KD edge weights (if any) are applied during seeding.
	if pw != nil {
		applyPriorWeights(ontology, pw)
	}

	seedFromOntology(storage, ontology, pw, cfg.KD)

	sm := semmap.NewWithPeers(storage, ontology, updater, reasoner, proposer, tuner, peerRegistry, peerClient)
	sm.SetIdentity(cfg.NodeID, cfg.AcceptForeignSamples)
	if cfg.StateMap != nil {
		// Both halves need it: the facade feeds observations into the model, and the
		// reasoner answers from it. Attaching only one would give a model that fills up
		// and never gets read, or a reasoner reading a model nothing updates.
		sm.AttachState(cfg.StateMap)
		reasoner.AttachState(cfg.StateMap)
	}
	if cfg.Relational && cfg.PairWindowSeconds > 0 {
		sm.SetPairWindow(cfg.PairWindowSeconds)
	}

	// Build collector(s): Netdata, Cgroup, or both via MultiCollector.
	// Empty CgroupRoot/NodeID or empty NetdataURL disables the respective
	// collector. Both absent → nil collector, collection loop disabled.
	var collector contracts.CollectorContract
	hasCgroup := cfg.CgroupRoot != "" && cfg.NodeID != ""
	hasNetdata := cfg.NetdataURL != ""

	switch {
	case hasCgroup && hasNetdata:
		cgroupC := minimal.NewCgroupCollector(cfg.NodeID, cfg.CgroupRoot)
		netdataC := minimal.NewNetdataCollector(cfg.NodeID, cfg.NetdataURL, nil)
		collector = minimal.NewMultiCollector(cgroupC, netdataC)
	case hasNetdata:
		collector = minimal.NewNetdataCollector(cfg.NodeID, cfg.NetdataURL, nil)
	case hasCgroup:
		collector = minimal.NewCgroupCollector(cfg.NodeID, cfg.CgroupRoot)
		// else: collector stays nil — collection loop disabled
	}

	return sm, collector
}

// applyPriorWeights overwrites proposition PriorStrength values in the ontology
// with those from prior_weights.json via the ontology's safe setter (locks
// internally, does not mutate pointers returned by Propositions()). Unknown
// proposition IDs are silently ignored so old files remain compatible with
// new code.
func applyPriorWeights(ontology *minimal.SpecOntology, pw *priorWeightsFile) {
	for propID, entry := range pw.Propositions {
		_ = ontology.SetPropositionStrength(propID, entry.PriorStrength)
	}
}

// seedFromOntology pre-populates storage with one node per construct and one
// edge per proposition. Edge prior_weight selection precedence:
//
//  1. pw.DistributionEdgeWeights[cfg.KD][edgeKey] (per-KD calibrated, if both KD
//     and the file are provided);
//  2. proposition PriorStrength from the ontology (which may have been
//     overwritten by applyPriorWeights from the global pw.Propositions table).
//
// Whichever value wins is written back to the ontology as well. Without that,
// the two layers disagree for the whole cold-start period: the Reasoner reads
// the per-KD weight from storage while GET /propositions reports the global
// strength, and the first operator tune records a change from a number the
// agent never used.
func seedFromOntology(
	storage *minimal.InMemoryStorage,
	ontology *minimal.SpecOntology,
	pw *priorWeightsFile,
	kd string,
) {
	constructs, _ := ontology.Constructs()
	for _, c := range constructs {
		_ = storage.PutNode(&types.NodeDescriptor{
			NodeID:        c.ConstructID,
			ConstructType: c.Name,
			PriorValue:    0.5, // neutral prior; per-distribution values seeded by Netdata adapter
			EMAValue:      0.5,
			Confidence:    0.0,
			NObservations: 0,
		})
	}

	perKD := perKDEdgeWeights(pw, kd)

	propositions, _ := ontology.Propositions()
	for _, p := range propositions {
		prior := p.PriorStrength
		if e, ok := perKD[edgeKey(p.FromConstruct, p.ToConstruct, p.PropositionID)]; ok {
			prior = e.PriorWeight
			_ = ontology.SetPropositionStrength(p.PropositionID, prior)
		}
		_ = storage.PutEdge(&types.EdgeDescriptor{
			FromID:        p.FromConstruct,
			ToID:          p.ToConstruct,
			PropositionID: p.PropositionID,
			Direction:     p.Direction,
			PriorWeight:   prior,
			EMAWeight:     prior, // starts equal to prior
			Confidence:    0.0,
			NObservations: 0,
		})
	}
}

// perKDEdgeWeights returns the per-distribution edge map for kd, or nil if not
// applicable. Callers must handle the nil case (fall back to global priors).
// perKDEdgeWeights returns the calibrated edge priors for one cluster, or nil.
func perKDEdgeWeights(pw *priorWeightsFile, kd string) map[string]edgePrior {
	if pw == nil || kd == "" {
		return nil
	}
	return pw.DistributionEdgeWeights[kd]
}

// edgeKey mirrors the key format produced by prior_init/pipeline.py:
// "{from_c}→{to_c}:{prop_id}".
func edgeKey(fromID, toID, propID string) string {
	return fmt.Sprintf("%s→%s:%s", fromID, toID, propID)
}
