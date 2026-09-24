#!/usr/bin/env bash
# coordinator.sh — trust-weighted peer routing demonstration
#
# Runs ROUNDS rounds against the di-agent VMs, polling /cost on each,
# calling /recommend on the most-loaded node, and mid-way draining one peer's
# trust to demonstrate trust-weighted rerouting.
#
# Usage: ./coordinator.sh [vm1 vm2 ...]
#   ROUNDS=8   — number of rounds (default 8)
#   INTERVAL=10 — seconds between rounds (default 10)
#
# Requires: Python3 (standard macOS install) or jq for JSON parsing.

set -euo pipefail

# ── colours ─────────────────────────────────────────────────────────────────
GREEN=$(tput setaf 2 2>/dev/null || echo "")
YELLOW=$(tput setaf 3 2>/dev/null || echo "")
RED=$(tput setaf 1 2>/dev/null || echo "")
CYAN=$(tput setaf 6 2>/dev/null || echo "")
BOLD=$(tput bold 2>/dev/null || echo "")
RESET=$(tput sgr0 2>/dev/null || echo "")

info()    { echo "${YELLOW}[coord] $*${RESET}"; }
ok()      { echo "${GREEN}[coord] $*${RESET}"; }
err()     { echo "${RED}[coord] $*${RESET}" >&2; }
header()  { echo "${BOLD}${CYAN}$*${RESET}"; }
announce(){ echo "${BOLD}${GREEN}→ $*${RESET}"; }

# ── helpers ──────────────────────────────────────────────────────────────────
get_vm_ip() {
    virsh domifaddr "$1" | awk '/ipv4/ {print $4}' | cut -d'/' -f1 | head -n1
}

# JSON parsing: prefer python3, fall back to jq
json_get() {
    local json="$1"
    local key="$2"
    if command -v python3 >/dev/null 2>&1; then
        echo "$json" | python3 -c \
            "import sys,json; d=json.load(sys.stdin); print(d.get('${key}',''))" 2>/dev/null \
            || echo ""
    elif command -v jq >/dev/null 2>&1; then
        echo "$json" | jq -r ".${key} // empty" 2>/dev/null || echo ""
    else
        echo ""
    fi
}

# Returns the key with the highest numeric value from an associative array
# Prints: "vm_name value"
max_key() {
    # args: key1 val1 key2 val2 ...
    local max_k="" max_v="-9999"
    while [ "$#" -ge 2 ]; do
        local k="$1" v="$2"; shift 2
        if python3 -c 'import sys; exit(0 if float(sys.argv[1]) > float(sys.argv[2]) else 1)' -- "$v" "$max_v" 2>/dev/null; then
            max_k="$k"; max_v="$v"
        fi
    done
    echo "$max_k $max_v"
}

# ── args ─────────────────────────────────────────────────────────────────────
if [ "$#" -eq 0 ]; then
    VMS=(ubuntu-vm1 ubuntu-vm2 ubuntu-vm3)
else
    VMS=("$@")
fi

ROUNDS="${ROUNDS:-8}"
INTERVAL="${INTERVAL:-10}"
DRAIN_ROUND=$(( ROUNDS / 2 ))   # halfway point: drain the second worker's trust on the first worker

if [ "${#VMS[@]}" -lt 2 ]; then
    err "At least two VMs are required: control plane followed by one or more workers"
    exit 1
fi

# ── discover IPs (bash 3.x compat — no associative arrays) ──────────────────
IPS=()
for idx in "${!VMS[@]}"; do
    vm="${VMS[$idx]}"
    ip="$(get_vm_ip "$vm")"
    if [ -z "$ip" ]; then
        err "Could not find IP for $vm — is it running?"
        exit 1
    fi
    IPS+=("$ip")
done

# Helper: get IP for a VM by position in VMS array
ip_for() {
    local target="$1"
    for idx in "${!VMS[@]}"; do
        if [ "${VMS[$idx]}" = "$target" ]; then
            echo "${IPS[$idx]}"
            return 0
        fi
    done
}

info "VM map:"
for idx in "${!VMS[@]}"; do
    echo "  ${VMS[$idx]}  →  ${IPS[$idx]}"
done
echo ""

# ── main loop ─────────────────────────────────────────────────────────────────
DRAIN_DONE=false

