#!/bin/bash

# Simple Uncloud deployment script
# Usage: ./deploy.sh

set -euo pipefail

# ── colours ─────────────────────────────────────────────────────────────────
GREEN=$(tput setaf 2 2>/dev/null || echo "")
YELLOW=$(tput setaf 3 2>/dev/null || echo "")
RED=$(tput setaf 1 2>/dev/null || echo "")
RESET=$(tput sgr0 2>/dev/null || echo "")

info() { echo "${YELLOW}[agent] $*${RESET}"; }
ok()   { echo "${GREEN}[agent] $*${RESET}"; }
err()  { echo "${RED}[agent] $*${RESET}" >&2; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
POC_DIR="$(dirname "$SCRIPT_DIR")"
DOCKER_COMPOSE_SRC="$(dirname "$POC_DIR")/semantic-map/docker-compose.yml"

# ── deploy ────────────────────────────────────────────────────────────────────
info "Deploying with uc..."
uc deploy -f "$DOCKER_COMPOSE_SRC" -y
ok "Deployment complete"
