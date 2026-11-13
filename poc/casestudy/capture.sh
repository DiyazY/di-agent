#!/usr/bin/env bash
# capture.sh — record the agent's API as JSON lines until killed. Runs on the node.
#
# Usage: ./capture.sh <out-dir> [interval-seconds]
#
# Every line of <out-dir>/capture.jsonl is {"t", "endpoint", "status", "body"}, t in the
# node's epoch seconds. A poll that fails (the agent restarting, say) is written with
# status 0 and body null, so a gap is recorded rather than skipped; a 200 whose body is
# not JSON is written with body null and an "error". Either is a gap: body is null.

set -uo pipefail

OUT="${1:?usage: $0 <out-dir> [interval-seconds]}"
INTERVAL="${2:-30}"
API="${API:-http://localhost:9090}"
mkdir -p "$OUT"
FILE="$OUT/capture.jsonl"

# JOURNAL and EXTRA let a longer run poll the journal's tail and more endpoints (master.sh).
EVERY_TICK=(/state "/candidates?status=all" /state/relationships /relief /relief/actions "${JOURNAL:-/state/journal}" ${EXTRA:-})
EVERY_TENTH=(/state/decisions)
# Which binary and which spec answered: polled until both answer, and again after any
# failed /state poll, since an agent that went away may come back as another build or
# on another spec (the P3 restart does).
META=(/version /config)

poll() {
    local endpoint="$1" t body status
    t=$(date +%s.%N)
    body=$(curl -s --max-time 10 -w '\n%{http_code}' "$API$endpoint") || body=$'\n0'
    status="${body##*$'\n'}"
    status=$((10#${status:-0}))
    body="${body%$'\n'*}"
    if [ "$status" -ne 200 ]; then
        jq -cn --argjson t "$t" --arg e "$endpoint" --argjson s "$status" \
            '{t: $t, endpoint: $e, status: $s, body: null}' >>"$FILE"
        return 1
    fi
    # The body goes through stdin: /state can be larger than one argument may be. A 200
    # whose body is not JSON is a gap too, and says why rather than passing as an answer.
    local line
    # Buffered, so a body jq rejects halfway leaves no partial line behind.
    if line=$(jq -ec --argjson t "$t" --arg e "$endpoint" \
        '{t: $t, endpoint: $e, status: 200, body: .}' <<<"$body" 2>/dev/null) &&
        [ "$(wc -l <<<"$line")" -eq 1 ]; then
        printf '%s\n' "$line" >>"$FILE"
    else
        jq -cn --argjson t "$t" --arg e "$endpoint" \
            '{t: $t, endpoint: $e, status: 200, body: null, error: "body is not JSON"}' >>"$FILE"
        return 1
    fi
}

# Each verify decision, once, when an action first reports it: whether the outcome taught
# the map is recorded there, not on the action.
verify_decisions() {
    local id
    for id in $(tail -n 50 "$FILE" | jq -r 'select(.endpoint == "/relief/actions" and .status == 200)
            | .body[]?.verify_id // empty' | sort -u); do
        grep -qxF "$id" "$OUT/.fetched" 2>/dev/null && continue
        poll "/state/decisions/$id" && echo "$id" >>"$OUT/.fetched"
    done
}

jq -cn --argjson t "$(date +%s.%N)" --arg v "$(k0s version 2>/dev/null)" \
    '{t: $t, endpoint: "k0s version", status: 200, body: $v}' >>"$FILE"
meta_due=1
meta() {
    local ok=1 e
    for e in "${META[@]}"; do poll "$e" || ok=0; done
    [ "$ok" -eq 1 ] && meta_due=0
    return 0
}

tick=0
while true; do
    [ "$meta_due" -eq 1 ] && meta
    for e in "${EVERY_TICK[@]}"; do
        poll "$e" || { [ "$e" = /state ] && meta_due=1; }
    done
    verify_decisions
    if [ $((tick % 10)) -eq 0 ]; then
        for e in "${EVERY_TENTH[@]}"; do poll "$e"; done
    fi
    tick=$((tick + 1))
    sleep "$INTERVAL"
done
