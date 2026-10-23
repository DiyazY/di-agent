// Package agentmain is the edge-minimal Semantic Map daemon as a library: Main
// loads the agent's spec, builds the map, the collectors and the loops it names,
// and serves the HTTP/JSON surface until interrupted. cmd/agent is Main and
// nothing else; a custom binary blank-imports a package whose init registers a
// collector with collectors.Register and then calls Main, so an out-of-tree
// collector runs in the stock daemon without a fork.
//
// The daemon loads the configured profile, seeds the graph from Di-Select priors,
// and serves the agent queries plus the graph control surface. Telemetry is
// accepted via POST /ingest-sample. The graph introspection (/graph, /edges,
// /history, /constructs, /propositions, /neighbors), ontology mutation
// (/ontology/*), candidate review (/candidates/{id}/{confirm,reject,defer}), edge
// reset (/agent/reset), and operator meta (/healthz, /version, /ui/) routes are
// wired by registerRoutes in routes.go.
//
// The profile's kd (profile.kd) selects the distribution named in profile.priors
// when set. Omit it to use the global Di-Select proposition strengths.
//
// The autonomous collection loop ticks at sources.interval, calls Collect on the
// supervisor built from sources.collectors (pkg/collectors), and runs each sample
// through the facade's IngestSample. The supervisor runs every listed collector —
// built-in types, exec plugins, http plugins — isolating each one's faults, and
// GET /collectors reports their status. With an empty list, or sources.interval 0,
// the loop is disabled and only the manual POST /ingest-sample path updates the model.
//
// profile.regime (stable|default|bursty|volatile) sets alpha and convergence to a
// pre-characterised bundle matching the deployment's dynamics. Overrides any
// explicit profile.alpha and profile.convergence values when set.
package agentmain

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

	"github.com/DiyazY/di-agent/internal/minimal"
	"github.com/DiyazY/di-agent/pkg/collectors"
	"github.com/DiyazY/di-agent/pkg/contracts"
	"github.com/DiyazY/di-agent/pkg/domain"
	"github.com/DiyazY/di-agent/pkg/explain"
	"github.com/DiyazY/di-agent/pkg/groups"
	"github.com/DiyazY/di-agent/pkg/peers"
	"github.com/DiyazY/di-agent/pkg/policy"
	"github.com/DiyazY/di-agent/pkg/profiles"
	"github.com/DiyazY/di-agent/pkg/relief"
	"github.com/DiyazY/di-agent/pkg/runtime"
	_ "github.com/DiyazY/di-agent/pkg/runtime/kubernetes"
	"github.com/DiyazY/di-agent/pkg/semmap"
	"github.com/DiyazY/di-agent/pkg/spec"
	"github.com/DiyazY/di-agent/pkg/statemap"
)

// Version is the daemon's reported semver. Returned by GET /version.
// Phase 1 ships 0.1.0 (HTTP expansion). Bump for future control-surface work.
const Version = "0.1.0"

// DefaultExplainPrompt is explain.prompt when the spec leaves it empty. It is
// relative to the working directory, which is the module root (semantic-map/go)
// when the daemon is run as dev.sh and the README run it; the planner and critic
// defaults are looked up beside it.
var DefaultExplainPrompt = filepath.Join("pkg", "agentmain", "prompts", "explain-v2.md")

// BuildCommit is the short git SHA the binary was built from. Empty when the
// binary is built without `-ldflags "-X github.com/DiyazY/di-agent/pkg/agentmain.BuildCommit=…"`.
var BuildCommit = ""

// defineFlags declares the command line: the agent is described by its file, so
// the flags only name it and ask to see it.
func defineFlags(fs *flag.FlagSet) (configPath *string, printConfig *bool) {
	configPath = fs.String("config", "",
		"path to the agent's spec, agent.yaml; empty runs the defaults. Every setting the "+
			"agent takes lives in that file")
	printConfig = fs.Bool("print-config", false,
		"print the effective spec as YAML (the file over the defaults) and exit; "+
			"`agent -print-config` is the full document to copy and edit")
	return configPath, printConfig
}

