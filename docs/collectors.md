# Writing a collector

A collector is how the agent observes something new. This guide is for someone who wants to add one without editing the agent: write it, certify it, and plug it in with one entry in `sources.collectors`.

There are four ways in — a Go package compiled into the agent, a program the agent starts, a URL the agent asks, or an application that pushes its own readings — and one rule that decides whether any of them is correct: **a collector is valid if and only if it passes the collector compliance suite.** The rest of this guide is what that suite holds you to, how to run it, and the protocol an out-of-process collector speaks.

Changing the agent itself rather than plugging into it? [`DEVELOPING.md` §4.1](../DEVELOPING.md#41-add-a-collector-most-common) has the in-tree steps. Configuring collectors on a running node? [`semantic-map/README.md` §3](../semantic-map/README.md#3-running-the-edge-daemon) has the `sources.collectors` reference.

---

## Contents

1. [What a collector is](#1-what-a-collector-is)
2. [Choose a path](#2-choose-a-path)
3. [Wire protocol v1](#3-wire-protocol-v1)
4. [Producer rules](#4-producer-rules)
5. [Ownership and `GET /collectors`](#5-ownership-and-get-collectors)
6. [Certification](#6-certification)
7. [Security](#7-security)
8. [Walkthrough: the loadavg example](#8-walkthrough-the-loadavg-example)
9. [Go, out of process: `plugin.Serve`](#9-go-out-of-process-pluginserve)
10. [Go, in process: a custom binary](#10-go-in-process-a-custom-binary)
11. [Shell template](#11-shell-template)

---

## 1. What a collector is

A collector reads some source — a filesystem, a local daemon, a device — and turns what it sees into `MetricSample`s that say what they measure, about which subject, in which unit. It knows nothing about the map: the agent admits each sample as an observation of a property and works out everything downstream.

Every collector, whichever path it takes, is a [`CollectorContract`](../semantic-map/go/pkg/contracts/contracts.go):

```go
type CollectorContract interface {
    Collect() ([]*types.MetricSample, error)
    SourceID() string
    AvailableMetrics() []types.MetricType
    Health() error // nil, or why the source did not answer the last Collect
}
```

The contract's guarantees, verbatim from its doc comment:

> - Pure read: Collect never modifies any system state.
> - Empty on no data: Collect returns ([], nil) when no new samples are ready;
>   it never returns a non-nil error for a temporarily unavailable source.
> - Health: a source that could not be read (refused, unreachable, an error
>   answer) is reported by Health, not by Collect's error, which is for the
>   collector itself failing. An unhealthy collector is polled on every tick and
>   what it last produced is unobservable until it is healthy again.
> - Deterministic EventID: the same physical observation (source, node, subject,
>   metric, anchor timestamp) always produces the same EventID.
> - Declared types: every MetricType a Collect emits is in the AvailableMetrics
>   answer read after that Collect. The set MAY change over the instance's
>   lifetime: a collector driven by include patterns learns what exists only by
>   asking its source, and that changes as disks, interfaces and workloads come
>   and go. A fixed collector returns one set throughout.
> - Node ID completeness: every emitted MetricSample has a non-empty NodeID.
> - SourceID stability: SourceID returns the same string across restarts.
> - Subjects are implied by observation: a collector MAY set Subject on a sample; a
>   subject exists iff something observed it this tick. A collector never emits a
>   "gone" event — silence is the signal, and the map interprets it.
> - Declared meaning: a collector MUST set Unit on every sample, and Range where
>   the quantity has one (a share, a percentage; not a rate or a count), and keep
>   both stable per type: a type is declared with the same unit and range on
>   every sample, or with no range on any. The map assumes [0,1] for an
>   undeclared range and says so wherever it normalises by it.

The wire protocol in §3 is a transport for this contract, not a second contract. An out-of-process plugin is wrapped in an adapter that implements `CollectorContract`, so the same suite certifies it.

A collector is **node-local**: it observes the machine its agent runs on, the agent's `-node-id`. Questions about other machines go to their agents.

All configured collectors run under a **supervisor**. Each tick it calls every member concurrently, each under its own timeout. A member that errors, panics or times out loses only its own samples for that tick and backs off (1 s, doubling to 5 min, reset by a success); the others deliver. Every sample is checked against the contract on the way in, and one that breaks it is dropped and counted while the rest of its batch is kept. A broken collector cannot silence the others.

## 2. Choose a path

| Path | You write | The agent runs it as | Pick it when |
| ---- | --------- | -------------------- | ------------ |
| **In-process Go** (`type:`) | A Go package whose `init()` calls `collectors.Register`, and a custom agent binary (§10) | A goroutine in the agent | You write Go and want no process boundary: lowest overhead, but a Go collector that hangs cannot be killed — the supervisor abandons the call and skips the member until it returns. |
| **exec** (`exec:`) | A program in any language that speaks §3 on stdin and stdout | A child process, started once and kept, restarted after a fault | The default for a third party: any language, no rebuild, and a hang or crash costs a restart, not the agent. Helpers exist for Python (§8) and Go (§9); a shell script works too (§11). |
| **http** (`http:`) | A server that answers §3 as `POST` requests | A URL asked every tick | The collector is already a long-running service (a sidecar, an exporter) or lives in another container. |
| **push** (`POST /ingest-sample`) | An application that sends its own readings | Nothing — the application decides when | The thing being measured is your own application reporting on itself. Not a collector entry; see [Pushing application metrics](../semantic-map/README.md#pushing-application-metrics). The rules in §4 still apply. |

The first three are entries in `sources.collectors`; each entry names exactly one of `type`, `exec` or `http`:

```yaml
sources:
  interval: 10s
  collectors:                 # order = ownership priority for node-level types (§5)
    - name: cgroup            # instance name: [A-Za-z0-9._-]+, unique in the list
      type: cgroup
    - name: gpu
      exec: {command: /opt/di/collectors/gpu, args: [--fast]}
      timeout: 3s             # optional; default min(interval/2, 5s); less than sources.interval
      config: {device: 0}     # sent to the plugin in hello, verbatim, as JSON
    - name: redis
      http: {url: http://localhost:9121/di/collect}
```

## 3. Wire protocol v1

This section is normative. The Go types are in [`pkg/collectors/wire.go`](../semantic-map/go/pkg/collectors/wire.go).

One envelope, two transports:

- **exec:** the agent starts the program once. Newline-delimited JSON: the agent writes one request line on the plugin's stdin and reads one response line from its stdout. Flush after every reply line — the agent writes nothing more until it has read one. Stdout belongs to the protocol; log to stderr.
- **http:** each request is a `POST` of the same JSON body to the configured URL, with `Content-Type: application/json`; the response body is the reply. A non-2xx status is a fault.

### 3.1 Messages

```jsonc
// agent → plugin: once at start and after every (re)start / reconnect
{"op":"hello","protocol":1,"name":"gpu","node_id":"rpi-2","interval_ms":10000,
 "config":{ /* the entry's config block, verbatim, as JSON */ }}
// plugin → agent
{"protocol":1}
// optionally {"protocol":1,"doctor":["./doctor.sh","--verbose"]}
// or {"error":"no nvidia-smi on PATH"}

// agent → plugin: every tick
{"op":"collect","seq":42,"now_unix":1761160000}
// plugin → agent
{"seq":42,
 "available":["gpu_util","gpu_mem_used"],
 "samples":[{"metric_type":"gpu_util","value":0.37,"timestamp_unix":1761160000,
             "unit":"share","range":[0,1],"subject":"pod:9f3…","chart":"gpu"}]}
// or {"seq":42,"samples":[],"unhealthy":"nvidia-smi: no devices found"}
// or {"seq":42,"error":"…"}
```

- **Health.** `unhealthy` says the plugin is working but its source did not answer (the daemon it reads is down, the device is gone), with the reason. It is not a fault: the plugin is not restarted or backed off, it is asked again next tick, `GET /collectors` shows it `unhealthy` with the reason as `last_error`, and the map journals `collector.down` and holds what it last produced `unobservable` until a reply without `unhealthy`. Report a down source this way, not with an empty reply: an empty reply is "no data right now" and its properties would only go stale. The Go SDK's `plugin.Unhealthy(reason)` and the Python SDK's `Unhealthy(reason)` exception send it.
- **Doctor.** An exec plugin MAY name a command in its hello reply's `doctor`. When the member stays down (unhealthy, backoff or stuck) for one minute, the agent runs it once for that episode, never twice within ten minutes, for at most 30 s, in the plugin's directory with the plugin's environment and process group, and journals the command, its output (first 4 KiB) and how it ended as a `collector.doctor` event. An http sidecar's `doctor` is ignored. No `doctor`, nothing runs.

- `samples` entries are `types.MetricSample` JSON, the same body `POST /ingest-sample` takes ([field reference](../semantic-map/README.md#post-ingest-sample)). Each entry is decoded **strictly**: a field `MetricSample` does not have (a misspelt `"valu"`), a field of the wrong type, a missing or `null` `value`, a `null` entry or one that is not an object makes that sample *malformed*. A malformed sample is dropped and counted as a violation (`malformed sample`, §5); the rest of the reply is kept, and the tick is not a fault. A typo therefore never becomes a reading of zero.
- Within one reply, a sample whose `event_id` (§3.2) repeats an earlier one's is a *duplicate*: the first is kept, the rest are dropped and counted as violations (`duplicate sample`). The map would drop them anyway; dropping them here keeps the `samples` counter to what reaches the map.
- `available` is optional and three-valued. **Absent** (or `null`): it is exactly the set of types in that reply's samples — no union with earlier replies — which satisfies the contract (`AvailableMetrics` ⊇ the last `Collect`'s types). **Present**, even as `[]`: it is the full set of types the plugin can produce now, so `[]` says "nothing now". Send it when you can produce more types than one reply happens to carry. An entry that is not a valid `metric_type` is dropped and counted as a violation; a repeated one counts once.
- "No data right now" is `"samples":[]`, never an error. `error` means a fault.
- A reply whose `seq` differs from the request is discarded, and that tick is a fault. Echo the `seq` you were sent; do not count your own, because it does not restart from 1 when the plugin does.
- A plugin `protocol` other than 1, or a hello `error`, is a hello failure.
- `config` is `{}` when the entry has none, so a plugin can always index it. `interval_ms` is `sources.interval` in milliseconds; the interval is at least 1s, because timestamps are whole seconds.
- Stamp each sample with `now_unix` as its `timestamp_unix` unless the source carries its own time. Every collect a plugin receives has a `now_unix` of its own, strictly greater than the one before: when a collect falls in the same wall-clock second as the previous one (a late tick, or `mapctl collector check` calling back to back), the agent moves `now_unix` one second on, so a reading that changed is never hashed to the previous reading's `event_id` and dropped. It may lead the wall clock by a second. `timestamp_unix` is Unix **seconds**; `mapctl collector check` fails a timestamp more than two intervals from `now_unix` (§6), which is what milliseconds look like.

### 3.2 Stamped by the agent

Leave these out; the agent is authoritative for them.

- `node_id`: set to the agent's own when empty. A plugin that sends a different non-empty `node_id` has that sample counted as a violation and dropped.
- `source`: always set to the entry `name`. The adapter's `SourceID` is the entry name too, so it is stable across restarts.
- `timestamp_unix`: if `0` or absent, set to the request's `now_unix` before `event_id` is computed. Without that, every tick would hash the same time and carry the same `event_id`, and the map would drop every sample after the first as a duplicate.
- `event_id`: if empty, computed as `client.EventID(name, node, subject, type, ts)` — the function the ingest push client uses, so a plugin and a pushing producer that observe the same thing produce the same id. Two samples of one type and subject in the same second are therefore one observation; that is the idempotency the map relies on.

### 3.3 Lifecycle

- **Timeout.** Each tick has one deadline, the entry's `timeout` (default `min(interval/2, 5s)`). It covers the collect, and an http sidecar's re-sent `hello`. The supervisor waits 500 ms beyond it, so the adapter's own, clearer error arrives first.
- **Startup.** Every member is built and, for exec and http, sent `hello` before the collection loop starts. Any failure — unknown type, bad config, hello error, protocol mismatch, program not found — **refuses daemon start**, naming the entry (`sources.collectors[2] gpu: hello refused: …`). An exec plugin has 30 s (or its `timeout`, if longer) to start and answer this first `hello`: startup runs outside any tick, and a cold start — an interpreter loading its imports from an SD card, a device opening — can take seconds. A **restart** of an exec plugin is a cold start too and has the same allowance. A restart that fits in the tick's deadline is followed by that tick's collect; one that takes longer ends the tick with `restart: plugin took 7.2s to start and answer hello, past this tick's 5s timeout; it collects from the next tick`. The supervisor stops waiting for it after the tick's wait, as for any slow call: the member is shown `stuck` and skipped until the restart ends, and the other members are not held up. After startup every fault is isolated: it costs that member's tick and puts it in backoff.
- **exec.**
  - The command is resolved once per start: a bare name is looked up on the agent's `PATH`, a relative path against the agent's working directory. A command **given as a path** (it contains a `/`) runs with the **directory of the resolved command as its working directory**, so it can read files shipped beside it by relative path. A **bare name** found on `PATH` runs in the **agent's working directory**: its directory is a shared bin directory, not the plugin's.
  - The environment is minimal (§7).
  - The plugin runs in **its own process group**. A fault — a timeout, a crash, a line that is not JSON, a wrong `seq`, an error reply, an oversized line — kills the whole group, so the children of a wrapper script die with it. The next collect after the backoff starts the program again and re-sends `hello`; `restarts` in `GET /collectors` counts these.
  - A plugin that **exited between ticks** is restarted before the next collect, without that tick counting as a fault. It is counted in `restarts`, and logged as `WARNING plugin exited between ticks (exit status N)` — at most once a minute, with the number of exits since the last line — because a plugin must keep running and exit only when stdin closes. `mapctl collector check` fails such a plugin (`NoUnexpectedRestarts`, §6).
  - stderr is copied to the agent log as `collector[<name>]: <line>`, at most 20 lines per 10 s window per member (lines over 4 KiB are truncated). The first line over the allowance in a window logs a one-time `suppressing the rest of this window` notice; the lines after it are counted, and the count is logged (`N stderr lines suppressed`) when the next stderr line arrives after the window has ended. A plugin that falls silent after a burst therefore leaves its last count unlogged.
  - **Shutdown:** stdin is closed — a well-behaved plugin exits on EOF — then after 2 s the group gets SIGTERM, after 2 s more SIGKILL. Anything left in the group is killed after the plugin exits. A plugin that needed SIGTERM or SIGKILL is named in the error the agent logs at shutdown.
  - After a SIGKILL, on a fault or at shutdown, the agent waits at most 2 s for the process to be reaped; one that is still not gone (stuck in the kernel) is logged and abandoned rather than allowed to hang the collection loop or the shutdown.
- **http.** A timeout, transport error or non-2xx is a fault with backoff. Redirects are not followed: a 3xx is a non-2xx like any other. There is no connection to lose, so a fault is only remembered: the next contact sends `hello` before `collect`, within the same deadline. A sidecar that restarts on its own between ticks hears no new `hello` unless a request failed meanwhile, so it should answer a `collect` that arrives before any `hello` — with an `error` reply if it cannot, which faults the tick and brings the `hello` on the next contact.

### 3.4 Limits

| Limit | Value |
| ----- | ----- |
| Protocol version | `1` |
| One exec reply line / one http response body | 4 MiB; more is a fault |
| Requests in flight per member | 1 — the next is sent only after the reply, or after the fault |
| Backoff after consecutive faults | 1 s, doubling to 5 min, reset by a success |
| Default timeout | `min(interval/2, 5s)`; an explicit `timeout` must be less than `sources.interval`, because the supervisor waits `timeout` + 500 ms for an exec or http member and a timeout at the interval would let one hung plugin stretch every tick past it |
| stderr relayed (exec) | 20 lines per 10 s per member, 4 KiB per line |
| Shutdown (exec) | close stdin, 2 s, SIGTERM, 2 s, SIGKILL; then at most 2 s for the reap |

## 4. Producer rules

These hold for every producer — an in-process collector, an exec or http plugin, or an application pushing to `/ingest-sample`. A push producer has no code in the agent, so these are its obligations.

| Concern | Rule |
| ------- | ---- |
| What to collect | An include list of patterns over the source's own names, with a short curated default and `*` for everything. Selection happens in the producer; the map records whatever arrives. |
| Names | The source's name for the quantity (Netdata: `<context>.<dimension>`), collapsed onto an existing type only where it means the same thing, measured the same way. Two producers that measure a quantity differently keep two names: Netdata's `system.cpu` counts iowait, the root cgroup does not. A `metric_type` is one segment over `[A-Za-z0-9._-]`. |
| Subjects | A per-instance series is a subject: `disk:sda`, `net:eth0`, `pod:<uid>` — `<kind>:<identity>` over `[A-Za-z0-9._:-]`, empty for the node itself. |
| Units | Converted to the map's (a percentage to a fraction, KiB/s to bytes/s). A reading whose unit contradicts the held property is refused. |
| Range | Declared where the quantity has one (a share, a percentage), not for a rate or a count; the same declaration on every sample of a type. |
| Computed series | Skipped: a remainder (CPU idle) or a longer average of a series that is kept (PSI 60 s and 300 s). |
| Chart | The series computed together from one reading carry one `chart`, so discovery can group the arithmetic among them. |
| Values | Finite numbers only: NaN and ±Inf are not valid JSON. Both SDKs drop a sample whose value is not finite before encoding the reply, and write one stderr line per tick for each type that lost one; the rest of the reply is sent. A plugin that writes JSON itself must leave such a sample out — `NaN` on the wire is a reply the agent cannot parse, so a fault and a restart. A reading you cannot compute is a sample you leave out. |
| Identity | A sample is identified by source, node, `subject`, `metric_type` and `timestamp_unix` (its `event_id`). `timestamp_unix` is the reading's time, in seconds. A reading re-sent unchanged on a later tick with the same timestamp is the same observation and is deduplicated by the map — by design, so a source that has not refreshed costs nothing. `labels` are not part of identity: two samples of one type and subject in one reply must not differ only by labels, or the second is a duplicate and dropped. |

`AvailableMetrics()` is at least every type the last `Collect` emitted; a pattern-driven collector's answer grows as its source offers more.

> Watch out for the normalisation trap. A *routed* type — one `domain_spec.json` summarises into a construct — must be a fraction on `[0,1]`. A replay harness once sent network throughput in raw bytes/s and CO-adjacent edges went to tens of thousands. Unrouted types carry whatever unit and range they declare.

## 5. Ownership and `GET /collectors`

Two collectors can measure the same node-level quantity — a plugin and the cgroup collector both reporting `cpu_utilization`, say. If both fed one property, its moving average would mix two instruments. So:

- A node-level (empty `subject`) `metric_type` is **owned** by the earliest-listed member that has emitted it. Samples of an owned type from any other member are dropped and counted as `ownership_drops`, and logged once per member and type.
- Ownership is sticky: it does not pass to another member while the owner is in backoff. A later-listed owner is displaced only when an earlier-listed member starts emitting the type.
- Scoped samples (`type@subject`) have no owner; two collectors can report on different subjects of one type.

List order is therefore a decision. Put the collector you trust for a quantity first. The built-in Netdata collector has a `collapse` flag (default `true`) that emits node-level types from system charts; set it `false` when cgroup is listed before it, as [`agent.example.yaml`](../semantic-map/agent.example.yaml) does.

`GET /collectors` shows what each member has done ([reference](../semantic-map/README.md#get-collectors)):

```json
{"interval_ms":10000,
 "collectors":[
  {"name":"loadavg","kind":"exec","type":"","state":"ok",
   "last_success_unix":1761160000,"last_error":"","last_error_unix":0,
   "samples":1200,"violations":0,"ownership_drops":0,"restarts":0,
   "backoff_until_unix":0,"available":["load1","load5","load15"]}]}
```

How to read it when something is wrong:

| You see | It means |
| ------- | -------- |
| `state: starting` | No successful collect yet. |
| `state: unhealthy`, `last_error` | The call worked but the source did not answer (`Health`, or a reply's `unhealthy`); `last_error` is the reason. Asked again every tick, no backoff. |
| `state: backoff`, `last_error` | The last call faulted; the member is skipped until `backoff_until_unix`. `last_error` names the cause (`no reply within 3s`, `reply seq 4, want 5`, `plugin error: …`). |
| `state: stuck` | A call outlived its timeout and has not returned (an in-process collector that hangs; the exec and http adapters enforce their own timeout, so they rarely get here). The member is skipped until it returns. Its return is then recorded as a fault at that moment — `backoff` with a fresh `backoff_until_unix`, and `last_error` saying how it ended (`abandoned call ended: …`, or that it returned late and its samples were discarded). |
| `restarts` climbing | An exec plugin is crashing, being killed on faults, or exiting on its own between ticks (logged as `plugin exited between ticks`). Its stderr is in the agent log under `collector[<name>]:`. |
| `violations` > 0 | Samples broke the contract and were dropped: a malformed sample (§3.1), a duplicate within one reply, an invalid `metric_type` or `subject`, a `part_of` that is not a node-level type on a sample with a subject, no `unit`, an empty `range`, a foreign `node_id`, or a unit or range that differs from the member's first declaration of that type. An `available` entry that is not a valid type counts too. The first of each kind and type is logged. |
| `ownership_drops` > 0 | The member emits a node-level type an earlier member owns. Rename the type if it measures something different, scope it to a subject, or reorder the list. |
| `available` | The member's `AvailableMetrics` answer, read right after its last successful collect; the supervisor's own answer is the union of these. |

## 6. Certification

**Your collector is valid if and only if it passes the compliance suite.** That is the definition, not a check on top of one. Pass it before you list the collector anywhere.

**A Go collector** runs the suite under `go test` with [`compliance.RunCollectorCompliance`](../semantic-map/go/compliance/collector.go):

```go
func TestQueueCollectorCompliance(t *testing.T) {
    compliance.RunCollectorCompliance(t, func(t *testing.T) contracts.CollectorContract {
        return newFixtureQueue(t) // a collector with data behind it
    })
}
```

The factory takes the subtest's `*testing.T`, so it can use `t.TempDir()` for a fixture source. Build the collector over data: `CollectProducesSamples` fails on an empty batch, because every sample-level check would otherwise pass vacuously.

**Any plugin** — exec or http, any language — is certified with `mapctl collector check`. It runs the plugin behind the agent's real exec or http adapter (no agent needed), puts it through the same suite, and adds the protocol checks:

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
| `--ticks N` | `5` | Collects per protocol check, and per run of the suite's every-batch checks `NoMalformedSamples` and `NoDuplicateSamplesInBatch`. |
| `--interval D` | `10s` | The interval announced in hello, at least 1s. The timeout is the default an entry gets, `min(D/2, 5s)`; `UnitRangeStableOverTicks` spaces its collects `D/10` apart, at least 100 ms. |
| `--node-id ID` | `check` | The node id the agent side announces and stamps. |

| Check | Asserts |
| ----- | ------- |
| `HelloRoundTrip` | The plugin starts and accepts hello with protocol 1, within the 30 s an exec plugin has at agent start (§3.3). If it fails, the run stops here. |
| The compliance suite | `SourceIDNonEmpty`, `SourceIDStable`, `AvailableMetricsNonEmpty`, `AvailableMetricsStableWithoutACollect`, `CollectDoesNotError`, `CollectTwiceNoError`, `SamplesHaveNonEmptyNodeID`, `SamplesHaveNonEmptyEventID`, `SamplesHaveValidTypeAndSubject` (`metric_type`, `subject` and `part_of`), `NoDuplicateSamplesInBatch`, `NoMalformedSamples`, `CollectProducesSamples`, `SamplesDeclareUnitAndRange`, `UnitAndRangeStablePerType`, `SamplesMetricTypeInAvailable`, `EventIDDeterministic` — the same assertions a Go collector passes. The adapter drops a malformed or duplicate sample before the suite sees the batch, and reports it ([`contracts.DropReporter`](../semantic-map/go/pkg/contracts/contracts.go)); `NoMalformedSamples` and `NoDuplicateSamplesInBatch` fail on those reports. |
| `SeqEcho` | At least two collects in a row are answered with the request's `seq`. |
| `ReplyWithinTimeout` | Each of `--ticks` collects returns within the timeout. |
| `UnitRangeStableOverTicks` | A type keeps its unit and range over `--ticks` spaced collects. |
| `NoForeignNodeID` | No sample carries a `node_id` other than the agent's. |
| `TimestampNearNow` | Over `--ticks` collects, every `timestamp_unix` is within 2 × `--interval` of the request's `now_unix`: Unix seconds, not milliseconds. |
| (every batch) | `NoMalformedSamples` and `NoDuplicateSamplesInBatch` look at all `--ticks` collects of their run (3 under `go test`), so a plugin that goes bad on a later tick fails. |
| `NoUnexpectedRestarts` | Over `--ticks` collects (at least two, spaced as above), an exec plugin is never restarted: it keeps running between requests and exits only when stdin closes. |
| `AvailableCoversEmitted` | Every emitted type is in the `available` read after that collect. |

It prints `PASS <check>` or `FAIL <check>: <why>` per check, then `certified` (exit 0) or `not certified (k failed)` (exit 1). Each check starts the plugin afresh, and the plugin's stderr is shown. For an exec plugin the entry name is the command's base name without its extension (`loadavg` for `loadavg.py`).

Run it on the kind of machine the plugin will run on: a plugin that finds no data on your laptop answers `"samples":[]`, which is correct behaviour but certifies nothing. CI certifies the [loadavg](../semantic-map/examples/collectors/loadavg/) and [shell](../semantic-map/examples/collectors/shell/load1.sh) examples this way on Linux.

## 7. Security

There is no sandbox. Read this before listing someone else's plugin.

- **An exec plugin runs with the agent's privileges** — its user, its groups, its capabilities. The operator authorises it by writing it into the config; the agent does not check what it does. Install plugins where only the operator can write (`/opt/di/collectors/`, root-owned), and do not point `command` at a directory others can write to.
- **The environment is minimal.** A plugin gets `PATH`, `HOME` and `LANG` from the agent (only those that are set), plus `DI_NODE_ID` (the agent's node id) and `DI_COLLECTOR_NAME` (the entry name), and nothing else the agent happens to carry. A variable your plugin needs must be set by the plugin itself or a wrapper script, or passed in `config`.
- **`GET /config` withholds what a plugin might treat as a secret.** The agent cannot tell which value is a credential, so in the spec it serves (and prints with `-print-config`) every entry's non-empty `config` block is `[redacted]` and every `exec.args` value is `[redacted]`. An `http.url` is shown with its userinfo masked whether or not it has a password (`http://[redacted]@host/…`, since a bare token is often put there), its whole query replaced (`?[redacted]`) and its fragment dropped; that one view (`spec.RedactURL`) is also what the startup log and an http member's errors — `last_error`, the log — show. The command, the URL's scheme, host and path, and the built-in `type` stay. That is not a vault: the agent's HTTP surface has no authentication ([`OPERATING.md`](../OPERATING.md#read-this-first)), the values are in `agent.yaml` and the process's memory, and the plugin is sent its `config` in every hello. Prefer putting a credential in a file only the plugin's user can read, and pass the file's path.
- **An http plugin is a URL the agent POSTs to every tick.** Point it at something on the node; the agent sends it the node id, the entry name and the `config` block.
- Collectors are read-only by contract ("pure read"). A plugin that changes system state breaks the contract even though no check can see it.

## 8. Walkthrough: the loadavg example

[`semantic-map/examples/collectors/loadavg/loadavg.py`](../semantic-map/examples/collectors/loadavg/loadavg.py) is a complete exec plugin on the Python SDK, [`sdk/python/di_collector.py`](../semantic-map/sdk/python/di_collector.py) (standard library only, one file). It emits, node-level:

| Metric type | Unit | Range | Source |
| --- | --- | --- | --- |
| `load1`, `load5`, `load15` | `load_per_cpu` | none | `/proc/loadavg`, divided by the CPU count |
| `psi_cpu_some_avg10`, `psi_memory_some_avg10`, `psi_io_some_avg10` | `share` | `[0, 1]` | `some avg10` of `/proc/pressure/{cpu,memory,io}`, divided by 100 |

What to notice, in the order the file does it:

1. **It finds the SDK by its own location** (`../../../sdk/python`), not by the working directory or `PYTHONPATH` — the agent passes neither. Keep the `semantic-map/` layout or copy `di_collector.py` beside the plugin.
2. **`setup(hello)` returns `collect`.** The SDK calls `setup` on every hello; raising there refuses the hello, which at startup refuses the daemon. `loadavg` needs no config, so its `setup` just returns `collect`.
3. **`collect(now)` returns a list of samples**, each with `unit`, a `range` where the quantity has one (pressure is a share; load has no ceiling, so none), a `chart` per reading, and `timestamp_unix` set to `now`. It leaves out `node_id`, `source` and `event_id`; the agent stamps them. Returning `(samples, available)` instead would declare more types than one reply carries.
4. **A source that may legitimately be absent is no data; one that must be there is a fault.** A kernel without PSI has no `/proc/pressure`; `read_pressure` returns `[]` and the plugin emits only the load types. Every Linux has `/proc/loadavg`, so `read_load` lets a failure to read it raise: the tick becomes an error reply, the member backs off, and `GET /collectors` shows why — where an empty batch would look healthy. Anything `collect` raises, or returns in the wrong shape, is an error reply; the SDK never lets a tick kill the process.
5. **Logging goes to stderr.** Stdout belongs to the protocol.

Its entry in `agent.yaml`:

```yaml
sources:
  interval: 10s
  collectors:
    - name: cgroup
      type: cgroup
    - name: loadavg
      exec: {command: /opt/di-agent/semantic-map/examples/collectors/loadavg/loadavg.py}
      timeout: 3s
```

The `#!/usr/bin/env python3` line resolves `python3` on the agent's `PATH`, which is the only search path the plugin inherits. The cgroup collector emits no `load*` or `psi_*` type, so neither entry owns a type the other emits (§5).

Try it by hand, without an agent, by piping two requests in. `DI_PROC_ROOT` points it at a fake tree on a machine without `/proc`; the agent's minimal environment never passes it, so under the agent it always reads `/proc`:

```bash
printf '%s\n' '{"op":"hello","protocol":1,"name":"loadavg","node_id":"n1","interval_ms":10000,"config":{}}' \
              '{"op":"collect","seq":1,"now_unix":1761160000}' \
  | DI_PROC_ROOT=/tmp/fakeproc semantic-map/examples/collectors/loadavg/loadavg.py
```

Then certify it, which is what CI runs:

```bash
cd semantic-map/go
go run ./cmd/mapctl collector check --exec ../examples/collectors/loadavg/loadavg.py
```

## 9. Go, out of process: `plugin.Serve`

[`pkg/collectors/plugin`](../semantic-map/go/pkg/collectors/plugin/plugin.go) runs the exec loop for a Go plugin. It imports the standard library and `pkg/types` only, so the plugin binary stays small and does not move when the agent's internals do. `setup` is called on every hello with a `plugin.Hello` — `Name`, `NodeID`, `Interval` (a `time.Duration`) and `Config` — and returns the collect function; an error from it, or a nil function, is sent as the hello's refusal. The collect function takes the request's `now` and returns samples, an optional available list and an error. An error or panic in `collect` becomes an error reply for that tick, and so does a reply that cannot be encoded (a NaN in a `range`, say); a sample with a NaN or infinite value is dropped with a line on stderr. `Serve` returns `nil` when the agent closes stdin.

```go
// Command load1 is an exec collector plugin: load1_raw, the 1-minute load from
// /proc/loadavg as it stands (loadavg.py's load1 is divided by the CPU count).
package main

import (
    "fmt"
    "os"
    "strconv"
    "strings"
    "time"

    "github.com/DiyazY/di-agent/pkg/collectors/plugin"
    "github.com/DiyazY/di-agent/pkg/types"
)

func main() {
    err := plugin.Serve(func(h plugin.Hello) (plugin.CollectFunc, error) {
        path := "/proc/loadavg"
        if p, ok := h.Config["path"].(string); ok && p != "" {
            path = p
        }
        return func(now time.Time) ([]*types.MetricSample, []types.MetricType, error) {
            b, err := os.ReadFile(path)
            if err != nil {
                return nil, nil, nil // no data right now: not a fault
            }
            fields := strings.Fields(string(b))
            if len(fields) == 0 {
                return nil, nil, nil
            }
            v, err := strconv.ParseFloat(fields[0], 64)
            if err != nil {
                return nil, nil, fmt.Errorf("parse %s: %w", path, err)
            }
            return []*types.MetricSample{{
                MetricType: "load1_raw", Value: v, TimestampUnix: now.Unix(), Unit: "load",
            }}, nil, nil
        }, nil
    })
    if err != nil {
        fmt.Fprintln(os.Stderr, err) // stderr reaches the agent's log
        os.Exit(1)
    }
}
```

Returning `nil` samples is sent as `"samples":[]`; returning `nil` available lets the agent take the types in the batch, and a non-nil empty available is sent as `"available":[]`. Build it, certify it with `mapctl collector check --exec ./load1`, and list it as an `exec` entry.

## 10. Go, in process: a custom binary

An in-process collector is a Go package whose `init()` registers a type, compiled into an agent binary of your own. [`pkg/agentmain`](../semantic-map/go/pkg/agentmain/example_custom_test.go) holds the whole daemon, so the binary is five lines:

```go
package main

import (
    "github.com/DiyazY/di-agent/pkg/agentmain"

    _ "example.com/collectors/queue" // its init registers type "queue"
)

func main() { agentmain.Main() }
```

The collector package registers a `Factory`. The factory receives `collectors.Env` (the node id, the collection interval, and `Logf`, which may be nil) and the entry's `config` as a `*yaml.Node`, and decodes it **strictly**, so a misspelt key is refused at startup rather than silently ignored — the built-ins in [`pkg/collectors/builtin.go`](../semantic-map/go/pkg/collectors/builtin.go) do the same:

```go
package queue

import (
    "bytes"
    "errors"
    "io"
    "time"

    "go.yaml.in/yaml/v3"

    "github.com/DiyazY/di-agent/pkg/collectors"
    "github.com/DiyazY/di-agent/pkg/contracts"
    "github.com/DiyazY/di-agent/pkg/ingest/client"
    "github.com/DiyazY/di-agent/pkg/types"
)

func init() { collectors.Register("queue", newQueue) } // panics on an empty or duplicate type

type config struct {
    Socket string `yaml:"socket"`
}

func newQueue(env collectors.Env, n *yaml.Node) (contracts.CollectorContract, error) {
    cfg := config{Socket: "/run/queue.sock"}
    if n != nil && n.Kind != 0 { // no config block: keep the defaults
        raw, err := yaml.Marshal(n)
        if err != nil {
            return nil, err
        }
        dec := yaml.NewDecoder(bytes.NewReader(raw))
        dec.KnownFields(true) // a misspelt key is refused, not ignored
        if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
            return nil, err
        }
    }
    return &collector{node: env.NodeID, socket: cfg.Socket}, nil
}

type collector struct{ node, socket string }

func (c *collector) SourceID() string                     { return "queue" }
func (c *collector) AvailableMetrics() []types.MetricType { return []types.MetricType{"queue_depth"} }

func (c *collector) Collect() ([]*types.MetricSample, error) {
    depth, ok := readDepth(c.socket)
    if !ok {
        return []*types.MetricSample{}, nil // source unavailable: empty, not an error
    }
    now := time.Now()
    return []*types.MetricSample{{
        NodeID: c.node, MetricType: "queue_depth", Value: depth, TimestampUnix: now.Unix(),
        EventID: client.EventID(c.SourceID(), c.node, "", "queue_depth", now),
        Unit:    "items", Source: c.SourceID(),
    }}, nil
}
```

(`readDepth` is yours.) In process, nothing is stamped for you: set `NodeID` and a deterministic `EventID` yourself, as above. Then `type: queue` works in `sources.collectors` of your binary, and the stock `cmd/agent` refuses it as an unknown type. Certify it with `RunCollectorCompliance` (§6).

An in-process collector shares the agent's address space: it cannot be killed, only abandoned, and a panic is recovered but a goroutine it leaked is not. If that matters, run it out of process with §9 instead — the code is nearly the same.

## 11. Shell template

The smallest collector that certifies. It is in the repository as [`semantic-map/examples/collectors/shell/load1.sh`](../semantic-map/examples/collectors/shell/load1.sh), and CI certifies it on Linux.

```sh
#!/bin/sh
# Minimal exec collector: one JSON line in, one JSON line out.
while IFS= read -r line; do
  seq=$(printf '%s' "$line" | sed -n 's/.*"seq":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"op":"hello"'*) echo '{"protocol":1}' ;;
    *'"op":"collect"'*)
      now=$(printf '%s' "$line" | sed 's/.*"now_unix":\([0-9]*\).*/\1/')
      v=$(cut -d' ' -f1 /proc/loadavg)
      printf '{"seq":%s,"samples":[{"metric_type":"load1_raw","value":%s,"timestamp_unix":%s,"unit":"load"}]}\n' "$seq" "$v" "$now" ;;
    *) printf '{"seq":%s,"error":"unknown op"}\n' "${seq:-0}" ;;
  esac
done
```

It names its metric `load1_raw` (unit `load`) so it can run beside the loadavg example, whose `load1` is divided by the CPU count (unit `load_per_cpu`): under one name the two would contend for ownership of one node-level type (§5), and the later-listed one's samples would be dropped. It relies on the agent writing compact JSON (no spaces after `:`), which it does. Any other `op` gets an error reply echoing its `seq`, as §3 requires, so an operation a later agent adds costs that request rather than a timeout. It is a template, not a robust plugin: without `/proc/loadavg` it prints an empty `value`, which is not JSON — a fault and a restart every tick, where a plugin should send an error reply. Once a plugin needs more than this, write it on the Python SDK (§8) or `plugin.Serve` (§9).
