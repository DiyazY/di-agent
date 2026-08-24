#!/usr/bin/env bash
# 06a-switchboard.sh — build the switchboard image and deploy it to the
#                      kubeadm cluster (see 02-k8s.sh) as a Deployment.
#
# The switchboard is the central power-management authority between one or
# more gensets (see 06-genset.sh) and one or more consumers (see
# 06b-propulsion.sh): it aggregates genset supply and consumer power
# requests over Kafka and publishes each consumer's allocation. Deploy this
# before 06b-propulsion.sh so consumers have somewhere to request power from.
#
# Usage: ./06a-switchboard.sh [vm1 vm2 vm3]
#   VM names default to ubuntu-vm1 ubuntu-vm2 ubuntu-vm3. The switchboard pod
#   is scheduled on the third VM by default (override with SWITCHBOARD_VM).

set -euo pipefail

# ── colours ─────────────────────────────────────────────────────────────────
GREEN=$(tput setaf 2 2>/dev/null || echo "")
YELLOW=$(tput setaf 3 2>/dev/null || echo "")
RED=$(tput setaf 1 2>/dev/null || echo "")
RESET=$(tput sgr0 2>/dev/null || echo "")

info() { echo "${YELLOW}[switchboard] $*${RESET}"; }
ok()   { echo "${GREEN}[switchboard] $*${RESET}"; }
err()  { echo "${RED}[switchboard] $*${RESET}" >&2; }

# ── paths ────────────────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
POC_DIR="$(dirname "$SCRIPT_DIR")"
SWITCHBOARD_DIR="$POC_DIR/system/switchboard"
CONFIG_DIR="$POC_DIR/config"
ENV_FILE="$CONFIG_DIR/.env"
DEPLOYMENT_TMPL="$CONFIG_DIR/switchboard-deployment.yaml"

# ── config (see config/.env; existing exports still take priority) ──────────
if [ -f "$ENV_FILE" ]; then
    set -a
    # shellcheck disable=SC1090
    source "$ENV_FILE"
    set +a
fi

IMAGE_NAME="${SWITCHBOARD_IMAGE_NAME:-switchboard}"
IMAGE_TAG="${SWITCHBOARD_IMAGE_TAG:-latest}"

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
# Default to the third VM, alongside genset/propulsion (see 06-genset.sh),
# so it doesn't compete with the control plane or the Kafka broker.
SWITCHBOARD_VM="${SWITCHBOARD_VM:-${VMS[2]:-${VMS[-1]}}}"

# Kafka broker address (see 03-kafka.sh); defaults to the second VM.
KAFKA_VM="${KAFKA_VM:-${VMS[1]:-$CONTROL_PLANE_VM}}"
KAFKA_IP_FOR_BROKERS=$(get_vm_ip "$KAFKA_VM")
KAFKA_BROKERS="${KAFKA_BROKERS:-${KAFKA_IP_FOR_BROKERS}:9092}"
KAFKA_TOPIC="${SWITCHBOARD_KAFKA_TOPIC:-switchboard.telemetry}"
# Genset telemetry topic (see 06-genset.sh) the switchboard sums into supply.
GENSET_KAFKA_TOPIC="${GENSET_KAFKA_TOPIC:-genset.telemetry}"
# Battery telemetry topic (see 06c-battery.sh) also summed into supply.
BATTERY_KAFKA_TOPIC="${BATTERY_KAFKA_TOPIC:-battery.telemetry}"
# Power request topic (see 06b-propulsion.sh, 06d-auxload.sh) consumers publish demand to.
REQUEST_KAFKA_TOPIC="${REQUEST_KAFKA_TOPIC:-switchboard.requests}"

# ── image ─────────────────────────────────────────────────────────────────────
info "Building docker image ${IMAGE_NAME}:${IMAGE_TAG} from $SWITCHBOARD_DIR ..."
(
  cd "$SWITCHBOARD_DIR"
    docker build -t "${IMAGE_NAME}:${IMAGE_TAG}" .
) || {
    err "Docker build failed"
    exit 1
}
ok "Image ready: ${IMAGE_NAME}:${IMAGE_TAG}"

# ── distribute image (no registry: import straight into containerd) ─────────
SWITCHBOARD_IP=$(get_vm_ip "$SWITCHBOARD_VM")
if [ -z "$SWITCHBOARD_IP" ]; then
  err "Could not resolve IP for $SWITCHBOARD_VM"
    exit 1
fi
info "Importing image into containerd on $SWITCHBOARD_VM ($SWITCHBOARD_IP) ..."
docker save "${IMAGE_NAME}:${IMAGE_TAG}" | ssh_vm "$SWITCHBOARD_IP" "sudo ctr -n k8s.io images import -"
ok "Image imported"

# ── deploy ────────────────────────────────────────────────────────────────────
CP_IP=$(get_vm_ip "$CONTROL_PLANE_VM")
info "Applying switchboard manifest via kubectl on $CONTROL_PLANE_VM (pod scheduled on $SWITCHBOARD_VM) ..."
SWITCHBOARD_VM="$SWITCHBOARD_VM" IMAGE_NAME="$IMAGE_NAME" IMAGE_TAG="$IMAGE_TAG" \
KAFKA_BROKERS="$KAFKA_BROKERS" KAFKA_TOPIC="$KAFKA_TOPIC" \
GENSET_KAFKA_TOPIC="$GENSET_KAFKA_TOPIC" BATTERY_KAFKA_TOPIC="$BATTERY_KAFKA_TOPIC" \
REQUEST_KAFKA_TOPIC="$REQUEST_KAFKA_TOPIC" \
    envsubst '${SWITCHBOARD_VM} ${IMAGE_NAME} ${IMAGE_TAG} ${KAFKA_BROKERS} ${KAFKA_TOPIC} ${GENSET_KAFKA_TOPIC} ${BATTERY_KAFKA_TOPIC} ${REQUEST_KAFKA_TOPIC}' \
    < "$DEPLOYMENT_TMPL" \
    | ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config apply -f -"

ok "Deployment applied"

info "Waiting for switchboard pod to be Ready ..."
ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config wait --for=condition=Ready pod -l app=switchboard --timeout=180s"
ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config get pods -o wide -l app=switchboard"
ok "Switchboard streaming allocations to ${KAFKA_BROKERS} on topic ${KAFKA_TOPIC}"
