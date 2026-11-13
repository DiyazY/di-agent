#!/usr/bin/env bash
# master.sh — the master scenario (plan 07): one node, no model, no person, phases M0–M9.
# Runs on the node, as root, from this directory, next to an `agent` (or `agent.new`).
#
# Usage: sudo ./master.sh <out-dir>              the whole run, about 3 h 20 min, up to 40 min more
#                                                 when M7 waits for relief to calm
#        sudo ./master.sh <out-dir> --phase M3   one phase, against the running agent
#
# The agent starts on a fresh map from agent-autonomous.yaml (ledger in <out-dir>); the
# harness injects the faults the plan names (Netdata outage, wall-clock step, kill -9,
# truncated snapshots) and undoes them. Every event is a JSON line in <out-dir>/phases.jsonl:
# {t, mono, phase, event, inc, seq, ...}. t is the wall clock, which M4 steps; mono is
# /proc/uptime and inc/seq the agent's journal position when it answers, so a readout can
# window the journal without trusting the wall clock. Timing runs on mono. SCALE=0.05
# runs the timetable at a twentieth of its length. capture.sh runs alongside.
set -uo pipefail
cd "$(dirname "$0")"
HERE="$PWD"
OUT="$(realpath -m "${1:?usage: $0 <out-dir> [--phase M0..M9]}")"
ONLY="${3:-}"
mkdir -p "$OUT"
API=http://localhost:9090
SCALE="${SCALE:-1}"
DIR=/var/lib/di-agent
STATE=$DIR/state.json
K=(k0s kubectl)
DISCOVERY=(cycler mem-wave mem-squeeze io-wave twin-a twin-b noise)
PLANTED=("${DISCOVERY[@]}" mem-victim page-hog probe-1 probe-2 probe-3 probe-4 probe-5 probe-6)
# The config: agent-autonomous.yaml with the ledger in this run's directory.
CONFIG="$OUT/agent.yaml"

mono() { awk '{printf "%.1f", $1}' /proc/uptime; }
pos() { curl -sf --max-time 5 "$API/state/journal?limit=1" | jq -c '{inc: .incarnation, seq: .seq}' 2>/dev/null || echo '{"inc":null,"seq":null}'; }

# mark <phase> <event> [jq-args producing extra fields]
mark() {
    local phase="$1" event="$2"; shift 2
    jq -cn --argjson t "$(date +%s.%N)" --argjson mono "$(mono)" --arg p "$phase" --arg e "$event" \
        --argjson pos "$(pos)" "$@" \
        '{t: $t, mono: $mono, phase: $p, event: $e} + $pos + ($ARGS.named | del(.t, .mono, .p, .e, .pos))' >>"$OUT/phases.jsonl"
    echo "[$(date +%T)] $phase: $event"
}

# run <phase> <command...>: run an injected fault or its undo, marking the command and its exit code.
run() {
    local phase="$1"; shift
    "$@" >>"$OUT/master.cmd.log" 2>&1
    local rc=$?
    mark "$phase" "ran: $*" --argjson exit "$rc"
    return $rc
}

# SHIFT is how many (scaled) minutes the timetable after M7's calm wait runs late: M7's
# probes wait for relief to report calm, and everything after moves with them.
SHIFT=0

# minute <m>: the uptime second of timetable minute m (shifted), scaled.
minute() { awk -v t0="$T0" -v m="$1" -v d="$SHIFT" -v s="$SCALE" 'BEGIN { printf "%d", t0 + (m + d) * 60 * s }'; }

# at <minutes>: sleep until that many (scaled) minutes after the agent started, on uptime.
at() {
    local target now
    target=$(minute "$1")
    now=$(awk '{printf "%d", $1}' /proc/uptime)
    [ "$target" -gt "$now" ] && sleep $((target - now))
    return 0
}

agent_start() {
    systemctl reset-failed di-agent.service 2>/dev/null || true
    systemd-run --unit=di-agent --collect --quiet -p AllowedCPUs=0-5 -p WorkingDirectory="$HERE" \
        -p StandardOutput="append:$OUT/agent.log" -p StandardError="append:$OUT/agent.log" \
        "$HERE/agent" -config "$CONFIG"
    for _ in $(seq 60); do
        curl -sf "$API/healthz" >/dev/null && return 0
        sleep 1
    done
    mark agent "did not come up"
    return 1
}

