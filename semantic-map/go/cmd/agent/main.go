// Command agent is the edge-minimal Semantic Map daemon.
//
// It loads the configured profile, seeds the graph from Di-Select priors,
// and serves the agent queries plus the graph control surface over
// HTTP/JSON on :8080. Telemetry is accepted via POST /ingest-sample. The graph
// introspection (/graph, /edges, /history, /constructs, /propositions,
// /neighbors), ontology mutation (/ontology/*), candidate review
// (/candidates/{id}/{confirm,reject,defer}), edge reset (/agent/reset),
// and operator meta (/healthz, /version, /ui/) routes are wired by
// registerRoutes in routes.go.
//
// Usage:
//
//	agent -profile edge-minimal -addr :8080 -alpha 0.2 -convergence 500 \
//	      -priors /path/to/prior_weights.json -kd k0s \
//	      -collect-interval 10s -cgroup-root /sys/fs/cgroup \
//	      -peers http://node_1:8080,http://node_2:8080
//
// The -kd flag selects per-distribution edge weights from prior_weights.json
// when set. Omit it (or pass an empty string) to use the global Di-Select
// proposition strengths.
//
// The autonomous collection loop ticks at -collect-interval, calls
// CollectorContract.Collect on the profile's collector, and runs each sample
// through the facade's IngestSample. Setting -collect-interval=0 or
// -cgroup-root="" disables it (the manual POST /ingest-sample path still works).
//
// -regime (stable|default|bursty|volatile) sets alpha and convergence to a
// pre-characterised bundle matching the deployment's dynamics. Overrides any
// explicit -alpha and -convergence values when set.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/DiyazY/di-agent/pkg/contracts"
	"github.com/DiyazY/di-agent/pkg/domain"
	"github.com/DiyazY/di-agent/pkg/explain"
	"github.com/DiyazY/di-agent/pkg/peers"
	"github.com/DiyazY/di-agent/pkg/profiles"
	"github.com/DiyazY/di-agent/pkg/semmap"
	"github.com/DiyazY/di-agent/pkg/statemap"
)

// Version is the daemon's reported semver. Returned by GET /version.
// Phase 1 ships 0.1.0 (HTTP expansion). Bump for future control-surface work.
const Version = "0.1.0"

// BuildCommit is the short git SHA the binary was built from. Empty when the
// binary is built without `-ldflags "-X main.BuildCommit=…"`.
var BuildCommit = ""

