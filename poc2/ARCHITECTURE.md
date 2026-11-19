# PoC2 Architecture

## Purpose

PoC2 is a local distributed lab for testing the `di-agent` idea in a controllable environment. The goal is not to deploy a production cluster; it is to let several agent instances run on separate VM nodes, observe different workloads, exchange peer trust, and demonstrate how routing decisions change when one peer becomes less trusted.

This architecture is deliberately layered so each part of the system remains observable:

- VM infrastructure creates the cluster nodes,
- Kubernetes schedules the service stack,
- the telemetry pipeline replays real or profile-matched synthetic PostgreSQL data into InfluxDB,
- the `di-agent` process runs on each VM as a peer node,
- the demo coordinator exercises trust-aware routing behavior.

## High-level architecture

The system has five layers:

1. Infrastructure layer
   - libvirt/QEMU VM fleet created by Terraform
   - Ubuntu cloud image and networking from the libvirt default network

2. Cluster layer
   - Kubernetes control plane + worker nodes
   - cluster bootstrap done by `scripts/02-k8s.sh`

3. Runtime service layer
   - Kafka for event transport
   - InfluxDB for time-series storage
   - Grafana for dashboards
   - replay-streamer, PostgreSQL, Kafka, and telemetry-writer; the legacy physics simulators are opt-in

4. Agent layer
   - one `di-agent` pod per worker VM
   - direct peer-to-peer communication over VM IP addresses
   - trust registration and routing recommendation APIs exposed by the agent

5. Demo and observation layer
   - `scripts/coordinator.sh` probes `/cost` and `/recommend`
   - trust values are intentionally reduced to simulate degraded peers
   - the result is observable rerouting behavior

## Deployment model

PoC2 uses a two-part deployment strategy:

### 1) Helm chart for the system services

The chart under `helm/di-agent-system` manages the worker agent DaemonSet and shared runtime services:

- Kafka
- PostgreSQL engine-test dataset
- InfluxDB
- Grafana
- replay-streamer (real or synthetic data mode)
- dataset-collector (Kafka to di-agent HTTP collector protocol)
- simulation-collector (optional legacy device snapshots to the same HTTP protocol)
- telemetry-writer

Genset, battery, propulsion, auxload, shore-power, switchboard, simulation-collector, and playground deployments are disabled by default and can be enabled individually through Helm values.

The replay-streamer reads every source (`short` and `long_*`) of the selected `run_id` from PostgreSQL, merged by source time `t` (all sources share one time grid; shorter ones end first). In `real` mode it streams stored rows; in `synthetic` mode it uses that same run's per-signal min/max/mean/standard deviation to generate reproducible, correlated samples and carries the associated run metadata in each message. Both modes publish to `dataset.telemetry`, which telemetry-writer stores in the InfluxDB `dataset_timeseries` measurement. The HTTP control API at `replay-streamer:8000` exposes `GET /status` and `PUT /config`; `interval_ms` and `points_per_interval` (time steps per tick; default 1 step per 1000 ms) control the emission rate.

The dataset collector consumes `dataset.telemetry` and implements the agent's HTTP collector protocol. `agent.datasetNodeName` designates one worker whose agent polls it; empty disables dataset ingestion. All agents collect local host cgroup metrics. A shared collector drains its aggregates on each poll, so only the designated agent connects to it.

The collector averages all new Kafka values for each `(run_id, signal)` during one agent collection window, returning one sample per key. Its default source polling interval is 5s, independent of the streamer's interval and points-per-tick: faster streams simply contribute more samples to that window, and signals with no new events are omitted. Events provide per-signal units and ranges as `metadata.signals` JSON keyed by signal, for example `{"signals":{"speed":{"unit":"rpm","range":[0,6000]}}}`. Replay-streamer preserves explicit declarations from PostgreSQL; when absent in the supplied dataset, both real and synthetic modes preserve raw values and declare `dataset-native` units with observed min/max ranges pooled across sources. Constant signals receive symmetric padding of 1% (at least 1 unit). Generated metadata records its provenance and is logged, without inventing physical units or changing database rows. The collector forwards those values as `unit` and `range`, with source and origin labels. Since these are engine signals rather than host resource/pressure measurements, they remain unrouted in `semantic-map/domain_spec.json`: the agent records them as scoped properties, but no existing construct summarizes them. Replay-streamer also exposes HTTP instrumentation at `/metrics` for Prometheus.