agent_stop() {
    pgrep -x agent >/dev/null || return 0
    systemctl kill --signal=TERM di-agent.service 2>/dev/null || pkill -TERM -x agent
    for _ in $(seq 60); do pgrep -x agent >/dev/null || return 0; sleep 1; done
    pkill -KILL -x agent; mark agent "did not stop within 60 s; killed"
    while pgrep -x agent >/dev/null; do sleep 1; done
}

# pause <seconds>: a sleep scaled like the timetable.
pause() { sleep "$(awk -v x="$1" -v s="$SCALE" 'BEGIN { printf "%d", x * s + 1 }')"; }

uid_of() { "${K[@]}" get pod "$1" -o jsonpath='{.metadata.uid}' 2>/dev/null || true; }
apply() { "${K[@]}" apply -f "$HERE/workloads/$1.yaml" >/dev/null; }
remove() { "${K[@]}" delete pod "$@" --ignore-not-found --wait=true >/dev/null; }
ready() { "${K[@]}" get node "$(hostname)" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null; }
get() { curl -s --max-time 10 "$API$1"; }

# safe is false, and says why, on an OOM kill of anything but page-hog since M6 began, or when
# k0s or the agent stops answering (the memrelief kill criteria; Ollama is stopped for this run).
safe() {
    local bad
    bad=$(journalctl -k --since "@${M6_WALL:-$(date +%s)}" --no-pager 2>/dev/null | grep 'oom-kill:' | grep -v "pod${HOG_UID:-none}" || true)
    [ -n "$bad" ] && { echo "OOM kill outside page-hog: $bad"; return 1; }
    systemctl is-active --quiet k0scontroller.service || { echo "k0scontroller.service is not active"; return 1; }
    curl -sf --max-time 10 "$API/healthz" >/dev/null || { echo "the agent does not answer"; return 1; }
    "${K[@]}" get --raw /readyz >/dev/null 2>&1 || { echo "k0s apiserver not ready"; return 1; }
}

# watch_until <minutes> <phase>: every 30 s, each relief pod's subject into poll.jsonl and the
# kill criteria checked; an unsafe node deletes the relief workloads and ends the run.
watch_until() {
    local target u name
    target=$(minute "$1")
    while [ "$(awk '{printf "%d", $1}' /proc/uptime)" -lt "$target" ]; do
        for name in mem-victim page-hog; do
            u=$(uid_of "$name"); [ -z "$u" ] && continue
            jq -cn --argjson t "$(date +%s.%N)" --arg n "$name" --argjson b "$(get "/state?subject=pod:$u" | jq -c . 2>/dev/null || echo null)" \
                '{t: $t, pod: $n, body: $b}' >>"$OUT/poll.jsonl"
        done
        if ! why=$(safe); then
            mark "$2" "unsafe: $why; deleting the relief workloads, run ends"
            "${K[@]}" delete pod page-hog mem-victim --ignore-not-found --wait=false >/dev/null
            teardown; exit 2
        fi
        sleep 30
    done
}

prepare() {
    # memory.high under kubepods must be max, or the actuator refuses (MemoryQoS).
    local managed
    managed=$(find /sys/fs/cgroup/kubepods -name memory.high -exec sh -c 'v=$(cat "$1"); [ "$v" != max ] && echo "$1 $v"' _ {} \;)
    [ -n "$managed" ] && { mark prep "refused: memory.high is set under kubepods" --arg managed "$managed"; exit 1; }
    [ "$(timedatectl show -p NTPSynchronized --value)" = yes ] || { mark prep "refused: NTP not synchronised"; exit 1; }
    agent_stop
    remove "${PLANTED[@]}"
    local stamp f
    stamp=$(date +%Y%m%dT%H%M%S)
    for f in "$STATE" "$STATE.prev" "$DIR"/journal.ndjson*; do
        [ -e "$f" ] && mv "$f" "$f.pre-master-$stamp" && mark prep "moved aside" --arg path "$f.pre-master-$stamp"
    done
    [ -e agent.new ] && mv agent.new agent
    { cat agent-autonomous.yaml; echo "actuation: { ledger: $OUT/actions.json }"; } >"$CONFIG"
    systemctl is-active --quiet ollama.service && echo active >"$OUT/ollama.was" || echo inactive >"$OUT/ollama.was"
    run prep systemctl stop ollama.service
    JOURNAL='/state/journal?limit=50' EXTRA='/collectors /autonomy /healthz /tasks /peers' \
        setsid nohup ./capture.sh "$OUT" 30 </dev/null >"$OUT/capture.err" 2>&1 &
    echo $! >"$OUT/capture.pid"
    mark prep "ready"
}

