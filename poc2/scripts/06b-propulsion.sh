#!/usr/bin/env bash
# 06b-propulsion.sh — build the propulsion image and deploy it to the kubeadm
#                     cluster (see 02-k8s.sh) as a Deployment that streams
#                     telemetry to the Kafka broker (see 03-kafka.sh).
#
# Usage: ./06b-propulsion.sh [vm1 vm2 vm3]
#   VM names default to ubuntu-vm1 ubuntu-vm2 ubuntu-vm3. The propulsion pod
#   is scheduled on the third VM by default (override with PROPULSION_VM).

set -euo pipefail

# ── colours ─────────────────────────────────────────────────────────────────
GREEN=$(tput setaf 2 2>/dev/null || echo "")
YELLOW=$(tput setaf 3 2>/dev/null || echo "")
RED=$(tput setaf 1 2>/dev/null || echo "")
RESET=$(tput sgr0 2>/dev/null || echo "")

info() { echo "${YELLOW}[propulsion] $*${RESET}"; }
ok()   { echo "${GREEN}[propulsion] $*${RESET}"; }
err()  { echo "${RED}[propulsion] $*${RESET}" >&2; }

# ── paths ────────────────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
POC_DIR="$(dirname "$SCRIPT_DIR")"
PROPULSION_DIR="$POC_DIR/system/propulsion"
CONFIG_DIR="$POC_DIR/config"
ENV_FILE="$CONFIG_DIR/.env"
DEPLOYMENT_TMPL="$CONFIG_DIR/propulsion-deployment.yaml"

# ── config (see config/.env; existing exports still take priority) ──────────
if [ -f "$ENV_FILE" ]; then
    set -a
    # shellcheck disable=SC1090
    source "$ENV_FILE"
    set +a
fi

IMAGE_NAME="${PROPULSION_IMAGE_NAME:-propulsion}"
IMAGE_TAG="${PROPULSION_IMAGE_TAG:-latest}"

# ── ssh / vm helpers (matches 02-k8s.sh / 03-kafka.sh / 06-genset.sh) ────────
SSH_USER="${SSH_USER:-ubuntu}"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/id_ed25519_vms}"
SSH_OPTS=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i "$SSH_KEY")

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
# Default to the third VM, alongside the genset (see 06-genset.sh), so it
# doesn't compete with the control plane or the Kafka broker.
PROPULSION_VM="${PROPULSION_VM:-${VMS[2]:-${VMS[-1]}}}"

# Kafka broker address (see 03-kafka.sh); defaults to the second VM.
KAFKA_VM="${KAFKA_VM:-${VMS[1]:-$CONTROL_PLANE_VM}}"
KAFKA_IP_FOR_BROKERS=$(get_vm_ip "$KAFKA_VM")
KAFKA_BROKERS="${KAFKA_BROKERS:-${KAFKA_IP_FOR_BROKERS}:9092}"
KAFKA_TOPIC="${PROPULSION_KAFKA_TOPIC:-propulsion.telemetry}"

# ── image ─────────────────────────────────────────────────────────────────────
info "Building docker image ${IMAGE_NAME}:${IMAGE_TAG} from $PROPULSION_DIR ..."
(
  cd "$PROPULSION_DIR"
    docker build -t "${IMAGE_NAME}:${IMAGE_TAG}" .
) || {
    err "Docker build failed"
    exit 1
}
ok "Image ready: ${IMAGE_NAME}:${IMAGE_TAG}"

# ── distribute image (no registry: import straight into containerd) ─────────
PROPULSION_IP=$(get_vm_ip "$PROPULSION_VM")
if [ -z "$PROPULSION_IP" ]; then
  err "Could not resolve IP for $PROPULSION_VM"
    exit 1
fi
info "Importing image into containerd on $PROPULSION_VM ($PROPULSION_IP) ..."
docker save "${IMAGE_NAME}:${IMAGE_TAG}" | ssh_vm "$PROPULSION_IP" "sudo ctr -n k8s.io images import -"
ok "Image imported"

# ── deploy ────────────────────────────────────────────────────────────────────
CP_IP=$(get_vm_ip "$CONTROL_PLANE_VM")
info "Applying propulsion manifest via kubectl on $CONTROL_PLANE_VM (pod scheduled on $PROPULSION_VM) ..."
PROPULSION_VM="$PROPULSION_VM" IMAGE_NAME="$IMAGE_NAME" IMAGE_TAG="$IMAGE_TAG" \
KAFKA_BROKERS="$KAFKA_BROKERS" KAFKA_TOPIC="$KAFKA_TOPIC" \
    envsubst '${PROPULSION_VM} ${IMAGE_NAME} ${IMAGE_TAG} ${KAFKA_BROKERS} ${KAFKA_TOPIC}' \
    < "$DEPLOYMENT_TMPL" \
    | ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config apply -f -"

ok "Deployment applied"

info "Waiting for propulsion pod to be Ready ..."
ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config wait --for=condition=Ready pod -l app=propulsion --timeout=180s"
ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config get pods -o wide -l app=propulsion"
ok "Propulsion streaming to ${KAFKA_BROKERS} on topic ${KAFKA_TOPIC}"
