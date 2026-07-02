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
VMS := $(shell virsh list --all --name 2>/dev/null | grep -v "^$$" | sort)
VM_COUNT := $(words $(VMS))

# Generate VM variables dynamically (VM1, VM2, VM3, ...)
define generate_vm_vars
$(foreach i,$(shell seq 1 $(VM_COUNT)),VM$(i) ?= $(word $(i),$(VMS))$(\n))
endef

$(eval $(call generate_vm_vars))

# Create VM list for command arguments
VM_ARGS := $(foreach i,$(shell seq 1 $(VM_COUNT)),$(VM$(i)))

# Check if we have any VMs
ifeq ($(VM_COUNT),0)
$(error No VMs found. Please create VMs first.)
endif

SCRIPTS := ./scripts

# Display detected VMs
$(info Detected $(VM_COUNT) VM(s): $(VMS))

all: provision uncloud netdata agent peers

provision:
	@bash $(SCRIPTS)/01-provision.sh

uncloud:
	@bash $(SCRIPTS)/02-uncloud.sh $(VM_ARGS)

netdata:
	@bash $(SCRIPTS)/03-netdata.sh $(VM_ARGS)

agent:
	@bash $(SCRIPTS)/04-agent.sh

peers:
	@bash $(SCRIPTS)/05-peers.sh $(VM_ARGS)

start-vms:
	@bash $(SCRIPTS)/startVMS.sh $(VM_ARGS)

stop-vms:
	@bash $(SCRIPTS)/stopVMS.sh $(VM_ARGS)

deploy: 
	@bash $(SCRIPTS)/deploy.sh

demo:
	@bash $(SCRIPTS)/coordinator.sh $(VM_ARGS)

workload-heavy:
	@bash $(SCRIPTS)/workload.sh $(VM1) heavy

workload-light:
	@bash $(SCRIPTS)/workload.sh $(VM1) light

workload-stop:
	@bash $(SCRIPTS)/workload.sh $(VM1) stop

teardown:
	@bash $(SCRIPTS)/teardown.sh $(VM_ARGS)

# Dynamic status target that works with any number of VMs
status:
	@echo "========================================="
	@echo "Checking status of $(VM_COUNT) VM(s)..."
	@echo "========================================="
	$(foreach vm,$(VMS), \
		echo "=== $(vm) ==="; \
		ip=$$(virsh domifaddr $(vm) --source lease 2>/dev/null | grep -oE '([0-9]{1,3}\.){3}[0-9]{1,3}' | head -1); \
		if [ -n "$$ip" ]; then \
			curl -s "http://$$ip:8080/cost" 2>/dev/null || echo "Agent not responding on $(vm)"; \
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

.PHONY: all provision uncloud netdata agent peers start-vms stop-vms deploy demo \
        workload-heavy workload-light workload-stop teardown status list-vms get-ip-%

# Help target
help:
	@echo "Available targets:"
	@echo "  all          - Full stack: provision → k0s → netdata → agent → peers"
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