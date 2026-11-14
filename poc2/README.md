# PoC2: local di-agent lab on libvirt VMs

This directory is the second proof-of-concept for the di-agent project: a small Kubernetes lab that runs on libvirt virtual machines, with a distributed telemetry stack and a trust-based coordination experiment.

The important thing to understand is that PoC2 is not just a cluster bootstrap. It is a reproducible environment for testing a specific thesis:

- multiple agent instances run on different machines,
- each node observes a different workload pattern,
- the nodes exchange peer trust information,
- a coordinator observes how routing recommendations change when trust drops.

## What is actually required to run this PoC

This is the minimal working stack the project expects:

- Linux host with libvirt + KVM + QEMU enabled
- Terraform installed and configured for libvirt
- Docker for building the custom images used by the app stack
- Helm 3 installed
- kubectl and a valid kubeconfig after cluster bootstrap
- SSH access to the VMs, using the key at `$HOME/.ssh/id_ed25519_vms`
- a container registry for the chart-managed services (`ghcr.io/...` by default)
- access to Ubuntu cloud images so Terraform can create VM disks

The default topology is simple and intentional:

- `ubuntu-vm1` = Kubernetes control plane
- `ubuntu-vm2`, `ubuntu-vm3`, ... = worker nodes

The first VM in the list is the control plane; all remaining VMs are treated as workers.

VM root disks default to 30 GiB (`vm_disk_size_gib`). Kubernetes local-path PVC
sizes do not reserve or enlarge the underlying disk: PostgreSQL data and container
images can exhaust an older 15 GiB VM. Deployment checks for node `DiskPressure`
before running Helm. Restore capacity rather than deleting database PVCs or
disabling eviction thresholds. Existing VMs need an explicit disk, partition and
filesystem expansion; changing the Terraform default alone does not grow their
mounted root filesystem. Review Terraform plans before applying to an existing lab.

If Helm reports a failed `postgres-ingest` hook, inspect its pod logs with
`kubectl logs -l app=postgres-ingest -c ingest --tail=50`. Failed attempts are
retained for diagnosis until the next upgrade recreates the hook. `DATA_URL`
must resolve anonymously to a SharePoint folder with an `id` query parameter;
an HTTP 200 sharing/error page alone does not prove access. The Kubernetes
`data-url` Secret is independent of the local `.env`; keep it updated when the
working share link changes. Never commit or print private share links.

## What this PoC deploys

The deployment consists of two separate layers:

1. Infrastructure and cluster layer
   - VM creation via Terraform in `main.tf`
   - Kubernetes bootstrap via `scripts/02-k8s.sh`
   - one cluster for the VM fleet

2. Application and demo layer
   - Helm chart in `helm/di-agent-system` deploys Kafka, PostgreSQL, InfluxDB, Grafana, replay-streamer, telemetry-writer, and dataset-collector; legacy simulators and the playground are disabled by default
   - the Helm chart deploys the Go `di-agent` as a DaemonSet, one pod per worker node
   - `scripts/04-peers.sh` discovers the Helm agent pods through kubectl, registers peers and sets trust values
   - `scripts/coordinator.sh` runs the trust-drain/routing experiment

The default `make all` workflow deploys the entire stack through Helm, including the worker agent DaemonSet (`agent.enabled=true`). Nodes labeled `node-role.kubernetes.io/control-plane` or `node-role.kubernetes.io/master` are excluded. Three eligible worker nodes produce three agents; the control plane is additional. `agent.nodeSelector` and `global.nodeSelector` can further restrict placement. Agents use host networking and listen on each worker's port 9090 by default.

When migrating, remove the old Helm Deployment `di-agent`, if present, and the script-created Deployments named `di-agent-<worker>` before installing the DaemonSet. List them with `kubectl get deployments -l app=di-agent`, then delete only those legacy Deployments by name. Do not run the old deployment script alongside the DaemonSet: both would claim the same node identity and host port.

## Required secrets and configuration

Before the chart is installed, the chart expects a namespace and credentials for the telemetry services.

