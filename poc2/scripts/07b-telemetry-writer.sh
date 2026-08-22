#!/usr/bin/env bash
# 07b-telemetry-writer.sh — build and deploy a Kafka -> InfluxDB bridge that
#                            consumes genset telemetry (see 06-genset.sh)
#                            and continuously writes it into InfluxDB (see
#                            07-influxdb.sh) so it can be queried/graphed
#                            (see 08-grafana.sh).
#
# Usage: ./07b-telemetry-writer.sh [vm1 vm2 vm3]
#   VM names default to ubuntu-vm1 ubuntu-vm2 ubuntu-vm3. The writer pod is
#   scheduled on the second VM by default (override with WRITER_VM),
#   alongside Kafka and InfluxDB. It runs with hostNetwork so it can reach
#   both directly.

set -euo pipefail

# ── colours ─────────────────────────────────────────────────────────────────
GREEN=$(tput setaf 2 2>/dev/null || echo "")
YELLOW=$(tput setaf 3 2>/dev/null || echo "")
RED=$(tput setaf 1 2>/dev/null || echo "")
RESET=$(tput sgr0 2>/dev/null || echo "")

info() { echo "${YELLOW}[telemetry-writer] $*${RESET}"; }
ok()   { echo "${GREEN}[telemetry-writer] $*${RESET}"; }
err()  { echo "${RED}[telemetry-writer] $*${RESET}" >&2; }

# ── paths ────────────────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
POC_DIR="$(dirname "$SCRIPT_DIR")"
WRITER_DIR="$POC_DIR/system/telemetry-writer"
CONFIG_DIR="$POC_DIR/config"
ENV_FILE="$CONFIG_DIR/.env"
DEPLOYMENT_TMPL="$CONFIG_DIR/telemetry-writer-deployment.yaml"

# ── config (existing exports still take priority) ───────────────────────────
if [ -f "$ENV_FILE" ]; then
    set -a
    # shellcheck disable=SC1090
    source "$ENV_FILE"
    set +a
fi
IMAGE_NAME="${TELEMETRY_WRITER_IMAGE_NAME:-telemetry-writer}"
IMAGE_TAG="${TELEMETRY_WRITER_IMAGE_TAG:-latest}"

# ── ssh / vm helpers (matches 06-genset.sh / 07-influxdb.sh) ─────────────────
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
# Default to the second VM, alongside Kafka and InfluxDB.
WRITER_VM="${WRITER_VM:-${VMS[1]:-$CONTROL_PLANE_VM}}"

# Kafka broker address (see 03-kafka.sh); defaults to the second VM.
KAFKA_VM="${KAFKA_VM:-${VMS[1]:-$CONTROL_PLANE_VM}}"
KAFKA_IP_FOR_BROKERS=$(get_vm_ip "$KAFKA_VM")
KAFKA_BROKERS="${KAFKA_BROKERS:-${KAFKA_IP_FOR_BROKERS}:9092}"
GENSET_KAFKA_TOPIC="${GENSET_KAFKA_TOPIC:-genset.telemetry}"
PROPULSION_KAFKA_TOPIC="${PROPULSION_KAFKA_TOPIC:-propulsion.telemetry}"
KAFKA_GROUP_ID="${KAFKA_GROUP_ID:-telemetry-writer}"

# InfluxDB connection details (see 07-influxdb.sh); defaults to the second VM.
INFLUXDB_PORT="${INFLUXDB_PORT:-8086}"
INFLUXDB_ORG="${INFLUXDB_ORG:-di-agent}"
INFLUXDB_BUCKET="${INFLUXDB_BUCKET:-telemetry}"
INFLUXDB_ADMIN_TOKEN="${INFLUXDB_ADMIN_TOKEN:-di-agent-genset-admin-token}"
INFLUXDB_VM="${INFLUXDB_VM:-${VMS[1]:-$CONTROL_PLANE_VM}}"
INFLUXDB_IP=$(get_vm_ip "$INFLUXDB_VM")
INFLUXDB_URL="${INFLUXDB_URL:-http://${INFLUXDB_IP}:${INFLUXDB_PORT}}"

# ── image ─────────────────────────────────────────────────────────────────────
info "Building docker image ${IMAGE_NAME}:${IMAGE_TAG} from $WRITER_DIR ..."
(
  cd "$WRITER_DIR"
  docker build -t "${IMAGE_NAME}:${IMAGE_TAG}" .
) || {
    err "Docker build failed"
    exit 1
}
ok "Image ready: ${IMAGE_NAME}:${IMAGE_TAG}"

# ── distribute image (no registry: import straight into containerd) ─────────
WRITER_IP=$(get_vm_ip "$WRITER_VM")
if [ -z "$WRITER_IP" ]; then
    err "Could not resolve IP for $WRITER_VM"
    exit 1
fi
info "Importing image into containerd on $WRITER_VM ($WRITER_IP) ..."
docker save "${IMAGE_NAME}:${IMAGE_TAG}" | ssh_vm "$WRITER_IP" "sudo ctr -n k8s.io images import -"
ok "Image imported"

# ── deploy ────────────────────────────────────────────────────────────────────
CP_IP=$(get_vm_ip "$CONTROL_PLANE_VM")
info "Applying telemetry-writer manifest via kubectl on $CONTROL_PLANE_VM (pod scheduled on $WRITER_VM) ..."
WRITER_VM="$WRITER_VM" IMAGE_NAME="$IMAGE_NAME" IMAGE_TAG="$IMAGE_TAG" \
KAFKA_BROKERS="$KAFKA_BROKERS" GENSET_KAFKA_TOPIC="$GENSET_KAFKA_TOPIC" PROPULSION_KAFKA_TOPIC="$PROPULSION_KAFKA_TOPIC" \
KAFKA_GROUP_ID="$KAFKA_GROUP_ID" \
INFLUXDB_URL="$INFLUXDB_URL" INFLUXDB_ORG="$INFLUXDB_ORG" INFLUXDB_BUCKET="$INFLUXDB_BUCKET" \
INFLUXDB_ADMIN_TOKEN="$INFLUXDB_ADMIN_TOKEN" \
    envsubst '${WRITER_VM} ${IMAGE_NAME} ${IMAGE_TAG} ${KAFKA_BROKERS} ${GENSET_KAFKA_TOPIC} ${PROPULSION_KAFKA_TOPIC} ${KAFKA_GROUP_ID} ${INFLUXDB_URL} ${INFLUXDB_ORG} ${INFLUXDB_BUCKET} ${INFLUXDB_ADMIN_TOKEN}' \
    < "$DEPLOYMENT_TMPL" \
    | ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config apply -f -"

ok "Deployment applied"

info "Waiting for telemetry-writer pod to be Ready ..."
ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config wait --for=condition=Ready pod -l app=telemetry-writer --timeout=180s"
ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config get pods -o wide -l app=telemetry-writer"
ok "Writing ${GENSET_KAFKA_TOPIC} and ${PROPULSION_KAFKA_TOPIC} from ${KAFKA_BROKERS} into ${INFLUXDB_URL} (org=${INFLUXDB_ORG}, bucket=${INFLUXDB_BUCKET})"
