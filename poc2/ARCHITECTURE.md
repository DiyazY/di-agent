# PoC2 Architecture

## Purpose

PoC2 is a local distributed lab for testing the `di-agent` idea in a controllable environment. The goal is not to deploy a production cluster; it is to let several agent instances run on separate VM nodes, observe different workloads, exchange peer trust, and demonstrate how routing decisions change when one peer becomes less trusted.

This architecture is deliberately layered so each part of the system remains observable:

- VM infrastructure creates the cluster nodes,
- Kubernetes schedules the service stack,
- the telemetry pipeline moves workload data into InfluxDB,
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
   - switchboard, genset, battery, propulsion, auxload, and telemetry-writer services

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

The chart under `helm/di-agent-system` manages the shared runtime services:

- Kafka
- InfluxDB
- Grafana
- genset
- battery
- propulsion
- auxload
- switchboard
- telemetry-writer
- playground

The chart wraps all of these in one release but does not include the `di-agent` itself.

### 2) Direct pod deployment for the agent mesh

`/scripts/03-agent.sh` handles the `di-agent` runtime separately:

- builds the Go binary from the `semantic-map` project,
- builds a Docker image,
- imports the image into each worker VM's containerd runtime,
- creates a Kubernetes `Deployment` per VM,
- applies `nodeSelector` so each pod lands on its expected host,
- uses `hostNetwork: true` so each agent can reach the others over VM networking.

This split matters: the workload and telemetry services are cluster-native; the agent peers are host-native VM processes that behave like edge nodes in the same lab.

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

These components generate and consume telemetry, and the switchboard decides who gets power based on priority and available supply.

### Telemetry writer

`system/telemetry-writer` consumes the Kafka topics and maps them to InfluxDB measurements. This bridge is essential because the simulation services publish machine-readable events, while Grafana expects a time-series database.

### InfluxDB and Grafana

The chart provisions InfluxDB and Grafana and preconfigures Grafana's datasource to point at InfluxDB. This allows the operator to observe system health and telemetry without separate manual config.

The practical requirement is simple: the InfluxDB credentials must exist before `helm install` or the chart will fail to initialize the datasource.

## Agent and trust architecture

The `di-agent` layer is the heart of the experiment.

### Agent placement

Each worker VM hosts one agent pod. The pod runs with `hostNetwork: true` and is pinned to the VM hostname via `nodeSelector`. This lets the agent:

- interact with local runtime configuration,
- reach other agent nodes directly over the VM network,
- behave as a peer endpoint rather than a normal in-cluster service.

### Peer registration

`scripts/04-peers.sh` resolves each VM IP, calls `POST /peers` on each target agent, and then issues `POST /peers/{id}/trust` to assign an explicit trust value. The result is a dense mesh where each agent knows about all the others.

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
- The `di-agent` binary is built from the `semantic-map` Go sources and deployed separately.
- The telemetry service credentials are required before chart deployment.

These boundaries keep the lab understandable: one step creates the nodes, one step creates the cluster, one step deploys services, and one step deploys the peer agent mesh.

## Execution flow

The real operational flow is:

```text
Terraform -> VM fleet
  -> kubeadm/k3s/KubeEdge cluster
  -> image build + registry push
  -> Helm chart deployment
  -> agent deployment on each worker VM
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
- `scripts/03-agent.sh`: per-node agent deployment
- `scripts/04-peers.sh`: peer + trust registration
- `scripts/coordinator.sh`: trust-aware routing experiment
- `helm/di-agent-system/values.yaml`: runtime service defaults and secret references
- `helm/di-agent-system/templates/`: deployed workloads and their wiring
- `system/*`: generators and telemetry consumers

## Operational caveat

This is a lab environment, not a production cluster. The service network is intentionally direct, host networking is used for the agent mesh, and the VM setup assumes a small local libvirt environment with a default network and known SSH key paths.

That is the correct architecture for PoC2: simple enough to reproduce, explicit enough to debug, and close enough to the target coordination problem to test the behavior that matters.
