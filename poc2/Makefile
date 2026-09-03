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

# Generate VM variables dynamically (VM1, VM2, VM3, ...)
define VM_DEFS
$(foreach i,$(shell seq 1 $(VM_COUNT)),VM$(i) ?= $(word $(i),$(VMS))
)
endef

$(eval $(VM_DEFS))

SCRIPTS := ./scripts

# Helm chart covering every service except di-agent (kafka, genset,
# switchboard, propulsion, battery, auxload, influxdb, telemetry-writer,
# grafana, playground) — see helm/di-agent-system. Any of its pods can be
# scheduled on any cluster node; there is no per-service VM pinning.
HELM_CHART := helm/di-agent-system
HELM_RELEASE ?= di-agent-system
HELM_NAMESPACE ?= default
REGISTRY ?= ghcr.io/chuducanh242002
TAG ?= latest

# Display detected VMs
$(info Detected $(VM_COUNT) VM(s): $(VMS))

all: provision k8s images helm-install agent peers

provision:
	@bash $(SCRIPTS)/01-provision.sh

k8s:
	@bash $(SCRIPTS)/02-k8s.sh $(VMS)

# Build and push all chart-managed service images to $(REGISTRY):$(TAG).
images:
	@REGISTRY=$(REGISTRY) TAG=$(TAG) bash $(SCRIPTS)/build-push-images.sh

helm-lint:
	@helm lint $(HELM_CHART)

helm-template:
	@helm template $(HELM_RELEASE) $(HELM_CHART) --namespace $(HELM_NAMESPACE)

helm-install:
	@helm upgrade --install $(HELM_RELEASE) $(HELM_CHART) \
		--namespace $(HELM_NAMESPACE) --create-namespace \
		--set global.imageRegistry=$(REGISTRY) --set global.imageTag=$(TAG)

helm-upgrade: helm-install

helm-uninstall:
	@helm uninstall $(HELM_RELEASE) --namespace $(HELM_NAMESPACE)

helm-status:
	@helm status $(HELM_RELEASE) --namespace $(HELM_NAMESPACE)

agent:
	@bash $(SCRIPTS)/03-agent.sh $(VMS)

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
	@echo "  k3s          - Install k3s server on VM1 and agents on remaining VMs"
	@echo "  kubeedge     - Install KubeEdge CloudCore on VM1 and EdgeCore on remaining VMs"
	@echo "                 telemetry-writer/playground images to REGISTRY (default $(REGISTRY))"
	@echo "  helm-install - helm upgrade --install the di-agent-system chart (kafka..playground)"
	@echo "  helm-upgrade - Alias for helm-install"
	@echo "  helm-uninstall - helm uninstall the di-agent-system release"
	@echo "  helm-status  - helm status of the di-agent-system release"
	@echo "  helm-lint    - helm lint the chart"
	@echo "  helm-template - Render the chart's manifests locally (no cluster needed)"
	@echo "  agent        - Deploy di-agent (one pod per worker VM; not part of the Helm chart)"
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