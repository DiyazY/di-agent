# End-to-end case study

One machine, one run: workloads arrive, change and leave, and the agent's map, discovery,
counterfactual estimate, relief loop and learning from its own actions are all exercised and traced
back to map state. It is a demonstration, not a measurement: one run, no repetitions.

## The machine

A Multipass VM (Ubuntu 24.04, arm64, 12 vCPU, 16 GB, 40 GB disk, cgroup v2; CPUs 0–5 for pods, 6–11 for the agent's models, `node-cpus.sh`) with k0s **v1.36.4+k0s.1** as a single
node, installed by `poc/scripts/02-k0s.sh`, and the agent's own decision model (`tev1:4b-node`, `tev1:4b` at a 2048-token context, on Ollama;
`tier1.sh`: CPUs 6–11 only, CPU weight 20, memory held between 6 and 9 GB). The agent runs on
the VM as root from `agent.yaml`, then `agent-relief.yaml`, with the review loop deciding on its own:
rules, then the decision model, on the node.

## Provisioning

From the host, in this directory:

```bash
./provision.sh di-node     # VM, k0s, Netdata, the tunnel key, this directory on the node
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go -C ../../semantic-map/go build -ldflags "-s -w -X github.com/DiyazY/di-agent/pkg/agentmain.BuildCommit=$(git rev-parse --short HEAD)" -o "$PWD/agent" ./cmd/agent
multipass transfer agent di-node:/tmp/agent.new && multipass exec di-node -- sudo mv /tmp/agent.new /home/ubuntu/casestudy/agent.new
```

The decision model is the node's own: a node that cannot decide without the network is not
autonomous. `node-cpus.sh` and `tier1.sh` give it six CPUs no pod can use, a low CPU weight, and a memory
band of 6–9 GB (a reserved floor and a cap), so it cannot take what the workloads need and its footprint
is a constant in the node's memory. There is no reasoning tier on the node: beside the decision model, a
local `qwen3:8b`, then `qwen3:4b`, ran Ollama past its cap, and loading one model at a time swung node
memory by gigabytes and buried a workload's wave (see `tier1.sh`). A reasoning model on a peer or the host
is optional: `tunnel.sh`
carries the host's Ollama to the node's `localhost:11435` (`--install` keeps it under launchd), using the
key `provision.sh` authorises. The case study used the host's `qwen3.8:27b` once and dropped it — it ran
the host out of GPU memory mid-review — and may return to one.

The agent runs as `di-agent.service` (`systemd-run`), and the configs model the node's other consumers as
`unit:` subjects: the agent itself, its decision model (`ollama.service`), k0s, containerd and Netdata. CPUs
6–9 are the decision model's alone: pods, k0s, containerd, Netdata and the agent are held to 0–5. Ollama on the host keeps listening on 127.0.0.1 only.

`demo.sh <out-dir>` (on the node) is a 48-minute compressed run (`agent-demo.yaml`): planted pods arrive and leave, then two relief episodes on the hog.

## The workloads and what each should produce

Each moves one resource on its own period, on cycles that never line up, so pods should not
correlate with each other — except the twins, which share a wall-clock schedule.

Each pod carries a `di-agent/group` label — `compute` (cycler, twins, noise), `memory` (mem-wave, mem-squeeze),
`storage` (io-wave), `relief` (hog, victim, mem-victim, page-hog) — so the map holds each group's use of the node and P2 also asks
node memory `without=group:memory`.

| Pod | Moves | Period | Planted relationship |
|---|---|---|---|
| `cycler` | CPU, 45 s busy / 45 s idle, limit 1 core | 90 s | its CPU → node CPU; its CPU → node CPU pressure |
| `mem-wave` | memory, 400 MB held / freed | 131 s | its memory → node memory |
| `mem-squeeze` | memory pressure, buffered writes through a 64 Mi limit | 173 s | its memory pressure → node memory pressure |
| `io-wave` | direct-I/O writes | 97 s | its I/O → node I/O pressure |
| `twin-a`, `twin-b` | CPU on one wall clock, throttled at 200m | 113 s | twin-a CPU ↔ twin-b CPU |
| `noise` | CPU in random bursts | none | nothing |
| `hog` | CPU, steady, six busy loops, limit 4.5 cores | none | relief phase only: its CPU → node CPU contention, learned by intervention |
| `victim` | CPU, three busy loops, limit 6 cores, never throttled, frozen by policy | none | relief phase only: what relief protects; the workload whose waiting is the contention |
| `mem-victim` | page cache: a 1 GiB file in an emptyDir re-read every second, within its 1280Mi request | none | memory relief only: the protected pod whose memory stall is `memory_contention_ratio` |
| `page-hog` | 6 GiB anonymous held, then a file set growing 256 MiB/min, re-read whole, no memory limit; holds once a pass refaults a tenth of the set | none | memory relief only: the candidate for `memory.high` |

