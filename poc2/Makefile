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

VM1 ?= ubuntu-vm1
VM2 ?= ubuntu-vm2
VM3 ?= ubuntu-vm3

SCRIPTS := ./scripts

all:
	provision uncloud netdata agent peers

provision:
	@bash $(SCRIPTS)/01-provision.sh

uncloud:
	@bash $(SCRIPTS)/02-uncloud.sh $(VM1) $(VM2) $(VM3)

netdata:
	@bash $(SCRIPTS)/03-netdata.sh $(VM1) $(VM2) $(VM3)

agent:
	@bash $(SCRIPTS)/04-agent.sh

peers:
	@bash $(SCRIPTS)/05-peers.sh $(VM1) $(VM2) $(VM3)

start-vms:
	@bash $(SCRIPTS)/startVMS.sh $(VM1) $(VM2) $(VM3)

stop-vms:
	@bash $(SCRIPTS)/stopVMS.sh $(VM1) $(VM2) $(VM3)

deploy: 
	@bash $(SCRIPTS)/deploy.sh

demo:
	@bash $(SCRIPTS)/coordinator.sh $(VM1) $(VM2) $(VM3)

workload-heavy:
	@bash $(SCRIPTS)/workload.sh $(VM1) heavy

workload-light:
	@bash $(SCRIPTS)/workload.sh $(VM1) light

workload-stop:
	@bash $(SCRIPTS)/workload.sh $(VM1) stop

teardown:
	@bash $(SCRIPTS)/teardown.sh $(VM1) $(VM2) $(VM3)