### 2) Helm-managed worker agent mesh

`helm/di-agent-system/templates/agent.yaml` deploys the `di-agent` runtime:

- `scripts/build-push-images.sh` builds the Go binary and publishes the image,
- a DaemonSet runs one pod on each eligible worker, excluding both control-plane role labels,
- `NODE_ID` comes from the pod's assigned node name,
- the host cgroup tree is mounted read-only,
- `hostNetwork: true` preserves individually addressable worker endpoints,
- `scripts/04-peers.sh` discovers ready agent pods and registers their host-IP endpoints.

The agent mesh and telemetry services now share one Helm lifecycle. Three agents require three eligible workers in addition to the control plane. Legacy agent Deployments must be removed before migration to avoid host-port conflicts.

## Infrastructure architecture

`main.tf` creates the VM fleet. The important responsibilities are:

- create a libvirt storage pool
- download the Ubuntu 22.04 cloud image
- clone VM disks based on that image
- inject cloud-init data for SSH and hostname configuration
- create one libvirt domain per VM
- attach each VM to the default libvirt network
- give the first VM a slightly higher resource allocation so it acts as the control plane

The cluster is intentionally small and local. There is no production HA or external managed service layer.

## Cluster architecture

`scripts/02-k8s.sh` builds the Kubernetes cluster. The pattern is straightforward:

- resolve each VM IP via `virsh`
- install required packages and kernel settings
- initialize the control plane on the first VM
- install the pod network
- join remaining VMs as workers
- write a local kubeconfig for `kubectl`

The design assumption is simple: control-plane + workers on a local VM network, no external load balancer, no external storage, and no complicated service mesh.

## Runtime service interaction

The data plane is implemented as a classic event stream plus time-series store:

```text
workload generators -> Kafka -> telemetry-writer -> InfluxDB -> Grafana
```

### Kafka

Kafka is a single-broker KRaft deployment. It serves as the system event backbone. The Helm templates in `helm/di-agent-system/templates/kafka.yaml` define the service and broker configuration. The relevant design point is that all application services talk to it by cluster DNS name:

- `kafka.<namespace>.svc.cluster.local:9092`

### Workload simulators

The services in `system/` model power-system components rather than generic application load:

- `genset`: source of electrical generation
- `battery`: source/storage device
- `propulsion`: power consumer
- `auxiliary-load`: another power consumer
- `switchboard`: central allocation authority

These components generate and consume telemetry. The switchboard reads genset and battery supply from Modbus, ignores stale source readings and consumer requests, and allocates available power by descending consumer priority. Higher-priority consumers are served first; if supply is insufficient, lower-priority consumers receive partial or zero allocations (load shedding).

### Telemetry writer

`system/telemetry-writer` consumes the Kafka topics and maps them to InfluxDB measurements. This bridge is essential because the simulation services publish machine-readable events, while Grafana expects a time-series database.

The writer commits Kafka offsets only after a successful InfluxDB write. Malformed records are committed as intentionally dropped when `STRICT_VALIDATION=false`; with strict validation enabled, the writer exits without committing the invalid record so the failure is visible. The chart's Prometheus service scrapes annotated application `/metrics` endpoints, including telemetry and HTTP instrumentation; Grafana dashboards can query those metrics alongside InfluxDB telemetry.

### InfluxDB and Grafana

The chart provisions InfluxDB and Grafana and preconfigures Grafana datasources for InfluxDB telemetry and Prometheus application metrics. This allows the operator to observe system health and telemetry without separate manual config.

The practical requirement is simple: the InfluxDB credentials must exist before `helm install` or the chart will fail to initialize the datasource.

## Agent and trust architecture

The `di-agent` layer is the heart of the experiment.

### Agent placement

Each eligible worker hosts one DaemonSet agent pod. The pod runs with `hostNetwork: true`; node affinity excludes control-plane/master nodes, and optional node selectors narrow the worker set. This lets the agent:

- interact with local runtime configuration,
- reach other agent nodes directly over the VM network,
- behave as a peer endpoint rather than a normal in-cluster service.

### Peer registration

`scripts/04-peers.sh` lists ready agent pods for the Helm release through kubectl. It executes HTTP requests inside each target pod, registering every other worker's host IP and configured HTTP port, then sets trust to 0.8. The result is a dense mesh where each agent knows all the others; three agents have two peers each. It checks the exact resulting peer sets and fails visibly on missing, duplicate, unready or stale endpoints. Re-run registration after agent restarts or topology changes; this container configuration does not persist peers.

