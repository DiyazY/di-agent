# Semantic Map

An adaptive behavioral data structure for autonomous edge orchestration agents.

The Semantic Map is the "brain" of an edge agent: a live model of one machine's state — the properties it exhibits, and how they relate. Its *structure* comes from the [Di-Select framework](../di-select/) and five empirical publications: which properties relate, and in which direction, is knowledge one machine's telemetry cannot produce. Every *number* comes from the machine itself. A freshly started agent reports that it does not know what any relationship is worth, and learns on two timescales — what is happening now, and what is normal here — without any labeling or manual tuning.

For design rationale, contract decisions, language strategy, and research connection see **[ARCHITECTURE.md](ARCHITECTURE.md)**.

> **Research context:** Core artifact for Publication P6 — *Semantic Map & Context-Aware Agent Architecture* — part of the dissertation *A Context-Aware Agentic Framework for Decentralized Edge Computing* (Tampere University, Diyaz Yakubov).

---

> **Deploying this?** Read [`OPERATING.md`](../OPERATING.md) first — security posture, footprint, systemd, monitoring.
>
> **Changing code?** Read [`DEVELOPING.md`](../DEVELOPING.md) first — install, the `dev.sh` inner loop, extension recipes, testing, and conventions. This file is the operational reference; that one is the developer guide.

## Table of Contents

