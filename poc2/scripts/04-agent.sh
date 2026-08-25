#!/usr/bin/env bash
# 03-agent.sh — build the di-agent image and deploy it to the kubeadm
#               cluster (see 02-k8s.sh) as one pod per VM via kubectl.
#
# Usage: ./03-agent.sh [vm1 vm2 vm3]
#   VM names default to ubuntu-vm1 ubuntu-vm2 ubuntu-vm3.
#   The first VM is the control-plane and gets REGIME=bursty; all others
#   get REGIME=stable.

set -euo pipefail

# ── colours ─────────────────────────────────────────────────────────────────
GREEN=$(tput setaf 2 2>/dev/null || echo "")
YELLOW=$(tput setaf 3 2>/dev/null || echo "")
RED=$(tput setaf 1 2>/dev/null || echo "")
RESET=$(tput sgr0 2>/dev/null || echo "")

info() { echo "${YELLOW}[agent] $*${RESET}"; }
ok()   { echo "${GREEN}[agent] $*${RESET}"; }
err()  { echo "${RED}[agent] $*${RESET}" >&2; }

# ── paths ────────────────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
POC_DIR="$(dirname "$SCRIPT_DIR")"
SEM_DIR="$(dirname "$POC_DIR")/semantic-map"
GO_SRC="$(dirname "$POC_DIR")/semantic-map/go"
SERVICE_SRC="$POC_DIR/config/di-agent.service"
BINARY_OUT="./cmd/agent/tmp/di-agent-poc"
BUILD_GOOS="${BUILD_GOOS:-linux}"
BUILD_GOARCH="${BUILD_GOARCH:-${GOARCH:-$(go env GOARCH)}}"
IMAGE_NAME="${IMAGE_NAME:-di-agent}"
IMAGE_TAG="${IMAGE_TAG:-latest}"

# ── ssh / vm helpers (matches 02-k8s.sh) ─────────────────────────────────────
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
WORKER_VMS=("${VMS[@]:1}")

if [ "${#WORKER_VMS[@]}" -eq 0 ]; then
    err "At least one worker VM is required to deploy di-agent"
    exit 1
fi

# Kafka broker address: the broker is deployed by the Helm chart
# (helm/di-agent-system) as an in-cluster ClusterIP Service, not pinned to
# any particular VM, so it's reachable at its cluster DNS name.
KAFKA_NAMESPACE="${KAFKA_NAMESPACE:-default}"
KAFKA_BROKERS="${KAFKA_BROKERS:-kafka.${KAFKA_NAMESPACE}.svc.cluster.local:9092}"

# ── build ─────────────────────────────────────────────────────────────────────
info "Building di-agent for ${BUILD_GOOS}/${BUILD_GOARCH} from $GO_SRC ..."
if [ ! -d "$GO_SRC" ]; then
    err "Go source directory not found: $GO_SRC"
    exit 1
fi

(
    cd "$GO_SRC"
    GOOS="$BUILD_GOOS" GOARCH="$BUILD_GOARCH" go build -o "$BINARY_OUT" ./cmd/agent/
)
ok "Binary built: $BINARY_OUT ($(du -sh "$BINARY_OUT" | cut -f1))"

# ── image ─────────────────────────────────────────────────────────────────────
info "Building docker image ${IMAGE_NAME}:${IMAGE_TAG} ..."
if docker images "${IMAGE_NAME}:${IMAGE_TAG}" --format "{{.Repository}}" | grep -q .; then
    info "Image already exists, skipping build"
else
    (
        cd "$SEM_DIR"
        docker build -t "${IMAGE_NAME}:${IMAGE_TAG}" .
    ) || {
        err "Docker build failed"
        exit 1
    }
fi
ok "Image ready: ${IMAGE_NAME}:${IMAGE_TAG}"

# ── distribute image (no registry: import straight into each node's containerd) ─
info "Distributing image to worker VMs: ${WORKER_VMS[*]} ..."
pids=()
for vm in "${WORKER_VMS[@]}"; do
    ip=$(get_vm_ip "$vm")
    if [ -z "$ip" ]; then
        err "Could not resolve IP for $vm"
        exit 1
    fi
    info "Importing image into containerd on $vm ($ip) ..."
    docker save "${IMAGE_NAME}:${IMAGE_TAG}" | ssh_vm "$ip" "sudo ctr -n k8s.io images import -" &
    pids+=($!)
done

# Wait for all imports to complete
for pid in "${pids[@]}"; do
    wait "$pid" || {
        err "Image import failed for one or more VMs"
        exit 1
    }
done
ok "All images imported"

# ── deploy ────────────────────────────────────────────────────────────────────
CP_IP=$(get_vm_ip "$CONTROL_PLANE_VM")
info "Applying di-agent manifests via kubectl on $CONTROL_PLANE_VM ..."

{
    for vm in "${WORKER_VMS[@]}"; do
        regime="stable"
        cat <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: di-agent-${vm}
  labels:
    app: di-agent
    node: ${vm}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: di-agent
      node: ${vm}
  template:
    metadata:
      labels:
        app: di-agent
        node: ${vm}
    spec:
      nodeSelector:
        kubernetes.io/hostname: ${vm}
      hostNetwork: true
      # hostNetwork pods don't use cluster DNS by default; needed to resolve
      # the kafka.*.svc.cluster.local Service name above.
      dnsPolicy: ClusterFirstWithHostNet
      containers:
        - name: di-agent
          image: ${IMAGE_NAME}:${IMAGE_TAG}
          imagePullPolicy: Never
          env:
            - name: NODE_ID
              value: "${vm}"
            - name: REGIME
              value: "${regime}"
            - name: KAFKA_BROKERS
              value: "${KAFKA_BROKERS}"
          ports:
            - containerPort: 9090
---
EOF
    done
} | ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config apply -f -"

ok "Deployment complete"

info "Waiting for di-agent pods to be Ready ..."
ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config wait --for=condition=Ready pod -l app=di-agent --timeout=180s"
ssh_vm "$CP_IP" "kubectl --kubeconfig=\$HOME/.kube/config get pods -o wide -l app=di-agent"
ok "All di-agent pods Ready"


