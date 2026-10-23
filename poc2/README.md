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

## What this PoC deploys

The deployment consists of two separate layers:

1. Infrastructure and cluster layer
   - VM creation via Terraform in `main.tf`
   - Kubernetes bootstrap via `scripts/02-k8s.sh`
   - one cluster for the VM fleet

2. Application and demo layer
   - Helm chart in `helm/di-agent-system` deploys Kafka, PostgreSQL, InfluxDB, Grafana, replay-streamer, telemetry-writer, and dataset-collector; legacy simulators and the playground are disabled by default
   - `scripts/03-agent.sh` builds and deploys the Go `di-agent` binary as one pod per worker VM
   - `scripts/04-peers.sh` registers each agent as a peer of the others and sets trust values
   - `scripts/coordinator.sh` runs the trust-drain/routing experiment

The default `make all` workflow deploys the multi-node `di-agent` fleet separately, one pod per worker VM, to model host-level peer nodes directly on the VM network. `agent.enabled=true` in Helm is an alternative single-agent deployment; do not enable it together with `make agent`.

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

The `dataset-collector` consumes `dataset.telemetry` and exposes di-agent's HTTP collector protocol. The first agent deployed by `make agent` is configured to poll it; the other agents collect their local cgroup metrics as before. Alternatively, run a single Helm-managed agent with `make helm-install HELM_ARGS="--set agent.enabled=true"` and do not run `make agent`.

The collector averages all new Kafka values for each run and signal over one agent collection window, then returns one sample per run/signal. The default agent poll interval is 5s (`AGENT_COLLECT_INTERVAL` for `make agent`, or `agent.collectInterval` for the Helm-managed agent): a 1-step/s stream is averaged over the values received in those 5 seconds; faster streams contribute more values, and a signal with no new events is omitted. The collector adapts to the actual Kafka arrivals, so no rate setting needs to be synchronized with the streamer. It emits every signal as `metric_type=<signal>`, `subject=engine:<run_id>`, `unit=unknown`, and no range. Source and `origin` are labels. These are intentionally unrouted in `semantic-map/domain_spec.json`; the agent accepts and records them as scoped properties but does not summarize them into constructs. The existing `RC` and `PS` constructs describe host resource cost and pressure, not engine signals with unknown physical units.

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
make helm-install
make agent
make peers
make demo
```

That sequence does the following:

1. `make provision` creates the libvirt VMs.
2. `make k8s` bootstraps the cluster on the VMs.
3. `make images` builds and pushes the workload images used by the Helm chart.
4. `make helm-install` deploys Kafka, PostgreSQL, InfluxDB, Grafana, replay-streamer, telemetry-writer, and dataset-collector. Legacy simulations and playground remain off by default.
5. `make agent` builds the Go agent binary and imports the image into each worker node; it then deploys one `di-agent` pod per VM.
6. `make peers` registers each agent as a peer on the others and assigns trust values.
7. `make demo` executes the coordinator loop that probes `/cost`, inspects recommendations, and lowers trust to show rerouting.

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
- `scripts/03-agent.sh`: builds and deploys the `di-agent` pods
- `scripts/04-peers.sh`: registers peers and trust values
- `scripts/coordinator.sh`: runs the trust-based recommendation demo
- `scripts/build-push-images.sh`: builds/pushes the runtime service images
- `helm/di-agent-system/values.yaml`: all chart defaults and required secrets
- `helm/di-agent-system/templates/`: Kubernetes manifests for Kafka, InfluxDB, Grafana, data services, and the optional single Helm-managed agent
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
