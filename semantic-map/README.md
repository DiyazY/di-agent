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

---

## Quick Start

Five commands to see the live ontology end-to-end. Requires Go 1.22+. See [ARCHITECTURE.md §9](ARCHITECTURE.md#9-control-surface) for the design.

```bash
# 1. Start the daemon (edge-minimal profile, in-memory).
# -domain is required: the binary embeds no domain model.
cd semantic-map/go
go run ./cmd/agent -profile edge-minimal -addr :8080 -domain ../domain_spec.json &
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
# 4. Open the embedded viewer (Cytoscape.js)
open http://localhost:8080/ui/            # macOS; xdg-open on Linux
# → click an edge → side panel populates → Deprecate / Set strength / Reset
```

```bash
# 5. Tear down
kill %1
```

Three surfaces, one daemon: `curl` for inspection, `mapctl` for scripts and headless ops, the browser at `/ui/` for click-to-mutate demos. All three speak the same JSON HTTP API.

---

## 1. Project Structure

```
semantic-map/
│
├── domain_spec.json            THE DOMAIN MODEL — constructs, propositions, metric routing,
│                               adjustment policy, tuner intent rules. The daemon loads this
│                               at startup (-domain) and refuses to run without it; no
│                               construct or proposition identifier exists in any binary.
│
│  ── Python layer (the calibration pipeline — a build-time step) ──
│
├── prior_weights.json          Init-pipeline output: which propositions the agent carries, which
│                               it declines, and where it overrides a declared direction. No strengths.
├── requirements.txt            scipy (spearmanr); the pipeline's only outside dependency
│
├── prior_init/                 Structural initialization pipeline (Step 4)
│   ├── pipeline.py             Entry point — reads P1–P5 constants, writes prior_weights.json
│   ├── calibration.py          Construct scoring + per-proposition diagnostics (no strengths)
│   ├── constants.py            Publication constants (J/pod, mJ/op, CIS scores, …)
│   └── loaders.py              CSV / result file loaders
│
│  ── Go layer (edge daemon) ──────────────────────────────────────
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
    │   ├── peers/              Multi-agent coordination (concrete in v1, NOT a contract)
    │   │   ├── peers.go        Registry + Descriptor + Client (HTTP /cost, /healthz, /offload)
    │   │   └── peers_test.go   Registry + httptest client coverage
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
    │   │   ├── persist.go      Versioned, owner-stamped snapshots (atomic write, refuses foreign)
    │   │   └── peer.go         PeerStore — other agents' maps, labelled and never merged
    │   └── profiles/           Profile factory
    │       ├── profiles.go     Build("edge-minimal", cfg) + ontology seeding + peer wire-up
    │       └── state_seed.go   SeedStateMap — spec metrics/constructs → properties; structure only
    │
    ├── internal/               Implementation packages — not importable externally
    │   ├── scripted/           Synthetic collectors for demos, scenarios and tests
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
    │       ├── collector_netdata.go  NetdataCollector  (Netdata HTTP API v1, system.cpu/ram/net)
    │       ├── multi_collector.go    MultiCollector    (fan-out to N collectors)
    │       ├── ontology.go     SpecOntology           (the declaration layer: constructs and
    │       │                                           propositions from domain_spec.json. No
    │       │                                           strengths, no history — both live on the
    │       │                                           state model)
    │       ├── reasoner.go     RuleEngineReasoner     (deterministic, blended, skips retired)
    │       ├── proposer.go     DisabledProposer       (no-op)
    │       ├── proposer_mi.go  MICorrelationProposer  (Pearson r + Fisher z p-values; default via -proposer=true)
    │       ├── tuner.go        RuleBasedTuner + DisabledTuner (intent rules read from the spec;
    │       │                                           default via -tuner=true)
    │       └── tests/
    │           ├── compliance_test.go   Runs all Go compliance suites
    │           └── scenarios_test.go    End-to-end narrated scenarios (ColdStart, Convergence,
    │                                    PerKDDecisionsDiffer, DeprecationShrinksGraph,
    │                                    IdempotentReplay, AuditTrailRecordsEverything,
    │                                    CoordinationOffload — 3-agent multi-agent demo,
    │                                    ProposerNaturalDiscovery, OperatorTuneAndAuditTrail)
    │
    ├── compliance/             Go compliance test suites — one per contract
    │   ├── collector.go        RunCollectorCompliance(t, factory)
    │   ├── ontology.go         RunOntologyCompliance(t, factory)  — vocabulary + validation
    │   ├── reasoner.go         RunReasonerCompliance(t, factory)
    │   ├── proposer.go         RunProposerCompliance(t, factory)
    │   └── tuner.go            RunTunerCompliance(t, factory)
    │
    ├── pkg/profiles/
    │   ├── build_priors_test.go  Asserts structure arrives and no magnitude does, through Build
    │   └── profiles_test.go      KD validation + prior_weights.json discovery
    │
    ├── cmd/agent/              Daemon binary
    │   ├── main.go             Flag parsing + profile build + ListenAndServe
    │   ├── routes.go           registerRoutes + writeError + requireJSON (CSRF guard)
    │   ├── state_routes.go     /state* — query, lifecycle, journal, decisions, estimate
    │   ├── peer_state_routes.go  /state/cluster, /state/peers, /state/where + the peer poll loop
    │   ├── dto.go              Named JSON DTOs (Direction serialized as "+"/"-")
    │   ├── static.go           //go:embed all:static + staticHandler()
    │   ├── routes_test.go      HTTP integration tests via httptest.NewServer
    │   ├── explain_route_test.go  POST /explain: 501/400/200 + NDJSON streaming
    │   ├── prompts/            Versioned LLM system prompts (loaded at startup)
    │   │   ├── explain-v1.md   Answering agent — graph semantics, tools, response schema
    │   │   ├── planner-v1.md   Planner agent — planning rules + worked examples
    │   │   └── critic-v1.md    Critic agent — what Gate 1 already covers, what only it can catch
    │   └── static/             Embedded web UI assets
    │       ├── index.html      Cytoscape mount + side panel + <dialog> modal + toast region
    │       ├── app.js          Vanilla-JS controller; fetches /graph; POSTs mutations
    │       └── style.css       Edge color by direction, opacity by confidence, dashed when deprecated
    │
    ├── cmd/mapctl/             CLI binary — cobra + tablewriter; speaks the daemon's HTTP API
    │   ├── main.go             cmd.Execute()
    │   ├── cmd/                One file per subcommand (graph, edges, history, strength,
    │   │                       deprecate, construct, proposition, reset, candidates,
    │   │                       recommend, simulate, watch, dot, health, peers,
    │   │                       version, completion)
    │   ├── client/             HTTP client + DTOs duplicated (NOT imported) from cmd/agent
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
        │   ├── divergence.go   effective (assertion|established|recent); Range,
        │   │                   sample StdDev, sorted by Range desc (most
        │   │                   discriminative first)
        │   ├── output.go       Table / JSON / CSV formatters
        │   ├── runner_test.go  Two synthesized "KDs" diverge as expected; 5-run
        │   │                   averaging differs from single-run snapshot
        │   └── divergence_test.go  Range/StdDev arithmetic, sort order, formula
        └── client/             POST /ingest-sample wrapper + DTOs duplicated
            └── client.go        from cmd/agent (same wire-boundary discipline as mapctl)
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

Four more fields are optional and let a producer describe what it is reporting:

| Field     | Type          | Meaning                                                                 |
| --------- | ------------- | ------------------------------------------------------------------------ |
| `subject` | `str`         | Empty for a node-level reading; `<kind>:<identity>` (e.g. `pod:1234`) to scope it to something other than the node itself |
| `unit`    | `str`         | The unit the value is expressed in                                       |
| `range`   | `[lo, hi]`    | The value's declared bounds                                              |
| `source`  | `str`         | Free-form identifier for the producer, distinct from `node_id`           |

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
_ = c.Push(ctx, client.Metric{Type: "queue_depth", Unit: "items", Range: [2]float64{0, 100}}, float64(len(queue)), time.Now(), nil)
```

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

### `GET /candidates`

Lists Proposer candidate edges pending review.

```json
[{"candidate_id":"cand-001","from_id":"CO","to_id":"PS","direction":1,
  "mi_score":0.73,"p_value":0.002,"n_observations":1240,"deployments_seen":2,"status":0}]
```

Review via `POST /candidates/{id}/confirm`, `/reject`, or `/defer`.

### Full endpoint table

The five summaries above are the original control-plane queries. Phase 1 of the control-surface work added 14 endpoints for graph introspection, ontology mutations, candidate review, meta probes, and the embedded UI. All Phase 1 endpoints return JSON on both success and failure; mutation POSTs require `Content-Type: application/json` (lightweight CSRF mitigation — see [ARCHITECTURE.md §9](ARCHITECTURE.md#9-control-surface)).

| Verb | Path                                | Body / params                                            | Since    |
| ---- | ----------------------------------- | -------------------------------------------------------- | -------- |
| POST | `/ingest-sample`                    | `MetricSampleRequest`                                    | replay   |
| GET  | `/cost`                             | `?task=&node=`                                           | existing |
| POST | `/recommend`                        | `OffloadContext`                                         | existing | `409` when no peer qualifies or none are registered — an ordinary state, not a fault |
| POST | `/simulate`                         | `{context, target_node_id}`                              | existing |
| GET  | `/candidates`                       | —                                                        | existing |
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
| POST | `/candidates/{id}/confirm`          | path only                                                | Phase 1  |
| POST | `/candidates/{id}/reject`           | path only                                                | Phase 1  |
| POST | `/candidates/{id}/defer`            | path only                                                | Phase 1  |
| GET  | `/peers`                            | —                                                        | Step 4.9 |
| POST | `/peers`                            | `{url, note?}`                                           | Step 4.9 |
| DELETE | `/peers/{id}`                     | path only                                                | Step 4.9 |
| POST | `/peers/{id}/trust`                 | `{value}`                                                | Step 4.9 |
| POST | `/offload`                          | `OffloadHTTPRequest`                                     | Step 4.9 |
| POST | `/explain`                          | `{question, session_id?, use_planner?, use_critic?, stream?, max_iterations?, max_tool_calls?}` | Explain v1/v2 |
| GET  | `/ui/...`                           | —                                                        | Phase 2B |
| GET  | `/state`                            | `?kind=&status=&min-confidence=&related-to=&id=`         | State model |
| GET  | `/state/properties/{id}`            | path only; `Accept: text/plain` renders the neighbourhood | State model |
| POST | `/state/properties`                 | `Property`                                               | State model |
| DELETE | `/state/properties/{id}`          | `?reason=&actor=` — `reason` required                    | State model |
| GET  | `/state/relationships`              | `?from=&to=`                                             | State model |
| POST | `/state/relationships`              | `Relationship`                                           | State model |
| DELETE | `/state/relationships/{id}`       | `?reason=&actor=`                                        | State model |
| POST | `/state/relationships/{id}/strength` | `{strength, actor?, reason}` — `reason` required        | State model |
| GET  | `/state/estimate`                   | `?target=[&id=][&assume=<property>=<value>]*[&without=<subject|property>]*` — answer from the map, optionally under hypotheses; `id` names the decision (default `est-<revision>-<target>`); returns a decision id | State model |
| GET  | `/state/journal`                    | `?since=&limit=`                                         | State model |
| GET  | `/state/decisions`                  | `?limit=`                                                | State model |
| GET  | `/state/decisions/{id}`             | path only — `410` when the journal has dropped it        | State model |
| POST | `/state/sweep`                      | —                                                        | State model |
| GET  | `/state/cluster`                    | — this node and every peer, unmerged                     | Peer state |
| GET  | `/state/peers`                      | — per-peer owner, age, census, last error                | Peer state |
| GET  | `/state/peers/{id}`                 | path only                                                | Peer state |
| POST | `/state/peers/refresh`              | — poll every peer now                                    | Peer state |
| GET  | `/state/where`                      | `?property=` — per-node answers, deliberately not averaged | Peer state |

`POST /explain` returns `200` with an `ExplainResponse`, `422` with `{error, response}` when the answer failed a gate after `max_iterations`, `501` when `-explain-provider=none`, and `application/x-ndjson` instead of JSON when `stream:true`. See [§8](#8-natural-language-explain-explain).

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
GOOS=linux GOARCH=arm64 go build -o agent-arm64 ./cmd/agent

# Deploy
scp agent-arm64 pi@192.168.1.x:/usr/local/bin/agent

# Run on RPi4
agent -profile edge-minimal -addr :8080 -alpha 0.2 -convergence 500 \
  -domain /etc/semantic-map/domain_spec.json \
  -priors /etc/semantic-map/prior_weights.json -kd k0s

# Run locally (development)
go run ./cmd/agent -profile edge-minimal -domain ../domain_spec.json
```

### The state model

The map holds what the system exhibits and how those properties relate, updated by
every sample, queryable and traceable.

```bash
# what is this system doing right now
curl -s localhost:8080/state | jq '.counts'

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
  | jq '.relationships[] | select(.sign_suspect) | {label, from, to, sign, strength, sign_conflicts, sign_agreements}'
```

**The three strength layers.** A relationship reports `recent` (the fast EMA, as
`ema_weight`), `established` (the long-run learned baseline), `assertion` (an operator's
override), the `effective` value the agent reasons with, and `basis` naming which of the
three that came from. `effective` and the two pointer layers are **absent rather than
zero** when they do not exist, because zero is the claim that a relationship is worth
nothing and absence is not that claim.

```bash
# what each relationship is worth, and on what basis
curl -s localhost:8080/edges \
  | jq '.[] | {proposition_id, basis, effective, established, assertion, ema_weight, n_observations}'

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
marks a property stale (`-stale-after`) and optionally retires it (`-retire-after`);
`-no-admit` closes the model for a deployment that wants that.

### One agent per machine

The map is node-local: each agent models the machine it runs on, and its graph holds
that machine's evidence. `-node-id` is its identity (falling back to the hostname),
and the default `-ingest-scope=self` rejects telemetry labelled with another
machine's ID rather than folding it in. Every map is stamped with its owner, so a
property is attributable once state crosses a node boundary, and a snapshot copied
from another host is refused instead of adopted as local history.

```bash
# A deployment: one agent per node, modelling that node
agent -profile edge-minimal -domain ../domain_spec.json -node-id node_1

# Replaying a whole testbed into one daemon — an aggregate, not a deployment
agent -profile edge-minimal -domain ../domain_spec.json -ingest-scope=any
```

`-ingest-scope=any` exists for exactly that replay case and logs a startup warning:
the resulting edge weights are means over machines that may be different physical
systems. `GET /cost?node=X` returns 409 when X is not this agent, pointing at that
machine's own endpoint when it is a known peer.

### Asking other nodes

A node-local map means any question wider than one machine is a question for other
agents. Each agent fetches its peers' maps (`-peer-state-poll`, default 30s) and holds
them under the owner each peer reported.

```bash
agent -domain ../domain_spec.json -node-id node_1 -peers http://node_2:8080

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
view. And a snapshot older than `-peer-state-stale` (default 90s) is labelled history
rather than withheld.

### How relationships learn

A relationship advances on a *pair*: both endpoints observed within
`-pair-window-seconds` (default 15s). What it learns is `|r|`, the correlation
magnitude over the last `-pair-history` pairs, on `[0, 1]`. It is folded twice from the
same pairs: a **recent** estimate at `-alpha` (default 0.20, memory ≈ 5 pairs) and an
**established** baseline at `-alpha-slow` (default 0.001, memory ≈ 1000 pairs,
bias-corrected). `Effective()` picks between them by authority rather than by arithmetic
— an operator assertion first, else the established baseline, else the recent estimate,
else `unknown` — and `basis` on every edge names which one answered.

```bash
agent -profile edge-minimal -domain ../domain_spec.json \
  -priors ../prior_weights.json -kd k0s \
  -pair-window-seconds 15 -pair-support 8 -pair-history 60
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

`-pair-window-seconds` is a tolerance, not smoothing: collectors sample on independent
grids (`system.*` on 0, 5, 10 …, PSI on 2, 6, 12 …), so without it no pair ever forms.
`-pair-support` is how many pairs a relationship needs before its strength moves at all
— below that, `n_observations` stays at zero, because confidence has to keep reporting
that nothing has been learned. There was a `-relational` flag here selecting between
this estimator and one that moved an edge on any single sample; only this reading was
defensible, so the choice went. See ARCHITECTURE.md §5 for what it does and does not
claim.

`-domain` is mandatory. The daemon carries no constructs, propositions, metric
routes, adjustment bounds or tuner keywords of its own, so without a
specification there is no graph to serve and it exits with
`no domain spec: pass -domain <path> to load one` rather than starting on a
built-in default. Deploy the specification alongside the binary and treat it as
part of the release: two daemons on the same binary and different specifications
are different agents. The daemon also rejects positional arguments, because Go's
`flag` package stops parsing at the first one — `-proposer false` (a space
instead of `=`) silently discarded every flag after it and cost this project a
full round of invalid measurements.

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

| Flag                | Default          | Description                                                              |
| ------------------- | ---------------- | ------------------------------------------------------------------------ |
| `-profile`          | `edge-minimal`   | Deployment profile name                                                  |
| `-addr`             | `:8080`          | HTTP listen address                                                      |
| `-alpha`            | `0.2`            | EMA decay factor                                                         |
| `-convergence`      | `500`            | Observations until confidence = 1.0                                      |
| `-min-trust`        | `0.5`            | Minimum peer trust score for offloading                                  |
| `-priors`           | `""`             | Path to `prior_weights.json` from the initialization pipeline. Supplies structure only — which propositions to instantiate and in which direction |
| `-kd`               | `""`             | KD running on this node (`k3s`/`k0s`/`k8s`/`kubeEdge`/`openYurt`). Validated against the artefact's `distributions` list; the daemon refuses to start on a name that is not there. It no longer selects a magnitude — two agents differing only in `-kd` answer identically until telemetry arrives. |
| `-collect-interval` | `10s`            | How often the autonomous collection loop ticks the profile's collector. Set to `0` to disable the loop (only manual `POST /ingest-sample` then updates the model). |
| `-cgroup-root`      | `/sys/fs/cgroup` | Filesystem root the cgroup collector reads from. Empty string disables the loop (useful on macOS dev machines or nodes without cgroups v2). |
| `-cgroup-subjects`  | `true`           | Walk pod cgroups (and `-cgroup-units`) as subjects: each becomes its own set of properties, admitted on first observation and retired on silence. |
| `-cgroup-units`     | `""`             | Comma-separated globs of systemd units under `system.slice` to model as `unit:<name>` subjects (e.g. `k0s*.service,containerd.service`); empty models none. |
| `-cgroup-max-subjects` | `256`         | Upper bound on subjects the cgroup walk admits per tick; beyond it the rest are skipped. |
| `-cgroup-cmd-label` | `false`          | Stamp `cmd=<argv0>` from each subject's first process (label only; needs `/proc` visibility, i.e. hostPID or privileged). |
| `-node-id`          | `""`             | Identifier this agent puts on emitted `MetricSample`s and uses in event IDs. Empty falls back to `os.Hostname()`. |
| `-netdata-url`      | `""`             | Base URL of a Netdata daemon to poll for live node metrics (e.g. `http://localhost:19999`). Empty disables Netdata collection. When set together with `-cgroup-root`, both run as a `MultiCollector`. |
| `-script`           | `""`             | Run the synthetic system from this scenario file instead of real collectors — replaces cgroup and Netdata collection entirely when set. `-collect-interval` must equal the scenario's `tick_seconds` (all seed scenarios use 10s): the script stamps simulated time one tick per collect while the map sweeps on the wall clock, so the daemon refuses to start when they differ, as it does when the file cannot be loaded. |
| `-proposer`         | `true`           | Enable `MICorrelationProposer` (Fisher z p-values, construct-level pairing). Set `false` on nodes where ring-buffer overhead is undesirable; the daemon falls back to `DisabledProposer` (no-op). |
| `-proposer-threshold` | `0.85`         | `\|Pearson r\|` above which the proposer emits a candidate. |
| `-proposer-min-pairs` | `30`           | Co-observations a pair needs inside the pair window before it can become a candidate — the pace at which structure appears. |
| `-tuner`            | `true`           | Enable `RuleBasedTuner`. Set `false` to disable operator tuning entirely; `POST /agent/tune` still accepts requests but returns empty adjustments. |
| `-regime`           | `""`             | Dynamics preset (`stable`/`default`/`bursty`/`volatile`). Overrides `-alpha` and `-convergence` when set. |
| `-peers`            | `""`             | Comma-separated peer agent URLs to register at startup. Additional peers can be added at runtime via `POST /peers`. |

#### Scenario file format

A scenario is JSON with a name, `seed`, `tick_seconds`, `duration_seconds`, optional `noise`, and three blocks:

| Block | Fields |
| --- | --- |
| `node` | one entry per node-level property, `{"coupling": "sum" \| "logistic" \| "none", "base", "of", "theta", "k"}`. `sum` is `base + Σ` over active subjects of their property `of`; `logistic` is `σ((x − theta)/k)` where `x` is the node property `of` (or, if `of` names a subject property, `base +` its sum); `none` is `base`, a property nothing drives. |
| `subjects` | `{"id": "<kind>:<identity>", "arrive", "depart", "return", "properties": {...}}`, times in seconds from the start; `depart` absent means never, `return` revives the same subject. Each property is `{"pattern": "constant" \| "ramp" \| "sine" \| "burst", "value", "min", "max", "period", "burst_start", "burst_duration", "unit", "range"}` — `constant` holds `value`; `ramp` goes `min → max` over `period` seconds then holds; `sine` oscillates between `min` and `max` with `period`; `burst` is `min` except `max` during `[burst_start, burst_start + burst_duration)`, repeating every `period` when it is positive. Schedules are relative to the subject's arrival or return. |
| `expect` | what the runner asserts: `admitted_within_ticks`, `stale_within_seconds`, `retired_within_seconds`; `candidates` (`{"from", "to", "sign", "within_seconds", "reproposed_after_return"}`), `no_candidates_from` (subject ids that must never be proposed), and `counterfactuals` (`{"target", "assume": {"<property>@<subject>": value}, "regime": "linear" \| "saturated", "tolerance", "min_error"}` — `linear` asserts the projection is within `tolerance` of the model's truth, `saturated` asserts it is at least `min_error` off and that the standing slope caveat is present). |

Property names are metric types (`[A-Za-z0-9._-]`); the runner addresses a subject's property as `<property>@<subject>`. The loader validates all of this and refuses a file it cannot run: every `of` must name an existing subject property (or, for `logistic`, a non-logistic node property), a `linear` counterfactual needs `tolerance > 0` and a `saturated` one `min_error > 0`, its target must be a node property and its `assume` keys must name existing subject properties. Past `duration_seconds` the script emits nothing and logs once that the scenario finished.

See `scenarios/` for five seed scenario files to drive `-script` with a known ground truth: `linear` and `saturation` (single-subject couplings, one that extrapolates and one that saturates), `confounded` (one subject drives a node property while another merely co-varies with it — the map proposes both, because correlation alone cannot separate them), `decoupled` (a correlated subject plus an uncorrelated noise subject) and `churn` (a subject that departs and returns, exercising staleness and retirement).

**State model** — the properties this system exhibits, their lifecycle, and what this agent holds about other nodes.

| Flag                     | Default | Meaning                                                                            |
| ------------------------ | ------- | ---------------------------------------------------------------------------------- |
| `-stale-after`           | `2m`    | Silence after which a property is marked stale. Its last value is kept and labelled: a stale reading is evidence about the past, and silence is not evidence about the present. |
| `-retire-after`          | `10m`   | Silence after which a property is retired automatically, cascading to relationships that reference it. The default, `10m`, is five default stale windows — long enough that a restarting collector is not mistaken for a departed subject — and does not follow `-stale-after`. `0` leaves retirement to an operator. Retirement is soft. |
| `-sweep-interval`        | `0`     | How often lifecycle transitions are applied. `0` derives an interval from `-stale-after`. |
| `-no-admit`              | `false` | Refuse to create a property for an undeclared metric. The default admits it and journals the admission, because a model that cannot represent something new describes the system as it was when someone wrote it down. |
| `-no-learn`              | `false` | Disable the paired estimator. Every relationship then stays at `basis: unknown` with confidence 0 for the life of the process — an agent that can report levels and nothing about how they relate. |
| `-pair-window-seconds`   | `15`    | How far apart two observations may be and still count as simultaneous. Collectors sample on independent grids, so without a tolerance no pair ever forms. |
| `-pair-support`          | `8`     | Pairs a relationship needs before its strength moves at all. Below it the pair is buffered and confidence keeps reporting that nothing has been learned. |
| `-pair-history`          | `60`    | Recent pairs the estimate is computed over. Older pairs fall out, which is what lets a relationship follow a system whose behaviour changes rather than averaging its whole history. |
| `-state-file`            | `""`    | Path to persist the map and its journal. Empty keeps everything in memory, so a restart returns the agent to cold start on a system it has already watched. A snapshot naming a different owner is refused. |
| `-save-interval`         | `1m`    | Snapshot cadence when `-state-file` is set. A snapshot is also written synchronously on shutdown, so this bounds what an unclean exit loses. |
| `-journal-size`          | `0`     | Change and decision entries held in memory (`0` = default 2000). The journal reports how many it dropped, so an absence is not read as "nothing happened". |
| `-ingest-scope`          | `self`  | `self` ingests only this machine's samples. `any` aggregates every machine's telemetry into one map — correct for replaying a testbed into a single daemon, wrong for a deployment. |
| `-peer-state-poll`       | `30s`   | How often to fetch each peer's map. `0` disables polling, leaving `GET /state/cluster` to report only what a manual refresh collected. |
| `-peer-state-stale`      | `90s`   | How old a peer's snapshot may be before the cluster view marks it history. Nothing is deleted at this age: during a partition the last snapshot is all this agent has about that node. |

**Explain layer** (all optional; `POST /explain` returns 501 unless `-explain-provider` is set). Full walkthrough in [§8](#8-natural-language-explain-explain).

| Flag                       | Default                          | Meaning                                                                 |
| -------------------------- | -------------------------------- | ----------------------------------------------------------------------- |
| `-explain-provider`        | `none`                           | `none` (disabled) or `openai-compatible` (Ollama, llama-server, LM Studio, vLLM, hosted OpenAI). |
| `-explain-url`             | `http://localhost:11434/v1`      | Base URL of the OpenAI-compatible backend.                              |
| `-explain-model`           | `qwen2.5:7b-instruct`            | Model name passed to that backend.                                      |
| `-explain-prompt`          | `cmd/agent/prompts/explain-v1.md`| Answering-agent system prompt. Required when the provider is enabled.   |
| `-explain-planner-prompt`  | `<prompt-dir>/planner-v1.md`     | Planner prompt. A missing *derived* default disables the planning stage with a log line rather than failing startup. |
| `-explain-critic-prompt`   | `<prompt-dir>/critic-v1.md`      | Critic prompt. Same degradation semantics as the planner prompt.        |
| `-explain-keep-alive`      | `30m`                            | Passed to Ollama-style backends as `keep_alive` so the model stays resident between calls. Empty omits the field. |
| `-explain-sessions`        | `true`                           | Enable multi-turn session memory and the session-scoped tool-result cache. |
| `-explain-api-key`         | `""`                             | Bearer token for the backend. `EXPLAIN_API_KEY` in the environment takes precedence. |

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

The daemon loads the file via `-priors`. `-kd` names the distribution running on this
node; it is validated against the file's `distributions` list and the daemon refuses to
start if the name is not there.

```bash
agent -priors /etc/semantic-map/prior_weights.json -kd k0s
```

**Both agents built with different `-kd` values produce identical answers until telemetry
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

### `--peers` flag

Pass a comma-separated list of peer URLs at daemon start:

```bash
go run ./cmd/agent \
  -profile edge-minimal -addr :8080 \
  -peers http://node_1:8080,http://node_2:8080
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
* `peers.Registry` and `peers.Client` are concrete types in `pkg/peers/`, **not** a seventh contract. The contract surface stays at six (Storage, Ontology, Updater, Reasoner, Proposer, Collector). We promote when a second implementation arrives (e.g. SQLite-backed registry, gossip discovery).

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

The default is off (`-explain-provider=none`), so `POST /explain` returns 501 with instructions until you opt in. To enable, point the daemon at a local OpenAI-compatible backend — Ollama is the smallest starting point:

```bash
# 1. Install and start a local LLM (out of band, one-time)
brew install ollama
ollama serve &                       # runs on :11434 by default
ollama pull qwen2.5:7b-instruct

# 2. Start the daemon with -explain-provider=openai-compatible
cd semantic-map/go
go run ./cmd/agent \
    -profile edge-minimal \
    -addr :8080 \
    -explain-provider openai-compatible \
    -explain-url http://localhost:11434/v1 \
    -explain-model qwen2.5:7b-instruct \
    -explain-prompt cmd/agent/prompts/explain-v1.md &

# 3. Ask a question
curl -s -X POST http://localhost:8080/explain \
    -H 'Content-Type: application/json' \
    -d '{"question":"Which edges are driving my ResourceCost?"}' | jq
```

Any backend that speaks the OpenAI `/v1/chat/completions` surface works — `llama-server`, LM Studio, vLLM, or the hosted OpenAI API. Point `-explain-url` at its base and set the model name.

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
- **Determinism where it counts.** The reasoner and the state model stay pure Go, and the initialization pipeline is a build-time Python step that touches no node. Explain is on the operator-facing surface, not on the ingestion or reasoning path. All P6 results reported in `research-docs/` use `-explain-provider=none`.

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

### v2 flags

```
-explain-planner-prompt   path to planner prompt   (default: <prompt-dir>/planner-v1.md)
-explain-critic-prompt    path to critic prompt    (default: <prompt-dir>/critic-v1.md)
-explain-keep-alive       model residency hint     (default: 30m — big latency win on repeat calls)
-explain-sessions         enable session memory    (default: true)
```

A missing planner or critic prompt disables that stage with a log line rather than failing startup.
