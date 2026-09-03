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
BINARY_OUT="/tmp/di-agent"

# ── args ─────────────────────────────────────────────────────────────────────
if [ "$#" -eq 0 ]; then
    VMS=(diag-1 diag-2 diag-3)
else
    VMS=("$@")
fi

# ── build ─────────────────────────────────────────────────────────────────────
info "Building di-agent for linux/arm64 from $GO_SRC ..."
if [ ! -d "$GO_SRC" ]; then
    err "Go source directory not found: $GO_SRC"
    exit 1
fi

(
    cd "$GO_SRC"
    GOOS=linux GOARCH=arm64 go build -o "$BINARY_OUT" ./cmd/agent
)
ok "Binary built: $BINARY_OUT ($(du -sh "$BINARY_OUT" | cut -f1))"

# ── deploy to each VM ────────────────────────────────────────────────────────
for vm in "${VMS[@]}"; do
    # Regime: bursty for first VM, stable for the rest
    if [ "$vm" = "${VMS[0]}" ]; then
        REGIME="bursty"
        NODE_ID="${VMS[0]}"
    else
        REGIME="stable"
        NODE_ID="$vm"
    fi

    info "$vm (node-id=$NODE_ID, regime=$REGIME): transferring binary ..."
    multipass transfer "$BINARY_OUT" "$vm:/tmp/di-agent"

    info "$vm: installing binary to /usr/local/bin/di-agent ..."
    multipass exec "$vm" -- bash -c "
        sudo mv /tmp/di-agent /usr/local/bin/di-agent
        sudo chmod +x /usr/local/bin/di-agent
    "

    info "$vm: writing /etc/di-agent/env ..."
    multipass exec "$vm" -- bash -c "
        sudo mkdir -p /etc/di-agent
        sudo tee /etc/di-agent/env > /dev/null <<'ENVEOF'
NODE_ID=${NODE_ID}
REGIME=${REGIME}
ENVEOF
    "

    info "$vm: installing systemd service ..."
    multipass transfer "$SERVICE_SRC" "$vm:/tmp/di-agent.service"
    multipass exec "$vm" -- bash -c "
        sudo mv /tmp/di-agent.service /etc/systemd/system/di-agent.service
        sudo systemctl daemon-reload
        sudo systemctl enable di-agent
        sudo systemctl restart di-agent
    "

    # Verify
    info "$vm: waiting for agent to come up ..."
    local_attempts=0
    while [ "$local_attempts" -lt 12 ]; do
        if multipass exec "$vm" -- bash -c \
            "curl -sf http://localhost:9090/healthz > /dev/null 2>&1"; then
            ok "$vm: di-agent healthy at localhost:9090"
            break
        fi
        sleep 5
        local_attempts=$((local_attempts + 1))
        if [ "$local_attempts" -eq 12 ]; then
            err "$vm: agent did not respond within 60s"
            multipass exec "$vm" -- bash -c "sudo journalctl -u di-agent -n 30 --no-pager" || true
            exit 1
        fi
    done
done

echo ""
ok "di-agent deployed and running on all VMs."