- [Quick Start](#quick-start)
- [1. Project Structure](#1-project-structure)
- [2. The Agent API](#2-the-agent-api)
- [3. Running the Edge Daemon](#3-running-the-edge-daemon)
- [4. Compliance Tests](#4-compliance-tests)
- [5. Structural Initialization](#5-structural-initialization)
- [6. Coordination](#6-coordination)
- [7. PoC — Live Multi-VM Demo](#7-poc--live-multi-vm-demo)
- [8. Natural-Language Explain (`/explain`)](#8-natural-language-explain-explain)
- [9. Autonomous Candidate Review (`loops.review.level`)](#9-autonomous-candidate-review--autonomy)

---

## Quick Start

Five commands to see the live ontology end-to-end. Requires Go 1.22+. See [ARCHITECTURE.md §9](ARCHITECTURE.md#9-control-surface) for the design.

```bash
# 1. Start the daemon (edge-minimal profile, in-memory).
# domain.path loads the domain model; without it (and with no domain_spec.json found
# above the working directory) the agent runs with none. persist.file "" keeps the
# map in memory: the agent refuses to start where it cannot write the default,
# /var/lib/di-agent, which usually needs root. See §3.
cd semantic-map/go
printf 'domain:\n  path: ../domain_spec.json\npersist:\n  file: ""\n' > /tmp/agent.yaml
go run ./cmd/agent -config /tmp/agent.yaml &
```

```bash
# 2. Read the backbone over HTTP
curl -s localhost:8080/graph | jq '{constructs:(.constructs|length),
                                    propositions:(.propositions|length),
                                    edges:(.edges|length)}'
# → {"constructs":2,"propositions":2,"edges":2}   with the committed domain_spec.json

curl -s 'localhost:8080/edges?from=RC&to=PS' | jq '.[] | {proposition_id, basis, effective}'
# → one edge, basis "unknown" and no effective value until telemetry arrives
```

```bash
# 3. Drive the agent from the terminal
go run ./cmd/mapctl graph                 # table view of the snapshot
go run ./cmd/mapctl edges --from RC --to PS
go run ./cmd/mapctl deprecate P1 "smoke test"   # soft-delete a proposition
go run ./cmd/mapctl history --since 1h    # audit log entries
go run ./cmd/mapctl reset RC PS           # discard evidence → back to unknown
go run ./cmd/mapctl tune "prioritize performance" # natural-language strength assertion
go run ./cmd/mapctl estimate cpu_pressure_ratio \
  --assume cpu_utilization@pod:<uid>=0.6 --without pod:<uid>   # answer, optionally under hypotheses
# <uid> is a pod's UID as it appears in GET /state — properties are named <metric_type>@<subject>
```

```bash
# 4. Open the operator console (no external assets — works offline)
open http://localhost:8080/ui/            # macOS; xdg-open on Linux
# → click a property → value, confidence, influences, and "what if" estimates
# → click a relationship → the three strength layers, and which one answers
# → the drawer carries the journal, traced decisions and the discovery queue
```

```bash
# 5. Tear down
kill %1
```

Three surfaces, one daemon: `curl` for inspection, `mapctl` for scripts and headless ops, the browser at `/ui/` for watching the map move and asking it questions. When `loops.relief.enabled` is on, the console opens on an **Actions** pane (a summary strip of actions, kept, reverted, predictions held, frozen workloads and refused, then each action's workload, before → after, in the action's unit, predicted against observed drop and outcome; a row opens the act and verify decisions behind it and highlights the capped workload and node pressure on the map; a change whose undo the actuator keeps refusing shows `undo failing: <error>` under its outcome and an *undo failing* tile, and a refresh that fails after an earlier one landed says the pane is not current), a **Resources** pane lists the machine (cores, memory, disk) and every consumer the map models — pods, and the units a cgroup collector's `config.units` names, such as the agent and its decision model — with its CPU in cores and memory in bytes, and the rest of the system no subject accounts for, computed when shown; pod boxes carry the workload's name from `GET /workloads` and a `[throttled]` badge (`[throttled (dry-run)]` on the dry-run runtime, which changes nothing) or `[frozen]`, and with relief off the pane says so, asking `/relief` again every tenth poll so an agent restarted with relief on is noticed without a reload. All three speak the same JSON HTTP API.

---

## 1. Project Structure

```
semantic-map/
│
├── domain_spec.json            THE DOMAIN MODEL — constructs, propositions, metric routing,
│                               adjustment policy, tuner intent rules. The daemon loads this
│                               at startup (domain.path) and runs without a domain model when
│                               there is none; no construct or proposition identifier
│                               exists in any binary.
│
│  ── Python layer (the calibration pipeline — a build-time step) ──
│
├── prior_weights.json          Init-pipeline output: which propositions the agent carries, which
│                               it declines, and where it overrides a declared direction. No strengths.
├── requirements.txt            scipy (spearmanr); the pipeline's only outside dependency
├── agent.example.yaml          The agent's spec at its defaults — `agent -print-config`'s output, to copy and edit
│
├── prior_init/                 Structural initialization pipeline (Step 4)
│   ├── pipeline.py             Entry point — reads P1–P5 constants, writes prior_weights.json
│   ├── calibration.py          Construct scoring + per-proposition diagnostics (no strengths)
│   ├── constants.py            Publication constants (J/pod, mJ/op, CIS scores, …)
│   └── loaders.py              CSV / result file loaders
│
│  ── Collector plugins (out-of-process, any language) ───────────
│
├── sdk/python/di_collector.py  Python plugin SDK, stdlib only, one file: serve(setup) speaks wire
│                               protocol v1 on stdin/stdout so a plugin only says what to measure
├── sdk/python/test_di_collector.py  Its unit tests (stdlib unittest; CI runs them)
├── examples/collectors/
│   ├── loadavg/                Example exec plugin on the Python SDK: load1/5/15 per CPU and
│   │   ├── loadavg.py          PSI some avg10 for cpu, memory, io; certified in CI by
│   │   └── README.md           `mapctl collector check` (see §3)
│   └── shell/load1.sh          The shell template from docs/collectors.md §11; certified in CI
│
│  ── Go layer (edge daemon) ──────────────────────────────────────
│
├── deploy/kubernetes/rbac.yaml ServiceAccount, ClusterRole and ClusterRoleBinding the `kubernetes` runtime needs (see §3)
├── deploy/kubernetes/daemonset.yaml One agent per node: its agent.yaml, POD_UID, and the map kept on the node in /var/lib/di-agent (see §3)
│
├── scenarios/                  Scenario files for SystemScript: a synthetic system
│   │                           with known ground truth (subjects on a schedule + a
│   │                           coupled node model), loaded by
│   │                           internal/minimal/tests/scenario_files_test.go
│   ├── linear.json             One subject, sum coupling — counterfactual estimate
│   │                           against a linear ground truth
│   ├── saturation.json         Logistic coupling — counterfactual against a
│   │                           saturating (nonlinear) ground truth
│   ├── churn.json              Subjects arrive, depart and return — admission,
│   │                           staleness and retirement timing
│   ├── confounded.json         One subject drives a node property; a second
│   │                           co-varies with it without driving anything —
│   │                           both are proposed, and the map cannot tell
│   │                           which is the cause
│   └── decoupled.json          A subject the node model never reads — asserts no
│                               discovery candidate is proposed from it
│
└── go/
    ├── go.mod                  Module: github.com/DiyazY/di-agent
    │
    ├── pkg/                    Public packages — importable by agent code
    │   ├── types/types.go      Go equivalents of all Python types
    │   ├── contracts/          The contract surface — five interfaces
    │   │   └── contracts.go    Collector, Ontology, Reasoner, Proposer, Tuner + sentinel errors
    │   ├── stats/              Shared estimator primitives (concrete, NOT a contract)
    │   │   ├── stats.go        PairWindow (dedup'd ring buffer) + Pearson + FisherPValue —
    │   │   │                   the one estimator statemap and the MI proposer both use
    │   │   └── stats_test.go   Ring dedup, Pearson, Fisher p-value coverage
    │   ├── ingest/client/      Wire face of the sample boundary for applications
    │   │   ├── client.go       Client.Push — a workload POSTs its own metrics under its own
    │   │   │                   subject, same MetricSample an in-process collector produces
    │   │   └── client_test.go  Push wire-format + EventID determinism coverage
    │   ├── collectors/         Telemetry sources from sources.collectors (concrete, NOT a contract)
    │   │   ├── registry.go     Register(type, Factory) + Build(env, entries) → Supervisor
    │   │   ├── builtin.go      The built-in types: cgroup, netdata, script, each decoding its own config
    │   │   ├── supervisor.go   Supervisor — every member each tick: timeout, fault isolation, backoff,
    │   │   │                   contract enforcement, one owner per node-level type, Status()
    │   │   ├── wire.go         Wire protocol v1 (hello, collect; strict sample decoding) shared by both transports
    │   │   ├── exec.go         ExecCollector — a plugin process speaking the protocol on stdin/stdout
    │   │   ├── http.go         HTTPCollector — a plugin reached over HTTP
    │   │   └── plugin/         plugin.Serve — the exec loop for a plugin written in Go (stdlib + pkg/types only)
    │   ├── peers/              Multi-agent coordination (concrete in v1, NOT a contract)
    │   │   ├── peers.go        Registry + Descriptor + Client (HTTP /cost, /healthz, /offload)
    │   │   └── peers_test.go   Registry + httptest client coverage
    │   ├── spec/               The agent's one configuration document, agent.yaml (concrete, NOT a contract)
    │   │   ├── spec.go         Spec types + Default(), Parse (defaults overlaid, ${VAR} expanded, unknown keys refused), Load, Validate, Redacted, YAML
    │   │   └── spec_test.go    Defaults, overlay, validation, redaction, round trip
    │   ├── policy/             What the agent may touch: rules over workloads, freeze always wins (concrete, NOT a contract)
    │   ├── runtime/            Runtime contract: the node's workloads and their resources (inventory only); registry + `dry-run` implementation
    │   │   └── kubernetes/     The `kubernetes` runtime: this node's pods; the `cpu.limit` actuator, a pod's CPU limit changed in place through the pod `resize` subresource
    │   ├── actuators/          Actuator contract: one action's Descriptor, Check, Read, Set; registry, the ledger, Acting, DryRun and Static
    │   │   ├── actuators.go    Descriptor + Actuator + Env + Register/Build/Names + ErrGone, ErrInvalidIdentity
    │   │   ├── ledger.go       Entry + Ledger: every change the agent holds and what it replaced; OpenLedger (atomic file) / MemoryLedger
    │   │   ├── acting.go       Acting: Apply (records the original first), Revert, Forget, Held (reconcile against the workloads listed), Unbuilt
    │   │   ├── dryrun.go       DryRun: wraps an actuator so Set changes nothing outside the process
    │   │   ├── memoryhigh.go   The `memory.high` actuator: a pod's cgroup v2 memory.high, a soft cap the kernel enforces by reclaim; Build refuses no cgroup v2, the kubelet managing memory.high (MemoryQoS), or a tree it cannot write
    │   │   ├── writable_*.go   Whether a directory may be written (access W_OK on unix; nothing to ask elsewhere)
    │   │   └── static.go       Static: an actuator over in-memory values, the stand-in where the runtime is dry-run
    │   ├── facts/              Reading the machine's facts (concrete, NOT a contract)
    │   │   ├── facts.go        Source iface + registry; Load() into the map; the host keys
    │   │   ├── host.go         host: /proc, /sys, /etc/os-release under an optional root
    │   │   └── statfs_*.go     Disk size by statfs, where the platform has it
    │   ├── autonomy/           Autonomous candidate review (concrete, NOT a contract)
    │   │   ├── autonomy.go     Levels, thresholds, the review loop, the tiers, decision records
    │   │   ├── registry.go     Named parts: backends, rules, tools, renderers; Build() from names
    │   │   ├── rules.go        Tier 0: Rule, and the composition rule
    │   │   ├── decider.go      Tier 1 systemone: Ollama decision models over /v1/systemone
    │   │   ├── logprobs.go     Tier 1 openai-logprobs: a general model's letter probabilities
    │   │   ├── reasoner.go     Tier 2 openai-tools: a tool-calling model reading the map
    │   │   ├── tools.go        Tool, and the read-only tools over the map
    │   │   ├── openai.go       The OpenAI-compatible chat client both share
    │   │   └── evidence.go     Renderer, and the candidate's evidence as text
    │   ├── explain/            Natural-language operator surface (concrete, NOT a contract)
    │   │   ├── explainer.go    Explainer iface + Request/Response/Citation/Plan/Verdict/Usage
    │   │   ├── disabled.go     DisabledExplainer — the default; returns ErrNotEnabled
    │   │   ├── tools.go        8 READ-ONLY tools over the facade + Dispatch()
    │   │   ├── validator.go    Deterministic citation validator (Gate 1)
    │   │   ├── planner.go      Plan parse / structural validate / deterministic execute
    │   │   ├── critic.go       Critic verdict parse + prompt assembly (Gate 2)
    │   │   ├── session.go      LRU session store + tool-result cache w/ TTL + invalidation
    │   │   ├── streaming.go    NDJSON Event vocabulary + StreamingExplainer iface
    │   │   └── openai.go       OpenAI-compatible client; planner→answer→critic loop
    │   ├── semmap/
    │   │   ├── map.go          SemanticMap Go facade (includes peer registry + client)
    │   │   └── projection.go   Renders the state model onto the surfaces that predate it:
    │   │                       relationships as EdgeDescriptors (/graph, /edges,
    │   │                       /neighbors, viewer, mapctl), strengths and retirement
    │   │                       onto /propositions, the journal onto /history
    │   ├── statemap/           The state model — what THIS system exhibits (concrete, NOT a contract)
    │   │   ├── property.go     Property + Relationship + Map: kinds, lifecycle, observe, retire
    │   │   ├── learn.go        Paired estimator — relationships learn their own strength
    │   │   ├── query.go        State(Query) + Explain(id) — census-carrying views
    │   │   ├── journal.go      Bounded change log + DecisionBuilder (records inputs as read)
    │   │   ├── facts.go        Fact — what the machine is (OS, CPU, memory, disk), journalled on change
    │   │   ├── persist.go      Versioned, owner-stamped snapshots (atomic write, refuses foreign)
    │   │   └── peer.go         PeerStore — other agents' maps, labelled and never merged
    │   └── profiles/           Profile factory
    │       ├── profiles.go     Build("edge-minimal", cfg) + ontology seeding + peer wire-up (no collectors)
    │       └── state_seed.go   SeedStateMap — spec metrics/constructs → properties; structure only
    │
    ├── internal/               Implementation packages — not importable externally
    │   ├── scripted/           Synthetic collectors for demos, scenarios and tests
    │   ├── testhooks/          Hooks tests set to make production code fail where nothing else can (a ledger save); nil outside a test
    │   │   ├── collector.go    ScriptedCollector — programmable patterns (Constant /
    │   │   │                   Ramp / Step / Sine / Burst / Noisy)
    │   │   ├── collector_test.go
    │   │   ├── scenario.go     Scenario — a synthetic system with known ground
    │   │   │                   truth loaded from a JSON file in scenarios/
    │   │   ├── scenario_test.go
    │   │   ├── system.go       SystemScript — CollectorContract over a Scenario:
    │   │   │                   subjects on a schedule + a coupled node model
    │   │   └── system_test.go
    │   └── minimal/            edge-minimal profile implementations
    │       ├── collector_cgroup.go   CgroupCollector   (cgroups v2, no daemon)
    │       ├── cgroup_recognise.go   Subject recognisers: pod cgroups (systemd + cgroupfs
    │       │                         drivers) and allowlisted system.slice units
    │       ├── collector_netdata.go  NetdataCollector  (Netdata HTTP API v1, include patterns
    │       │                         over chart contexts)
    │       ├── ontology.go     SpecOntology           (the declaration layer: constructs and
    │       │                                           propositions from domain_spec.json. No
    │       │                                           strengths, no history — both live on the
    │       │                                           state model)
    │       ├── reasoner.go     RuleEngineReasoner     (deterministic, blended, skips retired)
    │       ├── proposer.go     DisabledProposer       (no-op)
    │       ├── proposer_mi.go  MICorrelationProposer  (Pearson r + Fisher z p-values; default via map.proposer.enabled)
    │       ├── budget.go       DerivePairBudget       (the pair budget from memory, CPUs and the tick)
    │       ├── tuner.go        RuleBasedTuner + DisabledTuner (intent rules read from the spec;
    │       │                                           default via map.tuner: true)
    │       └── tests/
    │           ├── compliance_test.go   Runs all Go compliance suites
    │           └── scenarios_test.go    End-to-end narrated scenarios (ColdStart, Convergence,
    │                                    PerKDDecisionsDiffer, DeprecationShrinksGraph,
    │                                    IdempotentReplay, AuditTrailRecordsEverything,
    │                                    CoordinationOffload — 3-agent multi-agent demo,
    │                                    ProposerNaturalDiscovery, OperatorTuneAndAuditTrail)
    │
    ├── compliance/             Go compliance test suites — one per contract
    │   ├── collector.go        RunCollectorCompliance(t, factory); RunCollectorChecks(reporter, factory)
    │   ├── reporter.go         Reporter interface, *testing.T adapter, Recorder for non-test callers
    │   ├── ontology.go         RunOntologyCompliance(t, factory)  — vocabulary + validation
    │   ├── reasoner.go         RunReasonerCompliance(t, factory)
    │   ├── proposer.go         RunProposerCompliance(t, factory)
    │   └── tuner.go            RunTunerCompliance(t, factory)
    │
    ├── pkg/profiles/
    │   ├── build_priors_test.go  Asserts structure arrives and no magnitude does, through Build
    │   └── profiles_test.go      KD validation + prior_weights.json discovery
    │
    ├── cmd/agent/main.go       Daemon binary: calls agentmain.Main()
    │
    ├── pkg/agentmain/          The daemon's body, public so a custom binary can register extra collectors
    │   ├── example_custom_test.go  Example_customCollector: blank-import a collector, call Main()
    │   ├── main.go             Main(): -config / -print-config + spec load + profile build + ListenAndServe
    │   ├── config_route.go     GET /config: the running spec, secrets redacted
    │   ├── collectors_route.go GET /collectors: each configured collector's status
    │   ├── acting.go           /workloads, /policy, /relief*: what the runtime lists, the policy that governs each, and the relief loops with their kill switch; buildActing (each lever's actuator + ledger), buildRelief (one loop per lever, one shared Budget)
    │   ├── actuators_route.go  GET /actuators: each action's descriptor and the changes the ledger holds
    │   ├── requests.go         The pods' memory requests, listed from the runtime every 30s for the cgroup collector's memory contention
    │   ├── routes.go           registerRoutes + writeError + requireJSON (CSRF guard)
    │   ├── state_routes.go     /state* — query, lifecycle, journal, decisions, estimate
    │   ├── peer_state_routes.go  /state/cluster, /state/peers, /state/where + the peer poll loop
    │   ├── dto.go              Named JSON DTOs (Direction serialized as "+"/"-")
    │   ├── machine.go          The memory and CPUs the agent may use (host, or its cgroup limit)
    │   ├── static.go           //go:embed all:static + staticHandler()
    │   ├── routes_test.go      HTTP integration tests via httptest.NewServer
    │   ├── explain_route_test.go  POST /explain: 501/400/200 + NDJSON streaming
    │   ├── prompts/            Versioned LLM system prompts (loaded at startup)
    │   │   ├── explain-v2.md   Answering agent — graph semantics, tools, response schema
    │   │   ├── planner-v2.md   Planner agent — planning rules + worked examples
    │   │   └── critic-v2.md    Critic agent — what Gate 1 already covers, what only it can catch
    │   └── static/             Embedded operator console — no external assets
    │       ├── index.html      Shell: header, census strip, canvas, inspector, drawer
    │       ├── style.css       Design tokens (sign / status / provenance / ink) + layout
    │       └── js/             ES modules, served from the binary
    │           ├── app.js      Poll loop, census strip, theme, toasts, prompt dialog
    │           ├── api.js      One place that knows the routes and the error shape
    │           ├── model.js    Effective-strength precedence, value history, subjects
    │           ├── force.js    Force-directed layout (hand-rolled; no CDN)
    │           ├── graph.js    SVG canvas: properties, relationships, subject enclosures
    │           ├── charts.js   Sparkline, strength layers, diverging contributions
    │           ├── inspector.js  Property / relationship detail + counterfactual studio
    │           └── drawer.js   Journal, decisions, discovery, subjects, cluster,
    │                           backbone, ask
    │
    ├── cmd/mapctl/             CLI binary — cobra + tablewriter; speaks the daemon's HTTP API
    │   ├── main.go             cmd.Execute()
    │   ├── cmd/                One file per subcommand (graph, edges, history, strength,
    │   │                       deprecate, construct, proposition, reset, candidates,
    │   │                       recommend, simulate, watch, dot, health, peers,
    │   │                       version, tune, completion, collector)
    │   ├── client/             HTTP client + DTOs duplicated (NOT imported) from pkg/agentmain
    │   │   ├── client.go
    │   │   ├── types.go
    │   │   └── client_test.go
    │   └── render/             Output formatters
    │       ├── table.go        tablewriter wrapper honoring --no-color
    │       └── json.go         render.JSON(w, v) for --json mode
    │
    └── cmd/replay/             Parquet replay binary — drives the 225 Netdata
        │                       parquets (P1–P5 dataset) into POST /ingest-sample
        ├── main.go             run / all / probe / list subcommands
        ├── parquet/            Streaming long-format reader over parquet-go v0.25.1
        │   ├── reader.go       Open + Next + Close; 4096-row batched buffer
        │   └── reader_test.go  Synthesized fixture parquets in t.TempDir()
        ├── mapping/            chart_context+metric_id+units → MetricType + normalizer
        │   ├── mapping.go      v1 table (cpu/ram/net) — cross-KD; documented at top
        │   └── mapping_test.go Table-driven (+ negative cases)
        ├── playback/           Tick-grouped replay loop with time-warp speed control
        │   ├── runner.go       Run(ctx, sender, cfg) + deterministic EventID()
        │   └── runner_test.go  httptest.Server-backed Sender; covers EventID
        │                       determinism across two replays (idempotency proof)
        │   ├── runner.go       Build a SemanticMap per KD, stream parquet rows,
        │   │                   snapshot edges. Skips HTTP — driven on pkg/semmap
        │   │                   directly. See top-of-file comment for rationale.
        │   ├── divergence.go   effective (assertion|measured|established|recent); Range,
        │   │                   sample StdDev, sorted by Range desc (most
        │   │                   discriminative first)
        │   ├── output.go       Table / JSON / CSV formatters
        │   ├── runner_test.go  Two synthesized "KDs" diverge as expected; 5-run
        │   │                   averaging differs from single-run snapshot
        │   └── divergence_test.go  Range/StdDev arithmetic, sort order, formula
        └── client/             POST /ingest-sample wrapper + DTOs duplicated
            └── client.go        from pkg/agentmain (same wire-boundary discipline as mapctl)
```

For the architectural rationale behind the multigraph, live ontology, control surface, and per-layer language strategy, see [ARCHITECTURE.md](ARCHITECTURE.md).


> **The Python contract mirror is gone.** `contracts/`, `compliance/`, `map.py`,
> `profiles.py` and `types.py` mirrored the contract surface as ABCs, on the theory that
> the Python definitions were the specification and Go implemented them. Nothing was ever
> built behind the Python side — `cloud-full` does not exist — so the mirror was a second
> definition with no implementation to keep it honest, and it drifted: it still declared
> Storage and Updater after both were deleted from Go, and its Ontology carried a strength
> and an audit log the Go interface had stopped holding. What remains of the Python layer
> is `prior_init/`, which produces `prior_weights.json` and is the one part that was doing
> real work. See ARCHITECTURE.md §4.

---

## 2. The Agent API

Three stable queries across all profiles:

### `GET /cost?task=<type>&node=<id>`

Estimates the cost of executing a task on a given node.

```json
{
  "cpu_cost": 0.12,
  "energy_cost": 0.034,
  "latency_estimate": 7.4,
  "confidence": 0.62,
  "rationale": "task=pod-scheduling node=node_1 path=[SC→RC(0.58), RC→PS(0.41)]",
  "graph_path_used": ["SC→RC(0.58)", "RC→PS(0.41)"]
}
```

### `POST /recommend`

Finds the best peer for task offloading. Returns `InsufficientTrustError` if no peer meets the minimum trust threshold.

```json
// request
{"task_type":"pod-scheduling","source_node_id":"node_1","data_size_bytes":2048,"latency_budget_ms":500}

// response
{"peer_id":"node_2","expected_savings":0.018,"rationale":"...","graph_path_used":["..."]}
```

### `POST /simulate`

Pre-flight simulation before committing an offload. Read-only — never modifies state.

```json
// request
{"context":{...},"target_node_id":"node_2"}

// response
{
  "expected_latency": 8.1,
  "expected_energy": 0.029,
  "confidence": 0.55,
  "p95_latency": 12.4,
  "p95_energy": null,
  "risk_flags": [],
  "graph_path_used": ["..."]
}
```

`p95_*` is `null` on `edge-minimal` — requires Gaussian descriptors (`edge-standard` upward).

### `POST /ingest-sample`

Feed one typed `MetricSample` to the facade's `IngestSample`. The daemon records
the observation as a property, recomputes whatever derives from it, and folds it
into every relationship incident to that property — the same path an in-process
collector uses.

```json
{"node_id":"master","metric_type":"cpu_utilization","value":0.71,
 "timestamp_unix":1703208286,"event_id":"replay:idle_run1:master:system.cpu:idle:0"}
```

Five more fields are optional and let a producer describe what it is reporting:

| Field     | Type          | Meaning                                                                 |
| --------- | ------------- | ------------------------------------------------------------------------ |
| `subject` | `str`         | Empty for a node-level reading; `<kind>:<identity>` (e.g. `pod:1234`) to scope it to something other than the node itself |
| `unit`    | `str`         | The unit the value is expressed in                                       |
| `range`   | `[lo, hi]`    | The value's declared bounds                                              |
| `source`  | `str`         | Free-form identifier for the producer, distinct from `node_id`           |
| `chart`   | `str`         | The series the producer computes together from one reading, as parts of one whole (a stacked Netdata chart, one `/proc/stat` line) — not series that are merely drawn together, such as received and sent. Series of one chart and one subject whose pair reaches \|r\| ≥ 0.99, and holds within its sampling error of it, are an arithmetic group: discovery reports them once instead of proposing them |
| `part_of` | `str`         | On a sample with a subject: the node-level metric type the subject's quantity is summed into (a physical disk's `disk.io.writes` into `system.io.out`). A candidate from it to that type is marked `composition`, as one against the node metric of the same name already is. Send it on every sample it applies to: a sample without it withdraws the declaration. `400` without a subject or when it is not a metric type |

A malformed `subject` (not `<kind>:<identity>` over `[A-Za-z0-9._:-]`) returns `400`.
`metric_type` need not be one of the values the domain specification routes: an
unrouted type, or any sample carrying a non-empty `subject`, is still recorded as a
property and answered `202`, not `400`:

```json
{"recorded": true, "routed": false, "metric_type": "queue_depth", "subject": "pod:1234",
 "note": "scoped readings are not routed: recorded as a property of the subject, not summarised by any construct"}
```

A routed, unscoped sample is acknowledged with `204 No Content`. This is the
public-API entry point for out-of-tree collectors — the parquet replay tool in
particular speaks only this endpoint.

### Pushing application metrics

A workload on the node can be an instrument of its own state. It pushes samples to
the agent under its own subject with a declared unit and range; the map admits them
beside the resource properties the cgroup collector observes for the same subject.

```go
c := client.New("http://127.0.0.1:8080", os.Getenv("NODE_NAME"), "pod:"+os.Getenv("POD_UID"), "app:transcoder")
ack, err := c.Push(ctx, client.Metric{Type: "queue_depth", Unit: "items", Range: [2]float64{0, 100}}, float64(len(queue)), time.Now(), nil)
```

`Push` refuses a malformed subject or metric type, an empty unit or an empty range before
sending, naming the field. The `Ack` says whether a construct summarises the reading
(`Routed`, the 204 case) or it was recorded as a property nothing summarises (202, with
the agent's `Note`) — for a scoped subject always the latter, and for a node-level push
the way a mistyped metric type shows up.

`NODE_NAME` and `POD_UID` come from the downward API (`spec.nodeName`, `metadata.uid`). In Python:

```python
import hashlib, json, time, urllib.request
def push(base, node, subject, source, metric, unit, rng, value):
    at = int(time.time())
    eid = hashlib.sha256(f"{source}|{node}|{subject}|{metric}|{at}".encode()).hexdigest()[:16]
    body = json.dumps({"node_id": node, "metric_type": metric, "value": value, "timestamp_unix": at,
                       "event_id": eid, "subject": subject, "unit": unit, "range": rng, "source": source}).encode()
    req = urllib.request.Request(base + "/ingest-sample", body, {"Content-Type": "application/json"})
    urllib.request.urlopen(req, timeout=5).read()
```

### `GET /collectors`

Each collector in `sources.collectors`, in list order, with what it has done so far.
Read-only: collectors are configured in the spec, not over HTTP.

```json
{"interval_ms":10000,
 "collectors":[
  {"name":"cgroup","kind":"type","type":"cgroup","state":"ok",
   "last_success_unix":1761160000,"last_error":"","last_error_unix":0,
   "samples":51234,"violations":0,"ownership_drops":0,"restarts":0,
   "backoff_until_unix":0,"available":["cpu_utilization","..."]}]}
```

`kind` is `type` (a built-in), `exec` or `http`; `type` is the built-in's name and empty
otherwise. `state` is `starting` until the first successful collect, `ok` after one, and
`backoff` after an error, panic or timeout, until `backoff_until_unix` (1 s doubling to
5 min, reset by a success), and `stuck` while a call that outlived its timeout has not
returned — the member is skipped until it does, and its return is then recorded as a
fault at that moment (`backoff`, a fresh `backoff_until_unix`, `last_error` saying how it
ended). `interval_ms` is `sources.interval` in milliseconds. `violations` counts samples dropped for breaking the collector contract (a
malformed or duplicate sample in a plugin's reply, an invalid metric type, subject or
`part_of`, no unit, a foreign `node_id`, a unit or range that changed, an invalid
`available` entry); `ownership_drops` counts node-level samples dropped because an earlier
collector in the list owns that type; `restarts` counts an exec plugin's process restarts,
after a fault or after the plugin exited on its own between ticks.
With no collectors configured the list is `[]`.

### `GET /candidates`

Lists Proposer candidate edges pending review. `?status=all` lists every candidate the
proposer has emitted, first-seen first, each with its current `Status` and, when deferred,
its `Reason`; `?status=confirmed`, `rejected` or `deferred` lists one status of them
(`mapctl candidates --status all`). Any other value is `400`. Without the history, a
candidate an operator rejected and one never proposed look the same.

```json
[{"CandidateID":"cpu_utilization@pod:a->cpu_pressure_ratio","FromID":"cpu_utilization@pod:a","ToID":"cpu_pressure_ratio",
  "Direction":0,"MIScore":0.91,"PValue":0.002,"PBar":0.011,"NObservations":40,"DeploymentsSeen":0,"Status":"pending",
  "Undirected":false,"Kind":"","Caveats":null},
 {"CandidateID":"cpu_pressure_ratio<->cpu_utilization","FromID":"cpu_pressure_ratio","ToID":"cpu_utilization",
  "Direction":0,"MIScore":0.88,"PValue":0.004,"PBar":0.011,"NObservations":40,"DeploymentsSeen":0,"Status":"pending",
  "Undirected":true,"Kind":"","Caveats":null}]
```

`Status` is a name — `pending`, `confirmed`, `rejected` or `deferred` — and a deferred
candidate carries `Reason`: `grouped` when an arithmetic group took it over (its pair links
the group, or an end joined a group it does not represent); `bar` when its p-value no
longer clears the corrected bar, because more pairs came to be tested beside it; `weakened`
when its `|r|` fell below the threshold by more than one standard error of Fisher's z
(about 0.82 for the 0.85 default over a full window), so the evidence it was emitted on no
longer holds; a pending candidate inside that margin stays pending and shows its current `|r|`;
`untested` when its pair was displaced from the pair budget, so
its numbers stopped being updated (it re-enters if the pair is tested again and still clears
the threshold); `cap` when the proposer's pending cap deferred it (provisional;
it re-enters when it outranks a pending candidate), `operator` when `/defer` did (it
stands) or `agent` when the agent deferred it (it returns to pending once its evidence
moves). A rejected candidate carries `operator` or `agent` the same way: only an operator's
rejection is permanent for the session; an agent's returns to pending once the sign or kind
changes, or `|r|` moves by more than two standard errors of Fisher's z, since the review.
`Direction` is `0` for positive, `1` for negative: the sign, not the orientation.

`Review` is the latest review, `null` until there is one: `By` (`operator`, `rules`,
`decision-model`, `reasoning-agent`, or `intervention` when the relief loop's own verified
actions created or promoted the relationship the candidate proposed), `Model`, `Tier`, `Action` (`confirm`, `reject`,
`defer`, or `undecided` when no tier was sure enough: the candidate stays pending and is reviewed again when its evidence moves),
`Applied` (false when the autonomy level let the reviewer only suggest), `DecisionID` (the
record at `GET /state/decisions/{id}`; none for an operator), `At`, and the candidate's
`MIScore`, `NObservations`, `Direction` and `Kind` when it was reviewed.

`Undirected` is `true` for two node-level properties or two subjects' properties:
correlation does not say which drives the other, so `FromID`/`ToID` are just the pair in
lexicographic order and the id uses `<->`. `Kind` is `composition` when the source is part
of the target by definition (a pod's CPU against node CPU, or a source whose producer declared it `part_of` the target), `same-metric` for two
subjects' readings of one metric, where a shared driver is the likelier explanation, and
`same-subject` for two metrics of one subject (a capped pod's CPU and its throttling),
which often hold together by construction.
`Caveats` lists what the proposer could not check, such as an end with no declared range.
`PBar` is the p-value bar the candidate cleared at its last update: Benjamini–Hochberg at
`map.proposer.fdr` over every pair being tested, so the more pairs are tested the more
evidence a candidate needs. It is `0` when the correction is off.

How many pairs are being tested is in `GET /state`, not here: an empty list means only
that no *tested* pair has cleared the threshold.

```json
"discovery": {"budget": 7989, "properties": 39, "eligible_pairs": 741, "tested_pairs": 741, "idle_pairs": 0,
  "tested_by_class": {"node<->node": 36, "subject->node": 270, "subject<->subject": 435},
  "displaced": 0, "groups": [{"chart": "system.cpu", "representative": "user", "members": ["system", "user"]}],
  "fdr": 0.05, "p_bar": 0.0021, "p_values_tested": 741}
```

`budget` is `map.proposer.max_pairs` as resolved at start (`0`: no limit). When
`tested_pairs` is below `eligible_pairs` the budget is rationing: the most variable
properties get pairs first, and `displaced` counts pairs dropped to make room.
`idle_pairs` are tested pairs with an end not observed within the pair window: they still
hold budget, are displaced first, and go when the property retires. `groups` are the
arithmetic groups found; only each `representative` is paired outside its group. `p_bar`
is the bar a candidate's p-value must be at or under now, at false-discovery rate `fdr`
over the `p_values_tested` pairs being tested — arithmetic links aside, and counting a
pair without enough readings yet as a test that found nothing. The block is absent when the
proposer is off.

Review via `POST /candidates/{id}/confirm`, `/reject`, or `/defer`. Confirming an undirected
candidate needs a body naming the end it runs from, `{"from": "cpu_utilization"}`
(`mapctl candidates confirm <id> --from cpu_utilization`); without one, or with a property
that is not an end, the answer is `400` and the candidate stays pending. A confirmation whose
declaration the map refuses (an opposite-sign edge already there) reopens the candidate;
a confirmed candidate cannot be rejected or deferred — retire the relationship instead.

### Full endpoint table

The five summaries above are the original control-plane queries. Phase 1 of the control-surface work added 14 endpoints for graph introspection, ontology mutations, candidate review, meta probes, and the embedded UI. All Phase 1 endpoints return JSON on both success and failure; mutation POSTs require `Content-Type: application/json` (lightweight CSRF mitigation — see [ARCHITECTURE.md §9](ARCHITECTURE.md#9-control-surface)).

| Verb | Path                                | Body / params                                            | Since    |
| ---- | ----------------------------------- | -------------------------------------------------------- | -------- |
| POST | `/ingest-sample`                    | `MetricSampleRequest`                                    | replay   |
| GET  | `/cost`                             | `?task=&node=`                                           | existing |
| POST | `/recommend`                        | `OffloadContext`                                         | existing | `409` when no peer qualifies or none are registered — an ordinary state, not a fault |
| POST | `/simulate`                         | `{context, target_node_id}`                              | existing |
| GET  | `/candidates`                       | `?status=all\|pending\|confirmed\|rejected\|deferred` (default pending) | existing |
| GET  | `/graph`                            | —                                                        | Phase 1  |
| GET  | `/edges`                            | `?from=&to=`                                             | Phase 1  |
| GET  | `/constructs`                       | —                                                        | Phase 1  |
| GET  | `/propositions`                     | —                                                        | Phase 1  |
| GET  | `/history`                          | `?since=` (RFC3339 or duration)                          | Phase 1  |
| GET  | `/neighbors`                        | `?node=`                                                 | Phase 1  |
| GET  | `/healthz`                          | —                                                        | Phase 1  |
| GET  | `/version`                          | —                                                        | Phase 1  |
| POST | `/ontology/strength`                | `{proposition_id, strength}`                             | Phase 1  | `404` when the proposition is declared but not modelled here |
| POST | `/ontology/deprecate`               | `{proposition_id, reason}`                               | Phase 1  | `404` likewise |
| POST | `/ontology/construct`               | `{construct_id, name, description}`                      | Phase 1  |
| POST | `/ontology/proposition`             | `{proposition_id, from, to, direction:"+"|"-"}` | Phase 1 |
| POST | `/agent/reset`                      | `{from, to}`                                             | Phase 1  |
| POST | `/agent/tune`                       | `TuneRequest{intent, operator?}`                         | Step 7   | Map natural-language intent to proposition strength adjustments |
| POST | `/candidates/{id}/confirm`          | optional `{from}`; required for an undirected candidate  | Phase 1  |
| POST | `/candidates/{id}/reject`           | path only                                                | Phase 1  |
| POST | `/candidates/{id}/defer`            | path only                                                | Phase 1  |
| GET  | `/config`                           | —                                                        | spec     | The spec the agent runs on (`agent.yaml` over the defaults), secrets shown as `[redacted]`; so are each collector entry's non-empty `config` block and `exec.args`, and an `http.url`'s userinfo and query (its fragment is dropped) |
| GET  | `/collectors`                       | —                                                        | collectors | `{interval_ms, collectors:[…]}`: `sources.interval` in milliseconds and each configured collector's state, counters and declared types, in list order; see [`GET /collectors`](#get-collectors) |
| GET  | `/workloads`                        | — each workload the runtime lists: `subject, name, labels, resources, can, policy`; `resources` by name, `cpu` in cores and `memory` in bytes, each `{request, limit}` (`null` when not set); `can` by action, why that action cannot apply to the workload, `""` when it can; `policy` the effective policy (`frozen`, `frozen_by`, `rule`, `settings`) | runtime  |
| GET  | `/actuators`                        | `?subject=pod:x` optional — each action the agent can take: `action`, `built: true`, `descriptor` (`action, kinds, resource, unit, capacity, min, relieves, usage, describe`) `held`, the ledger's entries this agent manages and gives back (this node's: `action, subject, decision_id, original, original_set, applied, expires, node_id`; `original_set` false: there was no setting to give back; an entry written ahead of its change and not yet settled also has `pending: true`, `set_at` (when it was written; its `settle` runs from then) and, for a further step, `prev_applied`, `prev_set`, `prev_decision_id`, `prev_expires`), and `unmanaged`, the ledger's entries of the action nothing in this agent gives back, each an entry with `why`: another node's (`recorded by node X; this agent is Y; not managed`), never adopted, and, on a dry run on a runtime that can change the system, a real run's changes still in force (`held by a real run and not given back while dry_run is on`). After them, one row per action the ledger holds changes of and no lever runs (relief off, or its lever removed): `action`, `built: false`, `held: []` and those changes under `unmanaged` (`no lever runs for this action`), no `descriptor` and no `can` (OPERATING.md §10). Start logs a warning for every unmanaged entry. With `subject`, `held` and `unmanaged` are that workload's and each built action adds `can` (`""` when it can apply); `404` when the runtime has no such workload | actuators |
| GET  | `/policy`                           | `?subject=pod:x` — the effective policy for one workload, `settings.cooldown` a Go duration (`"10m0s"`) as in `/workloads` and `/config`; `400` without `subject`, `404` when the runtime has no such workload | runtime  |
| GET  | `/relief`                           | — `{"levers": [...]}`, one relief loop per lever in `loops.relief.levers` order, each with its `action`, `phase`, `paused`, `runtime`, `dry_run` (true when the lever decides and records and its actuator changes nothing: `loops.relief.dry_run`, or `runtime.kind: dry-run`), `in_flight`, `actions`, `kept`, `reverted`, `refused`, `predictions_held`, `stuck` (changes in force whose undo the actuator has refused, or whose setting it cannot read; absent when none), `unread` (the ledger's changes whose setting the actuator could not read at the latest reconcile, including the pending entry of a change in flight that has not read back, and a step that never took whose setting cannot be read again before it is settled (with `reconcile_error` set and nothing new applied to that workload until it reads; the lever goes on with the others), each `entry`, `error` and `attempts`: kept untouched, never stepped, retried every tick, and given back from the ledger's record once expired and closed once that reads back as the original, `given_back` true meanwhile; a given-back change that goes through and still does not read back as its original is set again at most 3 more times, waiting twice, four and eight times the loop's interval, then left for an operator, still listed, its `error` saying so (a Set refused outright backs off the same way without counting); one that reads back as another writer's value is closed as `gone`, saying so; absent when none), `reconcile_error` (why the latest reconcile failed — the listing failed or held no workload while this node holds changes, or the ledger could not be saved; while it lasts every change of the lever is listed under `unread` with it, nothing new is applied, and one past its expiry is still given back through its subject; or, with the reconcile otherwise done, why a step that never took could not be read again — only that workload is held; absent otherwise), `cooling` (workloads this lever will not act on before the time given, whichever lever's cooldown that is) | relief   |
| GET  | `/relief/actions`                   | — every lever's actions, newest first by `acted_at` (at most 200 per lever; `entry.action` says which lever), each with its `plan` (`subject, from, to, unit, predicted, estimate_id, basis, rule, effect`: usage `from` and the setting `to`, in `unit`; `effect` the learned factor the prediction was scaled by), `entry` (the ledger's record, as in `/actuators`: `original` is the setting before the agent's first step), `verify_id` (the decision that judged it; absent until verified), observed drop, `outcome` (`kept`, `reverted`, `released`, `expired`, `gone`, `manual`, or `not_applied`: the change still read as before once `settle` had passed, never took, and its hourly action was given back), `shares_at_act` (every workload's share when it acted, against which the outcome is checked for confounding), and, for a change whose undo the actuator has refused, `revert_attempts` (refusals since its latest step) and `last_error` (the latest one); the undo is retried every tick and its first refusal is journalled once, as a `relief-stuck-*` decision | relief   |
| POST | `/relief/actions/{id}/revert`       | JSON content type — undo one action in force, on whichever lever holds it, and cool its workload down; outcome `manual`; `404` for an id no lever holds in force | relief   |
| POST | `/relief/pause`                     | JSON content type — every lever stops making and releasing changes; verifying, reverting a failed action and expiry go on; answers as `GET /relief` | relief   |
| POST | `/relief/resume`                    | JSON content type — undo pause, on every lever            | relief   |
| GET  | `/groups`                           | —                                                        | groups | The node's topology: `groups` (`id`, `name`, `source`, `members`, `consumers` — whether the map holds the group's sums as `cpu_utilization@group:<name>` and `memory_utilization@group:<name>`) and `parents` (a container's or a `veth`'s pod) |
| GET  | `/autonomy`                         | —                                                        | autonomy | The reviewer's level, models, thresholds and counts; `{"level":"off"}` without one |
| GET  | `/peers`                            | —                                                        | Step 4.9 |
| POST | `/peers`                            | `{url, note?}`                                           | Step 4.9 |
| DELETE | `/peers/{id}`                     | path only                                                | Step 4.9 |
| POST | `/peers/{id}/trust`                 | `{value}`                                                | Step 4.9 |
| POST | `/offload`                          | `OffloadHTTPRequest`                                     | Step 4.9 |
| POST | `/explain`                          | `{question, session_id?, use_planner?, use_critic?, stream?, max_iterations?, max_tool_calls?}` | Explain v1/v2 |
| GET  | `/ui/...`                           | —                                                        | Phase 2B |
| GET  | `/state`                            | `?kind=&status=&min-confidence=&related-to=&id=&subject=` — `kind` and `status` repeat or take a comma list; an unknown value is `400`. Carries `discovery`, the pair budget and how far it rationed | State model |
| GET  | `/state/properties/{id}`            | path only; `Accept: text/plain` renders the neighbourhood | State model |
| POST | `/state/properties`                 | `Property`                                               | State model |
| DELETE | `/state/properties/{id}`          | `?reason=&actor=` — `reason` required                    | State model |
| GET  | `/state/relationships`              | `?from=&to=`                                             | State model |
| POST | `/state/relationships`              | `Relationship` — `learned` and `intervened` provenance are refused; relationships carry `interventions` (agreed, null, confounded counts) once the agent has acted on them | State model |
| DELETE | `/state/relationships/{id}`       | `?reason=&actor=`                                        | State model |
| POST | `/state/relationships/{id}/strength` | `{strength, actor?, reason}` — `reason` required        | State model |
| GET  | `/state/estimate`                   | `?target=[&id=][&assume=<property>=<value>]*[&without=<subject|property>]*` — answer from the map, optionally under hypotheses (`without=group:<name>` takes every member away); `id` names the decision (default `est-<revision>-<target>`); returns a decision id | State model |
| GET  | `/state/journal`                    | `?since=&limit=`                                         | State model |
| GET  | `/state/decisions`                  | `?limit=`                                                | State model |
| GET  | `/state/decisions/{id}`             | path only — `410` when the journal has dropped it        | State model |
| POST | `/state/sweep`                      | —                                                        | State model |
| GET  | `/state/cluster`                    | — this node and every peer, unmerged                     | Peer state |
| GET  | `/state/peers`                      | — per-peer owner, age, census, last error                | Peer state |
| GET  | `/state/peers/{id}`                 | path only                                                | Peer state |
| POST | `/state/peers/refresh`              | — poll every peer now                                    | Peer state |
| GET  | `/state/where`                      | `?property=` — per-node answers, deliberately not averaged | Peer state |

When `loops.relief.enabled` is false every `/relief*` route is `404` with `relief is not enabled in the spec`. `/workloads`, `/policy` and `/actuators` answer either way, but no actuator is built: `/actuators` lists only the ledger's changes, under `unmanaged` in `built: false` rows (`[]` when it holds none), and each workload's `can` is `{}`. The mutating relief routes refuse a body without `Content-Type: application/json` with `400`, as every other mutating route does.

`POST /explain` returns `200` with an `ExplainResponse`, `422` with `{error, response}` when the answer failed a gate after `max_iterations`, `501` when `explain.provider: none`, and `application/x-ndjson` instead of JSON when `stream:true`. See [§8](#8-natural-language-explain-explain).

JSON error shape for Phase 1 endpoints:

```json
{"error": "Content-Type must be application/json"}
```

The five pre-Phase-1 endpoints keep `http.Error`'s plain-text error body for backward compatibility.

---

## 3. Running the Edge Daemon

**Prerequisites:** Go 1.22+, `linux/arm64` for RPi4.

```bash
# Build for RPi4
cd semantic-map/go
GOOS=linux GOARCH=arm64 go build -ldflags "-s -w" -o agent-arm64 ./cmd/agent   # -s -w strips symbols: advisable on edge devices, since client-go makes the binary roughly 36 MiB without it

# Deploy
scp agent-arm64 pi@192.168.1.x:/usr/local/bin/agent

# Run on RPi4, described by a file (see below)
agent -config /etc/semantic-map/agent.yaml

# Run locally (development): the defaults, with a domain model, the map kept in /tmp
printf 'domain:\n  path: ../domain_spec.json\npersist:\n  file: /tmp/di-agent/state.json\n' > /tmp/agent.yaml
go run ./cmd/agent -config /tmp/agent.yaml
```

### The agent is described by `agent.yaml`

The agent takes its whole configuration from one YAML document. There are two flags and no others:

| Flag | Meaning |
| ---- | ------- |
| `-config <path>` | The spec file. Empty runs the defaults. |
| `-print-config` | Print the effective spec as YAML (the file over the defaults) and exit. |

The file overlays the defaults, so it names only what it changes. A key the agent does not know is refused at startup, because a misspelt threshold that silently keeps its default is a setting that describes nothing. `${VAR}` in a value is replaced from the environment, so a secret stays out of the file (`explain.api_key: ${EXPLAIN_API_KEY}`). A variable that is not set is refused at startup, naming every one missing, rather than read as empty; one set to the empty string is empty. `$$` is a literal `$`, and any other `$` is kept as written, so a password or a pattern survives. Variables are expanded in values after the file is read, never in keys' structure or comments: what a variable holds is the value it stands for, not more YAML, and a plain value is typed afresh (`enter: ${ENTER}` with `ENTER=0.3` is a number; quote it to keep a string). In a flow mapping (`{api_key: ${KEY}}`) the braces end the value, so quote it there: `{api_key: "${KEY}"}`. This replaces the old `EXPLAIN_API_KEY` fallback, which the agent no longer reads on its own, and the `AUTONOMY_*_API_KEY` variables likewise: name them in `loops.review.decision.api_key` and `loops.review.reasoning.api_key`.

`agent -print-config` prints every setting with its default, and [`agent.example.yaml`](agent.example.yaml) is that output. Copy it and delete what you do not change. A running agent shows the spec it runs on, secrets redacted, at `GET /config`.

```yaml
# agent.yaml: a k0s node polling its local Netdata, with the review loop on
node:
  id: node_1
profile:
  kd: k0s
  priors: /etc/semantic-map/prior_weights.json
sources:
  collectors:
    - name: cgroup
      type: cgroup
    - name: netdata
      type: netdata
      config: {url: http://localhost:19999, collapse: false}
loops:
  review:
    level: full
```

Lists are YAML lists, durations are Go durations (`10s`, `2m`, `0s`), and the old `-flag=false` booleans are plain `true`/`false` values. Three lists replace rather than extend their defaults: `sources.collectors` written in the file is the whole set of collectors (naming Netdata alone drops the default cgroup entry); `policy.rules` written in the file **replaces** the default rules (it does not append to them), so copy the default freeze rules from `agent -print-config` before adding your own — at start the agent logs `policy: policy.rules replaces the default rules and has none to freeze where …` for each default freeze rule (the `di-agent/freeze` label, and the critical priority classes) the effective rules no longer cover; `facts` likewise names the fact sources outright (`facts: []` reads none). `loops.relief.levers` replaces its default too, but a lever in it names only what it changes: it starts from the default lever for its action, so `levers: [{action: cpu.limit, enter: 0.4}]` keeps every other `cpu.limit` threshold. A `floor` is per action and not a list: `policy.defaults.floor` and a rule's `floor` name actions, each with `min` (in the action's unit: cores for `cpu.limit`, bytes for `memory.high`), `of_original` (a share of the workload's setting before the agent's first change) or both, and every action and bound one leaves out is the default's, so a rule's `floor: {cpu.limit: {min: 0.5}}` keeps `cpu.limit`'s `of_original: 0.5` and the whole `memory.high` floor. The defaults are `cpu.limit: {min: 0.1, of_original: 0.5}` and `memory.high: {min: 67108864, of_original: 0}`. The keys these replaced, `loops.relief.cpu` and `floor.cpu_cores`, are refused.

Values that cannot be what was meant are refused at startup, every one named. In the policy: an `allow` or `deny` entry or a `floor` key that is not an action (`cpu.limit`, `memory.high`, or `evict`, which no runtime performs and the defaults deny), in the defaults or any rule, since a misspelt deny would deny nothing; a floor bound out of range (`min` below zero, `of_original` outside [0,1]); and a `match.kind` that is not a workload kind (`pod`, `container`, `unit`, `app`), since a misspelt kind would match nothing. In relief, each lever is checked and named by its index (`loops.relief.levers[1].exit`): an empty `action`, two levers with one action, a lever whose action has no default lever (`loops.relief.levers[1] cpu.limt: unknown action (known: cpu.limit, memory.high)`), and, with relief enabled, a lever whose actuator cannot be built on this node (`loops.relief.levers[1] memory.high: memory.high needs cgroup v2: …`) refuse start, as do relief enabled with no lever and a lever whose action has no `policy.defaults.floor`; `expiry` must be longer than `settle`, or a change would be given back before it is judged. `min_effect` is a planning floor: a step whose predicted drop in pressure is below it is not taken. A step is then judged against its own prediction: kept when the observed drop is at least half the predicted one, reverted otherwise; a change taken up after a restart is always kept, as its baseline was read with the cap in force.

### The runtime and the relief loops

The agent changes the system it models only through a `runtime`, and only when `loops.relief.enabled` is true (the default is off). It runs one relief loop per lever in `loops.relief.levers`: a lever is an action and the thresholds it acts on, the pressure it relieves being the one its actuator names (`GET /actuators`). The defaults hold two: `cpu.limit`, relieving `cpu_contention_ratio`, and `memory.high`, relieving `memory_contention_ratio`; naming the list in the file replaces it, so `levers: [{action: cpu.limit}]` turns the memory lever off. The actuators are built only while relief is enabled, so with relief off `GET /actuators` lists no built action and `GET /workloads` has an empty `can`. A change the ledger holds for an action no lever runs is not given back by anything: start logs a warning for each, and `GET /actuators` shows it under `unmanaged` in a `built: false` row. So is one another node recorded (a ledger file shared or copied between nodes): it is never adopted, warned of, and shown under `unmanaged`. The loops share one budget: `max_actions_per_hour` is one count for every lever (a rule's own `max_actions_per_hour` is the limit for its workloads, against that one count), and a workload one lever acts on is cooling down for every other lever for its `cooldown`, so two levers never stack changes on one workload; the lever that acted may still step its own change. The action and the hold on the workload are reserved together, in one step, before the change is made, so a lever ticking at the same moment cannot take the workload while the first change is still going through; a change whose Set fails keeps the action and the hold: one that reads back changed is adopted, and any other is left pending, since a request the client gave up on may still land; it is read again every pass, committed if it lands, and settled as `not_applied` once it still reads as before `settle` after it was written (the entry's `set_at`), which gives the action back; meanwhile nothing is planned on its workload. Each lever has at most one action in flight. `runtime.kind` is `dry-run` (changes are recorded in memory and nothing is touched; the default; `cpu.limit` acts there on every workload but the agent's own, limited or not); `kubernetes` acts on this node's pods (below); there `runtime.kubeconfig` is a kubeconfig file (empty means the in-cluster service account) and `runtime.node` is the node's name in Kubernetes, defaulting to `node.id`. What may be touched is `policy`, shown at `GET /workloads` and `GET /policy`.

```yaml
# relief on dry-run: the loop decides, records and verifies; nothing is changed
node:
  id: node_1
runtime:
  kind: dry-run
loops:
  relief:
    enabled: true
    interval: 15s       # how often every lever's loop looks
    # dry_run: true     # unset: a dry run exactly when runtime.kind is dry-run
    levers:
      - action: cpu.limit
        enter: 0.25     # CPU contention (stall less own throttling) that starts relief, held for enter_for
        exit: 0.10      # contention below which caps are given back, held for exit_for
      - action: memory.high
        enter: 0.20     # memory stall of a pod within its request, not limiting itself
        exit: 0.05
        step: 0.25      # each step lowers memory.high by a quarter of what the pod uses
actuation:
  ledger: ""            # "": actions.json beside persist.file
policy:
  defaults:
    floor:
      cpu.limit: {min: 0.1, of_original: 0.5}   # never below 0.1 cores, nor half the original limit
      memory.high: {min: 67108864}              # never below 64 MiB
```

The trigger is the node's CPU contention, `cpu_contention_ratio`: the share of time a pod waited for CPU it was owed and another pod held — PSI stall less the pod's own throttling, counted only for a pod getting less than its weighted fair share of the pods' CPUs (a pod past its share waits on its own threads, and capping a neighbour cannot help it) — not CPU utilisation, and not `cpu_pressure_ratio`, which counts a pod held back by its own limit as pressure. A node can be fully used and not contended, and the loop acts only on the waiting. It comes from the cgroup collector with subjects on (the default).

The memory lever's trigger is `memory_contention_ratio`: the worst memory stall (PSI `some`) of a pod promised memory (not BestEffort) that is not limiting itself (its `memory.events` `high` and `max` did not rise; a pod whose `memory.events` delta is unknown is left out) and is within its memory request. It is 0 only when no pod is promised memory or every promised pod was read this window; with some unread only a stall above 0 is reported, since a 0 releases caps. The requests are listed from the runtime at start and every 30s (`memory requests: listed from runtime … every 30s` in the log); a listing that fails keeps the last requests and is logged once. On `dry-run` there are none, and the condition is dropped. `memory.high` acts on a pod using more than its request, by lowering the pod's cgroup v2 `memory.high`: a soft cap the kernel enforces by reclaiming and throttling the pod's allocations, never by killing it, and given back by writing `max`. It is never `memory.max`. On a real runtime it needs cgroup v2 at the cgroup collector's `config.root` and the kubelet's MemoryQoS off, or start is refused; [OPERATING.md, Memory relief](../OPERATING.md#10-memory-relief) has how to check and what is written.

`loops.relief.dry_run` unset is the runtime's say: a dry run exactly when `runtime.kind` is `dry-run`. Set `true` on `kubernetes`, the loops decide, record and verify against the real pods while every actuator is wrapped so that nothing is changed, the ledger is kept in memory, and no outcome is evidence; the on-disk ledger, when it exists, is read without ever being written, so a real run's changes still in force are not taken for the kubelet's (memory.high), are warned of at start, are shown under `unmanaged` at `GET /actuators`, and are not given back until a real run starts again (a corrupt one refuses start); `false` on `dry-run` is refused, since that runtime changes nothing.

Every change the agent holds is in its ledger, shown at `GET /actuators`: the file `actuation.ledger` names, or, left empty (the default), `actions.json` beside `persist.file` (`/var/lib/di-agent/actions.json` when `persist.file` is empty). With relief enabled and not a dry run, a ledger that cannot be written, or holds an invalid entry, refuses start, naming its path; with relief off the agent changes nothing and still shows what the file holds: its valid entries are warned of and listed under `unmanaged`, and each invalid one is named in a start warning by index and reason; keep it on persistent storage, as it is what lets a restarted agent give back what it changed. On a dry run the ledger is in memory.

The hourly limit (`max_actions_per_hour`) survives a restart: changes the agent takes up from the ledger count toward it from the time the journal says they were made, or from the restart when the journal no longer has them. Cooldowns do not, except that a change taken up holds the other levers off its workload for its cooldown from when it was made: they are per process lifetime, so a restarted agent may act on a workload that was cooling down.

Outcomes teach the map — interventions on relationships, each workload's learned effect, candidates settled — only on a runtime that really changes the system, never on `dry-run` (ARCHITECTURE §16, "Learning from outcomes"). At start the agent logs `relief: <action> acting every <interval> through runtime <name>` for each lever, with `, as a dry run` when it is one. Watch it with `curl localhost:8080/relief` and `curl localhost:8080/relief/actions`; stop it making changes with `curl -X POST -H 'Content-Type: application/json' localhost:8080/relief/pause`. Every other lever key is in [`agent.example.yaml`](agent.example.yaml).

#### The `kubernetes` runtime

```bash
# once per cluster: the agent's ServiceAccount (`di-agent`, namespace kube-system), the ClusterRole and the binding
kubectl apply -f deploy/kubernetes/rbac.yaml
# one agent per node; set `image` first, as no image is published
kubectl apply -f deploy/kubernetes/daemonset.yaml
```

The DaemonSet keeps the map on the node, in `/var/lib/di-agent` (a `hostPath`), so it survives the pod being restarted or upgraded; a drain sends SIGTERM and the agent saves before it exits. The node's filesystem is mounted read-only at `/host`, where the agent reads the node's cgroups, pod logs and facts (`facts[].root: /host`), so the map describes the node and not the container. It tolerates every taint, so control-plane and tainted nodes run an agent too, and ships with `loops.relief.enabled: false`. With relief on, the memory lever needs the cgroup tree writable and refuses start on the read-only `/host` mount, except on a dry run (`loops.relief.dry_run: true`), which never writes.

The agent's own pod must carry its UID in `POD_UID`, from the downward API, or the agent refuses to start in-cluster: it is how the agent recognises itself among the pods it may cap.

```yaml
env:
  - name: POD_UID
    valueFrom: {fieldRef: {fieldPath: metadata.uid}}
```

Run from a host with a `kubeconfig` and no `POD_UID`, it starts and logs that its own pod cannot be recognised.

```yaml
node:
  id: worker-1            # a pod with this node name; or set runtime.node
runtime:
  kind: kubernetes
  kubeconfig: ""          # empty: in-cluster; else a kubeconfig path
```

It needs Kubernetes 1.33 or later, where a pod's CPU limit can be changed in place (the `resize` subresource): the container is not restarted. `POD_UID` (above) marks the agent's own pod, which no rule may touch. Every API request times out after 30s, and a relief pass, or a manual revert, gives up on the runtime after `loops.relief.interval` (at least 10s). The role grants `list` on `pods` and `patch` on `pods/resize`, and nothing else; nothing is written to the pod but its limit. Note that `patch` on `pods/resize` is cluster-wide in RBAC: Kubernetes cannot scope it to a node. The agent only ever resizes pods on its own node. A workload is a pod on this node, named by its bare pod name, with `k8s/namespace` as a label. Pods that cannot take a CPU cap are listed with the reason in `GET /workloads` (`can["cpu.limit"]`); see ARCHITECTURE §16.

### The state model

The map holds what the system exhibits and how those properties relate, updated by
every sample, queryable and traceable.

```bash
# what is this system doing right now
curl -s localhost:8080/state | jq '.counts'

# what the machine is: OS, CPU, memory, disk, read at startup
curl -s localhost:8080/state | jq -r '.facts[] | "\(.key)\t\(.value) \(.unit // "")"'

# how many property pairs discovery is testing, of how many it could
curl -s localhost:8080/state | jq '.discovery'

# one property, readable in a terminal
curl -s -H 'Accept: text/plain' localhost:8080/state/properties/RC

# every cost answer is grounded in the map and carries its trace
curl -s 'localhost:8080/cost?task=placement' | jq '.ResourceCost, .DecisionID, .Caveats'
curl -s localhost:8080/state/decisions/$(curl -s 'localhost:8080/cost?task=placement' | jq -r .DecisionID) \
  | jq '.revision, .properties_read, .relationships_read'

# or ask about any property
curl -s 'localhost:8080/state/estimate?target=PS&id=why-slow' | jq '.answer, .influences, .caveats'

# lifecycle
curl -s -X DELETE 'localhost:8080/state/properties/gpu_util?reason=device+removed&actor=me'
curl -s -X POST localhost:8080/state/sweep | jq
curl -s 'localhost:8080/state/journal?limit=10' | jq '.held, .dropped'

# does the graph assert a direction this machine keeps contradicting?
curl -s localhost:8080/state | jq '.counts.relationships_sign_suspect'
curl -s localhost:8080/state \
  | jq '.relationships[] | select(.sign_suspect) | {label, from, to, sign, effective, basis, sign_conflicts, sign_agreements}'
```

**The strength layers.** A relationship reports `recent` (the fast EMA; `/edges`
calls it `ema_weight`), `established` (the long-run learned baseline), `measured` (what
the agent's own interventions on the pair observed: the mean drop in the target per unit
drop in the source), `assertion` (an operator's override), the `effective` value the
agent reasons with, and `basis` naming which layer that came from — assertion, then
measured, then established, then recent. Read `effective`: `recent` is the fastest-moving
layer and answers only when none of the others exists. `effective` and the pointer layers are **absent rather than
zero** when they do not exist, because zero is the claim that a relationship is worth
nothing and absence is not that claim.

```bash
# what each relationship is worth, and on what basis
curl -s localhost:8080/edges \
  | jq '.[] | {proposition_id, basis, effective, measured, established, assertion, ema_weight, n_observations}'

# relationships the agent has no measurement for yet
curl -s localhost:8080/edges | jq '[.[] | select(.basis == "unknown") | .proposition_id]'
```

Nothing seeds a magnitude. A freshly started agent reports `basis: "unknown"` for every
relationship and no `effective` value at all; strengths appear as pairs accumulate.
`established` needs roughly 1000 pairs — about two workloads, or half an hour at 1 Hz —
so it survives restarts by design rather than by accident.

**Sign-suspect relationships.** A relationship whose declared sign the machine
contradicts holds zero strength — the estimator's gate treats a correlation of the
opposite sign as evidence *against* that relationship, which is what lets two opposed
claims over one pair separate. The cost of that design is a reporting ambiguity: zero
strength also describes a relationship on a quiet system, and only one of the two is a
bug. `sign_conflicts`, `sign_agreements` and `sign_suspect` on each relationship, and
`counts.relationships_sign_suspect` in the census, are what tell them apart. A
relationship is flagged when at least 30 pairs have been folded in and 60% or more of
them contradicted its sign; a sign that is merely regime-dependent lands near 50% and
is not flagged, because that is a fact about the system rather than a wrong
declaration. Investigate a flagged relationship as a defect in the specification —
usually a direction inherited from a framework that phrased its outcome in the opposite
polarity to the construct it is now attached to (see ARCHITECTURE §5 on polarity).

A metric the specification never declared still becomes a property: `/ingest-sample`
answers 202 with `routed:false` rather than rejecting it, because a reading the system
produced is not a typo to discard, and the journal records the admission. Silence
marks a property stale (`map.stale_after`) and optionally retires it (`map.retire_after`);
`map.admit: false` closes the model for a deployment that wants that.

### One agent per machine

The map is node-local: each agent models the machine it runs on, and its graph holds
that machine's evidence. `node.id` is its identity (falling back to the hostname),
and the default `node.ingest_scope: self` rejects telemetry labelled with another
machine's ID rather than folding it in. Every map is stamped with its owner, so a
property is attributable once state crosses a node boundary, and a snapshot copied
from another host is refused instead of adopted as local history.

Each example below keeps the map in `/var/lib/di-agent/state.json`, the default; run as a user who cannot write there, add `persist: {file: ""}` to keep it in memory, or name a writable path.

```bash
# A deployment: one agent per node, modelling that node
# agent.yaml: node: {id: node_1}
agent -config agent.yaml

# Replaying a whole testbed into one daemon — an aggregate, not a deployment
# agent.yaml: node: {ingest_scope: any}
agent -config agent.yaml
```

`node.ingest_scope: any` exists for exactly that replay case and logs a startup warning:
the resulting edge weights are means over machines that may be different physical
systems. `GET /cost?node=X` returns 409 when X is not this agent, pointing at that
machine's own endpoint when it is a known peer.

### Asking other nodes

A node-local map means any question wider than one machine is a question for other
agents. Each agent fetches its peers' maps (`peers.state_poll`, default 30s) and holds
them under the owner each peer reported.

```bash
# agent.yaml: node: {id: node_1}, peers: {urls: [http://node_2:8080]}
agent -config agent.yaml

# this node and every peer, side by side
curl -s localhost:8080/state/cluster | jq '.cluster.self_id, .cluster.peers | keys, .stale_peers'

# who has this property, and what do they say it is
curl -s 'localhost:8080/state/where?property=cpu_utilization' | jq '.holders'
# → [{"node":"node_1","local":true,"value":0.15,…},
#    {"node":"node_2","local":false,"value":0.88,"age_seconds":1.8,…}]

curl -s localhost:8080/state/peers | jq            # who, when, how much, last error
curl -s -X POST localhost:8080/state/peers/refresh # poll now instead of waiting
```

Peer state is never merged into the local map. A property with the same ID on two
nodes describes two systems, and folding them together would make every "this system"
answer a claim about some union of machines. So answers stay per node and are not
averaged — the reason to ask several nodes is to be able to pick one.

Three failure modes are reported rather than smoothed over. A peer that answered
before and cannot be reached now keeps its last snapshot and is listed under
`unreachable`, because during a partition that snapshot is all this agent has. An
address that never identified itself appears under `silent` and does not become a node,
since inventing one from a URL would put a machine that may not exist into the cluster
view. And a snapshot older than `peers.state_stale` (default 90s) is labelled history
rather than withheld.

### How relationships learn

A relationship advances on a *pair*: both endpoints observed within
`map.pair_window` (default 15s). What it learns is `|r|`, the correlation
magnitude over the last `map.pair_history` pairs, on `[0, 1]`. It is folded twice from the
same pairs: a **recent** estimate at `profile.alpha` (default 0.20, memory ≈ 5 pairs) and an
**established** baseline at `profile.alpha_slow` (default 0.001, memory ≈ 1000 pairs,
bias-corrected). `Effective()` picks between them by authority rather than by arithmetic
— an operator assertion first, else what the agent's interventions measured, else the
established baseline, else the recent estimate, else `unknown` — and `basis` on every
edge names which one answered.

```bash
# agent.yaml
#   profile: {priors: ../prior_weights.json, kd: k0s}
#   map: {pair_window: 15s, pair_support: 8, pair_history: 60}
agent -config agent.yaml
```

Two consequences worth knowing before reading a number off `/edges`:

- **One endpoint moving proves nothing.** A stream that drives `cpu_utilization` and
  nothing else leaves every relationship at `basis: unknown` with confidence 0, however
  long it runs. That is the honest report: the magnitude of a construct is not a
  measurement of how strongly it influences another. The same holds for a *constant*
  endpoint — a correlation over a flat series is undefined, and the estimator declines to
  move rather than reporting a coefficient it did not measure. On this testbed the NUC
  control plane exhibits exactly that: its cpu stall-pressure reads identically zero, so
  nine full workloads leave it with no baseline at all.
- **A contradicted sign is distinguishable from silence, and only because it is
  counted.** A relationship whose declared sign the machine refutes collects pairs, has
  each one rejected at the gate, and holds a strength of zero — which is also what a
  relationship on a quiet system holds. Every relationship therefore carries
  `sign_agreements` and `sign_conflicts`, exposes a conflict share, and is flagged
  `sign_suspect` past 30 pairs at 60% conflict; the census of every view reports how many
  relationships the machine persistently refutes. This is not hypothetical: two of the
  four propositions an earlier specification carried sat in that state on every cluster,
  and nothing in the aggregate metrics showed it.

- **Two claims over one pair learn the same number.** Where a specification declares both
  directions over a construct pair, both relationships receive the same pairs and Pearson
  correlation is symmetric, so their magnitudes agree to four decimals. Direction comes
  from the declaration and is not inferred; what the pair gives the Reasoner is a
  sensitivity for each construct's own cost term, not two independent measurements.

`map.pair_window` is a tolerance, not smoothing: collectors sample on independent
grids (`system.*` on 0, 5, 10 …, PSI on 2, 6, 12 …), so without it no pair ever forms.
`map.pair_support` is how many pairs a relationship needs before its strength moves at all
— below that, `n_observations` stays at zero, because confidence has to keep reporting
that nothing has been learned. There was a `-relational` flag here selecting between
this estimator and one that moved an edge on any single sample; only this reading was
defensible, so the choice went. See ARCHITECTURE.md §5 for what it does and does not
claim.

`domain.path` is optional. The daemon carries no constructs, propositions, metric
routes, adjustment bounds or tuner keywords of its own. Without `domain.path` it
searches upward from the working directory for `domain_spec.json`, and if there is
none it runs **without a domain model**: every observed property is still admitted,
subjects are still modelled, relationships are still discovered and estimates still
answered, but nothing is routed into a construct, and `GET /cost`, `POST /recommend`,
`POST /simulate` and `POST /agent/tune` answer `501` saying so. `domain.path: none`
runs without one even when a `domain_spec.json` sits above the working directory — the
search will otherwise find a stray copy in a home directory. The startup log states which
mode the agent is in and, when a model is loaded, the file it came from. A `domain.path` path that does not load is an error,
not a fallback. When you do deploy a specification, treat it as part of the
release: two daemons on the same binary and different specifications are different
agents. The daemon also refuses a stray argument after its flags (a spec path
given without `-config`, or `-print-config false` written with a space), because
Go's `flag` package stops parsing at the first one and would drop it and every
flag after it without a word.

### Replay (Netdata parquet datasets)

`cmd/replay/` drives the dissertation's 225 Netdata parquets
(`multidimensional-analysis/data/raw/{kd}/{test}_runN.parquet` — five KDs ×
nine test types × five runs) into the daemon's `/ingest-sample` endpoint at
a configurable speed. This is how the real-data convergence story is
reproducible from a single command:

```bash
./dev.sh build                                     # also produces /tmp/semantic-map-replay
./dev.sh start

./dev.sh replay list                               # inventory of parquets on disk
./dev.sh replay probe --kd k0s --test idle --run 1 # unique chart_context triples
./dev.sh replay run --kd k0s --test idle --run 1 --speed 0    # max throughput
./dev.sh replay run --kd k0s --test idle --run 1 --speed 60   # 60× compression
./dev.sh replay all --kd k0s --speed 0              # all 9 test types × 5 runs
```

Each row whose `(chart_context, metric_id, units)` matches the v1 mapping
table becomes one HTTP POST to `/ingest-sample`. The `EventID` for a row
is `sha256("replay:" + parquet + ":" + hostname + ":" + chart_context + ":"
+ metric_id + ":" + relative_time)[:16]` — deterministic, so replaying the
same parquet twice cannot inflate `n_observations`. The end-to-end
idempotency proof lives in `cmd/replay/playback/runner_test.go`
(`TestRunner_EventIDsAreDeterministicAcrossRuns`).

Mapping table (cross-KD; see `cmd/replay/mapping/mapping.go` for the
single-row normalizer per triple):

| `chart_context` | `metric_id` | `MetricType` | Normalizer |
| --- | --- | --- | --- |
| `system.cpu` | `idle` | `cpu_utilization` | `1.0 - value/100.0`, clipped to `[0,1]` |
| `system.ram` | `used` | `memory_utilization` | `value / hostRAM`, host total from testbed (master=64 GiB, RPi4=8 GiB) |
| `system.net` | `InOctets` | `network_rx_bps` | `value * 125 / 125e6` — kilobits/s → bytes/s → fraction of a 1 Gbps link |
| `system.net` | `OutOctets` | `network_tx_bps` | `|value| * 125 / 125e6` (Netdata reports outbound as signed-negative) |

Every normalizer lands on `[0,1]`, which is required rather than tidy: the
divergence measure is defined on that interval and an out-of-range value clips,
freezing the affected edge while aggregates keep looking healthy. The reference
capacity (1 Gbps) is a property of the testbed, so a deployment on a different
link must change it or its network-derived weights will not be comparable.

Rows outside the table (Netdata's own `netdata.workers.*` self-monitoring,
disk/inode contexts, per-core `cpu.cpu` channels, …) are silently dropped
by the playback layer. Extending the table later is a contained change in
`cmd/replay/mapping/mapping.go`; the runner and CLI need no edits.

#### Cross-KD compare — removed

`replay compare` built one SemanticMap per distribution in a single process, fed each
its own parquets, and printed a per-edge × per-KD divergence table. It is gone.

The reason is scope rather than mechanics. Cross-distribution comparison is not a goal
of this work — the Semantic Map is about giving one agent a defensible model of one
system — and the tool's own documentation had to open by warning that its table was not
a comparison of Kubernetes distributions, because the parquets are controlled benchmark
runs rather than deployment behaviour. A measurement instrument whose output has to be
labelled "do not read this as what it looks like" is better removed than maintained.

`replay run`, `replay all` and `replay probe` remain: they stream a real dataset into a
running daemon over HTTP, which is how the map gets exercised with telemetry that has
the shape of the real thing.

---

**Reference: every setting, by its path in `agent.yaml`.** Each default is what the flag it replaced defaulted to; `agent -print-config` prints them all.

| Setting               | Default          | Description                                                              |
| ------------------- | ---------------- | ------------------------------------------------------------------------ |
| `profile.name`          | `edge-minimal`   | Deployment profile name                                                  |
| `node.addr`             | `:8080`          | HTTP listen address                                                      |
| `profile.alpha`            | `0.2`            | EMA decay factor                                                         |
| `profile.convergence`      | `500`            | Observations until confidence = 1.0                                      |
| `profile.min_trust`        | `0.5`            | Minimum peer trust score for offloading                                  |
| `profile.priors`           | `""`             | Path to `prior_weights.json` from the initialization pipeline. Supplies structure only — which propositions to instantiate and in which direction |
| `profile.kd`               | `""`             | KD running on this node (`k3s`/`k0s`/`k8s`/`kubeEdge`/`openYurt`). Validated against the artefact's `distributions` list; the daemon refuses to start on a name that is not there. It no longer selects a magnitude — two agents differing only in `profile.kd` answer identically until telemetry arrives. |
| `sources.interval` | `10s`            | How often the autonomous collection loop ticks the collectors in `sources.collectors`; at least `1s`, since timestamps are whole seconds. Set to `0s` to disable the loop (only manual `POST /ingest-sample` then updates the model). |
| `sources.collectors` | `[{name: cgroup, type: cgroup}]` | The ordered list of telemetry collectors. A list in the file replaces the default, it does not add to it; `[]` runs none (only `POST /ingest-sample` then updates the model). Order is ownership priority: a node-level metric type (no subject) is taken from the earliest-listed collector that has emitted it, and the same type from any other collector is dropped and counted (`ownership_drops` in `GET /collectors`); subject-scoped samples have no owner. Every collector runs under a supervisor: one that errors, panics or exceeds its timeout loses only its own samples for that tick and backs off (1 s doubling to 5 min), and a sample that breaks the collector contract is dropped and counted. A list the agent cannot build — an unknown type, a config key the collector does not know, a plugin that does not complete its hello — is refused at startup, naming the entry. |
| `sources.collectors[].name` | — | Instance name, `[A-Za-z0-9._-]+`, unique in the list. It names the collector in the log and in `GET /collectors`. |
| `sources.collectors[].type` | — | A built-in collector: `cgroup`, `netdata` or `script`. Exactly one of `type`, `exec` and `http` per entry. |
| `sources.collectors[].exec` | — | `{command, args}`: an out-of-process plugin the agent starts and speaks wire protocol v1 to on its stdin and stdout; restarted when it exits. |
| `sources.collectors[].http` | — | `{url}`: a plugin reached over HTTP, speaking the same protocol. |
| `sources.collectors[].timeout` | `min(interval/2, 5s)` | The longest one collect may take. Less than `sources.interval`: the supervisor waits it plus 500 ms for an exec or http member, so a timeout at the interval would let one hung plugin stretch every tick past it. |
| `sources.collectors[].config` | — | The collector's own settings. A built-in decodes them strictly (an unknown key is refused); an `exec` or `http` plugin receives them in its hello. |
| `type: cgroup` `config.root` | `/sys/fs/cgroup` | Filesystem root the cgroup collector reads from. On macOS or a node without cgroups v2, leave the cgroup entry out of the list. The agent's `GET /groups` reads cgroups from the first cgroup entry's root. |
| `type: cgroup` `config.subjects` | `true` | Walk pod cgroups (and `config.units`) as subjects: each becomes its own set of properties, admitted on first observation and retired on silence. |
| `type: cgroup` `config.units` | `[]` | A list of globs of systemd units under `system.slice` to model as `unit:<name>` subjects (e.g. `[k0s*.service, containerd.service]`); empty models none. A malformed pattern is refused at startup. A unit whose name needs sanitising to the subject charset gets a short hash suffix so distinct units stay distinct; the `unit` label keeps the true name. |
| `type: cgroup` `config.max_subjects` | `256` | Upper bound on subjects the cgroup walk admits per tick; beyond it the rest are skipped. |
| `type: cgroup` `config.cmd_label` | `false` | Stamp `cmd=<argv0>` from each subject's first process (label only; needs `/proc` visibility, i.e. hostPID or privileged). |
| `type: cgroup` `config.pod_logs` | `/var/log/pods` | The kubelet's pod log directory, one `<namespace>_<name>_<uid>` entry per pod. Each pod subject is labelled `namespace` and `pod` from it, which the console shows instead of the UID; the subject stays `pod:<uid>`. Empty names none; in a container, mount the host's directory. The kubelet's per-container links beside it (`/var/log/containers`) break a pod with more than one container into `container:<uid>.<name>` subjects, each labelled with its pod as `parent`. |
| `type: netdata` `config.url` | required | Base URL of a Netdata daemon to poll for live node metrics (e.g. `http://localhost:19999`). |
| `type: netdata` `config.include` | CPU, memory, I/O, network, disk | A list of `path.Match` patterns over Netdata chart contexts (`system.*`, `disk.io`); each selected chart's dimensions become properties named `<context>.<dimension>`. `*` selects every chart Netdata has. The default is `system.cpu,system.ram,system.io,system.net,disk.io,disk.util,disk.await,net.net,cpu.cpu`; pressure is left out because the cgroup collector reads it, and alone Netdata still supplies the pressure types. |
| `type: netdata` `config.collapse` | `true` | Also emit the node's own types (`cpu_utilization` and the rest) from Netdata's system charts. Set `false` when a cgroup entry is listed before it: the cgroup collector supplies those types, and Netdata measures CPU differently. |
| `type: script` `config.path` | required | Run the synthetic system from this scenario file. Exclusive: a list holding a `script` entry and any other is refused. `sources.interval` must equal the scenario's `tick_seconds` (all seed scenarios use 10s): the script stamps simulated time one tick per collect while the map sweeps on the wall clock, so the daemon refuses to start when they differ, as it does when the file cannot be loaded. |
| `node.id`          | `""`             | Identifier this agent puts on emitted `MetricSample`s and uses in event IDs. Empty falls back to `os.Hostname()`. |
| `map.proposer.enabled`         | `true`           | Enable `MICorrelationProposer` (Fisher z p-values; pairs any two properties — subject → node, node ↔ node, subject ↔ subject — within the pair budget). Set `false` on nodes where ring-buffer overhead is undesirable; the daemon falls back to `DisabledProposer` (no-op). |
| `map.proposer.threshold` | `0.85`         | `\|Pearson r\|` above which the proposer emits a candidate. |
| `map.proposer.min_pairs` | `30`           | Co-observations a pair needs inside the pair window before it can become a candidate — the pace at which structure appears. Each end must also take this many distinct readings (the readings off its commonest value, plus that value once), so a rate at zero but for a few shared spikes is not tested on its spikes. |
| `map.proposer.min_spread` | `0.01`        | Least movement, as a share of its declared range, each end of a pair must show over the window to become a candidate. Correlation is scale-free, so without it a series that barely moves can correlate perfectly. `0` disables it. |
| `map.proposer.fdr` | `0.05`               | False-discovery rate of the candidate bar: a pair becomes a candidate only when its p-value clears the Benjamini–Hochberg bar at this rate over every pair tested, so the more pairs are tested the more evidence a candidate needs. `0` disables the correction. |
| `map.proposer.max_pairs` | `0`            | How many property pairs discovery tests at once, across every pair class. `0` derives it from the machine at start: 1% of its memory and 1% of each collection tick's CPU, whichever allows fewer (4 096 when memory cannot be read). The resolved number is logged and reported in `GET /state` under `discovery`. |
| `map.tuner`            | `true`           | Enable `RuleBasedTuner`. Set `false` to disable operator tuning entirely; `POST /agent/tune` still accepts requests but returns empty adjustments. |
| `profile.regime`           | `""`             | Dynamics preset (`stable`/`default`/`bursty`/`volatile`). Overrides `profile.alpha` and `profile.convergence` when set. |
| `peers.urls`            | `[]`             | A list of peer agent URLs to register at startup. Additional peers can be added at runtime via `POST /peers`. |

#### Scenario file format

A scenario is JSON with a name, `seed`, `tick_seconds`, `duration_seconds`, optional `noise`, and three blocks:

| Block | Fields |
| --- | --- |
| `node` | one entry per node-level property, `{"coupling": "sum" \| "logistic" \| "none", "base", "of", "theta", "k"}`. `sum` is `base + Σ` over active subjects of their property `of`; `logistic` is `σ((x − theta)/k)` where `x` is the node property `of` (or, if `of` names a subject property, `base +` its sum); `none` is `base`, a property nothing drives. |
| `subjects` | `{"id": "<kind>:<identity>", "arrive", "depart", "return", "properties": {...}}`, times in seconds from the start; `depart` absent means never, `return` revives the same subject. Each property is `{"pattern": "constant" \| "ramp" \| "sine" \| "burst", "value", "min", "max", "period", "burst_start", "burst_duration", "unit", "range"}` — `constant` holds `value`; `ramp` goes `min → max` over `period` seconds then holds; `sine` oscillates between `min` and `max` with `period`; `burst` is `min` except `max` during `[burst_start, burst_start + burst_duration)`, repeating every `period` when it is positive. Schedules are relative to the subject's arrival or return. |
| `expect` | what the runner asserts: `admitted_within_ticks`, `stale_within_seconds`, `retired_within_seconds`; `candidates` (`{"from", "to", "sign", "within_seconds", "reproposed_after_return"}`), `no_candidates_from` (subject ids that must never be proposed), and `counterfactuals` (`{"target", "assume": {"<property>@<subject>": value}, "regime": "linear" \| "saturated", "tolerance", "min_error"}` — `linear` asserts the projection is within `tolerance` of the model's truth, `saturated` asserts it is at least `min_error` off and that the standing slope caveat is present). |

Property names are metric types (`[A-Za-z0-9._-]`); the runner addresses a subject's property as `<property>@<subject>`. The loader validates all of this and refuses a file it cannot run: every `of` must name an existing subject property (or, for `logistic`, a non-logistic node property), a `linear` counterfactual needs `tolerance > 0` and a `saturated` one `min_error > 0`, its target must be a node property and its `assume` keys must name existing subject properties. Past `duration_seconds` the script emits nothing and logs once that the scenario finished.

See `scenarios/` for five seed scenario files to drive a `script` collector with a known ground truth: `linear` and `saturation` (single-subject couplings, one that extrapolates and one that saturates), `confounded` (one subject drives a node property while another merely co-varies with it — the map proposes both, because correlation alone cannot separate them), `decoupled` (a correlated subject plus an uncorrelated noise subject) and `churn` (a subject that departs and returns, exercising staleness and retirement).

**State model** — the properties this system exhibits, their lifecycle, and what this agent holds about other nodes.

| Setting                    | Default | Meaning                                                                            |
| ------------------------ | ------- | ---------------------------------------------------------------------------------- |
| `map.stale_after`           | `2m`    | Silence after which a property is marked stale. Its last value is kept and labelled: a stale reading is evidence about the past, and silence is not evidence about the present. |
| `map.retire_after`          | `10m`   | Silence after which a property is retired automatically, cascading to relationships that reference it. The default, `10m`, is five default stale windows — long enough that a restarting collector is not mistaken for a departed subject — and does not follow `map.stale_after`. `0` leaves retirement to an operator. Retirement is soft. |
| `map.sweep_interval`        | `0s`    | How often lifecycle transitions are applied. `0` derives an interval from `map.stale_after`. |
| `map.admit`              | `true`  | Admit an observation of an undeclared metric as a new property and journal the admission. `false` refuses it; the default admits it, because a model that cannot represent something new describes the system as it was when someone wrote it down. With no domain model and no restored `persist.file`, nothing is declared, so the agent refuses to start rather than drop every sample. |
| `map.learn`              | `true`  | Learn each relationship from paired observations. `false` disables the paired estimator: every relationship then stays at `basis: unknown` with confidence 0 for the life of the process — an agent that can report levels and nothing about how they relate. |
| `map.pair_window`   | `15s`   | How far apart two observations may be and still count as simultaneous. Collectors sample on independent grids, so without a tolerance no pair ever forms. |
| `map.pair_support`          | `8`     | Pairs a relationship needs before its strength moves at all. Below it the pair is buffered and confidence keeps reporting that nothing has been learned. |
| `map.pair_history`          | `60`    | Recent pairs the estimate is computed over. Older pairs fall out, which is what lets a relationship follow a system whose behaviour changes rather than averaging its whole history. |
| `persist.file`            | `/var/lib/di-agent/state.json` | Path to persist the map and its journal. The agent checks at start that it can write there and refuses to start if not; `""` keeps everything in memory, so a restart returns the agent to cold start on a system it has already watched. A snapshot naming a different owner is refused. |
| `persist.interval`         | `1m`    | Snapshot cadence when `persist.file` is set. A snapshot is also written synchronously on shutdown, so this bounds what an unclean exit loses; a shutdown save that fails exits non-zero. |
| `map.journal_size`          | `0`     | Change and decision entries held in memory (`0` = default 2000). The journal reports how many it dropped, so an absence is not read as "nothing happened". |
| `facts[].kind`                 | `[{kind: host}]` | Sources of the machine's facts — OS, kernel, CPU model and cores, memory, swap, disk, boot time — read once at startup into the map, and carried by every `GET /state` and every review decision. `facts: []` reads none. A fact read again with a different value is journalled as `fact.changed`. |
| `facts[].root`            | `""`    | Where the machine's filesystem is mounted, for an agent in a container reading its host (such as `/host`). Empty reads the agent's own. |
| `node.ingest_scope`          | `self`  | `self` ingests only this machine's samples. `any` aggregates every machine's telemetry into one map — correct for replaying a testbed into a single daemon, wrong for a deployment. |
| `peers.state_poll`       | `30s`   | How often to fetch each peer's map. `0s` disables polling, leaving `GET /state/cluster` to report only what a manual refresh collected. |
| `peers.state_stale`      | `90s`   | How old a peer's snapshot may be before the cluster view marks it history. Nothing is deleted at this age: during a partition the last snapshot is all this agent has about that node. |

**Explain layer** (all optional; `POST /explain` returns 501 unless `explain.provider` is set). Full walkthrough in [§8](#8-natural-language-explain-explain).

| Setting                      | Default                          | Meaning                                                                 |
| -------------------------- | -------------------------------- | ----------------------------------------------------------------------- |
| `explain.provider`        | `none`                           | `none` (disabled) or `openai-compatible` (Ollama, llama-server, LM Studio, vLLM, hosted OpenAI). |
| `explain.url`             | `http://localhost:11434/v1`      | Base URL of the OpenAI-compatible backend.                              |
| `explain.model`           | `qwen2.5:7b-instruct`            | Model name passed to that backend.                                      |
| `explain.prompt`          | `pkg/agentmain/prompts/explain-v2.md`| Answering-agent system prompt. Required when the provider is enabled.   |
| `explain.planner_prompt`  | `<prompt-dir>/planner-v2.md`     | Planner prompt. A missing *derived* default disables the planning stage with a log line rather than failing startup. |
| `explain.critic_prompt`   | `<prompt-dir>/critic-v2.md`      | Critic prompt. Same degradation semantics as the planner prompt.        |
| `explain.keep_alive`      | `30m`                            | Passed to Ollama-style backends as `keep_alive` so the model stays resident between calls. Empty omits the field. |
| `explain.sessions`        | `true`                           | Enable multi-turn session memory and the session-scoped tool-result cache. |
| `explain.api_key`         | `""`                             | Bearer token for the backend. Write `${EXPLAIN_API_KEY}` to read it from the environment; the agent no longer reads that variable on its own. |

**Autonomous review** (off by default). Full walkthrough in [§9](#9-autonomous-candidate-review--autonomy).

| Setting                       | Default               | Meaning |
| --------------------------- | --------------------- | ------- |
| `groups.label_keys`                  | `[di-agent/group, app.kubernetes.io/part-of]` | Pod labels naming a pod's group, first set wins; a `kube-system` pod with none is in `kube-system`. Label your storage pods `di-agent/group: storage` and the map holds what the storage group uses. |
| `groups.units`                       | `{system: ["*"]}`     | Group name → globs of the units a cgroup collector's `config.units` models. CPU cores, disks and physical NICs are `hardware`, other virtual interfaces `network`, a `veth` is its pod's. |
| `groups.interval`                    | `30s`                 | How often the topology is recomputed; `0` turns grouping off. |
| `loops.review.level`                 | `full`                | `full` (3): the agent decides. The manual modes are `bounded` (2), `rules` (1), `suggest` (0), and `off`. |
| `loops.review.interval`        | `1m`                  | How often the reviewer looks at the queue. |
| `loops.review.per_cycle`       | `10`                  | Candidates reviewed per cycle, strongest first. |
| `loops.review.rules`           | `composition`         | Tier 0 rules, a list, first match decides; `[]` for none. |
| `loops.review.renderer`        | `text`                | How a candidate's evidence is written for the models. |
| `loops.review.decision.backend` | `systemone`          | Tier 1: `systemone` (Ollama decision models, `/v1/systemone`), `openai-logprobs` (any OpenAI-compatible server returning token log-probabilities: llama.cpp, vLLM, Ollama `/v1`), or `none` — then what rules cannot settle is undecided. |
| `loops.review.decision.url`    | `http://localhost:11434` | Ollama's root for `systemone`; the `/v1` base for `openai-logprobs`. |
| `loops.review.decision.model`  | `tev1:4b`             | Tier 1 model. `nimble-v3:9b-nvfp4` on an Apple-silicon machine with memory to spare. |
| `loops.review.decision.keep_alive` | `""`              | Ollama `keep_alive` for an `openai-logprobs` tier 1 model. |
| `loops.review.decision.reasoning_effort` | `none`      | `reasoning_effort` sent by `openai-logprobs`, so a thinking model answers directly; empty omits it, and a server that refuses it (vLLM accepts only `low`/`medium`/`high`) gets it dropped. |
| `loops.review.reasoning.backend` | `openai-tools`      | Tier 2: `openai-tools` (any OpenAI-compatible server with tool calling) or `none`. Unreachable, it skips only what needs it, each cycle. |
| `loops.review.decision.timeout`  | `5m`                | How long one tier-1 call may take. A decision model on the node's own CPUs scoring a candidate with tier 2's analysis takes minutes; a call past this fails and the candidate waits for the next cycle. |
| `loops.review.reasoning.timeout` | `10m`               | How long one tier-2 analysis may take, tool calls included. |
| `loops.review.reasoning.url`   | `http://localhost:11434/v1` | Tier 2 `/v1` base; it may be another machine. |
| `loops.review.reasoning.model` | `qwen3.8:27b-nvfp4`   | Tier 2 model. |
| `loops.review.reasoning.keep_alive` | `10m`            | Ollama `keep_alive` for the tier 2 model. |
| `loops.review.reasoning.tools` | `get_property,get_state,get_candidates` | Read-only tools the tier 2 model may call, a list. |
| `loops.review.thresholds.confirm`         | `0.85`                | Probability a model tier needs to confirm, and to orient an undirected candidate. |
| `loops.review.thresholds.reject`          | `0.95`                | Probability a model tier needs to reject. |
| `loops.review.thresholds.defer`           | `0.7`                 | Probability a model tier needs to defer. |

### Collector plugins and `mapctl collector check`

A collector in any language is an `exec` plugin (the agent starts it and speaks wire
protocol v1 on its stdin and stdout) or an `http` one (each request is a `POST` of the
same JSON). Two helpers write the loop for you:

| Language | Helper | Shape |
| -------- | ------ | ----- |
| Python | [`sdk/python/di_collector.py`](sdk/python/di_collector.py), stdlib only | `di_collector.serve(setup)`; `setup(hello)` returns `collect(now)`, which returns a list of samples or `(samples, available)`; tests in `sdk/python/test_di_collector.py` |
| Go | `pkg/collectors/plugin` | `plugin.Serve(func(plugin.Hello) (plugin.CollectFunc, error))`; a `CollectFunc` is `func(now time.Time) (samples, available, error)` |

[`examples/collectors/loadavg/`](examples/collectors/loadavg/) is a complete Python plugin. [`docs/collectors.md`](../docs/collectors.md) is the developer guide: the protocol in full,
the producer rules, ownership, security, and Go and shell examples.

A plugin is valid when it passes the collector compliance suite. `mapctl collector check`
runs that suite over it through the agent's own exec or http adapter, with no agent
running — including `NoMalformedSamples` and `NoDuplicateSamplesInBatch`, which fail on
the samples the adapter drops from a reply — plus the protocol checks `HelloRoundTrip`,
`SeqEcho`, `ReplyWithinTimeout`, `UnitRangeStableOverTicks`, `NoForeignNodeID`,
`TimestampNearNow`, `NoUnexpectedRestarts` and `AvailableCoversEmitted`:

```bash
cd semantic-map/go
go run ./cmd/mapctl collector check --exec ../examples/collectors/loadavg/loadavg.py
go run ./cmd/mapctl collector check --exec ./gpu --config gpu.yaml -- --device 0   # plugin arguments after --
go run ./cmd/mapctl collector check --http http://localhost:9100/collect
```

| Flag | Default | Meaning |
| ---- | ------- | ------- |
| `--exec CMD [-- ARGS…]` / `--http URL` | — | The plugin; exactly one. |
| `--config FILE` | none | A YAML mapping sent as the hello's `config`, as an entry's `config` block would be. |
| `--ticks N` | `5` | Collects per protocol check, and per run of the every-batch checks `NoMalformedSamples` and `NoDuplicateSamplesInBatch`. |
| `--interval D` | `10s` | The interval announced in hello, at least 1s. The timeout is the default an entry gets, `min(D/2, 5s)`; `UnitRangeStableOverTicks` spaces its collects `D/10` apart, at least 100 ms. |
| `--node-id ID` | `check` | The node id the agent side announces and stamps. |

It prints `PASS <check>` or `FAIL <check>: <why>` per check, then `certified` (exit 0) or
`not certified (k failed)` (exit 1). Every check starts the plugin afresh; its stderr
is shown. A plugin that does not complete its hello stops the run at `HelloRoundTrip`.
CI certifies the loadavg example this way on Linux.

---

## 4. Compliance Tests

Every contract has a compliance suite. A new implementation is valid if and only if it passes the suite for its contract.

The suites are Go, and they are the only definition of correctness there is. A parallel
set of Python suites over ABC mirrors of the same interfaces used to sit beside them,
described as the authoritative specification; since no Python implementation ever existed
to run them against, what they specified was never checked, and they drifted out of step
with the Go interfaces they mirrored (see §1).

```go
func TestMyOntologyCompliance(t *testing.T) {
    compliance.RunOntologyCompliance(t, func(t *testing.T) contracts.OntologyContract {
        return NewMyOntology(spec)
    })
}

func TestCgroupCollectorCompliance(t *testing.T) {
    compliance.RunCollectorCompliance(t, func(t *testing.T) contracts.CollectorContract {
        return NewCgroupCollector("node_1", "/sys/fs/cgroup")
    })
}
```

```bash
cd go && go test ./...
```

---

## 5. Structural Initialization

Run once to produce the artefact the daemon loads at startup. It declares **which
propositions the agent instantiates and in which direction** — and no magnitude for any
of them:

```bash
# from semantic-map/
python3 -m prior_init.pipeline --root ../../ --out prior_weights.json
```

Both arguments matter and both were wrong in this file until now. The module path is
`prior_init.pipeline` run from inside `semantic-map/`: the documented
`semantic_map.prior_init.pipeline` cannot resolve, because the directory is
`semantic-map` and a hyphen is not a valid Python module name. And `--root` points at the
**mega-research** root rather than at the di-agent checkout, because the pipeline reads
`energy-analysis/results/` and `overhead-decomposition/results/`, which are siblings of
`di-agent/` rather than children of it. Run as written above, the pipeline reproduces the
committed `prior_weights.json` byte for byte — propositions, per-KD edge weights,
construct scores, scope and warnings all identical — which is what makes it a replication
artefact rather than a checked-in number.

The pipeline reads publication constants for the constructs a running cluster exhibits
and writes:

- `agent_edges[]` — the structure of each relationship the agent instantiates:
  `from_id`, `to_id`, `proposition_id`, `direction`. **No strength.** Not per-distribution
  either, because which constructs relate and in which direction is a property of the
  domain rather than of a cluster.
- `propositions[P*]` — per-proposition *diagnostics*: `spearman_rho`, `p_value`,
  `sign_consistent`, `method`, and `would_have_been`, which records the magnitude the
  retired calibration would have emitted so that its exclusion is checkable from the
  artefact rather than only asserted in prose.
- `scope` — `telemetry_constructs`, `agent_propositions`, `excluded_propositions` with a
  reason each, `sign_overrides` with a reason each, and a `rationale`
- `distribution_construct_scores[kd][construct]` — restricted to the constructs in scope,
  informational only
- `constructs` and `warnings`

### Why there is no strength in it

The file used to emit `propositions[P*].prior_strength` and a
`distribution_edge_weights[kd][key].prior_weight` table, and the daemon seeded every
relationship from them, blending each into decisions with weight `(1 − confidence)` —
hardest, that is, exactly when the agent had least evidence to correct it. Three findings
retired that:

1. **The numbers were not measurements of an association.** Each came from a rank
   correlation between construct proxies across five distributions, and for both
   propositions that survive the scoping the pipeline reports `spearman_rho: 0.0`,
   `p_value: 1.0`, and its own *proxy reversal — investigate* warning. The per-cluster
   values came from min-max normalised construct scores over the same five distributions,
   so k0s sat at exactly 0.0 on the resource proxy and k8s at exactly 1.0 **by
   construction**, and a sixth distribution scoring below the current minimum would have
   rescaled every value including those of clusters whose measurements had not changed.
2. **A wrong magnitude is not inert.** Two of the four propositions the graph then carried
   asserted a direction the machine contradicted in 45% to 100% of paired observations.
   Their evidence gate never opened, so their learned strength stayed at zero — while
   their residual prior still entered the cost sum with the declared sign, and subtracted.
3. **The cold-start window they covered is about a minute** at 1 Hz, on an agent that runs
   for weeks.

An earlier version also calibrated all seven Di-Select constructs, including from a
third-party CIS scanner, a GitHub star count, and setup time in human hours. Those were
dropped first, on the same rule one level up: **if there is no measurement, do not
manufacture a score.** Dropping the magnitudes is that rule reaching the last place it had
not been applied.

Propositions are included only when *both* endpoints are constructs this deployment
measures **and** the telemetry does not contradict the declared direction — the same rule
the daemon's specification applies (ARCHITECTURE.md §2). What is declined and what is
overridden is recorded in `scope` rather than applied silently.

### Loading the artefact at startup

The daemon loads the file via `profile.priors`. `profile.kd` names the distribution running on this
node; it is validated against the file's `distributions` list and the daemon refuses to
start if the name is not there.

```bash
# agent.yaml: profile: {priors: /etc/semantic-map/prior_weights.json, kd: k0s}
agent -config agent.yaml
```

**Both agents built with different `profile.kd` values produce identical answers until telemetry
arrives**, which `TestScenario_PerKDAgentsAreIdenticalUntilTheyObserve` asserts directly.
That is the inversion of the old expectation — the scenario used to be called
`PerKDDecisionsDiffer` — and it is the point: nothing distinguishes two agents but what
they measure.

`TestBuildSeedsStructureAndNoMagnitude` asserts the other half for every distribution in
the file and every proposition in the map: structure arrives, `basis` is `unknown`, and
`established`, `assertion` and `effective` are all absent. It also greps the committed
artefact for `prior_strength`, `prior_weight` and `ema_weight` and fails if any is
present — because the artefact carried that table for a while after the daemon had
stopped reading it, and a magnitude nothing reads is worse than an absent one.

Re-run the pipeline if new empirical papers are incorporated, if the KD set
changes, or if `domain_spec.json` changes which propositions are in scope — the
file's `scope` block records what the current artefact covers.

---

## 6. Coordination

Multiple daemons can know about each other, query each other's `/cost`, and offload tasks based on trust-weighted savings. This is the multi-agent layer — the spine of the *decentralized* framework.

### `peers.urls`

Name the peer URLs in the spec at daemon start:

```yaml
# agent.yaml
peers:
  urls: [http://node_1:8080, http://node_2:8080]
```

Each URL is added to the in-memory peer registry with a default trust of `0.5`. The daemon logs `registered N peers: …` on startup. Additional peers can be added at runtime via `POST /peers`.

### HTTP surface

| Verb | Path | Body / Params | Response |
| --- | --- | --- | --- |
| `GET` | `/peers` | — | `[]PeerDTO{id,url,trust,n_observed,last_seen,note}` |
| `POST` | `/peers` | `{url,note?}` | `200 PeerDTO` (idempotent on URL) |
| `DELETE` | `/peers/{id}` | — | `204` |
| `POST` | `/peers/{id}/trust` | `{value}` | `204` (manual operator override) |
| `POST` | `/offload` | `{task_type,source_node_id,data_size_bytes,latency_budget_ms,energy_budget_joules?}` | `200 {accepted,reason,expected_latency,expected_energy}` |

`/offload` is the peer side of the protocol: it runs `CostOfAction` on the local graph and accepts when the result fits the requested budgets. It does not actually schedule any work — that is a P7 (execution) concern. The expected latency/energy in the response let the source agent record an outcome and adjust trust.

These three endpoints decide *where work goes*. What a peer knows about its own system travels over `GET /state` and is held apart from local state — see [Asking other nodes](#asking-other-nodes).

### `mapctl peers`

```bash
go run ./cmd/mapctl peers list                      # table view
go run ./cmd/mapctl peers add http://node_1:8080 --note "rpi-1"
go run ./cmd/mapctl peers list
# ┌──────────────┬──────────────────────┬───────┬───┬──────────┬───────┐
# │      ID      │         URL          │ Trust │ N │ LastSeen │  Note │
# ├──────────────┼──────────────────────┼───────┼───┼──────────┼───────┤
# │ a1b2c3d4e5f6 │ http://node_1:8080   │ 0.500 │ 0 │ never    │ rpi-1 │
# └──────────────┴──────────────────────┴───────┴───┴──────────┴───────┘

go run ./cmd/mapctl peers trust a1b2c3d4e5f6 0.9    # manual override
go run ./cmd/mapctl peers remove a1b2c3d4e5f6
```

### How `RecommendPeer` uses peers

For every call, the reasoner:

1. Lists peers from the registry. Empty → `ErrInsufficientTrust` ("no peers registered").
2. Skips any peer below `--min-trust` (default `0.5`).
3. For each survivor: `GET /cost` on the peer URL; on success `MarkSeen` + record `savings = myEnergy − peerEnergy`; on failure log + nudge trust down by `0.05` (clamped at 0).
4. Picks the peer maximizing `savings × peer.Trust` — trust-weighted savings.
5. If no peer beats local cost → `ErrInsufficientTrust` with rationale.

Trust dynamics in v1 are simple: default `0.5` on registration; manual override via `POST /peers/{id}/trust`; automatic `-0.05` penalty on HTTP failure. Richer schemes (per-outcome updates after `/offload`, decay over time, signed identities) are deferred.

### Demo scenario

`TestScenario_CoordinationOffload` wires three in-process agents (A idle, B loaded, C medium), cross-registers them, has B call `RecommendPeer`, and asserts A wins. Run with:

```bash
go test -v -run TestScenario_CoordinationOffload ./internal/minimal/tests/...
```

The verbose output narrates pre-flight self-costs, the peer query, the rationale, the offload acceptance, and the trust update.

### v1 scope and security

* No auth on `/peers` or `/offload` yet. Intended for localhost / lab-network deployment. Production hardening (mTLS, bearer tokens, signed peer identities) is a P7 concern.
* `peers.Registry` and `peers.Client` are concrete types in `pkg/peers/`, **not** a seventh contract. The contract surface stays at six (Collector, Ontology, Reasoner, Proposer, Tuner, Runtime). We promote when a second implementation arrives (e.g. SQLite-backed registry, gossip discovery).

---

## 7. PoC — Live Multi-VM Demo

`poc/` provisions three local VMs (via [Multipass](https://multipass.run/)) and runs the full di-agent stack on each — k0s, Netdata, and di-agent — then demonstrates trust-weighted peer routing under real CPU load. This is the executable proof of the P7 claim.

### Prerequisites

```bash
brew install --cask multipass   # Apple Silicon macOS
```

### Topology

| VM | k0s role | di-agent regime | Workload |
|----|----------|-----------------|---------|
| `diag-1` | single-node | `bursty` (α=0.30, N=200) | heavy (`stress-ng`) |
| `diag-2` | single-node | `stable` (α=0.05, N=1000) | light |
| `diag-3` | single-node | `stable` (α=0.05, N=1000) | idle |

Full peer mesh: each agent registers the other two at trust=0.8.

### Commands

```bash
# One-time setup (~15 min)
make -C poc all

# Apply heavy load to diag-1
make -C poc workload-heavy

# 8-round coordinator demo: cost table → /recommend → trust drain → routing flip
make -C poc demo

# Snapshot /cost from all three agents
make -C poc status

# Remove all VMs
make -C poc teardown
```

### What the demo shows

1. diag-1 under `stress-ng` → the RC level rises and both endpoints vary, so the RC-adjacent relationship starts accumulating pairs → `ResourceCost` rises with the level and confidence climbs.
2. `POST /recommend` on diag-1 → diag-2 recommended (lower cost, trust=0.8, highest trust-weighted savings).
3. Round 4: diag-2 trust set to 0.15 via `POST /peers/diag-2/trust {"value":0.15}` — below min-trust floor 0.5.
4. Rounds 5–8: recommendation flips to diag-3. Trust-weighted routing confirmed on live VMs.

See [ARCHITECTURE.md §12](ARCHITECTURE.md#12-poc-deployment-poc) for the full design rationale.

---

## 8. Natural-Language Explain (`/explain`)

The daemon ships a natural-language operator interface at `POST /explain`. It reads the live graph, calls tools to answer, and returns a **grounded, cited** English response with a deterministic citation validator behind it. Full design in [ARCHITECTURE.md §13](ARCHITECTURE.md#13-natural-language-explain-layer-pkgexplain).

### Enable it

The default is off (`explain.provider: none`), so `POST /explain` returns 501 with instructions until you opt in. To enable, point the daemon at a local OpenAI-compatible backend — Ollama is the smallest starting point:

```bash
# 1. Install and start a local LLM (out of band, one-time)
brew install ollama
ollama serve &                       # runs on :11434 by default
ollama pull qwen2.5:7b-instruct

# 2. Start the daemon with explain.provider: openai-compatible
cd semantic-map/go
cat > /tmp/agent.yaml <<'SPEC'
explain:
  provider: openai-compatible
  url: http://localhost:11434/v1
  model: qwen2.5:7b-instruct
  prompt: pkg/agentmain/prompts/explain-v2.md
persist:
  file: ""
SPEC
go run ./cmd/agent -config /tmp/agent.yaml &

# 3. Ask a question
curl -s -X POST http://localhost:8080/explain \
    -H 'Content-Type: application/json' \
    -d '{"question":"Which edges are driving my ResourceCost?"}' | jq
```

Any backend that speaks the OpenAI `/v1/chat/completions` surface works — `llama-server`, LM Studio, vLLM, or the hosted OpenAI API. Point `explain.url` at its base and set the model name.

### What you get back

```json
{
  "answer": "P10 (PS→RC, direction +) dominates with effective 0.62 from its recent estimate, followed by P3 (RC→PS, direction +) at effective 0.30. Both have 15 observations at confidence 0.03.",
  "citations": [
    {"kind": "edge", "id": "P10", "ema_weight": 0.62, "effective": 0.62, "basis": "recent", "confidence": 0.03, "n_observations": 15},
    {"kind": "edge", "id": "P3",  "ema_weight": 0.30, "effective": 0.30, "basis": "recent", "confidence": 0.03, "n_observations": 15}
  ],
  "confidence": "high",
  "proposal": null,
  "tool_trace": [
    {"name": "get_cost",  "arguments": {"node_id":"master","task_type":"pod-scheduling"}, "result_digest": "cost node=master task=pod-scheduling rc=0.6912 conf=0.030"},
    {"name": "get_edges", "arguments": {"to":"RC"}, "result_digest": "edges from= to=RC count=5"}
  ],
  "model_name": "qwen2.5:7b-instruct",
  "prompt_version": "a1b2c3d4e5f6",
  "iterations": 1
}
```

If the LLM asks *"should I…?"* it can also return a `proposal` — a suggested action + endpoint + payload the operator can invoke explicitly. The Explain layer itself **never mutates state**; every action stays a draft.

### What the layer guarantees

- **Groundedness.** Every value the LLM cites is checked against live graph state before the answer leaves the daemon. Fabricated proposition IDs, wrong EMA values, and references to deprecated edges are rejected — the LLM gets a critique and up to two more attempts. See [ARCHITECTURE.md §13](ARCHITECTURE.md#13-natural-language-explain-layer-pkgexplain) for the reflection-loop shape.
- **Read-only.** The LLM's tool set is `get_cost`, `get_edges`, `get_history`, `get_peers`, `get_recommend`, `get_graph`. No mutation tool exists in the registry; the LLM literally cannot call `POST /agent/tune` or `POST /ontology/deprecate`. That preserves the human-judgment anchor from [ARCHITECTURE.md §8](ARCHITECTURE.md#8-connection-to-research).
- **Determinism where it counts.** The reasoner and the state model stay pure Go, and the initialization pipeline is a build-time Python step that touches no node. Explain is on the operator-facing surface, not on the ingestion or reasoning path. All P6 results reported in `research-docs/` use `explain.provider: none`.

### v2 features (opt-in per request)

Everything below is off unless you ask for it. A v1-shaped body still behaves exactly as v1. Design rationale in [ARCHITECTURE.md §14](ARCHITECTURE.md#14-explain-v2--planning-critic-sessions-streaming).

**Planning** — a dedicated planner turn writes a structured plan; Go executes exactly the tools it names.

```bash
curl -s -X POST localhost:8080/explain -H 'Content-Type: application/json' \
  -d '{"question":"Why is my ResourceCost high?","use_planner":true}' | jq '.plan'
```

**Critic** — a second LLM reviews the answer for semantic errors the deterministic validator cannot see (wrong causal reading, direction-sign mistakes, miscalibrated confidence).

```bash
curl -s -X POST localhost:8080/explain -H 'Content-Type: application/json' \
  -d '{"question":"Which edge dominates?","use_critic":true}' | jq '.critic_verdict'
```

**Sessions** — multi-turn conversation plus a session-scoped tool-result cache.

```bash
# Turn 1 — omit session_id to mint one
SID=$(curl -s -X POST localhost:8080/explain -H 'Content-Type: application/json' \
      -d '{"question":"What drives my cost?"}' | jq -r '.session_id')

# Turn 2 — follow up without restating context
curl -s -X POST localhost:8080/explain -H 'Content-Type: application/json' \
  -d "{\"question\":\"And how do my peers compare?\",\"session_id\":\"$SID\"}" | jq -r '.answer'
```

**Streaming** — NDJSON progress events instead of a blocking call.

```bash
curl -N -s -X POST localhost:8080/explain -H 'Content-Type: application/json' \
  -d '{"question":"Why?","use_planner":true,"use_critic":true,"stream":true}' \
  | jq -c '{event, tool, iteration}'
```

Every stream ends in exactly one `final` or `error` event — loop until you see either.

**All four together:**

```bash
curl -N -s -X POST localhost:8080/explain -H 'Content-Type: application/json' \
  -d '{"question":"Should I deprecate P7?","use_planner":true,"use_critic":true,"stream":true}'
```

### Cost accounting

Every response carries a `usage` block so you can see what each feature costs:

```json
{"prompt_tokens":2481,"completion_tokens":193,"total_tokens":2674,
 "wall_clock_ms":4120,"tool_calls":3,"tool_cache_hits":1,"llm_turns":3}
```

`llm_turns` counts planner, answering, critic, and revision turns separately.

### v2 settings

```yaml
explain:
  planner_prompt: ""   # path to the planner prompt; empty is <dir of explain.prompt>/planner-v2.md
  critic_prompt: ""    # path to the critic prompt; empty is <dir of explain.prompt>/critic-v2.md
  keep_alive: 30m      # model residency hint: a big latency win on repeat calls
  sessions: true       # session memory
```

A missing planner or critic prompt disables that stage with a log line rather than failing startup.

---

## 9. Autonomous Candidate Review (`loops.review.level`)

The agent reviews the proposer's candidates itself: `loops.review.level` defaults to `full`, with a
decision model and a reasoning model, and every review is a decision record. `suggest` and `off` are
the manual modes, for an operator who wants to decide.

```bash
# A node (8 GB): a decision model only; what it is unsure of waits for its evidence to move.
# agent.yaml: loops.review: {reasoning: {backend: none}}
ollama pull tev1:4b
./agent -config agent.yaml

# A machine with memory to spare: a stronger decision model and a reasoning tier.
# agent.yaml: loops.review: {decision: {model: "nimble-v3:9b-nvfp4"}}  (the reasoning tier is on by default)
ollama pull nimble-v3:9b-nvfp4 && ollama pull qwen3.8:27b-nvfp4
./agent -config agent.yaml

# Any OpenAI-compatible server instead of Ollama (llama.cpp's llama-server, vLLM):
# agent.yaml: loops.review: {decision: {backend: openai-logprobs, url: "http://localhost:8081/v1", model: <model>}}
./agent -config agent.yaml

curl -s localhost:8080/autonomy
curl -s 'localhost:8080/candidates?status=all' | jq '.[] | {CandidateID, Status, Reason, Review}'
curl -s localhost:8080/state/decisions/<Review.DecisionID>
```

A candidate goes up the tiers only as far as it has to:

| Tier | Decider | What it does |
| ---- | ------- | ------------ |
| 0 | rules | A `composition` candidate is confirmed: a part against the whole it is summed into is true by definition, and `without=` answers from it. |
| 1 | decision model | One forward pass: a probability for confirm, reject and defer, and for an undirected candidate which end drives the other. The top action is taken when it reaches its threshold. `systemone` asks an Ollama decision model's typed-question endpoint; `openai-logprobs` asks a general model for one lettered answer and reads the distribution off the letters' log-probabilities, undecided when less than half the model's probability fell on any letter. |
| 2 | reasoning model | Reads the map with read-only tools (`get_property`, `get_state`, `get_candidates`) and writes an analysis. The decision model scores the candidate again with the analysis, and that probability is the one compared with the threshold. |
| — | undecided | What no tier was sure of stays in `GET /candidates` with `Review.Action: "undecided"`, and is reviewed again when its evidence moves. A reasoning or decision model that cannot be reached skips only what needs it, for that cycle. |

The level decides who may act:

| Level | Rules act | Model tiers act |
| ----- | --------- | --------------- |
| `suggest` (0) | no | no — every review is recorded as a suggestion (`Review.Applied: false`) |
| `rules` (1) | yes | no |
| `bounded` (2) | yes | confirm and defer; a model's rejection is left undecided |
| `full` (3), the default | yes | yes |

An agent's rejection or deferral is not final: the pair is still tested, and the candidate
returns to pending, to be reviewed again, once its evidence moves. A reviewed candidate is
not reviewed again until then, so the models are not asked the same question every cycle.
A candidate whose end is stale waits. The decision record holds the candidate, both
properties as read, the text each model was given, each model's answer, the tool calls,
and the level and thresholds in force. `GET /state` carries the same `autonomy` status, so
a reader of the map can see the level it was shaped under.

Every part is chosen by name — the rules, the evidence renderer, each tier's backend, URL
and model, and the reasoning tools — from registries in `pkg/autonomy`. Each tier has its
own URL, so the decision model can run on the node and the reasoning model elsewhere. A
new backend, rule, tool or renderer is a type implementing `Decider`, `Reasoner`, `Rule`,
`Tool` or `Renderer`, registered under a name in an `init` function; nothing else changes.
Bearer tokens for a backend are `loops.review.decision.api_key` and
`loops.review.reasoning.api_key`; write them as `${VAR}` to read them from the
environment and keep them out of the file. The agent reads no variable on its own.

The thresholds are uncalibrated defaults set by what each action costs to undo.