m0() {
    agent_start || exit 1
    T0=$(awk '{printf "%d", $1}' /proc/uptime)
    mark M0 start --argjson version "$(get /version)"
    at 10; mark M0 end --argjson snapshot_bytes "$(stat -c %s "$STATE" 2>/dev/null || echo null)"
}

m1() {
    at 10; mark M1 start
    local pod
    for pod in "${DISCOVERY[@]}"; do
        apply "$pod"; mark M1 "applied $pod" --arg subject "pod:$(uid_of "$pod")"
        pause 41
    done
    at 50; mark M1 end
}

m2() {
    at 50; mark M2 start
    local uid body
    uid=$(uid_of mem-wave)
    body=$(get "/state/estimate?target=memory_utilization&without=pod:$uid")
    mark M2 "estimate without mem-wave" --arg subject "pod:$uid" \
        --argjson estimate "$(jq -c '{decision_id, excluded, answer}' <<<"$body" 2>/dev/null || echo null)"
    remove mem-wave; mark M2 "deleted mem-wave" --arg subject "pod:$uid"
    at 65; mark M2 end
}

m3() {
    at 65; mark M3 start
    run M3 systemctl stop netdata.service
    at 72
    local body
    body=$(curl -s --max-time 10 -X POST -H 'Content-Type: application/json' "$API/tasks" \
        -d '{"name":"offer-m3","demand":{"cpu":{"value":0.1,"source":"declared"},"memory":{"value":134217728,"source":"declared"}}}')
    mark M3 "offered offer-m3 (not applied)" --argjson response "$(jq -c . <<<"$body" 2>/dev/null || echo null)"
    at 78
    run M3 systemctl start netdata.service
    at 80; mark M3 end
}

m4() {
    at 80; mark M4 start
    run M4 timedatectl set-ntp false
    run M4 date -s "+2 hours"
    sleep 20; mark M4 "node ready after forward step" --arg ready "$(ready)"
    at 84
    run M4 date -s "-2 hours"
    run M4 timedatectl set-ntp true
    sleep 20; mark M4 "node ready after backward step" --arg ready "$(ready)"
    at 86
    local uid; uid=$(uid_of noise)
    remove noise; mark M4 "deleted noise" --arg subject "pod:$uid"
    mark M4 end
}

m5() {
    at 87; mark M5 start
    # What the agent has learned, just before the kill.
    jq -cn --argjson relief "$(get /relief)" --argjson peers "$(get /peers)" \
        --argjson calibration "$(get /state | jq -c .calibration)" --argjson actions "$(get /relief/actions)" \
        --argjson tasks "$(get /tasks)" '{relief: $relief, peers: $peers, calibration: $calibration, actions: $actions, tasks: $tasks}' \
        >"$OUT/before-kill.json"
    mark M5 "learned state saved" --arg file before-kill.json
    run M5 pkill -KILL -x agent
    while pgrep -x agent >/dev/null; do sleep 1; done
    agent_start; mark M5 "restarted after kill -9"
    jq -cn --argjson relief "$(get /relief)" --argjson peers "$(get /peers)" \
        --argjson calibration "$(get /state | jq -c .calibration)" --argjson actions "$(get /relief/actions)" \
        --argjson tasks "$(get /tasks)" '{relief: $relief, peers: $peers, calibration: $calibration, actions: $actions, tasks: $tasks}' \
        >"$OUT/after-kill.json"
    at 94
    agent_stop; mark M5 "stopped"
    run M5 truncate -s "$(($(stat -c %s "$STATE") / 2))" "$STATE"
    agent_start; mark M5 "restarted on a truncated state.json"
    at 97
    agent_stop; mark M5 "stopped"
    run M5 truncate -s "$(($(stat -c %s "$STATE") / 2))" "$STATE"
    run M5 truncate -s "$(($(stat -c %s "$STATE.prev") / 2))" "$STATE.prev"
    agent_start; mark M5 "restarted on truncated state.json and state.json.prev"
    at 100; mark M5 end
}

