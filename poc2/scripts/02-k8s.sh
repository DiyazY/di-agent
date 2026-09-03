#!/bin/bash
# 02-k8s.sh — bootstrap a kubeadm cluster across VMs.
#
# Usage: ./02-k8s.sh [vm1 vm2 vm3 ...]
#   The first VM becomes the control-plane node; all remaining VMs join as
#   worker nodes. VM names default to ubuntu-vm1 ubuntu-vm2 ubuntu-vm3.

set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

SSH_USER="${SSH_USER:-ubuntu}"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/id_ed25519_vms}"
SSH_OPTS=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i "$SSH_KEY")
K8S_REPO_CHANNEL="${K8S_REPO_CHANNEL:-v1.30}"
POD_NETWORK_CIDR="${POD_NETWORK_CIDR:-10.244.0.0/16}"
LOCAL_KUBECONFIG="${LOCAL_KUBECONFIG:-$HOME/.kube/config-poc2}"

info() { echo -e "${YELLOW}[k8s] $*${NC}"; }
ok()   { echo -e "${GREEN}[k8s] $*${NC}"; }
err()  { echo -e "${RED}[k8s] $*${NC}" >&2; }

# Useful for using network = "default"
get_vm_ip() {
    virsh domifaddr "$1" | awk '/ipv4/ {print $4}' | cut -d'/' -f1 | head -n1
}

ssh_vm() {
    local ip="$1"; shift
    ssh "${SSH_OPTS[@]}" "${SSH_USER}@${ip}" "$@"
}

wait_for_vm_ip() {
    local vm_name="$1"
    local max_attempts=40
    local attempt=1

    while [ "$attempt" -le "$max_attempts" ]; do
        local ip
        ip=$(get_vm_ip "$vm_name")
        if [ -n "$ip" ]; then
            echo "$ip"
            return 0
        fi
        sleep 3
        ((attempt++))
    done

    err "Timed out waiting for IP address of $vm_name"
    return 1
}

# ── args ─────────────────────────────────────────────────────────────────────
if [ "$#" -eq 0 ]; then
    VMS=(ubuntu-vm1 ubuntu-vm2 ubuntu-vm3)
else
    VMS=("$@")
fi

CONTROL_PLANE_VM="${VMS[0]}"
WORKER_VMS=("${VMS[@]:1}")

# ── resolve IPs ───────────────────────────────────────────────────────────────
declare -A VM_IP
for vm in "${VMS[@]}"; do
    info "Resolving IP for $vm ..."
    VM_IP[$vm]=$(wait_for_vm_ip "$vm")
    ok "$vm -> ${VM_IP[$vm]}"
done

# ── prerequisites (all nodes) ─────────────────────────────────────────────────
install_prereqs() {
    local ip="$1"

    ssh_vm "$ip" "sudo swapoff -a && sudo sed -i '/[[:space:]]swap[[:space:]]/d' /etc/fstab"

    ssh_vm "$ip" "cat <<'EOF' | sudo tee /etc/modules-load.d/k8s.conf >/dev/null
overlay
br_netfilter
EOF
sudo modprobe overlay
sudo modprobe br_netfilter
cat <<'EOF' | sudo tee /etc/sysctl.d/k8s.conf >/dev/null
net.bridge.bridge-nf-call-iptables  = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward                 = 1
EOF
sudo sysctl --system >/dev/null"

    ssh_vm "$ip" "sudo mkdir -p /etc/containerd && containerd config default | sudo tee /etc/containerd/config.toml >/dev/null && sudo sed -i 's/SystemdCgroup = false/SystemdCgroup = true/' /etc/containerd/config.toml && sudo systemctl restart containerd"

    ssh_vm "$ip" "sudo mkdir -p /etc/apt/keyrings && \
        curl -fsSL https://pkgs.k8s.io/core:/stable:/${K8S_REPO_CHANNEL}/deb/Release.key | sudo gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg && \
        echo 'deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/${K8S_REPO_CHANNEL}/deb/ /' | sudo tee /etc/apt/sources.list.d/kubernetes.list >/dev/null && \
        sudo apt-get update -y && \
        sudo apt-get install -y kubelet kubeadm kubectl && \
        sudo apt-mark hold kubelet kubeadm kubectl"
}

