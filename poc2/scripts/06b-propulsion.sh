#!/usr/bin/env bash
# 06b-propulsion.sh — build the propulsion image and deploy it to the kubeadm
#                     cluster (see 02-k8s.sh) as PROPULSION_COUNT Deployments
#                     (default 2), each streaming telemetry to the Kafka
#                     broker (see 03-kafka.sh) under its own propulsion-<N>
#                     identity.
#
# Each propulsion drive requests power from, and is capped by allocations
# from, the switchboard (see 06a-switchboard.sh) rather than reading genset
# telemetry directly. Deploy the switchboard first.
#
# Usage: ./06b-propulsion.sh [vm1 vm2 vm3]
#   VM names default to ubuntu-vm1 ubuntu-vm2 ubuntu-vm3. All propulsion
#   instances are scheduled on the third VM by default (override with
#   PROPULSION_VM, or set PROPULSION_VMS to a comma-separated list of VM
#   names to spread instances round-robin across several VMs).

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
SERVICE_TMPL="$CONFIG_DIR/propulsion-service.yaml"

# ── config (see config/.env; existing exports still take priority) ──────────
if [ -f "$ENV_FILE" ]; then
    set -a
    # shellcheck disable=SC1090
    source "$ENV_FILE"
    set +a
fi

IMAGE_NAME="${PROPULSION_IMAGE_NAME:-propulsion}"
IMAGE_TAG="${PROPULSION_IMAGE_TAG:-latest}"
PROPULSION_COUNT="${PROPULSION_COUNT:-2}"

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
# doesn't compete with the control plane or the Kafka broker. PROPULSION_VMS
# (comma-separated) overrides this to spread instances round-robin across
# several VMs; otherwise every instance lands on the same PROPULSION_VM.
if [ -n "${PROPULSION_VMS:-}" ]; then
    IFS=',' read -r -a PROPULSION_VM_ARR <<< "$PROPULSION_VMS"
else
    PROPULSION_VM_ARR=("${PROPULSION_VM:-${VMS[2]:-${VMS[-1]}}}")
fi
# Per-instance load-shedding priority (comma-separated); defaults to 1 for
# every instance if not set.
if [ -n "${PROPULSION_PRIORITIES:-}" ]; then
    IFS=',' read -r -a PROPULSION_PRIORITY_ARR <<< "$PROPULSION_PRIORITIES"
else
    PROPULSION_PRIORITY_ARR=(1)
fi

# Kafka broker address (see 03-kafka.sh); defaults to the second VM.
KAFKA_VM="${KAFKA_VM:-${VMS[1]:-$CONTROL_PLANE_VM}}"
KAFKA_IP_FOR_BROKERS=$(get_vm_ip "$KAFKA_VM")
KAFKA_BROKERS="${KAFKA_BROKERS:-${KAFKA_IP_FOR_BROKERS}:9092}"
KAFKA_TOPIC="${PROPULSION_KAFKA_TOPIC:-propulsion.telemetry}"
# Switchboard topics (see 06a-switchboard.sh): requests are published there,
# allocations are consumed from there to cap this pod's own load.
REQUEST_KAFKA_TOPIC="${REQUEST_KAFKA_TOPIC:-switchboard.requests}"
ALLOCATION_KAFKA_TOPIC="${ALLOCATION_KAFKA_TOPIC:-switchboard.telemetry}"

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

CP_IP=$(get_vm_ip "$CONTROL_PLANE_VM")
declare -A IMPORTED_VMS=()

for i in $(seq 1 "$PROPULSION_COUNT"); do
    PROPULSION_NAME="propulsion-${i}"
    PROPULSION_ID="propulsion-${i}"
    PROPULSION_VM="${PROPULSION_VM_ARR[$(( (i - 1) % ${#PROPULSION_VM_ARR[@]} ))]}"
    PROPULSION_PRIORITY="${PROPULSION_PRIORITY_ARR[$(( (i - 1) % ${#PROPULSION_PRIORITY_ARR[@]} ))]}"

    # ── distribute image (no registry: import straight into containerd) ─────
    PROPULSION_IP=$(get_vm_ip "$PROPULSION_VM")
    if [ -z "$PROPULSION_IP" ]; then
        err "Could not resolve IP for $PROPULSION_VM"
        exit 1
    fi
    if [ -z "${IMPORTED_VMS[$PROPULSION_VM]:-}" ]; then
        info "Importing image into containerd on $PROPULSION_VM ($PROPULSION_IP) ..."
        docker save "${IMAGE_NAME}:${IMAGE_TAG}" | ssh_vm "$PROPULSION_IP" "sudo ctr -n k8s.io images import -"
        IMPORTED_VMS[$PROPULSION_VM]=1
        ok "Image imported on $PROPULSION_VM"
    fi

    # ── deploy ────────────────────────────────────────────────────────────────
    info "Applying $PROPULSION_NAME manifest via kubectl on $CONTROL_PLANE_VM (pod scheduled on $PROPULSION_VM) ..."
    PROPULSION_NAME="$PROPULSION_NAME" PROPULSION_ID="$PROPULSION_ID" PROPULSION_VM="$PROPULSION_VM" \
    PROPULSION_PRIORITY="$PROPULSION_PRIORITY" IMAGE_NAME="$IMAGE_NAME" IMAGE_TAG="$IMAGE_TAG" \
    KAFKA_BROKERS="$KAFKA_BROKERS" KAFKA_TOPIC="$KAFKA_TOPIC" \
    REQUEST_KAFKA_TOPIC="$REQUEST_KAFKA_TOPIC" ALLOCATION_KAFKA_TOPIC="$ALLOCATION_KAFKA_TOPIC" \
        envsubst '${PROPULSION_NAME} ${PROPULSION_ID} ${PROPULSION_VM} ${PROPULSION_PRIORITY} ${IMAGE_NAME} ${IMAGE_TAG} ${KAFKA_BROKERS} ${KAFKA_TOPIC} ${REQUEST_KAFKA_TOPIC} ${ALLOCATION_KAFKA_TOPIC}' \
        < "$DEPLOYMENT_TMPL" \
        | ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config apply -f -"
    PROPULSION_NAME="$PROPULSION_NAME" PROPULSION_ID="$PROPULSION_ID" \
        envsubst '${PROPULSION_NAME} ${PROPULSION_ID}' \
        < "$SERVICE_TMPL" \
        | ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config apply -f -"
    ok "$PROPULSION_NAME deployment/service applied"
done

info "Waiting for propulsion pods to be Ready ..."
ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config wait --for=condition=Ready pod -l app=propulsion --timeout=180s"
ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config get pods -o wide -l app=propulsion"
ok "${PROPULSION_COUNT} propulsion instance(s) streaming to ${KAFKA_BROKERS} on topic ${KAFKA_TOPIC}"