m6() {
    at 100; mark M6 start
    M6_WALL=$(date +%s)
    apply mem-victim; mark M6 "applied mem-victim" --arg subject "pod:$(uid_of mem-victim)"
    pause 60
    # ANON_GIB 11, not 6: Ollama held 6-9 GB in the memrelief runs and is stopped here.
    sed -e "s/__VICTIM_UID__/$(uid_of mem-victim)/" -e 's/name: ANON_GIB, value: "6"/name: ANON_GIB, value: "11"/' \
        workloads/page-hog.yaml | "${K[@]}" apply -f - >/dev/null
    sleep 5; HOG_UID=$(uid_of page-hog)
    mark M6 "applied page-hog" --arg subject "pod:$HOG_UID"
    watch_until 145 M6
    "${K[@]}" logs page-hog >"$OUT/page-hog.log" 2>/dev/null || true
    mark M6 end
}

offer() {
    curl -s --max-time 10 -X POST -H 'Content-Type: application/json' "$API/tasks" \
        -d "{\"name\":\"$1\",\"demand\":{\"cpu\":{\"value\":0.1,\"source\":\"declared\"},\"memory\":{\"value\":268435456,\"source\":\"declared\"}}}"
}

# calm_wait <max minutes>: after page-hog is deleted, wait (bounded) until every relief
# lever reports calm, since admission declines while relief is active; the probes and
# the rest of the timetable then run SHIFT minutes late.
CALM_WAIT=40
calm_wait() {
    local deadline phases event
    deadline=$(( $(awk '{printf "%d", $1}' /proc/uptime) + $(awk -v m="$1" -v s="$SCALE" 'BEGIN { printf "%d", m * 60 * s }') ))
    while :; do
        phases=$(get /relief | jq -c '[.levers[].phase]' 2>/dev/null)
        jq -e 'length > 0 and all(. == "calm")' <<<"${phases:-null}" >/dev/null 2>&1 && { event="relief calm"; break; }
        [ "$(awk '{printf "%d", $1}' /proc/uptime)" -ge "$deadline" ] && { event="relief not calm after the wait; probing anyway"; break; }
        sleep 15
    done
    # Minutes late against the first probe's slot (146), rounded up, never negative.
    SHIFT=$(awk -v now="$(awk '{print $1}' /proc/uptime)" -v t0="$T0" -v s="$SCALE" \
        'BEGIN { late = (now - t0) / (60 * s) - 146; d = int(late); if (late > d) d++; if (d < 0) d = 0; print d }')
    mark M7 "$event" --argjson phases "${phases:-null}" --argjson shift_min "$SHIFT"
}

m7() {
    at 145; mark M7 start
    remove page-hog; mark M7 "deleted page-hog"
    calm_wait "$CALM_WAIT"
    local n name body accept slot
    for n in 1 2 3 4 5 6; do
        slot=$((145 + 4 * (n - 1) + 1))
        at "$slot"
        name=probe-$n
        body=$(offer "$name")
        accept=$(jq -r 'select(.accepted_by != null) | .decision_id // empty' <<<"$body" 2>/dev/null)
        mark M7 "offered $name" --argjson response "$(jq -c . <<<"$body" 2>/dev/null || echo null)"
        [ -z "$accept" ] && continue
        sed "s/__NAME__/$name/" workloads/probe.yaml | "${K[@]}" apply -f - >/dev/null
        mark M7 "applied $name" --arg subject "pod:$(uid_of "$name")" --arg accept "$accept"
        # Wait for the outcome citing the accept, up to the next slot (or M8 for the last).
        local last=$((slot + 4)); [ "$n" -eq 6 ] && last=175
        local target; target=$(( $(minute "$last") - 10 ))
        local outcome=""
        while [ "$(awk '{printf "%d", $1}' /proc/uptime)" -lt "$target" ]; do
            outcome=$(get "/state/decisions?kind=admission.outcome&limit=50" |
                jq -r --arg a "$accept" '[.decisions[] | select((.cites // []) | index($a))][0].id // empty' 2>/dev/null)
            [ -n "$outcome" ] && break
            if ! why=$(safe); then
                mark M7 "unsafe: $why; deleting the relief workloads, run ends"
                "${K[@]}" delete pod mem-victim "$name" --ignore-not-found --wait=false >/dev/null
                teardown; exit 2
            fi
            sleep 15
        done
        mark M7 "outcome for $name" --arg outcome "$outcome"
        remove "$name"; mark M7 "deleted $name"
    done
    mark M7 end
}

