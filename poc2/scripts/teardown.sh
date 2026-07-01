#!/usr/bin/env bash
# teardown.sh — stop, delete, and purge the PoC Multipass VMs
#
# Usage: ./teardown.sh [vm1 vm2 vm3]
#   Defaults to diag-1 diag-2 diag-3

set -euo pipefail

# ── colours ─────────────────────────────────────────────────────────────────
GREEN=$(tput setaf 2 2>/dev/null || echo "")
YELLOW=$(tput setaf 3 2>/dev/null || echo "")
RED=$(tput setaf 1 2>/dev/null || echo "")
RESET=$(tput sgr0 2>/dev/null || echo "")

info() { echo "${YELLOW}[teardown] $*${RESET}"; }
ok()   { echo "${GREEN}[teardown] $*${RESET}"; }
err()  { echo "${RED}[teardown] $*${RESET}" >&2; }

# ── args ─────────────────────────────────────────────────────────────────────
if [ "$#" -eq 0 ]; then
    VMS=(diag-1 diag-2 diag-3)
else
    VMS=("$@")
fi

# ── stop ─────────────────────────────────────────────────────────────────────
info "Stopping VMs: ${VMS[*]} ..."
multipass stop "${VMS[@]}" 2>/dev/null || true
ok "VMs stopped"

# ── delete ───────────────────────────────────────────────────────────────────
info "Deleting VMs: ${VMS[*]} ..."
multipass delete "${VMS[@]}" 2>/dev/null || true
ok "VMs deleted"

# ── purge ────────────────────────────────────────────────────────────────────
info "Purging deleted VMs and reclaiming disk ..."
multipass purge
ok "Purge complete"

echo ""
ok "Teardown done — all PoC VMs removed."
