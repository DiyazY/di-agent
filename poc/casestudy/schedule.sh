#!/usr/bin/env bash
# schedule.sh — run the case study's phases. Runs on the node, as root.
#
# Usage: sudo ./schedule.sh <out-dir>              the whole run, about 2 h 10 min
#        sudo ./schedule.sh <out-dir> --phase P2   one phase, against the running agent
#
# Starts the agent itself (from ./agent, ./agent.yaml, ./agent-relief.yaml) on a fresh map:
# a snapshot already on disk is moved aside, never deleted. Every event is a JSON line in
# <out-dir>/phases.jsonl: {"t", "phase", "event", ...ids}. SCALE=0.1 runs the timetable at
# a tenth of its length, for trying the script out.

set -euo pipefail

OUT="${1:?usage: $0 <out-dir> [--phase P0..P5]}"
ONLY="${3:-}"
[ "${2:-}" = "--phase" ] || [ -z "${2:-}" ] || { echo "unknown argument $2" >&2; exit 2; }
# --phase with no name would otherwise run the whole timetable, whose first step stops
# the running agent and moves its snapshot aside.
[ "${2:-}" != "--phase" ] || [ -n "$ONLY" ] || { echo "--phase needs a phase name (P0..P5)" >&2; exit 2; }

HERE="$(cd "$(dirname "$0")" && pwd)"
API="${API:-http://localhost:9090}"
SCALE="${SCALE:-1}"
STATE=/var/lib/di-agent/state.json
KUBECTL=(k0s kubectl)
DISCOVERY=(cycler mem-wave mem-squeeze io-wave twin-a twin-b noise)

mkdir -p "$OUT"
PHASES="$OUT/phases.jsonl"

# mark <phase> <event> [jq-args producing extra fields as $x]
mark() {
    local phase="$1" event="$2"; shift 2
    jq -cn --argjson t "$(date +%s.%N)" --arg p "$phase" --arg e "$event" "$@" \
        '{t: $t, phase: $p, event: $e} + ($ARGS.named | del(.t, .p, .e))' >>"$PHASES"
    echo "[$(date +%T)] $phase: $event"
}

# at <minutes>: sleep until that many (scaled) minutes after the agent started.
at() {
    local target now
    target=$(awk -v t0="$T0" -v m="$1" -v s="$SCALE" 'BEGIN { printf "%d", t0 + m * 60 * s }')
    now=$(date +%s)
    [ "$target" -gt "$now" ] && sleep $((target - now))
    return 0
}

# The agent runs as the unit di-agent.service, so it is under system.slice and its own
# cgroup collector models it as unit:di-agent — the agent is one of the node's consumers.
agent_start() {
    local config="$1"
    systemctl reset-failed di-agent.service 2>/dev/null || true
    systemd-run --unit=di-agent --collect --quiet -p AllowedCPUs=0-5 -p WorkingDirectory="$HERE" \
        -p StandardOutput="append:$OUT/agent.log" -p StandardError="append:$OUT/agent.log" \
        "$HERE/agent" -config "$HERE/$config"
    for _ in $(seq 60); do
        curl -sf "$API/healthz" >/dev/null && return 0
        sleep 1
    done
    echo "agent did not come up on $config" >&2
    exit 1
}

# agent_stop asks the agent to stop and waits up to a minute for it to flush its
# snapshot; one that has not gone by then is killed, and the mark says so.
agent_stop() {
    local pid
    pid=$(pgrep -x agent || true)
    [ -z "$pid" ] && return 0
    systemctl kill --signal=TERM di-agent.service 2>/dev/null || kill -TERM $pid
    for _ in $(seq 60); do
        pgrep -x agent >/dev/null || return 0
        sleep 1
    done
    pkill -KILL -x agent || true
    mark agent "did not stop within 60 s; killed"
    while pgrep -x agent >/dev/null; do sleep 1; done
}

# The map's revision, or null when the agent does not answer: a mark records the gap
# rather than set -e ending the timetable mid-phase.
revision() { curl -sf "$API/state" | jq '.revision' 2>/dev/null || echo null; }

uid_of() { "${KUBECTL[@]}" get pod "$1" -o jsonpath='{.metadata.uid}' 2>/dev/null || true; }

apply() { "${KUBECTL[@]}" apply -f "$HERE/workloads/$1.yaml" >/dev/null; }

