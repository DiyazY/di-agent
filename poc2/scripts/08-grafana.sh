#!/usr/bin/env bash
# 08-grafana.sh — deploy Grafana onto a kubeadm worker node (see 02-k8s.sh)
#                 with an InfluxDB datasource (see 07-influxdb.sh)
#                 auto-provisioned, reachable at <grafana-vm-ip>:3000.
#
# Usage: ./08-grafana.sh [vm1 vm2 vm3]
#   VM names default to ubuntu-vm1 ubuntu-vm2 ubuntu-vm3. The first VM is
#   treated as the control-plane; the Grafana pod is scheduled on the
#   second VM by default (override with GRAFANA_VM), alongside InfluxDB
#   (see 07-influxdb.sh). It runs with hostNetwork so it is directly
#   reachable, and reaches InfluxDB the same way.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONFIG_DIR="$(cd "$SCRIPT_DIR/../config" && pwd)"
ENV_FILE="$CONFIG_DIR/.env"
DEPLOYMENT_TMPL="$CONFIG_DIR/grafana-deployment.yaml"

# ── colours ─────────────────────────────────────────────────────────────────
GREEN=$(tput setaf 2 2>/dev/null || echo "")
YELLOW=$(tput setaf 3 2>/dev/null || echo "")
RED=$(tput setaf 1 2>/dev/null || echo "")
RESET=$(tput sgr0 2>/dev/null || echo "")

info() { echo "${YELLOW}[grafana] $*${RESET}"; }
ok()   { echo "${GREEN}[grafana] $*${RESET}"; }
err()  { echo "${RED}[grafana] $*${RESET}" >&2; }

# ── config (see config/.env; existing exports still take priority) ──────────
if [ -f "$ENV_FILE" ]; then
    set -a
    # shellcheck disable=SC1090
    source "$ENV_FILE"
    set +a
fi

# ── ssh / vm helpers (matches 02-k8s.sh / 07-influxdb.sh) ────────────────────
SSH_USER="${SSH_USER:-ubuntu}"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/id_ed25519_vms}"
SSH_OPTS=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i "$SSH_KEY")

GRAFANA_IMAGE="${GRAFANA_IMAGE:-grafana/grafana}"
GRAFANA_IMAGE_TAG="${GRAFANA_IMAGE_TAG:-latest}"
GRAFANA_PORT="${GRAFANA_PORT:-3000}"
GRAFANA_NAMESPACE="${GRAFANA_NAMESPACE:-default}"
GRAFANA_ADMIN_USER="${GRAFANA_ADMIN_USER:-admin}"
GRAFANA_ADMIN_PASSWORD="${GRAFANA_ADMIN_PASSWORD:-admin12345}"

# InfluxDB connection details (see config/.env); must match the
# values used there, since Grafana's datasource is provisioned against them.
INFLUXDB_PORT="${INFLUXDB_PORT:-8086}"
INFLUXDB_ORG="${INFLUXDB_ORG:-di-agent}"
INFLUXDB_BUCKET="${INFLUXDB_BUCKET:-genset-telemetry}"
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
# Default to the second VM, alongside InfluxDB (see 07-influxdb.sh); falls
# back to the control-plane if no other VM was given.
GRAFANA_VM="${GRAFANA_VM:-${VMS[1]:-$CONTROL_PLANE_VM}}"
GRAFANA_IP=$(get_vm_ip "$GRAFANA_VM")
if [ -z "$GRAFANA_IP" ]; then
    err "Could not resolve IP for $GRAFANA_VM"
    exit 1
fi

# InfluxDB is expected to be reachable at this VM's IP (see 07-influxdb.sh
# INFLUXDB_VM default, which is also the second VM).
INFLUXDB_VM="${INFLUXDB_VM:-${VMS[1]:-$CONTROL_PLANE_VM}}"
INFLUXDB_IP=$(get_vm_ip "$INFLUXDB_VM")
if [ -z "$INFLUXDB_IP" ]; then
    err "Could not resolve IP for $INFLUXDB_VM"
    exit 1
fi
INFLUXDB_URL="${INFLUXDB_URL:-http://${INFLUXDB_IP}:${INFLUXDB_PORT}}"

# ── image ─────────────────────────────────────────────────────────────────────
info "Pulling ${GRAFANA_IMAGE}:${GRAFANA_IMAGE_TAG} locally ..."
docker pull "${GRAFANA_IMAGE}:${GRAFANA_IMAGE_TAG}"
ok "Image ready: ${GRAFANA_IMAGE}:${GRAFANA_IMAGE_TAG}"

# ── distribute image (no registry: import straight into containerd) ─────────
info "Importing image into containerd on $GRAFANA_VM ($GRAFANA_IP) ..."
docker save "${GRAFANA_IMAGE}:${GRAFANA_IMAGE_TAG}" | ssh_vm "$GRAFANA_IP" "sudo ctr -n k8s.io images import -"
ok "Image imported"

# kubectl runs against the control-plane's API server even though the pod
# itself is scheduled onto $GRAFANA_VM via nodeSelector below.
API_IP=$(get_vm_ip "$CONTROL_PLANE_VM")

# ── deploy ────────────────────────────────────────────────────────────────────
info "Applying Grafana manifests via kubectl on $CONTROL_PLANE_VM (pod scheduled on $GRAFANA_VM) ..."
GRAFANA_VM="$GRAFANA_VM" GRAFANA_IMAGE="$GRAFANA_IMAGE" GRAFANA_IMAGE_TAG="$GRAFANA_IMAGE_TAG" \
GRAFANA_PORT="$GRAFANA_PORT" GRAFANA_ADMIN_USER="$GRAFANA_ADMIN_USER" GRAFANA_ADMIN_PASSWORD="$GRAFANA_ADMIN_PASSWORD" \
INFLUXDB_URL="$INFLUXDB_URL" INFLUXDB_ORG="$INFLUXDB_ORG" INFLUXDB_BUCKET="$INFLUXDB_BUCKET" \
INFLUXDB_ADMIN_TOKEN="$INFLUXDB_ADMIN_TOKEN" \
    envsubst '${GRAFANA_VM} ${GRAFANA_IMAGE} ${GRAFANA_IMAGE_TAG} ${GRAFANA_PORT} ${GRAFANA_ADMIN_USER} ${GRAFANA_ADMIN_PASSWORD} ${INFLUXDB_URL} ${INFLUXDB_ORG} ${INFLUXDB_BUCKET} ${INFLUXDB_ADMIN_TOKEN}' \
    < "$DEPLOYMENT_TMPL" \
    | ssh_vm "$API_IP" "kubectl --kubeconfig=\$HOME/.kube/config apply -n ${GRAFANA_NAMESPACE} -f -"

ok "Deployment applied"

info "Waiting for Grafana pod to be Ready ..."
ssh_vm "$API_IP" "kubectl --kubeconfig=\$HOME/.kube/config wait -n ${GRAFANA_NAMESPACE} --for=condition=Ready pod -l app=grafana --timeout=180s"
ssh_vm "$API_IP" "kubectl --kubeconfig=\$HOME/.kube/config get pods -n ${GRAFANA_NAMESPACE} -o wide -l app=grafana"
ok "Grafana Ready at http://${GRAFANA_IP}:${GRAFANA_PORT} (datasource -> ${INFLUXDB_URL})"