```bash
kubectl create namespace default --dry-run=client -o yaml | kubectl apply -f -
kubectl -n default create secret generic influxdb-credentials \
  --from-literal=admin-user="$INFLUXDB_ADMIN_USER" \
  --from-literal=admin-password="$INFLUXDB_ADMIN_PASSWORD" \
  --from-literal=admin-token="$INFLUXDB_ADMIN_TOKEN"
kubectl -n default create secret generic grafana-credentials \
  --from-literal=admin-user="$GRAFANA_ADMIN_USER" \
  --from-literal=admin-password="$GRAFANA_ADMIN_PASSWORD"
```

This is required because `helm/di-agent-system/values.yaml` references:

- `influxdb.existingSecret: influxdb-credentials`
- `grafana.existingSecret: grafana-credentials`

### PostgreSQL engine dataset

The local `data/` directory and the existing `DATA_URL` share include the added
`IMEPn1` through `IMEPn4`, `MFB50_4`, `PMax4`, and `PowerCyl4` signal CSVs.
Each file contains its `Data1_<signal>` value column alongside the matching
`Data1_time_<signal>` column. Ingestion discovers new files on every Helm
install/upgrade and leaves unchanged, already-loaded files alone. No signal
allowlist or new DATA_URL is required. The `_0001` and `_0001_2` run IDs stay
separate; select the desired run with `streamer.runId` or the control API.
After loading and committing the files, the Helm ingestion job sends an empty
`PUT /config` to replay-streamer to reload its PostgreSQL profiles. This keeps
both modes current even when the streamer started before the new files were
ingested, without changing its configuration. A failed refresh fails the job
visibly and is retried by Kubernetes. Standalone ingestion can opt into the same
behavior with `STREAMER_CONTROL_URL=http://replay-streamer:8000/config`.
The unchanged di-agent contract requires stable units and ranges per signal
name for the lifetime of its collector. Run one dataset at a time and restart
the agent when changing run IDs: the same signal can have different observed
bounds in another run. Stop the previous stream and let its queued Kafka events
and pending collector aggregates drain before switching, so the restarted
agent receives only the new run. Signal names, raw values and per-run bounds
remain unchanged; no changes to `semantic-map/` are required.

The chart runs a stock `postgres` StatefulSet and a `postgres-ingest` Helm hook Job (image `data-ingest`, built from `poc2/data-ingest`) that downloads the CSVs from SharePoint and `COPY`s them into tables `runs`, `run_metadata` and `timeseries` (long format: `run_id, source, signal, t, value`). Re-running is idempotent (files already loaded are skipped). The CSVs are not stored in git or in any image.

```bash
kubectl -n default create secret generic postgres-credentials \
  --from-literal=username=di --from-literal=password="$POSTGRES_PASSWORD"
kubectl -n default create secret generic data-url --from-literal=data-url="$DATA_URL"
helm upgrade --install di-agent-system poc2/helm/di-agent-system --wait --timeout 30m
```

Connect in-cluster at `postgres:5432`, database `engine`. Set `postgres.enabled=false` to skip it.

### PostgreSQL replay and simulation modes

The streamer defaults to `synthetic` mode, generating bounded, correlated samples from the selected run's measured signal profiles and carrying its `run_metadata` in each message. Set `streamer.mode=real` to replay the actual stored samples. Both modes publish generic engine signals to `dataset.telemetry`; telemetry-writer writes them to InfluxDB's `dataset_timeseries` measurement with `run_id`, `source`, `signal`, and `origin` tags. If PostgreSQL data is unavailable, the streamer reports the error through `/status` and retries; it never silently substitutes synthetic values for a requested real stream.

The `dataset-collector` consumes `dataset.telemetry` and exposes di-agent's HTTP collector protocol. Set `agent.datasetNodeName` to one worker's Kubernetes node name (or use `make agent DATASET_NODE_NAME=ubuntu-vm2`) to designate the only agent that polls it. Empty means engine ingestion is disabled. Every agent still monitors its own host cgroups. Do not point multiple agents at the shared collector: each HTTP poll drains the pending aggregates, so they would split the data rather than receive copies. `make peers` rejects a configured dataset node that is absent from the ready agent fleet.

