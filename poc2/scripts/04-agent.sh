#!/usr/bin/env bash
# 04-agent.sh — build the di-agent binary (linux/arm64), transfer to each VM,
#               and install it as a systemd service.
#
# Usage: ./04-agent.sh [vm1 vm2 vm3]
#   VM names default to diag-1 diag-2 diag-3.
#   diag-1 gets REGIME=bursty; all others get REGIME=stable.

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
GO_SRC="$(dirname "$POC_DIR")/semantic-map/go"
SERVICE_SRC="$POC_DIR/config/di-agent.service"
BINARY_OUT="./cmd/agent/tmp/di-agent-poc"
DOCKER_COMPOSE_SRC="$(dirname "$POC_DIR")/semantic-map/docker-compose.yml"

# ── args ─────────────────────────────────────────────────────────────────────
if [ "$#" -eq 0 ]; then
    VMS=(ubuntu-vm1 ubuntu-vm2 ubuntu-vm3)
else
    VMS=("$@")
fi

# ── build ─────────────────────────────────────────────────────────────────────
info "Building di-agent for linux/amd64 from $GO_SRC ..."
if [ ! -d "$GO_SRC" ]; then
    err "Go source directory not found: $GO_SRC"
    exit 1
fi

(
    cd "$GO_SRC"
    GOOS=linux GOARCH=amd64 go build -o "$BINARY_OUT" ./cmd/agent/
)
ok "Binary built: $BINARY_OUT ($(du -sh "$BINARY_OUT" | cut -f1))"

# ── deploy ────────────────────────────────────────────────────────────────────
info "Deploying with uc..."
uc deploy -f "$DOCKER_COMPOSE_SRC" -y
ok "Deployment complete"

