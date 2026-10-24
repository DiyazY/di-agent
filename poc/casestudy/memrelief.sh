#!/usr/bin/env bash
# memrelief.sh — the memory lever, end to end on one node. Runs on the node, as root, from
# this directory, next to an `agent` (or `agent.new`) built from this branch.
#
# Usage: sudo ./memrelief.sh <out-dir> [phase-1-minutes] [phase-2-minutes]
#
# Phase 1 (default 45 min): a fresh map on agent-relief.yaml with the ledger in <out-dir>;
# mem-victim (a page-cache working set within its request) and then page-hog (6 GiB of
# anonymous memory held, then page cache growing 256 MiB a minute past its request, no limit,
# until its set no longer fits). Expected: memory.high on page-hog,
# kept, memory_contention_ratio lower after settle than at act, mem-victim never acted on.
# Ollama holds 6-9 GB; with page-hog's 6 GiB held, its file set fills the room left in under
# ten minutes. run-2026-10-23-memrelief/results.md says what the runs showed.
# Phase 2 (default 10 min; 0 skips it): mem-squeeze alone, which limits itself at its own 64Mi limit.
# Expected: no memory action, memory_contention_ratio < 0.20 while its memory_events_high or
# memory_events_max rise. (mem-wave holds 400 MB under a 600Mi limit and never reaches it.)
#
# Every 30 s poll.jsonl gets one line per endpoint ({t, endpoint, status, body}) and
# node.jsonl one line of the node's own view (MemAvailable, PSI, each pod's memory.current,
# memory.high and memory.events). Safety: an OOM kill of anything but page-hog, or k0s,
# Ollama or the agent going unhealthy, deletes page-hog and ends the run (exit 2).
set -uo pipefail
cd "$(dirname "$0")"
HERE="$PWD"
OUT="$(realpath -m "${1:?usage: $0 <out-dir> [phase-1-min] [phase-2-min]}")"; mkdir -p "$OUT"
P1="${2:-45}"; P2="${3:-10}"
K=(k0s kubectl); API=http://localhost:9090; T0=$(date +%s)
mark() { jq -cn --argjson t "$(date +%s.%N)" --arg e "$1" '{t:$t, event:$e}' >>"$OUT/phases.jsonl"; echo "[$(date +%T)] $1"; }

# The service this run replaces, as it was started.
systemctl show di-agent.service -p ExecStart -p ActiveEnterTimestamp >"$OUT/replaced-service.txt" 2>&1

# The memory.high actuator refuses to start when anything under kubepods has a memory.high
# other than max (MemoryQoS): say so here, before anything is stopped.
managed=$(find /sys/fs/cgroup/kubepods -name memory.high -exec sh -c 'v=$(cat "$1"); [ "$v" != max ] && echo "$1 $v"' _ {} \;)
if [ -n "$managed" ]; then echo "memory.high is set under kubepods:"; echo "$managed"; exit 1; fi
mark "precondition: every memory.high under kubepods is max"

pkill -TERM -x agent; for _ in $(seq 60); do pgrep -x agent >/dev/null || break; sleep 1; done
"${K[@]}" delete pod mem-victim page-hog mem-squeeze mem-wave hog victim --ignore-not-found --wait=true >/dev/null
[ -e /var/lib/di-agent/state.json ] && mv /var/lib/di-agent/state.json "/var/lib/di-agent/state.json.pre-memrelief-$(date +%H%M%S)"
[ -e agent.new ] && mv agent.new agent
# agent-relief.yaml with the ledger in this run's directory.
{ cat agent-relief.yaml; echo "actuation: { ledger: $OUT/actions.json }"; } >"$OUT/agent.yaml"
systemctl reset-failed di-agent.service 2>/dev/null || true
systemd-run --unit=di-agent --collect --quiet -p AllowedCPUs=0-5 -p WorkingDirectory="$HERE" \
    -p StandardOutput="append:$OUT/agent.log" -p StandardError="append:$OUT/agent.log" "$HERE/agent" -config "$OUT/agent.yaml"
for _ in $(seq 60); do curl -sf $API/healthz >/dev/null && break; sleep 1; done
curl -sf $API/healthz >/dev/null || { mark "agent did not come up"; exit 1; }
mark "agent started on $OUT/agent.yaml ($(curl -s $API/version | jq -r .build_commit))"

