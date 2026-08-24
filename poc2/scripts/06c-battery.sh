#!/usr/bin/env bash
# 06c-battery.sh — build the battery image and deploy it to the kubeadm
#                 cluster (see 02-k8s.sh) as BATTERY_COUNT Deployments
#                 (default 1), each streaming telemetry to the Kafka broker
#                 (see 03-kafka.sh) under its own battery-<N> identity.
#
# The battery publishes power to the bus like a genset (see 06-genset.sh);
# the switchboard (see 06a-switchboard.sh) sums both into available supply.
# Deploy the switchboard first.
#
# Usage: ./06c-battery.sh [vm1 vm2 vm3]
#   VM names default to ubuntu-vm1 ubuntu-vm2 ubuntu-vm3. All battery
#   instances are scheduled on the third VM by default (override with
#   BATTERY_VM, or set BATTERY_VMS to a comma-separated list of VM names to
#   spread instances round-robin across several VMs).

set -euo pipefail

# ── colours ─────────────────────────────────────────────────────────────────
GREEN=$(tput setaf 2 2>/dev/null || echo "")
YELLOW=$(tput setaf 3 2>/dev/null || echo "")
RED=$(tput setaf 1 2>/dev/null || echo "")
RESET=$(tput sgr0 2>/dev/null || echo "")

info() { echo "${YELLOW}[battery] $*${RESET}"; }
ok()   { echo "${GREEN}[battery] $*${RESET}"; }
err()  { echo "${RED}[battery] $*${RESET}" >&2; }

# ── paths ────────────────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
POC_DIR="$(dirname "$SCRIPT_DIR")"
BATTERY_DIR="$POC_DIR/system/battery"
CONFIG_DIR="$POC_DIR/config"
ENV_FILE="$CONFIG_DIR/.env"
DEPLOYMENT_TMPL="$CONFIG_DIR/battery-deployment.yaml"
SERVICE_TMPL="$CONFIG_DIR/battery-service.yaml"

# ── config (see config/.env; existing exports still take priority) ──────────
if [ -f "$ENV_FILE" ]; then
    set -a
    # shellcheck disable=SC1090
    source "$ENV_FILE"
    set +a
fi

IMAGE_NAME="${BATTERY_IMAGE_NAME:-battery}"
IMAGE_TAG="${BATTERY_IMAGE_TAG:-latest}"
BATTERY_COUNT="${BATTERY_COUNT:-1}"

# ── ssh / vm helpers (matches 02-k8s.sh / 03-kafka.sh / 04-agent.sh) ─────────
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
# Default to the third VM so the battery doesn't compete with the control
# plane or the Kafka broker (see 03-kafka.sh); falls back to the last VM.
# BATTERY_VMS (comma-separated) overrides this to spread instances
# round-robin across several VMs; otherwise every instance lands on the
# same BATTERY_VM.
if [ -n "${BATTERY_VMS:-}" ]; then
    IFS=',' read -r -a BATTERY_VM_ARR <<< "$BATTERY_VMS"
else
    BATTERY_VM_ARR=("${BATTERY_VM:-${VMS[2]:-${VMS[-1]}}}")
fi

# Kafka broker address (see 03-kafka.sh); defaults to the second VM.
KAFKA_VM="${KAFKA_VM:-${VMS[1]:-$CONTROL_PLANE_VM}}"
KAFKA_IP_FOR_BROKERS=$(get_vm_ip "$KAFKA_VM")
KAFKA_BROKERS="${KAFKA_BROKERS:-${KAFKA_IP_FOR_BROKERS}:9092}"
KAFKA_TOPIC="${BATTERY_KAFKA_TOPIC:-battery.telemetry}"

# ── image ─────────────────────────────────────────────────────────────────────
info "Building docker image ${IMAGE_NAME}:${IMAGE_TAG} from $BATTERY_DIR ..."
(
  cd "$BATTERY_DIR"
    docker build -t "${IMAGE_NAME}:${IMAGE_TAG}" .
) || {
    err "Docker build failed"
    exit 1
}
ok "Image ready: ${IMAGE_NAME}:${IMAGE_TAG}"

CP_IP=$(get_vm_ip "$CONTROL_PLANE_VM")
declare -A IMPORTED_VMS=()

for i in $(seq 1 "$BATTERY_COUNT"); do
    BATTERY_NAME="battery-${i}"
    BATTERY_ID="battery-${i}"
    BATTERY_VM="${BATTERY_VM_ARR[$(( (i - 1) % ${#BATTERY_VM_ARR[@]} ))]}"

    # ── distribute image (no registry: import straight into containerd) ─────
    BATTERY_IP=$(get_vm_ip "$BATTERY_VM")
    if [ -z "$BATTERY_IP" ]; then
        err "Could not resolve IP for $BATTERY_VM"
        exit 1
    fi
    if [ -z "${IMPORTED_VMS[$BATTERY_VM]:-}" ]; then
        info "Importing image into containerd on $BATTERY_VM ($BATTERY_IP) ..."
        docker save "${IMAGE_NAME}:${IMAGE_TAG}" | ssh_vm "$BATTERY_IP" "sudo ctr -n k8s.io images import -"
        IMPORTED_VMS[$BATTERY_VM]=1
        ok "Image imported on $BATTERY_VM"
    fi

    # ── deploy ────────────────────────────────────────────────────────────────
    info "Applying $BATTERY_NAME manifest via kubectl on $CONTROL_PLANE_VM (pod scheduled on $BATTERY_VM) ..."
    BATTERY_NAME="$BATTERY_NAME" BATTERY_ID="$BATTERY_ID" BATTERY_VM="$BATTERY_VM" \
    IMAGE_NAME="$IMAGE_NAME" IMAGE_TAG="$IMAGE_TAG" \
    KAFKA_BROKERS="$KAFKA_BROKERS" KAFKA_TOPIC="$KAFKA_TOPIC" \
        envsubst '${BATTERY_NAME} ${BATTERY_ID} ${BATTERY_VM} ${IMAGE_NAME} ${IMAGE_TAG} ${KAFKA_BROKERS} ${KAFKA_TOPIC}' \
        < "$DEPLOYMENT_TMPL" \
        | ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config apply -f -"
    BATTERY_NAME="$BATTERY_NAME" BATTERY_ID="$BATTERY_ID" \
        envsubst '${BATTERY_NAME} ${BATTERY_ID}' \
        < "$SERVICE_TMPL" \
        | ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config apply -f -"
    ok "$BATTERY_NAME deployment/service applied"
done

info "Waiting for battery pods to be Ready ..."
ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config wait --for=condition=Ready pod -l app=battery --timeout=180s"
ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config get pods -o wide -l app=battery"
ok "${BATTERY_COUNT} battery instance(s) streaming to ${KAFKA_BROKERS} on topic ${KAFKA_TOPIC}"