for round in $(seq 1 "$ROUNDS"); do
    header "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    header "  Round $round / $ROUNDS"
    header "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

    # ── 1. poll /cost on all agents ──────────────────────────────────────────
    printf "  %-10s  %-18s  %-14s  %-12s\n" "VM" "IP" "ResourceCost" "Confidence"
    printf "  %-10s  %-18s  %-14s  %-12s\n" "----------" "------------------" "--------------" "------------"

    COST_ARGS=()
    for idx in "${!VMS[@]}"; do
        if [ "$idx" -eq 0 ]; then
            continue
        fi
        vm="${VMS[$idx]}"
        ip="${IPS[$idx]}"
        raw=$(curl -sf "http://${ip}:9090/cost?task=pod-scheduling&node=master" 2>/dev/null || echo "{}")
        rc=$(json_get "$raw" "ResourceCost")
        conf=$(json_get "$raw" "Confidence")
        rc="${rc:-0.000}"
        conf="${conf:-0.000}"
        COST_ARGS+=("$vm" "$rc")
        printf "  %-10s  %-18s  %-14s  %-12s\n" "$vm" "$ip" "$rc" "$conf"
    done
    echo ""

    # ── 2. find highest-cost VM ──────────────────────────────────────────────
    read -r busiest_vm busiest_cost < <(max_key "${COST_ARGS[@]}")
    info "Highest ResourceCost: $busiest_vm (cost=$busiest_cost)"

    # ── 3. call /recommend on the busiest agent ──────────────────────────────
    busiest_ip=$(ip_for "$busiest_vm")
    rec_raw=$(curl -sf -X POST \
        -H "Content-Type: application/json" \
        -d "{\"TaskType\":\"pod-scheduling\",\"SourceNodeID\":\"master\"}" \
        "http://${busiest_ip}:9090/recommend" 2>/dev/null || echo "{}")

    peer_id=$(json_get "$rec_raw" "PeerID")
    savings=$(json_get "$rec_raw" "ExpectedSavings")
    rationale=$(json_get "$rec_raw" "Rationale")
    error_msg=$(json_get "$rec_raw" "error")

    if [ -n "$error_msg" ]; then
        err "  /recommend error on $busiest_vm: $error_msg"
        if echo "$error_msg" | grep -qi "trust"; then
            info "  (ErrInsufficientTrust — all known peers below min-trust threshold)"
        fi
    elif [ -n "$peer_id" ]; then
        announce "$busiest_vm recommends: $peer_id (savings=$savings)"
        if [ -n "$rationale" ]; then
            info "  rationale: $rationale"
        fi
    else
        info "  /recommend returned no peer (possibly no peers registered)"
    fi
    echo ""

    # ── 4. mid-point trust drain ──────────────────────────────────────────────
    if [ "$round" -eq "$DRAIN_ROUND" ] && [ "$DRAIN_DONE" = "false" ]; then
        if [ "${#VMS[@]}" -lt 3 ]; then
            info "Skipping trust drain: need at least 2 worker VMs"
            DRAIN_DONE=true
            continue
        fi

        DRAIN_TARGET_VM="${VMS[2]}"  # second worker VM
        DRAIN_HOST="${VMS[1]}"       # first worker VM
        drain_ip=$(ip_for "$DRAIN_HOST")
        drain_target_ip=$(ip_for "$DRAIN_TARGET_VM")

        header "  *** Trust drain event at round $round ***"
        # Peer IDs are sha256(url)[:12] — look up the real ID on diag-1 by
        # matching the URL that points to diag-2.
        peers_raw=$(curl -sf "http://${drain_ip}:9090/peers" 2>/dev/null || echo "[]")
        drain_peer_id=$(echo "$peers_raw" | python3 -c "
import sys, json
peers = json.load(sys.stdin)
target_url = 'http://${drain_target_ip}:9090'
for p in peers:
    if p.get('url','').rstrip('/') == target_url.rstrip('/'):
        print(p['id'])
        break
" 2>/dev/null || echo "")

        if [ -z "$drain_peer_id" ]; then
            err "  Could not find peer ID for $DRAIN_TARGET_VM on $DRAIN_HOST — skipping drain"
        else
            # Set absolute trust to 0.15 — below the default min-trust floor of 0.5
            # so diag-1 stops recommending diag-2 and routes to diag-3 instead.
            info "Setting trust for $DRAIN_TARGET_VM (id=$drain_peer_id) on $DRAIN_HOST to 0.15 ..."
            drain_applied=false
            if drain_resp=$(curl -sf -X POST \
                    -H "Content-Type: application/json" \
                    -d '{"value": 0.15}' \
                    "http://${drain_ip}:9090/peers/${drain_peer_id}/trust" 2>/dev/null); then
                drain_err=$(json_get "$drain_resp" "error")
                if [ -n "$drain_err" ] && [ "$drain_err" != "null" ] && [ "$drain_err" != "" ]; then
                    err "  Trust drain failed: $drain_err"
                else
                    drain_applied=true
                    ok "  Trust drain applied: $DRAIN_TARGET_VM (id=$drain_peer_id) trust=0.15 on $DRAIN_HOST"
                fi
            else
                err "  Trust drain request failed for $DRAIN_TARGET_VM on $DRAIN_HOST"
            fi
            if [ "$drain_applied" = true ]; then
                if [ "${#VMS[@]}" -ge 3 ]; then
                    announce "Trust drain: $DRAIN_TARGET_VM trust=0.15 (< min-trust 0.5) → expect ${VMS[2]} to win next rounds"
                else
                    announce "Trust drain: $DRAIN_TARGET_VM trust=0.15 (< min-trust 0.5) → expect other eligible peers to win next rounds"
                fi
            fi
        fi
        echo ""
        DRAIN_DONE=true
    fi

    # ── 5. wait before next round ─────────────────────────────────────────────
    if [ "$round" -lt "$ROUNDS" ]; then
        info "Waiting ${INTERVAL}s before round $((round + 1)) ..."
        sleep "$INTERVAL"
    fi
done

echo ""
header "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
ok "Coordinator demo complete ($ROUNDS rounds)."
header "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
