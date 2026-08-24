#!/usr/bin/env bash
# 06d-auxload.sh — build the auxiliary (hotel) load image and deploy it to
#                  the kubeadm cluster (see 02-k8s.sh) as AUXLOAD_COUNT
#                  Deployments (default 1), each streaming telemetry to the
#                  Kafka broker (see 03-kafka.sh) under its own auxload-<N>
#                  identity.
#
# The auxiliary load requests power from, and is capped by allocations
# from, the switchboard (see 06a-switchboard.sh), same as propulsion (see
# 06b-propulsion.sh); its target load ratio is adjustable via API too.
# Deploy the switchboard first.
#
# Usage: ./06d-auxload.sh [vm1 vm2 vm3]
#   VM names default to ubuntu-vm1 ubuntu-vm2 ubuntu-vm3. All auxload
#   instances are scheduled on the third VM by default (override with
#   AUXLOAD_VM, or set AUXLOAD_VMS to a comma-separated list of VM names to
#   spread instances round-robin across several VMs).

set -euo pipefail

# ── colours ─────────────────────────────────────────────────────────────────
GREEN=$(tput setaf 2 2>/dev/null || echo "")
YELLOW=$(tput setaf 3 2>/dev/null || echo "")
RED=$(tput setaf 1 2>/dev/null || echo "")
RESET=$(tput sgr0 2>/dev/null || echo "")

info() { echo "${YELLOW}[auxload] $*${RESET}"; }
ok()   { echo "${GREEN}[auxload] $*${RESET}"; }
err()  { echo "${RED}[auxload] $*${RESET}" >&2; }

# ── paths ────────────────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
POC_DIR="$(dirname "$SCRIPT_DIR")"
AUXLOAD_DIR="$POC_DIR/system/auxiliary-load"
CONFIG_DIR="$POC_DIR/config"
ENV_FILE="$CONFIG_DIR/.env"
DEPLOYMENT_TMPL="$CONFIG_DIR/auxload-deployment.yaml"
SERVICE_TMPL="$CONFIG_DIR/auxload-service.yaml"

# ── config (see config/.env; existing exports still take priority) ──────────
if [ -f "$ENV_FILE" ]; then
    set -a
    # shellcheck disable=SC1090
    source "$ENV_FILE"
    set +a
fi

IMAGE_NAME="${AUXLOAD_IMAGE_NAME:-auxload}"
IMAGE_TAG="${AUXLOAD_IMAGE_TAG:-latest}"
AUXLOAD_COUNT="${AUXLOAD_COUNT:-1}"
INITIAL_LOAD_RATIO="${INITIAL_LOAD_RATIO:-0.5}"

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
# Default to the third VM, alongside propulsion (see 06b-propulsion.sh), so
# it doesn't compete with the control plane or the Kafka broker. AUXLOAD_VMS
# (comma-separated) overrides this to spread instances round-robin across
# several VMs; otherwise every instance lands on the same AUXLOAD_VM.
if [ -n "${AUXLOAD_VMS:-}" ]; then
    IFS=',' read -r -a AUXLOAD_VM_ARR <<< "$AUXLOAD_VMS"
else
    AUXLOAD_VM_ARR=("${AUXLOAD_VM:-${VMS[2]:-${VMS[-1]}}}")
fi

# Kafka broker address (see 03-kafka.sh); defaults to the second VM.
KAFKA_VM="${KAFKA_VM:-${VMS[1]:-$CONTROL_PLANE_VM}}"
KAFKA_IP_FOR_BROKERS=$(get_vm_ip "$KAFKA_VM")
KAFKA_BROKERS="${KAFKA_BROKERS:-${KAFKA_IP_FOR_BROKERS}:9092}"
KAFKA_TOPIC="${AUXLOAD_KAFKA_TOPIC:-auxload.telemetry}"
# Switchboard topics (see 06a-switchboard.sh): requests are published there,
# allocations are consumed from there to cap this pod's own load.
REQUEST_KAFKA_TOPIC="${REQUEST_KAFKA_TOPIC:-switchboard.requests}"
ALLOCATION_KAFKA_TOPIC="${ALLOCATION_KAFKA_TOPIC:-switchboard.telemetry}"