func main() {
	profileName := flag.String("profile", "edge-minimal", "deployment profile")
	addr := flag.String("addr", ":8080", "HTTP listen address")
	alpha := flag.Float64("alpha", 0.2, "EMA decay factor for the RECENT layer (0 < alpha < 1)")
	alphaSlow := flag.Float64("alpha-slow", 0.001,
		"EMA decay factor for the ESTABLISHED layer: the same paired observations read on a "+
			"slower clock, answering what is normal for this machine rather than what is "+
			"happening now. The default is a design choice on a measured trade-off, not a "+
			"derived optimum: order-invariance and responsiveness trade off monotonically, "+
			"so the measurement supports a band (roughly 0.004 to 0.0005) and 0.001 sits "+
			"mid-band with ~10x the recent layer's order-invariance and about a third of "+
			"its responsiveness. Pinning one value needs a stated requirement about how "+
			"fast a baseline should follow a persistent change. An offline fit reported an "+
			"interior maximum; a 135-stream sweep against this daemon refuted it as an "+
			"artefact of the offline streams being ~6x shorter than a deployment's.")
	convergence := flag.Float64("convergence", 500, "observations for confidence=1.0")
	minTrust := flag.Float64("min-trust", 0.5, "minimum peer trust score")
	priorsPath := flag.String("priors", "", "path to prior_weights.json from initialization pipeline")
	kd := flag.String("kd", "", "Kubernetes distribution running on this node "+
		"(k3s|k0s|k8s|kubeEdge|openYurt); selects per-KD edge weights from -priors when set")
	collectInterval := flag.Duration("collect-interval", 10*time.Second,
		"how often the collection loop ticks the Collector; 0 disables the loop")
	cgroupRoot := flag.String("cgroup-root", "/sys/fs/cgroup",
		"filesystem root the cgroup collector reads from; empty string disables the loop")
	nodeID := flag.String("node-id", "",
		"the machine this agent models. Used both to stamp its own MetricSamples and "+
			"as its identity: the map is node-local, so samples labelled with another "+
			"machine's ID are rejected unless -ingest-scope=any. Empty falls back to "+
			"os.Hostname()")
	staleAfter := flag.Duration("stale-after", 2*time.Minute,
		"silence after which a property is marked stale. A model that keeps reporting "+
			"a departed metric's last value asserts something it cannot support, so the "+
			"map says 'stale' instead of holding the number quietly.")
	retireAfter := flag.Duration("retire-after", 0,
		"silence after which a property is retired automatically. 0 leaves retirement "+
			"to an operator, which is the safer default for a system whose collectors "+
			"restart.")
	noLearn := flag.Bool("no-learn", false,
		"stop relationships learning their strength from paired observations of both "+
			"endpoints. They then stay at their seeded priors with confidence 0, which is "+
			"the honest report when nothing has been learned — but the map never improves "+
			"on what it was told.")
	pairWindowS := flag.Int("pair-window-seconds", 15,
		"how far apart two observations may be and still count as simultaneous for "+
			"learning. A tolerance, not smoothing: collectors sample on independent grids, "+
			"so without one no pair ever forms.")
	noAdmit := flag.Bool("no-admit", false,
		"reject observations of properties that were never declared. Off by default: a "+
			"model of a changing system that cannot represent something new is a model "+
			"of the system as it was when someone wrote it down.")
	sweepInterval := flag.Duration("sweep-interval", 0,
		"how often to apply lifecycle transitions. 0 derives it from -stale-after "+
			"(half, floored at 5s). Without a sweep, staleness and retirement only "+
			"happen when something asks for them, which makes -stale-after a setting "+
			"that describes nothing.")
	stateFile := flag.String("state-file", "",
		"path to persist the map and its journal. Empty keeps everything in memory, which "+
			"means a restart returns the agent to cold start on a system it has already "+
			"watched, and makes the audit trail an artefact of one process lifetime.")
	saveInterval := flag.Duration("save-interval", time.Minute,
		"how often to snapshot when -state-file is set. A snapshot is also written on "+
			"shutdown, so this bounds what an unclean exit loses rather than what a clean "+
			"one does.")
	journalSize := flag.Int("journal-size", 0,
		"entries of change and decision history to hold in memory (0 = default)")
	ingestScope := flag.String("ingest-scope", "self",
		"self: ingest only this machine's samples, which is what a node-local map means. "+
			"any: aggregate every machine's telemetry into one graph — correct for "+
			"replaying a whole testbed into a single daemon, wrong for a deployment, "+
			"because the resulting weights average over machines that may be different "+
			"physical systems")
	netdataURL := flag.String("netdata-url", "",
		"base URL of Netdata daemon to poll (e.g. http://localhost:19999). Empty disables Netdata collection.")
	peersFlag := flag.String("peers", "",
		"comma-separated peer agent URLs to register at startup "+
			"(e.g. http://node_1:8080,http://node_2:8080). RecommendPeer ranks "+
			"these by trust-weighted savings. Additional peers can be added at "+
			"runtime via POST /peers.")
	peerStatePoll := flag.Duration("peer-state-poll", 30*time.Second,
		"how often to fetch each peer's semantic map. 0 disables polling, leaving "+
			"GET /state/cluster to report only what a manual POST /state/peers/refresh "+
			"collected — an empty cluster view then means nobody asked, not that the "+
			"peers have no state.")
	peerStateStale := flag.Duration("peer-state-stale", 90*time.Second,
		"how old a peer's snapshot may be before the cluster view marks it history. "+
			"Nothing is deleted at this age: during a partition the last snapshot is "+
			"the only thing this agent has about that node, and it is labelled rather "+
			"than withheld.")
	domainPath := flag.String("domain", "",
		"path to domain_spec.json: the constructs, metric routing, propositions, "+
			"adjustment policy and operator vocabulary the agent reasons over. "+
			"Required — the binary carries no built-in model. When empty the daemon "+
			"searches upward from the working directory.")
	regime := flag.String("regime", "",
		"dynamics preset (stable|default|bursty|volatile); overrides -alpha and -convergence when set")
	var useProposer bool
	flag.BoolVar(&useProposer, "proposer", true, "enable MI correlation proposer (disable for low-CPU devices)")
	var useTuner bool
	flag.BoolVar(&useTuner, "tuner", true,
		"enable the RuleBasedTuner behind POST /agent/tune (disable to wire DisabledTuner, "+
			"which accepts requests and applies nothing)")

	pairSupport := flag.Int("pair-support", 0,
		"paired observations a relationship needs before its strength moves at all "+
			"(0 = default 8). Below it the pair is buffered and confidence keeps "+
			"reporting that nothing has been learned.")
	pairHistory := flag.Int("pair-history", 0,
		"recent pairs the strength estimate is computed over (0 = default 60). Older "+
			"pairs fall out, which is what lets a relationship follow a system whose "+
			"behaviour changes rather than averaging its whole history.")

	explainProvider := flag.String("explain-provider", "none",
		"natural-language explain provider: none (disabled) or openai-compatible "+
			"(routes to any OpenAI-compat backend — Ollama, llama-server, LM Studio, vLLM)")
	explainURL := flag.String("explain-url", "http://localhost:11434/v1",
		"base URL for the OpenAI-compatible backend (used when -explain-provider=openai-compatible)")
	explainModel := flag.String("explain-model", "qwen2.5:7b-instruct",
		"model name for the OpenAI-compatible backend")
	explainPrompt := flag.String("explain-prompt", "",
		"path to the system-prompt file for /explain (default: cmd/agent/prompts/explain-v1.md alongside the binary)")
	explainAPIKey := flag.String("explain-api-key", "",
		"optional bearer token for the OpenAI-compatible backend "+
			"(env var EXPLAIN_API_KEY takes precedence when both are set)")
	explainPlannerPrompt := flag.String("explain-planner-prompt", "",
		"path to the planner system prompt; empty derives planner-v1.md from -explain-prompt's directory")
	explainCriticPrompt := flag.String("explain-critic-prompt", "",
		"path to the critic system prompt; empty derives critic-v1.md from -explain-prompt's directory")
	explainKeepAlive := flag.String("explain-keep-alive", "30m",
		"how long the backend should keep the model resident between calls "+
			"(Ollama `keep_alive`); empty disables the hint")
	explainSessions := flag.Bool("explain-sessions", true,
		"enable multi-turn session memory and the session-scoped tool cache for /explain")

	flag.Parse()

	if err := checkNoPositionalArgs(flag.NArg(), flag.Arg(0)); err != nil {
		log.Fatalf("%v", err)
	}

	if err := applyRegime(*regime, alpha, convergence); err != nil {
		log.Fatalf("invalid -regime %q: %v", *regime, err)
	}

	if *nodeID == "" {
		if h, err := os.Hostname(); err == nil {
			*nodeID = h
		}
	}

	// The domain model is data, not code: the binary ships with no constructs,
	// propositions or routing of its own. An agent without a model has nothing to
	// reason over, so this fails loud rather than serving an empty graph.
	var (
		spec    *domain.Spec
		specErr error
	)
	if *domainPath != "" {
		spec, specErr = domain.Load(*domainPath)
	} else {
		spec, specErr = domain.LoadFound()
	}
	if specErr != nil {
		log.Fatalf("domain spec: %v (pass -domain <path>)", specErr)
	}
	log.Printf("domain model: %d constructs, %d propositions, %d routed metrics",
		len(spec.Constructs), len(spec.Propositions), len(spec.MetricRouting))
	switch *ingestScope {
	case "self":
		log.Printf("identity: %s (node-local: foreign samples rejected)", *nodeID)
	case "any":
		log.Printf("identity: %s (ingest-scope=any: this graph will be an aggregate over "+
			"every machine whose telemetry arrives, which is not a deployment topology)", *nodeID)
	default:
		log.Fatalf("invalid -ingest-scope %q (valid: self, any)", *ingestScope)
	}
	log.Printf("estimator: paired observations (window %ds, support %d, history %d)",
		*pairWindowS, orDefault(*pairSupport, 8), orDefault(*pairHistory, 60))

	peerURLs := parsePeerURLs(*peersFlag)

	cfg := profiles.Config{
		EMAAlpha:             *alpha,
		EMAAlphaSlow:         *alphaSlow,
		ConvergenceThreshold: *convergence,
		MinTrustScore:        *minTrust,
		PriorWeightsPath:     *priorsPath,
		KD:                   *kd,
		NodeID:               *nodeID,
		CgroupRoot:           *cgroupRoot,
		NetdataURL:           *netdataURL,
		CollectInterval:      *collectInterval,
		PeerURLs:             peerURLs,
		UseProposer:          useProposer,
		// Must be set explicitly: this literal does not start from
		// DefaultConfig(), so an omitted field is false, and omitting this one
		// silently wired DisabledTuner into every daemon. POST /agent/tune then
		// returned HTTP 200 with an empty applied[] — accepting the request and
		// doing nothing — which is indistinguishable from an intent that matched
		// no keyword group.
		UseRuleBasedTuner:    useTuner,
		DomainSpec:           spec,
		AcceptForeignSamples: *ingestScope == "any",
		PairWindowSeconds:    *pairWindowS,
		PairMinSupport:       *pairSupport,
		PairWindow:           *pairHistory,
	}

	// The state model: what this system exhibits, updated by every sample. It is
	// seeded from the domain specification so the properties the agent already knows
	// how to interpret exist before any telemetry arrives — an operator inspecting a
	// fresh agent should see the model it will fill in, not an empty map — and
	// admission is on, so a metric nobody declared still becomes a property.
	state := statemap.New(statemap.Config{
		// The map is owned by this node, and says so. Everything it reports about
		// itself is attributable once state starts crossing node boundaries, and a
		// snapshot from another machine is refused rather than adopted.
		Owner:                   *nodeID,
		StaleAfter:              *staleAfter,
		RetireAfter:             *retireAfter,
		ConvergenceObservations: int(*convergence),
		Alpha:                   *alpha,
		AlphaSlow:               *alphaSlow,
		AdmitUnknown:            !*noAdmit,
		Learn:                   !*noLearn,
		LearnConfig: statemap.LearnConfig{
			PairWindowSeconds: *pairWindowS,
			MinSupport:        orDefault(*pairSupport, 8),
			Window:            orDefault(*pairHistory, 60),
		},
	}, statemap.NewJournal(*journalSize))
	// Load before seeding. A snapshot holds what this agent learned; seeding holds what
	// the specification declares. Loading second would refuse (the map would be
	// populated), and merging blindly would silently pick a winner per property — so the
	// order is: restore, then let seeding re-declare, where the rules are explicit and
	// journalled.
	if *stateFile != "" {
		loaded, err := state.Load(*stateFile)
		if err != nil {
			log.Fatalf("restoring state from %s: %v", *stateFile, err)
		}
		if loaded {
			c := state.Census()
			log.Printf("state model: restored %d properties and %d relationships from %s "+
				"at revision %d", c.PropertiesTotal, c.RelationshipsTotal, *stateFile,
				state.Revision())
		} else {
			log.Printf("state model: no snapshot at %s, starting cold", *stateFile)
		}
	}

	// Seeding happens inside profiles.Build, which holds the calibration as well as
	// the specification, so relationships get this cluster's priors rather than a
	// placeholder.

	cfg.StateMap = state

	sm, collector, err := profiles.Build(*profileName, cfg)
	if err != nil {
		log.Fatalf("failed to build profile %q: %v", *profileName, err)
	}
	if len(peerURLs) > 0 {
		log.Printf("registered %d peers: %s", len(peerURLs), strings.Join(peerURLs, ", "))
	}

	explainer, err := buildExplainer(sm, explainOptions{
		Provider:      *explainProvider,
		BaseURL:       *explainURL,
		Model:         *explainModel,
		PromptPath:    *explainPrompt,
		PlannerPath:   *explainPlannerPrompt,
		CriticPath:    *explainCriticPrompt,
		FlagAPIKey:    *explainAPIKey,
		KeepAlive:     *explainKeepAlive,
		EnableSession: *explainSessions,
	})
	if err != nil {
		log.Fatalf("failed to build explainer: %v", err)
	}
	defer explainer.Close()

	// What this agent has heard from other agents, kept apart from what it observes
	// itself. This is how a cluster-level question is answered without pretending the
	// answer came from local observation.
	peerState := statemap.NewPeerStore(*nodeID, *peerStateStale)
	peerFetcher := &peerStateFetcher{
		// The registry, not the -peers list: a peer added at runtime via POST /peers is
		// then polled without a restart.
		registry: sm.Peers(),
		client:   peers.NewClient(5 * time.Second),
		store:    peerState,
	}

	mux := http.NewServeMux()
	registerRoutes(mux, sm, explainer)
	registerStateRoutes(mux, state)
	registerPeerStateRoutes(mux, state, peerState, peerFetcher)

	srv := &http.Server{Addr: *addr, Handler: mux}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		log.Printf("semantic-map agent starting profile=%s addr=%s", *profileName, *addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	// Start the autonomous collection loop if the profile produced a
	// collector AND the interval is positive. Both must hold — a configured
	// collector with interval=0 is a deliberately disabled loop (useful for
	// tests and for nodes that only accept manual POST /ingest).
	// Lifecycle is time-based, so something has to advance it. Without this loop
	// -stale-after and -retire-after would only take effect when an operator posted
	// /state/sweep, and a quiet property would keep being reported as current.
	startSweepLoop(ctx, state, sweepEvery(*sweepInterval, *staleAfter))
	startSaveLoop(ctx, state, *stateFile, *saveInterval)
	startPeerStateLoop(ctx, peerFetcher, *peerStatePoll)

	startCollectionLoop(ctx, sm, collector, *collectInterval)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutting down")
	cancel()

	// The final save happens HERE, synchronously, not in the save loop. Cancelling the
	// context and returning let the process exit while the goroutine was still racing to
	// write, so the shutdown snapshot was usually lost and a clean restart silently
	// dropped everything since the last periodic save.
	if *stateFile != "" {
		if err := state.Save(*stateFile); err != nil {
			log.Printf("state model: shutdown save failed: %v", err)
		} else {
			log.Printf("state model: saved at revision %d (shutdown)", state.Revision())
		}
	}
}

// regimePreset bundles the alpha and convergence values for a named dynamics regime.
type regimePreset struct {
	alpha       float64
	convergence float64
}

// regimes maps regime names to their pre-characterised parameter bundles.
// Values are calibrated against the k0s idle and cp_heavy_12client convergence
// experiments (see mega-research/convergence/NOTES.md):
//
//	stable   — predictable IoT / idle edge node; slow EMA, conservative trust
//	default  — general-purpose; the daemon's baseline
//	bursty   — control-plane heavy / variable load; tracks bursts faster
//	volatile — rapid workload transitions; evidence dominates quickly
var regimes = map[string]regimePreset{
	"stable":   {alpha: 0.05, convergence: 1000},
	"default":  {alpha: 0.20, convergence: 500},
	"bursty":   {alpha: 0.30, convergence: 200},
	"volatile": {alpha: 0.50, convergence: 100},
}

// applyRegime overwrites *alpha and *convergence with the preset values for the
// named regime. It is a no-op when regime is empty (explicit flags win).
// Returns an error for unrecognised regime names so the caller can log.Fatalf.
func applyRegime(regime string, alpha, convergence *float64) error {
	if regime == "" {
		return nil
	}
	p, ok := regimes[regime]
	if !ok {
		return fmt.Errorf("unknown regime %q; valid values: stable, default, bursty, volatile", regime)
	}
	*alpha = p.alpha
	*convergence = p.convergence
	log.Printf("regime=%s → alpha=%.2f convergence=%.0f", regime, p.alpha, p.convergence)
	return nil
}

// parsePeerURLs splits the --peers comma-separated value into a clean slice.
// Empty entries (e.g. trailing commas, all-whitespace tokens) are dropped so
// the registry never sees a "" URL. Returns nil when the input is empty so
// callers can branch on len() instead of inspecting both flag and slice.
func parsePeerURLs(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// startCollectionLoop launches the autonomous tick goroutine. It is a no-op
// (with a single explanatory log line) when either the collector is nil or
// the interval is zero — both cases are treated as "operator intentionally
// disabled the loop" rather than as errors.
func startCollectionLoop(
	ctx context.Context,
	sm *semmap.SemanticMap,
	collector contracts.CollectorContract,
	interval time.Duration,
) {
	switch {
	case collector == nil:
		log.Printf("collection loop disabled: no collector for this profile/configuration")
		return
	case interval <= 0:
		log.Printf("collection loop disabled: -collect-interval=%s", interval)
		return
	}

	log.Printf("collection loop started: source=%s interval=%s", collector.SourceID(), interval)
	go runCollectionLoop(ctx, sm, collector, interval)
}

// runCollectionLoop is the body of the scheduler goroutine. Errors from the
// collector or from any individual sample are logged but never stop the loop —
// transient failures (a missing cgroup file, an unknown construct) must not
// disable the agent. Shutdown is via ctx cancellation; the function returns
// promptly once Done is closed.
func runCollectionLoop(
	ctx context.Context,
	sm *semmap.SemanticMap,
	collector contracts.CollectorContract,
	interval time.Duration,
) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("collection loop exiting: %v", ctx.Err())
			return
		case <-t.C:
			samples, err := collector.Collect()
			if err != nil {
				log.Printf("collection loop: Collect error: %v", err)
				continue
			}
			for _, s := range samples {
				if s == nil {
					continue
				}
				if err := sm.IngestSample(s); err != nil {
					log.Printf("collection loop: IngestSample error metric=%s node=%s: %v",
						s.MetricType, s.NodeID, err)
				}
			}
		}
	}
}

