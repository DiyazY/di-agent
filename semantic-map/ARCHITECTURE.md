# Semantic Map — Architecture

Design rationale and decision record. Update this file when a contract, profile, MetricType, or structural decision changes. For usage (running, API, compliance), see [README.md](README.md).

---

## Table of Contents

- [1. Core Concept](#1-core-concept)
  - [The agent at a glance](#the-agent-at-a-glance)
  - [Component reference](#component-reference)
  - [The four request lifecycles](#the-four-request-lifecycles)
- [2. Contract Architecture](#2-contract-architecture)
  - [The six contracts](#the-six-contracts)
  - [What is deliberately not a contract](#what-is-deliberately-not-a-contract)
  - [Behavioral guarantees](#behavioral-guarantees)
  - [End-to-end validation: integration scenarios](#end-to-end-validation-integration-scenarios)
- [3. Deployment Profiles](#3-deployment-profiles)
- [4. Language Strategy](#4-language-strategy)
- [5. Telemetry Pipeline](#5-telemetry-pipeline)
  - [CollectorContract](#collectorcontract)
  - [MetricType catalogue](#metrictype-catalogue)
  - [The Bridge](#the-bridge)
  - [Planned collector implementations](#planned-collector-implementations)
- [6. Automatic Graph Extension](#6-automatic-graph-extension)
- [7. Adding a New Profile](#7-adding-a-new-profile)
- [8. Connection to Research](#8-connection-to-research)
- [9. Control Surface](#9-control-surface)
- [10. Coordination](#10-coordination)
- [11. Operator Tuning Interface](#11-operator-tuning-interface)
- [12. PoC Deployment (`poc/`)](#12-poc-deployment-poc)
- [13. Natural-Language Explain Layer (`pkg/explain`)](#13-natural-language-explain-layer-pkgexplain)
- [14. Explain v2 — Planning, Critic, Sessions, Streaming](#14-explain-v2--planning-critic-sessions-streaming)

---

## 1. Core Concept

The Semantic Map has two layers that are always present simultaneously:

```
┌────────────────────────────────────────────────────────────────┐
│  Layer 2 — Evidence (dynamic)                                  │
│  Statistical descriptors updated by live telemetry             │
│  "In THIS cluster, under THESE workloads, here is reality"     │
├────────────────────────────────────────────────────────────────┤
│  Layer 1 — Backbone (stable prior)                             │
│  7 Di-Select constructs + 15 causal propositions (P1–P15)      │
│  "What matters and how things relate"                          │
└────────────────────────────────────────────────────────────────┘
```

**The cold-start arc:** on day one the agent relies entirely on Di-Select priors. As deployment telemetry flows in, each edge's EMA drifts toward observed reality. A `confidence` score on every edge tracks the transition:

```
effective_value = (1 - confidence) × prior  +  confidence × ema
```

At `confidence = 0.0` the agent uses the literature. At `confidence = 1.0` it uses its own deployment history. The transition is smooth and automatic.

**What is stable and what is not:**

| Element                               | Stable?                                        |
| ------------------------------------- | ---------------------------------------------- |
| Graph topology — the 7 constructs     | Yes — domain-invariant                         |
| Proposition directions (P1–P15)       | Yes — causal directions do not change          |
| Proposition magnitudes (edge weights) | No — learned from evidence                     |
| New edges (P16+)                      | Possible — discovered by the Proposer contract |

### The agent at a glance

One daemon, four concentric layers. Everything outside the core is optional; the core is what makes the agent an agent.

```
                            ╔═══════════════════════════════════════════════════╗
   HUMAN / SCRIPT ─────────▶║  LAYER 4 · OPERATOR SURFACE                       ║
   "why is my cost high?"   ║                                                   ║
                            ║   HTTP API  ·  mapctl CLI  ·  /ui/  ·  /explain   ║
                            ║   ─────────────────────────────────────────────   ║
                            ║   pkg/explain: planner → answer → critic          ║
                            ║   (LLM CONSUMES the graph; never mutates it)      ║
                            ╚════════════════════════╤══════════════════════════╝
                                                     │ read-only tools
                            ╔════════════════════════▼══════════════════════════╗
   PEER AGENTS ────────────▶║  LAYER 3 · DECISION                               ║
   GET /cost                ║                                                   ║
   POST /offload            ║   SemanticMap facade                              ║
                            ║   CostOfAction · RecommendPeer · SimulateOutcome  ║
                            ║   ┌──────────┬───────────┬──────────┬──────────┐  ║
                            ║   │ Reasoner │ Ontology  │ Proposer │  Tuner   │  ║
                            ║   └──────────┴───────────┴──────────┴──────────┘  ║
                            ╚════════════════════════╤══════════════════════════╝
                                                     │ reads / writes
                            ╔════════════════════════▼══════════════════════════╗
                            ║  LAYER 2 · STATE                                  ║
                            ║                                                   ║
                            ║   Storage (multigraph, keyed by from,to,propID)   ║
                            ║   ┌─────────────────────────────────────────────┐ ║
                            ║   │ Backbone: 7 constructs, 15 propositions     │ ║
                            ║   │ Evidence: per-edge EMA + confidence + n_obs │ ║
                            ║   │ Audit:    append-only OntologyEvent log     │ ║
                            ║   └─────────────────────────────────────────────┘ ║
                            ╚════════════════════════▲══════════════════════════╝
                                                     │ update_edge()
                            ╔════════════════════════╧══════════════════════════╗
   /sys/fs/cgroup ─────────▶║  LAYER 1 · INGESTION                              ║
   Netdata :19999           ║                                                   ║
   parquet replay           ║   Collector ──samples──▶ Bridge ──▶ Updater (EMA) ║
                            ║   (typed MetricSample)   (routes    (idempotent   ║
                            ║                           to        per event_id) ║
                            ║                           construct)              ║
                            ╚═══════════════════════════════════════════════════╝
```

**Read the diagram bottom-up for data, top-down for questions.** Telemetry flows up from Layer 1 and settles into Layer 2. Questions arrive at Layer 4 or Layer 3 and read downward. The two directions meet at Storage, which is the only mutable state in the process.

**The one-way rule.** Layer 4 can read everything and write nothing. The LLM in `pkg/explain` gets six read-only tools; it cannot call `Deprecate`, `Tune`, or `ResetEdge`. When it wants a mutation it emits a *draft proposal* that a human must POST themselves. This is what keeps the human-judgment anchor intact (§8) and what keeps P6's results reproducible without an LLM in the loop (§13).

### Component reference

What each piece is for, when it runs, and whether it is required.

| Component | Package | Purpose | Runs when | Required? |
|---|---|---|---|---|
| **Collector** | `internal/minimal` | Read raw metrics from cgroup / Netdata / parquet; emit typed `MetricSample`s | Collection loop tick (`-collect-interval`) | Optional — `POST /ingest` works without one |
| **Bridge** | `pkg/semmap` | Route one `MetricSample` to its construct, then to every edge touching it | Every sample | Yes (stateless, not a contract) |
| **Updater** | `internal/minimal` | Fold the observation into each edge's EMA; bump confidence | Every routed sample | Yes |
| **Storage** | `internal/minimal` | Hold node + edge descriptors as a multigraph | Every read and write | Yes |
| **Ontology** | `internal/minimal` | Own the backbone: constructs, propositions, validation, audit log | Reasoning, mutations | Yes |
| **Reasoner** | `internal/minimal` | Turn graph state into `ActionCost` / `PeerRecommendation` with a rationale | `/cost`, `/recommend`, `/simulate` | Yes |
| **Proposer** | `internal/minimal` | Mine observation history for candidate new edges (MI correlation) | Background, opt-in (`-proposer`) | Optional |
| **Tuner** | `internal/minimal` | Parse operator intent text → proposition-strength deltas | `POST /agent/tune` | Optional |
| **Peers** | `pkg/peers` | Peer registry + trust; HTTP client for remote `/cost` | `/recommend`, `/peers`, `/offload` | Optional |
| **Explain** | `pkg/explain` | Natural-language Q&A grounded in the graph; planner + critic + sessions | `POST /explain` | Optional (`-explain-provider=none` default) |
| **Profiles** | `pkg/profiles` | Wire concrete implementations to each contract at startup | Once, at boot | Yes |

### The four request lifecycles

Four distinct things can happen to this daemon. Each takes a different path.

**① Telemetry arrives** — the loop that makes the agent learn.

```
Collector.Collect()
  └─▶ []*MetricSample {NodeID, MetricType, Value, EventID}
        └─▶ Bridge: MetricType → construct (e.g. cpu_utilization → RC)
              └─▶ Ontology.Relationships(RC) → every proposition touching RC
                    └─▶ Updater.UpdateEdge(from, to, value, eventID)  × each pair
                          └─▶ Storage: ema += α(value − ema); n_obs++; confidence = n_obs/N
```
*Idempotent per `(edge, event_id)` — replaying the same sample changes nothing.*

**② A decision is requested** — the loop that makes the agent useful.

```
GET /cost?task=pod-scheduling&node=master
  └─▶ Reasoner.CostOfAction
        ├─▶ Storage.AllEdges()          — current EMA, prior, confidence per edge
        ├─▶ Ontology.Propositions()     — skip anything Deprecated
        └─▶ for each RC-destination edge:
              effective = (1−c)·prior + c·ema
              ResourceCost += (effective − prior) · sign(direction)
        └─▶ ActionCost {ResourceCost, Confidence, Rationale, GraphPathUsed}
```
*Pure read. No state changes. Always returns a rationale naming the edges used.*

**③ The graph is mutated** — the loop that keeps the agent honest.

```
POST /ontology/deprecate {"proposition_id":"P7","reason":"..."}
  └─▶ Ontology.Deprecate
        ├─▶ mark Proposition.Deprecated = true      (soft delete — never removed)
        ├─▶ sync EdgeDescriptor.Deprecated in Storage
        └─▶ append OntologyEvent {actor, kind, target, timestamp}
              └─▶ readable forever via GET /history
```
*Only four mutations exist: `SetPropositionStrength`, `AddConstruct`, `AddValidatedProposition`, `Deprecate`. Every one is audited. Construct removal and direction reversal are impossible by design.*

**④ A human asks a question** — the loop that makes the agent legible.

```
POST /explain {"question":"why is my cost high?","use_planner":true,"use_critic":true}
  └─▶ planner LLM (no tools)  → Plan{steps:[{tool:"get_cost"},…]}
        └─▶ Go executes the plan  → evidence bundle
              └─▶ answering LLM (read-only tools) → answer + citations
                    ├─▶ GATE 1: deterministic validator — do the cited values match live state?
                    └─▶ GATE 2: critic LLM — is the reasoning actually right?
                          └─▶ ExplainResponse {answer, citations, plan, verdict, usage}
```
*Detailed in §13–§14. The LLM never writes; it only reads and drafts.*

---

## 2. Contract Architecture

The Semantic Map is not a monolith. It is a **set of responsibilities, each behind a contract (interface)**. Concrete implementations are fully swappable — agent code never imports an implementation directly.

```
  Metric source          Semantic Map
  (cgroup / Netdata)
        │
   [Collector] ──samples──▶ [Bridge] ──update_edge()──▶ [Updater]
                                                              │
                    ┌─────────────────────────────────────────┘
                    ▼
        ┌───────────────────────────────────────────┐
        │              SemanticMap facade            │
        │  cost_of_action()  recommend_peer()        │
        │  simulate_outcome()  tune()                │
        └───┬───────┬──────────┬────────┬───────┬───┘
            │       │          │        │       │
        Storage  Ontology  Reasoner  Proposer  Tuner
            ▲                                        
            │ read-only                              
   ┌────────┴──────────┬──────────────────┐          
[peers]            [explain]         [control surface]
 registry       planner·critic       HTTP · mapctl · /ui
 + client       + validator          (§9)
   (§10)          (§13–14)
```

The Collector and Bridge live outside the SemanticMap facade — they feed it. The Bridge is not a contract; it is a thin, stateless mapper (see §5). The three components below the facade are *consumers*: they read graph state and expose it, but only the facade's own mutation methods can change it.

### What is deliberately not a contract

The contract set has stayed at six since the first release. Three substantial components sit outside it on purpose:

| Component | Why it is concrete, not a contract |
|---|---|
| **Bridge** (`pkg/semmap`) | Stateless pure function of `(MetricType, Ontology)`. There is nothing to swap — a second implementation would be the same code with a different routing table, and the routing table is already data. |
| **Peers** (`pkg/peers`) | One implementation exists. Promoting it to a contract before a second one (SQLite-backed registry, gossip discovery) would be designing an interface against a sample size of one. |
| **Explain** (`pkg/explain`) | Same reasoning, plus: it is an operator convenience, not part of the agent's decision path. A contract would imply the daemon depends on it. It does not — the default is `-explain-provider=none`. |

The rule we hold to: **no new contract without a second implementation that needs it.** Interfaces derived from one example encode that example's accidents. Each of these three gets promoted the day a real second implementation arrives, and not before.

### The six contracts

| Contract      | Responsibility                                              | Key guarantees                                                                                            |
| ------------- | ----------------------------------------------------------- | --------------------------------------------------------------------------------------------------------- |
| **Collector** | Read raw metrics from a source; emit normalized samples     | Pure read; deterministic `event_id`; `available_metrics()` is static; never raises on empty data         |
| **Storage**   | Read/write node and edge descriptors                        | Atomic writes; `nil` on miss, never raises. **Multigraph:** edges keyed by `(from, to, proposition_id)` — `GetEdgesByPair` returns all edges between two constructs; `GetEdge` returns one deterministic pick |
| **Ontology**  | Live structural knowledge — constructs, propositions, validation, audit | Always returns ≥7 constructs + P1–P15; constructs are append-only; propositions are soft-deleted via `Deprecate` (never removed or direction-reversed); every mutation appends to an audit log readable via `GetHistory` |
| **Updater**   | Incorporate telemetry into edge/node descriptors            | Idempotent per `(edge, event_id)` — one observation updates every edge in a `(from, to)` pair, each tracking its own EMA. `Reset` restores prior without deleting |
| **Reasoner**  | Produce agent decisions with traceable rationales           | Every result includes a non-empty rationale referencing graph path; `SimulateOutcome` is pure (read-only) |
| **Proposer**  | Detect statistical patterns suggesting new backbone edges   | Never modifies Storage or Ontology directly; `Reject` permanently suppresses within session               |

### Behavioral guarantees

Guarantees are not just signatures — they are documented pre/post-conditions on each method in the contract source files. The compliance test suites in `compliance/` verify them mechanically. **A new implementation is valid if and only if it passes the compliance suite for its contract.** This is the definition, not a check.

Compliance suites exist for all six contracts (`compliance/{collector,storage,updater,ontology,reasoner,proposer}.go`). Each runs against a factory the implementation supplies, so a new storage or ontology can be validated with a single test file wired to the suite.

### End-to-end validation: integration scenarios

Compliance proves each part works in isolation. **Scenarios prove the parts compose** into the behaviors the architecture promises. `internal/minimal/tests/scenarios_test.go` runs six narrated end-to-end flows against the same wiring the production daemon uses; each emits `t.Logf` snapshots so `go test -v -run TestScenario` reads like a paper results section while hard assertions guard the mechanics that must not regress:

| Scenario                            | Demonstrates                                                                                          |
| ----------------------------------- | ----------------------------------------------------------------------------------------------------- |
| `ColdStart`                         | 15 edges seeded, confidence=0, effective value == prior — agent defers entirely to literature         |
| `ConvergenceOnOneEdge`              | 500 obs at fixed value: EMA drifts prior → observed, confidence climbs 0→1, effective crosses over    |
| `PerKDDecisionsDiffer`              | Two agents with same query but different `-kd`: cost outputs diverge — the per-KD priors steer        |
| `DeprecationShrinksGraph`           | After `Deprecate("P1")`: graph path length drops by exactly 1; storage retains the EdgeDescriptor      |
| `IdempotentReplay`                  | 200 obs replayed with same eventIDs is a no-op; new eventIDs accumulate — idempotency is per-event    |
| `AuditTrailRecordsEverything`       | Four ontology mutations → exactly four `OntologyEvent`s in chronological order via `GetHistory`        |

A separate numerical verification (`pkg/profiles/profiles_test.go::TestPerKDSeedingMatchesPriorWeights`) confirms that for every KD in `prior_weights.json` and every one of the 15 propositions, the seeded `EdgeDescriptor.PriorWeight` matches the file to 1e-6 precision. This is the production reason to trust the `-kd` flag.

#### Evolution scenarios

`internal/minimal/tests/evolution_test.go` runs six longer-form scenarios driven by the `ScriptedCollector` (§5) and, for scenario 6, the `MICorrelationProposer` (§6). Each prints checkpoint tables + an `EVOLUTION SUMMARY` block via `t.Logf` so the convergence story is reproducible from CI. Run with `go test -v -run TestEvolution ./internal/minimal/tests/...`:

| Scenario                              | Demonstrates                                                                                                |
| ------------------------------------- | ----------------------------------------------------------------------------------------------------------- |
| `ColdToWarmConvergence`               | Constant CPU=0.8 for 500 ticks; P2/P3/P10 EMA → 0.8, confidence 0→1, advisor flags the 6 high-Δ edges        |
| `RegimeChange`                        | Step pattern 0.3→0.85→0.3 over 800 ticks; EMA tracks each regime, ending pulled back toward 0.3              |
| `ConflictPairCoupling`                | P2 (RC→PS−) and P3 (RC→PS+) share EMA + confidence updates from one observation; reasoner aggregates both    |
| `MultiConstructStress`                | Four simultaneous patterns drive RC, CO, PS; edges touching observed constructs converge, others stay prior |
| `DeprecationFromContradiction`        | Low CO evidence pushes P5 off prior; advisor fires (|Δeff|+conf); operator deprecates; path shrinks by 1     |
| `NewEdgeProposeConfirm`               | MI proposer detects MU↔PS correlation; operator confirms; backbone grows 15→16 (evidence: `proposer-mi`)     |

### The ontology is alive

The ontology is not a static reference. Empirical priors get recalibrated as new papers land, operators deprecate claims that the deployment's evidence contradicts, and new domains may introduce new constructs. The contract therefore admits four kinds of mutation, each emitting one `OntologyEvent` to an append-only audit log:

| Mutation                          | Method                                       | Typical caller                          |
| --------------------------------- | -------------------------------------------- | --------------------------------------- |
| Edge magnitude recalibrated       | `SetPropositionStrength(propID, strength)`   | `prior_init` pipeline; operator tuning  |
| New edge added (validated)        | `AddValidatedProposition(p)`                 | `Proposer.Confirm` (post-review)        |
| New construct added               | `AddConstruct(c)`                            | Operator (new domain extension)         |
| Existing edge retired (soft)      | `Deprecate(propID, reason)`                  | Operator (evidence-against accumulated) |

What is stable, what is not:

- **Construct removal** is impossible. Constructs are domain-stable per the architecture; once added they stay forever.
- **Proposition removal** is impossible. `Deprecate` is the only retirement path. Deprecated propositions remain in `Propositions()` so the audit trail / replay are intact, but the Reasoner skips them during cost computation. The `EdgeDescriptor` in storage stays in place too — soft-delete preserves both the structural and the evidence record.
- **Proposition direction reversal** is impossible. `ValidateProposition` rejects a new edge that contradicts an existing direction. The three Di-Select conflict pairs (P2/P3, P5/P6, P7/P9) are exempt because both halves are present from the bootstrap; the Proposer cannot introduce *new* conflict pairs without explicit operator action (a future extension).

The audit log (`GetHistory(since)`) lets the agent answer "why is this edge weight what it is?" at any point in time. On the edge-minimal profile the log is in-memory and ephemeral across restarts. The `cloud-full` profile persists it.

Implementations that intentionally do not support a mutation (e.g. a read-only ontology cache layered in front of the canonical store) return `contracts.ErrNotImplemented` rather than silently succeeding. The compliance suite tolerates this — every live-ontology subtest skips on `ErrNotImplemented`.

### Why the backbone is a multigraph

Di-Select's 15 propositions span only 12 distinct construct pairs because three are **conflict pairs** — two propositions on the same `(from, to)` capturing distinct mechanisms with opposite directions:

| Pair        | Mechanism captured by each proposition                                                       |
| ----------- | -------------------------------------------------------------------------------------------- |
| **P2 / P3** on RC→PS | P2 (−): security/resource overhead reduces throughput. P3 (+): lightweight distributions reduce pod-startup latency. |
| **P5 / P6** on CO→RR | P5 (+): offline autonomy improves continuity during partition. P6 (−): cloud dependency reduces stability in poor networks. |
| **P7 / P9** on CE→MU | P7 (+): rich ecosystem lowers operator effort. P9 (−): excessive features increase maintenance complexity. |

These are not contradictions — they are **co-existing, evidence-distinguishable** mechanisms. In a real deployment, observed telemetry will support one mechanism more than the other, and each proposition's EMA drifts independently. The agent therefore needs to store both edges, update both from a single observation, and let the relative confidence-weighted magnitudes encode which mechanism dominates in this deployment.

Implications for the contracts:

- **Storage** keys edges by the full triple `(from, to, proposition_id)`. `GetEdgesByPair(from, to)` returns every edge — critical for the Updater. `GetEdge(from, to)` returns a deterministic pick (lex-smallest `proposition_id`) so single-edge callers keep working.
- **Updater** applies one observation to every edge between `(from, to)`. Idempotency is keyed on `(from, to, proposition_id, event_id)` so a replay is a no-op per-edge, not just per-pair.
- **Reasoner** iterates `AllEdges()` directly and uses each edge's own `Direction`. There is no proposition-to-edge join, and so no risk of conflating P2 with P3.
- **Ontology** `ValidateProposition` rejects a new proposition that contradicts an existing one. The three bootstrap conflict pairs are exempt because both are present from the start with domain validation. New auto-proposed conflicts from the Proposer go through the normal rejection path — backbone extension does not introduce conflict pairs without explicit operator action.

---

## 3. Deployment Profiles

A profile is a named configuration that wires specific implementations to each contract. The agent binary is identical across profiles — only the profile name changes at startup.

| Profile         | Collector                   | Storage   | Ontology                 | Updater        | Reasoner         | Proposer          | Target                |
| --------------- | --------------------------- | --------- | ------------------------ | -------------- | ---------------- | ----------------- | --------------------- |
| `edge-minimal`  | cgroup (direct `/sys/fs`)   | In-memory | Static Di-Select         | EMA            | Rule engine      | Disabled          | RPi4, IoT nodes       |
| `edge-standard` | cgroup + kubelet `/metrics` | SQLite    | Static + extension table | EMA + Gaussian | Rule engine      | Threshold-based   | Standard edge servers |
| `cloud-full`    | Netdata HTTP API            | Neo4j     | RDF/OWL + SPARQL         | Bayesian       | SLM (Phi-3 Mini) | Correlation miner | Cloud VMs             |

**EMA** — Exponential Moving Average: `new = α × observation + (1-α) × old`. Controls how fast the agent adapts. `α = 0.2` is the default.

**Gaussian (μ, σ)** — adds variance tracking alongside the mean. Required for `simulate_outcome()` to return P95 risk estimates. Available from `edge-standard` upward.

**Bayesian updater** — full posterior distribution update. Richer uncertainty quantification but heavier. Cloud-only.

**Why not Python on the edge?** Baseline interpreter footprint (~50–80 MB), unpredictable GC pauses under latency budgets, and the operational cost of managing a Python runtime on every constrained node.

---

## 4. Language Strategy

| Layer                                       | Language   | Why                                                                                                                           |
| ------------------------------------------- | ---------- | ----------------------------------------------------------------------------------------------------------------------------- |
| Contract interfaces + compliance tests      | **Python** | Specification role — readable, fast to iterate, serves as the authoritative definition of correct behavior                    |
| Prior initialization pipeline               | **Python** | One-time data wrangling from P1–P5; pandas/numpy/scipy ecosystem                                                              |
| `cloud-full` profile service                | **Python** | scipy for Bayesian updater; correlation miner; SLM integration                                                                |
| `edge-minimal` and `edge-standard` daemons  | **Go**     | Single ARM binary, <10 MB footprint, no runtime to manage on edge nodes, goroutines for concurrent telemetry, predictable GC  |

**The contract boundary enables this split.** The Python ABCs are the specification. The Go interfaces mirror them exactly. Both language implementations run against their respective compliance suites — passing both suites proves behavioral equivalence across languages.

---

## 5. Telemetry Pipeline

Live observations flow into the Semantic Map through a three-stage pipeline:

```
┌──────────────┐   MetricSample[]   ┌────────┐  update_edge()  ┌─────────┐
│  Collector   │ ─────────────────▶ │ Bridge │ ──────────────▶ │ Updater │
│  (contract)  │                    │ (thin) │                  │(contract│
└──────────────┘                    └────────┘                  └─────────┘
  cgroup plugin                     maps metric type      EMA / Gaussian /
  Netdata plugin                    → (from_id, to_id)    Bayesian update
  kubelet plugin                    via Ontology
```

### CollectorContract

A collector reads from a raw source, normalizes to `MetricSample`s, and returns them. It knows nothing about the graph.

```python
samples: list[MetricSample] = collector.collect()
```

Each `MetricSample` carries:

| Field            | Type         | Description                                              |
| ---------------- | ------------ | -------------------------------------------------------- |
| `node_id`        | `str`        | Cluster node (`"master"`, `"node_1"`, …)                |
| `metric_type`    | `MetricType` | Semantic type — see catalogue below                      |
| `value`          | `float`      | Normalized value in the fixed unit for the metric type   |
| `timestamp_unix` | `int`        | Unix timestamp of the observation                        |
| `event_id`       | `str`        | Deterministic ID — same observation always → same ID     |
| `container_id`   | `str`        | Empty for node-level aggregates; set for per-container   |
| `labels`         | `dict`       | Source metadata (cgroup path, Netdata chart, …); opaque  |

**`event_id` determinism** is the collector's responsibility. A stable recipe: `sha256(source_id + node_id + container_id + metric_type + str(timestamp_unix))[:16]`. This carries the Updater's idempotency guarantee end-to-end: replaying the same telemetry batch has no effect on the graph.

**`available_metrics()` is static** — declared once at construction, never changes within a deployment session. The Bridge uses this to know which graph edges can be updated without calling `collect()` first.

### MetricType catalogue

Units are fixed per type. Collectors must normalize raw source values to these units before emitting.

| `MetricType`            | Unit           | Maps to construct(s)            | Note                          |
| ----------------------- | -------------- | ------------------------------- | ----------------------------- |
| `cpu_utilization`       | fraction [0,1] | RC                              |                               |
| `memory_utilization`    | fraction [0,1] | RC                              |                               |
| `cpu_throttle_ratio`    | fraction [0,1] | RC → PS edge (P2 proxy)         | cgroup `cpu.stat` throttled_periods / total_periods |
| `block_io_util`         | fraction [0,1] | RC                              |                               |
| `pod_startup_ms`        | milliseconds   | PS                              | creation timestamp → Running  |
| `scheduling_latency_ms` | milliseconds   | PS                              | Pending → Scheduled           |
| `network_rx_bps`        | bytes/sec      | CO                              |                               |
| `network_tx_bps`        | bytes/sec      | CO                              |                               |
| `network_loss_ratio`    | fraction [0,1] | CO → PS edge (P13 proxy)        |                               |
| `network_latency_ms`    | milliseconds   | CO, PS                          | RTT to a peer node            |
| `energy_joules`         | joules         | RC (energy cost per interval)   | from RAPL or P4 model         |

**Constructs with no runtime telemetry** (SC, MU, CE, RR) are updated exclusively from the prior. This is intentional — those constructs reflect structural properties of the distribution (security posture, setup complexity, community health) that do not change during a running deployment. Their priors are set by the initialization pipeline.

### The Bridge

The bridge is a stateless function wired inside the agent. It is not a contract because its logic is fully determined by the Ontology — there is nothing to swap. For each `MetricSample` it:

1. Looks up which proposition edges involve the metric's target construct via `OntologyContract.Relationships(construct_id)`
2. Calls `UpdaterContract.update_edge(from_id, to_id, sample.value, sample.event_id)` for each edge
3. Calls `UpdaterContract.update_node(construct_id, sample.value, sample.event_id)` for the node descriptor

Because `event_id` flows unchanged from Collector → Bridge → Updater, idempotency is end-to-end.

### Planned collector implementations

| Plugin              | Source                           | Profile                 | Status  | Available metrics                                                    |
| ------------------- | -------------------------------- | ----------------------- | ------- | -------------------------------------------------------------------- |
| `CgroupCollector`   | `/sys/fs/cgroup/`                | `edge-minimal`          | ✅ done — `internal/minimal/collector_cgroup.go` | cpu\_utilization, memory\_utilization, cpu\_throttle\_ratio |
| `ScriptedCollector` | programmable patterns (in-process) | demo / scenarios / replay | ✅ done — `internal/scripted/collector.go`     | any MetricType the patterns declare (Constant / Ramp / Step / Sine / Burst / Noisy) |
| `ParquetReplay`     | Netdata parquet datasets (out-of-process HTTP) | dissertation reproducibility | ✅ done — `cmd/replay/`               | cpu\_utilization, memory\_utilization, network\_rx\_bps, network\_tx\_bps           |
| `KubeletCollector`  | kubelet `/metrics/resource`      | `edge-standard`         | planned | pod\_startup\_ms, scheduling\_latency\_ms                            |
| `NetdataCollector`  | Netdata HTTP streaming API       | `edge-minimal` + `cloud-full` | ✅ done — `internal/minimal/collector_netdata.go` | cpu\_utilization, memory\_utilization, network\_rx\_bps, network\_tx\_bps |

Multiple collectors can run concurrently in the same agent (e.g., `edge-standard` runs both Cgroup and Kubelet). The Bridge processes all their outputs — idempotency ensures overlapping `event_id`s from the same physical observation are harmless.

#### Externally-driven path: parquet replay

`cmd/replay/` differs from the other rows above: it is a standalone HTTP
client, not a `CollectorContract` implementation living inside the daemon.
The split is deliberate — the replay tool reproduces the dissertation's
P1–P5 dataset (225 Netdata parquets) from outside the daemon by POSTing
`MetricSample`s to `/ingest-sample`. That endpoint runs the Bridge
server-side, so externally-driven samples take the same code path as
in-process collectors. Two benefits fall out:

- Anyone with a Go toolchain and the dataset can reproduce the convergence
  story without linking against internal packages — `cmd/replay/` imports
  only `pkg/types` (via duplicated wire DTOs in `cmd/replay/client/`).
- The replay tool's `EventID` derivation (`sha256("replay:" + parquet +
  ":" + hostname + ":" + chart_context + ":" + metric_id + ":" +
  relative_time)[:16]`) carries the Updater's idempotency guarantee
  end-to-end: re-replaying the same parquet cannot inflate
  `n_observations`. The acceptance proof is the
  `n_observations`-before/after/after-again triple in the README.

The `(chart_context, metric_id, units)` → `MetricType` mapping table lives
in `cmd/replay/mapping/mapping.go`. Extending it is a one-package change
with no impact on the daemon or its profiles.

#### `replay compare` — debug/inspection side-tool (`cmd/replay/compare/`)

**Auxiliary, not a research artifact.** `replay compare` is for spotting
mapping bugs, sanity-checking that the Bridge produces a consistent shape
of evidence across different real-shaped recorded inputs, and inspecting
which edges respond to which telemetry — not for drawing production
conclusions about "which KD is better." The parquets it consumes are
synthetic benchmark loads from the P1/P2 study (controlled exercise
runs), so cross-KD divergence in its output reflects *the recorded test
harness inputs*, not natural deployment behavior.

Mechanically, compare builds N independent `SemanticMap`s — one per KD,
each seeded with that KD's calibrated priors from `prior_weights.json` —
feeds each only its own KD's parquet rows, snapshots every map's final
edge set, and emits a per-edge × per-KD inspection table (plus JSON/CSV
for downstream tooling). `Effective = (1−c)·prior + c·ema` is what the
Reasoner would consume; `Range = max − min` flags inputs that the Bridge
propagated differently per KD.

Compare deliberately **breaks the cmd/replay HTTP rule** and imports
`pkg/profiles` + `pkg/semmap` directly. The reason is mechanism
correctness: streaming k3s observations into a single daemon after k0s
leaves k0s's EMAs contaminated, so per-KD inspection in one process
needs isolated maps. The general-purpose `replay run` and `replay all`
subcommands stay HTTP-based.

EventIDs reuse `playback.EventID` so compare's outputs are deterministic
across re-runs (`diff /tmp/c1.json /tmp/c2.json == ∅` over two
consecutive `replay compare --json` invocations) — that's the only
reproducibility contract it makes. The role of this tool in the
dissertation arc is *engineering hygiene*, not a published figure.

### Implementation status

The Bridge ships as a stateless function in `go/pkg/semmap/bridge.go::Bridge`, exposed on the facade as `SemanticMap.IngestSample`. The autonomous scheduler that ticks the configured collector and feeds each sample through the Bridge lives in `go/cmd/agent/main.go::runCollectionLoop`; it is started by `startCollectionLoop` once the daemon has built its profile. Both pieces are profile-agnostic — adding a new collector means returning it from a profile build function, no changes to the loop or the Bridge.

---

## 6. Automatic Graph Extension

The Proposer contract supports discovering relationships beyond P1–P15. The flow is **propose-then-confirm** — patterns are detected automatically, but a human confirms before the backbone is modified.

```
Telemetry accumulates in the evidence layer
        ↓
Proposer computes mutual information between construct time series
        ↓
If MI > threshold AND p < 0.05 AND n_observations > min_support:
    → Emit CandidateEdge (visible via GET /candidates)
        ↓
Operator reviews: confirm / reject / defer
        ↓
Confirm → OntologyContract.AddValidatedProposition()
          (structural validation runs first — contradictions are rejected)
Reject  → Suppressed for this deployment session
```

The Proposer **never modifies the backbone directly**. `Confirm` delegates to `OntologyContract.AddValidatedProposition`, which validates the new edge against existing propositions before accepting. A proposed edge that contradicts a validated proposition (e.g., a positive direction where a negative is already established) is rejected.

The `edge-minimal` profile ships with `MICorrelationProposer` enabled by default (daemon flag `-proposer=true`). It ring-buffers construct-level observations fed by `IngestSample` via `ObserveConstruct`, pairs values across constructs lexicographically, and emits `CandidateEdge`s when `|Pearson r|` exceeds the threshold (default 0.85). P-values are computed via the Fisher z-transform (`z = atanh(r) × √(n−3)`, two-tailed using `math.Erfc`). `Confirm` delegates to `OntologyContract.AddValidatedProposition` with evidence source tag `proposer-mi`. The coverage check is direction-aware so conflict-pair siblings (opposite direction on the same `(from, to)`) remain reachable. Pearson stands in for true mutual information here — a richer estimator can drop in at `edge-standard`/`cloud-full` without touching the interface. Pass `-proposer=false` on resource-constrained nodes where the ring-buffer overhead is undesirable.

---

## 7. Adding a New Profile

1. Create `go/internal/<profile-name>/` and implement all six contracts, or reuse existing implementations.
2. Every implementation must pass its contract's compliance suite before being wired into a profile.
3. Add a case to `go/pkg/profiles/profiles.go`:

```go
case "my-profile":
    collector := myprofile.NewMyCollector(...)
    storage   := myprofile.NewMyStorage(...)
    ontology  := minimal.NewStaticDiSelectOntology() // reuse if sufficient
    updater   := myprofile.NewMyUpdater(storage, ...)
    reasoner  := myprofile.NewMyReasoner(storage, ontology, ...)
    proposer  := myprofile.NewMyProposer(...)
    tuner     := myprofile.NewMyTuner(...)      // or minimal.NewDisabledTuner() to opt out
    seedFromOntology(storage, ontology)
    return semmap.New(storage, ontology, updater, reasoner, proposer, tuner), collector, nil
```

4. Add the profile to `profiles.py` (Python registry) if a Python equivalent is needed.
5. Update the profiles table in this file (§3) and the project structure in README.md.

No other file needs to change.

---

## 8. Connection to Research

| Publication                                 | Role in Semantic Map                                                          |
| ------------------------------------------- | ----------------------------------------------------------------------------- |
| P1 (Performance & Resource Efficiency)      | Initial priors: pod-startup latency, throughput constants per KD              |
| P2 (Security, Resilience & Maintainability) | Initial priors: security compliance scores, recovery time constants           |
| P3 (Di-Select Framework)                    | Backbone topology: 7 constructs, 15 propositions, prior directions            |
| P4 (Energy Analysis / DVFS)                 | Initial priors: J/pod, mJ/op, interrupt amplification ratios per KD           |
| P5 (Overhead Decomposition)                 | Initial priors: per-component CPU overhead (kube-apiserver = 66.7% idle)      |
| **P6 (this work)**                          | The Semantic Map itself — schema, prior initialization, convergence study     |
| P7 (Decentralized Framework)                | Extends the Semantic Map with P2P trust edges and gossip-based peer discovery |

**P6 scientific contributions:**
1. Contract-based architecture enabling RPi4-to-cloud profile switching without changing agent logic
2. Prior initialization protocol connecting Di-Select to agent runtime (grounded in P1–P5 empirical constants)
3. Convergence study: how quickly does deployment evidence override generic priors?
4. Propose-then-confirm loop: controlled automatic backbone extension with structural validation

**Theoretical framing.** The architecture reported here can be read as a concrete instance of the *graph stage* in Andrew Ng's progression from single-loop to graph-based agentic workflows (Ng, *What's Next for AI Agentic Workflows*, Sequoia AI Ascent 2024; Schluntz & Zhang, *Building Effective Agents*, Anthropic 2024). Ng characterises the graph stage as one in which shared state is externalised into a durable, queryable structure that agents read from and write to via typed handoffs, rather than living in prompts and transcripts. A knowledge graph, in this framing, plays three complementary roles: shared memory for orchestrator–worker configurations, grounding layer for evaluator–optimiser loops, and persistent world model for reflective loops. The Semantic Map instantiates the latter two directly for the orchestration-selection domain.

Both authors identify a further requirement — *anchors* — without which "even a graph of loops can become a circular system of mutual confirmation" (Ng, ibid.). Our architecture supplies three explicit anchors:

| Anchor (Ng)          | Semantic Map implementation                                                                                             |
| -------------------- | ----------------------------------------------------------------------------------------------------------------------- |
| Real-world outcomes  | Netdata telemetry — only `MetricSample` observations update the EMA; no model-estimated evidence                        |
| Frozen rules         | Di-Select's 15 propositions as an append-only backbone (constructs never removed; direction reversal disallowed)        |
| Human judgment       | Operator tune, candidate confirm/reject, deprecate — every mutation stamped in the `OntologyEvent` audit log            |

This design also satisfies Ng's reliable-agentic-system invariant — *every important output can be traced to a task, a plan, an artifact, a source, an evaluator decision, and a bounded execution record* — as an architectural property: `CostRequest`, `GraphPathUsed`, `ActionCost`, `EventID` provenance, `Rationale`, and `n_observations` correspond one-to-one to the six required trace elements. The distinction from most LLM-agent frameworks in this space is that the backbone is not an ad-hoc ontology invented by prompting an LLM: it is Di-Select's grounded-theory result [P3]. Any downstream LLM consumer of this graph sits on the *operator-facing surface* — the reasoning and ingestion paths remain deterministic Go code, and reproducibility of P6 results does not depend on any LLM's behaviour.

---

## 9. Control Surface

The Semantic Map facade is a Go API. The agent daemon wraps it in three layers so that operators, scripts, and demos can drive the same surface without sharing process memory:

```
┌─────────────────────────────────────────────────────────────┐
│  Layer C — Web UI         cmd/agent/static/{index,app,style} │
│  Vanilla JS + Cytoscape.js, embedded via go:embed all:static │
│  Click-to-mutate viewer at /ui/                              │
├─────────────────────────────────────────────────────────────┤
│  Layer B — CLI            cmd/mapctl/                        │
│  cobra + tablewriter; one binary, sixteen subcommands        │
│  Default --addr http://localhost:8080; --json for scripting  │
├─────────────────────────────────────────────────────────────┤
│  Layer A — HTTP API       cmd/agent/{routes,dto,static}.go   │
│  net/http only; JSON in/out; CSRF via Content-Type guard     │
└─────────────────────────────────────────────────────────────┘
                            │
                            ▼
                  SemanticMap facade (pkg/semmap)
```

Every layer talks to the layer above only via HTTP — the CLI does not import `cmd/agent`, and the UI is served as static assets. This is deliberate: the daemon is the single integration point, and any third tool (e.g. a future TUI, a fleet controller) only needs to speak JSON to participate.

### HTTP API

Two endpoint families coexist on the same mux. The five pre-existing endpoints (`/ingest`, `/cost`, `/recommend`, `/simulate`, `/candidates`) keep their original `http.Error` plain-text error format to minimize diff against the v0 daemon. Every endpoint added in the Phase 1 expansion emits structured JSON errors and gates mutations on `Content-Type: application/json`.

| Verb | Path                              | Request body / params                                                  | Response (2xx)                | Semantics                                                                                              |
| ---- | --------------------------------- | ---------------------------------------------------------------------- | ----------------------------- | ------------------------------------------------------------------------------------------------------ |
| GET  | `/healthz`                        | —                                                                      | `{"ok":true}`                 | Liveness probe; never touches the facade                                                                |
| GET  | `/version`                        | —                                                                      | `VersionResponse`             | Agent version, Go runtime, build commit, construct/proposition counts                                  |
| GET  | `/graph`                          | —                                                                      | `GraphSnapshot`               | Full snapshot: every construct, every proposition (incl. deprecated), every edge                       |
| GET  | `/edges`                          | `?from=&to=` (either or both, optional)                                | `[]EdgeDTO`                   | Filtered edge list; when both `from` and `to` are set, returns the conflict-pair multigraph fan-out    |
| GET  | `/constructs`                     | —                                                                      | `[]ConstructDTO`              | Backbone nodes                                                                                         |
| GET  | `/propositions`                   | —                                                                      | `[]PropositionDTO`            | All propositions including those soft-deleted by `Deprecate` (the DTO carries a `deprecated` flag)     |
| GET  | `/history`                        | `?since=` (RFC3339 timestamp or Go duration like `1h`; omitted → zero) | `[]OntologyEventDTO`          | Append-only audit log of mutations                                                                     |
| GET  | `/neighbors`                      | `?node=ID` (required)                                                  | `[]string`                    | IDs of constructs reachable in one hop                                                                 |
| POST | `/ontology/strength`              | `SetStrengthRequest`                                                   | `204 No Content`              | Recalibrate `prior_strength` for one proposition; audit-logged                                          |
| POST | `/ontology/deprecate`             | `DeprecateRequest`                                                     | `204 No Content`              | Soft-delete a proposition (Reasoner skips deprecated edges; descriptor stays in storage for audit)     |
| POST | `/ontology/construct`             | `AddConstructRequest`                                                  | `204 No Content`              | Append a new construct (append-only; constructs are domain-stable)                                     |
| POST | `/ontology/proposition`           | `AddPropositionRequest` (`direction` is `"+"` or `"-"`)                | `204 No Content`              | Add a validated proposition; `ValidateProposition` rejects direction contradictions                    |
| POST | `/agent/reset`                    | `ResetRequest`                                                         | `204 No Content`              | Reset the EMA for a `(from, to)` pair back to its prior — does not delete the edge                     |
| POST | `/candidates/{id}/confirm`        | path only                                                              | `204 No Content`              | Promote a proposer candidate to a validated proposition                                                |
| POST | `/candidates/{id}/reject`         | path only                                                              | `204 No Content`              | Permanently suppress a candidate within the session                                                    |
| POST | `/candidates/{id}/defer`          | path only                                                              | `204 No Content`              | Keep the candidate pending; re-surface on next review                                                  |
| GET  | `/ui/...`                         | —                                                                      | static assets                 | Embedded HTML/JS/CSS for the viewer; served by `http.FileServer` over an `embed.FS` sub-tree            |

Errors on the new endpoints follow a single shape:

```json
{"error": "Content-Type must be application/json"}
```

`writeError` (in `cmd/agent/routes.go`) is the only path to a non-2xx response. The five pre-existing endpoints retain `http.Error`'s plain-text body for backward compatibility.

### CSRF mitigation: `requireJSON`

There is no auth in v1 — the daemon is intended for lab-network localhost. To stop a malicious page in a browser from issuing cross-origin mutations against a daemon on `localhost:8080`, every body-bearing POST handler calls `requireJSON(r)` and rejects requests whose `Content-Type` is not `application/json`. Browsers do not send that header on simple cross-origin form posts, so a CSRF attempt fails the Content-Type check before reaching the facade. The path-only candidate-review endpoints (`/candidates/{id}/{confirm,reject,defer}`) skip the check because they take no body; the candidate ID being unguessable in practice (UUID-shaped) is the mitigation.

This is sufficient for the v1 threat model. When the agent grows beyond localhost, a token-based auth layer is the next step (tracked in the plan's "Out of scope for v1" section).

### Direction on the wire: `"+"` vs `"-"`

`types.Direction` is a Go `int` internally (0 / 1). The DTO layer in `cmd/agent/dto.go` converts it to `"+"` (positive) and `"-"` (negative) before JSON serialization. Raw integers would render unreadably in CLI tables and UI legends; the string form preserves the publication notation. Mappers — `edgeToDTO`, `propositionToDTO`, `constructToDTO`, `eventToDTO` — are the only places conversion happens.

### Static UI: `embed.FS` with no explicit redirect

`cmd/agent/static.go` declares `//go:embed all:static` and exposes the sub-tree under `/ui/` via `http.FileServer(http.FS(sub))`. The `all:` prefix is required so dot-prefixed files (e.g. `.gitkeep`) are bundled into the binary.

There is no explicit `/ui/{$}` → `/ui/index.html` redirect. `http.FileServer` already serves `index.html` for directory roots that end in `/`, and it independently canonicalizes any URL ending in `/index.html` back to `./`. The two behaviors compose into an infinite redirect loop if you also add a manual `/ui/{$}` → `/ui/index.html` handler — which is what the v0 expansion did, and what hotfix `edffaa3` removed. The rule is: trust the file server, do not redirect.

### The `mapctl` CLI

`cmd/mapctl/` is a separate Go binary that speaks the same HTTP API. It exists for three reasons:

1. **Scripting.** `--json` makes every subcommand emit a parseable payload, suitable for Bash pipelines and CI checks.
2. **Demo control.** Subcommands map one-to-one to mutations the UI offers, so a recorded terminal session is a deterministic alternative to a click-through.
3. **Headless ops.** RPi4 nodes often lack a browser; the CLI is the only operator surface there.

| Subcommand                          | Wraps                                       | Notes                                                       |
| ----------------------------------- | ------------------------------------------- | ----------------------------------------------------------- |
| `graph`                             | `GET /graph`                                | Default table; `--json` for raw                             |
| `edges --from --to`                 | `GET /edges`                                | Multigraph: returns both edges for RC→PS, CO→RR, CE→MU      |
| `history --since`                   | `GET /history`                              | RFC3339 or duration                                         |
| `strength <id> <value>`             | `POST /ontology/strength`                   | Recalibrate one proposition                                 |
| `deprecate <id> <reason>`           | `POST /ontology/deprecate`                  | Soft-delete                                                 |
| `construct add <id> <name> <desc>`  | `POST /ontology/construct`                  |                                                             |
| `proposition add <id> <f> <t> ±<s>` | `POST /ontology/proposition`                |                                                             |
| `reset <from> <to>`                 | `POST /agent/reset`                         | EMA → prior                                                 |
| `candidates [list|confirm|reject|defer]` | `GET/POST /candidates*`                 |                                                             |
| `recommend` / `simulate`            | the corresponding POST                      | Existing endpoints                                          |
| `watch graph|edges`                 | polled GET                                  | 2s ticker; clear-screen unless `--no-color`                 |
| `dot`                               | `GET /graph` → Graphviz                     | Direct paste into `dot -Tpdf`                               |
| `health` / `version`                | `GET /healthz` / `GET /version`             | `version` also prints client build                          |

DTOs are duplicated in `cmd/mapctl/client/types.go` rather than imported from `cmd/agent` — the duplication is the contract. Treating the daemon as a remote service from day one means a third party can write a Python or Rust client without reverse-engineering Go types.

Dependencies: `github.com/spf13/cobra v1.8.1`, `github.com/olekukonko/tablewriter v0.0.5`. `tablewriter` is pinned below 1.x because the 1.x API revamp is breaking.

### The web viewer

`/ui/` serves a single-page application:

- `index.html` — markup: header (title + healthz dot + refresh), Cytoscape mount, side panel, one `<dialog>` modal, toast container
- `app.js` — controller: fetches `/graph`, builds the Cytoscape model, renders the side panel from selection events, POSTs mutations back through the same API
- `style.css` — visual rules: edge color by direction (green `+`, red `−`), opacity proportional to confidence, dashed when deprecated

Cytoscape.js 3.28.1 is loaded from `unpkg.com` (single CDN pin, no build chain). The built-in `cose` layout is sufficient for seven nodes — no extension packages needed.

Mutation flow (single edge):

1. User clicks edge → side panel populates from cached graph state and a filtered `/history` fetch
2. User clicks Deprecate / Set strength / Reset → the same `<dialog>` opens with class swaps providing the appropriate input
3. Submit → `fetch(..., {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify(req)})`
4. `204` → success toast → re-fetch `/graph` → Cytoscape redraws (dashed edge for Deprecate, opacity/weight change for Strength, EMA fields reset for Reset)
5. non-2xx → toast with the server's `{"error":"..."}` message

There is no auto-poll by default; an opt-in checkbox enables a 5-second refresh tick. Same-origin only — no CORS configuration.

### Why three layers, not one

Each layer answers a different question:

- **HTTP API** answers: "what can the agent do, expressed in JSON?"
- **CLI** answers: "what can an operator do from a script, expressed in subcommands?"
- **Web UI** answers: "what can a reviewer see and click, expressed visually?"

Collapsing them — e.g. embedding HTML rendering inside Go handlers, or building a TUI that calls `pkg/semmap` directly — would couple the daemon to its consumers. Keeping the HTTP boundary firm means the Netdata adapter (step 5 in `research-docs/SEMANTIC-MAP-STATUS.md`) can land without touching the control surface, and any future client (mobile, TUI, fleet view) can be added without changing the daemon.

---

## 10. Coordination

The dissertation calls this work a *Context-Aware Agentic Framework for **Decentralized** Edge Computing*. Single-agent EMA convergence is the mechanism; multi-agent coordination is the spine. Without it, `RecommendPeer` is dead code, "decentralized" is aspirational, and P7 has nothing to build on.

### The peer registry — concrete, not a contract

In v1 the peer registry lives at `pkg/peers/` as a **concrete package**, not a seventh contract. The contract surface stays at six (Storage, Ontology, Updater, Reasoner, Proposer, Collector) — broadening it would force every profile to re-implement a peer table that the edge-minimal profile already provides for free. We promote to a contract when a second implementation arrives:

- SQLite-backed registry for the edge-standard / cloud-full profile so trust history survives restarts;
- gossip-based discovery (mDNS / dat-style) so peers find each other without a static `--peers` flag.

The trade-off: callers couple to `peers.Registry` and `peers.Client` directly. That is acceptable inside `internal/minimal/` (the only consumer in v1). When external implementations appear, we lift the surface into `pkg/contracts/PeerCoordinatorContract` and the existing concrete types become its first implementation.

### Trust mechanics

`Descriptor.Trust` is a float in `[0, 1]`. Three forces move it:

1. **Default at registration** = `0.5`. We neither blanket-trust nor pre-distrust an operator-supplied URL.
2. **Manual override** via `POST /peers/{id}/trust` — operator console + test fixtures.
3. **Automatic soft-penalty** on outbound HTTP failure inside `RecommendPeer` — `−0.05` per failed `/cost` query, clamped at 0. Persistently-down peers drain out of the eligible set over ~20 attempts without being hard-banned on a single transient blip.

Outcome-driven trust updates (boost on successful offload accept, penalty on energy/latency overrun) are designed but not wired in v1. The hook exists — `Registry.UpdateTrust(id, delta)` — and the scenario test demonstrates a `+0.10` bump after a successful offload, but the daemon itself does not yet call it automatically on `/offload` responses. Promoting that to an automatic pipeline is the obvious P7 next step.

### Why `RecommendPeer` uses trust-weighted savings

Two candidate ranking functions:

| Rank by | Effect |
| --- | --- |
| `savings` alone | Picks the lowest-cost peer regardless of reliability — vulnerable to a freshly-added unverified peer winning every recommendation. |
| `savings × trust` | A high-cost-but-reliable peer beats a low-cost-but-suspect one when their trust ratio outweighs the cost ratio. Establishes a clean operational meaning for "trust": *how much do I discount this peer's stated savings*. |

We chose the latter. The product collapses two dimensions into a single ordering, which is good enough for v1 routing decisions. A future profile that needs finer control (e.g. Pareto frontiers when latency and energy both matter) can build a richer ranker without changing the contract.

### `/offload` is the decision interface, not a scheduler

The `POST /offload` handler computes `CostOfAction` locally and answers *would I accept this task within these budgets?* It does not run anything. Two reasons:

1. Execution requires a workload runtime (container manager, function-as-a-service, etc.) that the Semantic Map deliberately does not own. The map is the *brain*, not the *body*.
2. Keeping the decision interface pure makes it cheap and idempotent. The source agent can poll multiple peers in parallel, pick a winner, and only then commit to actually moving work — entirely outside the Semantic Map's surface.

The response carries `expected_latency` and `expected_energy` so the source agent can record an outcome, regardless of whether the actual offload executor ever exists.

### Proof: the headline scenario

`TestScenario_CoordinationOffload` is the load-bearing test for this layer. It wires three in-process agents (A idle, B loaded, C medium), gives each its own `httptest.NewServer` running the minimum surface (`/cost`, `/healthz`, `/offload`), cross-registers them, and walks B through a complete coordination cycle:

1. Pre-flight: print self-cost for A, B, C — assert `A < C < B`.
2. `B.RecommendPeer` → must return A (highest trust-weighted savings).
3. `B → A POST /offload` via `peers.Client` → A accepts within the budget.
4. B updates `Registry.UpdateTrust(A.ID, +0.10)` and prints the before/after table.

The verbose output narrates each step. If the scenario passes with A chosen and trust incremented, the coordination claim is proven end-to-end. The HTTP routes, the CLI subcommand, and the operator-facing surface are plumbing around this core demo.

### v1 security stance

No auth on `/peers` or `/offload`. The deployment target is localhost / lab-network. Production hardening — mTLS, signed peer identities, bearer tokens, or a capability-based access control — is a deliberate P7 concern. Treating it as "out of scope for v1" is documented here so a future reviewer cannot mistake the omission for an oversight.

---

## 11. Operator Tuning Interface

Operators express priorities in natural language. The Tuner maps intent to structured proposition adjustments; the SemanticMap validates and applies them deterministically. The Tuner is **never in the execution path** — offload decisions remain graph-driven and fully traceable regardless of how weights were tuned.

### Pipeline

```
POST /agent/tune {"intent": "prioritize security", "operator": "alice"}
          ↓
TunerContract.ParseIntent(text) → []TuneIntent{PropositionID, Delta}
          ↓
SemanticMap.Tune: resolve current strengths → compute newStrength = clamp(old+delta, floor, ceil)
          ↓
TunerContract.Validate(adjustments) — hard bounds check
          ↓
OntologyContract.SetPropositionStrength × N   ← each emits "set-strength" event
OntologyContract.RecordTune(text, operator)   ← single "operator-tune" event
          ↓
Return []TuneAdjustment: PropositionID, OldStrength, NewStrength, Rationale
```

### Hard bounds

| Proposition class | Floor | Ceiling |
|---|---|---|
| SC-related (P1, P4, P11, P14) | 0.30 | 0.95 |
| All others | 0.10 | 0.95 |

Security propositions have a higher floor: operators cannot fully deprioritize security compliance even under resource pressure.

### V1 rule table (RuleBasedTuner)

| Keyword group | Example phrase | Propositions adjusted |
|---|---|---|
| security, secure, compliance | "prioritize security" | P1 +0.12, P11 +0.12 |
| performance, throughput, latency | "focus on throughput" | P3 +0.12, P2 −0.10 |
| energy, power, efficient | "prioritize energy efficiency" | P10 +0.12, P8 +0.08 |
| reliability, resilience, ha | "prioritize reliability" | P5 +0.12, P15 +0.12 |
| maintainability, simple, admin | "simplify operations" | P7 +0.12, P8 +0.10 |
| connectivity, offline | "offline capability" | P5 +0.12, P13 +0.08 |
| community, ecosystem | "leverage community" | P7 +0.12, P11 +0.08 |

Direction modifiers: "deprioritize / reduce / lower / minimize" negate all deltas. Default (no direction word) = increase.

### SLM back-end (cloud-full)

The `cloud-full` profile will substitute a Phi-3 Mini / Gemma 2B inference call behind the same `TunerContract` interface. The contract, validation, audit trail, and hard bounds are profile-agnostic — swapping the back-end changes no other code.

---

## 12. PoC Deployment (`poc/`)

`poc/` is a self-contained Makefile + shell-script suite that provisions three local VMs, deploys k0s + Netdata + di-agent on each, and runs a coordinator demo. It is the live proof-of-concept for the P7 dissertation claim: *"agents with identical priors diverge under different workload histories and trust-weighted routing self-corrects."*

### VM topology

```
Host (macOS, Apple Silicon)
│
├── diag-1  Ubuntu 22.04 ARM64  k0s single-node  regime=bursty   ← heavy workload
├── diag-2  Ubuntu 22.04 ARM64  k0s single-node  regime=stable   ← light workload
└── diag-3  Ubuntu 22.04 ARM64  k0s single-node  regime=stable   ← idle
```

Each VM: 2 vCPU, 2 GB RAM, 10 GB disk. Provisioned via [Multipass](https://multipass.run/).

Each node runs:
- **k0s** in `--single` (controller+worker) mode — provides the k8s surface that Netdata's `k8s.cgroup` collector observes.
- **Netdata** — system metrics at `localhost:19999`.
- **di-agent** — `edge-minimal` profile, `-netdata-url http://localhost:19999`, `-kd k0s`, polling every 5 s. Runs as a systemd service reading `/etc/di-agent/env` for `NODE_ID` and `REGIME`.

Peer mesh: each agent registers the other two via `POST /peers`. The server derives the peer ID as `sha256(url)[:12]` and initialises trust at 0.5; `05-peers.sh` follows each add with `POST /peers/{id}/trust {"value":0.8}` to set the operational trust. diag-1 uses this mesh for `/recommend`.

### What the binary sees on each node

The `NetdataCollector` polls one chart at a time via:

```
GET /api/v1/data?chart=CHART&points=1&after=-30&format=json
```

and maps:
- `system.cpu idle %` → `CPUUtilization = 1 - idle/100` → RC-adjacent edges
- `system.ram used/(used+free+cached+buffers)` → `MemoryUtilization` → RC-adjacent edges
- `system.net InOctets` (kb/s) → `NetworkRxBps` normalized to [0,1] vs 1 Gbps reference
- `system.net OutOctets` (sign-flipped) → `NetworkTxBps` normalized similarly

At idle k0s, CPU utilization is ≈ 0.05 — well below the Di-Select priors (RC propositions are calibrated to heavier workloads). The bursty regime (α=0.30, N=200) converges faster, so diag-1 accumulates confidence and cost quicker than the stable VMs (α=0.05, N=1000).

**Note on `stress-ng`**: High CPU pushes CPUUtilization above the RC priors. Because P8 (MU→RC, direction −) and P10 (PS→RC, direction −) have negative direction, their contributions subtract from cost when EMA exceeds priors, which can push `ResourceCost` to zero. The coordinator demo works cleanly at idle or light load without `stress-ng`.

### Coordinator demo

`poc/scripts/coordinator.sh` runs N rounds (default 8, `ROUNDS=` env var, `INTERVAL=` between rounds):

1. Query `/cost?taskType=pod-scheduling&nodeID=master` on all three agents → print cost table.
2. Call `POST /recommend` on the highest-cost agent → print recommended peer ID and savings. The response uses `PeerID`/`ExpectedSavings`/`Rationale` (PascalCase).
3. Round `ROUNDS/2`: look up diag-2's real peer ID on diag-1 (via `GET /peers`, match by URL) → `POST /peers/{id}/trust {"value":0.15}`. Trust 0.15 < default min-trust floor 0.5 → diag-2 filtered out.
4. Subsequent rounds: diag-2 remains excluded; recommendation stays on diag-3 (lowest cost in the eligible set).

Actual output observed at idle (no artificial workload):

```
  VM          IP               ResourceCost    Confidence
  ----------  ---------------  --------------  ----------
  diag-1      192.168.252.2    0.035           0.4         ← bursty, converges faster
  diag-2      192.168.252.3    0.026           0.4
  diag-3      192.168.252.4    0.025           0.4

  → diag-1 recommends: e80cbdf42748 (diag-3, trust=0.80)
    savings=0.010; trust-weighted=0.008

  *** Round 3: Trust drain event ***
  Trust drain applied: diag-2 (id=6fed3bd30c6e) trust=0.15 on diag-1

  → Rounds 4–6: diag-1 continues to recommend diag-3 (diag-2 excluded: trust below floor)
```

### Quickstart

```bash
brew install --cask multipass          # one-time
make -C poc all                        # provision → k0s → netdata → agent → peers (~15 min)
make -C poc workload-heavy             # stress diag-1
make -C poc demo                       # 8-round coordinator loop
make -C poc status                     # snapshot /cost from all three
make -C poc teardown                   # delete VMs and purge
```

### Design constraints

- **linux/arm64 binary**: `04-agent.sh` cross-compiles with `GOOS=linux GOARCH=arm64`. On an Apple Silicon host this is a same-arch cross (darwin → linux, same ISA); no emulation layer.
- **Independent single-node clusters**: each VM runs k0s `--single`, not a joined multi-node cluster. The PoC tests agent-level routing decisions, not k8s scheduling. Three separate clusters keep provisioning simple and failure-isolated.
- **No auth**: same v1 stance as the coordination layer — lab network only.
- **`-proposer=false`**: MI proposer disabled on 2-vCPU VMs to keep CPU headroom for workload and measurement.

---

## 13. Natural-Language Explain Layer (`pkg/explain`)

`pkg/explain` adds an operator-facing surface that lets a human ask questions in natural language and get a grounded, cited answer. It exists to make the semantic map talk-to-able: *"Why is my ResourceCost higher than my peers?"*, *"Which propositions are driving the recommendation?"*, *"Should I deprecate P7?"*

The design follows the framing in §8: our graph is the **world model** for an LLM operator agent, and every anchor property (real-world outcomes, frozen rules, human judgment) is preserved because the LLM is a **consumer** of the graph, never a mutator.

### Position in the architecture

```
Operator → POST /explain (question)
             │
             ▼
        pkg/explain (OpenAICompatibleExplainer)
             │
             │  (system prompt: cmd/agent/prompts/explain-v1.md)
             │  (tool schemas: get_cost, get_edges, get_history,
             │                 get_peers, get_recommend, get_graph)
             │
             ▼                              ┌────────────────────────────┐
        OpenAI-compatible LLM   ◀───tool───▶│  SemanticMap (read-only)   │
        (local: Ollama /                    │  All reads bypass HTTP:    │
        llama-server /                      │  in-process Go calls to    │
        LM Studio / vLLM)                   │  the facade methods.       │
             │                              └────────────────────────────┘
             ▼
        Draft answer + citations
             │
             ▼
        Deterministic citation validator
             │  (every cited edge/proposition/peer exists?
             │   values match live graph within Epsilon=1e-3?
             │   answer mentions Pn ⇒ Pn is in citations?)
             │
             ├── valid ──▶ Return ExplainResponse to operator
             │
             └── invalid ─▶ Feed critique back to LLM (reflection loop, max 3)
```

### The two guarantees the layer ships

1. **Groundedness.** Every value in the answer's `citations` array is checked against live graph state before the response leaves the daemon. A hallucinated proposition ID, wrong EMA value, or reference to a deprecated edge is rejected with a critique that goes back to the LLM. If the reflection loop can't produce a valid response within `MaxIterations`, the daemon returns HTTP 422 with both the error message and the partial response — no "confident lies" reach the operator.

2. **No mutation.** The tool set is **read-only** by construction. The LLM cannot call `POST /agent/tune`, `POST /ontology/deprecate`, or any other mutation endpoint. When the operator asks *"what should I change?"* the LLM produces a structured `Proposal` in its response — a suggested endpoint + payload + rationale — that the operator invokes explicitly if they choose to act. The human-judgment anchor from §8 stays intact.

### Provider

v1 speaks the OpenAI `/v1/chat/completions` surface with function-tool semantics. This routes to any local backend that exposes that shape:

| Backend         | Base URL                          |
| --------------- | --------------------------------- |
| Ollama          | `http://localhost:11434/v1`       |
| llama-server    | `http://localhost:8080/v1`        |
| LM Studio       | `http://localhost:1234/v1`        |
| vLLM            | `http://localhost:8000/v1`        |
| Hosted (OpenAI) | `https://api.openai.com/v1`       |

Default: Ollama on `localhost:11434/v1`. The daemon does not ship an LLM binary — install one out of band. `POST /explain` returns HTTP 501 with a clear remediation message when `-explain-provider=none` (the default).

### Configuration flags

```
-explain-provider     none | openai-compatible          (default: none)
-explain-url          http://localhost:11434/v1         (default)
-explain-model        qwen2.5:7b-instruct               (default)
-explain-prompt       cmd/agent/prompts/explain-v1.md   (default)
-explain-api-key      ""                                (env EXPLAIN_API_KEY overrides)
```

### Prompt versioning

The system prompt lives at [`cmd/agent/prompts/explain-v1.md`](../go/cmd/agent/prompts/explain-v1.md), loaded once at daemon startup. Every response records a `prompt_version` field (the first 12 hex chars of `sha256(prompt)`), so a paper's replication package can pin the exact prompt used for reported results. Bump the filename (v2, v3, …) rather than editing v1 in place — old snapshots then remain reproducible.

### Response shape

```json
{
  "answer": "The dominant edge is P10 (PS→RC, prior 0.645, effective 0.62 at confidence 0.60).",
  "citations": [
    {"kind": "edge", "id": "P10", "ema_weight": 0.62, "prior_weight": 0.645, "confidence": 0.60, "n_observations": 15}
  ],
  "confidence": "high",
  "proposal": null,
  "tool_trace": [{"name": "get_edges", "arguments": {"from": "PS", "to": "RC"}, "result_digest": "edges from=PS to=RC count=1"}],
  "model_name": "qwen2.5:7b-instruct",
  "prompt_version": "a1b2c3d4e5f6",
  "iterations": 1
}
```

### Reproducibility stance

`pkg/explain` is on the **operator-facing** surface, not on the ingestion or reasoning path. Every P6 result reported in `research-docs/` uses `-explain-provider=none`: the reasoner, updater, prior-init pipeline, and convergence measurements are pure Go and fully deterministic. The Explain layer is a demo asset and an operator convenience, not a load-bearing component of the scientific claims.

### Tests

- `pkg/explain/tools_test.go` — tool registry + citation validator against live SemanticMap.
- `pkg/explain/openai_test.go` — reflection loop end-to-end against a scripted mock LLM. Covers happy path, tool-then-answer, and fabrication-then-fix.
- `cmd/agent/explain_route_test.go` — route wiring: 501 when disabled, 400 on malformed input, 200 with grounded answer through a mock LLM.

None of these require a real LLM to be running. `go test ./...` stays green on any machine.

---

## 14. Explain v2 — Planning, Critic, Sessions, Streaming

§13 describes the v1 surface: one answering agent, tool access, a deterministic validator, and a reflection loop. v2 adds the two Ng patterns v1 was missing and the production hardening the v1 live smoke exposed as necessary.

Everything here is **opt-in per request**. A v1-shaped body (`{"question": "..."}`) still produces v1 behaviour.

### The full pipeline

```
POST /explain {question, session_id?, use_planner?, use_critic?, stream?}
   │
   ├─▶ [session]  resolve or mint · replay prior turns · flush tool cache if
   │              the ontology history watermark advanced
   │
   ├─▶ [planning] ── planner LLM (NO tools) ──▶ Plan{steps:[{tool,args}...]}
   │       │                                       │
   │       │                                validatePlan: unknown tool?
   │       │                                step with no action? no tool steps?
   │       │                                       │
   │       └──▶ Go executes the plan ─────────────┘
   │              (log-and-continue on step failure,
   │               session cache consulted per step,
   │               budget-capped with truncation noted)
   │                       │
   │                  Evidence bundle
   │                       │
   ├─▶ [answering] ◀───────┘  answering LLM (WITH tools, may fetch more)
   │       │
   │       ▼
   ├─▶ GATE 1  deterministic Validate() ── fail ──▶ citation-diff critique ──┐
   │       │ pass                                                            │
   │       ▼                                                                 │
   ├─▶ GATE 2  critic LLM (NO tools) ───── reject ─▶ critic critique ────────┤
   │       │ approve                                                         │
   │       ▼                                                            revise
   └─▶ Response {answer, citations, plan, critic_verdict, usage, session_id} ┘
                                                          (≤ MaxIterations)
```

### Why the planner gets no tools

The planner emits JSON naming which tools to call; **Go** executes exactly those. The alternative — letting the planner call tools directly — collapses planning and execution back into one opaque loop. Keeping them separate means:

- The plan is inspectable *before* it costs anything, so a plan naming `drop_database` is rejected structurally, not discovered at dispatch time.
- The plan appears in the response for audit. An operator can see the strategy, not just the outcome.
- Tool execution is deterministic Go: same plan, same tools, same order, every time.

### Why the critic gets no tools either

Tool access would let the critic fetch data the answering agent never saw, producing objections that cannot be acted on — *"you missed edge X"* when X was never in the evidence bundle. Reviewing the same evidence keeps the loop closed and every critique actionable.

### The two gates are not redundant

They cover genuinely different properties:

| | Gate 1 — deterministic validator | Gate 2 — critic LLM |
|---|---|---|
| Always on? | Yes | Opt-in (`use_critic`) |
| Catches | Fabricated IDs, wrong numeric values, deprecated edges cited as live, malformed proposals | Wrong causal reading, direction-sign errors, unsupported conclusions, question drift, miscalibrated confidence |
| Cost | Free (pure Go) | One LLM turn per round |
| Can be wrong? | No — it compares against live state | Yes — it is a model |

The motivating case is real. During the v1 live smoke, `qwen2.5:7b` produced *"P7: Community & Ecosystem → Resource & Cost"*. P7 exists; the cited numbers were correct; Gate 1 passed it. The claim was still false — P7 is CE→MU. Structural grounding and semantic correctness are different properties, and only Gate 2 covers the second.

Gate 1 runs first and unconditionally: a fabricated citation never reaches the critic, let alone the operator.

### Degradation policy

Every optional stage fails **open**, never closed:

| Failure | Behaviour |
|---|---|
| Planner returns unparseable output | Answering agent is told planning failed; gathers evidence itself (the v1 path) |
| Plan is structurally invalid | Same — plan rejected, request continues |
| A plan step's tool errors | Recorded in the evidence bundle as `ERROR`; remaining steps still run |
| Plan exceeds tool budget | Executor stops and notes the truncation *in the bundle*, so the answering agent knows its picture is incomplete |
| Critic errors or is unreachable | Structurally valid answer ships with the degradation recorded in `critic_verdict.issues` |
| Critic rejects with no stated issue | Upgraded to approval — burning a revision round on "make it better" helps nobody |

The one thing that never degrades is Gate 1. A response that fails deterministic validation after `MaxIterations` returns HTTP 422 with the partial response and every mismatch enumerated.

### Session memory

`session_id` opts into multi-turn. The store is in-memory with LRU eviction (defaults: 100 sessions, 20 turns each, 32 cached tool results per session, 60 s tool-cache TTL, 30 min idle TTL).

Two things live in a session:

1. **Prior turns**, replayed into the answering agent's context so an operator can ask *"what about diag-2?"* without restating the question.
2. **A tool-result cache**, keyed by `(tool_name, canonical(args))`. Flushed wholesale when the ontology history watermark advances — any mutation (tune, deprecate, strength set, construct or proposition added) invalidates it.

Whole-cache invalidation rather than per-key is deliberate: the graph is 7 nodes and 15 edges. Reasoning about which cached results a given mutation could have affected costs more, in code and in bug surface, than just refetching.

Sessions are **not persisted**. They carry no scientific role — P6 results come from the reasoner, not the Explainer — so crash-recovery machinery would be complexity without a customer. The durable substrate already exists: `/graph` and `/history`.

An unknown `session_id` returns an error rather than silently minting a fresh session. A client that lost its ID should find out, not get an amnesiac conversation that looks like it worked.

### Streaming

`"stream": true` switches the response to **NDJSON over chunked encoding** — one compact JSON event per line.

```
{"event":"session","session_id":"a3f2..."}
{"event":"planning"}
{"event":"plan","plan":{"approach":"...","steps":[...]}}
{"event":"tool_call","tool":"get_cost","args":{"node_id":"master"}}
{"event":"tool_result","tool":"get_cost","digest":"cost node=master rc=0.0350"}
{"event":"answering","iteration":1}
{"event":"validating","iteration":1}
{"event":"critic","iteration":1}
{"event":"critic_verdict","iteration":1,"verdict":{"approved":true}}
{"event":"final","response":{...}}
```

Every stream terminates in exactly one `final` or `error`, so a client loops until it sees either. Clients should tolerate unknown event kinds — new markers may be added without a schema bump precisely because that terminal guarantee holds.

NDJSON rather than SSE because the consumer is a CLI or script, not a browser `EventSource`. Line-delimited JSON parses with a plain line reader in any language and pipes into `jq`; SSE would only buy browser auto-reconnect, which nothing in this deployment wants.

`StreamingExplainer` is a separate optional interface rather than a widening of `Explainer`, so `DisabledExplainer` stays trivial and a streaming request against a non-streaming backend gets a clear 501 instead of a silent downgrade.

### Structured decoding

`response_format: {"type":"json_object"}` is sent **only on tool-free turns** (planner, critic, and the final answering turn once evidence is gathered).

This matters more than it looks: constraining output to a JSON object during a *tool-calling* turn prevents the model from emitting `tool_calls` at all. The symptom would be "the model ignores its tools" — an expensive thing to debug from the outside.

### Cost accounting

Every response carries `usage`:

```json
{
  "prompt_tokens": 2481, "completion_tokens": 193, "total_tokens": 2674,
  "wall_clock_ms": 4120, "tool_calls": 3, "tool_cache_hits": 1, "llm_turns": 3
}
```

`llm_turns` counts planner + answering + critic + revision turns separately, so the cost of each opt-in feature is directly measurable. This is what lets the paper defend the *"the LLM sits on the operator-facing surface, not the hot path"* claim with numbers rather than assertion.

### Configuration

```
-explain-provider         none | openai-compatible          (default: none)
-explain-url              http://localhost:11434/v1
-explain-model            qwen2.5:7b-instruct
-explain-prompt           cmd/agent/prompts/explain-v1.md
-explain-planner-prompt   (derived: <prompt-dir>/planner-v1.md)
-explain-critic-prompt    (derived: <prompt-dir>/critic-v1.md)
-explain-keep-alive       30m
-explain-sessions         true
-explain-api-key          ""   (env EXPLAIN_API_KEY wins)
```

A missing *derived* planner/critic prompt disables that stage with a log line; an explicitly-specified path that fails to load logs a warning and disables the stage. Either way the daemon boots — a partial prompt directory degrades features, it does not block startup.

### Prompt versioning

Three prompts, each versioned by filename: `explain-v1.md`, `planner-v1.md`, `critic-v1.md`. The answering prompt's `sha256[:12]` is stamped on every response as `prompt_version`. Bump the filename rather than editing in place, so results reported against v1 stay reproducible.

### What v2 does not change

- **The reasoner, updater, Bridge, and prior-init pipeline remain pure deterministic Go.** No LLM touches the ingestion or reasoning path.
- **The tool registry is still read-only.** Planner, answering agent, and critic all get the same six read tools. None can mutate.
- **`-explain-provider=none` remains the default.** All P6 results in `research-docs/` are produced with the Explain layer off.

### Tests

| File | Covers |
|---|---|
| `pkg/explain/session_test.go` | LRU eviction, touch-on-get, TTL expiry, mutation-driven invalidation, turn-buffer trimming |
| `pkg/explain/planner_test.go` | Plan parsing (fences, prose, truncation), structural validation, evidence collection, error continuation, budget capping, cache hits |
| `pkg/explain/critic_test.go` | Verdict parsing, issue truncation, unactionable-rejection upgrade, prompt assembly, revision formatting |
| `pkg/explain/streaming_test.go` | Event ordering, terminal event on success and failure, payload correctness, nil-emitter safety, interface satisfaction |
| `pkg/explain/openai_test.go` | v2 integration: planner-runs-tools, planner-failure fallback, critic approve, **critic catches structurally-valid-but-wrong**, critic-failure ships anyway, session mint and persist, unknown session, usage accounting |
| `cmd/agent/explain_route_test.go` | NDJSON content type and parse, 501 for non-streaming explainers |

All run against a scripted mock LLM. No test requires Ollama.
