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
   - Helm chart in `helm/di-agent-system` deploys Kafka, InfluxDB, Grafana, genset, battery, propulsion, auxload, switchboard, telemetry-writer, and playground
   - `scripts/03-agent.sh` builds and deploys the Go `di-agent` binary as one pod per worker VM
   - `scripts/04-peers.sh` registers each agent as a peer of the others and sets trust values
   - `scripts/coordinator.sh` runs the trust-drain/routing experiment

The key point is that the `di-agent` pods are intentionally not part of the Helm chart. They are created separately, one pod per VM, to model host-level peer nodes directly on the VM network.

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

You should also set the registry and tag before installing the chart if you are not using the defaults:

```bash
make images REGISTRY=ghcr.io/your-org TAG=v1
make helm-install REGISTRY=ghcr.io/your-org TAG=v1
```

The chart also expects the build context directories under `system/` and `playground/` to exist and to be buildable with Docker.

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
4. `make helm-install` deploys Kafka, InfluxDB, Grafana, switchboard, genset, battery, propulsion, auxload, and the playground.
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
- `helm/di-agent-system/templates/`: the actual Kubernetes manifests for Kafka, InfluxDB, Grafana, and simulation services
- `system/*`: workload generators and telemetry services

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
