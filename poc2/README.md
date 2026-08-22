# PoC2: distributed multi-node di-agent lab

This directory contains the second proof-of-concept for the di-agent project: a local Kubernetes lab built from libvirt virtual machines, with a full telemetry and coordination pipeline around the agent runtime.

The purpose is not just to "start a cluster." This PoC simulates a small edge environment where multiple di-agent nodes run on separate VMs, discover each other as peers, generate workload and telemetry data, and demonstrate trust-weighted routing decisions under changing conditions.

## What happens in this PoC

The flow is:

1. Terraform creates a small libvirt VM fleet.
2. A kubeadm bootstrap script installs Kubernetes across those VMs.
3. Kafka is deployed as a single-broker KRaft service.
4. di-agent is built and deployed as one pod per worker node.
5. Each agent registers the others as peers and sets trust values.
6. Synthetic workloads emit telemetry into Kafka.
7. A telemetry bridge writes Kafka data into InfluxDB.
8. Grafana visualizes the stored metrics.
9. A coordinator script polls each node and triggers a trust-drain event to demonstrate rerouting logic.

In other words, this is a realistic-ish distributed test bed for evaluating the same trust and routing ideas that are central to the di-agent research.

## Topology

The default topology is:

- VM1: Kubernetes control-plane node
- VM2: worker node, Kafka, InfluxDB, Grafana, telemetry writer
- VM3: worker node, genset workload generator
- VM4: additional worker node, optional for expansion

The scripts are written so the first VM in the list is treated as the control plane and later VMs become workers.

## Core components

- Terraform: builds the VMs and libvirt storage pool in `main.tf`
- kubeadm bootstrap: `scripts/02-k8s.sh`
- Kafka: `scripts/03-kafka.sh` with `config/kafka-deployment.yaml`
- di-agent deployment: `scripts/04-agent.sh`
- peer registration: `scripts/05-peers.sh`
- synthetic power-plant simulation:
  - `scripts/06-genset.sh`
  - `scripts/06b-propulsion.sh`
- time-series storage: `scripts/07-influxdb.sh`
- Kafka-to-InfluxDB bridge: `scripts/07b-telemetry-writer.sh`
- Grafana dashboard: `scripts/08-grafana.sh`
- trust-routing demo: `scripts/coordinator.sh`

## Prerequisites

- Terraform
- libvirt / KVM / QEMU
- Ubuntu cloud image access
- SSH key for the VMs
- Docker for building images used by the cluster
- `kubectl` and kube config access after bootstrap
- `virsh` and a default libvirt network
- `envsubst` for templated manifests

The scripts expect the SSH key to be at:

- `$HOME/.ssh/id_ed25519_vms`

## Quick start

From this directory:

```bash
make provision
make k8s
make kafka
make agent
make peers
make genset
make propulsion
make influxdb
make telemetry-writer
make grafana
```

This is the intended end-to-end setup sequence. The project is not a single monolithic deployment; it is a scripted pipeline that stages infrastructure, then service workloads, then telemetry and demo traffic.

## Useful commands

```bash
make status
make list-vms
make demo
make teardown
```

The demo script runs a multi-round coordinator loop that:

- calls `/cost` on each agent
- identifies the busiest node
- asks that node for a routing recommendation
- drops trust for one peer half-way through the experiment
- shows how the recommendation changes when trust falls below the threshold

## Important config files

- `main.tf`: libvirt domain definition and VM disk setup
- `variables.tf`: VM count and image settings
- `providers.tf`: provider configuration for libvirt
- `config/.env.example`: environment variables for the runtime services
- `config/*.yaml`: Kubernetes manifests rendered per deployment
- `scripts/*.sh`: orchestration steps

## Filesystem layout

```text
poc2/
├── config/
│   ├── .env
│   ├── .env.example
│   ├── cloud_init.yml
│   ├── metadata.yml
│   ├── kafka-deployment.yaml
│   ├── influxdb-deployment.yaml
│   ├── grafana-deployment.yaml
│   ├── genset-deployment.yaml
│   ├── propulsion-deployment.yaml
│   └── telemetry-writer-deployment.yaml
├── scripts/
│   ├── 01-provision.sh
│   ├── 02-k8s.sh
│   ├── 03-kafka.sh
│   ├── 04-agent.sh
│   ├── 05-peers.sh
│   ├── 06-genset.sh
│   ├── 06b-propulsion.sh
│   ├── 07-influxdb.sh
│   ├── 07b-telemetry-writer.sh
│   ├── 08-grafana.sh
│   ├── coordinator.sh
│   ├── deploy.sh
│   ├── teardown.sh
│   └── ...
├── system/
│   ├── genset/
│   ├── propulsion/
│   └── telemetry-writer/
├── main.tf
├── variables.tf
├── providers.tf
├── Makefile
├── README.md
└── terraform.tfstate*
```

## What the system is proving

This PoC is meant to test the following idea in a controllable lab environment:

- multiple agents start with similar priors
- each agent learns from different workload histories
- peer trust diverges over time
- a routing recommendation must account for confidence and trust, not just raw load
- a trust drop should change the recommended next hop, demonstrating adaptive rerouting

The coordinator script is the visible manifestation of that behavior: it polls the agents, inspects their routing recommendations, and then intentionally lowers the trust on one peer to demonstrate the system's self-correction.

## Notes

- The first VM is the Kubernetes API endpoint and is treated as the control plane.
- The scripts usually default to host networking so pods can directly reach each other and external services.
- Kafka and InfluxDB are intentionally placed on the same worker node by default for simplicity in a small testbed.
- The environment is a lab, not production infrastructure. It is meant for exercising trust-aware coordination patterns in a repeatable local environment.

## Cleanup

To remove the environment:

```bash
make teardown
```

This destroys the VMs and associated libvirt resources created by the PoC.