remove() { "${KUBECTL[@]}" delete pod "$@" --ignore-not-found --wait=true >/dev/null; }

phase_prepare() {
    agent_stop
    if [ -e "$STATE" ]; then
        local aside="$STATE.pre-casestudy-$(date +%Y%m%dT%H%M%S)"
        mv "$STATE" "$aside"
        mark prep "snapshot moved aside" --arg path "$aside"
    fi
    remove "${DISCOVERY[@]}" hog victim
    mark prep "planted pods removed"
}

phase_P0() {
    agent_start agent.yaml
    T0=$(date +%s)
    mark P0 "agent started, fresh map" --argjson revision "$(revision)"
}

phase_P1() {
    at 10
    for pod in "${DISCOVERY[@]}"; do
        apply "$pod"
        mark P1 "applied $pod" --arg subject "pod:$(uid_of "$pod")"
        sleep 41
    done
}

phase_P2() {
    at 50
    local uid subject ids id estimate status decision
    uid=$(uid_of mem-wave)
    if [ -z "$uid" ]; then
        mark P2 "refused: mem-wave is not running, nothing deleted"
        return 1
    fi
    subject="pod:$uid"

    # The agent's review loop settles what discovery proposed about mem-wave and node
    # memory — a composition candidate, confirmed by rule — and nothing here confirms
    # anything. P2 waits for that relationship to exist and to have been observed, since
    # one confirmed a moment ago has no strength and the estimate would leave it out.
    local observed=0 by="" review=""
    for _ in $(seq 120); do
        observed=$(curl -sf "$API/state/relationships" | jq --arg f "memory_utilization@$subject" \
            '[.relationships[] | select(.from == $f and .to == "memory_utilization") | .n_observations] | max // 0' || echo 0)
        [ "$observed" -gt 0 ] && break
        sleep 5
    done
    if [ "$observed" -eq 0 ]; then
        mark P2 "refused: the agent has not confirmed mem-wave's memory against node memory after 10 min, nothing deleted" \
            --arg subject "$subject"
        return 1
    fi
    review=$(curl -sf "$API/candidates?status=all" | jq -c --arg f "memory_utilization@$subject" \
        '[.[] | select(.FromID == $f and .ToID == "memory_utilization") | .Review // empty] | last // null' || echo null)
    by=$(jq -r '.By // "unknown"' <<<"$review" 2>/dev/null || echo unknown)
    mark P2 "agent confirmed the relationship" --arg subject "$subject" --arg by "$by" --argjson review "$review"
    mark P2 "relationship observed" --arg subject "$subject" --argjson n_observations "$observed"

    estimate=$(curl -s -w '\n%{http_code}' "$API/state/estimate?target=memory_utilization&without=$subject") || estimate=$'\n0'
    status="${estimate##*$'\n'}"
    estimate="${estimate%$'\n'*}"
    decision=$(jq -r '.decision_id // empty' <<<"$estimate" 2>/dev/null || true)
    # A 200 alone proves nothing: the map answers for a subject it does not hold, and
    # says so only by leaving it out of "excluded".
    if [ "$status" != 200 ] || [ -z "$decision" ] ||
        ! jq -e --arg s "$subject" '.excluded | index($s)' >/dev/null <<<"$estimate"; then
        mark P2 "refused: the estimate did not exclude mem-wave, nothing deleted" \
            --arg subject "$subject" --arg status "$status"
        return 1
    fi
    mark P2 "estimate without mem-wave" --arg subject "$subject" --arg decision_id "$decision" \
        --argjson level "$(jq '.answer.level' <<<"$estimate")" \
        --argjson projected "$(jq '.hypothetical.projected_level' <<<"$estimate")"
    # The same question of mem-wave's group, which mem-squeeze shares: one request takes
    # every member away. Recorded, not required; nothing below depends on it.
    local group
    group=$(curl -s "$API/state/estimate?target=memory_utilization&without=group:memory" || true)
    mark P2 "estimate without group:memory" --arg decision_id "$(jq -r '.decision_id // empty' <<<"$group" 2>/dev/null)" \
        --argjson level "$(jq '.answer.level // null' <<<"$group" 2>/dev/null || echo null)" \
        --argjson projected "$(jq '.hypothetical.projected_level // null' <<<"$group" 2>/dev/null || echo null)" \
        --argjson excluded "$(jq -c '.excluded // []' <<<"$group" 2>/dev/null || echo '[]')"

    if ! remove mem-wave; then
        mark P2 "deleting mem-wave failed" --arg subject "$subject"
        return 1
    fi
    mark P2 "deleted mem-wave" --arg subject "$subject"
}

