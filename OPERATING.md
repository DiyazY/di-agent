# Operating di-agent

For the person deploying and running this on real nodes.

If you are changing code, read [`DEVELOPING.md`](DEVELOPING.md) instead. If you want to know why the system is shaped this way, read [`semantic-map/ARCHITECTURE.md`](semantic-map/ARCHITECTURE.md).

---

## Read this first

**This is a research artifact, not production software.** It is the implementation behind a dissertation chapter. It is stable enough to run unattended on a lab cluster for weeks — that is how the reported results were produced — but it makes assumptions no production deployment should accept:

| Assumption | Consequence for you |
| ---------- | ------------------- |
| **No authentication on any endpoint.** No TLS, no bearer tokens, no middleware. | Anyone who can reach the port can read the **whole model** — `GET /state` returns every property, relationship, decision and journal entry, not only the graph — *and mutate it*: deprecate propositions, reset edges, retune weights. Agents fetch each other's `/state` the same way, so a reachable agent also discloses to any host that can pose as a peer. Treat the listen address as fully trusted. |
| **State is in memory unless `-state-file` is set.** | Without it, a restart discards every learned strength, confidence and journal entry, and the agent returns to cold start — every relationship reporting `unknown` again. With it, the map and its journal are snapshotted periodically and on shutdown, and a snapshot naming a different owner is refused. |
| **`-addr :8080` binds every interface.** | On a multi-homed host this is reachable from anywhere routable. Bind explicitly. |

