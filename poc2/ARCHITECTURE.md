# PoC2 Architecture

## Purpose

PoC2 is a controlled distributed test environment for the di-agent idea: multiple agents run on separate machines, observe different workloads, exchange trust information, and choose routing or scheduling partners based on both cost and trust.

This is more than a Kubernetes demo. It is a reference architecture for observing how a multi-agent coordination system behaves when:

- nodes start with similar priors,
- workloads differ across machines,
- trust between peers changes over time,
- a recommendation system must route around a degraded or untrusted peer.

## High-level system view

The deployment stack has five layers:

1. Infrastructure layer
   - libvirt/QEMU VMs created by Terraform
   - Ubuntu cloud images and VM networking

2. Control-plane layer
   - kubeadm cluster
   - one control-plane VM and worker VMs

3. Messaging and data layer
   - Kafka as the event backbone
   - InfluxDB as the time-series store
   - Grafana as observability front-end

4. Agent layer
   - a di-agent process per worker node
   - peers discovered over HTTP
   - trust and recommendation APIs exposed by the agent

5. Workload simulation layer
   - artificial generators such as genset and propulsion telemetry
   - coordinator script that drives the demo behavior

## Deployment pattern

The deployment is orchestrated by a shell-script pipeline rather than a single declarative stack. Each script performs a distinct step:

- `01-provision.sh`: terraform init + plan + apply for VM creation
- `02-k8s.sh`: bootstrap Kubernetes with kubeadm
- `03-kafka.sh`: deploy a single-node KRaft Kafka broker
- `04-agent.sh`: build the di-agent binary and deploy it to each worker node as a pod
- `05-peers.sh`: register peer URLs and set trust values
- `06-genset.sh`, `06a-switchboard.sh` and `06b-propulsion.sh`: generate telemetry workloads for one or more gensets, a central switchboard, and one or more propulsion consumers
- `07-influxdb.sh`: create the time-series database
- `07b-telemetry-writer.sh`: stream Kafka messages into InfluxDB
- `08-grafana.sh`: provision Grafana with an InfluxDB datasource

The `Makefile` simply groups these steps into a standard operating sequence.

## Infrastructure architecture

The VM environment is created in `main.tf`.

### Terraform responsibilities

`main.tf` does the following:

- creates a libvirt storage pool
- downloads the Ubuntu 22.04 cloud image into that pool
- clones a base disk for each VM
- creates cloud-init disks for SSH and network bootstrapping
- defines `libvirt_domain` resources for each VM
- assigns one interface to the default libvirt network
- sets the first VM to a slightly larger CPU allocation, which matches the control-plane role

This gives the lab a small cluster without requiring external cloud resources.

## Kubernetes architecture

The cluster is created over the VM set using kubeadm. The script `02-k8s.sh` does the following:

- resolves each VM IP from `virsh`
- installs container runtime and Kubernetes packages
- enables the required kernel modules and sysctls
- initializes kubeadm on the first VM as the control plane
- installs Flannel as the pod network
- joins remaining VMs as workers
- writes a local kubeconfig for cluster access

The result is a simple control-plane + workers cluster that is easy to reason about and enough for the local PoC.

## Message and telemetry architecture

The data-plane is built around Kafka and InfluxDB.

### Kafka

Kafka is deployed with a single broker in KRaft mode. The manifest in `config/kafka-deployment.yaml` sets:

- host networking
- a single-node broker/controller configuration
- one topic for genset telemetry and one for propulsion telemetry
- a static `CLUSTER_ID` and `KAFKA_NODE_ID`

Because the broker runs on a VM and the other components also use host networking, each component can reach the broker by direct host IP rather than by Kubernetes service discovery.

### data producers

The workload simulators are containerized Python services in `system/genset/` and `system/propulsion/`.

Their behavior is intentionally simple:

- they expose a lightweight HTTP API (`/status`, `/load`, `/health`)
- they adjust a simulated load ratio over time
- they emit telemetry records to Kafka with timestamps and physical metrics

The genset and propulsion workloads are not just monitoring tools; they model a real system under load.

### switchboard

`system/switchboard/` is the central power-management authority between gensets and consumers. Rather than every consumer summing raw genset telemetry itself (which only works for one genset talking to one consumer), the switchboard is the single place that:

- consumes `genset.telemetry` from every genset and sums it into total available bus supply
- consumes `switchboard.requests` from every consumer (e.g. propulsion), each carrying a requested power and a load-shedding priority
- every tick, allocates the available supply across consumers in priority order — high-priority consumers are served first, and low-priority ones are shed if supply can't cover total demand
- publishes each consumer's grant to `switchboard.telemetry`, and exposes the full picture (per-genset supply, per-consumer request/allocation) over `/status`

This is the same role a physical switchboard plays on a vessel: it is the shared busbar all generators feed and all loads draw from, with a power-management layer that decides who gets power when supply is short. It also makes the topology genuinely multi-source/multi-consumer: adding a second genset or a second propulsion drive only means pointing them at the same switchboard, with no consumer needing to know how many gensets exist.

### telemetry writer

`system/telemetry-writer/main.py` is the bridge between Kafka and InfluxDB.

It:

- subscribes to one or more Kafka topics
- deserializes JSON payloads
- maps each payload to a measurement schema
- writes points into InfluxDB using the proper tags and fields

This is the part that converts raw event streams from machine simulation into time-series data that Grafana can visualize.

### InfluxDB and Grafana

InfluxDB is deployed as a single-node time-series database. Grafana is then provisioned with an InfluxDB datasource, using the token and bucket settings defined in the config environment.

This combination makes the system visible: the lifecycle is

- generator emits telemetry
- Kafka stores and distributes the event stream
- telemetry writer writes to InfluxDB
- Grafana renders the metrics

## Agent architecture

Each worker VM runs a di-agent pod. In `scripts/04-agent.sh`, the build process does the following:

- builds the Go agent binary from the semantic-map project
- creates a Docker image for the agent
- imports the image into the worker node's containerd
- deploys one `Deployment` per VM
- schedules each pod to its own host via `nodeSelector`
- passes environment variables such as `NODE_ID`, `REGIME`, and `KAFKA_BROKERS`

The important part is the hostNetworking and node targeting: each agent can reach the others directly on the VM network, which matches the peer-to-peer coordination scenario.

## Peer and trust model

The peer registration flow is defined in `scripts/05-peers.sh`.

### Registration

For each VM, the script does this:

- resolves all peer IPs
- calls `POST /peers` on the target agent
- extracts the derived peer ID from the response
- calls `POST /peers/{id}/trust` to set an explicit trust value

This creates a mesh where each node knows the others and the system has a non-trivial trust value to reason over.

### Routing experiment

The coordinator script `scripts/coordinator.sh` is the demonstration layer that exercises the peer logic.

For each round, it:

- queries `/cost` on the agents
- picks the node with the highest resource cost
- calls `/recommend` on that node
- inspects the recommendations and expected savings
- optionally drains trust for a peer mid-run by setting trust to a value below the minimum threshold

This creates the desired effect: as trust falls, a previously recommended peer becomes ineligible, and the system reroutes to another peer.

## Execution sequence in one view

```text
Terraform -> VM fleet
      -> kubeadm cluster
      -> Kafka broker
      -> di-agent pods
      -> peer registration
      -> workload simulators
      -> Kafka stream
      -> InfluxDB
      -> Grafana
      -> coordinator demo
```

The design is intentionally layered so each system boundary remains visible and debuggable.

## Why this architecture matters

PoC2 is designed to validate an operational claim of the di-agent project:

- agents start in similar states,
- their local observations diverge under different workloads,
- trust and routing therefore diverge too,
- and recommendation logic responds to that drift in a way that can be observed and measured.

This is not a production cluster setup. It is a lab proving ground for a distributed coordination policy that depends on local context plus peer trust.

## Key files

- `main.tf`: VM creation
- `variables.tf`: configuration variables
- `providers.tf`: libvirt provider
- `Makefile`: orchestration entrypoints
- `scripts/*.sh`: workflow scripts
- `config/*.yaml`: rendered Kubernetes manifests
- `system/*`: workload and bridge implementations

## Operational notes

- The default assumption is a small local cluster on top of libvirt.
- Host networking is used intentionally so the components can talk to each other directly.
- The scripts assume a specific SSH key path and VM naming convention (`ubuntu-vm1`, `ubuntu-vm2`, ...).
- The environment is meant for demonstration and iteration, not for deployment to a production private network.

## End state

When the PoC is fully active, the user can observe all of the following:

- a Kubernetes cluster with multiple worker nodes
- multiple di-agent instances with peer knowledge
- real-time or near-real-time telemetry generated by the workloads
- metrics in InfluxDB
- dashboards in Grafana
- evidence that routing decisions change when one peer loses trust

That end-to-end path is the actual purpose of PoC2.