phase_P3() {
    at 65
    remove cycler mem-squeeze io-wave twin-a twin-b noise
    mark P3 "remaining planted pods removed"
    local before after
    before=$(revision)
    agent_stop
    mark P3 "agent stopped" --argjson revision "$before"
    local snapshot=false
    [ -e "$STATE" ] && snapshot=true
    agent_start agent-relief.yaml
    after=$(revision)
    mark P3 "agent restarted, relief on" --argjson revision "$after" --argjson from_snapshot "$snapshot"
}

# A hog of six threads beside a victim of three, both requesting 100m: the kernel's
# cpu.weight gives each its weighted share of the pods' CPUs, so the victim gets the
# three cores it runs on and the hog waits on its own threads past its share. Nothing
# is owed to anyone, cpu_contention_ratio stays near zero, and the cpu.limit lever
# must not act: capping the hog would only make its own wait worse. Runs 2026-11-05
# showed this with the victim at 100m and again at a 3-core request (results in
# run-2026-11-05-victim-req3). Checked five minutes in and again at steady state.
phase_P4() {
    at 72
    apply hog
    mark P4 "applied hog" --arg subject "pod:$(uid_of hog)"
    apply victim
    mark P4 "applied victim" --arg subject "pod:$(uid_of victim)"
    at 77
    cpu_relief_calm "five minutes in"
    at 100
    cpu_relief_calm "at steady state"
}

# cpu_relief_calm marks the cpu.limit lever's phase and action count with the node's
# contention and pressure, the verdict in the event: "calm" only when the lever is calm,
# has not acted, and the node's contention is under OWED_TOLERANCE — so the check holds
# P4's premise that nothing is owed, not just that relief did nothing (no reading is not
# calm: a lever with no signal is calm by default). A read that fails is recorded as
# null, as revision() does, rather than let set -e end the timetable before P5 removes
# the pods.
OWED_TOLERANCE=0.05 # the measure's own waitingFloor: below it a pod is not waiting
cpu_relief_calm() {
    local lever contention pressure verdict=calm
    lever=$(curl -sf "$API/relief" | jq -c '[.levers[] | select(.action == "cpu.limit") | {phase, actions}][0]' 2>/dev/null || echo null)
    contention=$(curl -sf "$API/state" | jq '[.properties[] | select(.id == "cpu_contention_ratio") | .value][0]' 2>/dev/null || echo null)
    pressure=$(curl -sf "$API/state" | jq '[.properties[] | select(.id == "cpu_pressure_ratio") | .value][0]' 2>/dev/null || echo null)
    if [ "$(jq -r '"\(.phase) \(.actions)"' <<<"${lever:-null}")" != "calm 0" ] ||
        ! jq -en --argjson c "${contention:-null}" --argjson t "$OWED_TOLERANCE" '$c != null and $c < $t' >/dev/null 2>&1; then
        verdict="not calm"
        echo "cpu.limit not calm $1: lever ${lever:-null}, contention ${contention:-null}" >&2
    fi
    mark P4 "cpu.limit $verdict $1" --argjson lever "${lever:-null}" --argjson contention "${contention:-null}" --argjson pressure "${pressure:-null}"
}

phase_P5() {
    at 117
    remove hog victim
    mark P5 "deleted hog and victim"
    at 127
    mark P5 "run ends" --argjson revision "$(revision)"
}

# One phase against the running agent, at its real pace: the clock is set as though
# the run had started that phase's offset ago.
if [ -n "$ONLY" ]; then
    declare -A START=([P0]=0 [P1]=10 [P2]=50 [P3]=65 [P4]=72 [P5]=117)
    T0=$(awk -v n="$(date +%s)" -v m="${START[$ONLY]:?unknown phase $ONLY}" -v s="$SCALE" 'BEGIN { printf "%d", n - m * 60 * s }')
    "phase_$ONLY"
    exit $?
fi

phase_prepare
phase_P0
phase_P1
# A refused P2 is marked and skipped; the rest of the timetable still runs, so the
# planted pods are removed and relief is still exercised.
phase_P2 || mark P2 "skipped; the timetable continues"
phase_P3
phase_P4
phase_P5