The collector averages all new Kafka values for each run and signal over one agent collection window, then returns one sample per run/signal. The default agent poll interval is 5s (`AGENT_COLLECT_INTERVAL` in Make, or `agent.collectInterval` in Helm): a 1-step/s stream is averaged over the values received in those 5 seconds; faster streams contribute more values, and a signal with no new events is omitted. The collector adapts to the actual Kafka arrivals, so no rate setting needs to be synchronized with the streamer. Each event must include per-signal `unit` and `[min, max]` `range` values under `metadata.signals`; for example, `{"signals":{"speed":{"unit":"rpm","range":[0,6000]}}}`. The collector forwards these as `unit` and `range`, with `metric_type=<signal>` and `subject=engine:<run_id>`. Source and `origin` are labels. These are intentionally unrouted in `semantic-map/domain_spec.json`; the agent accepts and records them as scoped properties but does not summarize them into constructs. The existing `RC` and `PS` constructs describe host resource cost and pressure, not engine signals.

On startup, only the designated dataset worker waits in an init container for the
collector's `/ready` endpoint before starting the agent's collector handshake.
Other workers start without that dependency. If Kafka or the collector is unavailable,
the designated pod stays initializing and logs its wait instead of crash-looping.
The replay streamer has a five-minute startup-probe budget for its Kafka connection
retry loop before liveness checks begin. These are chart-level settings; no image
rebuild is required to apply them.

The supplied PostgreSQL dataset has engine/run metadata, but no per-signal physical units or operating ranges. In both stream modes, replay-streamer preserves raw values and supplements each event's metadata with `unit: "dataset-native"` and observed min/max bounds pooled across all sources for that signal in the selected run. These are normalization bounds, not claimed physical operating limits. Constant signals get symmetric padding of 1% of their absolute value (at least 1 dataset-native unit) to keep the range nonzero. Generated declarations carry `unit_source: "dataset-native"` and `range_source: "observed"` and are logged. Explicit units and ranges in `run_metadata` take precedence, including JSON `signals` and flattened `<signal>.unit`/`<signal>.range` keys; invalid explicit declarations fail visibly rather than being replaced. PostgreSQL rows are not modified.

The chart's Prometheus service scrapes annotated application `/metrics` endpoints. The telemetry writer commits Kafka offsets only after successful InfluxDB writes; malformed records are fatal and left uncommitted with `STRICT_VALIDATION=true`, while `false` logs and drops them before committing.

Cloud sync commits only the processed record's offset after delivery or an explicitly logged permanent rejection. Transient errors are retried without advancing offsets, and records fetched later in the same Kafka batch remain uncommitted until processed.

```bash
make images IMAGES="replay-streamer telemetry-writer data-ingest dataset-collector"
make helm-install
# Select real samples instead:
make helm-install STREAMER_MODE=real
# Real samples, 5 time steps every 500 ms:
make helm-install STREAMER_MODE=real STREAMER_INTERVAL_MS=500 STREAMER_POINTS=5
# Any other chart value, e.g. enable the legacy genset simulator:
make helm-install HELM_ARGS="--set genset.enabled=true"
# Change emission rate while running:
kubectl port-forward svc/replay-streamer 8000:8000
curl -X PUT http://localhost:8000/config \
  -H 'Content-Type: application/json' \
  -d '{"interval_ms":500,"points_per_interval":5}'
```

#### Streamer control API (runtime changes)

The streamer exposes `replay-streamer:8000` inside the cluster (use `kubectl port-forward svc/replay-streamer 8000:8000` from your machine).

| Endpoint | Purpose |
|---|---|
| `GET /health` | Liveness (503 only if the worker thread died) |
| `GET /status` | Active config, `state` (`running`/`error`), `published` count, `last_error` |
| `GET /metrics` | Prometheus HTTP request metrics, scraped by the Helm deployment |
| `PUT /config` | Partial update; send only the fields you want to change |

