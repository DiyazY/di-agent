## poc/Makefile — multi-VM PoC for di-agent trust-weighted peer routing
##
## Usage:
##   make all          # full stack: provision → k0s → netdata → agent → peers
##   make demo         # run coordinator for 8 rounds (10s interval)
##   make status       # curl /cost from all agents
##   make teardown     # stop, delete, purge all VMs
##
## Auto-detects all VMs at runtime using virsh

# Auto-detect VMs from virsh (only running VMs)
VMS := $(shell virsh list --all --name 2>/dev/null | grep -v '^$$' | sort)
VM_COUNT := $(words $(VMS))
# Running VMs only: the kubeadm cluster nodes (first = control plane)
RUNNING_VMS := $(shell virsh list --state-running --name 2>/dev/null | grep -v '^$$' | sort)

# Generate VM variables dynamically (VM1, VM2, VM3, ...)
define VM_DEFS
$(foreach i,$(shell seq 1 $(VM_COUNT)),VM$(i) ?= $(word $(i),$(VMS))
)
endef

$(eval $(VM_DEFS))

SCRIPTS := ./scripts

# Helm chart covering every service except di-agent (kafka, postgres,
# replay-streamer, influxdb, telemetry-writer, grafana, and opt-in legacy
# simulations/playground) — see helm/di-agent-system. Any pod can be
# scheduled on any cluster node; there is no per-service VM pinning.
HELM_CHART := helm/di-agent-system
HELM_RELEASE ?= di-agent-system
HELM_NAMESPACE ?= default
# The postgres-ingest hook splits and loads ~170 MB of CSVs; Helm's 5m default is too short.
HELM_TIMEOUT ?= 30m
REGISTRY ?= ghcr.io/chuducanh242002
TAG ?= latest

# Optional replay-streamer overrides for helm-install/helm-template. Unset
# variables fall back to the chart defaults in values.yaml.
#   make helm-install STREAMER_MODE=real STREAMER_INTERVAL_MS=500 STREAMER_POINTS=25
STREAMER_MODE ?=
STREAMER_RUN_ID ?=
STREAMER_INTERVAL_MS ?=
STREAMER_POINTS ?=
STREAMER_LOOP ?=
# URL consumed by the first agent in the direct multi-node deployment.
DATASET_COLLECTOR_URL ?= http://dataset-collector.$(HELM_NAMESPACE).svc.cluster.local:8000/collect
AGENT_COLLECT_INTERVAL ?= 5s
# Any additional helm flags, e.g. HELM_ARGS="--set genset.enabled=true --wait".
HELM_ARGS ?=
HELM_STREAMER_SETS = \
	$(if $(STREAMER_MODE),--set streamer.mode=$(STREAMER_MODE)) \
	$(if $(STREAMER_RUN_ID),--set streamer.runId=$(STREAMER_RUN_ID)) \
	$(if $(STREAMER_INTERVAL_MS),--set streamer.intervalMs=$(STREAMER_INTERVAL_MS)) \
	$(if $(STREAMER_POINTS),--set streamer.pointsPerInterval=$(STREAMER_POINTS)) \
	$(if $(STREAMER_LOOP),--set streamer.loop=$(STREAMER_LOOP))

# Display detected VMs
$(info Detected $(VM_COUNT) VM(s): $(VMS))

all: provision k8s images helm-install agent peers

provision:
	@bash $(SCRIPTS)/01-provision.sh

k8s:
	@bash $(SCRIPTS)/02-k8s.sh $(VMS)

# Build and push chart-managed service images to $(REGISTRY):$(TAG).
# Override IMAGES to build/push only a subset, e.g.:
#   make images IMAGES="genset propulsion"
IMAGES ?=
images:
	@REGISTRY=$(REGISTRY) TAG=$(TAG) bash $(SCRIPTS)/build-push-images.sh $(IMAGES)

helm-lint:
	@helm lint $(HELM_CHART)

helm-template:
	@helm template $(HELM_RELEASE) $(HELM_CHART) --namespace $(HELM_NAMESPACE) \
		--set global.imageRegistry=$(REGISTRY) --set global.imageTag=$(TAG) \
		$(HELM_STREAMER_SETS) $(HELM_ARGS)

helm-install:
	@helm upgrade --install $(HELM_RELEASE) $(HELM_CHART) \
		--namespace $(HELM_NAMESPACE) --create-namespace --timeout $(HELM_TIMEOUT) \
		--set global.imageRegistry=$(REGISTRY) --set global.imageTag=$(TAG) \
		$(HELM_STREAMER_SETS) $(HELM_ARGS)

helm-upgrade: helm-install

helm-uninstall:
	@helm uninstall $(HELM_RELEASE) --namespace $(HELM_NAMESPACE)
	@kubectl -n $(HELM_NAMESPACE) delete job postgres-ingest --ignore-not-found

helm-status:
	@helm status $(HELM_RELEASE) --namespace $(HELM_NAMESPACE)

agent:
	@DATASET_COLLECTOR_URL="$(DATASET_COLLECTOR_URL)" COLLECT_INTERVAL="$(AGENT_COLLECT_INTERVAL)" \
		bash $(SCRIPTS)/03-agent.sh $(RUNNING_VMS)

peers:
	@bash $(SCRIPTS)/04-peers.sh $(VMS)

uncloud:
	@bash $(SCRIPTS)/uncloud.sh $(VMS)

k3s:
	@bash $(SCRIPTS)/k3s.sh $(VMS)

