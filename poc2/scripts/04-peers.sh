#!/usr/bin/env bash
# 05-peers.sh — discover VM IPs and register each VM as a peer on the others
#
# Peer topology (trust=0.8 for all pairs):
#   diag-1 ← diag-2, diag-3
#   diag-2 ← diag-1, diag-3
#   diag-3 ← diag-1, diag-2
#
# Usage: ./05-peers.sh [vm1 vm2 vm3]

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
vm_internal_ip() { 
    uc machine ls | awk -v vm="$1" '$1 == vm {split($5, endpoints, ","); split(endpoints[1], addr, ":"); print addr[1]}'
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
    VMS=(ubuntu-vm1 ubuntu-vm2 ubuntu-vm3)
else
    VMS=("$@")
fi

# ── discover IPs (no associative arrays — bash 3.x compat) ──────────────────
IP1=$(vm_internal_ip "${VMS[0]}")
IP2=$(vm_internal_ip "${VMS[1]}")
IP3=$(vm_internal_ip "${VMS[2]}")

for i in 1 2 3; do
    vm="${VMS[$((i-1))]}"
    eval "ip=\$IP$i"
    if [ -z "$ip" ]; then
        err "Could not find IP for $vm — is it running?"
        exit 1
    fi
    info "$vm → $ip"
done

# ── register peers ────────────────────────────────────────────────────────────
info ""
info "Registering peer relationships ..."

register_peer "${VMS[0]}" "$IP1" "${VMS[1]}" "http://${IP2}:9090" 0.8
register_peer "${VMS[0]}" "$IP1" "${VMS[2]}" "http://${IP3}:9090" 0.8

register_peer "${VMS[1]}" "$IP2" "${VMS[0]}" "http://${IP1}:9090" 0.8
register_peer "${VMS[1]}" "$IP2" "${VMS[2]}" "http://${IP3}:9090" 0.8

register_peer "${VMS[2]}" "$IP3" "${VMS[0]}" "http://${IP1}:9090" 0.8
register_peer "${VMS[2]}" "$IP3" "${VMS[1]}" "http://${IP2}:9090" 0.8

# ── verify ────────────────────────────────────────────────────────────────────
info ""
info "Verifying peer lists ..."
for ip in "$IP1" "$IP2" "$IP3"; do
    vm="${VMS[$(( $(echo "$IP1 $IP2 $IP3" | tr ' ' '\n' | grep -n "^${ip}$" | cut -d: -f1) - 1 ))]}"
    peer_count=$(curl -sf "http://${ip}:9090/peers" 2>/dev/null \
        | python3 -c "import sys,json; d=json.load(sys.stdin); print(len(d))" 2>/dev/null \
        || echo "?")
    ok "$ip: $peer_count peers registered"
done

echo ""
ok "Peer registration complete."
