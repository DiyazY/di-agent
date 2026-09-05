// Package profiles wires contract implementations into a ready SemanticMap.
// Agent code calls Build("edge-minimal") — it never imports internal packages.
package profiles

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/DiyazY/di-agent/internal/minimal"
	"github.com/DiyazY/di-agent/internal/scripted"
	"github.com/DiyazY/di-agent/pkg/contracts"
	"github.com/DiyazY/di-agent/pkg/domain"
	"github.com/DiyazY/di-agent/pkg/peers"
	"github.com/DiyazY/di-agent/pkg/semmap"
	"github.com/DiyazY/di-agent/pkg/statemap"
)

// defaultPeerTimeout caps a single peer HTTP call in v1. LAN-local peers
// resolve in single-digit milliseconds; 2s gives an order of magnitude of
// headroom for a stalled peer without making the reasoner block forever.
const defaultPeerTimeout = 2 * time.Second

// Config holds profile-specific tuning parameters.
type Config struct {
	// EMAAlphaSlow is the decay factor for the established layer. Zero takes the
	// derived default in statemap.Config.
	EMAAlphaSlow float64

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

	// CgroupSubjects walks pod cgroups (and CgroupUnitGlobs-allowlisted units) as
	// subjects. CgroupMaxSubjects bounds the walk (0 = 256). CgroupCmdLabel stamps a
	// cmd= label from /proc (needs hostPID). See minimal.CgroupOptions.
	CgroupSubjects    bool
	CgroupUnitGlobs   []string
	CgroupMaxSubjects int
	CgroupCmdLabel    bool

	// ScriptPath runs the synthetic system from a scenario file instead of any real
	// collector — for driving a live daemon through a known story.
	ScriptPath string

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

	// PairWindowSeconds, PairMinSupport and PairWindow tune the state model's paired
	// estimator: how far apart two observations may be and still count as
	// simultaneous, how many pairs a relationship needs before its strength moves,
	// and how many recent pairs the estimate is computed over. Zero means the
	// estimator's own defaults (15s, 8, 60).
	//
	// There used to be a Relational flag beside these, selecting between two
	// estimators — one that moved an edge on any single endpoint observation, and one
	// that waited for both. Only the paired reading was defensible (a single
	// construct's magnitude is not an observation of an association's strength), and
	// it is what the state model does; the choice went with the second model.
	PairWindowSeconds int
	PairMinSupport    int
	PairWindow        int

	// StateMap is the live state model this agent reasons from: every answer is read
	// from it and recorded in its journal, so each one carries a DecisionID that
	// reproduces its inputs.
	//
	// Nil is allowed and means "build one with the defaults" rather than "run without
	// one". There is nothing left for an agent without a state model to do — cost,
	// estimates, explanations and the graph surfaces are all read from it — so a
	// half-wired agent is a wiring mistake that used to surface much later as an empty
	// answer. A caller that wants control over the lifecycle (persistence, sweeps,
	// injected clock) passes its own, which is what the daemon does.
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
		EMAAlphaSlow:         0.001,
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
// The file supplies STRUCTURE and no magnitude. It carried a per-proposition
// `prior_strength` and a per-distribution `prior_weight`/`ema_weight` table until the
// seeded design was removed; those fields were still parsed here for a while after
// nothing read them, which is the failure this project treats as worse than an absent
// number. They are gone from the artefact and from this struct together.
type priorWeightsFile struct {
	Version       string              `json:"version"`
	GeneratedAt   string              `json:"generated_at"`
	Distributions []string            `json:"distributions"`
	Propositions  map[string]propDiag `json:"propositions"`
	AgentEdges    []agentEdge         `json:"agent_edges"`
}

// propDiag is the per-proposition diagnostic block. It records what the retired
// calibration would have produced and why it was not fit to seed a decision — the
// rank correlation, its p-value, and whether the proxy agreed with the declared
// direction. No field of it reaches a relationship.
type propDiag struct {
	WouldHaveBeen  float64 `json:"would_have_been"`
	Direction      string  `json:"direction"`
	SpearmanRho    float64 `json:"spearman_rho"`
	PValue         float64 `json:"p_value"`
	SignConsistent int     `json:"sign_consistent"`
	Method         string  `json:"method"`
}

// agentEdge is one entry in agent_edges: the structure of a relationship the agent
// instantiates, with no magnitude. It is not per-distribution, because which
// constructs relate and in which direction is a property of the domain.
type agentEdge struct {
	FromID        string `json:"from_id"`
	ToID          string `json:"to_id"`
	PropositionID string `json:"proposition_id"`
	Direction     string `json:"direction"`
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
		// A caller that did not supply a state model gets one. Everything this profile
		// answers is read from it, so building the agent without one produces something
		// that looks assembled and cannot answer.
		if cfg.StateMap == nil {
			cfg.StateMap = statemap.New(statemap.Config{
				ConvergenceObservations: int(cfg.ConvergenceThreshold),
				Alpha:                   cfg.EMAAlpha,
				AlphaSlow:               cfg.EMAAlphaSlow,
				AdmitUnknown:            true,
				Learn:                   true,
			}, statemap.NewJournal(0))
		}
		// Seed the state model here, where both the specification and the calibration are
		// in hand: relationships get this cluster's priors instead of a placeholder.
		if _, err := seedStateMap(cfg.StateMap, cfg.DomainSpec, pw, cfg.KD); err != nil {
			return nil, nil, err
		}
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
	ontology := minimal.NewOntologyFromSpec(cfg.DomainSpec)

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

	reasoner := minimal.NewRuleEngineReasoner(cfg.DomainSpec, cfg.MinTrustScore, peerRegistry, peerClient)
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
		proposer = minimal.NewMICorrelationProposer(cfg.StateMap, thresh, minPairs, bufSize,
			time.Duration(cfg.PairWindowSeconds)*time.Second)
	} else {
		proposer = minimal.NewDisabledProposer()
	}

	var tuner contracts.TunerContract
	if cfg.UseRuleBasedTuner {
		tuner = minimal.NewRuleBasedTunerFromSpec(cfg.DomainSpec)
	} else {
		tuner = minimal.NewDisabledTuner()
	}

	// Calibrated proposition strengths reach the declaration layer so /propositions
	// reports what this cluster was calibrated to. The operative copy is in the state
	// model, seeded from the same file in seedStateMap.
	sm := semmap.NewWithPeers(ontology, reasoner, proposer, tuner, peerRegistry, peerClient)
	sm.SetIdentity(cfg.NodeID, cfg.AcceptForeignSamples)
	// Both halves need it: the facade feeds observations into the model, and the
	// reasoner answers from it. Attaching only one would give a model that fills up
	// and never gets read, or a reasoner reading a model nothing updates.
	sm.AttachState(cfg.StateMap)
	reasoner.AttachState(cfg.StateMap)

	// Build collector(s): Netdata, Cgroup, or both via MultiCollector.
	// Empty CgroupRoot/NodeID or empty NetdataURL disables the respective
	// collector. Both absent → nil collector, collection loop disabled.
	var collector contracts.CollectorContract
	hasCgroup := cfg.CgroupRoot != "" && cfg.NodeID != ""
	hasNetdata := cfg.NetdataURL != ""

	cgOpts := minimal.CgroupOptions{
		Subjects: cfg.CgroupSubjects, UnitGlobs: cfg.CgroupUnitGlobs,
		MaxSubjects: cfg.CgroupMaxSubjects, CmdLabel: cfg.CgroupCmdLabel,
	}

	if cfg.ScriptPath != "" {
		sc, err := scripted.LoadScenario(cfg.ScriptPath)
		if err != nil {
			log.Printf("script: %v; no collector", err)
			return sm, nil
		}
		return sm, scripted.NewSystemScript(cfg.NodeID, sc, time.Now())
	}

	switch {
	case hasCgroup && hasNetdata:
		cgroupC := minimal.NewCgroupCollectorWithOptions(cfg.NodeID, cfg.CgroupRoot, cgOpts)
		netdataC := minimal.NewNetdataCollector(cfg.NodeID, cfg.NetdataURL, nil)
		collector = minimal.NewMultiCollector(cgroupC, netdataC)
	case hasNetdata:
		collector = minimal.NewNetdataCollector(cfg.NodeID, cfg.NetdataURL, nil)
	case hasCgroup:
		collector = minimal.NewCgroupCollectorWithOptions(cfg.NodeID, cfg.CgroupRoot, cgOpts)
		// else: collector stays nil — collection loop disabled
	}

	return sm, collector
}

// The calibration is applied in one place — seedStateMap, which puts each per-cluster
// prior on the relationship it belongs to. Two functions used to live here that also
// wrote the strengths into the declaration layer, plus a reconciliation pass to stop the
// two disagreeing. The declaration layer no longer holds a strength: GET /propositions
// overlays the value in force from the model (see semmap.projectedPropositions), so there
// is nothing left to reconcile and no window in which the exposed number and the used
// number can differ.

// perKDEdgeWeights and edgeKey stood here. Both existed to look up a calibrated
// magnitude for one cluster, and both were dead — no non-test caller remained once
// seeding declared structure only. They are removed rather than kept "in case",
// because a lookup helper for a field the artefact no longer emits is an invitation
// to reintroduce the seeding it served.
