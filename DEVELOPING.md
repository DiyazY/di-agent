# Developing di-agent

A working guide for building, running, extending, and testing this repository.

Deploying rather than developing? Read [`OPERATING.md`](OPERATING.md) — it covers the security posture, footprint, systemd, and monitoring.

For *what the system is and why it is shaped this way*, read [`semantic-map/ARCHITECTURE.md`](semantic-map/ARCHITECTURE.md) — start with §1, which has the layer map and the four request lifecycles. For the operational reference (endpoint tables, flag tables), read [`semantic-map/README.md`](semantic-map/README.md). This file is the one you want when you are about to change code.

---

## Contents

- [1. Install](#1-install)
- [2. The inner loop](#2-the-inner-loop)
- [3. Running for real](#3-running-for-real)
- [4. Extending](#4-extending)
  - [4.1 Add a Collector](#41-add-a-collector-most-common)
  - [4.2 Add a MetricType](#42-add-a-metrictype)
  - [4.3 Swap a contract implementation](#43-swap-a-contract-implementation)
  - [4.4 Add a deployment profile](#44-add-a-deployment-profile)
  - [4.5 Add an HTTP endpoint](#45-add-an-http-endpoint)
  - [4.6 Add an explain tool or prompt](#46-add-an-explain-tool-or-prompt)
- [5. Testing](#5-testing)
- [6. Conventions](#6-conventions)
- [7. Troubleshooting](#7-troubleshooting)

---

## 1. Install

### Required

| Requirement | Version | Why |
| ----------- | ------- | --- |
| Go | 1.22+ | The daemon, CLI, and replay tool. `go.mod` pins 1.22; CI runs 1.22. |

That is the whole hard requirement. The agent has no runtime dependencies beyond the standard library plus three vendored Go modules (`cobra`, `tablewriter`, `parquet-go`).

```bash
git clone https://github.com/DiyazY/di-agent.git
cd di-agent/semantic-map/go
go build ./...        # builds agent, mapctl, replay
go test ./...         # ~230 tests, no external services needed
```

If both commands succeed you have a working checkout. Nothing else on this list is needed to build, test, or run the agent.

### Optional, by task

| You want to… | Also install |
| ------------ | ------------ |
| Regenerate `prior_weights.json` (the structural artefact) from the P1–P5 constants | Python 3.9+ and scipy (`semantic-map/requirements.txt`). Run `python3 -m prior_init.pipeline --root ../../ --out prior_weights.json` from `semantic-map/` — the root is the mega-research checkout, since the pipeline reads the analysis results in its sibling directories |
| Replay the dissertation's Netdata parquets | The parquet dataset — see [§3](#3-running-for-real). Not in this repo (it is ~GB of private experimental data). |
| Use `POST /explain` | A local OpenAI-compatible LLM. [Ollama](https://ollama.com) is smallest: `brew install ollama && ollama pull qwen2.5:7b-instruct` |
| Run the 3-VM PoC | [Multipass](https://multipass.run) |

**The LLM is genuinely optional.** `-explain-provider` defaults to `none`; the daemon boots and serves every other endpoint without it, and `POST /explain` returns `501` with instructions. No published result depends on it — see [ARCHITECTURE §13](semantic-map/ARCHITECTURE.md#13-natural-language-explain-layer-pkgexplain).

---

## 2. The inner loop

Use [`semantic-map/go/dev.sh`](semantic-map/go/dev.sh). It hides the build/run/kill/curl boilerplate so the daily loop is one keystroke. It is dev-only — binaries land in `/tmp` and it is not how you deploy.

```bash
cd semantic-map/go

./dev.sh demo          # first time? this. 3-minute guided tour.
./dev.sh restart       # rebuild + restart the daemon
./dev.sh status        # PID + /healthz + /version
./dev.sh logs          # tail the daemon log
./dev.sh test          # go test ./... + go vet ./...
./dev.sh cli graph     # mapctl with --addr pre-filled
./dev.sh ui            # open the browser viewer
./dev.sh clean         # stop, remove binaries, clear build cache
```

`./dev.sh help` lists everything including env overrides (`PORT`, `PROFILE`, `KD`, `PRIORS`, `ALPHA`, `CONVERGENCE`).

A typical change-test cycle:

```bash
# edit something under pkg/ or internal/
./dev.sh test                    # fast feedback: build + vet + all tests
./dev.sh restart && ./dev.sh cli graph   # see it live
```

### Three ways to drive the same daemon

All three speak the same JSON HTTP API. Pick by task, not preference.

| Surface | Use it for |
| ------- | ---------- |
| `curl` | Poking one endpoint; scripting; anything you want to paste into an issue |
| `mapctl` | Terminal work and headless ops. `mapctl graph`, `mapctl deprecate P1 "reason"`, `mapctl watch edges` |
| `/ui/` | Demos and click-to-mutate exploration. Cytoscape graph, side panel, mutation dialogs |

---

## 3. Running for real

Not via `dev.sh`. Build the binary and run it on the host.

```bash
cd semantic-map/go
go build -o /usr/local/bin/di-agent ./cmd/agent

di-agent \
  -profile edge-minimal \
  -addr :8080 \
  -priors /etc/di-agent/prior_weights.json \
  -kd k0s \
  -netdata-url http://localhost:19999 \
  -collect-interval 10s \
  -regime bursty
```

The full flag table is in [README §3](semantic-map/README.md#3-running-the-edge-daemon). The four that matter most:

- **`-kd`** names this node's distribution and is validated against the artefact's `distributions` list. It selects no magnitude: two agents differing only in `-kd` answer identically until telemetry arrives, which `TestScenario_PerKDAgentsAreIdenticalUntilTheyObserve` asserts. What makes an agent "know it is on k0s" is what it has measured on k0s.
- **`-regime`** (`stable`/`default`/`bursty`/`volatile`) sets `-alpha` and `-convergence` to a pre-characterised bundle. Calibrated against the k0s workload matrix; prefer it over tuning the two numbers by hand.
- **`-collect-interval 0`** disables the autonomous collection loop. The manual `POST /ingest-sample` path still works — useful for replay and tests.
- **`-proposer=false`** on low-CPU nodes. The MI proposer keeps ring buffers per construct pair.

`poc/` has a Multipass-based 3-VM deployment (`make -C poc all`) if you want a real multi-node mesh; see [ARCHITECTURE §12](semantic-map/ARCHITECTURE.md#12-poc-deployment-poc).

### Replaying recorded telemetry

The `replay` binary feeds Netdata parquets through the real ingestion path, so a replayed run exercises exactly the production code: each sample becomes an observation of a property, derived properties recompute, and the paired estimator folds it into the relationships incident to whatever moved. Idempotency is keyed on the event id — for the property's value and observation count as well as for the estimator's pairs — which is what makes a replay a valid experiment rather than a simulation.

```bash
./dev.sh replay list                                        # inventory
./dev.sh replay run --kd k0s --test idle --run 1 --speed 0   # speed 0 = max throughput
./dev.sh replay all --kd k0s --speed 0                       # 9 tests x 5 runs
```

The parquet dataset is **not in this repo** — it is private experimental data from the `iot-edge` campaign. Without it, `replay` has nothing to read; everything else works.

---

## 4. Extending

The architecture exists to make extension local. Nearly every change below touches one file plus one test, and the compliance suite tells you whether you got it right.

### 4.1 Add a Collector (most common)

A Collector reads some metric source and emits typed `MetricSample`s. This is the extension point you will use if you want the agent to observe something new.

The contract is three methods:

```go
type CollectorContract interface {
    Collect() ([]*types.MetricSample, error)
    SourceID() string
    AvailableMetrics() []types.MetricType
}
```

**Steps:**

1. Create `internal/minimal/collector_<source>.go`. Model it on `collector_netdata.go` (HTTP source) or `collector_cgroup.go` (filesystem source).
2. Emit a deterministic `EventID` — the same physical observation must always produce the same ID, or idempotency breaks and replays double-count. Both existing collectors hash `(sourceID, nodeID, metricType, timestamp)`.
3. Return `(nil, nil)` for "no data right now". Never error on an empty source; a missing file or an unreachable endpoint is normal.
4. Wire it in `pkg/profiles/profiles.go`. If more than one collector is active, wrap them in `MultiCollector`.
5. Add `internal/minimal/collector_<source>_test.go` and run the compliance suite against it:

```go
func TestMyCollectorCompliance(t *testing.T) {
    compliance.RunCollectorCompliance(t, func(t *testing.T) contracts.CollectorContract {
        return NewMyCollector(/* … */)
    })
}
```

Note the factory takes `*testing.T` — every `compliance.*Factory` is `func(t *testing.T) contracts.XContract`, so it can call `t.TempDir()` or `t.Fatalf` while constructing. `internal/scripted/collector_test.go` is the shortest working example to copy.

**The rule: your implementation is valid if and only if it passes the compliance suite.** That is the definition, not a check on top of one.

> Watch out for the normalisation trap. Every `MetricSample.Value` must be in `[0,1]`. `NetdataCollector` shipped a bug where network throughput was emitted in raw bytes/s — CO-adjacent edges went to tens of thousands and the reasoner produced nonsense. Normalise against a documented reference (the network metrics use a 1 Gbps ceiling) and clamp.

### 4.2 Add a MetricType

**Three** edits, all required. The enum is currently duplicated across three hand-maintained lists, and skipping any one fails differently:

| # | File | Miss it and… |
| - | ---- | ------------ |
| 1 | `pkg/types/types.go` — the `MetricType` constant block | it does not compile |
| 2 | `pkg/semmap/bridge.go` — `metricTypeToConstruct` | the Bridge silently ignores the sample; no edge ever updates |
| 3 | `cmd/agent/dto.go` — `knownMetricTypes` | `POST /ingest-sample` rejects it with `400 unknown metric_type`, even though the routing table would have handled it fine |

```go
// 1. pkg/types/types.go
FuelConsumption MetricType = "fuel_consumption"

// 2. pkg/semmap/bridge.go
types.FuelConsumption: "RC",

// 3. cmd/agent/dto.go
types.FuelConsumption: {},
```

Miss #3 and the failure is genuinely confusing: an in-process collector works (it calls the Bridge directly), but the HTTP path — which the replay tool and any external collector use — rejects the same metric. Grep for the constant name after you add it; you should get three non-test hits.

Then update the MetricType catalogue in [ARCHITECTURE §5](semantic-map/ARCHITECTURE.md#5-telemetry-pipeline) — required by the documentation rule, see [§6](#6-conventions).

> Three copies of one enum is a smell, not a design. The right fix is a single registry that the Bridge and the DTO validator both read, which is the same refactor the deferred `metric_types.json` work would do. Until then, the third edit is load-bearing.

The catalogue is **compile-time closed on purpose**: `POST /ingest-sample` rejects any type not in the enum, so a misconfigured collector fails loudly instead of poisoning the model with silent unknowns. Externalising it to a config file is a known, deliberately deferred design item — see the open gap in `research-docs/SEMANTIC-MAP-STATUS.md` (private repo) for the chosen approach and why it waits for a real driver.

### 4.3 Swap a contract implementation

Five contracts, all swappable: `Collector` (above), `Ontology`, `Reasoner`, `Proposer`, `Tuner`. Agent code never imports an implementation directly — it takes the interface.

`Storage` and `Updater` used to be here and were deleted rather than reimplemented: they held a second model of the relations the state model already holds, learning from the same telemetry into a different structure. The state model itself (`pkg/statemap`) is deliberately *not* a contract — one implementation, and it is the agent's single model, so an interface there would invite exactly the second implementation that removal eliminated. [ARCHITECTURE §2](semantic-map/ARCHITECTURE.md#2-contract-architecture) has the reasoning.

```
1. Implement the interface in internal/<yourpkg>/
2. Add a test file wiring compliance.Run<Contract>Compliance(t, factory)
3. Register it in a profile (see 4.4)
```

If an **ontology** method genuinely does not apply to your implementation — say a read-only cache layered in front of the canonical store — return `contracts.ErrNotImplemented`. The ontology compliance suite skips those subtests rather than failing them. Do not silently succeed.

Note that this escape hatch is currently *ontology-only*: `ErrNotImplemented` is defined as "operation not implemented by this ontology profile", and only `compliance/ontology.go` checks for it. If you need the same partial-implementation semantics for another contract, generalise the sentinel and teach that contract's suite to skip — do not assume it already works.

**Do not add a sixth contract without a second implementation that needs it**, and the converse holds too: a contract whose subject has moved elsewhere gets removed rather than kept for symmetry. `pkg/statemap`, `pkg/peers` and `pkg/explain` are deliberately concrete, because an interface derived from one example encodes that example's accidents. [ARCHITECTURE §2](semantic-map/ARCHITECTURE.md#2-contract-architecture) records both directions of the rule.

There is also no longer a Python mirror of these interfaces. One existed, described as the authoritative specification, with its own compliance suites; since no Python implementation was ever built, nothing ran those suites, and the mirror drifted out of step with the Go interfaces it claimed to specify. If you are looking for the definition of a contract, it is the Go interface and the suite in `go/compliance` that checks it.

### 4.4 Add a deployment profile

A profile is a named wiring of implementations. The binary is identical across profiles; only the `-profile` flag changes.

In `pkg/profiles/profiles.go`:

```go
switch profileName {
case "edge-minimal":
    return buildEdgeMinimal(cfg)
case "your-profile":            // ← add here
    return buildYourProfile(cfg)
default:
    return nil, nil, fmt.Errorf("unknown profile %q", profileName)
}
```

Then document it in the profiles table in [ARCHITECTURE §3](semantic-map/ARCHITECTURE.md#3-deployment-profiles) and the structure tree in README §1. `cloud-full` (Neo4j + RDF/OWL + Bayesian + SLM) is the planned second profile and is currently unimplemented.

### 4.5 Add an HTTP endpoint

In `cmd/agent/routes.go`, register in the appropriate group function (`registerReadRoutes`, `registerMutationRoutes`, …).

House rules for new endpoints:

- Name the request/response DTOs in `cmd/agent/dto.go`. Serialise `types.Direction` as `"+"`/`"-"`, never the raw int.
- Every mutating POST calls `requireJSON(r)` first — the CSRF mitigation.
- Emit JSON errors via `writeError(w, code, msg)`. The five pre-existing endpoints keep plain-text `http.Error` to minimise diff; do not copy that for new work.
- Map errors to the right status. A caller's mistake is a `4xx`. Returning `500` for a bad `session_id` sends an operator hunting for a server fault over their own typo — that was a real bug in review.
- Add a test in `routes_test.go` using `httptest.NewServer`.

Then update the endpoint table in README §2 and the header comment at the top of `routes.go`.

### 4.6 Add an explain tool or prompt

**A new tool** (`pkg/explain/tools.go`):

1. Append a `ToolSchema` to `toolSchemas` — name, description, JSON-Schema parameters.
2. Add a `case` in `Dispatch` and a `dispatch<Name>` function.
3. Return a `ToolResult{Payload, Digest}`. Keep the payload trimmed to what the model needs; `Digest` is the one-line audit-trace summary.

**Tools must stay read-only.** The registry is what makes "the LLM cannot mutate the graph" true by construction rather than by policy. Adding a mutating tool breaks the human-judgment anchor described in [ARCHITECTURE §8](semantic-map/ARCHITECTURE.md#8-connection-to-research). If the model should be able to suggest a change, it already can — as a draft `Proposal` the operator POSTs themselves.

**A new prompt version:** copy `cmd/agent/prompts/explain-v1.md` to `-v2.md` and edit the copy. Never edit `v1` in place — the answering prompt's `sha256[:12]` is stamped on every response as `prompt_version`, and results reported against v1 must stay reproducible.

---

## 5. Testing

```bash
./dev.sh test                 # build + vet + all tests
go test -race ./...           # what CI runs; use before pushing
go test -v -run TestScenario ./internal/minimal/tests/    # narrated end-to-end flows
go test -v -run TestEvolution ./internal/minimal/tests/   # long-form convergence stories
```

### The four layers of test

| Layer | Where | Answers |
| ----- | ----- | ------- |
| **Compliance** | `compliance/*.go` | Does this implementation honour its contract? |
| **Scenarios** | `internal/minimal/tests/scenarios_test.go` | Do the parts compose into the promised behaviour? |
| **Evolution** | `internal/minimal/tests/evolution_test.go` | Does the graph converge correctly over hundreds of ticks? |
| **Integration** | `cmd/agent/*_test.go` | Does the HTTP surface behave? |

Scenarios and evolution tests print `t.Logf` snapshots, so `go test -v -run TestEvolution` reads like a results section. That is intentional — they double as reproducible evidence for the paper.

### Two hard-won lessons

**Write tests that fail when the feature is deleted.** Two tests in the explain package passed for months while asserting on a mock's own script rather than on production behaviour — deleting the code under test left them green. If you are unsure, mutation-test it: break the feature, confirm the test fails, revert.

```bash
# the check that matters
<break the production code>
go test ./pkg/explain/ -run TestYourThing   # must FAIL
<revert>
```

**A deleted test file makes the suite greener, not redder.** Test files were lost twice during this project — once to a truncating edit, once to an over-eager cleanup — and `go test ./...` stayed green both times because missing tests simply do not run. Check the count, not just the exit code:

```bash
grep -rc "^func Test" --include='*_test.go' . | awk -F: '{s+=$2} END {print s}'
```

### Race detection is not optional

Anything reachable from an HTTP handler needs a concurrency test. The session store shipped with a real data race that survived six commits and a clean `go test -race ./...` — because no test exercised two requests on one session. `-race` only finds what you actually execute concurrently.

---

## 6. Conventions

### The documentation rule

Some changes require a doc update **in the same commit**. From the repo-root `CLAUDE.md` (private repo):

| Change | Update |
| ------ | ------ |
| Add or remove a contract | `ARCHITECTURE.md` §2, `README.md` §1 |
| Add or remove a deployment profile | `ARCHITECTURE.md` §3, `README.md` §1 |
| Add or remove a `MetricType` | `ARCHITECTURE.md` §5 |
| Add a collector implementation | `ARCHITECTURE.md` §5, `README.md` §1 |
| Change the Agent API (endpoints, fields, flags) | `README.md` §2 and §3 |
| Change the structural initialization pipeline | `semantic-map/README.md` §5; regenerate `prior_weights.json` and re-run `pkg/profiles` tests |

**README is the quick-start** (structure, API, commands). **ARCHITECTURE is the design record** (why, how, what is stable). Keep them separate — no rationale in README, no commands in ARCHITECTURE.

### What CI enforces

[`.github/workflows/ci.yml`](.github/workflows/ci.yml) on every push to `main` and every PR: `go.mod` tidy, `go build ./...`, `go vet ./...`, and `go test -race ./...`. Run `go test -race ./...` locally first; it is the slowest gate and the one that catches real bugs.

### Public/private boundary

This repository is **public**. The `mega-research` repository is **private**. Implementation code, compliance suites, and artifacts a reader needs to reproduce or extend published work belong here. Raw experimental data, analysis scripts, manuscript drafts, reviewer correspondence, and intermediate results stay private.

The test: *would an independent researcher need this file to validate or build on the published result?* Yes → here. No → private.

Never commit absolute local paths (`/Users/…`).

---

## 7. Troubleshooting

**`POST /explain` returns 501.** Working as intended — `-explain-provider` defaults to `none`. The response body says how to enable it.

**Port already in use.** Under k0s, kube-router owns `:8080`; the PoC scripts use `:9090`. Use `-addr` or `PORT=9000 ./dev.sh start`. The PoC scripts pre-check ports and fail with a clear message rather than timing out mysteriously.

**Collector emits nothing.** Three usual causes: (1) `-collect-interval 0` disables the loop; (2) `-cgroup-root ""` disables the cgroup collector — normal on macOS, which has no cgroups v2; (3) the metric source is unreachable. `./dev.sh logs` shows which collector is configured at startup.

**Edge weights look absurd (thousands, not `[0,1]`).** A collector is emitting un-normalised values. Every `MetricSample.Value` must be in `[0,1]`. See the warning in [§4.1](#41-add-a-collector-most-common).

**Confidence plateaus below 1.0 and never rises.** Expected. Only 9 of 15 edges touch constructs observable from CPU/RAM/network metrics, so mean confidence ceilings around 0.60. It is a property of the metric-coverage boundary, not a bug — see `convergence/NOTES.md` in the private repo.

**`ResourceCost` is 0 under heavy synthetic load.** This was a real effect of the old cost function, which accumulated `(effective − prior) × sign(direction)` and let negative-direction contributions cancel positive ones once CPU ran above the calibrated priors. **It no longer applies:** the estimate leads with the observed level, so `ResourceCost` rises with utilization. `stress-ng` is now the *right* tool for a different reason — it makes both endpoints vary, which is what a pair needs, and an idle demo can run indefinitely and teach the map nothing. Note the relation sum, reported separately as a sensitivity, can still be zero or negative; that is a slope, not a cost. Correctly.

**`go test` green but you changed nothing that should pass.** Check the test count (see [§5](#5-testing)). A deleted or renamed test file does not fail.

**Replay finds no parquets.** The dataset is not in this repo. `./dev.sh replay list` shows what it looked for and where.

---

## Where to read next

| You want | Read |
| -------- | ---- |
| The mental model — layers, components, request lifecycles | [ARCHITECTURE §1](semantic-map/ARCHITECTURE.md#1-core-concept) |
| Why the contracts are shaped this way | [ARCHITECTURE §2](semantic-map/ARCHITECTURE.md#2-contract-architecture) |
| Endpoint and flag reference | [README §2](semantic-map/README.md#2-the-agent-api), [§3](semantic-map/README.md#3-running-the-edge-daemon) |
| Multi-agent coordination and trust | [ARCHITECTURE §10](semantic-map/ARCHITECTURE.md#10-coordination) |
| The natural-language layer | [ARCHITECTURE §13](semantic-map/ARCHITECTURE.md#13-natural-language-explain-layer-pkgexplain), [§14](semantic-map/ARCHITECTURE.md#14-explain-v2--planning-critic-sessions-streaming) |
| The 3-VM PoC | [ARCHITECTURE §12](semantic-map/ARCHITECTURE.md#12-poc-deployment-poc) |
