#!/usr/bin/env bash
# 02-k3s.sh - bootstrap a k3s cluster across VMs.
#
# Usage: ./02-k3s.sh [vm1 vm2 vm3 ...]
#   The first VM becomes the k3s server; remaining VMs join as agents.

set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

SSH_USER="${SSH_USER:-ubuntu}"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/id_ed25519_vms}"
SSH_OPTS=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i "$SSH_KEY")
K3S_CHANNEL="${K3S_CHANNEL:-stable}"
LOCAL_KUBECONFIG="${LOCAL_KUBECONFIG:-$HOME/.kube/config-poc2-k3s}"

info() { echo -e "${YELLOW}[k3s] $*${NC}"; }
ok()   { echo -e "${GREEN}[k3s] $*${NC}"; }
err()  { echo -e "${RED}[k3s] $*${NC}" >&2; }

get_vm_ip() {
    virsh domifaddr "$1" | awk '/ipv4/ {print $4}' | cut -d'/' -f1 | head -n1
}

wait_for_vm_ip() {
    local vm_name="$1"
    local attempt
    for attempt in $(seq 1 40); do
        local ip
        ip=$(get_vm_ip "$vm_name")
        if [[ -n "$ip" ]]; then
            echo "$ip"
            return 0
        fi
        sleep 3
    done
    err "Timed out waiting for IP address of $vm_name"
    return 1
}

ssh_vm() {
    local ip="$1"; shift
    ssh "${SSH_OPTS[@]}" "${SSH_USER}@${ip}" "$@"
}

if [[ "$#" -eq 0 ]]; then
    VMS=(ubuntu-vm1 ubuntu-vm2 ubuntu-vm3)
else
    VMS=("$@")
fi

CONTROL_PLANE_VM="${VMS[0]}"
WORKER_VMS=("${VMS[@]:1}")
declare -A VM_IP

for vm in "${VMS[@]}"; do
    info "Resolving IP for $vm ..."
    VM_IP[$vm]=$(wait_for_vm_ip "$vm")
    ok "$vm -> ${VM_IP[$vm]}"
done

CP_IP="${VM_IP[$CONTROL_PLANE_VM]}"
info "Installing k3s server on $CONTROL_PLANE_VM ..."
ssh_vm "$CP_IP" "curl -sfL https://get.k3s.io | INSTALL_K3S_CHANNEL='$K3S_CHANNEL' sh -s - server --write-kubeconfig-mode 644"
ok "k3s server installed"

K3S_TOKEN=$(ssh_vm "$CP_IP" "sudo cat /var/lib/rancher/k3s/server/node-token")
for vm in "${WORKER_VMS[@]}"; do
    info "Joining $vm as a k3s agent ..."
    ssh_vm "${VM_IP[$vm]}" "curl -sfL https://get.k3s.io | K3S_URL='https://${CP_IP}:6443' K3S_TOKEN='$K3S_TOKEN' INSTALL_K3S_CHANNEL='$K3S_CHANNEL' sh -"
    ok "$vm joined the k3s cluster"
done

info "Waiting for all k3s nodes to become Ready ..."
for attempt in $(seq 1 60); do
    ready_count=$(ssh_vm "$CP_IP" "sudo k3s kubectl get nodes --no-headers" | grep -c ' Ready' || true)
    if [[ "$ready_count" -eq "${#VMS[@]}" ]]; then
        break
    fi
    sleep 5
done

ssh_vm "$CP_IP" "sudo k3s kubectl get nodes -o wide"
mkdir -p "$(dirname "$LOCAL_KUBECONFIG")"
ssh_vm "$CP_IP" "sudo cat /etc/rancher/k3s/k3s.yaml" | sed "s/127\.0\.0\.1/${CP_IP}/g" > "$LOCAL_KUBECONFIG"
chmod 600 "$LOCAL_KUBECONFIG"
ok "k3s cluster ready. Kubeconfig saved to $LOCAL_KUBECONFIG"
echo "export KUBECONFIG=$LOCAL_KUBECONFIG"