None of these are oversights — they are recorded scope decisions ([ARCHITECTURE §10](semantic-map/ARCHITECTURE.md#10-coordination)) for a lab-network deployment, with production hardening deferred to P7. But they mean the deployment rule is simple:

> **Run it on an isolated or trusted network segment, bound to a private interface. Set `-state-file` if a restart must keep its learned state; without it, a restart returns to cold-start priors.**

If you need it on a shared network, put it behind something that terminates TLS and authenticates — a reverse proxy with client certs, or a WireGuard/Tailscale interface. Do not expose port 8080 to anything you do not control.

---

## Contents

- [1. What it needs](#1-what-it-needs)
- [2. Install](#2-install)
- [3. Configure](#3-configure)
- [4. Run under systemd](#4-run-under-systemd)
- [5. Verify it is working](#5-verify-it-is-working)
- [6. Monitor](#6-monitor)
- [7. Multi-node setup](#7-multi-node-setup)
- [8. Upgrade and rollback](#8-upgrade-and-rollback)
- [9. What it touches on your system](#9-what-it-touches-on-your-system)
- [10. Troubleshooting](#10-troubleshooting)

---

## 1. What it needs

### Hardware

Measured on the reference build (`linux/arm64`, single static binary, no runtime):

| Metric | Value |
| ------ | ----- |
| Binary size | 9.3 MB |
| RSS at idle | ~12 MB |
| RSS with collection loop at 5 s | ~11–12 MB |
| RSS after 500 observations (warm graph) | ~15 MB |
| OS threads | 8 |

A Raspberry Pi 4 (4 GB) runs this with room to spare — it was the reference target. The graph is fixed at 7 constructs and 15 edges, so memory does **not** grow with uptime; the ~3 MB delta above is the observation ring buffers, and those are bounded.

Set `-proposer=false` on nodes where you want to reclaim the ring-buffer overhead entirely.

### Software

| Requirement | Notes |
| ----------- | ----- |
| Linux with cgroups v2 | Only if you want the cgroup collector. `-cgroup-root ""` disables it. |
| Netdata (optional) | For richer host metrics. `-netdata-url ""` disables it. Tested against v1.42 and v1.33. |
| Nothing else | Static Go binary. No interpreter, no shared libraries, no package manager. |

The agent does **not** require Kubernetes to run. `-kd` only names the distribution for the record and is validated against the artefact's `distributions` list; it selects no magnitude and does not talk to an API server.

---

## 2. Install

There are no packages or images. Build the binary and copy it.

```bash
# On a build host with Go 1.22+
git clone https://github.com/DiyazY/di-agent.git
cd di-agent/semantic-map/go

# Cross-compile for a Raspberry Pi 4 or other arm64 Linux node.
# CGO_ENABLED=0 gives a fully static binary with Go's own DNS resolver —
# no glibc dependency, and no nscd/unix-socket needs to allow through the
# systemd sandbox in §4.
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o di-agent-arm64 ./cmd/agent

# amd64 edge server
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o di-agent-amd64 ./cmd/agent
```

Building natively on the node (`go build -o di-agent ./cmd/agent`) also works, but defaults to `CGO_ENABLED=1` and therefore glibc's resolver — see the sandbox caveat in [§4](#4-run-under-systemd).

Then on each node:

```bash
sudo install -m 0755 di-agent-arm64 /usr/local/bin/di-agent
sudo mkdir -p /etc/di-agent
di-agent --help          # confirm it runs
```

### Optional: initialization artefact

`prior_weights.json` declares which propositions the agent carries and in which direction — structure, and no strength for any of them. Without it the agent still works — it uses the built-in Di-Select proposition strengths.

```bash
sudo install -m 0644 semantic-map/prior_weights.json /etc/di-agent/prior_weights.json
```

Pass it with `-priors /etc/di-agent/prior_weights.json`. `-kd` names this node's distribution and is validated against the file; it no longer selects a magnitude, so two agents differing only in `-kd` answer identically until telemetry arrives.

---

## 3. Configure

Everything is command-line flags — there is no config file. The full table is in [README §3](semantic-map/README.md#3-running-the-edge-daemon); these are the ones that matter operationally.

### Must decide

| Flag | Why you care |
| ---- | ------------ |
| `-addr` | **Bind explicitly.** `-addr 127.0.0.1:8080` for local-only, or `-addr 10.0.0.5:8080` for one interface. The `:8080` default listens everywhere. |
| `-node-id` | Identifies this agent in observations and event IDs. Defaults to `os.Hostname()`. Set it if hostnames are not stable. |
| `-kd` | `k3s`/`k0s`/`k8s`/`kubeEdge`/`openYurt`. Validated against the artefact's `distributions` list; the daemon refuses to start on a name that is not there. Selects no magnitude, so a wrong-but-valid value changes no answer. Wrong value = a mislabelled record, and the agent will not tell you. |

### Should tune

| Flag | Guidance |
| ---- | -------- |
| `-regime` | `stable` / `default` / `bursty` / `volatile`. Sets `-alpha` and `-convergence` to a bundle calibrated against the k0s workload matrix. **Prefer this over tuning the two numbers by hand.** Use `stable` for steady IoT nodes, `bursty` for control-plane-heavy ones. |
| `-collect-interval` | `10s` default. `5s` on nodes you want to converge faster; `0` disables autonomous collection entirely (manual `POST /ingest-sample` still works). |
| `-proposer` | `true` default. Set `false` on CPU-constrained nodes — it keeps ring buffers per construct pair. |

### Port conflict warning

**Under k0s, kube-router owns `:8080`.** The PoC deployment uses `:9090` for exactly this reason. If you are co-locating with k0s, do not use the default port.

---

## 4. Run under systemd

The PoC ships a working unit at [`poc/config/di-agent.service`](poc/config/di-agent.service). Adapted for a general node:

```ini
# /etc/systemd/system/di-agent.service
[Unit]
Description=di-agent semantic map daemon
After=network-online.target netdata.service
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/di-agent/env
ExecStart=/usr/local/bin/di-agent \
  -profile edge-minimal \
  -addr ${LISTEN_ADDR} \
  -node-id ${NODE_ID} \
  -kd ${KD} \
  -priors /etc/di-agent/prior_weights.json \
  -netdata-url http://localhost:19999 \
  -collect-interval 10s \
  -regime ${REGIME}
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal

# Hardening — the agent needs no write access anywhere and no privileges.
# It reads /sys/fs/cgroup and opens one listening socket. Nothing else.
DynamicUser=yes
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ReadOnlyPaths=/sys/fs/cgroup
# AF_UNIX is required, not optional: a CGO-enabled build (the default when
# you `go build` natively on the node) uses glibc's resolver, which may talk
# to nscd/systemd-resolved over a unix socket. Omit it and hostname lookups
# for peers and Netdata fail while everything else looks healthy.
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
MemoryMax=128M

[Install]
WantedBy=multi-user.target
```

> **Two caveats on this unit, stated plainly.**
>
> 1. **It has not been run on a live systemd host.** The directives follow from verified properties of the binary — it writes nothing, needs no privileges, opens one socket — but the unit itself is untested. Deploy it to one node and check `systemctl status` and `journalctl -u di-agent` before rolling it out. If the sandbox is the problem, comment out the hardening block and re-add directives one at a time.
> 2. **`MemoryMax=128M` assumes default session caps.** It is ~8× the measured working set, which holds for the agent plus the explain layer at its default limits (100 sessions × 20 turns). Raise it if you increase those.
>
> Build with `CGO_ENABLED=0` to sidestep the resolver question entirely — you get a fully static binary using Go's own DNS resolver, which needs only `/etc/resolv.conf`:
>
> ```bash
> CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o di-agent-arm64 ./cmd/agent
> ```

```bash
# /etc/di-agent/env
LISTEN_ADDR=10.0.0.5:9090
NODE_ID=edge-node-1
KD=k0s
REGIME=stable
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now di-agent
sudo systemctl status di-agent
journalctl -u di-agent -f
```

The hardening directives above are additions to the PoC unit, not copied from it — as written (no `-state-file`) the agent needs no filesystem writes, no privileges, and no address families beyond IP, so lock all of that down. **If you enable persistence**, add the state file's directory to `ReadWritePaths=`: `ProtectSystem=strict` makes the whole tree read-only, and both the periodic write and its temp file need that directory writable, so a hardened unit otherwise fails to persist silently while everything else looks healthy. `MemoryMax=128M` is roughly 8× the measured working set, which leaves headroom while still killing a runaway.

**`Restart=always` interacts with persistence — know which mode you are in.** With `-state-file`, a restart restores the map and journal from the last snapshot (written periodically and on clean shutdown), so `Restart=always` keeps the agent both available and warm; only what changed since the last periodic write is lost, and only on an unclean kill. Without a state file, every restart is a full reset to cold-start priors — automatic restart keeps the agent available but discards all learned evidence, and frequent restarts in `journalctl` are a data-loss event, not a blip.

---

## 5. Verify it is working

Four checks, in order. Each one tells you something the previous one cannot.

```bash
ADDR=10.0.0.5:9090

# 1. Process is up and serving
curl -sf http://$ADDR/healthz && echo
# → {"ok":true}

# 2. Build identity — confirm you deployed what you think you did
curl -s http://$ADDR/version | jq
# → {"agent_version":"0.1.0","go_version":"go1.22…","semmap_constructs":7,"semmap_propositions":15}

# 3. Graph seeded correctly
curl -s http://$ADDR/graph | jq '{constructs:(.constructs|length),
                                  propositions:(.propositions|length),
                                  edges:(.edges|length)}'
# → {"constructs":7,"propositions":15,"edges":15}
# These counts are FIXED for the edge-minimal profile and stay 15 for the
# life of the process — deprecation flips a flag rather than removing an
# edge. Anything other than 7/15/15 means seeding failed.

# 4. Evidence is actually accumulating (the one that catches a silent collector)
curl -s http://$ADDR/graph | jq '[.edges[].n_observations] | add'
# → 0 immediately after start; must be climbing a minute later
```

**Check 4 is the important one.** Checks 1–3 pass on a completely blind agent — one whose collector is misconfigured and has never observed anything. If `n_observations` stays at 0 after several collection intervals, the agent is running but learning nothing. See [§10](#10-troubleshooting).

---

## 6. Monitor

### What to alert on

| Signal | How | Meaning |
| ------ | --- | ------- |
| Liveness | `GET /healthz` != 200 | Process down or wedged |
| Restarts | `systemctl show -p NRestarts di-agent` | Each one resets learned state |
| Learning stalled | `sum(n_observations)` flat over 10× the collect interval | Collector broken or source unreachable |
| Backbone retired | count of `edges[].deprecated == true` > 0 | Propositions were deprecated — intentionally or not |

```bash
# Total observations — the single best health number
curl -s http://$ADDR/graph | jq '[.edges[].n_observations] | add'

# Mean confidence — how far the agent has moved from literature to evidence
curl -s http://$ADDR/graph | jq '[.edges[].confidence] | add / length'

# Retired propositions — count, then which ones
curl -s http://$ADDR/graph | jq '[.edges[] | select(.deprecated == true)] | length'
curl -s http://$ADDR/graph | jq -r '.edges[] | select(.deprecated == true) | "\(.proposition_id): \(.deprecated_reason)"'

# Who changed the ontology, and when
curl -s "http://$ADDR/history" | jq -r '.[] | "\(.timestamp) \(.actor) \(.kind) \(.target_id)"'
```

> **Do not monitor `edges | length` to detect deprecation — it never changes.** Deprecation is a soft delete: the `EdgeDescriptor` and the `Proposition` both stay in `/graph` (so replay and audit remain intact) and only the `deprecated` flag flips. Counts stay at 15 forever. The reasoner skips flagged edges; your monitoring has to look at the flag too.

### Expect confidence to plateau around 0.60

This is not a bug and not a misconfiguration. Only 9 of the 15 propositions touch constructs observable from CPU/RAM/network metrics, so mean confidence ceilings near `9/15 = 0.60`. An agent sitting at 0.60 with a healthy observation count is fully converged for the telemetry it has.

### Logs

Everything goes to stdout/stderr → journal. At startup the agent logs which collectors are configured, whether peers were registered, and the explain provider. Read those three lines after any config change — they are the fastest confirmation your flags took effect.

```bash
journalctl -u di-agent --since "5 min ago" | grep -E "collection loop|registered|explain:"
```

---

## 7. Multi-node setup

Peers are how agents offload work to each other. Registration is runtime, not config.

```bash
# On node A, register B and C
curl -sf -X POST http://nodeA:9090/peers -H 'Content-Type: application/json' \
  -d '{"url":"http://nodeB:9090","note":"rack-1"}'
```

Two things that surprise people:

1. **The server derives the peer ID** as `sha256(url)[:12]` and **ignores any `id` or `trust` you send.** New peers start at trust `0.5`. To set trust you need a second call:

```bash
PEER_ID=$(curl -sf -X POST http://nodeA:9090/peers -H 'Content-Type: application/json' \
  -d '{"url":"http://nodeB:9090"}' | jq -r '.id')
curl -sf -X POST http://nodeA:9090/peers/$PEER_ID/trust -H 'Content-Type: application/json' \
  -d '{"value":0.8}'
```

2. **Peering is directional.** Registering B on A does not register A on B. For a full mesh, register every pair in both directions — that is 6 calls for 3 nodes.

`-peers http://nodeB:9090,http://nodeC:9090` registers at startup, but only at trust `0.5`. Peers registered at runtime survive until restart, then vanish with the rest of the in-memory state.

Verify the mesh:

```bash
curl -s http://nodeA:9090/peers | jq -r '.[] | "\(.id) trust=\(.trust) \(.url)"'
curl -sf -X POST http://nodeA:9090/recommend -H 'Content-Type: application/json' \
  -d '{"task_type":"pod-scheduling","source_node_id":"master"}' | jq '{PeerID,ExpectedSavings}'
```

Peers below the `-min-trust` floor (default `0.5`) are excluded from recommendations entirely. `poc/scripts/05-peers.sh` is a working full-mesh registration script if you want to copy the pattern.

---

## 8. Upgrade and rollback

There is no in-place upgrade and no state migration, because there is no persisted state.

```bash
sudo systemctl stop di-agent
sudo install -m 0755 di-agent-arm64-new /usr/local/bin/di-agent
sudo systemctl start di-agent
curl -s http://$ADDR/version | jq -r .agent_version    # confirm
```

Rollback is the same procedure with the old binary. Keep the previous one around.

**Plan for the state loss.** Without `-state-file` every upgrade returns the graph to cold start — every relationship reporting `unknown` — and the agent needs `-convergence` *paired* observations per relationship to re-converge (500 at default, ~40 minutes at a 5 s collect interval). Upgrade during a period when degraded decision quality is acceptable, and stagger across nodes rather than restarting a whole mesh at once.

If you need the learned state, snapshot it before stopping — `GET /graph` returns everything, and it is a reasonable audit record even though the agent cannot reload it:

```bash
curl -s http://$ADDR/graph > graph-$(date +%Y%m%dT%H%M%S).json
curl -s "http://$ADDR/history" > history-$(date +%Y%m%dT%H%M%S).json
```

---

## 9. What it touches on your system

Full inventory, so you can reason about it before deploying.

### Reads

| Path / endpoint | When | Why |
| --------------- | ---- | --- |
| `/sys/fs/cgroup/**` | Every `-collect-interval` | CPU/memory/IO metrics. Read-only. Disable with `-cgroup-root ""`. |
| `$NETDATA_URL/api/v1/data` | Every `-collect-interval` | Host metrics. Disable with `-netdata-url ""`. |
| `-priors` file | Once, at startup | Structure: which propositions to instantiate, in which direction. No magnitudes. |
| `-explain-prompt` files | Once, at startup | Only when the explain layer is enabled. |

### Writes

**Nothing.** No files, no databases, no temp files. The agent is read-only on the filesystem. That is why `ProtectSystem=strict` in [§4](#4-run-under-systemd) works without exceptions.

### Network

| Direction | Target | When |
| --------- | ------ | ---- |
| Inbound | `-addr` (default `:8080`, **all interfaces**) | Always. **Unauthenticated.** |
| Outbound | Netdata at `-netdata-url` | Every collect interval |
| Outbound | Peer agents' `/cost`, `/healthz`, `/offload` | On `/recommend` and offload |
| Outbound | LLM at `-explain-url` | Only if `-explain-provider` is set; default is `none`, so **no LLM traffic by default** |

No telemetry, analytics, or update checks. The agent never contacts anything you have not configured.

### Privileges

None. It needs no root, no capabilities, and no write access. `DynamicUser=yes` works. If your cgroup files are root-only you may need `SupplementaryGroups=` or a read ACL — that is the only case requiring any privilege consideration at all.

---

## 10. Troubleshooting

**`/healthz` fine but `n_observations` stays 0.** The agent is running blind. Three causes, in order of likelihood:

```bash
journalctl -u di-agent | grep "collection loop"
# "collection loop disabled: -collect-interval=0s"  → you disabled it
# "collection loop disabled: no collector"          → both sources are off
# "collection loop started: source=… interval=…"    → loop is up; source is unreachable
```

If the loop is up, test the source directly: `curl -s localhost:19999/api/v1/data?chart=system.cpu | head`. On macOS or a container without cgroups v2, `-cgroup-root ""` is correct and expected — you need Netdata instead.

**Port already in use.** Under k0s, kube-router owns `:8080`. Use `:9090`. Check with `ss -tlnp | grep :8080`.

**A proposition was deprecated and you did not do it.** `GET /history` shows the actor, kind, and timestamp for every ontology mutation. Note that `/graph` still reports 15 edges — deprecation only flips a flag (see [§6](#6-monitor)), so find them with:

```bash
curl -s http://$ADDR/graph | jq -r '.edges[] | select(.deprecated == true) | .proposition_id'
```

`POST /agent/reset` discards what a relationship learned — both layers cleared, confidence to zero, pair window emptied, the discard journalled — but does **not** un-deprecate — there is no un-deprecate operation by design, since the audit trail must stay honest. Restart the agent for a clean backbone (and accept the state loss).

**Confidence stuck at ~0.60.** Expected and correct. See [§6](#6-monitor).

**`ResourceCost` is 0 or negative under heavy load.** This was a real effect of the old cost function, which accumulated `(effective − prior) × sign(direction)`, so driving CPU far above the calibrated priors let negative-direction contributions cancel positive ones. **It no longer applies.** The estimate now leads with the observed *level* of the construct, so `ResourceCost` tracks utilization directly and rises under load. The relation sum is reported separately as a per-unit sensitivity and can legitimately be negative or zero — that is a slope, not a cost.

**Confidence stays at 0 no matter how long the agent runs.** Check that *both* endpoints of a relationship are varying, not just present. A relationship advances on a *pair*, and a correlation over a constant series is undefined, so the estimator declines to move on it rather than reporting a coefficient it did not measure. A well-provisioned control-plane host whose stall-pressure reads identically zero will run for hours and learn nothing — correctly. `GET /state` reports `n_unknown` beside the mean confidence for exactly this case.

**A relationship shows strength 0 with a high `sign_conflicts` count.** That is not a quiet system; it is a wrong declaration. The machine is producing the opposite sign to the one the specification declares, every pair is being rejected at the gate, and `sign_suspect` flags it past 30 pairs at 60% conflict. Fix the declared direction or the route's polarity in `domain_spec.json` — do not wait for it to converge, because it cannot.

**`/recommend` returns no peer.** Check, in order: peers registered (`GET /peers`), trust at or above `-min-trust` (default `0.5`), peers reachable (`curl` their `/healthz` from this node), and that at least one peer has a *lower* cost than local — a recommendation requires positive savings.

**`POST /explain` returns 501.** Working as intended. `-explain-provider` defaults to `none`.

**Learned state vanished.** The process restarted. Check `systemctl show -p NRestarts di-agent`. All state is in memory; see [§8](#8-upgrade-and-rollback).

**Everything looks right but decisions seem wrong.** `-kd` is no longer a candidate: it seeds no magnitude, so a mismatch mislabels the record and changes no answer. Check instead what the agent is actually reading — `GET /edges` gives `basis` per relationship, and `basis: unknown` on a relationship you expected to be learned means no pairs have formed. `GET /state/decisions/{id}` reproduces the inputs of any cost answer.

---

## Where to read next

| You want | Read |
| -------- | ---- |
| Endpoint and flag reference | [README §2](semantic-map/README.md#2-the-agent-api), [§3](semantic-map/README.md#3-running-the-edge-daemon) |
| Why state is in-memory, why no auth | [ARCHITECTURE §3](semantic-map/ARCHITECTURE.md#3-deployment-profiles), [§10](semantic-map/ARCHITECTURE.md#10-coordination) |
| A worked 3-node deployment | [ARCHITECTURE §12](semantic-map/ARCHITECTURE.md#12-poc-deployment-poc) and `poc/` |
| To change or extend the code | [`DEVELOPING.md`](DEVELOPING.md) |