// buildExplainer constructs the /explain backend based on the -explain-provider
// flag. Returns a DisabledExplainer when the operator hasn't opted in, so the
// route handler can always safely call methods on the returned value.
//
// The bearer token comes from EXPLAIN_API_KEY (env) if set, otherwise from the
// -explain-api-key flag. Env-first matches the standard secret-handling
// convention: credentials don't live in shell history or systemd unit files
// unless the operator explicitly opted in via the flag.
// explainOptions bundles the -explain-* flags. A struct rather than a long
// positional list so adding a knob doesn't churn every call site.
type explainOptions struct {
	Provider      string
	BaseURL       string
	Model         string
	PromptPath    string
	PlannerPath   string
	CriticPath    string
	FlagAPIKey    string
	KeepAlive     string
	EnableSession bool
}

func buildExplainer(sm *semmap.SemanticMap, opt explainOptions) (explain.Explainer, error) {
	if opt.Provider == "" || opt.Provider == "none" {
		log.Printf("explain: provider=none (POST /explain will return 501)")
		return explain.NewDisabled(), nil
	}
	if opt.Provider != "openai-compatible" {
		return nil, fmt.Errorf("unknown -explain-provider %q (valid: none, openai-compatible)", opt.Provider)
	}
	if opt.PromptPath == "" {
		opt.PromptPath = filepath.Join("cmd", "agent", "prompts", "explain-v1.md")
	}
	prompt, err := explain.LoadPrompt(opt.PromptPath)
	if err != nil {
		return nil, fmt.Errorf("read prompt: %w", err)
	}

	// Planner and critic prompts are optional: a missing file disables that
	// stage rather than failing startup, so an operator can run answer-only
	// even with a partial prompt directory.
	promptDir := filepath.Dir(opt.PromptPath)
	plannerPrompt := loadOptionalPrompt(opt.PlannerPath, promptDir, "planner-v1.md", "planner")
	criticPrompt := loadOptionalPrompt(opt.CriticPath, promptDir, "critic-v1.md", "critic")

	apiKey := os.Getenv("EXPLAIN_API_KEY")
	if apiKey == "" {
		apiKey = opt.FlagAPIKey
	}

	var sessions *explain.SessionStore
	if opt.EnableSession {
		sessions = explain.NewSessionStore(explain.SessionConfig{})
	}

	e, err := explain.NewOpenAICompatible(explainReader{sm}, explain.OpenAICompatibleConfig{
		BaseURL:       opt.BaseURL,
		Model:         opt.Model,
		APIKey:        apiKey,
		SystemPrompt:  prompt,
		PromptFile:    opt.PromptPath,
		PlannerPrompt: plannerPrompt,
		CriticPrompt:  criticPrompt,
		KeepAlive:     opt.KeepAlive,
		Sessions:      sessions,
	})
	if err != nil {
		return nil, err
	}
	log.Printf("explain: provider=openai-compatible url=%s model=%s prompt=%s planner=%t critic=%t sessions=%t keep_alive=%s",
		opt.BaseURL, opt.Model, opt.PromptPath, plannerPrompt != "", criticPrompt != "", opt.EnableSession, opt.KeepAlive)
	return e, nil
}