`PUT /config` fields (all optional): `mode` (`real`\|`synthetic`), `run_id`, `interval_ms` (10–3,600,000), `points_per_interval` (1–10,000), `loop` (bool), `random_seed` (int, synthetic only). A "point" is one time step: every signal still active at that step emits one sample, so samples per tick = `points_per_interval` × active signals. Defaults: 1 step every 1000 ms. Changes apply within ~0.25 s without a restart.

```bash
# Switch to another run, in real mode
curl -X PUT localhost:8000/config -H 'Content-Type: application/json' \
  -d '{"mode":"real","run_id":"1000rpm_150kW_0001_2"}'
# Pause-like slow replay: 1 point every 10 s
curl -X PUT localhost:8000/config -H 'Content-Type: application/json' \
  -d '{"interval_ms":10000,"points_per_interval":1}'
curl localhost:8000/status
```

API changes are in memory only: a pod restart or a new `make helm-install` returns to the Helm/`STREAMER_*` values. Use those for permanent settings.

#### Engine dataset playground

The optional playground includes an Engine Dataset panel for the configured
run. It can switch between synthetic generation and **recorded real-data
replay**, adjust pacing and looping, and set a synthetic seed. It does not
control a physical engine, provide pause/seek, or switch run IDs. Each apply
restarts the shared stream from the beginning, and switching modes can mix
origins in an agent collection window. The panel displays streamer and
collector status separately; collector readiness does not prove that the agent
is ingesting data.

Enable the panel with `playground.enabled=true` and
`playground.dataset.enabled=true`. For example:

```bash
# Build and push the updated playground to the registry/tag used by your chart.
make -C poc2 images IMAGES=playground REGISTRY=<your-registry> TAG=<your-tag>
helm upgrade --install di-agent-system poc2/helm/di-agent-system --reuse-values \
  --set playground.image.tag=<your-tag> \
  --set playground.enabled=true \
  --set playground.dataset.enabled=true
kubectl port-forward svc/playground 8080:80
kubectl port-forward svc/grafana 3000:3000
```

Open `http://localhost:8080`. Dataset settings can be changed without a
password by anyone who can reach the playground; changes affect the shared
stream. The playground NodePort and the in-cluster replay-streamer are not
authenticated. Restrict access to trusted developers and networks, or use an
authenticated TLS ingress. The local Vite development proxy is also
unauthenticated and must not be exposed to untrusted networks.

Grafana provisions an **Engine dataset telemetry** dashboard at
`http://localhost:3000/d/engine-dataset` when port-forwarded as above. It
filters by run, signal, source and origin and plots raw pipeline values
aggregated into display windows against emission time. Set
`playground.dataset.grafanaDashboardUrl` to a browser-accessible dashboard URL
to show a link in the panel. The agent's collection-window means are separate
from Grafana's display aggregation. Configure `agent.datasetNodeName` to enable
ingestion by one agent, and inspect that agent's UI to verify its properties.
See [playground/README.md](playground/README.md) for local development and tests.

`streamer.runId` selects the dataset (default `1000rpm_150kW_0001`). All of the run's sources (`short` and every `long_*` file) are streamed together: they share the same time grid (`t` steps of 7200 from 0), so each tick emits the next `pointsPerInterval` time steps for every signal, and shorter sources simply end before the longer ones (the run then loops if `loop=true`).

At ingest, each multi-signal CSV is split into one file per signal next to it in `data/` (`<run>_timeseries_<source>_<signal>.csv`, containing only `Data1_<signal>` and `Data1_time_<signal>`); the split files are what is loaded into PostgreSQL. Files that are already split can be placed in `data/` (or the `DATA_URL` share) as-is and are loaded directly. Run `DATA_DIR=poc2/data python3 poc2/data-ingest/ingest.py --split-only` to produce them locally without a database. To disable streaming entirely, set `streamer.enabled=false`. The legacy genset/battery/propulsion/etc. simulators and playground can be enabled individually with their respective `*.enabled=true` values; they all default to false.

