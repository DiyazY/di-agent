#!/usr/bin/env bash
# 03-kubeedge.sh - install KubeEdge CloudCore and EdgeCore.
#
# The first VM must already have a working Kubernetes control plane and
# kubectl access (for example, from 02-k3s.sh). Remaining VMs become edge
# nodes and must not also run kubelet/k3s-agent.
#
# Usage: ./03-kubeedge.sh [cloud-vm edge-vm ...]

set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

SSH_USER="${SSH_USER:-ubuntu}"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/id_ed25519_vms}"
SSH_OPTS=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i "$SSH_KEY")
KUBEEDGE_VERSION="${KUBEEDGE_VERSION:-v1.20.0}"
LOCAL_KUBECONFIG="${LOCAL_KUBECONFIG:-$HOME/.kube/config-poc2-kubeedge}"

info() { echo -e "${YELLOW}[kubeedge] $*${NC}"; }
ok()   { echo -e "${GREEN}[kubeedge] $*${NC}"; }
err()  { echo -e "${RED}[kubeedge] $*${NC}" >&2; }

get_vm_ip() {
    virsh domifaddr "$1" | awk '/ipv4/ {print $4}' | cut -d'/' -f1 | head -n1
}

ssh_vm() {
    local ip="$1"; shift
    ssh "${SSH_OPTS[@]}" "${SSH_USER}@${ip}" "$@"
}

install_keadm() {
    local ip="$1"
    local arch
    arch=$(ssh_vm "$ip" "dpkg --print-architecture")
    case "$arch" in
        amd64) arch=amd64 ;;
        arm64) arch=arm64 ;;
        *) err "Unsupported VM architecture: $arch"; return 1 ;;
    esac
    ssh_vm "$ip" "
        tmp_dir=\$(mktemp -d) &&
        curl -fL --retry 3 -o \"\$tmp_dir/keadm.tar.gz\" \
          https://github.com/kubeedge/kubeedge/releases/download/${KUBEEDGE_VERSION}/keadm-${KUBEEDGE_VERSION}-linux-${arch}.tar.gz &&
        tar -xzf \"\$tmp_dir/keadm.tar.gz\" -C \"\$tmp_dir\" &&
        sudo install -m 0755 \"\$tmp_dir/keadm-${KUBEEDGE_VERSION}-linux-${arch}/keadm/keadm\" /usr/local/bin/keadm &&
        rm -rf \"\$tmp_dir\"
    "
}

if [[ "$#" -lt 1 ]]; then
    VMS=(ubuntu-vm1 ubuntu-vm2 ubuntu-vm3)
else
    VMS=("$@")
fi

CLOUD_VM="${VMS[0]}"
EDGE_VMS=("${VMS[@]:1}")
declare -A VM_IP
for vm in "${VMS[@]}"; do
    VM_IP[$vm]=$(get_vm_ip "$vm")
    [[ -n "${VM_IP[$vm]}" ]] || { err "Could not resolve IP for $vm"; exit 1; }
done

CLOUD_IP="${VM_IP[$CLOUD_VM]}"
info "Installing keadm on cloud and edge nodes ..."
for vm in "${VMS[@]}"; do
    install_keadm "${VM_IP[$vm]}"
done

KUBECONFIG_PATH=$(ssh_vm "$CLOUD_IP" "
    if [[ -f /etc/rancher/k3s/k3s.yaml ]]; then
        echo /etc/rancher/k3s/k3s.yaml
    elif [[ -f /etc/kubernetes/admin.conf ]]; then
        echo /etc/kubernetes/admin.conf
    else
        exit 1
    fi
") || {
    err "No k3s or kubeadm kubeconfig found on $CLOUD_VM"
    exit 1
}

info "Initializing KubeEdge CloudCore on $CLOUD_VM ..."
ssh_vm "$CLOUD_IP" "sudo env KUBECONFIG='$KUBECONFIG_PATH' keadm init --advertise-address='$CLOUD_IP' --kubeedge-version='$KUBEEDGE_VERSION'"
TOKEN=$(ssh_vm "$CLOUD_IP" "sudo keadm gettoken")
for vm in "${EDGE_VMS[@]}"; do
    info "Joining $vm as a KubeEdge edge node ..."
    ssh_vm "${VM_IP[$vm]}" "sudo keadm join --cloudcore-ipport='$CLOUD_IP:10000' --edgenode-name='$vm' --kubeedge-version='$KUBEEDGE_VERSION' --token='$TOKEN'"
    ok "$vm joined KubeEdge"
done

mkdir -p "$(dirname "$LOCAL_KUBECONFIG")"
ssh_vm "$CLOUD_IP" "sudo cat '$KUBECONFIG_PATH'" | sed "s/127\.0\.0\.1/${CLOUD_IP}/g" > "$LOCAL_KUBECONFIG"
chmod 600 "$LOCAL_KUBECONFIG"
ok "KubeEdge CloudCore initialized. Kubeconfig saved to $LOCAL_KUBECONFIG"
echo "export KUBECONFIG=$LOCAL_KUBECONFIG"