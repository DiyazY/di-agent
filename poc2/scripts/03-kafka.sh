#!/usr/bin/env bash
# 05-kafka.sh — deploy a single-broker Apache Kafka (KRaft mode, no
#               zookeeper) onto a kubeadm worker node (see 02-k8s.sh),
#               reachable at <kafka-vm-ip>:9092.
#
# Usage: ./05-kafka.sh [vm1 vm2 vm3]
#   VM names default to ubuntu-vm1 ubuntu-vm2 ubuntu-vm3. The first VM is
#   treated as the control-plane; the Kafka pod is scheduled on the second
#   VM (override with KAFKA_VM) so it doesn't compete with the control
#   plane. It runs with hostNetwork, so it is directly reachable by
#   di-agent pods which also run with hostNetwork — see 03-agent.sh.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONFIG_DIR="$(cd "$SCRIPT_DIR/../config" && pwd)"
ENV_FILE="$CONFIG_DIR/.env"
DEPLOYMENT_TMPL="$CONFIG_DIR/kafka-deployment.yaml"

# ── colours ─────────────────────────────────────────────────────────────────
GREEN=$(tput setaf 2 2>/dev/null || echo "")
YELLOW=$(tput setaf 3 2>/dev/null || echo "")
RED=$(tput setaf 1 2>/dev/null || echo "")
RESET=$(tput sgr0 2>/dev/null || echo "")

info() { echo "${YELLOW}[kafka] $*${RESET}"; }
ok()   { echo "${GREEN}[kafka] $*${RESET}"; }
err()  { echo "${RED}[kafka] $*${RESET}" >&2; }

# ── config (see config/.env; existing exports still take priority) ──────────
if [ -f "$ENV_FILE" ]; then
    set -a
    # shellcheck disable=SC1090
    source "$ENV_FILE"
    set +a
fi

# ── ssh / vm helpers (matches 02-k8s.sh / 03-agent.sh) ───────────────────────
SSH_USER="${SSH_USER:-ubuntu}"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/id_ed25519_vms}"
SSH_OPTS=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i "$SSH_KEY")

KAFKA_IMAGE="${KAFKA_IMAGE:-apache/kafka}"
KAFKA_IMAGE_TAG="${KAFKA_IMAGE_TAG:-latest}"
KAFKA_PORT="${KAFKA_PORT:-9092}"
KAFKA_NAMESPACE="${KAFKA_NAMESPACE:-default}"
KAFKA_CLUSTER_ID="${KAFKA_CLUSTER_ID:-ZGlhZ2VudC1rYWZrYS0}"

get_vm_ip() {
    virsh domifaddr "$1" | awk '/ipv4/ {print $4}' | cut -d'/' -f1 | head -n1
}

ssh_vm() {
    local ip="$1"; shift
    ssh "${SSH_OPTS[@]}" "${SSH_USER}@${ip}" "$@"
}

# ── args ─────────────────────────────────────────────────────────────────────
if [ "$#" -eq 0 ]; then
    VMS=(ubuntu-vm1 ubuntu-vm2 ubuntu-vm3)
else
    VMS=("$@")
fi

CONTROL_PLANE_VM="${VMS[0]}"
# Default to the second VM so Kafka doesn't share the control-plane node;
# falls back to the control-plane if no other VM was given.
KAFKA_VM="${KAFKA_VM:-${VMS[1]:-$CONTROL_PLANE_VM}}"
CP_IP=$(get_vm_ip "$KAFKA_VM")
if [ -z "$CP_IP" ]; then
    err "Could not resolve IP for $KAFKA_VM"
    exit 1
fi

# ── image ─────────────────────────────────────────────────────────────────────
info "Pulling ${KAFKA_IMAGE}:${KAFKA_IMAGE_TAG} locally ..."
docker pull "${KAFKA_IMAGE}:${KAFKA_IMAGE_TAG}"
ok "Image ready: ${KAFKA_IMAGE}:${KAFKA_IMAGE_TAG}"

# ── distribute image (no registry: import straight into containerd) ─────────
info "Importing image into containerd on $KAFKA_VM ($CP_IP) ..."
docker save "${KAFKA_IMAGE}:${KAFKA_IMAGE_TAG}" | ssh_vm "$CP_IP" "sudo ctr -n k8s.io images import -"
ok "Image imported"

# kubectl runs against the control-plane's API server even though the pod
# itself is scheduled onto $KAFKA_VM via nodeSelector below.
API_IP=$(get_vm_ip "$CONTROL_PLANE_VM")

# ── deploy ────────────────────────────────────────────────────────────────────
info "Applying Kafka manifests via kubectl on $CONTROL_PLANE_VM (pod scheduled on $KAFKA_VM) ..."
KAFKA_VM="$KAFKA_VM" CP_IP="$CP_IP" KAFKA_IMAGE="$KAFKA_IMAGE" KAFKA_IMAGE_TAG="$KAFKA_IMAGE_TAG" \
KAFKA_PORT="$KAFKA_PORT" KAFKA_CLUSTER_ID="$KAFKA_CLUSTER_ID" \
    envsubst '${KAFKA_VM} ${CP_IP} ${KAFKA_IMAGE} ${KAFKA_IMAGE_TAG} ${KAFKA_PORT} ${KAFKA_CLUSTER_ID}' \
    < "$DEPLOYMENT_TMPL" \
    | ssh_vm "$API_IP" "kubectl --kubeconfig=\$HOME/.kube/config apply -n ${KAFKA_NAMESPACE} -f -"

ok "Deployment applied"

info "Waiting for Kafka pod to be Ready ..."
ssh_vm "$API_IP" "kubectl --kubeconfig=\$HOME/.kube/config wait -n ${KAFKA_NAMESPACE} --for=condition=Ready pod -l app=kafka --timeout=180s"
ssh_vm "$API_IP" "kubectl --kubeconfig=\$HOME/.kube/config get pods -n ${KAFKA_NAMESPACE} -o wide -l app=kafka"
ok "Kafka broker Ready at ${CP_IP}:${KAFKA_PORT}"