// Main runs the agent daemon: it parses the command-line flags, loads the spec and
// serves until interrupted. It is the whole of cmd/agent, kept in a library so an
// out-of-tree collector can be compiled into a custom binary: blank-import the package
// whose init registers it with collectors.Register, then call Main.
func Main() {
	configPath, printConfig := defineFlags(flag.CommandLine)
	flag.Parse()

	if err := checkNoPositionalArgs(flag.NArg(), flag.Arg(0)); err != nil {
		log.Fatalf("%v", err)
	}

	cfg, err := spec.Load(*configPath)
	if err != nil {
		log.Fatalf("%v", err)
	}
	if *printConfig {
		out, err := shownSpec(cfg)
		if err != nil {
			log.Fatalf("print-config: %v", err)
		}
		os.Stdout.Write(out)
		return
	}

	if err := applyRegime(cfg.Profile.Regime, &cfg.Profile.Alpha, &cfg.Profile.Convergence); err != nil {
		log.Fatalf("invalid profile.regime %q: %v", cfg.Profile.Regime, err)
	}

	if cfg.Node.ID == "" {
		if h, err := os.Hostname(); err == nil {
			cfg.Node.ID = h
		}
	}

	// The domain model is data, not code, and it is optional: the binary ships with
	// no constructs, propositions or routing of its own, and a node may have none to
	// give it. Without one the map still records every property the node exhibits
	// and discovers how they relate; what it cannot do is reason over constructs
	// nobody declared. A path that was named and does not load is still an error,
	// because silently running without the model someone asked for is worse.
	dom, domFrom, err := loadDomain(cfg.Domain.Path, "")
	if err != nil {
		log.Fatalf("domain spec: %v", err)
	}
	if dom != nil {
		log.Printf("domain model: %d constructs, %d propositions, %d routed metrics, from %s",
			len(dom.Constructs), len(dom.Propositions), len(dom.MetricRouting), domFrom)
	} else {
		log.Printf("domain model: none (domain.path is none, or no domain_spec.json was found): every " +
			"observed property is recorded and relationships are discovered; cost, " +
			"recommendations and tuning return 501")
	}
	switch cfg.Node.IngestScope {
	case "self":
		log.Printf("identity: %s (node-local: foreign samples rejected)", cfg.Node.ID)
	case "any":
		log.Printf("identity: %s (ingest_scope=any: this graph will be an aggregate over "+
			"every machine whose telemetry arrives, which is not a deployment topology)", cfg.Node.ID)
	default:
		log.Fatalf("invalid node.ingest_scope %q (valid: self, any)", cfg.Node.IngestScope)
	}
	pairWindowS := int(cfg.Map.PairWindow / time.Second)
	log.Printf("estimator: paired observations (window %ds, support %d, history %d)",
		pairWindowS, orDefault(cfg.Map.PairSupport, 8), orDefault(cfg.Map.PairHistory, 60))
	prop := cfg.Map.Proposer
	maxPairs := prop.MaxPairs
	if maxPairs <= 0 && prop.Enabled {
		mem, cores := machineMemory(), machineCores()
		maxPairs = minimal.DerivePairBudget(mem, cores, cfg.Sources.Interval, profiles.DefaultProposerBufSize)
		if mem == 0 {
			log.Printf("proposer: machine memory unreadable; pair budget %d (fallback)", maxPairs)
		} else {
			log.Printf("proposer: pair budget %d derived from %.1f GiB and %d cores at a %s tick",
				maxPairs, float64(mem)/(1<<30), cores, cfg.Sources.Interval)
		}
	}
	log.Printf("proposer: threshold=%.2f min-pairs=%d min-spread=%.3f fdr=%.3f max-pairs=%d",
		prop.Threshold, prop.MinPairs, prop.MinSpread, prop.FDR, maxPairs)

	peerURLs := cfg.Peers.URLs

	minSpread, fdr := prop.MinSpread, prop.FDR
	pcfg := profiles.Config{
		EMAAlpha:             cfg.Profile.Alpha,
		EMAAlphaSlow:         cfg.Profile.AlphaSlow,
		ConvergenceThreshold: cfg.Profile.Convergence,
		MinTrustScore:        cfg.Profile.MinTrust,
		PriorWeightsPath:     cfg.Profile.Priors,
		KD:                   cfg.Profile.KD,
		NodeID:               cfg.Node.ID,
		PeerURLs:             peerURLs,
		UseProposer:          prop.Enabled,
		ProposerThreshold:    prop.Threshold,
		ProposerMinPairs:     prop.MinPairs,
		ProposerMinSpread:    &minSpread,
		ProposerFDR:          &fdr,
		ProposerMaxPairs:     maxPairs,
		// Must be set explicitly: this literal does not start from
		// DefaultConfig(), so an omitted field is false, and omitting this one
		// silently wired DisabledTuner into every daemon. POST /agent/tune then
		// returned HTTP 200 with an empty applied[] — accepting the request and
		// doing nothing — which is indistinguishable from an intent that matched
		// no keyword group.
		UseRuleBasedTuner:    cfg.Map.Tuner,
		DomainSpec:           dom,
		AcceptForeignSamples: cfg.Node.IngestScope == "any",
		PairWindowSeconds:    pairWindowS,
		PairMinSupport:       cfg.Map.PairSupport,
		PairWindow:           cfg.Map.PairHistory,
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
		Owner:                   cfg.Node.ID,
		StaleAfter:              cfg.Map.StaleAfter,
		RetireAfter:             cfg.Map.RetireAfter,
		ConvergenceObservations: int(cfg.Profile.Convergence),
		Alpha:                   cfg.Profile.Alpha,
		AlphaSlow:               cfg.Profile.AlphaSlow,
		AdmitUnknown:            cfg.Map.Admit,
		Learn:                   cfg.Map.Learn,
		LearnConfig: statemap.LearnConfig{
			PairWindowSeconds: pairWindowS,
			MinSupport:        orDefault(cfg.Map.PairSupport, 8),
			Window:            orDefault(cfg.Map.PairHistory, 60),
		},
	}, statemap.NewJournal(cfg.Map.JournalSize))
	// Load before seeding. A snapshot holds what this agent learned; seeding holds what
	// the specification declares. Loading second would refuse (the map would be
	// populated), and merging blindly would silently pick a winner per property — so the
	// order is: restore, then let seeding re-declare, where the rules are explicit and
	// journalled.
	if cfg.Persist.File != "" {
		if err := statemap.CanSave(cfg.Persist.File); err != nil {
			log.Fatalf("persist.file %s cannot be written: %v. Set persist.file to a writable "+
				"path, or to \"\" to keep the map in memory only", cfg.Persist.File, err)
		}
		loaded, err := state.Load(cfg.Persist.File)
		if err != nil {
			log.Fatalf("restoring state from %s: %v", cfg.Persist.File, err)
		}
		if loaded {
			c := state.Census()
			log.Printf("state model: restored %d properties and %d relationships from %s "+
				"at revision %d", c.PropertiesTotal, c.RelationshipsTotal, cfg.Persist.File,
				state.Revision())
		} else {
			log.Printf("state model: no snapshot at %s, starting cold", cfg.Persist.File)
		}
	}

	if err := checkCanRecord(dom != nil, !cfg.Map.Admit, state.Census().PropertiesTotal); err != nil {
		log.Fatal(err)
	}

	// What the machine is, read once: the frame every property is read in. After the
	// snapshot, which restores the revision, so the facts' entries follow it.
	if err := loadFacts(state, cfg.Facts); err != nil {
		log.Fatal(err)
	}

	// Seeding happens inside profiles.Build, which holds the calibration as well as
	// the specification, so relationships get this cluster's priors rather than a
	// placeholder.

	pcfg.StateMap = state

	sm, err := profiles.Build(cfg.Profile.Name, pcfg)
	if err != nil {
		log.Fatalf("failed to build profile %q: %v", cfg.Profile.Name, err)
	}
	// Collectors are built after the map so a refused entry stops the daemon before it
	// serves anything; a broken collector found later is isolated by the supervisor
	// and reported at GET /collectors instead.
	// The pods' memory requests, which memory contention counts a pod against: stored
	// by the refresh started once the runtime is built, read by the cgroup collector.
	requests := &minimal.Requests{}
	sup, err := collectors.Build(collectors.Env{NodeID: cfg.Node.ID, Interval: cfg.Sources.Interval, Logf: log.Printf,
		Requests: requests}, cfg.Sources.Collectors)
	if err != nil {
		log.Fatalf("collectors: %v", err)
	}
	if sup != nil {
		// Close names a plugin that needed SIGTERM or SIGKILL to stop; say so.
		defer func() {
			if err := sup.Close(); err != nil {
				log.Printf("collectors: shutdown: %v", err)
			}
		}()
		for _, e := range cfg.Sources.Collectors {
			timeout := e.Timeout
			if timeout <= 0 {
				timeout = collectors.DefaultTimeout(cfg.Sources.Interval)
			}
			log.Printf("collector %s: %s %s timeout=%s", e.Name, e.Kind(), collectorSource(e), timeout)
		}
	}
	if len(peerURLs) > 0 {
		log.Printf("registered %d peers: %s", len(peerURLs), strings.Join(peerURLs, ", "))
	}

	explainer, err := buildExplainer(sm, cfg.Explain, dom)
	if err != nil {
		log.Fatalf("failed to build explainer: %v", err)
	}
	defer explainer.Close()

	// What this agent has heard from other agents, kept apart from what it observes
	// itself. This is how a cluster-level question is answered without pretending the
	// answer came from local observation.
	peerState := statemap.NewPeerStore(cfg.Node.ID, cfg.Peers.StateStale)
	peerFetcher := &peerStateFetcher{
		// The registry, not the peers.urls list: a peer added at runtime via POST /peers is
		// then polled without a restart.
		registry: sm.Peers(),
		client:   peers.NewClient(5 * time.Second),
		store:    peerState,
	}

	mux := http.NewServeMux()
	registerRoutes(mux, sm, explainer)
	registerConfigRoute(mux, cfg)
	registerCollectorsRoute(mux, sup, cfg.Sources.Interval)
	rt, err := runtime.Build(cfg.Runtime.Kind, runtime.Env{NodeID: cfg.Node.ID, Spec: cfg.Runtime, Subjects: workloadSubjects(state)})
	if err != nil {
		log.Fatalf("runtime.kind in %s: %v", *configPath, err)
	}
	pol, err := policy.New(cfg.Policy)
	if err != nil {
		log.Fatalf("policy.rules in %s: %v", *configPath, err)
	}
	for _, rule := range policy.MissingDefaultFreezes(cfg.Policy) {
		log.Printf("policy: policy.rules replaces the default rules and has none to %s; "+
			"copy it from `agent -print-config` unless leaving it out is meant", rule)
	}
	// Outcomes are evidence only when the changes really reach the system: after a
	// dry run, pressure moves with the workload's own load and nothing else.
	acting, evidence, err := buildActing(cfg, rt, log.Printf)
	if err != nil {
		log.Fatalf("actuation in %s: %v", *configPath, err)
	}
	var reliefLoops []*relief.Loop
	if cfg.Loops.Relief.Enabled {
		reliefLoops, err = buildRelief(cfg, rt, acting, pol, state, evidence, sm, log.Printf)
		if err != nil {
			log.Fatalf("%s: %v", *configPath, err)
		}
	}
	registerActingRoutes(mux, rt, acting, pol, reliefLoops)
	reviewer, err := buildReviewer(sm, cfg.Loops.Review)
	if err != nil {
		log.Fatalf("autonomy: %v", err)
	}
	registerStateRoutes(mux, state, sm.Discovery, autonomyStatus(reviewer))
	registerAutonomyRoutes(mux, reviewer)
	// CgroupRoot does not re-check the entries: collectors.Build above has already
	// refused a list whose cgroup config does not decode.
	grp := &grouper{state: state, cfg: groups.Config{LabelKeys: cfg.Groups.LabelKeys, Units: cfg.Groups.Units},
		linux:     groups.Linux{SysRoot: "/sys", ProcRoot: "/proc", CgroupRoot: collectors.CgroupRoot(cfg.Sources.Collectors)},
		workloads: rt.Workloads, logf: log.Printf}
	registerGroupsRoute(mux, grp)
	registerPeerStateRoutes(mux, state, peerState, peerFetcher)

	srv := &http.Server{Addr: cfg.Node.Addr, Handler: mux}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		log.Printf("semantic-map agent starting profile=%s addr=%s", cfg.Profile.Name, cfg.Node.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	// Start the autonomous collection loop if sources.collectors built a
	// supervisor AND the interval is positive. Both must hold — a configured
	// collector with interval=0 is a deliberately disabled loop (useful for
	// tests and for nodes that only accept manual POST /ingest-sample).
	// Lifecycle is time-based, so something has to advance it. Without this loop
	// map.stale_after and map.retire_after would only take effect when an operator posted
	// /state/sweep, and a quiet property would keep being reported as current.
	for _, l := range reliefLoops {
		dry := ""
		if !evidence {
			dry = ", as a dry run"
		}
		log.Printf("relief: %s acting every %s through runtime %s%s", l.Status().Action, cfg.Loops.Relief.Interval, rt.Name(), dry)
		go l.Start(ctx)
	}
	if startRequestsLoop(ctx, cfg.Runtime.Kind, rt, requests, log.Printf) {
		log.Printf("memory requests: listed from runtime %s every %s", rt.Name(), requestsEvery)
	}
	startSweepLoop(ctx, state, sweepEvery(cfg.Map.SweepInterval, cfg.Map.StaleAfter))
	startSaveLoop(ctx, state, cfg.Persist.File, cfg.Persist.Interval)
	startPeerStateLoop(ctx, peerFetcher, cfg.Peers.StatePoll)

	// A nil *Supervisor in the interface would not compare equal to nil, and the loop
	// would call it; no collectors is a nil collector.
	var collector contracts.CollectorContract
	if sup != nil {
		collector = sup
	}
	startCollectionLoop(ctx, sm, collector, cfg.Sources.Interval)
	if reviewer != nil {
		go reviewer.Run(ctx)
	}
	if cfg.Groups.Interval > 0 {
		go grp.run(ctx, cfg.Groups.Interval)
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutting down")
	cancel()

	// The final save happens HERE, synchronously, not in the save loop. Cancelling the
	// context and returning let the process exit while the goroutine was still racing to
	// write, so the shutdown snapshot was usually lost and a clean restart silently
	// dropped everything since the last periodic save.
	if cfg.Persist.File != "" {
		if err := state.Save(cfg.Persist.File); err != nil {
			// Exits non-zero, so a supervisor sees what the log says: what changed
			// since the last periodic save may be lost.
			log.Fatalf("state model: shutdown save failed: %v", err)
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
// named regime. It is a no-op when regime is empty (an explicit alpha and convergence lose to it).
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
		log.Printf("collection loop disabled: no collector (sources.collectors is empty)")
		return
	case interval <= 0:
		log.Printf("collection loop disabled: sources.interval=%s", interval)
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

// buildExplainer constructs the /explain backend from the spec's explain block.
// Returns a DisabledExplainer when the operator hasn't opted in, so the route
// handler can always safely call methods on the returned value.
//
// The bearer token is explain.api_key; write it as ${EXPLAIN_API_KEY} to keep the
// credential out of the file.
// withDomain fills a prompt's {{DOMAIN}} with what the loaded domain model declares, or
// says that none is loaded. A prompt names no construct itself: the constructs are data
// this agent loaded, and a model told about others reasons about a map that is not here.
// loadDomain returns the domain model and the file it came from. "none" runs without
// one; a named path must load; an empty path searches upward from searchFrom (the
// working directory when empty) and runs without one when nothing is found. The path is
// returned so the log says which file supplied the constructs: a search can find a spec
// nobody meant this agent to have.
func loadDomain(path, searchFrom string) (*domain.Spec, string, error) {
	switch path {
	case "none":
		return nil, "", nil
	case "":
		found, err := domain.FindSpec(searchFrom)
		if err != nil {
			return nil, "", nil
		}
		path = found
	}
	dom, err := domain.Load(path)
	if err != nil {
		return nil, "", err
	}
	return dom, path, nil
}

func withDomain(prompt string, dom *domain.Spec) string {
	if prompt == "" {
		return ""
	}
	return strings.ReplaceAll(prompt, "{{DOMAIN}}", dom.PromptSummary())
}

func buildExplainer(sm *semmap.SemanticMap, opt spec.Explain, dom *domain.Spec) (explain.Explainer, error) {
	if opt.Provider == "" || opt.Provider == "none" {
		log.Printf("explain: provider=none (POST /explain will return 501)")
		return explain.NewDisabled(), nil
	}
	if opt.Provider != "openai-compatible" {
		return nil, fmt.Errorf("unknown explain.provider %q (valid: none, openai-compatible)", opt.Provider)
	}
	if opt.Prompt == "" {
		opt.Prompt = DefaultExplainPrompt
	}
	prompt, err := explain.LoadPrompt(opt.Prompt)
	if err != nil {
		return nil, fmt.Errorf("read prompt: %w", err)
	}

	// Planner and critic prompts are optional: a missing file disables that
	// stage rather than failing startup, so an operator can run answer-only
	// even with a partial prompt directory.
	promptDir := filepath.Dir(opt.Prompt)
	prompt = withDomain(prompt, dom)
	plannerPrompt := withDomain(loadOptionalPrompt(opt.PlannerPrompt, promptDir, "planner-v2.md", "planner"), dom)
	criticPrompt := withDomain(loadOptionalPrompt(opt.CriticPrompt, promptDir, "critic-v2.md", "critic"), dom)

	var sessions *explain.SessionStore
	if opt.Sessions {
		sessions = explain.NewSessionStore(explain.SessionConfig{})
	}

	e, err := explain.NewOpenAICompatible(explainReader{sm}, explain.OpenAICompatibleConfig{
		BaseURL:       opt.URL,
		Model:         opt.Model,
		APIKey:        opt.APIKey,
		SystemPrompt:  prompt,
		PromptFile:    opt.Prompt,
		PlannerPrompt: plannerPrompt,
		CriticPrompt:  criticPrompt,
		KeepAlive:     opt.KeepAlive,
		Sessions:      sessions,
	})
	if err != nil {
		return nil, err
	}
	log.Printf("explain: provider=openai-compatible url=%s model=%s prompt=%s planner=%t critic=%t sessions=%t keep_alive=%s",
		opt.URL, opt.Model, opt.Prompt, plannerPrompt != "", criticPrompt != "", opt.Sessions, opt.KeepAlive)
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

// checkNoPositionalArgs rejects a stray argument after the flags. The agent
// takes only -config and -print-config; everything else lives in the spec file.
// Go's flag package stops at the first argument that is not a flag, so a path
// given without -config, or `-print-config false` written with a space, would be
// dropped along with any flag after it, and the agent would start on settings
// the operator did not give.
func checkNoPositionalArgs(n int, first string) error {
	if n == 0 {
		return nil
	}
	return fmt.Errorf("unexpected argument %q: the agent takes only -config <path> and "+
		"-print-config (a boolean, written -print-config=false, not -print-config false); "+
		"every other setting is in the spec file", first)
}

// checkCanRecord refuses a configuration that can never record anything: without a
// domain model nothing is declared, and admit: false refuses whatever is not, so unless
// a restored snapshot supplies properties every sample would be dropped while /state
// stays empty, which looks the same as telemetry that has not arrived yet.
func checkCanRecord(hasSpec, noAdmit bool, restored int) error {
	if hasSpec || !noAdmit || restored > 0 {
		return nil
	}
	return fmt.Errorf("admit: false without a domain model records nothing: no property " +
		"is declared, so every observation would be refused. Set domain.path, restore a " +
		"snapshot with persist.file, or set map.admit: true")
}

// collectorSource names what a collector entry runs, for the startup log: the
// registered type, the plugin's command, or the plugin's URL.
func collectorSource(e spec.CollectorEntry) string {
	switch {
	case e.Exec != nil:
		return e.Exec.Command
	case e.HTTP != nil:
		// A URL may carry credentials in its userinfo or query; the log gets the
		// same redacted view GET /config serves.
		return spec.RedactURL(e.HTTP.URL)
	}
	return e.Type
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