// loadOptionalPrompt reads an optional stage prompt. An explicit path that
// fails to load is a hard error (the operator asked for it); a derived
// default that is absent just disables the stage with a log line.
func loadOptionalPrompt(explicitPath, dir, defaultName, label string) string {
	path := explicitPath
	derived := false
	if path == "" {
		path = filepath.Join(dir, defaultName)
		derived = true
	}
	text, err := explain.LoadPrompt(path)
	if err != nil {
		if derived {
			log.Printf("explain: %s stage disabled (no prompt at %s)", label, path)
			return ""
		}
		log.Printf("explain: WARNING %s prompt %q could not be read (%v); stage disabled", label, path, err)
		return ""
	}
	return text
}

// explainReader adapts *semmap.SemanticMap to explain.SemanticMapReader. All
// the methods already exist on SemanticMap; this shim only exists because Go
// interfaces require an explicit adapter when one type implements an interface
// declared in a package that would create an import cycle if imported directly.
type explainReader struct{ *semmap.SemanticMap }

func (r explainReader) Peers() *peers.Registry { return r.SemanticMap.Peers() }

// checkNoPositionalArgs rejects stray positional arguments after flag parsing.
//
// Go's flag package stops parsing at the first non-flag argument, so
// `-proposer false` (space instead of `=`) is read as `-proposer` followed by
// the positional "false" — and every flag after it is silently discarded. That
// failure mode produced convergence runs whose -alpha, -convergence, -priors
// and -kd were all ignored while the daemon started and served normally on
// hardcoded ontology defaults. Failing loud is cheaper than the silent version.
func checkNoPositionalArgs(n int, first string) error {
	if n == 0 {
		return nil
	}
	return fmt.Errorf("unexpected positional argument %q; boolean flags require "+
		"the form -flag=false (not -flag false), otherwise every flag after it "+
		"is silently ignored", first)
}

