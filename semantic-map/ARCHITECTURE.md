# Semantic Map — Architecture

Design rationale and decision record. Update this file when a contract, profile, MetricType, or structural decision changes. For usage (running, API, compliance), see [README.md](README.md).

---

## Table of Contents

- [1. Core Concept](#1-core-concept)
  - [The agent at a glance](#the-agent-at-a-glance)
  - [Component reference](#component-reference)
  - [The four request lifecycles](#the-four-request-lifecycles)
- [2. Contract Architecture](#2-contract-architecture)
  - [The seven contracts](#the-seven-contracts)
  - [Actuators: a dumb actuator, a smart loop](#actuators-a-dumb-actuator-a-smart-loop)
  - [What is deliberately not a contract](#what-is-deliberately-not-a-contract)
  - [Behavioral guarantees](#behavioral-guarantees)
  - [End-to-end validation: integration scenarios](#end-to-end-validation-integration-scenarios)
- [3. Deployment Profiles](#3-deployment-profiles)
- [4. Language Strategy](#4-language-strategy)
- [5. Telemetry Pipeline](#5-telemetry-pipeline)
  - [CollectorContract](#collectorcontract)
  - [MetricType catalogue](#metrictype-catalogue)
  - [How a relationship's strength is learned](#how-a-relationships-strength-is-learned)
  - [The graph surfaces are a projection](#the-graph-surfaces-are-a-projection)
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
- [15. Autonomous Candidate Review (`pkg/autonomy`)](#15-autonomous-candidate-review-pkgautonomy)
- [16. Acting (`pkg/relief`, `pkg/policy`, `pkg/runtime`)](#16-acting-pkgrelief-pkgpolicy-pkgruntime)

---

## 1. Core Concept

The Semantic Map has two layers that are always present simultaneously:

```
┌────────────────────────────────────────────────────────────────┐
│  Layer 2 — Evidence (dynamic)                                  │
│  Statistical descriptors updated by live telemetry             │
│  "In THIS cluster, under THESE workloads, here is reality"     │
├────────────────────────────────────────────────────────────────┤
│  Layer 1 — Backbone (structure only, no magnitudes)            │
│  Constructs + causal propositions, declared in domain_spec.json │
│  "What matters and how things relate"                          │
└────────────────────────────────────────────────────────────────┘
```

**The cold-start arc:** on day one the agent has no magnitudes at all, and says so. A
relationship's strength is learned from the machine, on two timescales, with an
operator able to override either:

```
effective = assertion    if an operator set one          — takes effect in full
          | established  else, once pairs have accumulated — the machine's baseline
          | recent       else, once anything has folded    — the last few pairs
          | unknown      otherwise
```

`Basis()` names which of the four answered, and the fourth is the one that matters:
"I have not measured this yet" is representable, so no caller is handed a figure as
though it had been measured.

**Why two learned layers.** Both smooth the same input — |r| over a trailing window of
pairs — at different time constants. `recent` (α = 0.20, memory ≈ 5 pairs) answers what
is happening now. `established` (α_slow = 0.001, memory ≈ 2.3 workload regimes) answers
what is normal for this machine. Their *difference* answers how unusual the present is,
which is the quantity an agent actually wants and the honest replacement for measuring
divergence from a calibration — that only ever described the calibration.

The separation is real on replayed telemetry. One agent, three regimes in sequence:

| after | recent | established | basis |
| ----- | ------ | ----------- | ----- |
| *(cold start)* | — | — | `unknown` |
| `dp_redis_density` (528 pairs) | 0.887 | 0.442 | `established` |
| `cp_heavy_12client` (729) | 0.555 | 0.527 | `established` |
| `idle` (911) | 0.007 | 0.558 | `established` |

The last row is the point. An idle stretch drives the recent estimate to 0.007, because
an idle machine exhibits no association between resource and pressure. A single-layer
agent would take that as having *learned* that resource does not affect pressure. The
established layer holds 0.558 — what this machine does when it is doing anything — and
the gap is the signal that the present is atypical.

**α_slow sits on a measured trade-off, and is not a derived optimum.** The criterion is
the one the layer's purpose dictates: a baseline must distinguish machines without
depending on the order the machine happened to be exercised in. An offline fit over 231
traces reported an interior maximum in that ratio; sweeping the constant against the
daemon itself, over 135 accumulated streams, showed the maximum to be an artefact of the
offline streams being ~6× shorter than a deployment's. At deployment scale the ratio
rises monotonically toward slower constants and peaks where the memory exceeds the whole
history — the degenerate case, where the estimate is a running mean and order-invariant
by definition.

What the sweep establishes is the trade-off: order-invariance improves 16-fold from
α = 0.20 to 0.0001 while the baseline's span over a stream falls from 0.539 to 0.099, so
the slow end buys stability by ceasing to move. 0.001 keeps ten times the recent layer's
order-invariance and about a third of its responsiveness. Pinning one value needs a
stated requirement about how fast a baseline should follow a persistent change, which is
a modelling decision rather than a measurement. See `convergence/derive_alpha_slow.py`
(offline) and `convergence/sweep_alpha_slow.sh` (daemon-fidelity).

**The estimate is bias-corrected**, dividing by 1 − (1−α)^n, and that is substance
rather than polish. Measuring the uncorrected form is what showed why: across five
replays of one workload on one machine its end value varied by σ ≈ 0.32 against 0.025
for the fast layer, because with ~89 pairs per run and a memory of 1000 it never left
its transient. A layer whose value depends mostly on its own initialisation is not a
baseline.

**Establishing takes longer than one run** — ~1000 pairs, about two workloads or half an
hour at 1 Hz. So the established layer only means anything because the map persists
across restarts (§ Persistence): an agent that has watched a machine for a week returns
with a baseline, and that is what makes the persistence guarantee load-bearing rather
than a convenience.

**What is stable and what is not:**

| Element                               | Stable?                                                          |
| ------------------------------------- | ---------------------------------------------------------------- |
| The domain model as a whole           | Declared in data — `domain_spec.json`, loaded at startup         |
| Proposition directions                | Yes — a direction never reverses once declared                   |
| Proposition magnitudes (edge weights) | No — learned from evidence                                       |
| New constructs and propositions       | Possible — added by the Proposer contract or `AddConstruct`      |

**The domain model is data, not code.** The binary contains no construct or
proposition identifier. `domain_spec.json` declares the constructs, the
propositions over them with their directions, the metric-to-construct routing,
the adjustment floor and ceiling, and the tuner's intent vocabulary; the daemon
loads it via `domain.path`. It is optional: a node may have no specification to give
it, and the state model needs none — it admits whatever a node exhibits and
discovers how those properties relate. Without one, only the operations defined
over declared constructs are unavailable (cost and its recommendations, tuning),
and they return `contracts.ErrNoDomainModel` (`501`) rather than a cost computed
over constructs nobody declared. Two consequences of keeping it data: the
graph a given deployment ran can be reconstructed from an artefact rather than
from a binary, and a property that appears while the cluster is running is
admitted by adding a construct and a route rather than by rebuilding. The
compliance suites assert against whatever specification is loaded — non-emptiness
and referential integrity of every proposition's endpoints — not against a fixed
count.

**Whose state is this?** One agent per machine, and its map holds that machine's
state. That is why nothing in the model has a machine dimension and none is needed:
the map *is* one machine's state, and it says whose in its owner. The backbone stays
construct-level because the causal claims do not differ by host — one shape, one
model per agent. What a peer knows travels as a labelled snapshot and is never merged
in (§5, Peer state).

Cluster-level questions are answered by asking peers (§6), not by one agent
accumulating everyone's telemetry. The alternative — every agent modelling the whole
cluster — requires telemetry fan-out and, worse, averages relations across machines
that may be different physical systems: an x86 control-plane host and a Cortex-A72
worker do not share a resource-to-pressure relation, so one edge spanning both is a
mean over incomparable mechanisms. The effect is measurable rather than theoretical.
When the pair tracker briefly keyed on construct alone and mixed nodes, the conflict
pair on RC→PS separated the opposite way under load; per-node pairing reversed the
conclusion.

Two mechanisms enforce this. `node.id` is the agent's identity, not merely the label
it stamps on its own samples, and `node.ingest_scope: self` (the default) rejects samples
belonging to another machine with a distinguishable error. `GET /cost?node=X` answers
409 when X is not this agent, naming that machine's own URL if it is a known peer,
because returning local numbers under a peer's name is a fabrication — and ignoring
the parameter, which is what the route did before, was exactly that. Replaying a
whole testbed into one daemon is legitimate and available via `node.ingest_scope: any`,
which logs at startup that the resulting graph is an aggregate and not a deployment
topology.

**Which constructs belong here.** A quantity that cannot change while the cluster
runs is not state; it is a property of the platform, fixed when the platform was
chosen. The committed specification therefore declares the two constructs this deployment
both exhibits *and* measures — RC (resource cost) and PS (performance) — and the two
Di-Select propositions over them whose declared direction the telemetry does not
contradict: **P3 on RC→PS (positive)** and **P10 on PS→RC (positive, corrected from
Di-Select's stated sign)**. Security posture, maintainability and ecosystem maturity
are selection-time knowledge and stay with Di-Select. Reliability is genuine runtime
state but no MetricType routes to it yet.

Two exclusions were made by measurement rather than on principle, and both are
recorded in `prior_weights.json` beside the propositions they removed. **CO** carried
only network throughput, which arrives at ~10⁻⁵ of the link capacity it is normalised
against, leaving the derived construct at 1.4 × 10⁻⁴ — fittable and unable to move any
answer — so CO and P13 with it were dropped. **P2** was dropped as a duplicate: its
subject is scheduling throughput, which is not measured here, and once its sign is
corrected for this deployment's polarity it is P3's contrapositive on the same signal.
A third route, `io_pressure_ratio`, was removed for a sharper reason: it was observed
306 times and read exactly zero on every one of them, and since a derived property is
the mean of its members, a constant-zero member **halved the construct it fed** —
derived PS reported 0.0247 against a true stall fraction of 0.0495.

The reason to exclude rather than seed-and-freeze is mechanical. A relationship with
no observations reports `unknown` and has no effective value, so there is nothing for
the sensitivity sum to read and it does not enter the arithmetic at all. Under the
seeded design the conclusion was the same by a longer route: `effective ≡ prior` for
an unobserved edge, so its deviation from that prior was exactly zero at every sample
count and after any operator adjustment. Either way, frozen relations change no
decision; what they do change is `mean_confidence`, which averages over the
relationships present, and `n_unknown`, which counts the ones with nothing behind
them. Carrying them makes the agent's self-report worse without making its decisions
better.

**One case is not inert, and it is why the scoping rule is stronger than "carry what
is observable".** A relationship whose declared sign its machine contradicts *does*
collect pairs — its gate refuses each one, so it holds a measured strength of zero,
which is indistinguishable from a quiet system by the strength alone. Two of the four
propositions the specification carried until recently were in that state on every
cluster, contradicted in 45% to 100% of paired observations. The map now declares a
polarity per construct and per metric route, reconciles the two at ingestion, and
counts sign agreements and conflicts per relationship, so a wrong declaration is a
number the agent publishes rather than an inference a reader has to make.

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
                            ║   │ Backbone: constructs + propositions (spec)  │ ║
                            ║   │ Evidence: per-edge EMA + confidence + n_obs │ ║
                            ║   │ Audit:    append-only OntologyEvent log     │ ║
                            ║   └─────────────────────────────────────────────┘ ║
                            ╚════════════════════════▲══════════════════════════╝
                                                     │ update_edge()
                            ╔════════════════════════╧══════════════════════════╗
   /sys/fs/cgroup ─────────▶║  LAYER 1 · INGESTION                              ║
   Netdata :19999           ║                                                   ║
   parquet replay           ║   Collector ──samples──▶ state model (properties) ║
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
| **Collector** | `pkg/collectors` (built-ins in `internal/minimal`, `internal/scripted`) | Read raw metrics from cgroup / Netdata / a plugin; emit typed `MetricSample`s. The configured list (`sources.collectors`) runs under one Supervisor | Collection loop tick (`sources.interval`) | Optional — `POST /ingest-sample` works without one |
| **Routing** | `domain_spec.json` | Say which construct summarises a metric — data, not code | Every sample, to tell the Proposer which construct moved | Yes (a table in the spec) |
| **State model** | `pkg/statemap` | Hold the properties the system exhibits and the relationships between them; recompute what derives; fold paired observations into learned strengths; journal every change and decision | Every sample, and every answer | Not a contract — one implementation, and the agent's single model |
| **Storage** | `internal/minimal` | Hold node + edge descriptors as a multigraph | Every read and write | Yes |
| **Ontology** | `internal/minimal` | Own the backbone: constructs, propositions, validation, audit log | Reasoning, mutations | Yes |
| **Reasoner** | `internal/minimal` | Turn graph state into `ActionCost` / `PeerRecommendation` with a rationale | `/cost`, `/recommend`, `/simulate` | Yes |
| **Proposer** | `internal/minimal` | Mine observation history for candidate new edges (MI correlation) | Background, opt-in (`map.proposer.enabled`) | Optional |
| **Tuner** | `internal/minimal` | Parse operator intent text → proposition-strength deltas | `POST /agent/tune` | Optional |
| **Peers** | `pkg/peers` | Peer registry + trust; HTTP client for remote `/cost` | `/recommend`, `/peers`, `/offload` | Optional |
| **Explain** | `pkg/explain` | Natural-language Q&A grounded in the graph; planner + critic + sessions | `POST /explain` | Optional (`explain.provider: none` default) |
| **Profiles** | `pkg/profiles` | Wire concrete implementations to each contract at startup (not collectors: those are `sources.collectors`) | Once, at boot | Yes |

### The four request lifecycles

Four distinct things can happen to this daemon. Each takes a different path.

**① Telemetry arrives** — the loop that makes the agent learn.

```
Collector.Collect()
  └─▶ []*MetricSample {NodeID, MetricType, Value, EventID}
        ├─▶ polarity: NormalizeForConstruct — reflect within range if route and
        │             construct disagree (v' = lo + hi − v), once, before storing
        └─▶ statemap.ObserveEvent(property, value, at, eventID)   ← recorded FIRST,
              │        so a metric no route names still becomes a property
              ├─▶ property: ema += α(value − ema); n_obs++; confidence = n_obs/N
              ├─▶ routing: MetricType → construct (e.g. cpu_utilization → RC)
              │            → derived property recomputed from its members
              └─▶ paired estimator, per relationship incident to what moved:
                    both endpoints inside the tolerance window → one pair;
                    strength = |r| over the window, zero if the sign is
                    contradicted; SignAgreements/SignConflicts advance either way;
                    recent EMA at α, established EMA at α_slow, bias-corrected
```
*Idempotent per `(property, event_id)` — replaying the same sample changes nothing:
not the value, not `n_observations`, not the estimator's pair window.*

**② A decision is requested** — the loop that makes the agent useful.

```
GET /cost?task=pod-scheduling&node=master
  └─▶ Reasoner.CostOfAction
        ├─▶ cost roles from spec.CostModel — which construct is resource, which pressure
        ├─▶ state.Decide(id) → DecisionBuilder: every read is recorded by the reading
        ├─▶ LEVEL      = b.Property(construct).Value        ← the estimate itself
        └─▶ SENSITIVITY, reported beside the level, never added into it:
              for each relationship INTO the construct, source property present:
                eff, known := rel.Effective()   ← assertion | measured | established | recent
                if !known { continue }          ← absent from the sum, not a zero
                sum += eff · sign(direction)
        └─▶ ActionCost {ResourceCost=level, ResourceSensitivity=sum,
                        Confidence, Rationale, GraphPathUsed, DecisionID}
```
*Pure read. No state changes. Always returns a rationale naming the relationships used
and a `DecisionID` that reproduces its inputs afterwards via `GET
/state/decisions/{id}`. Retired relationships leave the traversal, so deprecation needs
no separate filter here. The level/sensitivity split is empirical: on 182 replayed runs
the observed level ranked the next interval's pressure at 0.622 top-1 accuracy, and
adding the relation term degraded it monotonically to 0.582 at unit weight with no
mixing coefficient improving on the level alone. Sensitivity answers the question the
level cannot — what happens if the source construct changes — which is what
`SimulateOutcome` asks and `CostOfAction` does not.*

**③ The graph is mutated** — the loop that keeps the agent honest.

```
POST /ontology/deprecate {"proposition_id":"P7","reason":"..."}
  └─▶ Ontology.Deprecate
        ├─▶ mark Proposition.Deprecated = true      (soft delete — never removed)
        ├─▶ sync EdgeDescriptor.Deprecated in Storage
        └─▶ append OntologyEvent {actor, kind, target, timestamp}
              └─▶ readable forever via GET /history
```
*Four operator mutations exist, all on the facade: `SetPropositionStrength`, `Deprecate`, `AddConstruct`, `AddValidatedProposition`. Each reaches the state model, which is what gives it an effect; each is journalled. The declaration layer itself carries only the last two, because only those change the vocabulary. Construct removal and direction reversal are impossible by design.*

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
  Metric sources         Semantic Map
  (cgroup / Netdata / plugins, in-process or exec/http)
        │
   [Supervisor of Collectors] ──samples──▶ [state model: properties, relationships]
                                                              │
                    ┌─────────────────────────────────────────┘
                    ▼
        ┌───────────────────────────────────────────┐
        │              SemanticMap facade            │
        │  cost_of_action()  recommend_peer()        │
        │  simulate_outcome()  tune()                │
        └───┬──────────┬──────────┬────────┬───────┘
            │          │          │        │
      state model  Ontology  Reasoner  Proposer/Tuner
            ▲                                        
            │ read-only                              
   ┌────────┴──────────┬──────────────────┐          
[peers]            [explain]         [control surface]
 registry       planner·critic       HTTP · mapctl · /ui
 + client       + validator          (§9)
   (§10)          (§13–14)

  [Runtime] ──workloads──▶ [relief loop] ──may touch?──▶ [policy]
  dry-run · kubernetes          │                       (pkg/policy)
                                ▼
                 [Acting: Actuators + ledger] ──Read/Set──▶ orchestrator
                 cpu.limit · DryRun · Static      (pkg/actuators)
```

The Collector lives outside the SemanticMap facade — it feeds it. The three components below the facade are *consumers*: they read model state and expose it, but only the facade's own mutation methods can change it.

**Collectors are plugged in, not wired.** Which collectors run is configuration (`sources.collectors`, an ordered list), not a profile's code: `pkg/collectors` keeps a registry of types (`cgroup`, `netdata` and `script` are built in; an out-of-tree Go package adds one with `Register` in its `init`) and builds the list into a **Supervisor**, which is itself a `CollectorContract`, so the collection loop still sees one source. The Supervisor replaces `MultiCollector`, which fanned out to its members and halted the tick at the first member's error, so one broken source silenced the rest. Under the Supervisor each member runs concurrently under its own timeout; an error, panic or timeout costs that member its samples for the tick and puts it in exponential backoff, and the others deliver. A Go member cannot be cancelled, so a call that outlives its timeout is abandoned, the member shows as `stuck`, and it is not called again until it returns; its return is then recorded as a fault at that moment, with how it ended as the last error, so state, backoff deadline and last error agree as after any fault and status is never left describing the timeout of a call that has since reported something else. The Supervisor also enforces the contract on every member, built-ins included: a sample with an invalid type or subject, no unit, a foreign `node_id`, or a unit or range that differs from the member's first declaration of that type is dropped and counted, and the rest of the batch is kept. What each member is doing is `GET /collectors`.

**One owner per node-level type.** Two collectors can both measure a node-level quantity — cgroup and Netdata both report `cpu_utilization`, measured differently — and two producers alternating on one property would mix in its EMA. A node-level type is owned by the earliest-listed member that has emitted it; another member's samples of it are dropped and counted. Ownership is sticky across the owner's backoff and moves only to an earlier-listed member. This replaces a hidden coupling the old wiring had (Netdata stopped emitting node types whenever cgroup was configured too) with a rule the list order states, plus Netdata's explicit `collapse` flag. Subject-scoped samples are keyed `type@subject` and have no owner.

**The contract has a wire form.** A collector need not be Go or in the agent's process: an `exec` entry starts a program that speaks wire protocol v1 (JSON messages: a hello once per start, then one collect per tick) as newline-delimited lines on its stdin and stdout, and an `http` entry POSTs the same messages to a URL. The protocol is a transport for `CollectorContract`, not a second contract: the adapters (`ExecCollector`, `HTTPCollector`) implement the contract, the same guarantees apply, and the same compliance suite certifies a plugin. A plugin that cannot complete its hello refuses the agent's start rather than running as a silent gap.

### What is deliberately not a contract

The contract set is the five in `pkg/contracts` plus Runtime (`pkg/runtime`, the sixth) and Actuator (`pkg/actuators`, the seventh). Three substantial components sit outside it on purpose:

| Component | Why it is concrete, not a contract |
|---|---|
| **State model** (`pkg/statemap`) | One implementation, and it is the agent's model rather than a pluggable store. A contract here would invite a second implementation, which is the arrangement §2 just finished removing. |
| **Peers** (`pkg/peers`) | One implementation exists. Promoting it to a contract before a second one (SQLite-backed registry, gossip discovery) would be designing an interface against a sample size of one. |
| **Explain** (`pkg/explain`) | Same reasoning, plus: it is an operator convenience, not part of the agent's decision path. A contract would imply the daemon depends on it. It does not — the default is `explain.provider: none`. |

The rule we hold to: **no new contract without a second implementation that needs it.** Interfaces derived from one example encode that example's accidents. Each of these three gets promoted the day a real second implementation arrives, and not before. Runtime meets the rule: it is a contract because it has `dry-run` (in memory, what the loops are tested against) and `kubernetes` (`pkg/runtime/kubernetes`, §16) as two implementations the loops must not tell apart. Actuator meets it too: `cpu.limit` is the Kubernetes resize actuator on a Kubernetes runtime and `Static`, an in-memory stand-in, on any other, and the loop cannot tell them apart.

### The seven contracts

| Contract      | Responsibility                                              | Key guarantees                                                                                            |
| ------------- | ----------------------------------------------------------- | --------------------------------------------------------------------------------------------------------- |
| **Collector** | reads a source and emits `MetricSample`s; may scope a sample to a subject; declares unit, and range where the quantity has one, per type. In process, or out of process over wire protocol v1 (`exec`, `http`) | Pure read; deterministic `event_id`; every type a `collect()` emits is in the `available_metrics()` that follows it; never raises on empty data |
| **Ontology**  | The declaration layer — which constructs exist, which propositions relate them, whether a proposed one is valid | Returns whatever the loaded specification declares, with every proposition endpoint a declared construct; constructs and propositions are append-only, never removed or direction-reversed. Holds **no** strength and **no** history: a proposition's magnitude is its relationship's prior and its withdrawal is that relationship's retirement, both in the state model, and the journal is the one audit record |
| **Reasoner**  | Produce agent decisions with traceable rationales           | Every result includes a non-empty rationale referencing the properties and relationships read; `SimulateOutcome` is pure (read-only) |
| **Proposer**  | Detect statistical patterns suggesting new backbone claims  | Never modifies the model or the Ontology directly; an operator's `Reject` permanently suppresses within session |
| **Tuner**     | Map natural-language operator intent to proposition strength adjustments | Parses intent; resolves current magnitudes; clamps to `[floor, 0.95]` with a raised floor on SC-adjacent propositions; emits one `operator-tune` audit event per invocation. See §11 |
| **Runtime**   | The orchestrator-neutral inventory of the node: the workloads it runs, with their labels, QoS, UID and `resources` (`cpu` in cores, `memory` in bytes, each a request and a limit) | Lists every workload it has, so one absent from the list is really gone; the result is the caller's to keep. It changes nothing. Implementations: `dry-run` (in memory) and `kubernetes` (`pkg/runtime/kubernetes`, this node's pods; §16) |
| **Actuator**  | One action on one resource of a workload: a `Descriptor` (action, subject kinds, resource, unit, capacity fact, least value, the node property it relieves, the usage it moves), `Check` (why it cannot apply, `""` when it can), `Read` and `Set` | `Read` is the setting now in force, with "not set" distinct from zero; `Set` is idempotent and can remove the setting; a workload that has gone is `ErrGone` from both. It keeps no record of what it replaced: the agent's ledger does (below). Built by name from a registry (`Register`/`Build`), like collectors. Implementations: `cpu.limit` (`pkg/runtime/kubernetes`, the pod resize subresource) and `Static`; `DryRun` wraps any actuator so `Set` changes nothing outside the process. Policy (`pkg/policy`) decides what the agent may touch; an actuator only does what it is told |

### Actuators: a dumb actuator, a smart loop

An actuator reads and sets one setting; it decides nothing and remembers nothing. Everything that needs judgement or memory is the agent's, in `Acting` (`pkg/actuators`): it asks the actuator's `Check` before a change, reads the setting before its first change to a (action, subject) and records it as the entry's `Original`, sets the new value, and records what reads back as `Applied`, since an actuator may quantise (CPU is set in whole millicores). A further step keeps the first `Original`, so one revert gives back what the workload had before the agent touched it, never an intermediate cap; reverting is `Set(Original)`, including "no setting".

**Why undo is the agent's.** The Kubernetes runtime used to record each change on the pod, in an annotation holding the JSON of the undo. That put the agent's memory in the system it changes: a second write per change, `patch` on every pod in the cluster for a record, rules for a record another node or a template wrote, and an ordering problem (record first, so a resize that timed out was not lost). It also only fits an orchestrator with somewhere to write, and only an action whose object has annotations; a cgroup file has none. A record the agent keeps itself is the same for every action and every orchestrator, needs no write access beyond the setting itself, and can be checked against what the system says now: the agent's record is a claim, the setting read back is the fact.

**The ledger.** `Ledger` holds one `Entry` per outstanding change: action, subject, decision id, original (value and whether one was set), applied, expiry, node. It is a file, `actions.json` beside `persist.file` (`/var/lib/di-agent/actions.json` without one), written whole to a temp file, synced and renamed over, so a crash leaves the old file or the new and never half of one. Every entry is validated on `Put` and on load (an action, a subject and a node; finite values; a committed entry records no step before it; no original value without `original_set`), so a bad entry on disk refuses the ledger, naming the file and the entry; a `Put` that would change an entry's original is refused, since the original is what the setting was before the agent's first change. **Written ahead.** `Apply` records the entry before it sets anything, `pending`, with the value asked for as `Applied` and, for a further step, the step before it as `prev_applied`, so a change is never in force without a record, whatever happens next: a crash, or a resize whose call timed out after the API server took it. The setting is then read back under a fresh 10s context, not the caller's: a `Set` that ran out the pass deadline may still have taken effect, and only a read can tell. A read-back that shows the change commits the entry with what is in force (an actuator may quantise); after a failed `Set`, one that shows the setting from before restores the entry before, or none; one that cannot be read leaves the entry pending for the next reconcile. A change whose committed record cannot be saved is rolled back to what it replaced, since the agent could never give it back. Relief on a runtime that changes the system refuses to start without a writable ledger; under `dry-run` it is in memory.

**Reconciling.** On start, and after any failed change, `Held` reads every entry of this node and of one action back against the workloads the runtime lists, and changes the ledger only once all are read: a failed read changes nothing. A pending entry is settled first: the setting from before its `Apply` means the change never took, and the entry before it is restored (or none); any other setting is adopted as `Applied`. The listing does not decide that a workload left; the actuator does. An entry whose workload is not listed is read through its subject alone, which the actuator finds it by (`memory.high` by the UID in `pod:<uid>`, trying every QoS class; `cpu.limit` by the subject), and is dropped only when the actuator answers `ErrGone`; one it still reads as applied is held. A listing that holds no workload at all while this node holds entries of the action fails the reconcile and changes nothing: an empty listing on a node that was changing workloads is a listing problem until proven otherwise. One whose setting no longer reads as `Applied` is dropped too: the pod restarted, or someone else changed it, and the setting is no longer the agent's to give back. The relief loop journals each drop as a `gone` outcome with the reason. The rest are held and taken up. A failed read fails the reconcile, which is retried every pass, and nothing is applied until it succeeds. An entry of another node or another action is left untouched, as is every entry of an action this agent has no actuator for (a lever removed from the config), so one lever's reconcile can neither stop nor drop another's.

**The descriptor drives the loop.** The relief loop knows no metric names: the node pressure is `Descriptor.Relieves`, a workload's share is `Usage@subject` over the node's `Usage`, and a share becomes the action's `Unit` through the `Capacity` fact. The descriptor is JSON (`GET /actuators`), shaped so it can later be offered to a decision model as a tool description.

**Storage and Updater used to be here**, and their removal is the largest structural change
the design has been through. Storage held a graph of construct edges; Updater incorporated
telemetry into them, with `RelationalUpdaterContract` as an alternative estimator. Together
they were a second model of the same relations the state model holds — learning from the
same samples into a different structure, kept in step by a propagation call.

Once cost, estimates, explanations and the graph surfaces were all read from the state
model, that second copy had exactly one remaining role: being displayed. An operator
opening the viewer read weights and confidences that entered no decision, on a page that
looked exactly like the one that used to. So the contracts went, their implementations
(`InMemoryStorage`, `EMAUpdater`, `RelationalEMAUpdater`) went, their compliance suites went,
and the graph surfaces now project the state model — see §5, "The graph surfaces are a
projection". The endpoint that ingested a construct pair and a magnitude directly went
with them: an observation is of a property, and a single number about a pair is an
assertion rather than a measurement.

### Behavioral guarantees

Guarantees are not just signatures — they are documented pre/post-conditions on each method in the contract source files. The compliance test suites in `go/compliance/` verify them mechanically. **A new implementation is valid if and only if it passes the compliance suite for its contract.** This is the definition, not a check.

Subjects and discovery added guarantees to two of the five `pkg/contracts` contracts:

- **Collector** — subjects are implied by observation: a collector MAY set `Subject` on a sample; a subject exists iff something observed it this tick. A collector never emits a "gone" event — silence is the signal, and the map interprets it.
- **Collector** — declared meaning: a collector MUST set `Unit` and `Range` on every sample of a type it emits, and keep them stable per type.
- **Proposer** — the pairing rule: `ObserveProperty` pairs any two properties inside a time tolerance — a scoped property (non-empty subject) with an unscoped one directed scoped → unscoped, two unscoped or two scoped properties as an undirected candidate — except that series of one chart and one subject that stay perfectly correlated are an arithmetic group, represented outside it by one member. A candidate with a non-zero `PBar` had a p-value at or under it. How many pairs are tested at once is the implementation's budget; `Discovery()` reports the budget, the pairs eligible, tested and displaced, so a short candidate list cannot be mistaken for nothing to find.
- **Proposer** — orientation: `Confirm(id, "")` accepts a directed candidate as proposed; an undirected one needs `from`, one of its ends. Anything else is `ErrBadConfirmation` and leaves the candidate pending.
- **Proposer** — `Forget` drops every buffer and candidate involving a property, so a departed subject does not accumulate.

Compliance suites exist for the five `pkg/contracts` contracts; Runtime and Actuator, the sixth and seventh, are tested through their registries, the `dry-run` runtime and the `Static` and `cpu.limit` actuators (`compliance/{collector,ontology,reasoner,proposer,tuner}.go`). Each runs against a factory the implementation supplies, so a new ontology or collector can be validated with a single test file wired to the suite.

### End-to-end validation: integration scenarios

Compliance proves each part works in isolation. **Scenarios prove the parts compose** into the behaviors the architecture promises. `internal/minimal/tests/scenarios_test.go` runs seven narrated end-to-end flows against the same wiring the production daemon uses; each emits `t.Logf` snapshots so `go test -v -run TestScenario` reads like a paper results section while hard assertions guard the mechanics that must not regress:

| Scenario                            | Demonstrates                                                                                          |
| ----------------------------------- | ----------------------------------------------------------------------------------------------------- |
| `ColdStart`                                    | every declared relationship present with confidence=0 and **no effective value at all** — `Effective()` reports not-known, so the agent says it does not know rather than reporting a constant |
| `ConvergenceOnOneEdge`                         | observations at a fixed value: the recent layer converges, the established layer follows more slowly, confidence climbs 0→1 |
| `PerKDAgentsAreIdenticalUntilTheyObserve`      | two agents built with different `profile.kd` produce **identical** cost answers until telemetry arrives — the inversion of the old expectation, and the point: nothing distinguishes them but what they measure |
| `DeprecationShrinksGraph`                      | after `Deprecate(...)`: graph path length drops by exactly 1; the model retains the retired relationship with its learned strength |
| `IdempotentReplay`                             | observations replayed with the same eventIDs are a no-op; new eventIDs accumulate — idempotency is per-event, for value, count and pair window alike |
| `AuditTrailRecordsEverything`                  | four operator mutations through the facade → each appears in the journal, in chronological order |
| `CoordinationOffload`                          | a peer's cost query, the trust-weighted ranking, and the offload accept/reject path end to end |

A separate verification (`pkg/profiles/build_priors_test.go::TestBuildSeedsStructureAndNoMagnitude`)
asserts the inverse of what its predecessor did: for every KD in `prior_weights.json`
and every proposition in the map, the seeded relationship has **no** effective value and
basis `unknown`. The test it replaced confirmed that each seeded prior matched the file
to 1e-6; the inversion is deliberate, and the failure it now guards against is a number
appearing where nothing has been measured. It still runs through `Build` with the same
Config literal the daemon constructs, which a library-level test of the seeder could not
do — and that distinction caught a real failure once, when the convergence harness
passed `map.proposer.enabled: false` and Go's flag package silently dropped every flag after it,
including `profile.priors` and `profile.kd`.

`TestBuildKeepsOntologyAndStorageInAgreement` pins a second invariant: the strength a
caller reads from `GET /propositions` is the one every answer is computed from. This
mattered acutely under the seeded design, where per-KD seeding wrote a calibrated weight
into the state model but left the declaration layer at its global value — k0s P2 was
exposed as the global 0.55 while the operative value was the calibrated 0.319, and the
first operator tune recorded its transition from a number the agent had never used. With
no magnitudes seeded the two layers cannot disagree at startup, and the test now guards
the same property across operator mutations instead, where an assertion must reach both.

#### Evolution scenarios

`internal/minimal/tests/evolution_test.go` runs six longer-form scenarios driven by the `ScriptedCollector` (§5) and, for scenario 6, the `MICorrelationProposer` (§6). Each prints checkpoint tables + an `EVOLUTION SUMMARY` block via `t.Logf` so the convergence story is reproducible from CI. Run with `go test -v -run TestEvolution ./internal/minimal/tests/...`:

| Scenario                              | Demonstrates                                                                                                |
| ------------------------------------- | ----------------------------------------------------------------------------------------------------------- |
| `ColdToWarmConvergence`               | a sustained pattern drives both endpoints; the recent layer converges and confidence climbs 0→1               |
| `RegimeChange`                        | a step pattern: the recent layer tracks each regime while the established layer lags, which is the divergence `novelty` reports |
| `ConflictPairCoupling`                | two relationships over one construct pair receive the same pairs and learn the same magnitude — Pearson correlation is symmetric, so direction comes from the declaration and not from the evidence |
| `MultiConstructStress`                | simultaneous patterns across constructs; relationships whose *both* endpoints vary converge, and one driven on a single endpoint never advances |
| `DeprecationFromContradiction`        | evidence departs from the established baseline; the advisor fires; the operator deprecates; the traversal path shrinks by 1 |
| `NewEdgeProposeConfirm`               | the MI proposer detects an undeclared correlation; the operator confirms; the backbone grows by one, at no strength, evidence `proposer-mi` |

Two notes on reading that table. `ConflictPairCoupling` demonstrates a *limit* rather
than a capability: the multigraph can host two claims over one pair, and the estimator
cannot tell them apart. And these scenarios run against the `ScriptedCollector`, so they
exercise construct pairs the committed specification no longer declares — that is
deliberate, because the compliance suites assert against whatever specification is
loaded rather than a fixed graph, and the scripted spec is how behaviour on a larger
graph stays tested after the committed one was narrowed.

### The ontology is alive

The ontology is not a static reference. Operators assert strengths when they know something the estimator does not, deprecate claims the deployment's evidence contradicts, and new domains may introduce new constructs. The contract therefore admits four kinds of mutation, each emitting one `OntologyEvent` to an append-only audit log:

| Mutation                          | Method                                       | Typical caller                          | Write to the state model |
| --------------------------------- | -------------------------------------------- | --------------------------------------- | ------------------------ |
| Claim magnitude asserted          | `SetPropositionStrength(propID, strength)`   | Operator tuning                         | writes `Assertion` on every relationship carrying that label, with actor and reason; both learned layers are left untouched |
| New claim added (validated)       | `AddValidatedProposition(p)`                 | `Proposer.Confirm` (post-review)        | declares the relationship with no strength and zero confidence; declares an endpoint property first if nothing routes to that construct |
| New construct added               | `AddConstruct(c)`                            | Operator (new domain extension)         | declares an observed property at zero confidence — nothing routes to it, so a derived one would summarise nothing |
| Existing claim retired (soft)     | `Deprecate(propID, reason)`                  | Operator (evidence-against accumulated) | retires the relationship: it leaves the traversal, keeps its record, and reads as deprecated on the graph surfaces |

**Every operator mutation must reach the state model.** This is an invariant, not an implementation detail, and it was the one most easily broken: every answer is computed from the model's relationships, never from a proposition list. A declaration-only mutation is invisible to every decision the agent makes — it appears in `Propositions()`, it appears in the audit trail, and it changes nothing. That failure mode is silent by construction, because a log records the operator's intent faithfully whether or not the model acted on it.

**It is now structural rather than a rule to remember.** The declaration layer no longer *has* a strength to set or a flag to raise, so there is no longer a way to perform half of one of these mutations: `SetPropositionStrength` and `Deprecate` exist only on the facade, and each returns `ErrNotModelled` — rendered as 404 — when no relationship carries the named proposition, rather than reporting success for an action that landed nowhere. The invariant survived the removal of the storage graph unchanged, then stopped needing to be an invariant. `pkg/semmap/ontology_sync_test.go` and `strength_propagation_test.go` pin each mutation's effect on the model.

The separation of `Assertion` from the two learned layers is what makes an operator
override safe *and* effective. An assertion is a field of its own: what was learned from
this system stays where it is and remains readable at `GET /edges`, while the assertion
outranks both layers in `Effective()` and therefore reaches the decision in full at any
confidence.

That precedence replaced a blend, and the reason is worth recording because the blend
looked reasonable. The effective strength used to be `(1 − c)·prior + c·learned`, with an
operator adjustment writing the prior — so a declared δ moved the answer by `(1 − c)·δ`,
and at `c = 1` by exactly nothing. Measured on a saturated k0s daemon, `prioritize
performance` moved both sensitivities by 0.000000 while recording itself faithfully in
the audit log. The inversion — *the better the agent knows its machine, the less an
operator can steer it* — was a defect and not a considered trade-off. Under the current
precedence the same operation moves the effective strength from an established 0.6023 to
an asserted 0.7223, and the sensitivity by the declared 0.1200.

Anchoring is the remaining subtlety. A delta applies to whatever the relationship
currently reports — its established baseline where it has one, its recent estimate
otherwise, and a neutral 0.5 on a relationship that has measured nothing, with the
anchoring recorded in the adjustment's rationale so a placeholder is never mistaken for a
measurement.

What is stable, what is not:

- **Construct removal** is impossible. Constructs are domain-stable per the architecture; once added they stay forever.
- **Proposition removal** is impossible. `Deprecate` is the only retirement path. Deprecated propositions remain in `Propositions()` so the audit trail / replay are intact, and the relationship stays in the model marked retired — soft-delete preserves both the structural and the evidence record, so a decision taken before the retirement remains reconstructible.
- **Proposition direction reversal** is impossible. `ValidateProposition` rejects a new edge that contradicts an existing direction. The three Di-Select conflict pairs (P2/P3, P5/P6, P7/P9) are exempt because both halves are present from the bootstrap; the Proposer cannot introduce *new* conflict pairs without explicit operator action (a future extension).

The audit trail (`GetHistory(since)` on the facade, `GET /history`) lets the agent answer "why is this strength what it is?" at any point in time. It is a projection of the state model's journal — there is one record, and it holds more than the ontology vocabulary can name, so a property admitted or a decision taken appears there too. It survives a restart unless `persist.file` is set to `""` (§5, Persistence), and is bounded: `GET /state/journal` reports how many entries were dropped, which is what a caller needs before reading an absence as evidence.

Implementations that intentionally do not support a mutation (e.g. a read-only ontology cache layered in front of the canonical store) return `contracts.ErrNotImplemented` rather than silently succeeding. The compliance suite tolerates this — every live-ontology subtest skips on `ErrNotImplemented`.

### Why the backbone is a multigraph

A construct pair may host more than one proposition. Such **conflict pairs** carry distinct mechanisms with opposite directions on the same `(from, to)`, which is why the edge key is the full triple:

| Pair        | Mechanism captured by each proposition                                                       |
| ----------- | -------------------------------------------------------------------------------------------- |
| **P2 / P3** on RC→PS | P2 (−): security/resource overhead reduces throughput. P3 (+): lightweight distributions reduce pod-startup latency. |
| **P5 / P6** on CO→RR | P5 (+): offline autonomy improves continuity during partition. P6 (−): cloud dependency reduces stability in poor networks. |
| **P7 / P9** on CE→MU | P7 (+): rich ecosystem lowers operator effort. P9 (−): excessive features increase maintenance complexity. |

These are not contradictions — they are **co-existing, evidence-distinguishable** mechanisms. In a real deployment, observed telemetry will support one mechanism more than the other, and each proposition's EMA drifts independently. The agent therefore needs to store both edges, update both from a single observation, and let the relative confidence-weighted magnitudes encode which mechanism dominates in this deployment.

Implications for the contracts:

- **The state model** keys relationships by `(from, to, label)`, where the label is the proposition ID. Two claims over the same pair with opposite signs are therefore two relationships, and evidence for one is evidence against the other. `Relationships(from, to)` returns every relationship between two properties.
- **Retirement cascades.** Retiring a property retires the relationships that reference it, because an edge to something absent cannot be evaluated. Both are soft, so an earlier decision stays reconstructible.
- **Reasoner** iterates `AllEdges()` directly and uses each edge's own `Direction`. There is no proposition-to-edge join, and so no risk of conflating P2 with P3.
- **Ontology** `ValidateProposition` rejects a new proposition that contradicts an existing one. The three bootstrap conflict pairs are exempt because both are present from the start with domain validation. New auto-proposed conflicts from the Proposer go through the normal rejection path — backbone extension does not introduce conflict pairs without explicit operator action.

---

## 3. Deployment Profiles

A profile is a named configuration that wires specific implementations to each contract. The agent binary is identical across profiles — only the profile name changes at startup.

| Profile         | Collector                   | State model            | Ontology                 | Reasoner         | Proposer          | Target                |
| --------------- | --------------------------- | ---------------------- | ------------------------ | ---------------- | ----------------- | --------------------- |
| `edge-minimal`  | cgroup (direct `/sys/fs`)   | in-memory + snapshots  | Static Di-Select         | Rule engine      | MI correlation    | RPi4, IoT nodes       |
| `edge-standard` | cgroup + kubelet `/metrics` | + variance per property | Static + extension table | Rule engine      | Threshold-based   | Standard edge servers |
| `cloud-full`    | Netdata HTTP API            | + durable store         | RDF/OWL + SPARQL         | SLM (Phi-3 Mini) | Correlation miner | Cloud VMs             |

The Collector column is what the profile is meant to run on; it is not wired by the profile. Collectors are configured in `sources.collectors` for every profile alike (§2).

**EMA** — Exponential Moving Average: `new = α × observation + (1-α) × old`. Controls how fast a property's value follows what it observes. `α = 0.2` is the default.

**Variance (μ, σ)** — tracking a property's spread alongside its mean, which is what
`simulate_outcome()` needs to return P95 risk estimates rather than a point estimate.
Planned from `edge-standard` upward; the model records neither today, and the field is
`nil` rather than a fabricated number.

The state model is the same code in every profile — one implementation, deliberately (§2).
What differs upward is what it can afford to keep: variance per property, and a durable
store behind the snapshot file.

**Why not Python on the edge?** Baseline interpreter footprint (~50–80 MB), unpredictable GC pauses under latency budgets, and the operational cost of managing a Python runtime on every constrained node.

---

## 4. Language Strategy

| Layer                                       | Language   | Why                                                                                                                           |
| ------------------------------------------- | ---------- | ----------------------------------------------------------------------------------------------------------------------------- |
| Contract interfaces + compliance suites     | **Go**     | The contract surface and the only definition of correctness. Co-located with the implementations they check                    |
| Structural initialization pipeline          | **Python** | One-time data wrangling from P1–P5 to decide which propositions the agent carries. A build-time step — nothing here runs on a node          |
| `cloud-full` profile service                | **Python** | scipy for a Bayesian estimator; correlation miner; SLM integration. Specified, not implemented (§3)                           |
| `edge-minimal` and `edge-standard` daemons  | **Go**     | Single ARM binary, <10 MB footprint, no runtime to manage on edge nodes, goroutines for concurrent telemetry, predictable GC  |

**This used to be a two-language contract boundary, and the claim it rested on was not
true.** The contracts were mirrored as Python ABCs with their own compliance suites, and
the stated argument was that the Python definitions were the specification, the Go
interfaces mirrored them exactly, and passing both suites proved behavioural equivalence
across languages. No Python implementation was ever built — `cloud-full`, the profile that
would have needed one, is still unimplemented — so the Python suites had nothing to run
against, which means the specification half of the argument was never checked by anything.
Two definitions with one implementation is not a specification and an implementation; it is
a definition and a copy, and the copy drifted: it still declared Storage and Updater after
both were deleted from Go (§2), and its Ontology carried a proposition strength and an
audit log the Go interface had stopped holding. A reader had no way to tell which was
current.

The mirror is deleted. The contract surface is `go/pkg/contracts`, the suites that check it
are `go/compliance`, and they sit next to the implementations they check. When a second
language implementation actually arrives, the interface it has to satisfy is the one with a
passing suite behind it — which is the useful direction for the boundary to run.

What remains of the Python layer is `prior_init/`: it reads the published constants from
P1–P5 and emits `prior_weights.json`, which the daemon seeds *structure* from — which
propositions it instantiates, which it declines and why, and where it overrides a
direction Di-Select states against a quantity this deployment does not measure. It no
longer emits a strength; §"What the artefact contains" records why. It is verifiable in
the way the removed Go mirror was not — the committed artefact reproduces byte for byte
from a fresh run (README §5).

---

## 5. Telemetry Pipeline

Live observations flow into the Semantic Map through a three-stage pipeline:

```
┌──────────────┐   MetricSample[]   ┌────────┐  update_edge()  ┌─────────┐
│  Collector   │ ─────────────────▶ │ state model: property, then what derives │
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
| `value`          | `float`      | The reading, in the declared `unit`; routed types are fractions on [0,1] |
| `timestamp_unix` | `int`        | Unix timestamp of the observation                        |
| `event_id`       | `str`        | Deterministic ID — same observation always → same ID     |
| `subject`        | `str`        | Empty for node-level readings; set to `<kind>:<identity>` for a scoped subject |
| `unit`           | `str`        | What the value means (`fraction`, `items`, …); stamped on the property at admission |
| `range`          | `[lo, hi]`   | The value's bounds; the estimator normalises by it. Declared where the quantity has one (a share, a percentage), absent for a rate or a count; the same on every sample of a type. Without one the map assumes [0,1] and says so |
| `source`         | `str`        | Which instrument produced it (`cgroup:<node>`, `app:<name>`, …) |
| `chart`          | `str`        | The series the producer computes together from one reading (a Netdata chart, one `/proc/stat` line); optional. Discovery groups a chart's series that stay perfectly correlated (§6) |
| `labels`         | `dict`       | Source metadata (cgroup path, Netdata chart, …); opaque  |

**`event_id` determinism** is the collector's responsibility. A stable recipe: `sha256(source_id + node_id + subject + metric_type + str(timestamp_unix))[:16]`. This carries the map's idempotency guarantee end-to-end: replaying the same telemetry batch has no effect on the model, because the map recognises an observation it has already applied.

**`available_metrics()` may change.** It is at least every type the last `collect()` emitted. A fixed collector (cgroup) returns one set throughout; one driven by include patterns (Netdata) learns what exists only by asking its source, and that grows as disks, interfaces and workloads appear. It used to be static for the instance's lifetime, which a pattern-driven collector cannot promise.

### MetricType catalogue

Routing is declared in `domain_spec.json`, not in code, and every *routed* type is a
fraction on [0,1]: construct polarity and the divergence measure are defined on that
interval, so an out-of-range observation there is clipped and the affected edge stops
responding to evidence while aggregates keep tracing a smooth curve. A replay
harness that passed network throughput in raw bytes per second produced exactly
that failure, silently. The set of types is open by union (below): an unrouted type —
any name a producer sends, most often scoped to a subject — carries whatever `unit` and
`range` it declares, and the map normalises by the declared range wherever it needs a
fraction.

| `MetricType`            | Construct | Polarity | Note                                                       |
| ----------------------- | --------- | -------- | ---------------------------------------------------------- |
| `cpu_utilization`       | RC        | worse    |                                                            |
| `memory_utilization`    | RC        | worse    |                                                            |
| `cpu_throttle_ratio`    | RC        | worse    | cgroup `cpu.stat` throttled_periods / total_periods        |
| `block_io_util`         | RC        | worse    | block device utilization                                   |
| `energy_joules`         | RC        | worse    | from RAPL or the P4 energy model, normalized to a budget    |
| `network_rx_bps`        | CO        | better   | fraction of link capacity                                  |
| `network_tx_bps`        | CO        | better   | fraction of link capacity                                  |
| `network_loss_ratio`    | CO        | worse    | **inverted at ingestion** — opposed to CO                  |
| `network_latency_ms`    | CO        | worse    | **inverted at ingestion** — RTT to a peer, against a budget |
| `pod_startup_ms`        | PS        | worse    | creation timestamp → Running, normalized against a budget  |
| `scheduling_latency_ms` | PS        | worse    | Pending → Scheduled, likewise                              |
| `cpu_pressure_ratio`    | PS        | worse    | PSI `cpu.some` stall share of the window (cgroup `cpu.pressure`) |
| `io_pressure_ratio`     | —         | worse    | PSI `io.some` stall share of the window (cgroup `io.pressure`); route removed (§2) |
| `memory_pressure_ratio` | —         | worse    | PSI `memory.some` stall share of the window (cgroup `memory.pressure`); unrouted |
| `cpu_steal_ratio`       | —         | worse    | node only: share of CPU time the hypervisor spent elsewhere (`/proc/stat` steal); unrouted |
| `cpu_contention_ratio`  | —         | worse    | `cpu.some` stall less the cgroup's own `throttled_usec`, per subject; the node's is the worst waiting of a pod owed CPU it does not get — its weighted fair share of the pods' CPUs (`cpu.weight`, capped by its `cpu.max`), by water-filling (`owedContention`) — not a BestEffort pod's or a unit's, whose waiting is not the runtime's to relieve, and not a pod's past its share, which waits on its own threads; unrouted; relief's signal (§16) |
| `memory_contention_ratio` | —       | worse    | node only: the worst `memory_pressure_ratio` of a pod promised memory (not BestEffort, not a unit) that is not limiting itself (its `memory.events` `high` and `max` did not rise this window) and, when the runtime's requests are known, is within its memory request (`memory.current` ≤ request); emitted every window the node has a memory PSI reading, and 0 when no pod counts (none promised, all new this window, all excluded), so relief always sees a fresh reading; absent without node memory PSI; unrouted; the memory lever's trigger |
| `memory_events_high`    | —         | worse    | per subject: times over `memory.high` in the window (`memory.events` `high` delta), `count` on [0, 1e9]; none for the node; unrouted |
| `memory_events_max`     | —         | worse    | per subject: times at `memory.max` in the window (`memory.events` `max` delta), `count` on [0, 1e9]; unrouted |
| `memory_oom_kills`      | —         | worse    | per subject: processes OOM-killed in the window (`memory.events` `oom_kill` delta), `count` on [0, 1e9]; unrouted |

The catalogue names the types a collector or a test can reference; it is not the
limit of what the map accepts. The set of `MetricType`s the map actually admits is
open by union — it is whatever its producers declare, routed or not.

An unrouted `MetricType` is ignored rather than rejected: forward compatibility is
deliberate, so a collector upgraded ahead of the specification does not break
ingestion. Adding a construct and routing metrics to it is a specification change;
see §2 on why constructs that cannot change while the cluster runs are not routed
at all.

**Polarity, and why a route needs one.** Each construct declares the direction its
value runs — `RC` and `PS` are penalties (`higher_is_worse`), `CO` is a capability
(`higher_is_better`) — and each route declares the direction of the raw metric. When
they disagree the ingestion path reflects the reading within its declared range,
`v' = lo + hi − v`, once, in `Spec.NormalizeForConstruct`, before anything stores or
learns from it.

This is not tidiness. Two failures were live without it, both silent:

- **A construct fed by opposed metrics averaged quantities that cancel.** `CO`
  summarised throughput *and* loss and latency from the same raw scale, so a link
  getting faster and a link getting lossier pushed its value the same way.
- **A proposition's declared sign was unfalsifiable.** A relationship inherited from a
  framework that phrased its outcome as a goodness quantity ("resource overhead reduces
  *throughput*"), attached to a construct measured as a badness quantity (latency,
  pressure), asserts a correlation the system never shows. Its evidence gate never
  opens, so it holds zero strength — indistinguishable, from the strength alone, from a
  relationship on a quiet system. `Relationship.SignConflicts` and `SignSuspect()` are
  what make the difference visible, and `relationships_sign_suspect` carries it into
  the census so an aggregate reader cannot miss it. Polarity is what gives that check
  a fixed reference to test against.

**How many observations a relationship gets.** A relationship advances on a *pair*, so
its `n_obs` counts the times both its endpoints were observed close enough together —
not the sample count, and not the count reaching either endpoint alone. A stream that
drives one endpoint and not the other produces a relationship that never advances,
which is the honest report rather than a defect. `N_converge` should therefore be set
against expected *paired* coverage, which is bounded by the slower of the two
collectors.

### The state model: properties, relationships, trace

`pkg/statemap` is the map as a model of the system rather than of a framework. It
exists because an agent that decides anything about its own system needs a model that
is current, answerable and attributable, and none of those follow from a schema fixed
at build time.

**Vertices are properties.** An OBSERVED property is fed by telemetry and holds what
the system is doing. A DERIVED property aggregates members and is recomputed rather
than stored, so a summary can never disagree with what it summarises. A framework's
evaluation constructs live here as derived properties over the metrics that evidence
them, which keeps prior knowledge without the graph being about the framework.

**Edges are relationships**, each carrying its provenance: `seeded` when only its structure came from prior
knowledge, `learned` from observing this system, `asserted` by an operator, `intervened` when an action
the agent took and verified created or confirmed it. `intervened` ranks above `discovered`: a
discovered edge rests on correlation, which a shared cause can produce, while the agent moved
the cause end itself and saw the effect end respond. Only the map's intervention record can
write it, and the state census counts it as `intervened` beside the other provenances. An agent that cannot say why it believes an edge cannot be audited, so provenance is data.

**Facts are the frame.** Beside its properties the map holds what the machine *is* — OS,
kernel, CPU model and cores, memory, disk, boot time — read once at startup by a
`facts.Source` (`host` reads `/proc`, `/sys` and `/etc/os-release`; another is a
registration away). A fact is read, not learned: it has no confidence and no history,
only its source and when it was read. It exists because a value means nothing without
its frame — 0.3 of a core is most of a Raspberry Pi and a rounding error on a server —
and an agent reasoning about one machine should not have to be told which machine it is.
Every view carries the facts whatever its filter, and a decision that reads them keeps
its own copy (`facts_read`). Reading the same value again changes nothing; a different
value (memory hot-added, a disk grown) is journalled as `fact.changed`. They are not
snapshotted: a restart reads the machine afresh, since a snapshot's facts would describe
the machine as it was when it was written.

**Lifecycle is the substance.** An observation of an unknown property admits it and
journals the admission, so the map follows a system that changes rather than a schema
someone wrote down. Silence marks a property stale and can retire it, because a model
that keeps reporting a departed metric's last value asserts what it cannot support.
Retiring a property cascades to the relationships referencing it, since an edge to
something absent cannot be evaluated. Retirement is soft: an earlier decision has to
stay reconstructible.

The ordering in `IngestSample` matters: the state model records a sample BEFORE the
routing table is consulted. The table says what the agent knows how to summarise, not
what the system is allowed to exhibit.

**Queryable.** `State(Query)` answers "what is this system doing" with no arguments
and narrows by kind, status, confidence or one-hop neighbourhood. A view never
contains a dangling edge, and every response carries a census of the whole map so a
filtered view cannot be mistaken for the whole. `Explain(id)` renders one property's
neighbourhood as text, because the first question after a surprising decision is
"what did it think was going on" and that should not require a client.

**Traceable.** Every mutation advances a revision. A `DecisionBuilder` records the
state a decision reads *as it reads it*, so the record cannot drift from the reading,
and inputs are copied so a decision cannot silently become a description of a later
system. Caveats name stale inputs, unobserved values and absent properties. The
journal is bounded and reports what it dropped, so an absence is never mistaken for
evidence that nothing happened, and a decision evicted by that bound answers 410
rather than 404.

**The Reasoner answers from it, and only from it.** A reasoner without a state model
returns `ErrNoStateModel`; the construct-graph cost path was deleted rather than kept
as a fallback, because a fallback meant an untraceable answer could be produced by
forgetting one wiring call, silently, since an empty `DecisionID` is easy to miss.

**The operator surface reaches it too.** `Tune` and `SetPropositionStrength` assert the
strength on the state model's matching relationships, recording actor and reason;
`Deprecate` retires them so a withdrawn claim leaves the traversal path. An operator
action that stopped at the construct graph would change what `Propositions()` reports
and no decision — which is what it did until this was wired.

**Level and sensitivity are different quantities.** A level is the observed value of a
cost construct. A sensitivity is per unit change in a source construct — the signed sum
of effective strengths, NOT multiplied by the source's current value. Multiplying would
duplicate what the level already reports and would collapse the term to zero before any
telemetry arrived. Note what this no longer buys: with no seeded magnitudes the sum is
*empty* on a fresh agent rather than approximate, so a cold-start agent cannot answer a
counterfactual at all — which is an honest report, and the gap §"What the artefact
contains" accepts in exchange for never reporting an unmeasured number.
`/state/estimate` reports both, and calls the value-weighted one a contribution.

**Details.** `CostOfAction` reads the cost constructs' levels and
their incoming relationships through a DecisionBuilder, so the properties that reach
the arithmetic are exactly the ones the journal holds — a separate "log what we used"
pass would be free to disagree with what was used, and that disagreement is invisible
in precisely the cases where it matters. Every cost answer therefore carries a
`DecisionID` and its caveats, and `GET /state/decisions/{id}` reproduces the inputs
afterwards. A reasoner constructed without a state model has no cost path at all and
returns `ErrNoStateModel`, so an untraceable answer is not a thing the agent can
produce.

`GET /state/estimate?target=` does the same for any property, not only the cost
constructs.

**The map survives the process.** `persist.file` persists properties, relationships and
the journal, and is on by default (`/var/lib/di-agent/state.json`): an agent that keeps
nothing is the exception an operator asks for with `""`, not the default they forget to
change. The write is to a temporary file, flushed to disk, renamed, and the directory
flushed after it (a filesystem that cannot flush a directory is let be), because a half-written snapshot read back on the next start would look
like knowledge, and a rename that reached the disk before its bytes leaves an empty one
after a power cut, which an edge node on an SD card will see. The agent checks at start
that it can write there and refuses to start if not, rather than failing every minute
in a log while the operator believes the map is kept. Saves take turns, from snapshot to
rename, so a periodic save still writing at shutdown cannot land an older state over
the shutdown save; and a shutdown save that fails exits non-zero. Two things follow from
persisting at all. A restarted agent is not back at cold start on a system it has
already watched for a week, and "why did you do that yesterday" stays answerable —
without this the audit trail is an artefact of one process lifetime rather than of the
agent. The shutdown save is synchronous in `main` rather than in the save goroutine:
cancelling the context and returning let the process exit mid-write, so a clean restart
silently dropped everything since the last periodic save.

What is deliberately not persisted: the estimator's pair windows. Restoring them would
restore a claim of simultaneity between observations taken before a restart and after
it, across a gap of unknown length. The learned strengths and their confidences survive,
so the estimator resumes from what it concluded rather than from what it was mid-way
through concluding. The snapshot carries a format version and an owner, and a mismatch
in either is refused rather than guessed at — a version 1 file half-loads under version
2's field names, and a snapshot copied from another host would install that machine's
observations as this one's history at full confidence. A relationship serialises its
recent layer as `recent` and adds the derived `effective` and `basis`, so a reader is
handed the answer rather than three layers and a precedence rule.

#### The three paces

The map changes at three rates, and each is a setting rather than an accident.

| Pace | Appears | Disappears |
|---|---|---|
| **Property** | one observation, at confidence 0 | stale after `map.stale_after` (2 m) → retired after `map.retire_after` (10 m), cascading to incident relationships |
| **Magnitude** | `profile.alpha` (0.2) for the recent layer, `profile.alpha_slow` (0.001) for the established layer | — |
| **Structure** | `map.proposer.min_pairs` (30) co-observations inside `map.pair_window` before a candidate can exist; an operator confirms | cascade on endpoint retirement |

Admission is immediate and *trust* grows with observations: a property's confidence is its observation count against the convergence threshold, so confidence is the pace signal and there is no probation gate. The sweep never retires a derived property — it is declared structure — but it goes stale when no member is active and returns with them; an operator can retire it.

### Peer state: knowledge crossing a node boundary

A node-local map means every question wider than one machine is a question for another
agent. Until `pkg/statemap.PeerStore` and `peers.Client.State`, the only things that
crossed a node boundary were a cost number, a health probe and an offload request: an
agent could not ask a peer what properties it has, which of them have gone quiet, or
what it believes about a relation. "Cluster-level questions go to peers" meant "ask a
peer for one number".

Each agent now fetches its peers' maps on a timer and holds them under the owner each
peer *reported* — not the URL it was reached at, so a peer reachable at two addresses is
one peer and a proxy in front of several is not mistaken for one. `GET /state/cluster`
returns this node's state and every peer's, side by side; `GET /state/where?property=`
answers "who has this, and what do they say it is".

**Nothing is merged, and that is the whole design.** A `PeerStore` is a separate type
from `Map` precisely so a question about "this system" cannot be answered with a peer's
property by accident. Merging would produce a map whose properties belong to no definite
system, and confidence — the claim that a value rests on observation — would stop having
a subject. For the same reason `/state/where` returns per-node answers and does not
average them: a mean across machines describes none of them, and the reason to ask
several nodes is to be able to pick one.

Three refusals and three distinctions hold the boundary. State naming no owner is
refused at the client, because unattributed properties are the one kind that could
quietly be read as this machine's. State claiming *this* agent's identity is refused at
the store, since a proxy loop would otherwise make one machine appear twice in every
cluster view. A peer that answered before and cannot be reached now keeps its last
snapshot and is listed as unreachable — during a partition that snapshot is all this
agent has. An address that has never identified itself is reported as silent and does
not become a node, because inventing one from a URL would put a machine that may not
exist into the cluster view. And a snapshot older than `peers.state_stale` is labelled
history rather than withheld, so the caller decides whether to act on it.

### How a relationship's strength is learned

A relationship asserts that one property influences another with some strength. What a
telemetry sample says about that strength has one defensible reading, and the map used to
implement two.

**What it does.** A relationship advances only when both endpoints have been observed
within the pairing window (default 15 s). The strength learned is `|r|`, the magnitude of
the Pearson correlation over a sliding window of pairs, on `[0, 1]` — which is the scale a
proposition strength was always meant to be on. It is folded twice from the same pairs: a
recent estimate at `α = 0.20` (memory ≈ 5 pairs) and an established baseline at `α_slow =
0.001` (memory ≈ 1000 pairs), the latter bias-corrected by `1 − (1 − α_slow)ⁿ` so it does
not spend its first thousand pairs reporting the zero it started from. The pairing is what
makes the two comparable; nothing interpolates between them, because `Effective()` chooses
by authority rather than by arithmetic.

Two properties follow:

- **Conflict pairs separate.** P2 and P3 share `RC→PS` with opposite directions. Evidence
  whose sign matches one sibling's declared direction drives the other toward zero. A live
  20-pair stream of correlated `cpu_utilization` and `pod_startup_ms` leaves the positive
  sibling at 0.995 and the negative one at 0.000.
- **The learned sign can contradict the backbone.** A proposition asserts a direction;
  when the system shows the opposite sign, that is evidence against *that* proposition
  rather than evidence of a weaker relation.

**What it replaced.** The other reading was endpoint EMA: every sample updated every edge
incident to its construct, and the edge's weight tracked that construct's magnitude. It was
cheap and every sample carried information, so confidence grew with the raw sample rate —
but the quantity was a proxy. The utilization of RC is not a measurement of how strongly RC
influences PS, and under that reading a conflict pair received the identical observation and
moved identically, so the "evidence-distinguishable mechanisms" claim of §2 was not testable
at all. It also meant a strength could reach full confidence on a signal that never varied.
Keeping both estimators meant keeping two models; the endpoint reading is the one that went.

**Where pairing happens.** Inside the state model, which is the only place that has the
latest value per property. Two details are load-bearing. Collectors sample on independent
grids — in the dissertation testbed `system.*` lands on 0, 5, 10 … and PSI on 2, 6, 12 …, so
the two never share a timestamp — hence a tolerance window rather than an exact match. And
because the map is node-local, every pair comes from one machine by construction; the
earlier arrangement had to key its pair tracker on node as well as construct, because one
graph ingested a whole cluster and would otherwise pair the master's CPU reading with a
worker's pressure reading.

**Idempotency.** An observation carries an event identity, and the map recognises one it
has already applied: a replayed archive or a retried post is a no-op rather than a second
vote. This holds for properties as well as relationships — the count behind a value is a
claim about how much observation stands behind it, and double-counting inflates confidence
the map has not earned. Recognition is bounded (the most recent few thousand events), so it
covers retries and batch replays rather than all history.

**What it does not claim.** Correlation is not causation. A relationship's existence and
direction come from the grounded-theory backbone; what is learned is the magnitude of the
association this system exhibits and whether the declared sign is the sign it shows. `|r|`
carries no significance test, so a weak relation estimated over a short window can still
produce a sizeable magnitude; `confidence` reports how many pairs stand behind it, and is
the field a caller should read before trusting a strength.

### The graph surfaces are a projection

`/graph`, `/edges`, `/neighbors`, the web viewer and `mapctl edges` render the state
model's relationships as `EdgeDescriptor`s. The wire shape is unchanged from when they read
a storage graph, deliberately — the clients kept working — but the numbers are now the ones
that decide things.

The mapping: endpoints from the relationship's endpoints, `proposition_id` from its label,
direction from its sign, `established` and `assertion` from those two fields (absent when
there is none, because zero is the claim that a relationship is worth nothing and absence
is not that claim), `effective` and `basis` from `Effective()`, `ema_weight` from the
recent layer, `confidence` from how much rests on paired observation here, `deprecated`
from retirement. The descriptor carried a `prior_weight` until the magnitudes were removed;
nothing replaced it with a default, and the `mapctl` column that rendered it was retired
with it rather than left showing zeros. A relationship between metric-level properties appears too; the store only
ever held construct-level edges, and hiding the extras would make the surface a filtered
view that claims to be whole.

### What a cost estimate is

`CostOfAction` reports two quantities per cost construct, and keeping them apart is
the substance of the design rather than presentation:

| | source | available at cold start | moved by |
| --- | --- | --- | --- |
| **level** | the construct's own derived property, recomputed from its members | from the first sample, at low confidence | telemetry only |
| **sensitivity** | signed sum of effective strengths over the relationships terminating at it, per unit | no — the sum is empty until something is measured or asserted | telemetry, operator assertion, deprecation |

A level answers *what is it now*; a sensitivity answers *what would it become if load
changed*. The level is what `CostOfAction` reports as the estimate, and the ordering is
empirical rather than aesthetic: on 182 replayed runs the observed level ranked the next
interval's pressure at 0.622 top-1 accuracy against 0.582 for the graph-adjusted form,
with no mixing coefficient improving on the level alone. The sensitivity is reported
beside it because it answers the question the level cannot — and there the ordering
reverses: on 5928 held-out bins of a calm→loaded transition, where no observed level for
the load exists, the learned strengths cut prediction error 72% against "pressure stays
put" and held 0.909 admission accuracy at a budget where a fixed table fell below the
majority-class floor. Which of the two an agent is doing decides whether learning the
strengths can matter at all.

They are reported side by side and never summed. That is an empirical constraint,
not a preference: over 182 replayed runs the observed level was the best available
predictor of the next interval's ranking, adding the relation term degraded it
monotonically, and sweeping the mixing coefficient found no interior maximum. So
`CostOfAction` reports the level and `SimulateOutcome` — a counterfactual, which a
level cannot answer — is where a sensitivity is multiplied by an assumed demand.

An earlier version had no level at all. It summed edge weights and called the result
a latency, which carried no observed magnitude and so could not distinguish a busy
machine from an idle one; `/cost` returned the same numbers whatever machine it was
asked about. The estimate is now a physical statement: on the k0s idle testbed it
reports `RC_level = 0.0775` at c = 0.98, i.e. this machine is about 8% utilized and
the agent has seen enough to say so.

Which construct plays which role comes from `domain_spec.json`'s `cost_model` block.
The cost function was the last place in the daemon that knew a construct by name.

**Counterfactuals.** `assume=<property>=<value>` substitutes a source's value in the contribution sum; `without=<subject>` takes every property of that subject to its range floor. Contributions are normalised by each source's declared range (`(v − lo)/(hi − lo)`), so a kW reading and a ratio can share a sum, and the projection is `level + Δ·(hi_t − lo_t)`, clamped to the target's range. Strength is a correlation magnitude used as a unit sensitivity, not a fitted slope, and every counterfactual says so. The decision records the assumptions and exclusions beside the properties and relationships read, so the answer is re-derivable after the system has moved on.

### What ingestion does

`IngestSample` records the sample against the metric's own property, and everything else
follows from the model: derived properties recompute from their members, and the estimator
folds the observation into the relationships incident to whatever moved. Every observed
property, routed or not, is then fed to the Proposer through `ObserveProperty`, which
looks for relations that nothing declared, between a subject and the node and between
two node-level properties (§6).

The order matters and is the substantive part: the state model records the observation
**before** the routing table is consulted. The routing table says what this agent knows how
to summarise; it does not say what the system is allowed to exhibit. A metric nobody has
mapped is still something the system is doing, so it becomes a property — journalled as an
admission — where the construct path would have dropped it.

There used to be a Bridge here: a stateless function that fanned each sample out to every
construct edge touching it, calling `UpdateEdge` on each. It is gone with the storage graph
and for the reason given above — a single construct's magnitude is not an observation of any
association's strength.

### Planned collector implementations

| Plugin              | Source                           | Profile                 | Status  | Available metrics                                                    |
| ------------------- | -------------------------------- | ----------------------- | ------- | -------------------------------------------------------------------- |
| `CgroupCollector` — built-in `type: cgroup` | `/sys/fs/cgroup/`                | `edge-minimal`          | ✅ done — `internal/minimal/collector_cgroup.go`. Reads the root and walks pod cgroups (systemd and cgroupfs drivers) and allowlisted units as subjects; a pod's subject is `pod:<uid>`, stable and unique, and the pod is labelled `namespace` and `pod` from the kubelet's log directory (`<namespace>_<name>_<uid>` under the cgroup entry's `config.pod_logs`), which needs no API access — the console shows the name, the map keeps the UID, so a recreated pod of the same name is a new subject; declares CPU and memory as `fraction` `[0,1]` of the node, the unit the domain spec declares for those routed properties (`cpu_throttle_ratio` as `share-of-periods`); CPU, I/O and memory pressure from `cpu.pressure`, `io.pressure` and `memory.pressure`, per node and per subject; CPU steal from `/proc/stat`, node only; subject memory against `MemTotal`, none when that is unknown, the node's own from `/proc/meminfo` because a v2 root has no memory files; bounded by `config.max_subjects`; silence, not events, marks a departed subject; a pod with more than one container named in the kubelet's `/var/log/containers` links is broken into `container:<uid>.<name>` subjects labelled with the pod as `parent` (a single-container pod is its container, and gets no part that would only repeat it; the sandbox has no link and is no container); unit names outside the subject charset are sanitised with a short hash suffix (the `unit` label keeps the true name) and a malformed `config.units` pattern is refused at startup. | cpu\_utilization, memory\_utilization, cpu\_throttle\_ratio, cpu\_pressure\_ratio, io\_pressure\_ratio, memory\_pressure\_ratio, cpu\_steal\_ratio |
| `ApplicationPush`   | `POST /ingest-sample` from the workload itself via `pkg/ingest/client` | `edge-minimal` | ✅ done — `pkg/ingest/client/client.go`. The wire face; same subject as the cgroup walk when the pod pushes under `pod:<uid>`. `Push` refuses a malformed subject or metric type, an empty unit or an empty range before sending, and returns an `Ack` saying whether a construct summarises the reading (204) or it was recorded as a property nothing summarises (202, with the agent's note). | any `MetricType` the workload declares |
| `ScriptedCollector` | programmable patterns (in-process) | demo / scenarios / replay | ✅ done — `internal/scripted/collector.go`     | any MetricType the patterns declare (Constant / Ramp / Step / Sine / Burst / Noisy) |
| `SystemScript` — built-in `type: script` | scenario file (in-process)         | demo / scenarios / replay | ✅ done — `internal/scripted/system.go`. A scenario-driven synthetic system with known ground truth — subjects with schedules and a coupled node model; the runner in `internal/minimal/tests/scenario_files_test.go` scores lifecycle, discovery, counterfactual and replay assertions against it; scenario files live in `scenarios/`. | any MetricType the scenario declares (subjects and node metrics) |
| `ParquetReplay`     | Netdata parquet datasets (out-of-process HTTP) | dissertation reproducibility | ✅ done — `cmd/replay/`               | cpu\_utilization, memory\_utilization, network\_rx\_bps, network\_tx\_bps           |
| `KubeletCollector`  | kubelet `/metrics/resource`      | `edge-standard`         | planned | pod\_startup\_ms, scheduling\_latency\_ms                            |
| `NetdataCollector` — built-in `type: netdata` | Netdata HTTP API v1, `/api/v1/allmetrics` (one request a tick), `/api/v1/charts` for chart types (when a chart appears that it has not described, at most once a minute) | `edge-minimal` + `cloud-full` | ✅ done — `internal/minimal/collector_netdata.go`. Include patterns over chart contexts (`config.include`; a curated default of CPU, memory, I/O, network and disk — not pressure, which beside the cgroup collector is the same quantity read twice and discovery proposes the two readings as a near-perfect pair; `*` for every chart). Each dimension becomes `<context>.<dimension>`; a chart that is one instance of its context becomes a subject (`disk:sda`, `net:enp0s1`, `cpu:cpu0`, `app:<name>`; a kubelet container's cgroup `container:<id>` labelled with its pod UID); percentages become fractions on [0,1], KiB/s and kilobits/s bytes/s with no range, PSI stall time a fraction of the second; outgoing rates are magnitudes; CPU idle and the 60 s / 300 s averages are skipped. A sample's chart is the Netdata chart id only when Netdata calls the chart `stacked` — a whole split into parts (`system.cpu`, `system.ram`, `disk.space`) — and `<chart id>/<dimension>` otherwise: an area or line chart's dimensions share an axis, not a reading, and received and sent, or reads and writes, track each other when the traffic is request and response, which is a finding. A device chart Netdata counts into a node chart (by its `interface_type: real` or `device_type: physical` label, or any core) declares `part_of` the node type each dimension adds to. A chart not yet described counts as not stacked; a collapsed type carries its source dimension's chart, so `network_rx_bps` groups with `system.net.InOctets` and not with `OutOctets`. With `config.collapse: true` (the default) it also feeds the node's own types (`cpu_utilization` = busy without idle and iowait, memory, the three pressures, steal, network against 1 Gbps); listed after a cgroup entry it is configured `collapse: false`, because the two measure CPU differently and the cgroup collector owns those types. | any `<context>.<dimension>` Netdata offers under the include list, plus the node types when alone |
| `loadavg` — example `exec` plugin | `/proc/loadavg`, `/proc/pressure/{cpu,memory,io}` (out-of-process, Python) | any | ✅ done — `examples/collectors/loadavg/loadavg.py`, on the Python SDK `sdk/python/di_collector.py`. Load per CPU declared `load_per_cpu` with no range (load has no ceiling); PSI `some avg10` as `share` on `[0,1]`; a missing pressure file is no sample, not an error. Certified in CI by `mapctl collector check`. It is the reference for a plugin outside the Go tree, not a replacement for the cgroup collector's pressure, which covers exactly the collection window rather than PSI's fixed ten seconds. | load1, load5, load15, psi\_cpu\_some\_avg10, psi\_memory\_some\_avg10, psi\_io\_some\_avg10 |

The rows marked built-in are the `type`s `pkg/collectors` registers; any other source is an `exec` or `http` plugin speaking wire protocol v1, written with `plugin.Serve` (Go, stdlib and `pkg/types` only, so a plugin binary does not link the agent) or `di_collector.serve` (Python, stdlib only). Certification does not fork for them. `mapctl collector check` runs the same `RunCollectorChecks` assertion set a Go collector passes under `go test`, through the compliance `Recorder` instead of a `*testing.T`, over the plugin wrapped in the agent's real `ExecCollector` or `HTTPCollector`: the adapter's stamping and protocol enforcement are part of what is certified, so a plugin that passes behaves under the agent as it did under the check. The adapters decode each sample strictly and drop, before the batch leaves them, a malformed sample (an unknown field, no `value`), a duplicate `event_id` within one reply and an invalid `available` entry; they report those drops through the optional `contracts.DropReporter`, which the supervisor counts as violations and the suite's `NoMalformedSamples` and `NoDuplicateSamplesInBatch` fail on. The protocol checks `mapctl` adds — hello round trip, `seq` echo, reply within the default timeout, unit and range stable over several ticks, no foreign `node_id`, timestamps in Unix seconds near `now_unix` (`TimestampNearNow`), no restart between collects (`NoUnexpectedRestarts`), `available` covering every emitted type — together with the suite cover each fault and each drop the supervisor and the adapters apply at run time: a plugin that passes loses no sample to them, and is not restarted, under the agent.

The `CgroupCollector` reports memory from two different places, because a cgroup v2
root carries no memory files at all. The **root's** share comes from `/proc/meminfo`,
as `(MemTotal − MemAvailable) / MemTotal`; when meminfo is unreadable the node simply
has no memory property, which is the honest answer. A **subject's** share is its own
`memory.current` over `MemTotal`; when `MemTotal` is unknown the subject has no memory
property at all, because `memory.current` over the subject's own `memory.max` is a
share of its limit, a different quantity, and the estimator normalises by what a sample
declares. A root that *does* carry memory files — the daemon
itself running inside a container, whose cgroup is its root — keeps the subject rule
and is measured against `MemTotal`, so the ratio is still against the machine and not
the container's limit. Every CPU and memory share, node or subject, is declared as
`fraction` of the node — never a fraction of any per-process or per-container ceiling.
The label is the domain spec's: the node's readings land on routed properties the spec
declares as `fraction`, the map holds a declared unit fixed and refuses a reading that
contradicts it, and the Collector contract keeps a type's unit stable across subjects.
(An earlier `share-of-node-capacity` label named the same quantity and had every
node-level reading refused as a conflict.) The one reading
that is not a share of the node, `cpu_throttle_ratio` (throttled CFS periods over
elapsed periods), is declared as `share-of-periods` instead. `cpu_pressure_ratio` is
the share of the collection window in which at least one runnable task in the cgroup
waited for a CPU: the delta of PSI's cumulative `some` stall time over the elapsed
time, rather than the kernel's `avg10`, so it covers exactly the window whatever the
interval. It is declared as `fraction` on `[0,1]`, matching the spec's route into PS.
Before it was collected, no live producer observed PS at all, so on a real node both
propositions stayed unknown however long the agent ran; only replay supplied it. A
kernel without PSI (`psi=0`) yields no pressure sample rather than a zero.
`io_pressure_ratio` and `memory_pressure_ratio` are the same measurement over
`io.pressure` and `memory.pressure`, unrouted: they are properties the proposer can
pair, not members of a construct. `cpu_steal_ratio` is the node's share of CPU time the
hypervisor gave to someone else, the steal column of `/proc/stat` over the total of its
first eight columns, in the kernel's own ticks on both sides so no `USER_HZ` or CPU
count enters. It is zero on bare metal, and on a VM it separates "this machine is busy"
from "the host is busy" when pressure rises with nothing local to explain it.
`cpu_contention_ratio` exists because a cgroup's `cpu.some` stall counts the time its
own quota held it back, and the root's sums every cgroup's: on the k0s VM (kernel 6.8)
one pod alone at a 1.5-core limit put node pressure at 27% with nothing else waiting,
its own throttled share 29%, and lowering its limit to 1 core raised node pressure to
42%. Per subject, contention is the stall share left after the cgroup's own throttled
time, clamped at zero; a cgroup without `throttled_usec` has no quota, so all its stall
counts. It is an approximation that errs toward zero: `throttled_usec` sums the time each
of the cgroup's per-CPU runqueues spent throttled, while the PSI total is wall-clock, so a
cgroup throttled on several CPUs at once has more subtracted than its throttling explains
and reads low, never high. The node's is the highest of its subjects' this tick — the share of the window
in which some workload waited for a CPU another was using — over the subjects that were
promised CPU: a BestEffort pod has no request, so on a node that is merely busy it waits
nearly all the time, and letting it set the signal would have relief cap a workload that
was promised CPU to serve one promised none. Its own reading is still emitted. The node's
is absent when no such subject has a reading, rather than a zero that would read as
"nobody waiting". The two sources
account for page cache differently — meminfo's `MemAvailable` excludes reclaimable
cache, `memory.current` includes a cgroup's own — so the node figure and the sum of its
subjects' figures are not the same quantity and are not expected to add up.

Multiple collectors can run concurrently in the same agent (e.g., `edge-standard` runs both Cgroup and Kubelet). The map ingests all their outputs — event identities make overlapping reports of the same physical observation harmless.

#### Externally-driven path: parquet replay

`cmd/replay/` differs from the other rows above: it is a standalone HTTP
client, not a `CollectorContract` implementation living inside the daemon.
The split is deliberate — the replay tool reproduces the dissertation's
P1–P5 dataset (225 Netdata parquets) from outside the daemon by POSTing
`MetricSample`s to `/ingest-sample`, so externally-driven samples take
the same code path as in-process collectors. Two benefits fall out:

- Anyone with a Go toolchain and the dataset can reproduce the convergence
  story without linking against internal packages — `cmd/replay/` imports
  only `pkg/types` (via duplicated wire DTOs in `cmd/replay/client/`).
- The replay tool's `EventID` derivation (`sha256("replay:" + parquet +
  ":" + hostname + ":" + chart_context + ":" + metric_id + ":" +
  relative_time)[:16]`) carries the map's idempotency guarantee
  end-to-end: re-replaying the same parquet cannot inflate
  `n_observations`. The acceptance proof is the
  `n_observations`-before/after/after-again triple in the README.

The `(chart_context, metric_id, units)` → `MetricType` mapping table lives
in `cmd/replay/mapping/mapping.go`. Extending it is a one-package change
with no impact on the daemon or its profiles.

#### `replay compare` — debug/inspection side-tool (`cmd/replay/compare/`)

**Auxiliary, not a research artifact.** `replay compare` is for spotting
mapping bugs, sanity-checking that routing produces a consistent shape
of evidence across different real-shaped recorded inputs, and inspecting
which edges respond to which telemetry — not for drawing production
conclusions about "which KD is better." The parquets it consumes are
synthetic benchmark loads from the P1/P2 study (controlled exercise
runs), so cross-KD divergence in its output reflects *the recorded test
harness inputs*, not natural deployment behavior.

Mechanically, compare builds N independent `SemanticMap`s — one per KD, each seeded
with the same structure from `prior_weights.json` and no magnitudes, so any divergence
between them is entirely what their telemetry produced —
feeds each only its own KD's parquet rows, snapshots every map's final
edge set, and emits a per-edge × per-KD inspection table (plus JSON/CSV
for downstream tooling). The `effective` column is what the Reasoner
would consume — `assertion`, else `established`, else `recent`, and blank
where a relationship has none — and `Range = max − min` flags inputs that
ingestion propagated differently per KD.

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

Ingestion is `SemanticMap.IngestSample`, which records the sample in the state model and feeds the property to the Proposer. The autonomous scheduler that ticks the configured collector lives in `go/pkg/agentmain/main.go::runCollectionLoop`; it is started by `startCollectionLoop` once the daemon has built its profile. The loop ticks the Supervisor that `pkg/collectors` built from `sources.collectors`. Both pieces are profile-agnostic — adding a new collector means registering a type or listing an `exec`/`http` plugin, no changes to the loop, to ingestion or to any profile.

---

## 6. Automatic Graph Extension

The Proposer contract supports discovering relationships the loaded specification does not declare. The flow is **propose-then-confirm** — patterns are detected automatically, but a human confirms before the backbone is modified.

```
Telemetry accumulates in the evidence layer
        ↓
Proposer pairs any two properties that co-occur — subject → node, node ↔ node,
subject ↔ subject — one pair per reading, as many pairs at once as the pair budget
allows (the pairing rule below)
        ↓
If |r| ≥ map.proposer.threshold AND n ≥ map.proposer.min_pairs AND each end holds
map.proposer.min_pairs distinct readings AND each end moved by
map.proposer.min_spread of its declared range AND the map does not already hold the
pair (either way round, for an undirected one):
    → Emit CandidateEdge (visible via GET /candidates)
        ↓
Operator reviews: confirm / reject / defer
        ↓
Confirm → a Discovered relationship in the state map (a pair of properties),
          declared by the facade; an undirected candidate runs from the end the
          operator names. A sign that contradicts an existing edge is refused and
          the candidate reopened. A construct pair goes through
          OntologyContract.AddValidatedProposition() instead.
Reject  → an operator's is suppressed for this deployment session; an agent's
          returns to pending once the evidence moves past what it was decided on
Defer   → out of the pending list; an operator's deferral stands, an agent's
          returns like its rejection, the cap's re-enters when it outranks a
          pending candidate
```

Every review is recorded on the candidate as its `Review`: who decided (operator, rules,
or which model tier), the action, whether it was applied or only suggested, the decision
record holding its inputs, and the candidate's evidence at the time. The evidence is what
lets an agent's decision be provisional without being forgotten: the pair is still tested,
and the decision stands until the sign or kind changes or `|r|` moves by more than two
standard errors of Fisher's z at the smaller window — the band in which a second look
would see the same candidate. An operator's rejection or deferral is final for the session,
because a person who has looked at the pair is the last tier there is.

The Proposer **never modifies the map or the backbone directly**. `Confirm` returns the proposition and the facade applies it: a pair of state-map properties becomes a `Discovered` relationship in the state map, a construct pair goes through `OntologyContract.AddValidatedProposition`, which validates it against existing propositions. A declaration that fails — a positive direction where a negative already stands — reopens the candidate rather than leaving history saying Confirmed about a relationship that does not exist.

The `edge-minimal` profile ships with `MICorrelationProposer` enabled by default (`map.proposer.enabled: true` in `agent.yaml`). It keeps one pair window per `(from, to)`, fed by `IngestSample` via `ObserveProperty` with one pair per reading — so `n` counts co-observations, not arrivals — and emits `CandidateEdge`s when `|Pearson r|` reaches the threshold (default 0.85) over at least `map.proposer.min_pairs` pairs. P-values are computed via the Fisher z-transform (`z = atanh(r) × √(n−3)`, two-tailed using `math.Erfc`). The proposition a confirmation returns carries evidence source tag `proposer-mi`. The coverage check against the state map is direction-aware, so conflict-pair siblings (opposite direction on the same `(from, to)`) remain reachable there; the candidate cache itself is keyed on `(from, to)`, and a settled candidate — confirmed, or rejected or deferred by an operator — is not re-emitted under either sign within the session. Pearson stands in for true mutual information here — a richer estimator can drop in at `edge-standard`/`cloud-full` without touching the interface. Pass `map.proposer.enabled: false` on resource-constrained nodes where the ring-buffer overhead is undesirable.

### The pairing rule and Discovered provenance

The proposer is fed every observed property, and pairs them inside the pair window:

| Pair | Candidate | Why |
| --- | --- | --- |
| subject → node | directed, subject → node | a subject is a component of the node, so the stated assumption is component influences whole |
| node ↔ node | **undirected**, the pair in lexicographic order, id `a<->b` | correlation is symmetric and nothing says which node-level quantity drives the other; the operator names the end it runs from on confirm (`{"from": ...}`), and a relationship declared either way round covers it |
| subject ↔ subject | **undirected**, as node ↔ node | two workloads can drive each other or share a driver. Two subjects' readings of one metric (`cpu_utilization@pod:a <-> cpu_utilization@pod:b`) carry `Kind: "same-metric"`: they are proposed like any other pair, and the kind says a shared driver (the same cron, the same client) is the likelier explanation. Two metrics of *one* subject (`cpu_utilization@pod:a <-> cpu_throttle_ratio@pod:a`) carry `Kind: "same-subject"`: a workload's own quantities often hold together by construction — a capped pod that is busy is throttled and under pressure — and on a k0s VM with seven workloads they were 10 of 25 pending candidates |

Readings further apart than the pair window do not form a pair, and a reading no newer than its property's last one is a repeat and is not paired; a pending candidate carries its pair's latest numbers, and one whose `|r|` falls below the threshold by more than one standard error of Fisher's z, `1/√(n−3)`, is deferred with `Reason: "weakened"` (it re-enters at the threshold itself, if the pair holds again) — without that, a candidate emitted on one coincident spike stood in the queue at the `|r|` it had then, and without the margin a pair whose `|r|` sat on the threshold flapped between pending and weakened every minute or two, each flip a change for an operator to read; coverage is checked against the state map (a retired relationship does not count); pending candidates are capped at 64 with the weakest deferred; a retired property is forgotten.

### The pair budget

The pair count grows with the square of the properties: about 20 node ↔ node pairs for the cgroup collector's node metrics, about 2 million for a source with 2 000. Recording has no limit; testing pairs does. One budget, `map.proposer.max_pairs`, is shared by all three classes.

- **Default from the machine.** `0` (the default) derives it at start from 1% of the agent's memory (`MemTotal`, or its own cgroup's `memory.max` when lower) at ~2.6 KB per tested pair, and 1% of each collection tick across its CPUs (`cpu.max` when lower) at 5 µs per pair per tick — ten times what an Apple M-series core measured, so a Raspberry Pi-class core is not overcommitted — whichever allows fewer. A 2 GiB VM gets about 8 000 pairs; an 8 GiB, 4-core Raspberry Pi about 32 000. Fixed at start rather than adapting while the agent runs, so a run's budget is one number in its log and in `/state`. When the memory cannot be read (not Linux), 4 096. An explicit value always wins.
- **While there is room** every pair a reading can form is tested, so a node within its budget behaves as if there were none.
- **When it is full**, a rationing pass runs at most once per pair window of reading time:
  - pairs whose candidate was confirmed, rejected or deferred by an operator, or that became a relationship, stop being tested;
  - only properties observed within the pair window that moved by at least `map.proposer.min_spread` of their declared range (of their largest magnitude when they declare none) compete;
  - the most variable properties get pairs first, whatever the class — a pair ranks by its less variable end, because the variation gate needs both;
  - an arithmetic group competes through its representative only;
  - a new pair takes a free slot or displaces a tested pair that ranks below it: first one with an end gone quiet (not observed within the pair window), then one that has held 30 pairs and stays below the candidate threshold, weakest `|r|·n` first. A live pair at or above the threshold is never displaced, and a pair is never displaced by one that ranks no higher, so the budget settles instead of rotating;
  - a pending candidate whose pair is displaced is deferred with `Reason: "untested"`: nothing updates its numbers, so it should not ask for confirmation or hold a place under the pending cap. It re-enters if the pair is tested again and still clears the threshold.
- **Visible.** `GET /state` carries a `discovery` block — `budget`, `properties`, `eligible_pairs`, `tested_pairs`, `idle_pairs` (tested, with an end gone quiet), `tested_by_class`, `displaced`, the arithmetic `groups`, and the corrected bar (`fdr`, `p_bar`, `p_values_tested`) — and the console's Discovery pane says how many of the eligible pairs are being tested.

The per-pair cost is what made the budget affordable: each pair window used to keep a set of up to four windows' worth of pair identities against replays, ~23 KB of a pair's ~26 KB. The property path now refuses a reading no newer than its property's last instead, one timestamp per property, and its windows keep no identities.

Four gates shape what is proposed:

- **Variation.** Correlation is scale-free: a pod whose memory stepped between 572 KB and 1 MB correlated with node memory at r = 0.995 while moving 0.01% of the node. A candidate is emitted only when each end moved, over the window, by at least `map.proposer.min_spread` (default 0.01) of its declared range. An end whose producer declared no range cannot be checked; the candidate is still emitted and its `Caveats` say so.
- **Evidence, not length.** A rate that sits at zero and spikes a few times correlates, over a window, as if it had only those readings: idle pods' interfaces, at zero but for occasional bursts at the same moments (plausibly the bridge's broadcasts; not verified), paired at r = 1.0, 21 candidates in one 25-minute run. Each end must hold `map.proposer.min_pairs` readings' worth of evidence — its readings off its commonest value, plus that value once, which is the window's length when no value repeats — or its r is undefined, as a constant series' is: no candidate, no group link, a pending candidate deferred as weakened. Past that, spikes count like any other movement, so a bursty workload active a quarter of the time is still tested. Unlike the spread gate this needs no declared range, so it covers the rates the spread gate cannot check.
- **Composition.** A subject's metric against the node total of the same metric (`cpu_utilization@pod:a → cpu_utilization`) is part against whole, so its correlation is accounting rather than something learned. It is still proposed, because how much of the node one workload accounts for is what `without=` answers from, and carries `Kind: "composition"` so it is not read as a discovery. A part need not share the whole's name: a producer that knows a subject's quantity is summed into a node type declares it with `part_of`, and the Netdata collector does so for the devices Netdata counts into its node charts — real interfaces into `system.net`, physical disks into `system.io`, cores into `system.cpu`. On a node with one disk and one interface those pairs reached r = 1.000 and were offered as findings; they are the device being the node's total.
- **Arithmetic groups.** A producer that computes several series from one reading (a Netdata chart's dimensions, a counter and its 10 s and 60 s averages) names them with one `chart`. Series of one chart and one subject whose tested pair reaches |r| ≥ 0.99 are sums, complements or smoothings of each other: the pair links them into a group instead of becoming a candidate, and the members a chain of links connects are one group. Its representative — the most variable member when the group formed, kept while it stays a member so one link elsewhere does not move every group's pairs — is the only member paired outside the chart; the others' outside pairs stop being tested and their pending candidates are deferred with `Reason: "grouped"`. Pairs inside a chart keep being tested, so a group breaks when its members part ways and they are paired again. A link forms at 0.99 and holds until |r| falls more than one standard error of Fisher's z below it (0.988 at 120 pairs), the band a pending candidate gets under its threshold: a pair whose |r| hovers at the cut-off otherwise forms and breaks its group at every crossing, and each break sends the pair into the pending queue as an ordinary candidate (the group count went 0 → 5 → 3 → 1 → 3 in a 25-minute run with Netdata before the band). The band is a Schmitt trigger set by sampling error, the same device ODAC (Rodrigues, Gama and Pedroso, IEEE TKDE 2008) uses when it splits a cluster of correlated streams only once the difference in question exceeds its Hoeffding bound; a link displaced from the budget breaks it too, since an untested link can neither hold nor break a group. Without this, a wide include list fills the pending queue with arithmetic. Series with no chart are never grouped: two quantities that merely correlate perfectly are a finding, not an identity. The chart is therefore the producer's claim that its series are parts of one reading, not that they are drawn together: the Netdata collector gives a non-stacked chart's dimensions a chart each, because received and sent on one interface reached |r| ≥ 0.99 on a request/response workload and were grouped as if one were arithmetic of the other.
- **A bar corrected for the pairs tested.** With thousands of pairs, |r| ≥ 0.85 over few readings passes by chance often enough to matter. Each pair's Fisher p-value is kept at its last fold; a candidate must also have a p-value at or under the Benjamini–Hochberg bar at `map.proposer.fdr` (default 0.05) over the `m` pairs being tested — `k·q/m` for the largest `k` whose sorted p-value is under it, `q/m` when none is — and carries that bar as `PBar`. A pair without enough readings yet counts in `m` as a test that found nothing: without that, the first pairs to reach `map.proposer.min_pairs` were judged as if they were the only tests, and in a run of 30 noise properties about one noise pair per run got through. Arithmetic links are left out, since their p ≈ 0 would count as discoveries and loosen the bar for every real pair. A pending candidate the bar tightens past is deferred with `Reason: "bar"` and re-enters if it clears the bar again. The bar is recomputed once per round of folds, or sooner when `m` moves by a tenth: about one sort per tick. It controls the false-discovery rate under independence; readings that drift slowly together are autocorrelated, so the bar is a floor on the evidence asked for, not a guarantee.

Confirming a candidate between properties declares a state-map relationship with provenance **Discovered** and label `discovered`. It never becomes a proposition: Di-Select's vocabulary is constructs, and a pod↔pressure edge is a fact about this machine. Whether a confirmation goes to the state map or the ontology is decided by its ends, not their scope: two node-level properties are unscoped and still not constructs. Provenance answers why an edge exists (seeded, discovered, asserted); the two strength layers answer what it is worth. `ObserveConstruct` remains as the explicit construct-pairing path for callers that drive it directly; ingestion does not use it.

---

## 7. Adding a New Profile

1. Create `go/internal/<profile-name>/` and implement all five `pkg/contracts` contracts, or reuse existing implementations.
2. Every implementation must pass its contract's compliance suite before being wired into a profile.
3. Add a case to `go/pkg/profiles/profiles.go`:

```go
case "my-profile":
    // No collector here: collectors are configured in sources.collectors and built by
    // pkg/collectors. A new source is a registered type (collectors.Register in an
    // init) or an exec/http plugin, not a profile.
    ontology  := minimal.NewOntologyFromSpec(spec) // reuse if sufficient
    reasoner  := myprofile.NewMyReasoner(spec, ...)
    proposer  := myprofile.NewMyProposer(...)
    tuner     := myprofile.NewMyTuner(...)      // or minimal.NewDisabledTuner() to opt out
    // The state model is the agent's one model, and Build always has one: a caller
    // that passes none gets it built with the defaults, because an agent without a
    // state model can answer nothing. Seeding must run before the map is handed out
    // — it is what declares a property per routed metric and per construct, and a
    // relationship per proposition — structure only, with no magnitude on any of them.
    seedStateMap(cfg.StateMap, spec, pw, cfg.KD)
    sm := semmap.New(ontology, reasoner, proposer, tuner)
    sm.AttachState(cfg.StateMap)
    reasoner.AttachState(cfg.StateMap)
    return sm, nil
```

4. Update the profiles table in this file (§3) and the project structure in README.md.

No other file needs to change.

---

## 8. Connection to Research

| Publication                                 | Role in Semantic Map                                                          |
| ------------------------------------------- | ----------------------------------------------------------------------------- |
| P1 (Performance & Resource Efficiency)      | Evidence behind the propositions the specification declares; the workloads the convergence study replays |
| P2 (Security, Resilience & Maintainability) | Evidence for the constructs a running cluster *cannot* move, which is why they stay at selection time |
| P3 (Di-Select Framework)                    | The causal claims the specification declares: which construct pairs relate, and in which direction |
| P4 (Energy Analysis / DVFS)                 | The reason energy is *not* routed: the model is calibrated for one hardware class, and no node here measures a joule |
| P5 (Overhead Decomposition)                 | Per-container evidence that the orchestration tax is real and small; k0s-only, so not a cross-cluster input |
| **P6 (this work)**                          | The Semantic Map itself — schema, structural initialization, convergence study |

What the agent takes from P1–P5 is **structure, not magnitude**: which constructs relate
and in which direction. That is knowledge one machine's telemetry cannot produce — a node
watching itself can measure how strongly two of its own signals move together, but cannot
establish that the relation is worth positing, cannot rule out the pairs it might
otherwise correlate spuriously, and cannot establish a direction, because correlation is
symmetric and causation is not.
| P7 (Decentralized Framework)                | Extends the Semantic Map with P2P trust edges and gossip-based peer discovery |

**P6 scientific contributions:**
1. Contract-based architecture enabling RPi4-to-cloud profile switching without changing agent logic
2. Structural initialization protocol connecting Di-Select to agent runtime, with the propositions it declines and the directions it overrides recorded in the artefact
3. Convergence study: how quickly does a machine's own evidence accumulate, and what does the converged state describe?
4. Propose-then-confirm loop: controlled automatic backbone extension with structural validation

**Theoretical framing.** The architecture reported here can be read as a concrete instance of the *graph stage* in Andrew Ng's progression from single-loop to graph-based agentic workflows (Ng, *What's Next for AI Agentic Workflows*, Sequoia AI Ascent 2024; Schluntz & Zhang, *Building Effective Agents*, Anthropic 2024). Ng characterises the graph stage as one in which shared state is externalised into a durable, queryable structure that agents read from and write to via typed handoffs, rather than living in prompts and transcripts. A knowledge graph, in this framing, plays three complementary roles: shared memory for orchestrator–worker configurations, grounding layer for evaluator–optimiser loops, and persistent world model for reflective loops. The Semantic Map instantiates the latter two directly for the orchestration-selection domain.

Both authors identify a further requirement — *anchors* — without which "even a graph of loops can become a circular system of mutual confirmation" (Ng, ibid.). Our architecture supplies three explicit anchors:

| Anchor (Ng)          | Semantic Map implementation                                                                                             |
| -------------------- | ----------------------------------------------------------------------------------------------------------------------- |
| Real-world outcomes  | Netdata telemetry — only `MetricSample` observations update the EMA; no model-estimated evidence                        |
| Frozen rules         | The declared propositions as an append-only backbone (constructs never removed; direction reversal disallowed)          |
| Human judgment       | Operator tune, candidate confirm/reject, deprecate — every mutation stamped in the `OntologyEvent` audit log            |

This design also satisfies Ng's reliable-agentic-system invariant — *every important output can be traced to a task, a plan, an artifact, a source, an evaluator decision, and a bounded execution record* — as an architectural property: `CostRequest`, `GraphPathUsed`, `ActionCost`, `EventID` provenance, `Rationale`, and `n_observations` correspond one-to-one to the six required trace elements. The distinction from most LLM-agent frameworks in this space is that the backbone is not an ad-hoc ontology invented by prompting an LLM: it is Di-Select's grounded-theory result [P3]. Any downstream LLM consumer of this graph sits on the *operator-facing surface* — the reasoning and ingestion paths remain deterministic Go code, and reproducibility of P6 results does not depend on any LLM's behaviour.

---

## 9. Control Surface

**The agent is described by a spec, not by flags.** `pkg/spec` holds one document, `agent.yaml`: every setting the daemon takes, each defaulted to what the flag it replaced defaulted to. The file overlays the defaults, `${VAR}` is expanded from the environment so a secret stays out of it, and an unknown key is refused. The daemon's only flags are `-config` and `-print-config`; `GET /config` serves the running spec with secrets redacted. `main` builds every part from one `spec.Spec` value, so a later stage adds a block to the spec rather than a flag to the command line.

The Semantic Map facade is a Go API. The agent daemon wraps it in three layers so that operators, scripts, and demos can drive the same surface without sharing process memory:

```
┌─────────────────────────────────────────────────────────────┐
│  Layer C — Web console    pkg/agentmain/static/{index,style,js/} │
│  Vanilla ES modules, no CDN, embedded via go:embed all:static │
│  Live state-model console at /ui/                            │
├─────────────────────────────────────────────────────────────┤
│  Layer B — CLI            cmd/mapctl/                        │
│  cobra + tablewriter; one binary, sixteen subcommands        │
│  Default --addr http://localhost:8080; --json for scripting  │
├─────────────────────────────────────────────────────────────┤
│  Layer A — HTTP API       pkg/agentmain/{routes,dto,static}.go   │
│  net/http only; JSON in/out; CSRF via Content-Type guard     │
└─────────────────────────────────────────────────────────────┘
                            │
                            ▼
                  SemanticMap facade (pkg/semmap)
```

Every layer talks to the layer above only via HTTP — the CLI does not import `pkg/agentmain`, and the UI is served as static assets. This is deliberate: the daemon is the single integration point, and any third tool (e.g. a future TUI, a fleet controller) only needs to speak JSON to participate.

### HTTP API

Two endpoint families coexist on the same mux. The four surviving pre-Phase-1 endpoints (`/cost`, `/recommend`, `/simulate`, `/candidates`) keep their original `http.Error` plain-text error format to minimize diff against the v0 daemon. The fifth, `/ingest`, is gone: it named a construct pair and a magnitude directly, which is an assertion about a relation rather than an observation of one. Every endpoint added in the Phase 1 expansion emits structured JSON errors and gates mutations on `Content-Type: application/json`.

| Verb | Path                              | Request body / params                                                  | Response (2xx)                | Semantics                                                                                              |
| ---- | --------------------------------- | ---------------------------------------------------------------------- | ----------------------------- | ------------------------------------------------------------------------------------------------------ |
| GET  | `/healthz`                        | —                                                                      | `{"ok":true}`                 | Liveness probe; never touches the facade                                                                |
| GET  | `/config`                      | —                                                                      | the spec as JSON             | The spec the agent runs on (`agent.yaml` over the defaults); secrets shown as `[redacted]`, as are collector entries' non-empty `config`, `exec.args` and `http.url` userinfo and query (`spec.RedactURL`, also used for the startup log and http collector errors) |
| GET  | `/version`                        | —                                                                      | `VersionResponse`             | Agent version, Go runtime, build commit, construct/proposition counts                                  |
| GET  | `/graph`                          | —                                                                      | `GraphSnapshot`               | Full snapshot: every construct, every proposition (incl. deprecated), every edge                       |
| GET  | `/edges`                          | `?from=&to=` (either or both, optional)                                | `[]EdgeDTO`                   | Filtered edge list; when both `from` and `to` are set, returns the conflict-pair multigraph fan-out    |
| GET  | `/constructs`                     | —                                                                      | `[]ConstructDTO`              | Backbone nodes                                                                                         |
| GET  | `/propositions`                   | —                                                                      | `[]PropositionDTO`            | All propositions including those soft-deleted by `Deprecate` (the DTO carries a `deprecated` flag)     |
| GET  | `/history`                        | `?since=` (RFC3339 timestamp or Go duration like `1h`; omitted → zero) | `[]OntologyEventDTO`          | Append-only audit log of mutations                                                                     |
| GET  | `/neighbors`                      | `?node=ID` (required)                                                  | `[]string`                    | IDs of constructs reachable in one hop                                                                 |
| POST | `/ontology/strength`              | `SetStrengthRequest`                                                   | `204 No Content`              | Assert a strength for one proposition; writes `Assertion` on its relationships, outranking both learned layers; audit-logged |
| POST | `/ontology/deprecate`             | `DeprecateRequest`                                                     | `204 No Content`              | Soft-delete a proposition (the relationship is retired: out of the traversal, kept for audit)          |
| POST | `/ontology/construct`             | `AddConstructRequest`                                                  | `204 No Content`              | Append a new construct (append-only; constructs are domain-stable)                                     |
| POST | `/ontology/proposition`           | `AddPropositionRequest` (`direction` is `"+"` or `"-"`)                | `204 No Content`              | Add a validated proposition; `ValidateProposition` rejects direction contradictions                    |
| POST | `/agent/reset`                    | `ResetRequest`                                                         | `204 No Content`              | Discard what a `(from, to)` pair learned: both layers cleared, confidence to zero, pair window emptied, discard count journalled. Keeps the claim and any assertion; does not delete the relationship |
| POST | `/candidates/{id}/confirm`        | optional `ConfirmRequest{from}`; required for an undirected candidate  | `204 No Content`; `400` when the orientation is missing or wrong | Promote a proposer candidate: a Discovered relationship between properties, or a validated proposition between constructs |
| POST | `/candidates/{id}/reject`         | path only                                                              | `204 No Content`              | Permanently suppress a candidate within the session (an operator's review)                             |
| POST | `/candidates/{id}/defer`          | path only                                                              | `204 No Content`              | Out of the pending list for the session (an operator's review)                                         |
| POST | `/ingest-sample`                  | `MetricSampleRequest`: `node_id`, `metric_type`, `value`, `timestamp_unix`, `event_id`, plus optional `subject`, `unit`, `range`, `source`, `chart` | `204 No Content` (routed, unscoped) or `202` `{"routed":false,...}` (unrouted, or any scoped sample) | Feed the facade's `IngestSample`; see §5 |
| GET  | `/state/estimate`                 | `?target=` (required) `[&id=][&assume=<property>=<value>]*[&without=<subject\|property>]*` | `EstimateResult`              | Counterfactual answer from the map under `assume`/`without` hypotheses; `id` names the decision (default `est-<revision>-<target>`), reproduced later via `GET /state/decisions/{id}` |
| GET  | `/ui/...`                         | —                                                                      | static assets                 | Embedded HTML/CSS/ES modules for the console; served by `http.FileServer` over an `embed.FS` sub-tree   |

Errors on the new endpoints follow a single shape:

```json
{"error": "Content-Type must be application/json"}
```

`writeError` (in `pkg/agentmain/routes.go`) is the only path to a non-2xx response. The four surviving pre-Phase-1 endpoints retain `http.Error`'s plain-text body for backward compatibility.

### CSRF mitigation: `requireJSON`

There is no auth in v1 — the daemon is intended for lab-network localhost. To stop a malicious page in a browser from issuing cross-origin mutations against a daemon on `localhost:8080`, every body-bearing POST handler calls `requireJSON(r)` and rejects requests whose `Content-Type` is not `application/json`. Browsers do not send that header on simple cross-origin form posts, so a CSRF attempt fails the Content-Type check before reaching the facade. The path-only candidate-review endpoints (`/candidates/{id}/{confirm,reject,defer}`) skip the check because they take no body; the candidate ID being unguessable in practice (UUID-shaped) is the mitigation.

This is sufficient for the v1 threat model. When the agent grows beyond localhost, a token-based auth layer is the next step (tracked in the plan's "Out of scope for v1" section).

### Direction on the wire: `"+"` vs `"-"`

`types.Direction` is a Go `int` internally (0 / 1). The DTO layer in `pkg/agentmain/dto.go` converts it to `"+"` (positive) and `"-"` (negative) before JSON serialization. Raw integers would render unreadably in CLI tables and UI legends; the string form preserves the publication notation. Mappers — `edgeToDTO`, `propositionToDTO`, `constructToDTO`, `eventToDTO` — are the only places conversion happens.

### Static UI: `embed.FS` with no explicit redirect

`pkg/agentmain/static.go` declares `//go:embed all:static` and exposes the sub-tree under `/ui/` via `http.FileServer(http.FS(sub))`. The `all:` prefix is required so dot-prefixed files (e.g. `.gitkeep`) are bundled into the binary.

There is no explicit `/ui/{$}` → `/ui/index.html` redirect. `http.FileServer` already serves `index.html` for directory roots that end in `/`, and it independently canonicalizes any URL ending in `/index.html` back to `./`. The two behaviors compose into an infinite redirect loop if you also add a manual `/ui/{$}` → `/ui/index.html` handler — which is what the v0 expansion did, and what hotfix `edffaa3` removed. The rule is: trust the file server, do not redirect.

### The `mapctl` CLI

`cmd/mapctl/` is a separate Go binary that speaks the same HTTP API. It exists for three reasons:

1. **Scripting.** `--json` makes every subcommand emit a parseable payload, suitable for Bash pipelines and CI checks.
2. **Demo control.** Subcommands map one-to-one to mutations the UI offers, so a recorded terminal session is a deterministic alternative to a click-through.
3. **Headless ops.** RPi4 nodes often lack a browser; the CLI is the only operator surface there.

| Subcommand                          | Wraps                                       | Notes                                                       |
| ----------------------------------- | ------------------------------------------- | ----------------------------------------------------------- |
| `graph`                             | `GET /graph`                                | Default table; `--json` for raw                             |
| `edges --from --to`                 | `GET /edges`                                | Per-relationship basis, effective, established, assertion and recent; the multigraph fan-out when a specification declares two claims over one pair |
| `history --since`                   | `GET /history`                              | RFC3339 or duration                                         |
| `strength <id> <value>`             | `POST /ontology/strength`                   | Assert a strength on one proposition                        |
| `deprecate <id> <reason>`           | `POST /ontology/deprecate`                  | Soft-delete                                                 |
| `construct add <id> <name> <desc>`  | `POST /ontology/construct`                  |                                                             |
| `proposition add <id> <f> <t> ±<s>` | `POST /ontology/proposition`                |                                                             |
| `reset <from> <to>`                 | `POST /agent/reset`                         | Discard the evidence, keep the claim                        |
| `candidates [list|confirm|reject|defer]` | `GET/POST /candidates*`                 |                                                             |
| `estimate <target> [--assume p=v]... [--without x]...` | `GET /state/estimate` | Answer from the map, optionally under hypotheses; prints the decision id |
| `recommend` / `simulate`            | the corresponding POST                      | Existing endpoints                                          |
| `watch graph|edges`                 | polled GET                                  | 2s ticker; clear-screen unless `--no-color`                 |
| `dot`                               | `GET /graph` → Graphviz                     | Direct paste into `dot -Tpdf`                               |
| `health` / `version`                | `GET /healthz` / `GET /version`             | `version` also prints client build                          |

DTOs are duplicated in `cmd/mapctl/client/types.go` rather than imported from `pkg/agentmain` — the duplication is the contract. Treating the daemon as a remote service from day one means a third party can write a Python or Rust client without reverse-engineering Go types.

Dependencies: `github.com/spf13/cobra v1.8.1`, `github.com/olekukonko/tablewriter v0.0.5`. `tablewriter` is pinned below 1.x because the 1.x API revamp is breaking.

### The operator console

`/ui/` serves a single-page console over the state model. It is a tree of ES
modules under `static/js/`, embedded in the binary alongside `index.html` and
`style.css`.

| Module | Holds |
| --- | --- |
| `app.js` | the poll loop (it reads `/relief`, `/relief/actions` and `/workloads` with `/state`), the census strip, theme, toasts, the prompt dialog |
| `api.js` | every route the console calls, and how this agent reports failure |
| `model.js` | effective-strength precedence, per-property value history, subject grouping, workload names and the throttled/frozen facts |
| `force.js` | the force-directed layout |
| `graph.js` | the SVG canvas |
| `charts.js` | sparkline, strength layers, diverging contributions, census bar |
| `inspector.js` | property and relationship detail, and the counterfactual studio |
| `drawer.js` | actions, journal, decisions, discovery, subjects, cluster, backbone, ask |

**No external dependency.** The previous viewer pulled Cytoscape.js from
`unpkg.com`. A CDN pin makes the operator surface of an offline, air-gapped or
metered node a blank page — which is the deployment this agent exists for — so the
layout and every chart are drawn here instead. `TestStaticUI_HasNoExternalDependency`
pins that: a reviewer adding a remote `<script src>` fails in CI rather than on a
disconnected device.

**What the canvas encodes.** Each visual channel carries exactly one meaning, and
the two that would otherwise compete are separated deliberately:

| Channel | Meaning |
| --- | --- |
| edge colour | the relationship's sign (diverging blue/red) |
| edge width | effective strength — the number the agent reasons with |
| edge opacity | confidence (never a hue; hue is reserved for sign) |
| edge dash | nothing observed yet — distinct from a measured zero |
| midpoint dot | provenance: seeded / learned / discovered / asserted |
| node shape | observed (filled) vs derived (ring and core) |
| node ring | lifecycle: active / stale / retired |
| node arc | the value within its declared range, when one was declared |

Properties scoped to a subject are enclosed and labelled with that subject, so the
picture answers *which workloads are running* before anything is clicked. The machine's
own properties are enclosed too — beside the workloads, not around them. In a node-local
map everything is on this machine, so a box around the whole canvas would say nothing;
a box of its own says which signals are the machine's and which a workload's. It is
drawn solid where a workload's is dashed, labelled with the machine's facts
(`di-node · 4 cores · 7.7 GiB · 18.3 GiB disk · Ubuntu 24.04.5 LTS`), and clicking it
shows every fact with its source.

**The drawer points at the canvas.** A Discovery row, clicked, highlights the
candidate's two ends and draws the would-be edge between them, dashed in the colour a
discovered relationship takes; opening a decision's trace highlights the properties and
relationships it read and, for a candidate review, the candidate. Everything else is
dimmed and the view zooms to what is pointed at.
An end the canvas cannot draw — cleared by the search, or no longer in the map — is
named in a toast rather than silently left out. Escape or a click on empty canvas lets go.

**The console shows what the agent did.** `GET /relief`, `/relief/actions` and
`/workloads` are read with every `/state` poll. The **Actions** pane is the drawer's
default when `/relief` answers, and the pane it lands on is chosen once, not on each poll.
Its strip is the loop's own counts (`predictions_held` is shown as a percentage and as
"—" until something has been kept or reverted) plus the frozen workloads counted from
`/workloads`. A row is one action, newest first: workload, cores before → after, predicted
against observed drop, outcome. Opening it highlights `cpu_utilization@<subject>` and
`cpu_contention_ratio` and reads two decisions: the act (notice, attribution, plan, the
change) and the verify, named on the action as `verify_id` (the latest step's; after a
further step is reverted it is the failed step's verify, the one that caused the revert). The verify stage states the
observed and predicted drop, whether another workload moved (confounded), and whether the
outcome fed the map or why it did not. When the journal has evicted a decision the
stage says "evicted from the journal" instead of inventing one. The page polls `/relief` and
`/relief/actions` each tick and `/workloads` every fourth (or when the set of subjects changes),
never holds the map's redraw back for them, and stops asking once `/relief` has said 404; a
`/relief` that fails otherwise reads "The agent did not answer", not "not enabled". A
`throttled` badge on a `dry-run` runtime (`runtime` on `GET /relief`) reads "throttled (dry-run)". With relief off the `/relief*` routes are 404 and the
pane says "Relief is not enabled in this agent's spec". A pod's box is labelled with the
runtime's workload name when `/workloads` lists it (otherwise the collector's labels, then the
subject), and carries `[throttled]` while an action is in flight or kept, and `[frozen]`
while policy forbids touching it.

**Honesty rules the console enforces.** These exist because the state model draws
distinctions a naive renderer would flatten:

- a relationship nothing has been observed about has **no** strength, and is drawn
  dashed rather than as a measured `0.00`;
- the three strength layers are shown together with the one that *answers* marked,
  because precedence is by authority — assertion over measured over established over recent — and
  not arithmetic;
- the journal states its window (`held`, `dropped`, `oldest_revision`) so an empty
  list is not readable as "nothing happened";
- an undeclared range is never normalised, so no value arc is drawn for it;
- `sign_suspect` is surfaced as an alarm, because such a relationship reports a
  strength near zero and would otherwise read as a quiet system;
- an empty list says *why* it is empty — "the proposer needs N pairs" rather than
  "no candidates".

**Two panes answer questions wider than one property.**

*Cluster* lists the peers and, through `GET /state/where`, locates one property
across every node that holds it. The answers are kept apart and no aggregate is
offered, because the reason to ask several nodes is to be able to pick one. On a
four-machine replay of the testbed this reads `cpu_utilization` as 0.005 on the
master and 0.29–0.30 on the three workers: a mean of 0.22 would be the load on no
machine in the cluster.

*Ask* is the natural-language surface over `POST /explain`. The citation list is
rendered as the primary result rather than a footnote, because the agent runs a
deterministic validator over those citations before releasing the answer — one
naming a number the map does not hold is refused rather than shown. Each cited
property selects itself on the canvas, so a claim and the state that justifies it
are one click apart. With no provider configured the endpoint answers 501 and the
pane says which flag turns it on.

**Value history is the console's own.** The map holds a current value per property,
not a series, so the sparkline is accumulated from successive polls and labelled as
console history. It is never presented as the agent's record.

**Liveness** is a 2s poll of `/state`, adjustable or steppable, paused when the tab
is hidden. The revision counter, the journal, the value arcs and the edge-flow
animation all move from the same snapshot. Mutation flow is unchanged: POST with
`Content-Type: application/json`, `204` → toast → re-poll; non-2xx → toast carrying
the server's own `{"error":"…"}` text. Same-origin only — no CORS configuration.

### Why three layers, not one

Each layer answers a different question:

- **HTTP API** answers: "what can the agent do, expressed in JSON?"
- **CLI** answers: "what can an operator do from a script, expressed in subcommands?"
- **Web UI** answers: "what can a reviewer see and click, expressed visually?"

Collapsing them — e.g. embedding HTML rendering inside Go handlers, or building a TUI that calls `pkg/semmap` directly — would couple the daemon to its consumers. Keeping the HTTP boundary firm means the Netdata adapter (step 5 in `research-docs/SEMANTIC-MAP-STATUS.md`) can land without touching the control surface, and any future client (mobile, TUI, fleet view) can be added without changing the daemon.

---

## 10. Coordination

The dissertation calls this work a *Context-Aware Agentic Framework for **Decentralized** Edge Computing*. Single-agent EMA convergence is the mechanism; multi-agent coordination is the spine. Without it, `RecommendPeer` is dead code, "decentralized" is aspirational, and P7 has nothing to build on.

This section covers the registry and the trust mechanics that decide *where work goes*. What peers know about their own systems — and why it is held apart from local state rather than merged into it — is [§5, Peer state](#peer-state-knowledge-crossing-a-node-boundary).

### The peer registry — concrete, not a contract

In v1 the peer registry lives at `pkg/peers/` as a **concrete package**, not a contract. The contract surface stays at five (Collector, Ontology, Reasoner, Proposer, Tuner) — broadening it would force every profile to re-implement a peer table that the edge-minimal profile already provides for free. We promote to a contract when a second implementation arrives:

- SQLite-backed registry for the edge-standard / cloud-full profile so trust history survives restarts;
- gossip-based discovery (mDNS / dat-style) so peers find each other without a static `peers.urls` list.

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
POST /agent/tune {"intent": "prioritize performance", "operator": "alice"}
          ↓
TunerContract.ParseIntent(text) → []TuneIntent{PropositionID, Delta}
          ↓
SemanticMap.Tune: resolve each relationship's CURRENT effective strength from the
                  state model — assertion, else established, else recent, else a
                  neutral 0.5 with the anchoring recorded in the rationale
                  → newStrength = clamp(old+delta, floor, ceil)
          ↓
TunerContract.Validate(adjustments) — hard bounds check
          ↓
SemanticMap.SetPropositionStrength × N        ← writes Assertion on the model, with
                                                actor and reason, per proposition
statemap.RecordOperatorIntent(text, operator) ← one operator.intent event naming the
                                                whole act and what it touched
          ↓
Return []TuneAdjustment: PropositionID, OldStrength, NewStrength, Rationale
```

Two details in that flow are load-bearing and were both once wrong.

**The delta anchors to what the relationship currently reports, not to a fixed table.** On a saturated agent that is its established baseline; on a warm one its recent estimate; on one that has measured nothing, a neutral 0.5 with the anchoring stated in the rationale. Anchoring to a table instead would let a +0.12 nudge overwrite what the machine measured, which is the direction of authority this design deliberately reverses — an operator overriding evidence should have to say so, and the audit record should show both numbers. The neutral anchor exists so an operator can steer a fresh agent, when their knowledge is worth most; it differs from the seeded prior it replaces in that it appears only because someone asked for it, carries their actor and reason, and reports provenance `asserted`.

**Adjustments apply through `SemanticMap.SetPropositionStrength`.** There is no longer an ontology method of that name to call by mistake — see §2 on why it was removed. The consolidated intent is journalled alongside the individual assertions, because reading those separately afterwards gives no way to tell one coordinated adjustment from several unrelated ones that landed together.

**A tune reaches the decision in full, and that is a correction rather than a design choice.** The effective strength used to be `(1 − c)·prior + c·learned`, with a tune writing the prior — so a declared δ moved the sensitivity by `(1 − c)·δ`, and at `c = 1` by exactly nothing. That was measured: on a saturated k0s daemon `prioritize performance` moved both sensitivities by 0.000000 while recording itself faithfully in the audit log. It was reported for a while as intended arithmetic with a real limitation attached — *the better the agent knows its machine, the less an operator can steer it* — and that reading was wrong. The inversion was a defect of the write target, not a property of tuning.

An assertion is now a field of its own that outranks both learned layers and does not decay. The same operation on the same saturated agent moves the effective strength from an established 0.6023 to an asserted 0.7223 and the sensitivity by the declared 0.1200. The established layer still reads 0.6023 at `GET /edges`, so the audit shows what was measured beside what was asserted, and `Basis()` names which one answered.

Deprecation remains distinct, and now differs in *meaning* rather than in reach. It removes a relationship from the sensitivity sum outright and selectively: retiring the sole relationship terminating at the pressure construct takes that sensitivity and its contribution to exactly zero while the one terminating at the resource construct is untouched. A tune says the relation is worth something else; a deprecation withdraws the claim while keeping the measurement. Neither moves a reported *level*, because a level is a measurement and an assertion is not one — an operator cannot make a machine less busy by declaring a preference.

### Hard bounds

| Proposition class | Floor | Ceiling |
|---|---|---|
| Per-proposition floor (`per_proposition_floor`) | as declared | 0.95 |
| All others (`global_floor`) | 0.10 | 0.95 |

The per-proposition table is **empty in the committed specification**. It previously raised the floor to 0.30 on the four security-adjacent propositions, so that operators could not fully deprioritize security compliance under resource pressure; those propositions are no longer in the graph. The mechanism is retained because a floor is a property of a claim rather than of a construct, and it is keyed by proposition so a new claim can be given a policy at runtime without a code change.

### V1 rule table (RuleBasedTuner)

The vocabulary is declared in `domain_spec.json`, not compiled in. The committed
specification declares **three** rules, because the graph it describes carries two
relationships:

| Keyword group | Example phrase | Propositions adjusted |
|---|---|---|
| performance, throughput, latency, fast, speed | "focus on throughput" | P3 +0.12 |
| energy, power, efficient, battery, watt, cost | "prioritize energy efficiency" | P10 +0.12 |
| responsive, tail, p95, p99, jitter, pressure | "improve tail latency" | P3 +0.08 |

Where an intent string matches more than one rule, the larger magnitude wins per
proposition — so a phrase matching both `performance` and `responsiveness` resolves P3 to
+0.12, not +0.20. Direction modifiers ("deprioritize / reduce / lower / minimize") negate
all deltas; the default is increase.

Four rules were retired with the constructs and propositions they addressed, and
`retired_intents` records each one with what it would have adjusted rather than deleting
it: **security**, **reliability**, **maintainability** and **community** for constructs a
running cluster cannot exhibit or nothing routes to, and **connectivity** for one this
deployment measures at ~10⁻⁵ of its normalisation reference. An operator asking for those
is asking about a selection-time property, and the answer belongs to Di-Select. The
mechanism still supports signed per-proposition deltas — a rule raising one relationship
while suppressing another — but no committed rule uses it, because dropping P2 and P13
emptied every negative delta the vocabulary declared. The tuner's compliance suite
exercises that path instead.

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
- **di-agent** — `edge-minimal` profile, `sources.collectors` a cgroup entry then a Netdata one at `http://localhost:19999` (`collapse: false`), `profile.kd: k0s`, polling every 5 s. Runs as a systemd service reading `/etc/di-agent/env` for `NODE_ID` and `REGIME`.

Peer mesh: each agent registers the other two via `POST /peers`. The server derives the peer ID as `sha256(url)[:12]` and initialises trust at 0.5; `05-peers.sh` follows each add with `POST /peers/{id}/trust {"value":0.8}` to set the operational trust. diag-1 uses this mesh for `/recommend`.

### What the binary sees on each node

The `NetdataCollector` reads every chart in one request:

```
GET /api/v1/allmetrics?format=json
```

reads the chart types from `GET /api/v1/charts` when a chart appears that it has not described, and keeps the charts its include list selects (§5). With Netdata as the only collector it also maps:
- `system.cpu`, every dimension but idle and iowait → `CPUUtilization` → RC-adjacent edges
- `system.ram used/(used+free+cached+buffers)` → `MemoryUtilization` → RC-adjacent edges
- `system.cpu_some_pressure_stall_time` (ms stalled per second) → `CPUPressureRatio`, and the I/O and memory equivalents
- `system.net InOctets` / `OutOctets` (kb/s, outbound drawn negative) → `NetworkRxBps` / `NetworkTxBps` against a 1 Gbps reference

At idle k0s, CPU utilization is ≈ 0.05, so the reported resource level is low and the pressure series is close to flat — which means the paired estimator forms few pairs and declines to move on what it does form, and confidence stays near zero. That is the estimator working rather than the demo failing; §"What determines the saturation point" is the general form. The bursty regime (α=0.30, N=200) reaches saturation sooner, so diag-1 accumulates confidence faster than the stable VMs (α=0.05, N=1000).

**Note on `stress-ng`**: driving CPU is what gives *both* endpoints something to vary, which is the condition a pair needs — an idle demo can run indefinitely and teach the map nothing. Load also makes the reported levels move, and since the cost estimate now leads with the level rather than accumulating deviations, `ResourceCost` tracks utilization directly instead of being pushed toward zero by negative-direction terms as it once was. The coordinator demo works cleanly at idle or light load without `stress-ng`.

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
- **`map.proposer.enabled: false`**: MI proposer disabled on 2-vCPU VMs to keep CPU headroom for workload and measurement.

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
             │  (system prompt: pkg/agentmain/prompts/explain-v2.md)
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

Default: Ollama on `localhost:11434/v1`. The daemon does not ship an LLM binary — install one out of band. `POST /explain` returns HTTP 501 with a clear remediation message when `explain.provider: none` (the default).

### Configuration

```
explain.provider      none | openai-compatible          (default: none)
explain.url           http://localhost:11434/v1         (default)
explain.model         qwen2.5:7b-instruct               (default)
explain.prompt        pkg/agentmain/prompts/explain-v2.md   (default)
explain.api_key       ""                                (write ${EXPLAIN_API_KEY} to read it from the environment)
```

### Prompt versioning

The system prompt lives at [`pkg/agentmain/prompts/explain-v2.md`](go/pkg/agentmain/prompts/explain-v2.md), loaded once at daemon startup. Every response records a `prompt_version` field (the first 12 hex chars of `sha256(prompt)`), so a paper's replication package can pin the exact prompt used for reported results. Bump the filename (v2, v3, …) rather than editing v1 in place — old snapshots then remain reproducible.

### Response shape

```json
{
  "answer": "The dominant relationship is P10 (PS→RC, effective 0.62 from its recent estimate at confidence 0.03; no established baseline yet).",
  "citations": [
    {"kind": "edge", "id": "P10", "ema_weight": 0.62, "effective": 0.62, "basis": "recent", "confidence": 0.03, "n_observations": 15}
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

`pkg/explain` is on the **operator-facing** surface, not on the ingestion or reasoning path. Every P6 result reported in `research-docs/` uses `explain.provider: none`: the reasoner, updater, prior-init pipeline, and convergence measurements are pure Go and fully deterministic. The Explain layer is a demo asset and an operator convenience, not a load-bearing component of the scientific claims.

### Tests

- `pkg/explain/tools_test.go` — tool registry + citation validator against live SemanticMap.
- `pkg/explain/openai_test.go` — reflection loop end-to-end against a scripted mock LLM. Covers happy path, tool-then-answer, and fabrication-then-fix.
- `pkg/agentmain/explain_route_test.go` — route wiring: 501 when disabled, 400 on malformed input, 200 with grounded answer through a mock LLM.

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

Whole-cache invalidation rather than per-key is deliberate: the graph is single-digit in both nodes and edges. Reasoning about which cached results a given mutation could have affected costs more, in code and in bug surface, than just refetching.

Three properties of the transcript are load-bearing:

- **Only successful turns are recorded.** A response that failed Gate 1 or was rejected by Gate 2 is returned to the caller but never enters session history. Persisting it would replay the model's own rejected output as an assistant message on the next turn — the model would treat its mistake as established context. That is exactly the circular self-confirmation the anchors in §8 exist to prevent, and it fails *silently*, because a bad answer in the transcript looks like history rather than like an error.
- **Only the answer text is replayed**, not the serialized `ExplainResponse`. Replaying the full DTO would push `tool_trace`, `usage`, `plan`, and `critic_verdict` back into context — measured at ~16× the answer's size (813 bytes vs 49 for a typical response, ~4 000 tokens at a full 20-turn buffer). None of it is actionable for the model, and on a small-context local model it can evict the system prompt. The failure mode there presents as *"the model forgot its instructions"*, which is expensive to trace back to its cause.
- **`Get` returns a defensive copy.** Two concurrent `/explain` calls may legitimately carry the same `session_id`; handing out the live `*Session` would race the turn slice against `AppendTurn`. Mutation goes exclusively through `AppendTurn` / `CacheTool` / `InvalidateOnMutation`, all of which take the store lock. `TestSessionStore_ConcurrentAccessIsRaceFree` exercises this under `-race`.

Idle sessions are swept on `Create` rather than by a background ticker. `Create` is the only moment the store grows, so it is exactly when reclaiming dead entries matters, and a goroutine would need lifecycle management and leak tests to reclaim memory nobody is contending for.

Sessions are **not persisted**. They carry no scientific role — P6 results come from the reasoner, not the Explainer — so crash-recovery machinery would be complexity without a customer. The durable substrate already exists: `/graph` and `/history`.

An unknown `session_id` returns an error rather than silently minting a fresh session. A client that lost its ID should find out, not get an amnesiac conversation that looks like it worked.

### Streaming

`"stream": true` switches the response to **NDJSON over chunked encoding** — one compact JSON event per line.

```
{"event":"session","session_id":"a3f2..."}
{"event":"planning"}
{"event":"plan","plan":{"approach":"...","steps":[...]}}     ← plan always non-nil
{"event":"plan_failed","error":"..."}                        ← the alternative; no plan field
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
explain.provider         none | openai-compatible          (default: none)
explain.url              http://localhost:11434/v1
explain.model            qwen2.5:7b-instruct
explain.prompt           pkg/agentmain/prompts/explain-v2.md
explain.planner_prompt   (derived: <prompt dir>/planner-v2.md)
explain.critic_prompt    (derived: <prompt dir>/critic-v2.md)
explain.keep_alive       30m
explain.sessions         true
explain.api_key          ""   (write ${EXPLAIN_API_KEY} to read it from the environment)
```

A missing *derived* planner/critic prompt disables that stage with a log line; an explicitly-specified path that fails to load logs a warning and disables the stage. Either way the daemon boots — a partial prompt directory degrades features, it does not block startup.

### Prompt versioning

Three prompts, each versioned by filename (v1 is kept unedited; v2 removed the hard-coded constructs): `explain-v2.md`, `planner-v2.md`, `critic-v2.md`. The answering prompt's `sha256[:12]` is stamped on every response as `prompt_version`. No prompt names a construct: each carries `{{DOMAIN}}`, which startup fills with the loaded domain model's constructs, routes and propositions (`domain.Spec.PromptSummary`), or with the statement that none is loaded. The hash is taken after that substitution, so it identifies what the model was actually told. Earlier prompts listed Di-Select's seven constructs and the seeded-prior blend, describing a map this agent does not hold; `prompts_test.go` now fails on either. Bump the filename rather than editing in place, so results reported against v1 stay reproducible.

### What v2 does not change

- **The reasoner, the state model, and the prior-init pipeline remain pure deterministic Go.** No LLM touches the ingestion or reasoning path.
- **The tool registry is still read-only.** Planner, answering agent, and critic all get the same six read tools. None can mutate.
- **`explain.provider: none` remains the default.** All P6 results in `research-docs/` are produced with the Explain layer off.

### Tests

| File | Covers |
|---|---|
| `pkg/explain/session_test.go` | LRU eviction, touch-on-get, TTL expiry, mutation-driven invalidation, turn-buffer trimming |
| `pkg/explain/planner_test.go` | Plan parsing (fences, prose, truncation), structural validation, evidence collection, error continuation, budget capping, cache hits |
| `pkg/explain/critic_test.go` | Verdict parsing, issue truncation, unactionable-rejection upgrade, prompt assembly, revision formatting |
| `pkg/explain/streaming_test.go` | Event ordering, terminal event on success and failure, payload correctness, nil-emitter safety, interface satisfaction |
| `pkg/explain/openai_test.go` | v2 integration: planner-runs-tools, planner-failure fallback, critic approve, **critic catches structurally-valid-but-wrong**, critic-failure ships anyway, session mint and persist, unknown session, usage accounting |
| `pkg/agentmain/explain_route_test.go` | NDJSON content type and parse, 501 for non-streaming explainers |

All run against a scripted mock LLM. No test requires Ollama.

---

## 15. Autonomous Candidate Review (`pkg/autonomy`)

Candidate review is the one place the map waits on a person, and the first decision the
agent takes on its own authority. How much authority is an operator setting
(`loops.review.level`); what the agent does with it is always a decision record.

**Tiers, cheapest first.** A candidate goes up only as far as it has to.

| Tier | Decider | Why it is the right tool |
| ---- | ------- | ------------------------ |
| 0 | rules in the daemon | Some candidates are settled by what they are. A `composition` candidate is a part against its whole, true by definition and the relationship `without=` answers from, so it is confirmed. There is one rule: the other kinds (`same-metric`, `same-subject`) make a shared driver or accounting *likelier*, which is a judgement about this system, so they are evidence for the models rather than verdicts. |
| 1 | decision model | Acting without a person needs a calibrated signal, and a chat model's self-reported confidence is not one. A decision model (Ollama's `/v1/systemone`: a state and typed questions in, a probability per option out, in one forward pass) answers confirm/reject/defer as a distribution, so the threshold an action needs is a probability, not a word. |
| 2 | reasoning model with tools | When the decision model is unsure, the question is usually one the evidence summary cannot answer: is this metric following load, does the workload exist, what else does the source correlate with. A tool-calling model reads the map (read-only) and writes an analysis. Its own suggested action is recorded and *not* acted on: the decision model scores the candidate again with the analysis in its state, so the number compared with the threshold is still a decision model's. |
| — | undecided | What no tier was sure of stays pending with its review, and is reviewed again when its evidence moves. No tier is a person: a fleet of machines with different topologies cannot be checked one candidate at a time, so an operator who wants to decide sets a lower level. |

**Levels.** `suggest` (0) records every tier's verdict as a suggestion and applies none —
today's behaviour with the reasoning written down. `rules` (1) lets tier 0 act. `bounded`
(2) lets the model tiers confirm and defer; a model's rejection is left undecided, because a
rejection hides a pair. `full` (3), the default, lets every tier act above its threshold; the lower levels are the manual modes an operator opts into. Thresholds
differ by action for the same reason: confirmation is undone by retirement with history
kept, deferral costs only time, rejection the most. The defaults (0.85 / 0.95 / 0.7) are
not calibrated — there is no labelled set of candidates yet, and building one from an
operator's past reviews is a measurement round.

**An agent's decision is provisional; an operator's is not.** An agent's rejection or
deferral carries `Reason: "agent"`: the pair is still tested and the candidate returns to
pending when its evidence moves past what the review saw (sign, kind, or `|r|` by more than
two standard errors of Fisher's z). The same test decides when an undecided candidate is
reviewed again, which is what keeps the models from being asked the same question every
cycle. A candidate with a stale end waits: its window is not moving, so a decision now
would be on frozen evidence.

**Rules first.** Each cycle applies tier 0 to every due candidate before any model is asked: a rule
costs nothing and a model call a minute or more on a node, so a candidate a rule settles never waits
behind the models' queue (on the case-study node a composition candidate waited an hour behind
reasoning-tier reviews), and it does not count against `per_cycle`, which caps the models' work.

**An outage is not a review.** A model that cannot answer — the server still loading, the
network down, a request it refuses — leaves the candidate untouched: no decision, no
`Review`, so it is due again next cycle. Recording it as undecided would shelve the
candidate until its evidence moved, which on a steady machine may be never. A model that
failed is not asked again that cycle, since the next candidate would fail the same way;
what needs it waits, what does not — a rule, or tier 1 when only the reasoning model is
down — is still reviewed, and the outage is counted once in the status. A default that
names a reasoning model therefore cannot stall review on a node that has none. Tier 2's tool loop has a hard end: calls past the budget
are answered with an error, and a model told to answer that calls tools again fails the
review rather than looping.

**Orientation.** An undirected candidate cannot be confirmed with an arbitrary end. The
decision model is asked which end drives the other, with "unclear" as an option; the
confirmation needs the confirm threshold on both questions, or it is undecided.

**Traceability.** A review is `Map.Decide` like an estimate: both properties are read
through the `DecisionBuilder`, and `Inputs` holds the candidate, its neighbours, the level
and thresholds, the exact state text each model was given, each model's probabilities, and
the reasoning model's analysis and tool trace. The candidate's `Review` names the decision,
the confirmed relationship's note names the reviewer and the decision, and `GET
/state/decisions/{id}` reproduces it after the map has moved on. Model names and prompt
versions (`review-decide-v1`, `review-reason-v1`) are in every record because the model
choice is a snapshot.

**What it does not do.** `/explain` stays read-only: autonomy is a separate surface whose
only writes are the four review actions. No hosted model is contacted; the URL is the
operator's. What no tier settles waits for its evidence to move: a tier 3 on a peer or a
bigger model is not built, because where it should live is open.

**Every part is substitutable.** The reviewer depends on interfaces — `Map` for the
semantic map, `Rule` (tier 0), `Decider` (tier 1), `Reasoner` (tier 2), `Tool` (what tier 2
may read) and `Renderer` (how evidence becomes text) — and the daemon builds each from a
registry by the name a flag gives. Each tier has its own URL and model, so the decision
model can sit on the node while the reasoning model sits on a peer with memory to spare.
Two tier 1 backends exist because the calibrated signal can come from two places: a
decision model's typed-question endpoint (`systemone`, Ollama only), or any
OpenAI-compatible server returning token log-probabilities (`openai-logprobs`), where the
question is asked as lettered options with a one-token answer and the distribution is the
letters' probabilities, normalised. The second carries `OptionMass` — how much of the
model's probability fell on any letter — and below one half the verdict is undecided: a model
that wanted to say something else has not answered the question. The verdict records the
backend, model, question-set version and renderer, so substituting any of them leaves the
records comparable. The `text` renderer (`text-v2`) opens with the machine's facts —
`Machine: QEMU Virtual Machine, 4 CPU cores (Apple CPU (part 0x000)), 7.7 GiB memory,
18.3 GiB disk, Ubuntu 24.04.5 LTS` — so a model reads a correlation between two
utilisations against how much machine there is.

**Models.** On a 24 GB Apple-silicon machine, `nimble-v3:9b-nvfp4` (6.9 GB, MLX) as tier 1
and `qwen3.8:27b-nvfp4` (18 GB) as tier 2; the two do not stay resident together, so a
tier 2 review pays a model load. On an 8 GB node, `tev1:4b` (4.4 GB) as tier 1 with no tier
2. Nothing is known yet about either decision model's accuracy on candidates like these.

| Test | Covers |
| ---- | ------ |
| `pkg/autonomy/parts_test.go` | A part registered from outside is built by name; unknown names are refused; `openai-logprobs` normalises over the letters and records the mass; little mass is undecided; a refused `reasoning_effort` is dropped; a reasoner that keeps calling tools is stopped |
| `pkg/autonomy/evidence_test.go` | The evidence names the machine from its facts, and says nothing of one when none were read |
| `pkg/autonomy/autonomy_test.go` | The models are given the machine's facts and the decision keeps them; which tier acts at each level; an unsure model goes to tier 2 and then the operator; tier 2's analysis is scored by the decision model; the decision record's inputs; a reviewed candidate is not reviewed again; an undirected confirmation needs a driver; an unreachable model records nothing, stops the cycle and is retried |
| `internal/minimal/proposer_mi_test.go` | An agent's rejection reopens when the evidence moves, an operator's does not; annotate keeps a candidate pending |

All run against fake models. No test requires Ollama.

---

## 16. Acting (`pkg/relief`, `pkg/policy`, `pkg/actuators`, `pkg/runtime`)

The map answers what a system is doing; acting is the agent changing that system, and every change is a decision record that can be reproduced from the map state that justified it. The first act is CPU relief: when the node is hot, cap the workload the map says is responsible, check that it helped, undo it if it did not, and give every cap back when the node cools.

**Three parts, one direction of authority.** The *spec* (`loops.relief`) says how the loop behaves. The *policy* (`pkg/policy`) says what the agent may touch and how far: allow and deny lists, a floor, an hourly limit, a cooldown, and freezing a workload outright. The first matching rule overlays the defaults field by field, a floor bound by bound, and the floor that results is what is validated. The spec refuses an allow or deny entry that is not an action (`cpu.limit`, `memory.high`, or the reserved `evict`, which no runtime performs and the defaults deny) and a `match.kind` that is not a workload kind (`pod`, `container`, `unit`, `app`): a misspelt deny would deny nothing and a misspelt kind would match nothing. The lists live in `pkg/spec`, which the actuators import, and a test holds them to the registered actions. It refuses `expiry` no longer than `settle` too, since such a change would be given back before it was judged. The *actuators* (`pkg/actuators`, §2) only do what they are told, orchestrator-neutrally, and the agent's ledger keeps what each change replaced, so a revert restores the setting exactly as it was, including "no setting". The *runtime* (`pkg/runtime`) only lists. The loop decides; the policy bounds; the actuator executes. An actuator never decides and a policy never acts.

**Trigger.** The signal is the node's CPU contention, `cpu_contention_ratio` (§5): the share of time some workload waited for a CPU another was using. It is not utilisation; a node can be fully used without anything waiting, and that is not what relief is for. Nor is it `cpu_pressure_ratio`, which the loop used first: a cgroup's PSI stall counts its own throttling, so a pod at its limit raised node pressure with nobody else waiting, and capping it raised pressure further. On the VM every real action was then reverted by its own verification — the lever moved the signal the wrong way. Contention removes each workload's own throttling, so capping the workload that crowds the others lowers what the others wait. Relief begins only when it stays above `enter` for `enter_for`, and ends only when it stays below `exit` for `exit_for`, with `exit` under `enter`, so a node hovering at the line does not flap. A reading counts only while it is fresh: a stale reading, or one that has stopped changing, never counts toward either run, and the loop makes no new change and releases nothing on data that may describe a system that no longer exists. What needs no reading still happens: expiry, a pending revert retried, and the giving back of a change whose workload the runtime no longer lists.

**One action in flight.** The loop changes one workload at a time and waits `settle` before judging, so an outcome belongs to one action. It predicts before acting, from the map's relationship between the workload's share and node contention, or, where the map has none, from the workload's share of node usage times the fraction of its usage the step actually cuts (a floor can hold the cut short of `step`), and journals that prediction with the attribution and the policy that allowed it. `min_effect` is a planning floor: a step predicted to cut contention by less is not taken. After `settle` the step is judged against its own prediction: kept when the observed drop is at least half the predicted one, reverted otherwise. A change taken up after a restart is always kept, because its baseline was read with the cap already in force and the drop it caused is in neither reading; release and expiry give it back as usual. An action that did not help is reverted and its workload cooled down; a reverted action cools its workload down four times longer than a refused one. Predict before, verify after, revert what did not help: the verification is a recorded comparison, not a hope.

**The memory lever.** A second loop runs the same steps through `memory.high` (`pkg/actuators/memoryhigh.go`): it relieves `memory_contention_ratio` (§5) by lowering a pod's cgroup v2 `memory.high`, a soft cap the kernel enforces by reclaim and throttling, never by a kill, and gives it back by writing `max`. Its descriptor names `memory_utilization` as the usage, `memory_total` as the capacity and 64 MiB as the actuator's floor, so the plan is the CPU plan in bytes: from the pod's share times `memory_total`, `step` below it, never below the floors. Never below the pod's memory request either: for `cpu.limit` that floor is the orchestrator's (a limit below the request is refused), for `memory.high` it is a design choice — a pod is promised its request, so it is not cut below it. The default `step` is 0.25: at 0.10 a usage-basis step at `enter` (0.20) cleared `min_effect` (0.03) only for a pod using 1.5 times the node's memory usage, which cannot happen; at 0.25 a share over usage of 0.6 does. **Known bias:** the pod's share is `memory.current` over `MemTotal`, which includes its page cache — what `memory.high` reclaims — while the node's `memory_utilization` excludes reclaimable cache, so a pod's share over node usage can exceed 1, and a usage-basis prediction for a cache-heavy pod runs high. A prediction that runs high is judged against what was observed after `settle`, and a step that did not deliver half of it is reverted. The actuator finds a pod's cgroup by its UID and QoS class, which only the runtime's listing carries, so every give-back (release, expiry, a manual revert) goes through the pod as listed in that pass: a pass that cannot list gives nothing back and tries again. A pod the runtime no longer lists is given back through the UID in its subject, trying every QoS class in both layouts: its `memory.high` is written back to `max` and the change closes as released, expired or manual like any other, and it is `gone` only when its cgroup is gone (below). A `DryRun` actuator asks the real one the same question, so a dry run sees a workload leave too. A workload with no valid identity is the caller's error (`ErrInvalidIdentity`), never `ErrGone`, so `Acting` keeps the entry rather than forget a cap still in force. A pod is `ErrGone` only when its cgroup directory does not exist and the QoS parent it would be in does: a tree with no `kubepods`, or no parent for the pod's QoS class, says the root is wrong, any other error looking for it (`EACCES`, `EIO`) is returned as it is, and a cgroup that is there without a `memory.high` has its memory controller off; none of them is a pod that left, and each keeps the entry. Start is refused when the ledger holds this node's `memory.high` changes and the root has no `kubepods` tree at all.

**Stepping again, and giving back.** A workload that was helped but not enough may be stepped again from its current cap. It keeps one action, whose id is the first step's, and the ledger keeps the first original, so a revert always restores the limit the workload had before the agent first touched it, never an intermediate cap. When contention is gone, caps are released newest first, one per `settle`; the cooldown after a release doubles for a workload that keeps cycling between throttle and release within an hour, up to 4h. A cap the actuator will not give back does not hold up the rest: the next newest is released in the same pass and the refused one is tried again on the next.

**An undo that keeps failing.** A revert, release, expiry or the letting go of a vanished workload's change that the actuator refuses leaves the change in force and is retried every pass. It is not only logged: the action carries `revert_attempts` (refusals since its latest step) and `last_error`, `GET /relief` counts such changes as `stuck`, the console shows `undo failing: <error>` on the action's row, and the first refusal is journalled once per action as a `relief-stuck-*` decision rather than once per pass. Release also happens from Calm, so a loop restarted after pressure ended gives back what it finds.

**A failed change.** An `Apply` that returns an error may still have taken effect, as a resize whose call timed out after the API server accepted it. `Acting` writes its record ahead of the resize and keeps it when the setting reads back changed (§2), and after any `Apply` error the loop reconciles again on its next pass, so the change is taken up as a resumed action and given back by release or expiry rather than left until a restart.

**Timeouts.** Every pass (`Tick`), the start-up reconcile and a manual revert run under one context deadline of `interval`, at least 10s, and the Kubernetes client bounds each request at 30s. An orchestrator that stops answering costs one pass, not the pass lock: a manual revert it does not answer is an error and the action stays in force.

**Restarts.** A graceful shutdown keeps its changes, because reverting on every restart would bring the pressure straight back. On start the loop reconciles the ledger (§2): a change whose workload is gone, or whose setting was changed underneath, is journalled gone; the rest of this node's changes of its action (never another node's, never another action's) are taken up: the newest unexpired one is in flight until `settle` passes and is then kept without being judged (above), the rest are kept for release, expired ones are reverted. Nothing is applied until that has succeeded once, since a change made on top of caps the loop does not know of would record one of them as the original. Where the journal still holds the act that made a change, its prediction and first step are recovered. A change whose pre-agent limit cannot be established after a restart is never deepened: the floor measured as a share of the original would otherwise be a share of the agent's own cap and slide down with every restart. It stays for release and expiry, and the refusal is journalled once. Each unexpired change taken up counts toward `max_actions_per_hour` from its journalled act time, or from the restart when the journal no longer holds it, so a restart does not hand the loop a fresh hourly budget. Cooldowns and the release backoff are per process lifetime: a restarted loop has forgotten them.

**Kill switch.** `POST /relief/pause` stops the loop making and releasing changes. It does not abandon what is in flight: a paused loop still verifies, reverts a failed action, and gives back what expires, because pausing should never leave a harmful change stranded. An operator can revert one action by hand (outcome `manual`). With relief disabled in the spec the routes answer `404`; the loop does not exist. Nor, then, does anything that gives back what the ledger still holds: relief turned off, or a lever removed, leaves that action's changes in force past their expiry. The agent does not try to give them back without an actuator built for the action; it warns of each at start and `GET /actuators` shows them as `built: false`, and an operator reverts them by hand (OPERATING §10).

**The dead-agent bound.** An agent that is gone for good cannot revert anything, and an external janitor is out of scope. What it leaves behind is bounded by the policy floor: no workload is ever cut below its action's `floor.<action>.min` or `floor.<action>.of_original` of its original setting, so the worst a dead agent can have done is the floor, and that bound is the stated guarantee.

### Learning from outcomes

A verified action is the one kind of evidence correlation cannot give: the agent moved the cause end itself and watched the effect end. The loop feeds it back three ways, and every verify decision records what it fed and why not — inputs `confounded` (whether, and which workloads moved or left), `subject_share` (the throttled workload's own share at act and at verify), `evidence` (fed or not, with the reason) and `intervention` (the relationship and what `RecordIntervention` changed).

**What counts as evidence.** Only a verify verdict is an outcome: `keep` (observed at least half the predicted drop) is *agreed*, `revert` is *null*. Expiry, a workload that left, a manual revert and a refused change are not outcomes and teach nothing. Neither does an action with no prediction to judge against, nor any action resumed after a restart: its baseline was taken with the change already in force, so what it observes is noise. A further step on a resumed workload takes a fresh baseline and is evidence again. Nor does an outcome where the cap did not bind: if the workload's usage at verify (share × `cpu_cores`) is under 0.9 of its new limit, it asked for less on its own and the drop is its demand falling, not the cap. The keep/revert verdict is the same in every case but a resumed action, which is always kept; only what it teaches changes.

**Dry runs teach nothing.** Outcomes feed the map only when the runtime really changes the system (`Config.Evidence`, which the daemon sets for every runtime but `dry-run`). Under a dry run nothing was capped, so a drop in pressure after a recorded "action" is the workload's own load cycle; a demonstration run showed every dry-run `keep` to be exactly that coincidence. Recording it would build relationships on actions that never happened.

**Confounding.** At act time — and, for a resumed action, at the tick that gives it a baseline — the loop snapshots every workload's share of node CPU (`Action.shares_at_act`). An outcome is *confounded* when another workload, live at verify time, has since moved by more than `step` relative to its share then *and* by more than 0.02 of the node absolute; a workload that arrived since moved from nothing, and one that was using more than 0.02 at act and is no longer live has left, taking its load with it. The absolute bar keeps a small pod's relative jitter (0.01 → 0.02) from discrediting every outcome. Rises are the exception: capping a workload frees CPU and the contended workloads beside it take it up, which is the relief working rather than something else moving, so workloads present at act whose rises together stay within what the capped workload gave up (plus 0.02) are *absorbed* rather than moved. What it gave up is known only while the map holds its share; without one nothing counts as freed. An absorbed rise is excused only when the outcome agreed: when contention did not drop as predicted, the same rise may be a workload that grew on its own and hid the cap's effect, and excusing it would turn a confound into a null outcome that teaches, so a revert beside an absorbed rise is confounded. A larger rise, a fall, an arrival and a departure always count. The verify decision lists `moved`, `left` and `absorbed`. A confounded outcome is recorded on the relationship's `interventions` counts and changes nothing else.

**Relationships.** Each outcome is a `RecordIntervention` on `cpu_utilization@<subject> → cpu_contention_ratio`. Structure follows runs since the last structural change: two agreeing unconfounded outcomes create the relationship with `intervened` provenance, or promote a seeded, learned or discovered one to it; two nulls retire it. An operator's `asserted` relationship is neither promoted nor retired: its outcomes are counted and the run starts again. Confounded outcomes never move either run.

**Strength from what was measured.** Each unconfounded outcome also measures the relationship: the drop in node contention per unit drop in the workload's share of node CPU, both fractions of the node, clamped to [0, 1] (`measuredEffect`; none when the share fell by no more than 0.02). The pair's mean measured effect is the relationship's `measured` layer, which outranks `established` and `recent` and yields only to an assertion. Co-variation is the wrong measure once the agent acts on the source: a capped workload stops moving with the contention it caused, because the cap holds it still, so the learned layers settle near zero exactly when the relationship is strongest. In the 2026-10-19 case study that left the relationship at 0.047, predicted a drop of 0.0009 under `min_effect`, and relief refused four times while contention was back at 0.59. The verify decision records the outcome's `effect`.

**Effects.** Per workload and prediction basis, the loop keeps an EMA (alpha 0.3, from 1, clamped to [0.1, 2]) of observed over predicted drop, from unconfounded outcomes only, and scales every later prediction for that workload on the same basis by it. A usage-share prediction and a map prediction err differently, so the factor learned for one never scales the other. The plan records the factor (`effect`), and the ratio is taken against the prediction before scaling, so the effect converges on the workload's real response rather than back to 1. It is the map's first learned slope. It is held in memory and lost on restart, when every workload starts again from 1. An outcome that measures an effect resets the workload's map-basis factor to 1: the relationship's strength now carries what the interventions saw, and a factor learned against the old strength would correct the map's prediction twice.

**Settling candidates.** When interventions create or promote the relationship, a pending discovery candidate on the same pair — directed from the workload's usage to pressure, or undirected in either order, and not negative — is confirmed with `from` = the workload's usage and a review `{by: intervention, tier: 0, action: confirm}` citing the verify decision. This is how undecided candidates settle without an operator, slowly under contention (below). The facade declares no second relationship for a pair that is already covered. A confirmation that fails is a caveat on the verify decision, never an error in the loop.

**What is not here yet.** The thresholds are uncalibrated defaults, not measured, and the learned effects do not survive a restart. Learning is slowest where relief matters most: capping a workload frees CPU that the others absorb, so under real contention the other workloads move and many outcomes are confounded and teach nothing. Undecided candidates therefore settle slowly in exactly the situation relief exists for, and fastest on a node with one dominant workload.

### The Kubernetes runtime (`pkg/runtime/kubernetes`)

The runtime lists the pods scheduled on this node (`spec.nodeName`, the spec's `runtime.node` or else the node ID) and changes one pod's CPU limit in place through the pod `resize` subresource, so the container does not restart. It is built on the in-cluster service account, or the kubeconfig in `runtime.kubeconfig`; the factory error names that spec path.

**Workloads.** Every pod on the node is listed, including those that are not running, so a pod missing from the list is very likely gone; the relief loop still asks the actuator, and its "gone" path fires only on the actuator's `ErrGone`. The runtime remembers, from its latest listing, why each pod's CPU limit cannot be changed; `cpu.limit`'s `Check` answers from it, and a subject that listing did not hold is `not a workload the runtime lists`. The subject is `pod:<metadata.uid>`, so a pod recreated under a new UID is a different subject and the old one is gone. `Name` is the bare pod name; the namespace is the label `k8s/namespace`. The other labels are the pod's own, plus `k8s/priority-class` (`spec.priorityClassName`), `k8s/qos` (`status.qosClass`) and `k8s/owner-kind` (the first owner reference's kind). `resources.cpu` sums the containers' requests and limits in cores, the limit nil if any container has none; `resources.memory` does the same in bytes, its request nil too if any container has none. `UID` is the pod's and `QoS` its class in lower case. `Self` is the pod whose UID is in the `POD_UID` environment variable. In-cluster (empty `runtime.kubeconfig`) the factory refuses to build without it, naming the downward API's `metadata.uid`, because nothing else would stop the agent capping itself; with a kubeconfig it logs that its own pod cannot be recognised.

**The `cpu.limit` actuator.** `Read` is the single container's `limits.cpu` in cores; `Set` resizes it, in whole millicores rounded down and never below 1m, or removes it. Nothing is written to the pod but its limit: what a change replaced is in the agent's ledger (§2). A pod already at the value is not resized, so giving back a change that never took cannot fail on a resize the API keeps refusing. A pod that has been deleted, recreated under a new UID, or has finished (Succeeded or Failed) is `ErrGone` from both. RBAC grants `list` on pods and `patch` on `pods/resize`; the latter is cluster-wide because Kubernetes cannot scope it to a node, and the agent only ever resizes pods on its own node.

**What can be resized, and why.** These rules come from a spike on k0s v1.36.4 (cgroup v2), not from the documentation. Lowering and raising an existing limit is accepted and takes effect within seconds with no restart. A limit set below the request is refused, so the plan never goes below the request. The refusals, in the order they are reported (the Burstable, BestEffort and Guaranteed rows come from the spike):

| `cpu.limit` `Check` | Why |
| --- | --- |
| `the agent itself` | The agent's own pod (`Self`) is never touched. |
| `not running` | Only a running pod has a cgroup to resize; a Succeeded or Failed pod is also `ErrGone` from `Read` and `Set`. |
| `being deleted` | A pod with a deletion timestamp is on its way out. |
| `pod-level resources or sidecars: the agent cannot account for them` | Pod-level `spec.resources` and native sidecars (init containers with `restartPolicy: Always`) take CPU the single-container accounting does not see. |
| `more than one container: the limit cannot be split by the agent` | One pod-level cap cannot be divided between containers without a rule for the division. |
| `BestEffort: a limit would change the pod's QoS class` | Adding a limit to a BestEffort pod is refused by the API (`Pod QOS Class may not change as a result of resizing`). |
| `no CPU limit: a limit the agent added could never be removed` | Adding a limit to a Burstable pod is accepted, but removing it again is refused (`resource limits cannot be removed`), so a revert could not restore "no limit". |
| `Guaranteed: changing the limit alone would change the pod's QoS class` | A Guaranteed pod's limit equals its request; moving only the limit would change the class. |
| `resizePolicy restarts the container for a CPU change` | A container whose CPU `resizePolicy` is `RestartContainer` would restart on every cap and every revert. |

A limit is also refused at `Set` when it would make the pod Guaranteed: a CPU limit equal to the CPU request, on a container whose memory request equals its memory limit. The plan never goes below the request, so it can land on it; the actuator refuses rather than change the QoS class.

Because pods without a limit are refused by `Check`, every entry `cpu.limit` makes has an original limit, and a revert never asks the API to remove one.


## Groups: what belongs to what

Subjects keep flat, stable ids — `pod:<uid>`, `unit:ollama.service`, `net:enp0s1` — because
decisions, relationships and counterfactuals refer to them, and a regrouping must not change what
an earlier decision read. What a person needs, and what an agent asking "what if the storage pods
left" needs, is structure over those ids, recomputed from what the node runs now (`pkg/groups`,
every `groups.interval`).

**Groups from what an operator already says.** A pod's group is the first set label of
`groups.label_keys` (`di-agent/group`, then `app.kubernetes.io/part-of`); a `kube-system` pod with
none is in `kube-system`; a unit's group is the first whose `groups.units` glob matches. CPU cores,
disks and NICs with a device are `hardware`; virtual interfaces are `network`, except a host-side
`veth`, which is its pod's: its `iflink` is the peer's index in the pod's namespace, and the pod's
`/proc/<pid>/net/igmp` lists `eth0` under it. A container is its pod's by the collector's `parent`
label. `GET /groups` serves the whole topology.

**A group of consumers is a subject.** A group whose members use the machine (pods, units) is
`group:<name>` in the map, with `cpu_utilization@group:<name>` and `memory_utilization@group:<name>`:
derived properties that *sum* their members (`Aggregate: sum`; other derived properties are a mean),
recomputed from them and never stored, and the one derived property scoped to a subject, since a
group is the one subject made of others. Pressures and window ratios do not add up, so a group has
none. Membership moves with labels, journalled as a redeclaration; a group nothing belongs to any
more is retired, so its sum stops speaking for workloads that left. `without=group:<name>` in an
estimate takes every member away, so "what would node memory be without the storage group" is one
question. `hardware` and `network` are structure only: their members have no CPU or memory of
their own.