### Optional: cloud eventual-consistency sync

`telemetryCloudSync` mirrors telemetry Kafka topics to Azure (Event Hubs or IoT Hub), and `telemetryWriterCloud` is a second telemetry-writer that reads them back out of Event Hubs into a cloud-hosted InfluxDB, so the cloud store eventually converges with the edge one. Both are disabled by default. To enable them:

```bash
kubectl -n default create secret generic eventhub-credentials \
  --from-literal=connection-string="$EVENTHUB_CONNECTION_STRING"
kubectl -n default create secret generic influxdb-cloud-credentials \
  --from-literal=admin-token="$INFLUXDB_CLOUD_ADMIN_TOKEN"
```

then set `telemetryCloudSync.enabled=true`, `telemetryCloudSync.target=eventhub`, `telemetryWriterCloud.enabled=true`, and `telemetryWriterCloud.influxdb.url` to your cloud InfluxDB endpoint. The optional IoT Hub configuration also needs a `dataset-connection-string` key for the replay topic.

You should also set the registry and tag before installing the chart if you are not using the defaults:

```bash
make images REGISTRY=ghcr.io/your-org TAG=v1
make helm-install REGISTRY=ghcr.io/your-org TAG=v1
```

The chart also expects the enabled service build contexts under `system/` and `playground/` to exist and to be buildable with Docker.

## End-to-end workflow

Use this sequence from this directory:

```bash
make provision
make k8s
make images
make agent DATASET_NODE_NAME=ubuntu-vm2
make peers
make demo
```

That sequence does the following:

1. `make provision` creates the libvirt VMs.
2. `make k8s` bootstraps the cluster on the VMs.
3. `make images` builds and pushes all chart images, including the Go agent.
4. `make agent` installs/upgrades the chart and waits for the worker DaemonSet rollout. It deploys agents together with Kafka, PostgreSQL, InfluxDB, Grafana, replay-streamer, telemetry-writer, and dataset-collector. Legacy simulations and playground remain off by default.
5. `make peers` registers each ready worker agent on the others and assigns trust values. It requires kubectl pod-list/exec permissions and Python 3, not SSH or libvirt discovery. Re-run it after agent restarts, worker additions/removals, or Helm upgrades: the container configuration does not persist the peer registry. The script verifies the exact peer URL set and reports stale or missing entries rather than claiming success.
6. `make demo` executes the coordinator loop that probes `/cost`, inspects recommendations, and lowers trust to show rerouting. This legacy demo still assumes the libvirt VM list starts with the control plane and uses port 9090.

### Testing automatic discovery approval

The chart enables discovery and rules-only review by default:
`agent.proposer: "true"`, `agent.review.level: rules`,
`agent.review.rules: [composition, persistent-correlation]`, and both review
model backends set to `none`. These settings live in
[`values.yaml`](helm/di-agent-system/values.yaml). The container entrypoint writes
its configuration from the chart's environment variables; it does **not** load
[`semantic-map/agent.yaml`](../semantic-map/agent.yaml).

After changing Go code or the entrypoint, rebuild and push the agent image before
deploying. Other service images do not need rebuilding if already available:

```bash
cd poc2
mise exec go@1.26.1 -- make images IMAGES=di-agent
make agent
# Required when reusing the same image tag (for example latest):
kubectl rollout restart daemonset/di-agent
kubectl rollout status daemonset/di-agent --timeout=5m
make peers
```

Use the same `REGISTRY` and `TAG` overrides for building and deploying if not using
the defaults. `make agent` does not build images; it upgrades the whole chart.
Agent pods roll when their configuration or image reference changes, not on unrelated
service upgrades. When reusing an unchanged tag, explicitly restart the DaemonSet as
above to pull the rebuilt image (`imagePullPolicy: Always`); an immutable new `TAG`
changes the pod template without an extra restart.
Keep any existing deployment overrides, such as `DATASET_NODE_NAME`, on `make agent`.
Changing only chart review settings requires a Helm upgrade, not another image build.