uid() { "${K[@]}" get pod "$1" -o jsonpath='{.metadata.uid}' 2>/dev/null; }
poll() {
    local e="$1" t body status
    t=$(date +%s.%N)
    body=$(curl -s --max-time 10 -w '\n%{http_code}' "$API$e") || body=$'\n0'
    status="${body##*$'\n'}"; body="${body%$'\n'*}"
    jq -ec --argjson t "$t" --arg e "$e" --argjson s "$((10#${status:-0}))" '{t:$t, endpoint:$e, status:$s, body:.}' \
        <<<"$body" >>"$OUT/poll.jsonl" 2>/dev/null ||
        jq -cn --argjson t "$t" --arg e "$e" --argjson s "$((10#${status:-0}))" '{t:$t, endpoint:$e, status:$s, body:null}' >>"$OUT/poll.jsonl"
}
podcg() { ls -d /sys/fs/cgroup/kubepods/*/pod"$1" /sys/fs/cgroup/kubepods/pod"$1" 2>/dev/null | head -1; }
node_line() {
    local pods='{}' name u d
    for name in mem-victim page-hog mem-squeeze; do
        u=$(uid "$name"); [ -z "$u" ] && continue
        d=$(podcg "$u"); [ -z "$d" ] && continue
        pods=$(jq -c --arg n "$name" --arg u "$u" --arg cur "$(cat "$d/memory.current")" --arg high "$(cat "$d/memory.high")" \
            --arg ev "$(tr '\n' ' ' <"$d/memory.events")" --arg psi "$(head -1 "$d/memory.pressure")" \
            --arg file "$(awk '$1=="file"{print $2}' "$d/memory.stat")" \
            '.[$n] = {uid:$u, current:($cur|tonumber), file:($file|tonumber), high:$high, events:$ev, psi:$psi}' <<<"$pods")
    done
    jq -cn --argjson t "$(date +%s.%N)" --arg avail "$(awk '/^MemAvailable:/{print $2*1024}' /proc/meminfo)" \
        --arg psi "$(head -1 /proc/pressure/memory)" --argjson pods "$pods" \
        '{t:$t, mem_available:($avail|tonumber), psi:$psi, pods:$pods}' >>"$OUT/node.jsonl"
}
# safe is false (and says why) on an OOM kill of anything but page-hog since the run
# began (HOG_UID is set when it is applied), or when k0s, Ollama or the agent stops answering.
safe() {
    local bad
    bad=$(journalctl -k --since "@$T0" --no-pager 2>/dev/null | grep 'oom-kill:' | grep -v "pod${HOG_UID:-none}" || true)
    [ -n "$bad" ] && { echo "OOM kill outside page-hog: $bad"; return 1; }
    for u in k0scontroller ollama di-agent; do systemctl is-active --quiet "$u.service" || { echo "$u.service is not active"; return 1; }; done
    curl -sf --max-time 10 $API/healthz >/dev/null || { echo "the agent does not answer"; return 1; }
    "${K[@]}" get --raw /readyz >/dev/null 2>&1 || { echo "k0s apiserver not ready"; return 1; }
}
tick() {
    local u
    for e in /relief /relief/actions /actuators "/state?id=memory_contention_ratio&id=memory_pressure_ratio&id=memory_utilization"; do poll "$e"; done
    for name in "$@"; do u=$(uid "$name"); [ -n "$u" ] && poll "/state?subject=pod:$u"; done
    node_line
}
# observe <minutes> <pods...>: tick every 30 s; on an unsafe node, delete page-hog and stop.
observe() {
    local end=$(( $(date +%s) + $1 * 60 )); shift
    while [ "$(date +%s)" -lt "$end" ]; do
        tick "$@"
        if ! why=$(safe); then
            mark "unsafe: $why; deleting page-hog"
            "${K[@]}" delete pod page-hog --ignore-not-found --wait=false >/dev/null
            finish; exit 2
        fi
        sleep 30
    done
}
# finish fetches every act and verify decision the actions name, and the whole action list.
finish() {
    poll /relief; poll /relief/actions; poll /actuators
    for id in $(tail -n 200 "$OUT/poll.jsonl" | jq -r 'select(.endpoint=="/relief/actions" and .status==200) | .body[]? | (.entry.decision_id // empty), (.verify_id // empty)' | sort -u); do
        poll "/state/decisions/$id"
    done
    mark "finished"
}

sleep 120; tick; mark "baseline taken"
"${K[@]}" apply -f workloads/mem-victim.yaml >/dev/null && mark "applied mem-victim ($(uid mem-victim))"
sleep 60
"${K[@]}" apply -f workloads/page-hog.yaml >/dev/null; sleep 5; HOG_UID=$(uid page-hog); mark "applied page-hog ($HOG_UID)"
observe "$P1" mem-victim page-hog
"${K[@]}" logs page-hog >"$OUT/page-hog.log" 2>/dev/null || true
"${K[@]}" delete pod page-hog mem-victim --wait=true >/dev/null; mark "deleted page-hog and mem-victim"
observe 5
if [ "$P2" -gt 0 ]; then
    mark "phase 2: mem-squeeze alone"
    "${K[@]}" apply -f workloads/mem-squeeze.yaml >/dev/null && mark "applied mem-squeeze ($(uid mem-squeeze))"
    observe "$P2" mem-squeeze
    "${K[@]}" delete pod mem-squeeze --wait=true >/dev/null; mark "deleted mem-squeeze"
fi
finish
