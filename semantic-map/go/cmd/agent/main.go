// Command agent is the edge-minimal Semantic Map daemon.
//
// It loads the configured profile, seeds the graph from Di-Select priors,
// and serves the agent queries plus the graph control surface over
// HTTP/JSON on :8080. Telemetry is accepted via POST /ingest. The graph
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
// through the Bridge → Updater pipe. Setting -collect-interval=0 or
// -cgroup-root="" disables it (the manual POST /ingest path still works).
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
	"github.com/DiyazY/di-agent/pkg/explain"
	"github.com/DiyazY/di-agent/pkg/peers"
	"github.com/DiyazY/di-agent/pkg/profiles"
	"github.com/DiyazY/di-agent/pkg/semmap"
)

// Version is the daemon's reported semver. Returned by GET /version.
// Phase 1 ships 0.1.0 (HTTP expansion). Bump for future control-surface work.
const Version = "0.1.0"

// BuildCommit is the short git SHA the binary was built from. Empty when the
// binary is built without `-ldflags "-X main.BuildCommit=…"`.
var BuildCommit = ""

func main() {
	profileName     := flag.String("profile", "edge-minimal", "deployment profile")
	addr            := flag.String("addr", ":8080", "HTTP listen address")
	alpha           := flag.Float64("alpha", 0.2, "EMA decay factor (0 < alpha < 1)")
	convergence     := flag.Float64("convergence", 500, "observations for confidence=1.0")
	minTrust        := flag.Float64("min-trust", 0.5, "minimum peer trust score")
	priorsPath      := flag.String("priors", "", "path to prior_weights.json from initialization pipeline")
	kd              := flag.String("kd", "", "Kubernetes distribution running on this node "+
		"(k3s|k0s|k8s|kubeEdge|openYurt); selects per-KD edge weights from -priors when set")
	collectInterval := flag.Duration("collect-interval", 10*time.Second,
		"how often the collection loop ticks the Collector; 0 disables the loop")
	cgroupRoot      := flag.String("cgroup-root", "/sys/fs/cgroup",
		"filesystem root the cgroup collector reads from; empty string disables the loop")
	nodeID          := flag.String("node-id", "",
		"identifier this agent uses in MetricSamples; empty falls back to os.Hostname()")
	netdataURL      := flag.String("netdata-url", "",
		"base URL of Netdata daemon to poll (e.g. http://localhost:19999). Empty disables Netdata collection.")
	peersFlag       := flag.String("peers", "",
		"comma-separated peer agent URLs to register at startup "+
			"(e.g. http://node_1:8080,http://node_2:8080). RecommendPeer ranks "+
			"these by trust-weighted savings. Additional peers can be added at "+
			"runtime via POST /peers.")
	regime          := flag.String("regime", "",
		"dynamics preset (stable|default|bursty|volatile); overrides -alpha and -convergence when set")
	var useProposer bool
	flag.BoolVar(&useProposer, "proposer", true, "enable MI correlation proposer (disable for low-CPU devices)")
	var useTuner bool
	flag.BoolVar(&useTuner, "tuner", true,
		"enable the RuleBasedTuner behind POST /agent/tune (disable to wire DisabledTuner, "+
			"which accepts requests and applies nothing)")

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

	peerURLs := parsePeerURLs(*peersFlag)

	cfg := profiles.Config{
		EMAAlpha:             *alpha,
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
	}

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

	mux := http.NewServeMux()
	registerRoutes(mux, sm, explainer)

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
	startCollectionLoop(ctx, sm, collector, *collectInterval)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutting down")
	cancel()
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