// orDefault reports the value actually in force, so the startup log is a record of
// the configuration rather than of the flags typed.
func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

// sweepEvery resolves the sweep cadence: the explicit interval when given, otherwise
// half the staleness window so a property is marked within one window of going quiet.
// Floored at five seconds — a sweep is a lock and a map walk, and running it more
// often than that costs more than the freshness it buys.
func sweepEvery(explicit, staleAfter time.Duration) time.Duration {
	if explicit > 0 {
		return explicit
	}
	d := staleAfter / 2
	if d < 5*time.Second {
		d = 5 * time.Second
	}
	return d
}

// startSweepLoop applies lifecycle transitions on a ticker until ctx is cancelled.
// Transitions are logged because a property going stale is a change in what the agent
// claims to know, and that should be visible in the daemon's output rather than only
// in the journal.
func startSweepLoop(ctx context.Context, state *statemap.Map, every time.Duration) {
	if state == nil || every <= 0 {
		return
	}
	log.Printf("lifecycle sweep: every %s", every)
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				stale, retired := state.Sweep()
				if len(stale) > 0 {
					log.Printf("state model: %d properties went stale: %v", len(stale), stale)
				}
				if len(retired) > 0 {
					log.Printf("state model: %d properties retired for silence: %v",
						len(retired), retired)
				}
			}
		}
	}()
}

// startSaveLoop snapshots the map on a ticker and once more when the context ends.
//
// This loop only does the periodic saves, which bound what an UNCLEAN exit costs. The
// save that makes a clean restart lossless is done synchronously by main after the
// context is cancelled, because a goroutine racing process exit loses the race often
// enough to be useless. A failed save is logged rather than fatal — an agent that
// cannot write its snapshot should keep answering questions about the system it can
// still see.
func startSaveLoop(ctx context.Context, state *statemap.Map, path string, every time.Duration) {
	if state == nil || path == "" {
		return
	}
	if every <= 0 {
		every = time.Minute
	}
	log.Printf("state model: persisting to %s every %s", path, every)
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				// The shutdown save is main's job, done synchronously so the process
				// cannot exit before it lands.
				return
			case <-t.C:
				if err := state.Save(path); err != nil {
					log.Printf("state model: periodic save failed: %v", err)
					continue
				}
				log.Printf("state model: saved at revision %d (periodic)", state.Revision())
			}
		}
	}()
}