# ── image ─────────────────────────────────────────────────────────────────────
info "Building docker image ${IMAGE_NAME}:${IMAGE_TAG} from $AUXLOAD_DIR ..."
(
  cd "$AUXLOAD_DIR"
    docker build -t "${IMAGE_NAME}:${IMAGE_TAG}" .
) || {
    err "Docker build failed"
    exit 1
}
ok "Image ready: ${IMAGE_NAME}:${IMAGE_TAG}"

CP_IP=$(get_vm_ip "$CONTROL_PLANE_VM")
declare -A IMPORTED_VMS=()

for i in $(seq 1 "$AUXLOAD_COUNT"); do
    AUXLOAD_NAME="auxload-${i}"
    AUXLOAD_ID="auxload-${i}"
    AUXLOAD_VM="${AUXLOAD_VM_ARR[$(( (i - 1) % ${#AUXLOAD_VM_ARR[@]} ))]}"

    # ── distribute image (no registry: import straight into containerd) ─────
    AUXLOAD_IP=$(get_vm_ip "$AUXLOAD_VM")
    if [ -z "$AUXLOAD_IP" ]; then
        err "Could not resolve IP for $AUXLOAD_VM"
        exit 1
    fi
    if [ -z "${IMPORTED_VMS[$AUXLOAD_VM]:-}" ]; then
        info "Importing image into containerd on $AUXLOAD_VM ($AUXLOAD_IP) ..."
        docker save "${IMAGE_NAME}:${IMAGE_TAG}" | ssh_vm "$AUXLOAD_IP" "sudo ctr -n k8s.io images import -"
        IMPORTED_VMS[$AUXLOAD_VM]=1
        ok "Image imported on $AUXLOAD_VM"
    fi

    # ── deploy ────────────────────────────────────────────────────────────────
    info "Applying $AUXLOAD_NAME manifest via kubectl on $CONTROL_PLANE_VM (pod scheduled on $AUXLOAD_VM) ..."
    AUXLOAD_NAME="$AUXLOAD_NAME" AUXLOAD_ID="$AUXLOAD_ID" AUXLOAD_VM="$AUXLOAD_VM" \
    INITIAL_LOAD_RATIO="$INITIAL_LOAD_RATIO" IMAGE_NAME="$IMAGE_NAME" IMAGE_TAG="$IMAGE_TAG" \
    KAFKA_BROKERS="$KAFKA_BROKERS" KAFKA_TOPIC="$KAFKA_TOPIC" \
    REQUEST_KAFKA_TOPIC="$REQUEST_KAFKA_TOPIC" ALLOCATION_KAFKA_TOPIC="$ALLOCATION_KAFKA_TOPIC" \
        envsubst '${AUXLOAD_NAME} ${AUXLOAD_ID} ${AUXLOAD_VM} ${INITIAL_LOAD_RATIO} ${IMAGE_NAME} ${IMAGE_TAG} ${KAFKA_BROKERS} ${KAFKA_TOPIC} ${REQUEST_KAFKA_TOPIC} ${ALLOCATION_KAFKA_TOPIC}' \
        < "$DEPLOYMENT_TMPL" \
        | ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config apply -f -"
    AUXLOAD_NAME="$AUXLOAD_NAME" AUXLOAD_ID="$AUXLOAD_ID" \
        envsubst '${AUXLOAD_NAME} ${AUXLOAD_ID}' \
        < "$SERVICE_TMPL" \
        | ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config apply -f -"
    ok "$AUXLOAD_NAME deployment/service applied"
done

info "Waiting for auxload pods to be Ready ..."
ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config wait --for=condition=Ready pod -l app=auxload --timeout=180s"
ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config get pods -o wide -l app=auxload"
ok "${AUXLOAD_COUNT} auxload instance(s) streaming to ${KAFKA_BROKERS} on topic ${KAFKA_TOPIC}"
