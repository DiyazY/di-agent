#!/usr/bin/env bash
# 09-playground.sh — build the playground (React) frontend image and deploy
#                    it to the kubeadm cluster (see 02-k8s.sh) as a
#                    NodePort-exposed Deployment. Also ensures the genset,
#                    switchboard and propulsion Services exist, since the
#                    frontend proxies to them by in-cluster DNS name (see
#                    nginx.conf.template).
#
# Usage: ./09-playground.sh [vm1 vm2 vm3]
#   VM names default to ubuntu-vm1 ubuntu-vm2 ubuntu-vm3. The playground pod
#   is scheduled on the second VM by default, not the control plane
#   (override with PLAYGROUND_VM).

set -euo pipefail

# ── colours ─────────────────────────────────────────────────────────────────
GREEN=$(tput setaf 2 2>/dev/null || echo "")
YELLOW=$(tput setaf 3 2>/dev/null || echo "")
RED=$(tput setaf 1 2>/dev/null || echo "")
RESET=$(tput sgr0 2>/dev/null || echo "")

info() { echo "${YELLOW}[playground] $*${RESET}"; }
ok()   { echo "${GREEN}[playground] $*${RESET}"; }
err()  { echo "${RED}[playground] $*${RESET}" >&2; }

# ── paths ────────────────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
POC_DIR="$(dirname "$SCRIPT_DIR")"
PLAYGROUND_DIR="$POC_DIR/playground"
CONFIG_DIR="$POC_DIR/config"
ENV_FILE="$CONFIG_DIR/.env"
DEPLOYMENT_TMPL="$CONFIG_DIR/playground-deployment.yaml"
GENSET_SERVICE="$CONFIG_DIR/genset-service.yaml"
PROPULSION_SERVICE="$CONFIG_DIR/propulsion-service.yaml"
SWITCHBOARD_SERVICE="$CONFIG_DIR/switchboard-service.yaml"

# ── config (see config/.env; existing exports still take priority) ──────────
if [ -f "$ENV_FILE" ]; then
    set -a
    # shellcheck disable=SC1090
    source "$ENV_FILE"
    set +a
fi

IMAGE_NAME="${PLAYGROUND_IMAGE_NAME:-playground}"
IMAGE_TAG="${PLAYGROUND_IMAGE_TAG:-latest}"
PLAYGROUND_NODE_PORT="${PLAYGROUND_NODE_PORT:-30080}"
GENSET_UPSTREAM="${GENSET_UPSTREAM:-genset.default.svc.cluster.local:8000}"
PROPULSION_UPSTREAM="${PROPULSION_UPSTREAM:-propulsion.default.svc.cluster.local:8000}"
SWITCHBOARD_UPSTREAM="${SWITCHBOARD_UPSTREAM:-switchboard.default.svc.cluster.local:8000}"

# ── ssh / vm helpers (matches 02-k8s.sh / 06-genset.sh / 06b-propulsion.sh) ──
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
# Default to the second VM (a worker, not the control plane); falls back to
# the last VM. Matches the KAFKA_VM default in 06-genset.sh.
PLAYGROUND_VM="${PLAYGROUND_VM:-${VMS[1]:-${VMS[-1]}}}"

# ── image ─────────────────────────────────────────────────────────────────────
info "Building docker image ${IMAGE_NAME}:${IMAGE_TAG} from $PLAYGROUND_DIR ..."
(
  cd "$PLAYGROUND_DIR"
    docker build -t "${IMAGE_NAME}:${IMAGE_TAG}" .
) || {
    err "Docker build failed"
    exit 1
}
ok "Image ready: ${IMAGE_NAME}:${IMAGE_TAG}"

# ── distribute image (no registry: import straight into containerd) ─────────
PLAYGROUND_IP=$(get_vm_ip "$PLAYGROUND_VM")
if [ -z "$PLAYGROUND_IP" ]; then
  err "Could not resolve IP for $PLAYGROUND_VM"
    exit 1
fi
info "Importing image into containerd on $PLAYGROUND_VM ($PLAYGROUND_IP) ..."
docker save "${IMAGE_NAME}:${IMAGE_TAG}" | ssh_vm "$PLAYGROUND_IP" "sudo ctr -n k8s.io images import -"
ok "Image imported"

# ── deploy ────────────────────────────────────────────────────────────────────
CP_IP=$(get_vm_ip "$CONTROL_PLANE_VM")

info "Ensuring genset/switchboard/propulsion Services exist (playground proxies to them by DNS name) ..."
ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config apply -f -" < "$GENSET_SERVICE"
ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config apply -f -" < "$SWITCHBOARD_SERVICE"
ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config apply -f -" < "$PROPULSION_SERVICE"

info "Applying playground manifest via kubectl on $CONTROL_PLANE_VM (pod scheduled on $PLAYGROUND_VM) ..."
PLAYGROUND_VM="$PLAYGROUND_VM" IMAGE_NAME="$IMAGE_NAME" IMAGE_TAG="$IMAGE_TAG" \
GENSET_UPSTREAM="$GENSET_UPSTREAM" PROPULSION_UPSTREAM="$PROPULSION_UPSTREAM" \
SWITCHBOARD_UPSTREAM="$SWITCHBOARD_UPSTREAM" \
PLAYGROUND_NODE_PORT="$PLAYGROUND_NODE_PORT" \
    envsubst '${PLAYGROUND_VM} ${IMAGE_NAME} ${IMAGE_TAG} ${GENSET_UPSTREAM} ${PROPULSION_UPSTREAM} ${SWITCHBOARD_UPSTREAM} ${PLAYGROUND_NODE_PORT}' \
    < "$DEPLOYMENT_TMPL" \
    | ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config apply -f -"

ok "Deployment applied"

info "Waiting for playground pod to be Ready ..."
ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config wait --for=condition=Ready pod -l app=playground --timeout=180s"
ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config get pods -o wide -l app=playground"
ok "Playground available at http://${PLAYGROUND_IP}:${PLAYGROUND_NODE_PORT}"
