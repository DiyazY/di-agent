#!/usr/bin/env bash
# 04-peers.sh — discover VM IPs and register each VM as a peer on the others
#
# Peer topology (trust=0.8 for all pairs):
#   each VM receives every other VM as a peer
#
# Usage: ./04-peers.sh [vm1 vm2 vm3 ...]

set -euo pipefail

# ── colours ─────────────────────────────────────────────────────────────────
GREEN=$(tput setaf 2 2>/dev/null || echo "")
YELLOW=$(tput setaf 3 2>/dev/null || echo "")
RED=$(tput setaf 1 2>/dev/null || echo "")
RESET=$(tput sgr0 2>/dev/null || echo "")

info() { echo "${YELLOW}[peers] $*${RESET}"; }
ok()   { echo "${GREEN}[peers] $*${RESET}"; }
err()  { echo "${RED}[peers] $*${RESET}" >&2; }

# ── helpers ──────────────────────────────────────────────────────────────────
get_vm_ip() {
    virsh domifaddr "$1" | awk '/ipv4/ {print $4}' | cut -d'/' -f1 | head -n1
}

register_peer() {
    local target_vm="$1"   # VM whose agent receives the POST
    local target_ip="$2"   # IP of target_vm
    local peer_id="$3"     # human label (unused by server — kept for log readability)
    local peer_url="$4"    # URL of the peer
    local trust="${5:-0.8}"

    info "  $target_vm ← peer $peer_id ($peer_url, trust=$trust)"
    # POST /peers only takes url+note; server derives ID from sha256(url) and
    # starts trust at 0.5.  We must follow up with POST /peers/{id}/trust.
    local add_resp
    add_resp=$(curl -sf -X POST \
        -H "Content-Type: application/json" \
        -d "{\"url\": \"${peer_url}\"}" \
        "http://${target_ip}:9090/peers" 2>&1) || {
        err "  Failed to POST /peers on $target_vm ($target_ip): $add_resp"
        return 1
    }
    local derived_id
    derived_id=$(echo "$add_resp" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])" 2>/dev/null || echo "")
    if [ -z "$derived_id" ]; then
        err "  Could not extract peer ID from response on $target_vm: $add_resp"
        return 1
    fi
    # Set the initial trust explicitly.
    curl -sf -X POST \
        -H "Content-Type: application/json" \
        -d "{\"value\": ${trust}}" \
        "http://${target_ip}:9090/peers/${derived_id}/trust" >/dev/null 2>&1 || {
        err "  Failed to set trust for $derived_id on $target_vm"
        return 1
    }
    ok "  registered (id=$derived_id, trust=$trust)"
}

# ── args ─────────────────────────────────────────────────────────────────────
if [ "$#" -eq 0 ]; then
    # Default: use all ubuntu-vm* machines known to uc.
    VMS=()
    while IFS= read -r vm; do
        [ -n "$vm" ] && VMS+=("$vm")
    done < <(uc machine ls | awk 'NR > 1 && $1 ~ /^ubuntu-vm/ {print $1}')
else
    VMS=("$@")
fi

if [ "${#VMS[@]}" -eq 0 ]; then
    err "No VMs provided and none discovered from 'uc machine ls'."
    exit 1
fi

# ── discover IPs (bash 3.x compat — arrays only) ────────────────────────────
IPS=()
for vm in "${VMS[@]}"; do
    ip=$(get_vm_ip "$vm")
    if [ -z "$ip" ]; then
        err "Could not find IP for $vm — is it running?"
        exit 1
    fi
    IPS+=("$ip")
    info "$vm → $ip"
done

# ── register peers ────────────────────────────────────────────────────────────
info ""
info "Registering peer relationships ..."

for ((i = 1; i < ${#VMS[@]}; i++)); do
    target_vm="${VMS[$i]}"
    target_ip="${IPS[$i]}"
    for ((j = 1; j < ${#VMS[@]}; j++)); do
        [ "$i" -eq "$j" ] && continue
        peer_vm="${VMS[$j]}"
        peer_ip="${IPS[$j]}"
        register_peer "$target_vm" "$target_ip" "$peer_vm" "http://${peer_ip}:9090" 0.8
    done
done

# ── verify ────────────────────────────────────────────────────────────────────
info ""
info "Verifying peer lists ..."
for ((i = 1; i < ${#VMS[@]}; i++)); do
    vm="${VMS[$i]}"
    ip="${IPS[$i]}"
    peer_count=$(curl -sf "http://${ip}:9090/peers" 2>/dev/null \
        | python3 -c "import sys,json; d=json.load(sys.stdin); print(len(d))" 2>/dev/null \
        || echo "?")
    ok "$vm ($ip): $peer_count peers registered"
done

echo ""
ok "Peer registration complete."