After port-forwarding an agent, verify:

```bash
curl -s localhost:9090/autonomy
curl -s 'localhost:9090/candidates?status=all'
```

Expect autonomy level `rules`, both rule names, and no model names. Candidates expose
`PersistentWindows`, `PersistencePairs` and `PersistenceWindowSize`; the discovery
drawer displays progress toward three qualifying full windows, not three review
cycles. Eligible directed pairs confirm as before. Undirected pairs (including
engine-to-engine signals from the same subject) automatically become association-only
edges without inventing a driver; they are excluded from estimates and actuation.
Missing ranges still prevent persistence from accumulating. See the
[persistence requirements](../semantic-map/README.md#rules-only-persistent-correlation).

The agent mounts `/var/lib/di-agent` from `agent.persistence.hostPath` (default:
`/var/lib/di-agent-v6`) on each worker. This fresh host directory avoids loading
incompatible state left at the previous default; the old directory is left untouched.
State and discovery checkpoints survive pod
replacement on that worker; they are not shared across nodes and do not survive loss
of the node's disk. Fresh evidence resumes only within `map.stale_after` and under
matching discovery settings; stale checkpoints start a new evidence run. An unclean
exit loses at most the last snapshot interval of progress. Existing state held only
inside an old container is not automatically migrated by adding the host mount.
Snapshot format 5 is readable by this version (and imports format 4), but not by older
agent binaries; back up existing state before upgrading if rollback is required.

To view a particular agent, find its pod with `kubectl get pods -l app=di-agent -o wide`, then run `kubectl port-forward pod/<pod-name> 9090:9090`. Open `http://localhost:9090/ui/` (the chart sets `agent.ui: true`; the agent is headless without it). Select the designated dataset worker's pod to see `engine:<run_id>` properties. The shared `di-agent` Service load-balances agents and is not an individual peer identity. To remove a stale peer reported by registration, use that agent's `DELETE /peers/<id>` endpoint, then re-run `make peers`.

## Useful commands

```bash
make status
make list-vms
make demo
make teardown
```

The demo is the proof-of-value. It does the following:

- calls `/cost` on each agent,
- identifies the highest-cost node,
- asks that node for a recommendation,
- reduces trust on one peer mid-run,
- demonstrates that routing changes when trust crosses the effective threshold.

## Important project files

The files that matter for understanding or running the PoC are:

- `main.tf`: libvirt VM definition and disk layout
- `variables.tf`: VM and image configuration
- `providers.tf`: libvirt provider setup
- `scripts/01-provision.sh`: creates the VM fleet
- `scripts/02-k8s.sh`: bootstraps the cluster
- `helm/di-agent-system/templates/agent.yaml`: worker agent DaemonSet and shared Service
- `scripts/04-peers.sh`: registers peers and trust values
- `scripts/coordinator.sh`: runs the trust-based recommendation demo
- `scripts/build-push-images.sh`: builds/pushes the runtime service images
- `helm/di-agent-system/values.yaml`: all chart defaults and required secrets
- `helm/di-agent-system/templates/`: Kubernetes manifests for Kafka, InfluxDB, Grafana, data services, and worker agents
- `system/replay-streamer`: configurable PostgreSQL-to-Kafka real/synthetic data source
- `system/dataset-collector`: Python Kafka-to-di-agent HTTP collector
- `system/*`: legacy workload generators and telemetry services

## What the system is proving

PoC2 is designed to validate a distributed coordination scenario in a controlled local environment:

- nodes start with similar priors,
- local workloads diverge,
- trust values diverge between peers,
- recommendations are no longer purely cost-driven,
- traffic shifts when a previously trusted peer loses trust.

This is an operational test bed for the di-agent concept, not a production-ready cluster design.

## Cleanup

To destroy the VM fleet and associated libvirt resources:

```bash
make teardown
```

This leaves the host in a clean state after the PoC is finished.