kubeedge:
	@bash $(SCRIPTS)/kubeedge.sh $(VMS)

start-vms:
	@bash $(SCRIPTS)/startVMS.sh $(VMS)

stop-vms:
	@bash $(SCRIPTS)/stopVMS.sh $(VMS)

deploy: 
	@bash $(SCRIPTS)/deploy.sh

demo:
	@bash $(SCRIPTS)/coordinator.sh $(VMS)

workload-heavy:
	@bash $(SCRIPTS)/workload.sh $(firstword $(VMS)) heavy

workload-light:
	@bash $(SCRIPTS)/workload.sh $(firstword $(VMS)) light

workload-stop:
	@bash $(SCRIPTS)/workload.sh $(firstword $(VMS)) stop

teardown:
	@bash $(SCRIPTS)/teardown.sh

# Dynamic status target that works with any number of VMs
status:
	@echo "========================================="
	@echo "Checking status of $(VM_COUNT) VM(s)..."
	@echo "========================================="
	$(foreach vm,$(VMS), \
		echo "=== $(vm) ==="; \
		ip=$$(virsh domifaddr $(vm) --source lease 2>/dev/null | grep -oE '([0-9]{1,3}\.){3}[0-9]{1,3}' | head -1); \
		if [ -n "$$ip" ]; then \
			curl -s "http://$$ip:9090/cost" 2>/dev/null || echo "Agent not responding on $(vm)"; \
		else \
			echo "VM $(vm) not running or no IP assigned"; \
		fi; \
		echo ""; \
	)

# Show all VMs with their IPs
list-vms:
	@echo "Current VMs:"
	@virsh list --all
	@echo ""
	@echo "VM IP Addresses:"
	@for vm in $(VMS); do \
		echo -n "$$vm: "; \
		virsh domifaddr $$vm --source lease 2>/dev/null | grep -oE '([0-9]{1,3}\.){3}[0-9]{1,3}' | head -1 || echo "No IP"; \
	done

# Get IP of a specific VM
get-ip-%:
	@vm="$*"; \
	ip=$$(virsh domifaddr $$vm --source lease 2>/dev/null | grep -oE '([0-9]{1,3}\.){3}[0-9]{1,3}' | head -1); \
	if [ -n "$$ip" ]; then \
		echo "$$ip"; \
	else \
		echo "No IP found for $$vm" >&2; \
		exit 1; \
	fi

.PHONY: all provision k8s k3s kubeedge uncloud netdata agent peers images helm-lint helm-template \
        helm-install helm-upgrade helm-uninstall helm-status start-vms stop-vms deploy demo \
	workload-heavy workload-light workload-stop teardown status list-vms get-ip-%

# Help target
help:
	@echo "Available targets:"
	@echo "  all          - Full stack: provision → k8s → images → helm-install → agent → peers"
	@echo "  images       - Build+push genset/switchboard/propulsion/battery/auxload/"
	@echo "                 telemetry-writer/telemetry-cloud-sync/playground/data-ingest/replay-streamer/"
	@echo "                 dataset-collector/di-agent images to REGISTRY (default $(REGISTRY))"
	@echo "                 Override with IMAGES to build/push only a subset, e.g. IMAGES=\"genset propulsion\""
	@echo "                 (telemetry-writer-cloud reuses the telemetry-writer image)"
	@echo "  k3s          - Install k3s server on VM1 and agents on remaining VMs"
	@echo "  kubeedge     - Install KubeEdge CloudCore on VM1 and EdgeCore on remaining VMs"
	@echo "  helm-install - helm upgrade --install the di-agent-system chart (requires external credentials)"
	@echo "                 Streamer options: STREAMER_MODE=real|synthetic STREAMER_RUN_ID"
	@echo "                 STREAMER_INTERVAL_MS STREAMER_POINTS STREAMER_LOOP; extra flags via HELM_ARGS"
	@echo "  helm-upgrade - Alias for helm-install"
	@echo "  helm-uninstall - helm uninstall the di-agent-system release"
	@echo "  helm-status  - helm status of the di-agent-system release"
	@echo "  helm-lint    - helm lint the chart"
	@echo "  helm-template - Render the chart's manifests locally (no cluster needed)"
	@echo "  agent        - Deploy di-agent on worker VMs; the first agent consumes dataset telemetry"
	@echo "                 Options: AGENT_COLLECT_INTERVAL, DATASET_COLLECTOR_URL"
	@echo "                 Alternative: HELM_ARGS=\"--set agent.enabled=true\" (do not use both)"
	@echo "  demo         - Run coordinator for 8 rounds (10s interval)"
	@echo "  status       - Check /cost endpoint on all VMs"
	@echo "  list-vms     - Show all VMs with their IPs"
	@echo "  get-ip-<vm>  - Get IP address of a specific VM (e.g., make get-ip-ubuntu-vm1)"
	@echo "  start-vms    - Start all VMs"
	@echo "  stop-vms     - Stop all VMs"
	@echo "  teardown     - Stop, delete, purge all VMs"
	@echo "  workload-*   - Manage workload on VM1"
	@echo ""
	@echo "Detected $(VM_COUNT) VM(s): $(VMS)"
	@echo ""
	@echo "To override VM detection, set VMS variable:"
	@echo "  make all VMS=\"vm1 vm2 vm3\""
	@echo ""
	@echo "To override the registry/tag used by 'images' and 'helm-install':"
	@echo "  make images helm-install REGISTRY=ghcr.io/myorg TAG=v1"