This is important because the trust model is not implicit. It is explicit, observable, and adjustable.

### Routing experiment

`scripts/coordinator.sh` is the demonstration layer. It repeatedly:

- queries `/cost` from each agent,
- selects the highest-cost node,
- calls `/recommend` on that node,
- evaluates the recommendation,
- reduces trust for one peer mid-run,
- observes how the routing recommendation changes as trust falls.

This is the actual proof-of-concept behavior: the system is not merely measuring load; it is showing that trust-aware routing can redirect decisions when a peer becomes unreliable.

## Dependency and configuration boundaries

This project has a few hard boundaries that matter in practice:

- Terraform is only responsible for VM creation.
- Kubernetes bootstrap is separate from application deployment.
- The custom app images are built and pushed to a registry before the Helm chart is installed.
- The `di-agent` binary is built from the `semantic-map` Go sources and deployed by Helm.
- The telemetry service credentials are required before chart deployment.

The scripts support kubeadm (with Flannel), k3s, and a KubeEdge CloudCore/EdgeCore deployment path. These boundaries keep the lab understandable: one step creates the nodes, one step creates the cluster, one step deploys services, and one step deploys the peer agent mesh.

The chart configures each pod with its node identity, regime, collection interval and host cgroup root. The entrypoint independently enables the dataset and simulation HTTP collectors only on their designated workers (`DATASET_NODE_NAME` and `SIMULATION_NODE_NAME`). The Makefile exposes both names, `SIMULATION_COLLECTOR`, and `AGENT_COLLECT_INTERVAL`; peer registration checks that each nonempty selection names a ready agent.

The optional simulation collector consumes the six legacy telemetry topics in its
own Kafka group, independently of telemetry-writer. It returns the latest fresh
snapshot per asset once, preserving the source timestamp in Unix seconds; it
does not average or re-stamp stale observations. Same-second updates after a poll
are suppressed to preserve deterministic event identity. Snapshots expire after
15 seconds by default, and storage is bounded by `maxActiveSeries`. Restarting
the collector can lose unpolled snapshots: this is a live observation adapter,
not a durable history store; InfluxDB remains the history path.

Metric types are namespaced (`genset.power_kw`, `battery.soc`), with asset subjects
such as `genset:genset-1`. Switchboard totals are emitted once per board snapshot,
while allocations use `allocation:<board-id>:<consumer-id>` subjects. Static
declarations provide physical units and fixed ranges only for bounded quantities;
signed battery power is preserved. Curated metric defaults can be replaced with
`simulationCollector.include` patterns, including `["*"]` for all known fields.
These properties remain unrouted in the existing host-resource domain model.

## Execution flow

The real operational flow is:

```text
Terraform -> VM fleet
  -> kubeadm/k3s/KubeEdge cluster
  -> image build + registry push
  -> Helm chart deployment (services + worker agent DaemonSet)
  -> peer registration and trust assignment
  -> workload telemetry into Kafka
  -> telemetry writer to InfluxDB
  -> Grafana dashboarding
  -> coordinator trust-drain demonstration
```

## Why this architecture is valid for the PoC

This design is intentionally simple enough to debug. Every critical behavior remains visible:

- the machines are explicit VM nodes,
- Kubernetes handles service scheduling,
- the agent mesh is direct and host-level,
- telemetry is streamed through Kafka,
- trust changes are made visible through a coordinator loop.

That makes this PoC valuable as a local testbed for distributed trust-aware routing logic without making the runtime opaque.

## Key files to understand the architecture

- `main.tf`: VM lifecycle and node shape
- `scripts/02-k8s.sh`: cluster construction
- `helm/di-agent-system/templates/agent.yaml`: per-worker agent deployment
- `scripts/04-peers.sh`: peer + trust registration
- `scripts/coordinator.sh`: trust-aware routing experiment
- `helm/di-agent-system/values.yaml`: runtime service defaults and secret references
- `helm/di-agent-system/templates/`: deployed workloads and their wiring
- `system/*`: generators and telemetry consumers

## Operational caveat

This is a lab environment, not a production cluster. The service network is intentionally direct, host networking is used for the agent mesh, and the VM setup assumes a small local libvirt environment with a default network and known SSH key paths.

That is the correct architecture for PoC2: simple enough to reproduce, explicit enough to debug, and close enough to the target coordination problem to test the behavior that matters.