The relationships marked with a node quantity that has several drivers (node CPU pressure, node I/O
pressure) were missed in earlier runs: no single pair reaches the threshold.

## The run

`schedule.sh` drives it; `capture.sh` records the agent's API alongside. Times are from agent start.

| Phase | t (min) | What happens |
|---|---|---|
| P0 baseline | 0–10 | k0s system pods only |
| P1 arrival | 10–50 | the seven discovery pods applied 41 s apart |
| P2 counterfactual + departure | 50 | the agent is asked for node memory without `mem-wave`, and the decision id kept; then `mem-wave` is deleted |
| P2 observe | 50–65 | `mem-wave`'s properties go stale (2 min) and retire (10 min) |
| P3 quiesce + restart | 65–72 | the other planted pods are deleted; the agent restarts from its snapshot on `agent-relief.yaml` |
| P4 relief | 72–117 | `hog` and `victim` applied; relief caps hog and verifies (first episode); `victim` deleted at 88, so contention ends and the cap is released; `victim` back at 95 (second episode) |
| P5 release | 117–127 | `hog` and `victim` deleted |

Relief runs on its own pair because a result counts as evidence only when no other workload moved
during its window, and the planted pods move continuously. Its signal is node CPU contention, the
share of time some workload waited for a CPU another was using, so it needs a victim: `hog` alone
at its limit is throttled, not contended. A relationship needs two agreeing results and the policy
cools a workload down for 10 minutes after it is touched, hence two episodes.

Everything runs on the node, as root, from this directory, next to an `agent` binary:

```bash
sudo ./launch.sh run-$(date +%F)       # capture + timetable, detached; about 2 h 10 min
# when run-*/schedule.out says "done exit=0", stop this run's capture:
sudo kill "$(cat run-*/capture.pid)"
```

A `done` with a non-zero exit is a timetable that stopped early; `phases.jsonl` says where.

Nothing in the timetable confirms a relationship: the agent's review loop does. P2 refuses to delete
`mem-wave` — marking why and leaving the rest of the timetable to run — unless the agent has confirmed its
memory against node memory and that relationship has at least one observation (a relationship confirmed a
moment ago has no strength, and the estimate would leave it out), and the estimate returned 200 with a
decision id and `mem-wave` in `excluded`. The mark records which reviewer confirmed it.
`repeat-p2.sh <out-dir>` runs that episode on its own against the running agent, and exits non-zero when it refuses.
`verify.sh <out-dir>` reruns P3–P5 on a fresh map: the verification run of the measured `Intervened` strength.

Both write JSON lines stamped with the VM's clock: `capture.jsonl` (`t`, `endpoint`, `status`,
`body`; a failed poll has `status: 0` and `body: null`) and `phases.jsonl` (`t`, `phase`, `event`, and
any ids).

## Memory relief

`memrelief.sh <out-dir> [phase-1-min] [phase-2-min]` (on the node, as root) exercises the second lever on its
own: it saves the `di-agent.service` it replaces, refuses to start when any `memory.high` under `kubepods`
is not `max` (the actuator would refuse too), starts the agent on a fresh map from `agent-relief.yaml` with the
ledger in `<out-dir>/actions.json`, applies `mem-victim` then `page-hog`, and polls `/relief`,
`/relief/actions`, `/actuators`, the node's memory properties and each pod's subject every 30 s
(`poll.jsonl`), beside the node's own view of the same pods' cgroups (`node.jsonl`). Phase 2 runs `mem-squeeze`
alone, which limits itself at its own 64Mi limit (`mem-wave` never reaches its 600Mi limit). An OOM kill of
anything but `page-hog`, or k0s, Ollama or the agent going unhealthy, deletes `page-hog` and ends the run.
The VM has no swap, so `memory.high` can only reclaim file pages: both workloads live on page cache, and
`page-hog`'s anonymous block only shrinks the room left for it. What the runs showed is in
`run-2026-10-23-memrelief/results.md`.