for vm in "${VMS[@]}"; do
    info "Installing container runtime + kubeadm/kubelet/kubectl on $vm ..."
    install_prereqs "${VM_IP[$vm]}"
    ok "Prerequisites installed on $vm"
done

# ── control-plane init ─────────────────────────────────────────────────────────
CP_IP="${VM_IP[$CONTROL_PLANE_VM]}"
info "Initializing control-plane on $CONTROL_PLANE_VM ($CP_IP) ..."
# Get CRI socket automatically
CRI_SOCKET=$(ssh_vm "$CP_IP" "sudo crictl info | grep -o 'unix://[^\"]*' | head -1")
CRI_SOCKET=${CRI_SOCKET:-unix:///var/run/containerd/containerd.sock}

ssh_vm "$CP_IP" "sudo kubeadm init \
  --pod-network-cidr=${POD_NETWORK_CIDR:-10.244.0.0/16} \
  --service-cidr=10.96.0.0/12 \
  --apiserver-advertise-address=${CP_IP} \
  --node-name=${CONTROL_PLANE_VM} \
  --cri-socket=${CRI_SOCKET}"
ok "Control-plane initialized on $CONTROL_PLANE_VM"

info "Configuring kubectl access for ${SSH_USER}@${CONTROL_PLANE_VM} ..."
ssh_vm "$CP_IP" "mkdir -p \$HOME/.kube && sudo cp -f /etc/kubernetes/admin.conf \$HOME/.kube/config && sudo chown \$(id -u):\$(id -g) \$HOME/.kube/config"

info "Installing flannel CNI ..."
ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config apply -f https://github.com/flannel-io/flannel/releases/latest/download/kube-flannel.yml"
ok "Flannel CNI applied"

# ── worker join ────────────────────────────────────────────────────────────────
if [ "${#WORKER_VMS[@]}" -gt 0 ]; then
    info "Generating join command on $CONTROL_PLANE_VM ..."
    JOIN_CMD=$(ssh_vm "$CP_IP" "sudo kubeadm token create --print-join-command")

    for vm in "${WORKER_VMS[@]}"; do
        info "Joining $vm as worker ..."
        ssh_vm "${VM_IP[$vm]}" "sudo $JOIN_CMD --node-name=${vm}"
        ok "$vm joined the cluster"
    done
else
    info "No worker VMs specified; control-plane only."
fi

# ── verify ─────────────────────────────────────────────────────────────────────
info "Waiting for all nodes to be Ready ..."
timeout=300
elapsed=0
while [ "$elapsed" -lt "$timeout" ]; do
    ready_count=$(ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config get nodes --no-headers" | grep -c " Ready" || true)
    if [ "$ready_count" -eq "${#VMS[@]}" ]; then
        ok "All ${#VMS[@]} nodes are Ready"
        break
    fi
    sleep 5
    elapsed=$((elapsed + 5))
done

ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config get nodes -o wide"
ok "Cluster bootstrap complete. Control-plane: $CONTROL_PLANE_VM, workers: ${WORKER_VMS[*]:-none}"

# ── fetch kubeconfig for local use ─────────────────────────────────────────────
info "Fetching kubeconfig to $LOCAL_KUBECONFIG ..."
mkdir -p "$(dirname "$LOCAL_KUBECONFIG")"
ssh_vm "$CP_IP" "cat \$HOME/.kube/config" > "$LOCAL_KUBECONFIG"
chmod 600 "$LOCAL_KUBECONFIG"
ok "Kubeconfig saved to $LOCAL_KUBECONFIG (server: https://${CP_IP}:6443)"

echo ""
echo "To use this cluster from your local machine, either:"
echo "  export KUBECONFIG=$LOCAL_KUBECONFIG"
echo "or merge it into your default config:"
echo "  KUBECONFIG=\$HOME/.kube/config:$LOCAL_KUBECONFIG kubectl config view --flatten > /tmp/kubeconfig-merged && mv /tmp/kubeconfig-merged \$HOME/.kube/config"