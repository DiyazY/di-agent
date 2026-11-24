# di-agent: subtasks and diagrams

This page splits di-agent into six subtasks and draws each one: what runs, what it reads, what it writes, and how a scenario unfolds. Every diagram was drawn from the code, not from the prose elsewhere in the repository; file references are relative to `semantic-map/go/`. When the code changes a flow drawn here, change the diagram in the same commit.

The design record — why things are the way they are — stays in [`semantic-map/ARCHITECTURE.md`](../semantic-map/ARCHITECTURE.md). This page is the map of the territory; that one is the reasoning.

## Contents

- [1. The six subtasks](#1-the-six-subtasks)
- [2. The whole agent](#2-the-whole-agent) — context, components, loops
- [3. Self-knowledge: the Semantic Map](#3-self-knowledge-the-semantic-map)
- [4. Local adaptation: relief](#4-local-adaptation-relief)
- [5. Task offloading: admission](#5-task-offloading-admission)
- [6. Networking and coordination](#6-networking-and-coordination)
- [7. Trust and security](#7-trust-and-security)
- [8. Autonomy and governance](#8-autonomy-and-governance)
- [9. Deployments](#9-deployments)

---

## 1. The six subtasks

di-agent is one daemon per machine. It models its own system and acts on that model. The work divides into six subtasks, each answering one question:

| # | Subtask | The question it answers | Packages |
|---|---|---|---|
| S1 | **Self-knowledge** | What is this node, what runs on it, and how do its properties relate? | `statemap`, `semmap`, `collectors`, `facts`, `runtime`, `groups`, `internal/minimal` (proposer) |
| S2 | **Local adaptation** | Can I relieve pressure here, without moving work? | `relief`, `actuators`, `policy` |
| S3 | **Task offloading** | Should I accept this work — and if not, who should? | `admission`, `agentmain/admission.go` |
| S4 | **Networking and coordination** | How do nodes find each other and learn what the others know? | `peers`, `statemap` (`PeerStore`), `agentmain/peer_state_routes.go` |
| S5 | **Trust and security** | Whose claims do I believe, and who may call me? | `peers` (trust), `internal/minimal/reasoner.go`, `agentmain/routes.go` (`requireJSON`, foreign-sample check) |
| S6 | **Autonomy and governance** | Who decides, within what bound, and can every decision be traced and explained? | `autonomy`, `explain`, `statemap` (journal, decisions, calibration) |

**S1 is the hub.** Every other subtask reads the map, and S2, S3 and S6 write their decisions back into its journal.

```mermaid
flowchart LR
    S1(["S1 Self-knowledge<br/>the Semantic Map"])
    S2["S2 Local adaptation<br/>relief"]
    S3["S3 Task offloading<br/>admission"]
    S4["S4 Networking<br/>peers, peer state"]
    S5["S5 Trust and security"]
    S6["S6 Autonomy and governance"]

    S1 -->|pressure, usage, estimates| S2
    S2 -->|interventions, effects| S1
    S2 -->|relief domains and phase| S3
    S1 -->|predictions, calibration| S3
    S3 -->|forwards offers| S4
    S4 -->|peer cost, peer state| S1
    S5 -.->|gates which peers count| S4
    S5 -.->|gates who may call| S3
    S6 -->|autonomy: confirms or rejects discovered relationships| S1
    S6 -.->|explain: reads, never writes| S1
```

### What each subtask is, and what is open

**S1 · Self-knowledge.** Collectors produce typed samples; the state model admits any property it is sent, keeps an EMA and confidence per property, recomputes derived (construct) properties from their members, and holds per-workload properties as `metric_type@subject`. Silence makes a property stale and then retired; an observation revives it. The proposer pairs properties and offers candidate relationships; confirmed candidates become relationships with `Discovered` provenance. *Open:* the routing table and the construct vocabulary are still seeded from a domain spec.

**S2 · Local adaptation.** One relief loop per lever. Two levers exist: `memory.high` (cgroup v2) and `cpu.limit` (Kubernetes in-place resize). A loop enters *relieving* when its contention ratio stays above `Enter`, steps a cap down on the workload the map attributes most pressure to, verifies the effect after `Settle`, keeps or reverts it, and learns the effect. *Open:* no CPU-weight lever; relief is not gated by the autonomy reviewer — only `policy` rules and per-workload limits bound it.

**S3 · Task offloading.** A submitter offers a workload by name and demand. Each node answers from its own map, predicting the pressure each relief domain would reach; a node that declines forwards the offer to the next peer. Once the workload arrives, the node checks its own prediction and feeds the error into calibration. *Open:* the walk is in peer-id order and ignores trust; the agent never starts the workload itself.

**S4 · Networking and coordination.** A static registry (`peers.urls`, or `POST /peers`) of peer URLs. Agents query each other's `/cost`, forward `/offers`, and poll each other's `/state` into a `PeerStore` that is held apart from local state and never merged. *Open:* no discovery (mDNS/gossip is planned only); plain HTTP; one registry implementation, not yet a contract.

**S5 · Trust and security.** Trust is a number per peer in `[0, 1]`, moved by reachability (+0.05 when a peer answers `/cost`, −0.05 when the query fails other than by a 4xx) and by an operator. `RecommendPeer` ranks by `savings × trust` and skips peers under `min_trust`. Security is a scope decision: **no authentication on any endpoint**; the defences are a JSON content-type check against browser CSRF, node-local ingest (foreign samples rejected) and owner checks on peer state. *Open:* outcome-driven trust (accepted/declined offers, broken predictions) is not wired; admission does not read trust; no TLS, no peer identity, no authorisation.

**S6 · Autonomy and governance.** The autonomy reviewer decides on the proposer's candidates in tiers — rules, then a decision model, then a tool-using reasoner — bounded by `loops.review.level`. Every estimate, relief step, admission and review is a journalled decision whose inputs can be read back. `/explain` answers questions about the map through read-only tools, checked by a validator and a critic. *Open:* the reviewer governs discovery only, not actions; there is no AgentOps view across nodes.

---

## 2. The whole agent

### Context

```mermaid
flowchart LR
    op(["Operator or script<br/>mapctl, curl, /ui/"])
    sub(["Submitter<br/>any client calling POST /tasks"])
    llm(["LLM endpoint<br/>OpenAI-compatible or Ollama"])

    subgraph node["One machine"]
        agent["di-agent<br/>:8080 or :9090"]
        kern[("Kernel<br/>cgroup v2, /proc")]
        rt[("Container runtime<br/>kubelet API")]
        nd[("Netdata :19999<br/>optional")]
        disk[("persist.file<br/>snapshot + journal log")]
    end

    peers(["Peer agents<br/>one per machine"])

    op -->|HTTP API| agent
    sub -->|offer| agent
    agent -->|read metrics| kern
    agent -->|read metrics| nd
    agent -->|list workloads, resize cpu.limit| rt
    agent -->|write memory.high| kern
    agent <-->|/cost /offers /state /healthz| peers
    agent -->|review and explain prompts| llm
    agent --> disk
```

### Components

Grouped by subtask. Solid arrows are calls on the hot path; dotted arrows are reads for a decision.

```mermaid
flowchart TB
    subgraph S1["S1 Self-knowledge"]
        col["collectors.Supervisor<br/>cgroup, netdata, plugins"]
        facts["facts<br/>CPU, memory, OS"]
        rtinv["runtime<br/>workload inventory"]
        grp["groups<br/>what belongs to what"]
        sm["semmap.SemanticMap<br/>facade"]
        state[("statemap.Map<br/>properties, relationships,<br/>journal, decisions, calibration")]
        prop["MI proposer<br/>internal/minimal"]
        reas["Reasoner, Ontology, Tuner<br/>internal/minimal"]
    end

    subgraph S2["S2 Local adaptation"]
        relief["relief.Loop x lever<br/>shared Budget"]
        pol["policy"]
        act["actuators.Acting<br/>ledger"]
    end

    subgraph S3["S3 Offloading"]
        adm["admission<br/>Decide, Tracker"]
    end

    subgraph S45["S4 Networking / S5 Trust"]
        reg["peers.Registry<br/>trust"]
        cli["peers.Client"]
        pstore[("statemap.PeerStore")]
    end

    subgraph S6["S6 Autonomy and governance"]
        rev["autonomy.Reviewer<br/>rules, decider, reasoner"]
        exp["explain<br/>planner, answer, critic"]
    end

    api["HTTP API<br/>agentmain"]

    col --> sm --> state
    facts --> state
    sm --> prop
    sm --- reas
    reas -.-> state
    reas --> reg
    reas --> cli
    rtinv -.-> relief
    grp -.-> state
    relief -.-> state
    relief --> pol
    relief --> act
    adm -.-> relief
    adm -.-> state
    adm --> cli
    adm --> reg
    cli --> pstore
    rev -.-> prop
    rev --> sm
    exp -.-> sm
    api --> sm
    api --> relief
    api --> adm
    api --> rev
    api --> exp
```

### The loops that run

All started from `agentmain.Main` (`pkg/agentmain/main.go`). "Paced" loops wait four times longer when the pace monitor reports the agent slow (`pkg/agentmain/pace.go`).

| Loop | Subtask | Default tick | Enabled by |
|---|---|---|---|
| Collection: `Collect` → `IngestSample` | S1 | 10 s, paced | `sources.collectors` (default `cgroup`), `sources.interval` |
| Lifecycle sweep: stale and retire | S1 | `stale_after/2`, at least 5 s | always; `map.sweep_interval` overrides the tick |
| Groups | S1 | 30 s, paced | `groups.interval` (0 turns it off) |
| Requests refresh | S1 | 30 s | a non-dry-run runtime |
| Relief, one per lever | S2 | 15 s | `loops.relief.enabled` |
| Admission tracker | S3 | relief interval | always built |
| Peer state poll | S4 | 30 s | `peers.state_poll` (0 turns it off) |
| Review | S6 | 1 min, paced | `loops.review.level` ≠ `off` (default `full`) |
| Pace monitor | — | 1 min | always |
| Save | — | 1 min, paced | `persist.file` |

---

## 3. Self-knowledge: the Semantic Map

### Telemetry into the map

```mermaid
sequenceDiagram
    autonumber
    participant C as Collector
    participant L as collection loop
    participant F as semmap facade
    participant M as statemap.Map
    participant P as MI proposer

    L->>C: Collect()
    C-->>L: []MetricSample (node, metric_type, subject, value, event_id)
    loop each sample
        L->>F: IngestSample(s)
        alt sample names another node
            F-->>L: ErrForeignSample (ingest-scope=self)
        end
        Note over F: subject set: id = metric_type@subject<br/>else: polarity normalised for its construct
        F->>M: RecordAccepted(observation)
        Note over M: reject future, out-of-order, duplicate event_id<br/>admit an unknown property and journal it<br/>EMA, n_obs, confidence<br/>recompute derived (construct) parents<br/>learn on incident relationships
        M-->>F: accepted
        F->>P: ObserveProperty(reading)
    end
    L->>M: SetSourceDown per collector, up or down
```

The same path serves `POST /ingest-sample`, so a sample sent over HTTP is treated exactly like a collected one.

### A property's life

```mermaid
stateDiagram-v2
    [*] --> active: first observation admits it
    active --> active: observation updates EMA
    active --> stale: silent longer than stale_after
    stale --> active: observation revives it
    stale --> retired: silent longer than retire_after
    active --> retired: silent longer than retire_after at one sweep
    active --> unobservable: its collector is down
    stale --> unobservable: its collector is down
    unobservable --> active: observation
    unobservable --> stale: silent past stale_after, counted from the collector's return
    unobservable --> retired: silent past retire_after, counted from the collector's return
    retired --> active: observation revives it
    note right of unobservable
        Silence with a known cause
        is not evidence of departure.
    end note
    note right of retired
        Soft: earlier decisions still resolve.
        Retiring cascades to its relationships
        and the proposer forgets its pairs.
    end note
```

### Discovery: a candidate becomes a relationship

Two paths confirm a candidate — the operator over HTTP, or the autonomy reviewer ([§8](#8-autonomy-and-governance)). A relief intervention can also confirm one as reviewer `intervention` ([§4](#4-local-adaptation-relief)).

```mermaid
sequenceDiagram
    autonumber
    participant P as MI proposer
    participant O as Operator
    participant R as autonomy.Reviewer
    participant F as semmap facade
    participant M as statemap.Map

    Note over P: pairs subject to node, node to node,<br/>subject to subject within the pair budget<br/>min spread, BH false-discovery bar
    P-->>F: PendingCandidates
    alt operator
        O->>F: POST /candidates/{id}/confirm {from}
    else reviewer
        R->>F: ConfirmCandidate(id, from, review)
    end
    F->>P: Confirm(id)
    F->>M: Covered(pair)?
    F->>M: DeclareRelationship or DeclareComposition<br/>provenance Discovered
    alt declaration fails
        F->>P: Reopen(id)
    else
        F->>P: Applied(id)
    end
```

---

## 4. Local adaptation: relief

### Lever phase

One per relief loop (`pkg/relief/trigger.go`). Defaults: `cpu.limit` enters above contention 0.25 and exits below 0.10; `memory.high` enters above 0.08 and exits below 0.03. `EnterFor` 1 min, `ExitFor` 3 min. A reading that is not fresh holds the phase and breaks any run in progress.

```mermaid
stateDiagram-v2
    [*] --> calm
    calm --> relieving: pressure above Enter for EnterFor
    relieving --> releasing: pressure below Exit for ExitFor
    releasing --> relieving: pressure above Enter for EnterFor
    releasing --> calm: nothing in flight and no caps kept
```

### One action

```mermaid
stateDiagram-v2
    [*] --> in_flight: Apply cap
    in_flight --> not_applied: still reads as before after Settle
    in_flight --> kept: observed drop at least half the prediction
    in_flight --> reverted: first step, smaller drop, undone, cooldown x4
    in_flight --> kept: further step, smaller drop, earlier cap restored
    in_flight --> in_flight: undo refused, journalled "stuck", retried each tick
    in_flight --> released: workload no longer listed
    kept --> in_flight: further step on the same workload
    kept --> expired: expiry (1 h) passed, given back
    kept --> floored: floor rose above the cap, given back
    kept --> manual: operator revert
    kept --> gone: workload left
    reverted --> [*]
    not_applied --> [*]
    released --> [*]
    expired --> [*]
    floored --> [*]
    manual --> [*]
    gone --> [*]
```

A refused give-back of a kept cap (at expiry, floor or a manual revert) is journalled `stuck` the same way and retried each tick; `stuck` is a decision, not an outcome.

### A relief episode

```mermaid
sequenceDiagram
    autonumber
    participant T as relief.Loop tick
    participant M as statemap.Map
    participant Pol as policy
    participant B as Budget
    participant A as actuators.Acting
    participant K as cgroup or kubelet

    T->>M: read Relieves (contention ratio), fresh?
    Note over T: Observe: calm to relieving, journalled
    T->>M: usage@subject shares, capacity fact
    T->>Pol: For(workload).Allows(action)?
    Note over T: Attribute: rank by map estimate<br/>(Estimate without subject), else by usage share<br/>makePlan: step 25 pct, respect floors and request,<br/>drop if predicted effect below MinEffect
    T->>B: reserve slot (cools other levers off it)
    T->>M: Decide relief.act (cites the estimate)
    T->>A: Apply(plan)
    A->>A: ledger: write-ahead Pending entry
    A->>K: Set cap
    A->>K: read back within 10 s
    A-->>T: committed
    T->>M: Commit decision
    Note over T: wait Settle (2 min)
    T->>M: fresh reading after Settle
    T->>M: learn: RecordIntervention, effect EMA,<br/>RecordPrediction, confirm candidate as intervention
    T->>M: Decide relief.verify (kept or reverted)
    opt reverted
        T->>A: Revert or Restore previous cap
    end
    Note over T: pressure below Exit for ExitFor: releasing<br/>caps stay until expiry, floor or manual revert<br/>then releasing to calm
```

On restart, `Acting.Held` reads what the ledger holds and `reconcile` sorts the held entries by expiry: the latest-expiring one resumes as in flight (when nothing already is) and the rest as kept, while expired entries are taken as kept so that `expire` gives them back. Nothing new is applied until reconcile succeeds. `POST /relief/pause` stops new actions on every lever while the loops keep observing, verifying and expiring.

---

## 5. Task offloading: admission

There is no broker binary. Whichever agent receives `POST /tasks` is the submitter: it answers first, then walks its peers.

### An offer walks the chain

```mermaid
sequenceDiagram
    autonumber
    participant S as Submitter
    participant A as Agent A (receives /tasks)
    participant MA as A map and relief
    participant B as Agent B
    participant C as Agent C

    S->>A: POST /tasks Offer{name, arch, demand cpu, memory}
    A->>A: strict decode, Validate
    A->>MA: decide(offer)
    Note over A,MA: already_asked if already in the chain<br/>decline if already pending, workload list unavailable,<br/>already running, arch mismatch, no relief domains,<br/>headroom gate fails, any domain not calm,<br/>or predicted pressure above Exit
    MA-->>A: Hop{A, decline, decision_id, reason}
    A->>B: POST /offers Offer + chain[A]
    B->>B: same decide on its own map
    B-->>A: Hop{B, decline, decision_id}
    A->>C: POST /offers Offer + chain[A, B]
    C->>C: decide, tracker.Accepted
    C-->>A: Hop{C, accept, decision_id}
    A-->>S: {accepted_by: C, decision_id, chain[A, B, C]}
    Note over S,C: the walk stops at the first accept<br/>a dial error appends unreachable and continues<br/>a timeout appends unreachable and stops (it may have arrived)<br/>the submitter starts the workload on C, the agent never does
```

### How one node decides

```mermaid
flowchart TB
    o[Offer] --> g0{"this node already<br/>in the chain?"}
    g0 -->|yes| asked[already_asked]
    g0 -->|no| g1{"name pending, workload list<br/>unavailable, or already running?"}
    g1 -->|yes| dec[decline]
    g1 -->|no| g2{"arch matches,<br/>relief domains exist,<br/>demand per domain?"}
    g2 -->|no| dec
    g2 -->|yes| gate["headroom gate (Admit),<br/>always evaluated and cited"]
    gate --> g3{"every domain calm<br/>and the gate accepts?"}
    g3 -->|no| dec
    g3 -->|yes| p["per domain predict<br/>map: pressure + slope x share<br/>usage: pressure x (usage+share)/usage<br/>basis: map until both have 5 outcomes,<br/>then the lower calibration error"]
    p --> g4{"every prediction<br/>at or below Exit?"}
    g4 -->|no| dec
    g4 -->|yes| acc[accept, register pending]
```

### Checking the prediction afterwards

```mermaid
sequenceDiagram
    autonumber
    participant Tr as admission.Tracker tick
    participant R as runtime
    participant M as statemap.Map

    loop until arrival or 5 min
        Tr->>R: Workloads() includes name?
        Tr->>M: usage@name reported on every domain?
    end
    alt never arrived
        Tr->>M: Commit admission.outcome not_arrived
    else arrived
        Tr->>M: journal EventAdmissionArrived
        Note over Tr: wait max(Settle)
        Tr->>M: judge
        Note over Tr,M: relief acted: breached, not scored<br/>another workload moved, workload silent,<br/>or stale reading: confounded<br/>else above Enter: breached, else held
        Tr->>M: RecordPrediction for each basis predicted (map, usage)
        Tr->>M: Commit admission.outcome, linked to the accept
    end
```

---

## 6. Networking and coordination

### What crosses between agents

Every agent exposes the same surface; there is no coordinator role in the agent.

| Call | Direction | Used by | Effect on the caller |
|---|---|---|---|
| `GET /cost?task=` | A to B | `RecommendPeer` (S5) | B answers for itself; reward B, or penalise it unless B answered 4xx |
| `POST /offers` | A to B | admission walk (S3) | appends B's hop to the chain |
| `GET /state` | A to B | peer state poll | stored in `PeerStore` under the node id B reports for itself |
| `GET /healthz` | any | probes | none |

### Peer state polling

```mermaid
sequenceDiagram
    autonumber
    participant Pl as peer-state poll (30 s)
    participant Reg as peers.Registry
    participant B as Peer B
    participant PS as PeerStore
    participant M as statemap journal

    Pl->>Reg: List() (peers added at runtime included)
    loop each peer
        Pl->>B: GET /state
        alt transport error, or a view naming no owner
            Pl->>PS: RecordFailure(B URL)
            Pl->>M: EventPeerUnreachable (on change)
        else view names this node as owner
            Note over Pl: Record refuses it, logged as a refusal
            Pl->>M: EventPeerUnreachable (on change)
        else valid view
            B-->>Pl: StateView{owner: B node id, ...}
            Pl->>PS: Record(view), keyed by the owner it names
            Pl->>Reg: MarkSeen(B)
            opt was unreachable
                Pl->>M: EventPeerRecovered
            end
        end
    end
    Note over PS: held apart, never merged into local state<br/>read via /state/cluster, /state/peers, /state/where
```

### Topology

Peers are a static, operator-built mesh: `peers.urls` at start, or `POST /peers` at runtime. The PoC scripts register every other agent on each node (full mesh) and set trust to 0.8. A peer's id is the first 12 hex digits of `sha256(url)`.

---

## 7. Trust and security

### Trust per peer

`Descriptor.Trust` in `[0, 1]` (`pkg/peers/peers.go`). Registered at 0.5. `TrustStep` is 0.05. The ranking floor is `profile.min_trust` (default 0.5). Trust survives a restart in the loops snapshot.

```mermaid
stateDiagram-v2
    [*] --> eligible: registered, trust 0.5
    eligible --> eligible: /cost succeeds, +0.05
    eligible --> eligible: /cost fails (transport, 5xx, bad body), -0.05, stamp LastPenalised
    eligible --> eligible: /cost answers 4xx, no change
    eligible --> penalised: trust below min_trust after a penalty
    penalised --> reprobed: 10 min since LastPenalised
    reprobed --> eligible: /cost succeeds and trust back at the floor
    reprobed --> penalised: still below the floor, or fails again
    eligible --> operator_excluded: operator sets trust below the floor
    penalised --> operator_excluded: operator sets trust below the floor
    operator_excluded --> eligible: operator sets trust at or above the floor
    note right of operator_excluded
        SetTrust clears LastPenalised,
        so this peer is never re-probed.
    end note
```

### Recommending a peer

`POST /recommend` → `RuleEngineReasoner.RecommendPeer` (`internal/minimal/reasoner.go`).

```mermaid
sequenceDiagram
    autonumber
    participant Cl as Caller
    participant R as Reasoner
    participant Reg as peers.Registry
    participant P as Peer

    Cl->>R: POST /recommend {task, node}
    R->>Reg: List()
    R->>R: local CostOfAction
    loop each peer, 3 s budget overall
        alt trust below floor and not due a re-probe
            R->>R: skip
        else
            R->>P: GET /cost?task= (P answers for itself)
            alt transport error, 5xx or bad body
                R->>Reg: Penalise(P), -0.05
            else 4xx
                R->>R: skip, trust unchanged
            else ok
                R->>Reg: Reward(P), +0.05, MarkSeen
                Note over R: re-read trust, skip if still under the floor<br/>savings = local - peer cost<br/>weighted = savings x trust, skip if savings at or below 0
            end
        end
    end
    R-->>Cl: best weighted peer, rationale<br/>or 502 if a query failed, else 409, with skip counts
```

### Security boundary

```mermaid
flowchart LR
    subgraph lan["Trusted network segment (the deployment assumption)"]
        any(["Anything that can reach the port"])
        browser(["A browser page"])
        peer(["A host posing as a peer"])
        agent["di-agent HTTP API<br/>no TLS, no auth"]
    end

    any -->|GET /state: whole model, journal, decisions| agent
    any -->|POST mutations: ontology, peers, trust, relief, candidates| agent
    browser -.->|cross-origin form POST with a body<br/>blocked: requireJSON content-type| agent
    browser -->|path-only POST: candidate review,<br/>/state/sweep, /state/peers/refresh| agent
    peer -->|GET /state of this agent: disclosed| agent
    agent -->|peer /state without owner, or naming this node| refused["refused"]
    any -.->|sample naming another node<br/>refused: ingest-scope=self| agent
```

What exists today, and what does not:

| Defence | Where | Protects against |
|---|---|---|
| `requireJSON` on every POST that takes a body, `/recommend` and `/simulate` included | `pkg/agentmain/routes.go` | browser CSRF against a daemon on localhost |
| Foreign samples refused | `semmap.IngestSample`, `node.ingest_scope: self` | one node writing another's state |
| Peer state must name an owner, not this node | `peers.Client.State`, `statemap/peer.go` | a peer's view being mistaken for local state |
| Strict offer decoding | `readOffer`, `pkg/agentmain/admission.go` | a misspelt field read as zero demand |
| Snapshot owner check | `statemap/persist.go`, when `persist.file` is loaded | restoring another node's state |
| `policy` freeze rules, self always frozen | `pkg/policy` | relief acting on system or protected workloads |
| **Not covered:** path-only POSTs (`/candidates/{id}/{confirm,reject,defer}` without a body, `/state/sweep`, `/state/peers/refresh`) | `pkg/agentmain` | a forged form post can trigger them; candidate ids are unguessable in practice, and the other two only run early what a loop runs anyway |
| **Missing:** authentication, TLS, peer identity, authorisation | — | everything that can reach the port can read and mutate |
| **Missing:** outcome-driven trust | `Registry.UpdateTrust` exists, nothing calls it on an offer outcome | a peer that accepts and then breaks its prediction keeps its trust |

The no-auth stance is a recorded scope decision for a lab network ([`OPERATING.md`](../OPERATING.md#read-this-first), ARCHITECTURE §10).

---

## 8. Autonomy and governance

### Review levels

`loops.review.level` (default `full`; the PoC2 chart sets `rules`).

| Level | Applies |
|---|---|
| `off` | no reviewer built |
| `suggest` | nothing; annotates candidates |
| `rules` | tier 0 rules only |
| `bounded` | everything except a model reject |
| `full` | everything |

### A review cycle

```mermaid
sequenceDiagram
    autonumber
    participant Rv as Reviewer.Cycle (1 min)
    participant F as semmap facade
    participant T0 as Tier 0 rules
    participant T1 as Tier 1 decision model
    participant T2 as Tier 2 reasoner (read-only tools)
    participant M as statemap journal

    Rv->>F: PendingCandidates, CandidateHistory
    Note over Rv: due candidates, ranked by MI score x n_obs
    Rv->>T0: composition, persistent-correlation
    alt a rule decides
        T0-->>Rv: confirm or associate
    else up to MaxPer candidates, ends active
        Rv->>T1: evidence (candidate, ends, neighbours, facts)
        T1-->>Rv: option probabilities
        opt not decisive
            Rv->>T2: analyse with get_property, get_state, get_candidates
            T2-->>Rv: analysis, rescored by tier 1
        end
    end
    Note over Rv: thresholds: confirm 0.85, reject 0.95, defer 0.7<br/>level must permit the action, else annotate
    Rv->>M: Decide review (cites prior review, inputs, tier outputs)
    Rv->>F: Confirm, Reject, Defer or Annotate
    Rv->>M: Commit {action, applied, tier, level, model}
    Note over Rv: a model outage records nothing
```

### A grounded answer, reproduced later

```mermaid
sequenceDiagram
    autonumber
    participant Q as Caller
    participant API as HTTP API
    participant M as statemap.Map
    participant J as Journal and log

    Q->>API: GET /state/estimate?target=X&without=pod:a&assume=Y=0.4
    API->>M: Estimate(request)
    M->>M: Decide(id est-X-hypothesis, "estimate")
    Note over M: every read recorded as it happens<br/>walk relationships into X<br/>contributions range-normalised, caveats stated
    M->>J: Commit decision
    M-->>Q: estimate, contributions, caveats, decision_id
    Q->>API: GET /state/decisions/{id}
    API->>J: Journal, else DecisionFromLog
    J-->>Q: inputs as read at the time
    Note over Q,J: an id in neither: 410 once the journal has dropped entries,<br/>404 otherwise (a dropped id and an unknown one look alike)<br/>/state/decisions/{id}/explain narrates it
```

Relief steps (`relief.act`, `relief.verify`, …), admissions (`admission`, `admission.outcome`) and reviews (`review`) are decisions of the same kind, so each can be read back the same way.

### Asking in natural language

```mermaid
sequenceDiagram
    autonumber
    participant U as Operator
    participant E as explain
    participant Pl as Planner LLM
    participant An as Answering LLM
    participant V as Validator
    participant Cr as Critic LLM

    U->>E: POST /explain {question, use_planner, use_critic}
    opt use_planner
        E->>Pl: question, no tools
        Pl-->>E: plan of tool steps
        E->>E: run plan, evidence bundle
    end
    loop up to MaxIterations
        E->>An: question, evidence, read-only tools
        An-->>E: tool calls or answer
        E->>V: Gate 1, cited values match live state?
        opt use_critic
            E->>Cr: Gate 2, is the reasoning right?
        end
    end
    E-->>U: answer, citations, verdict (422 if no pass)
```

The explain LLM never writes. The autonomy reviewer's model can, within its level — that is the one place a model changes the map, and every such change is a journalled `review` decision.

---

## 9. Deployments

### `poc/` — three VMs, one agent each

```mermaid
flowchart TB
    host(["Host: coordinator.sh"])
    subgraph v1["diag-1 (Multipass)"]
        a1["di-agent :9090<br/>systemd"] --> n1[("Netdata :19999")]
        k1["k0s single node"]
    end
    subgraph v2["diag-2"]
        a2["di-agent :9090"] --> n2[("Netdata")]
        k2["k0s"]
    end
    subgraph v3["diag-3"]
        a3["di-agent :9090"] --> n3[("Netdata")]
        k3["k0s"]
    end
    host -->|GET /cost, POST /recommend, trust drain| a1
    host --> a2
    host --> a3
    a1 <-->|/cost /offers /state| a2
    a2 <--> a3
    a1 <--> a3
```

The case study (`poc/casestudy/`) is one VM, `di-node`, running k0s, Netdata, the agent with the Kubernetes runtime, and Ollama as the tier-1 decision model. The config changes by phase: `agent.yaml` has relief off, `agent-relief.yaml` turns it on from phase P3, and `master.sh` runs `agent-autonomous.yaml` and submits offers via `POST /tasks`.

### `poc2/` — Kubernetes, replayed and live telemetry

```mermaid
flowchart LR
    subgraph infra["libvirt: 4 VMs via Terraform, VM 1 the control plane; or EKS: 3 x t3.medium"]
        subgraph sys["Helm chart di-agent-system"]
            pg[("PostgreSQL")]
            ing["postgres-ingest Job"]
            rs["replay-streamer"]
            kf[("Kafka<br/>dataset.telemetry")]
            dc["dataset-collector :8000"]
            tw["telemetry-writer"]
            ix[("InfluxDB")]
            gf["Grafana"]
            pr["Prometheus"]
            pb["pontos-bridge<br/>optional"]
        end
        subgraph ds["di-agent DaemonSet, hostNetwork, workers only"]
            w1["agent worker-1 :9090"]
            w2["agent worker-2 :9090"]
            w3["agent worker-3 :9090"]
        end
    end
    sp(["Dataset CSVs"]) -->|skipped when PONTOS is on| ing --> pg --> rs --> kf
    pon(["PONTOS MQTT"]) --> pb --> kf
    kf --> tw --> ix --> gf
    kf --> dc
    w1 -->|POST /collect, designated node| dc
    pr -->|scrape /metrics| rs
    gf --> pr
    w1 <-->|full mesh, trust 0.8| w2
    w2 <--> w3
    w1 <--> w3
    host(["Host: 04-peers.sh, coordinator.sh"]) --> ds
```

Every agent also reads the host's `/sys/fs/cgroup` and keeps state under `/var/lib/di-agent`. Only the node named by `agent.datasetNodeName` collects from `dataset-collector`; the default is empty, so dataset collection is off until it is set (`04-peers.sh` requires it). The agent DaemonSet runs on the three workers.
