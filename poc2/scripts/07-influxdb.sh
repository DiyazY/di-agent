#!/usr/bin/env bash
# 07-influxdb.sh — deploy a single-node InfluxDB 2.x onto a kubeadm worker
#                  node (see 02-k8s.sh) as a time-series store for genset
#                  telemetry (see 06-genset.sh), reachable at
#                  <influxdb-vm-ip>:8086.
#
# Usage: ./07-influxdb.sh [vm1 vm2 vm3]
#   VM names default to ubuntu-vm1 ubuntu-vm2 ubuntu-vm3. The first VM is
#   treated as the control-plane; the InfluxDB pod is scheduled on the
#   second VM by default (override with INFLUXDB_VM), alongside Kafka (see
#   03-kafka.sh). It runs with hostNetwork so it is directly reachable by
#   di-agent / genset pods, which also run with hostNetwork.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONFIG_DIR="$(cd "$SCRIPT_DIR/../config" && pwd)"
ENV_FILE="$CONFIG_DIR/.env"
DEPLOYMENT_TMPL="$CONFIG_DIR/influxdb-deployment.yaml"

# ── colours ─────────────────────────────────────────────────────────────────
GREEN=$(tput setaf 2 2>/dev/null || echo "")
YELLOW=$(tput setaf 3 2>/dev/null || echo "")
RED=$(tput setaf 1 2>/dev/null || echo "")
RESET=$(tput sgr0 2>/dev/null || echo "")

info() { echo "${YELLOW}[influxdb] $*${RESET}"; }
ok()   { echo "${GREEN}[influxdb] $*${RESET}"; }
err()  { echo "${RED}[influxdb] $*${RESET}" >&2; }

# ── config (see config/.env; existing exports still take priority) ──────────
if [ -f "$ENV_FILE" ]; then
    set -a
    # shellcheck disable=SC1090
    source "$ENV_FILE"
    set +a
fi

# ── ssh / vm helpers (matches 02-k8s.sh / 03-kafka.sh) ───────────────────────
SSH_USER="${SSH_USER:-ubuntu}"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/id_ed25519_vms}"
SSH_OPTS=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i "$SSH_KEY")

INFLUXDB_IMAGE="${INFLUXDB_IMAGE:-influxdb}"
INFLUXDB_IMAGE_TAG="${INFLUXDB_IMAGE_TAG:-2.7}"
INFLUXDB_PORT="${INFLUXDB_PORT:-8086}"
INFLUXDB_NAMESPACE="${INFLUXDB_NAMESPACE:-default}"
INFLUXDB_ORG="${INFLUXDB_ORG:-di-agent}"
INFLUXDB_BUCKET="${INFLUXDB_BUCKET:-genset-telemetry}"
INFLUXDB_ADMIN_USER="${INFLUXDB_ADMIN_USER:-admin}"
INFLUXDB_ADMIN_PASSWORD="${INFLUXDB_ADMIN_PASSWORD:-admin12345}"
INFLUXDB_ADMIN_TOKEN="${INFLUXDB_ADMIN_TOKEN:-di-agent-genset-admin-token}"

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
# Default to the second VM, alongside Kafka (see 03-kafka.sh); falls back
# to the control-plane if no other VM was given.
INFLUXDB_VM="${INFLUXDB_VM:-${VMS[1]:-$CONTROL_PLANE_VM}}"
INFLUXDB_IP=$(get_vm_ip "$INFLUXDB_VM")
if [ -z "$INFLUXDB_IP" ]; then
    err "Could not resolve IP for $INFLUXDB_VM"
    exit 1
fi

# ── image ─────────────────────────────────────────────────────────────────────
info "Pulling ${INFLUXDB_IMAGE}:${INFLUXDB_IMAGE_TAG} locally ..."
docker pull "${INFLUXDB_IMAGE}:${INFLUXDB_IMAGE_TAG}"
ok "Image ready: ${INFLUXDB_IMAGE}:${INFLUXDB_IMAGE_TAG}"

# ── distribute image (no registry: import straight into containerd) ─────────
info "Importing image into containerd on $INFLUXDB_VM ($INFLUXDB_IP) ..."
docker save "${INFLUXDB_IMAGE}:${INFLUXDB_IMAGE_TAG}" | ssh_vm "$INFLUXDB_IP" "sudo ctr -n k8s.io images import -"
ok "Image imported"

# kubectl runs against the control-plane's API server even though the pod
# itself is scheduled onto $INFLUXDB_VM via nodeSelector below.
API_IP=$(get_vm_ip "$CONTROL_PLANE_VM")

# ── deploy ────────────────────────────────────────────────────────────────────
info "Applying InfluxDB manifests via kubectl on $CONTROL_PLANE_VM (pod scheduled on $INFLUXDB_VM) ..."
INFLUXDB_VM="$INFLUXDB_VM" INFLUXDB_IMAGE="$INFLUXDB_IMAGE" INFLUXDB_IMAGE_TAG="$INFLUXDB_IMAGE_TAG" \
INFLUXDB_PORT="$INFLUXDB_PORT" INFLUXDB_ORG="$INFLUXDB_ORG" INFLUXDB_BUCKET="$INFLUXDB_BUCKET" \
INFLUXDB_ADMIN_USER="$INFLUXDB_ADMIN_USER" INFLUXDB_ADMIN_PASSWORD="$INFLUXDB_ADMIN_PASSWORD" \
INFLUXDB_ADMIN_TOKEN="$INFLUXDB_ADMIN_TOKEN" \
    envsubst '${INFLUXDB_VM} ${INFLUXDB_IMAGE} ${INFLUXDB_IMAGE_TAG} ${INFLUXDB_PORT} ${INFLUXDB_ORG} ${INFLUXDB_BUCKET} ${INFLUXDB_ADMIN_USER} ${INFLUXDB_ADMIN_PASSWORD} ${INFLUXDB_ADMIN_TOKEN}' \
    < "$DEPLOYMENT_TMPL" \
    | ssh_vm "$API_IP" "kubectl --kubeconfig=\$HOME/.kube/config apply -n ${INFLUXDB_NAMESPACE} -f -"

ok "Deployment applied"

info "Waiting for InfluxDB pod to be Ready ..."
ssh_vm "$API_IP" "kubectl --kubeconfig=\$HOME/.kube/config wait -n ${INFLUXDB_NAMESPACE} --for=condition=Ready pod -l app=influxdb --timeout=180s"
ssh_vm "$API_IP" "kubectl --kubeconfig=\$HOME/.kube/config get pods -n ${INFLUXDB_NAMESPACE} -o wide -l app=influxdb"
ok "InfluxDB Ready at ${INFLUXDB_IP}:${INFLUXDB_PORT} (org=${INFLUXDB_ORG}, bucket=${INFLUXDB_BUCKET})"
