# Semantic Map — Architecture

Design rationale and decision record. Update this file when a contract, profile, MetricType, or structural decision changes. For usage (running, API, compliance), see [README.md](README.md).

---

## Table of Contents

- [1. Core Concept](#1-core-concept)
  - [The agent at a glance](#the-agent-at-a-glance)
  - [Component reference](#component-reference)
  - [The four request lifecycles](#the-four-request-lifecycles)
- [2. Contract Architecture](#2-contract-architecture)
  - [The five contracts](#the-five-contracts)
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
loads it via `-domain` and refuses to start without one. Two consequences: the
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

Two mechanisms enforce this. `-node-id` is the agent's identity, not merely the label
it stamps on its own samples, and `-ingest-scope=self` (the default) rejects samples
belonging to another machine with a distinguishable error. `GET /cost?node=X` answers
409 when X is not this agent, naming that machine's own URL if it is a known peer,
because returning local numbers under a peer's name is a fabrication — and ignoring
the parameter, which is what the route did before, was exactly that. Replaying a
whole testbed into one daemon is legitimate and available via `-ingest-scope=any`,
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
| **Collector** | `internal/minimal` | Read raw metrics from cgroup / Netdata / parquet; emit typed `MetricSample`s | Collection loop tick (`-collect-interval`) | Optional — `POST /ingest-sample` works without one |
| **Routing** | `domain_spec.json` | Say which construct summarises a metric — data, not code | Every sample, to tell the Proposer which construct moved | Yes (a table in the spec) |
| **State model** | `pkg/statemap` | Hold the properties the system exhibits and the relationships between them; recompute what derives; fold paired observations into learned strengths; journal every change and decision | Every sample, and every answer | Not a contract — one implementation, and the agent's single model |
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
                eff, known := rel.Effective()   ← assertion | established | recent
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
  Metric source          Semantic Map
  (cgroup / Netdata)
        │
   [Collector] ──samples──▶ [state model: properties, relationships]
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
```

The Collector lives outside the SemanticMap facade — it feeds it. The three components below the facade are *consumers*: they read model state and expose it, but only the facade's own mutation methods can change it.

### What is deliberately not a contract

The contract set has stayed at six since the first release. Three substantial components sit outside it on purpose:

| Component | Why it is concrete, not a contract |
|---|---|
| **State model** (`pkg/statemap`) | One implementation, and it is the agent's model rather than a pluggable store. A contract here would invite a second implementation, which is the arrangement §2 just finished removing. |
| **Peers** (`pkg/peers`) | One implementation exists. Promoting it to a contract before a second one (SQLite-backed registry, gossip discovery) would be designing an interface against a sample size of one. |
| **Explain** (`pkg/explain`) | Same reasoning, plus: it is an operator convenience, not part of the agent's decision path. A contract would imply the daemon depends on it. It does not — the default is `-explain-provider=none`. |

The rule we hold to: **no new contract without a second implementation that needs it.** Interfaces derived from one example encode that example's accidents. Each of these three gets promoted the day a real second implementation arrives, and not before.

### The five contracts

| Contract      | Responsibility                                              | Key guarantees                                                                                            |
| ------------- | ----------------------------------------------------------- | --------------------------------------------------------------------------------------------------------- |
| **Collector** | Read raw metrics from a source; emit normalized samples     | Pure read; deterministic `event_id`; `available_metrics()` is static; never raises on empty data         |
| **Ontology**  | The declaration layer — which constructs exist, which propositions relate them, whether a proposed one is valid | Returns whatever the loaded specification declares, with every proposition endpoint a declared construct; constructs and propositions are append-only, never removed or direction-reversed. Holds **no** strength and **no** history: a proposition's magnitude is its relationship's prior and its withdrawal is that relationship's retirement, both in the state model, and the journal is the one audit record |
| **Reasoner**  | Produce agent decisions with traceable rationales           | Every result includes a non-empty rationale referencing the properties and relationships read; `SimulateOutcome` is pure (read-only) |
| **Proposer**  | Detect statistical patterns suggesting new backbone claims  | Never modifies the model or the Ontology directly; `Reject` permanently suppresses within session          |
| **Tuner**     | Map natural-language operator intent to proposition strength adjustments | Parses intent; resolves current magnitudes; clamps to `[floor, 0.95]` with a raised floor on SC-adjacent propositions; emits one `operator-tune` audit event per invocation. See §11 |

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
projection". `POST /ingest`, which named a construct pair and a magnitude directly, went
with them: an observation is of a property, and a single number about a pair is an
assertion rather than a measurement.

### Behavioral guarantees

Guarantees are not just signatures — they are documented pre/post-conditions on each method in the contract source files. The compliance test suites in `go/compliance/` verify them mechanically. **A new implementation is valid if and only if it passes the compliance suite for its contract.** This is the definition, not a check.

Compliance suites exist for all five contracts (`compliance/{collector,ontology,reasoner,proposer,tuner}.go`). Each runs against a factory the implementation supplies, so a new ontology or collector can be validated with a single test file wired to the suite.

### End-to-end validation: integration scenarios

Compliance proves each part works in isolation. **Scenarios prove the parts compose** into the behaviors the architecture promises. `internal/minimal/tests/scenarios_test.go` runs seven narrated end-to-end flows against the same wiring the production daemon uses; each emits `t.Logf` snapshots so `go test -v -run TestScenario` reads like a paper results section while hard assertions guard the mechanics that must not regress:

| Scenario                            | Demonstrates                                                                                          |
| ----------------------------------- | ----------------------------------------------------------------------------------------------------- |
| `ColdStart`                                    | every declared relationship present with confidence=0 and **no effective value at all** — `Effective()` reports not-known, so the agent says it does not know rather than reporting a constant |
| `ConvergenceOnOneEdge`                         | observations at a fixed value: the recent layer converges, the established layer follows more slowly, confidence climbs 0→1 |
| `PerKDAgentsAreIdenticalUntilTheyObserve`      | two agents built with different `-kd` produce **identical** cost answers until telemetry arrives — the inversion of the old expectation, and the point: nothing distinguishes them but what they measure |
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
passed `-proposer false` and Go's flag package silently dropped every flag after it,
including `-priors` and `-kd`.

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

The audit trail (`GetHistory(since)` on the facade, `GET /history`) lets the agent answer "why is this strength what it is?" at any point in time. It is a projection of the state model's journal — there is one record, and it holds more than the ontology vocabulary can name, so a property admitted or a decision taken appears there too. It survives a restart when a state file is configured (§5, Persistence), and is bounded: `GET /state/journal` reports how many entries were dropped, which is what a caller needs before reading an absence as evidence.

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
| `value`          | `float`      | Normalized value in the fixed unit for the metric type   |
| `timestamp_unix` | `int`        | Unix timestamp of the observation                        |
| `event_id`       | `str`        | Deterministic ID — same observation always → same ID     |
| `container_id`   | `str`        | Empty for node-level aggregates; set for per-container   |
| `labels`         | `dict`       | Source metadata (cgroup path, Netdata chart, …); opaque  |

**`event_id` determinism** is the collector's responsibility. A stable recipe: `sha256(source_id + node_id + container_id + metric_type + str(timestamp_unix))[:16]`. This carries the map's idempotency guarantee end-to-end: replaying the same telemetry batch has no effect on the model, because the map recognises an observation it has already applied.

**`available_metrics()` is static** — declared once at construction, never changes within a deployment session. It says what this collector can report without calling `collect()` first.

### MetricType catalogue

Routing is declared in `domain_spec.json`, not in code, and every route fixes its
unit as a fraction on [0,1]. Collectors must normalize before emitting: edge
weights are Bernoulli parameters and the divergence measure is defined on that
interval, so an out-of-range observation is clipped and the affected edge stops
responding to evidence while aggregates keep tracing a smooth curve. A replay
harness that passed network throughput in raw bytes per second produced exactly
that failure, silently.

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
| `cpu_pressure_ratio`    | PS        | worse    | PSI `cpu.some` stall fraction                              |
| `io_pressure_ratio`     | PS        | worse    | PSI `io.some` stall fraction                               |

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
knowledge, `learned` from observing this system, `asserted` by an operator. An agent
that cannot say why it believes an edge cannot be audited, so provenance is data.

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

**The map survives the process.** `-state-file` persists properties, relationships and
the journal; the write is to a temporary file and renamed, because a half-written
snapshot read back on the next start would look like knowledge. Two things follow from
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
observations as this one's history at full confidence.

#### The three paces

The map changes at three rates, and each is a setting rather than an accident.

| Pace | Appears | Disappears |
|---|---|---|
| **Property** | one observation, at confidence 0 | stale after `-stale-after` (2 m) → retired after `-retire-after` (10 m), cascading to incident relationships |
| **Magnitude** | `-alpha` (0.2) for the recent layer, `-alpha-slow` (0.001) for the established layer | — |
| **Structure** | `-proposer-min-pairs` (30) co-observations inside `-pair-window-seconds` before a candidate can exist; an operator confirms | cascade on endpoint retirement |

Admission is immediate and *trust* grows at the magnitude pace: confidence is the pace signal, so there is no probation gate. A derived property never retires — it is declared structure — but it goes stale when no member is active and returns with them.

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
exist into the cluster view. And a snapshot older than `-peer-state-stale` is labelled
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

### What ingestion does

`IngestSample` records the sample against the metric's own property, and everything else
follows from the model: derived properties recompute from their members, and the estimator
folds the observation into the relationships incident to whatever moved. The routed
construct is passed to the Proposer, which needs construct-level values to look for
relations the backbone does not declare.

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
| `CgroupCollector`   | `/sys/fs/cgroup/`                | `edge-minimal`          | ✅ done — `internal/minimal/collector_cgroup.go` | cpu\_utilization, memory\_utilization, cpu\_throttle\_ratio |
| `ScriptedCollector` | programmable patterns (in-process) | demo / scenarios / replay | ✅ done — `internal/scripted/collector.go`     | any MetricType the patterns declare (Constant / Ramp / Step / Sine / Burst / Noisy) |
| `ParquetReplay`     | Netdata parquet datasets (out-of-process HTTP) | dissertation reproducibility | ✅ done — `cmd/replay/`               | cpu\_utilization, memory\_utilization, network\_rx\_bps, network\_tx\_bps           |
| `KubeletCollector`  | kubelet `/metrics/resource`      | `edge-standard`         | planned | pod\_startup\_ms, scheduling\_latency\_ms                            |
| `NetdataCollector`  | Netdata HTTP streaming API       | `edge-minimal` + `cloud-full` | ✅ done — `internal/minimal/collector_netdata.go` | cpu\_utilization, memory\_utilization, network\_rx\_bps, network\_tx\_bps |

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

Ingestion is `SemanticMap.IngestSample`, which records the sample in the state model and passes the routed construct to the Proposer. The autonomous scheduler that ticks the configured collector lives in `go/cmd/agent/main.go::runCollectionLoop`; it is started by `startCollectionLoop` once the daemon has built its profile. Both pieces are profile-agnostic — adding a new collector means returning it from a profile build function, no changes to the loop or to ingestion.

---

## 6. Automatic Graph Extension

The Proposer contract supports discovering relationships the loaded specification does not declare. The flow is **propose-then-confirm** — patterns are detected automatically, but a human confirms before the backbone is modified.

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

1. Create `go/internal/<profile-name>/` and implement all five contracts, or reuse existing implementations.
2. Every implementation must pass its contract's compliance suite before being wired into a profile.
3. Add a case to `go/pkg/profiles/profiles.go`:

```go
case "my-profile":
    collector := myprofile.NewMyCollector(...)
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
    return sm, collector, nil
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

Two endpoint families coexist on the same mux. The four surviving pre-Phase-1 endpoints (`/cost`, `/recommend`, `/simulate`, `/candidates`) keep their original `http.Error` plain-text error format to minimize diff against the v0 daemon. The fifth, `/ingest`, is gone: it named a construct pair and a magnitude directly, which is an assertion about a relation rather than an observation of one. Every endpoint added in the Phase 1 expansion emits structured JSON errors and gates mutations on `Content-Type: application/json`.

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
| POST | `/ontology/strength`              | `SetStrengthRequest`                                                   | `204 No Content`              | Assert a strength for one proposition; writes `Assertion` on its relationships, outranking both learned layers; audit-logged |
| POST | `/ontology/deprecate`             | `DeprecateRequest`                                                     | `204 No Content`              | Soft-delete a proposition (the relationship is retired: out of the traversal, kept for audit)          |
| POST | `/ontology/construct`             | `AddConstructRequest`                                                  | `204 No Content`              | Append a new construct (append-only; constructs are domain-stable)                                     |
| POST | `/ontology/proposition`           | `AddPropositionRequest` (`direction` is `"+"` or `"-"`)                | `204 No Content`              | Add a validated proposition; `ValidateProposition` rejects direction contradictions                    |
| POST | `/agent/reset`                    | `ResetRequest`                                                         | `204 No Content`              | Discard what a `(from, to)` pair learned: both layers cleared, confidence to zero, pair window emptied, discard count journalled. Keeps the claim and any assertion; does not delete the relationship |
| POST | `/candidates/{id}/confirm`        | path only                                                              | `204 No Content`              | Promote a proposer candidate to a validated proposition                                                |
| POST | `/candidates/{id}/reject`         | path only                                                              | `204 No Content`              | Permanently suppress a candidate within the session                                                    |
| POST | `/candidates/{id}/defer`          | path only                                                              | `204 No Content`              | Keep the candidate pending; re-surface on next review                                                  |
| GET  | `/ui/...`                         | —                                                                      | static assets                 | Embedded HTML/JS/CSS for the viewer; served by `http.FileServer` over an `embed.FS` sub-tree            |

Errors on the new endpoints follow a single shape:

```json
{"error": "Content-Type must be application/json"}
```

`writeError` (in `cmd/agent/routes.go`) is the only path to a non-2xx response. The four surviving pre-Phase-1 endpoints retain `http.Error`'s plain-text body for backward compatibility.

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
| `edges --from --to`                 | `GET /edges`                                | Per-relationship basis, effective, established, assertion and recent; the multigraph fan-out when a specification declares two claims over one pair |
| `history --since`                   | `GET /history`                              | RFC3339 or duration                                         |
| `strength <id> <value>`             | `POST /ontology/strength`                   | Assert a strength on one proposition                        |
| `deprecate <id> <reason>`           | `POST /ontology/deprecate`                  | Soft-delete                                                 |
| `construct add <id> <name> <desc>`  | `POST /ontology/construct`                  |                                                             |
| `proposition add <id> <f> <t> ±<s>` | `POST /ontology/proposition`                |                                                             |
| `reset <from> <to>`                 | `POST /agent/reset`                         | Discard the evidence, keep the claim                        |
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

This section covers the registry and the trust mechanics that decide *where work goes*. What peers know about their own systems — and why it is held apart from local state rather than merged into it — is [§5, Peer state](#peer-state-knowledge-crossing-a-node-boundary).

### The peer registry — concrete, not a contract

In v1 the peer registry lives at `pkg/peers/` as a **concrete package**, not a contract. The contract surface stays at five (Collector, Ontology, Reasoner, Proposer, Tuner) — broadening it would force every profile to re-implement a peer table that the edge-minimal profile already provides for free. We promote to a contract when a second implementation arrives:

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

The system prompt lives at [`cmd/agent/prompts/explain-v1.md`](go/cmd/agent/prompts/explain-v1.md), loaded once at daemon startup. Every response records a `prompt_version` field (the first 12 hex chars of `sha256(prompt)`), so a paper's replication package can pin the exact prompt used for reported results. Bump the filename (v2, v3, …) rather than editing v1 in place — old snapshots then remain reproducible.

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

- **The reasoner, the state model, and the prior-init pipeline remain pure deterministic Go.** No LLM touches the ingestion or reasoning path.
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