m8() {
    at 175; mark M8 start
    remove "${PLANTED[@]}"; mark M8 "deleted every planted pod"
    at 188
    local body; body=$(get "/state/estimate?target=memory_utilization")
    mark M8 "estimate at 188" --arg decision_id "$(jq -r '.decision_id // empty' <<<"$body" 2>/dev/null)" \
        --arg pace "$(get /state | jq -r .pace)"
    at 190
    apply noise; mark M8 "applied noise" --arg subject "pod:$(uid_of noise)"
    at 195; mark M8 end
}

# M9 takes no action on the node. Before the agent stops, it reads what readout.sh cannot read
# offline: the last relief verify's chain and narrative, and the run's first decision, which
# the in-memory journal has long evicted (from: log).
m9() {
    at 195; mark M9 start
    local log="$DIR/journal.ndjson" verify first
    verify=$(jq -r 'select(.kind == "decision" and .decision.kind == "relief.verify") | .decision.id' "$log" 2>/dev/null | tail -1)
    first=$(jq -r 'select(.kind == "decision") | .decision.id' "$log" 2>/dev/null | head -1)
    if [ -n "$verify" ]; then
        get "/state/decisions/$verify?chain=1" >"$OUT/m9-verify-chain.json"
        curl -s --max-time 10 -D "$OUT/m9-verify-explain.headers" "$API/state/decisions/$verify/explain" >"$OUT/m9-verify-explain.txt"
    fi
    [ -n "$first" ] && get "/state/decisions/$first" >"$OUT/m9-first-decision.json"
    mark M9 "read for the readout" --arg verify "$verify" --arg first "$first"
    at 200; mark M9 end --argjson snapshot_bytes "$(stat -c %s "$STATE" 2>/dev/null || echo null)"
}

# teardown: planted pods deleted, faults undone, ollama restored, files copied out.
teardown() {
    mark teardown start
    remove "${PLANTED[@]}"
    systemctl is-active --quiet netdata.service || run teardown systemctl start netdata.service
    [ "$(timedatectl show -p NTP --value)" = yes ] || run teardown timedatectl set-ntp true
    get /state >"$OUT/final-state.json"
    agent_stop
    [ -e "$OUT/capture.pid" ] && kill "$(cat "$OUT/capture.pid")" 2>/dev/null
    cp -a "$STATE" "$STATE.prev" "$DIR"/journal.ndjson "$OUT/" 2>/dev/null
    [ -e "$DIR/journal.ndjson.1" ] && cp -a "$DIR/journal.ndjson.1" "$OUT/"
    [ "$(cat "$OUT/ollama.was" 2>/dev/null)" = active ] && run teardown systemctl start ollama.service
    chmod -R a+r "$OUT"
    mark teardown end
}

if [ -n "$ONLY" ]; then
    declare -A START=([M0]=0 [M1]=10 [M2]=50 [M3]=65 [M4]=80 [M5]=87 [M6]=100 [M7]=145 [M8]=175 [M9]=195)
    T0=$(awk -v n="$(awk '{printf "%d", $1}' /proc/uptime)" -v m="${START[$ONLY]:?unknown phase $ONLY}" -v s="$SCALE" 'BEGIN { printf "%d", n - m * 60 * s }')
    "${ONLY,,}"
    exit $?
fi

prepare
m0; m1; m2; m3; m4; m5; m6; m7; m8; m9
teardown
