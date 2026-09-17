## poc/Makefile — multi-VM PoC for di-agent trust-weighted peer routing
##
## Usage:
##   make all          # full stack: provision → k0s → netdata → agent → peers
##   make demo         # run coordinator for 8 rounds (10s interval)
##   make status       # curl /cost from all three agents
##   make teardown     # stop, delete, purge all VMs
##
## Override VM names:
##   make all VM1=my-1 VM2=my-2 VM3=my-3

VM1 ?= diag-1
VM2 ?= diag-2
VM3 ?= diag-3

SCRIPTS := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))scripts

.PHONY: all provision k0s netdata agent peers \
        workload-heavy workload-stop demo status teardown

all: provision k0s netdata agent peers

provision:
	@bash $(SCRIPTS)/01-provision.sh $(VM1) $(VM2) $(VM3)

k0s:
	@bash $(SCRIPTS)/02-k0s.sh $(VM1) $(VM2) $(VM3)

netdata:
	@bash $(SCRIPTS)/03-netdata.sh $(VM1) $(VM2) $(VM3)

agent:
	@bash $(SCRIPTS)/04-agent.sh $(VM1) $(VM2) $(VM3)

peers:
	@bash $(SCRIPTS)/05-peers.sh $(VM1) $(VM2) $(VM3)

workload-heavy:
	@bash $(SCRIPTS)/workload.sh $(VM1) heavy

workload-light:
	@bash $(SCRIPTS)/workload.sh $(VM1) light

workload-stop:
	@bash $(SCRIPTS)/workload.sh $(VM1) stop

demo:
	@bash $(SCRIPTS)/coordinator.sh $(VM1) $(VM2) $(VM3)

status:
	@echo "=== di-agent cost snapshot ==="
	@for vm in $(VM1) $(VM2) $(VM3); do \
		ip=$$(multipass list | awk -v v="$$vm" '$$1==v{print $$3}'); \
		if [ -z "$$ip" ]; then \
			echo "  $$vm: not running"; \
			continue; \
		fi; \
		echo "--- $$vm ($$ip) ---"; \
		curl -sf "http://$$ip:9090/cost?taskType=pod-scheduling&nodeID=master" | \
			python3 -c "import sys,json; d=json.load(sys.stdin); \
			print('  ResourceCost={ResourceCost:.4f}  Confidence={Confidence:.4f}'.format(**d))" \
			2>/dev/null || echo "  (agent not responding)"; \
	done

teardown:
	@bash $(SCRIPTS)/teardown.sh $(VM1) $(VM2) $(VM3)
