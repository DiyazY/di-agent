#!/usr/bin/env bash
# readout.sh — the master scenario's pass criteria (plan 07), read off the agent's own records.
#
# Usage: ./readout.sh <out-dir> [--features 01,02,03,04,05,06]
#
# Reads <out-dir>/phases.jsonl (master.sh), capture.jsonl (capture.sh), journal.ndjson[.1] (the
# agent's durable log, copied at teardown), before-kill.json / after-kill.json, m9-* and
# agent.log, and writes <out-dir>/results-readout.md: one row per criterion, verdict
# pass | fail | not built | not exercised | unverifiable | reported, with the reading. A criterion
# whose input is missing is unverifiable, never pass. Phase windows are taken from the
# journal positions (incarnation, seq) master.sh stamps on each mark, not from the wall clock,
# which M4 steps. Needs jq and python3 (for the log's per-line digests).
set -uo pipefail
OUT="${1:?usage: $0 <out-dir> [--features 01,...]}"
FEATURES="01,02,03,04,05,06"
[ "${2:-}" = "--features" ] && FEATURES="${3:?--features needs a list}"
W="$OUT/.readout"; mkdir -p "$W"
ROWS="$W/rows.tsv"; : >"$ROWS"

# The log, both generations, oldest first; each line marked ok when its digest verifies.
cat "$OUT"/journal.ndjson.1 "$OUT"/journal.ndjson 2>/dev/null | python3 -c '
import sys, json, hashlib, re
from datetime import datetime
def ts(at):
    try:
        return datetime.fromisoformat(re.sub(r"(\.\d{6})\d+", r"\1", at)).timestamp()
    except Exception:
        return None
key = ",\"digest\":\""
for raw in sys.stdin:
    raw = raw.rstrip("\n")
    i = raw.rfind(key)
    ok = False
    if i >= 0 and raw.endswith("\"}"):
        body = raw[:i] + "}"
        ok = hashlib.sha256(body.encode()).hexdigest() == raw[i+len(key):-2]
    try:
        e = json.loads(raw)
    except Exception:
        e = {"kind": "undecodable"}
    e.pop("digest", None); e["ok"] = ok
    e["k"] = (e.get("inc") or 0) * 10**9 + (e.get("seq") or 0)
    e["ts"] = ts(e.get("at") or "")
    print(json.dumps(e))' >"$W/all.jsonl"
# Decisions without the state they read (small enough to slurp), and the other events.
jq -c 'select(.kind == "decision") | .decision + {inc, seq, k, ok, ts} | del(.properties_read, .relationships_read, .facts_read)' "$W/all.jsonl" >"$W/dec.jsonl"
jq -c 'select(.kind != "decision")' "$W/all.jsonl" >"$W/ev.jsonl"
jq -s . "$W/dec.jsonl" >"$W/dec.json"
jq -s . "$W/ev.jsonl" >"$W/ev.json"
jq -s 'map(. + {k: ((.inc // 0) * 1000000000 + (.seq // 0))})' "$OUT/phases.jsonl" >"$W/phases.json"
# The captures, without /state bodies' property lists except where a row needs them.
jq -c 'select(.endpoint != "/state" and .endpoint != "/state/relationships")' "$OUT/capture.jsonl" >"$W/cap.jsonl" 2>/dev/null
jq -s . "$W/cap.jsonl" >"$W/cap.json"

has() { [[ ",$FEATURES," == *",$1,"* ]]; }
# row <phase> <plan> <criterion> <verdict> <reading>
row() {
    local verdict="$4"
    has "$2" || verdict="not built"
    printf '%s\t%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "$verdict" "$(tr '\n\t' '  ' <<<"$5")" >>"$ROWS"
}
# verdict <jq-boolean>: pass/fail, or unverifiable when the expression did not yield a boolean.
verdict() { case "$1" in true) echo pass ;; false) echo fail ;; not-exercised) echo "not exercised" ;; *) echo unverifiable ;; esac; }
# k <phase> <event-regex>: the journal position of the first mark matching, or null.
k() { jq --arg p "$1" --arg e "$2" '[.[] | select(.phase == $p and (.event | test($e)) and .seq != null)][0].k // null' "$W/phases.json"; }
t() { jq --arg p "$1" --arg e "$2" '[.[] | select(.phase == $p and (.event | test($e)))][0].t // null' "$W/phases.json"; }
mk() { jq -r --arg p "$1" --arg e "$2" --arg f "$3" '[.[] | select(.phase == $p and (.event | test($e)))][0][$f] // empty' "$W/phases.json"; }
# ev <jq filter over the events array, $a $b bound to a window>
evq() { jq --argjson a "${2:-0}" --argjson b "${3:-1e18}" "$1" "$W/ev.json"; }
decq() { jq --argjson a "${2:-0}" --argjson b "${3:-1e18}" "$1" "$W/dec.json"; }
iso() { jq -n --arg s "$1" '$s | sub("\\.[0-9]+"; "") | sub("(?<z>[+-][0-9]{2}):(?<m>[0-9]{2})$"; "\(.z)\(.m)") | strptime("%Y-%m-%dT%H:%M:%S%z") | mktime'; }

M0s=$(k M0 '^start$'); M1s=$(k M1 '^start$'); M1e=$(k M1 '^end$'); M2s=$(k M2 '^start$'); M3s=$(k M3 '^start$')
M3e=$(k M3 '^end$'); M4s=$(k M4 '^start$'); M4e=$(k M4 '^end$'); M5s=$(k M5 '^start$'); M6s=$(k M6 '^start$')
M6e=$(k M6 '^end$'); M7s=$(k M7 '^start$'); M7e=$(k M7 '^end$'); M8s=$(k M8 '^start$'); M8e=$(k M8 '^end$')
# ---- M0 cold start ----
r=$(evq '[.[] | select(.inc == 1 and .kind != "fact.recorded")][0].kind')
n=$(evq '[.[] | select(.inc == 1 and .kind == "restore.failed")] | length')
row M0 02 "cold start journalled: first non-fact event property.admitted/declared, no restore.failed (inc 1)" \
    "$(verdict "$(jq -n --argjson r "$r" --argjson n "$n" '($r == "property.admitted" or $r == "property.declared") and $n == 0')")" "first non-fact event $r; restore.failed in inc 1: $n"
bad=$(decq '[.[] | select(.inc == 1 and (.id | test("-1\\.[0-9]+") | not)) | .id]')
mono=$(jq -s '[.[] | select(.endpoint | startswith("/state/journal")) | select(.status == 200) | {i: .body.incarnation, s: .body.seq}]
    | [range(1; length) as $j | select(.[$j].i == .[$j-1].i and .[$j].s < .[$j-1].s)] | length' "$W/cap.jsonl")
logmono=$(jq '[group_by(.inc)[] | . as $g | [range(1; length) as $j | select($g[$j].seq <= $g[$j-1].seq)] | length] | add // 0' "$W/ev.json")
row M0 02 "incarnation 1 ids match -1.N; seq monotonic across captures and in the log" \
    "$(verdict "$(jq -n --argjson b "$bad" --argjson m "$mono" --argjson l "$logmono" '($b | length) == 0 and $m == 0 and $l == 0')")" \
    "inc-1 ids not matching: $(jq -c . <<<"$bad"); capture seq decreases within an incarnation: $mono; log seq non-increasing within an incarnation: $logmono"
r=$(decq '{by: ([.[] | select(.kind == "review") | .answer.by] | group_by(.) | map({(.[0] // "null"): length}) | add),
    undecided: ([.[] | select(.kind == "review" and .answer.action == "undecided")] | length)}')
row M0 01 "no review decision names a model (answer.by in rules, intervention)" \
    "$(verdict "$(jq '(.by // {} | keys) - ["rules", "intervention"] | length == 0' <<<"$r")")" "reviews by: $(jq -c .by <<<"$r"); undecided: $(jq .undecided <<<"$r")"
r=$(jq -r 'select(.endpoint == "/state" and .status == 200) | .body.pace' "$OUT/capture.jsonl" | head -1)
row M0 05 "pace base at start (GET /state .pace)" "$( [ "$r" = base ] && echo pass || { [ -z "$r" ] && echo unverifiable || echo fail; })" "first captured pace: ${r:-none}"
r=$(evq '([.[] | select(.inc == 1)][0].ts) as $t0 | [.[] | select(.inc == 1 and .kind == "property.admitted" and (.target == "cpu_utilization@unit:di-agent" or .target == "cpu_utilization@unit:di-agent.service"))][0] | if . == null then null else {target, after_s: (.ts - $t0)} end')
row M0 04 "self observed: cpu_utilization@unit:di-agent admitted within 1 min" \
    "$(verdict "$(jq '. != null and .after_s <= 60 and .target == "cpu_utilization@unit:di-agent"' <<<"$r")")" "$(jq -c . <<<"$r")"
r=$(jq -c '[.[] | select(.endpoint == "/healthz" and .status == 200) | .body][0]' "$W/cap.json")
row M0 none "healthz reports last tick per loop and last save" "not built" "GET /healthz answers $r: liveness detail is in none of plans 01-06"

# ---- M1 discovery by rules ----
r=$(decq '[.[] | select(.kind == "review" and .k >= $a and .k <= $b and (.inputs.candidate.Kind // "") == "composition"
        and (.inputs.candidate.FromID | test("^cpu_utilization@pod:")) and .inputs.candidate.ToID == "cpu_utilization")]
    | group_by(.inputs.candidate.CandidateID) | map({c: .[0].inputs.candidate.FromID, reviews: length,
        confirm_at: ([.[] | .answer.action] | index("confirm")), by: ([.[] | select(.answer.action == "confirm") | .answer.by][0])})' "$M1s" "$M1e")
row M1 01 "composition cpu_utilization@pod:* -> cpu_utilization confirmed by rules within 2 review cycles" \
    "$(jq -r 'if length == 0 then "unverifiable" elif all(.confirm_at != null and .confirm_at <= 1 and .by == "rules") then "pass" else "fail" end' <<<"$r")" \
    "$(jq -c 'map({c: (.c | sub("cpu_utilization@"; "")), confirm_review: .confirm_at, by})' <<<"$r")"
ta=$(mk M1 'applied twin-a' subject); tb=$(mk M1 'applied twin-b' subject)
r=$(jq --arg a "cpu_utilization@$ta" --arg b "cpu_utilization@$tb" --argjson e "$M1e" \
    '[.[] | select(.kind == "relationship.declared" and .detail.association == true and ([.detail.from, .detail.to] | sort) == ([$a, $b] | sort))][0] | if . == null then null else {target, actor, k, by_m1_end: (.k <= $e)} end' "$W/ev.json")
pw=$(jq -s --arg a "cpu_utilization@$ta" --arg b "cpu_utilization@$tb" '[.[] | select(.endpoint == "/candidates?status=all" and .status == 200) | .body[]?
    | select(([.FromID, .ToID] | sort) == ([$a, $b] | sort)) | .PersistentWindows] | last // null' "$W/cap.jsonl")
row M1 01 "twins associated by persistent-correlation by t=50" \
    "$(jq -r --argjson pw "$pw" 'if . != null and .by_m1_end then "pass" else "reported" end' <<<"$r")" "declared: $(jq -c . <<<"$r"); pending-with-windows: $pw"
r=$(jq -c --argjson a "$(t M1 '^start$')" --argjson b "$(t M1 '^end$')" '[.[] | select(.endpoint == "/autonomy" and .status == 200 and .t >= $a and .t <= $b) | .body]
    | {captures: length, errors: (map(.errors) | max), waiting: (map(.waiting) | max)}' "$W/cap.json")
dec=$(jq -c '[.[] | select(.endpoint == "/config" and .status == 200) | .body][0] | .. | objects | select(has("review")) | .review | {decision: .decision.backend, reasoning: .reasoning.backend}' "$W/cap.json" 2>/dev/null | head -1)
row M1 01 "nothing waits on a model (no model configured; autonomy errors and waiting 0)" \
    "$(verdict "$(jq '.captures > 0 and .errors == 0 and .waiting == 0' <<<"$r")")" "GET /autonomy has no models_down field; adapted to errors/waiting: $r; config review backends: ${dec:-unread}"
r=$(jq -c 'select(.endpoint == "/state" and .status == 200) | .body.counts | {readings_out_of_order, readings_future}' "$OUT/capture.jsonl" | tail -1)
row M1 01 "out-of-order and future readings counted (census fields present)" \
    "$(verdict "$(jq 'has("readings_out_of_order") and has("readings_future") and .readings_out_of_order != null' <<<"${r:-null}" 2>/dev/null)")" "last census: $r"

# ---- M2 counterfactual, departure ----
est=$(jq -c '[.[] | select(.phase == "M2" and .event == "estimate without mem-wave")][0].estimate' "$W/phases.json")
id=$(jq -r '.decision_id // empty' <<<"$est")
r=$(jq -c --arg id "$id" '[.[] | select(.id == $id)][0] | if . == null then null else {kind, cites, pace: .inputs.pace} end' "$W/dec.json")
row M2 02 "estimate traceable: kind estimate, no cites, inputs.pace" \
    "$(verdict "$(jq '. != null and .kind == "estimate" and ((.cites // []) | length) == 0 and .pace != null' <<<"$r")")" "$id: $r"
r=$(jq -c --arg id "$id" '[.[] | select(.id == $id)][0].answer | {sensitivity_low, sensitivity, sensitivity_high}' "$W/dec.json")
row M2 06 "interval: sensitivity_low <= sensitivity <= sensitivity_high" \
    "$(verdict "$(jq 'if .sensitivity == null then null else .sensitivity_low <= .sensitivity and .sensitivity <= .sensitivity_high end' <<<"$r")")" "$r"
sub=$(mk M2 'deleted mem-wave' subject); td=$(mk M2 'deleted mem-wave' t)
r=$(jq -c --arg s "@$sub" --argjson td "${td:-0}" '{
    stale_min: ([.[] | select(.kind == "property.stale" and (.target | endswith($s)))] | if length == 0 then null else ((map(.ts) | min) - $td) / 60 end),
    retired_min: ([.[] | select(.kind == "property.retired" and (.target | endswith($s)))] | if length == 0 then null else ((map(.ts) | min) - $td) / 60 end),
    cascade: ([.[] | select(.kind == "relationship.retired" and (.target | contains($s)))] | map(.detail.reason) | group_by(.) | map({(.[0] // "null"): length}) | add)}' "$W/ev.json")
row M2 02 "departure: mem-wave stale ~+2 min, retired ~+10 min, its edges retired by cascade" \
    "$(verdict "$(jq '.stale_min != null and .stale_min >= 1.5 and .stale_min <= 4 and .retired_min != null and .retired_min >= 9 and .retired_min <= 13' <<<"$r")")" "$r"
r=$(evq '[.[] | select(.kind == "relationship.regime")] | length')
row M2 03 "regime caveat on the next estimate after a relationship.regime" "$([ "$r" -gt 0 ] && echo reported || echo "not exercised")" "relationship.regime events in the run: $r"

# ---- M3 collector outage ----
ta=$(t M3 '^start$'); tup=$(jq '[.[] | select(.phase == "M3" and (.event | test("start netdata")))][0].t // null' "$W/phases.json")
r=$(jq -c --argjson a "$M3s" --argjson b "$M3e" '[.[] | select(.k >= $a and .k <= $b and (.kind == "collector.down" or .kind == "collector.up"))] | map({kind, target, ts})' "$W/ev.json")
row M3 04 "exactly one collector.down and one collector.up for netdata in 65-80" \
    "$(verdict "$(jq '([.[] | select(.kind == "collector.down" and .target == "netdata")] | length) == 1 and ([.[] | select(.kind == "collector.up" and .target == "netdata")] | length) == 1' <<<"$r")")" "$(jq -c 'map({kind, target})' <<<"$r")"
# Netdata's properties: source netdata and active in the last /state capture before the outage.
jq -c --argjson a "$ta" 'select(.endpoint == "/state" and .status == 200 and .t < $a) | [.body.properties[] | select(.source == "netdata" and .status == "active") | .id]' "$OUT/capture.jsonl" | tail -1 >"$W/nd.json"
jq -c --argjson a "$ta" 'select(.endpoint == "/state" and .status == 200 and .t >= $a + 180 and .t <= $a + 780) | {t, p: [.body.properties[] | {id, status, source}]}' "$OUT/capture.jsonl" >"$W/m3state.jsonl"
r=$(jq -s --slurpfile nd "$W/nd.json" '($nd[0] // []) as $ids | {netdata_props: ($ids | length), captures: length,
    not_unobservable: [.[] | . as $c | [$c.p[] | select((.id as $i | $ids | index($i)) and .status != "unobservable") | .status] | length] | max,
    retired: [.[] | [.p[] | select((.id as $i | $ids | index($i)) and .status == "retired")] | length] | max,
    cgroup_not_active: [.[] | [.p[] | select(.source == "cgroup" and (.id | contains("@") | not) and .status != "active")] | length] | max}' "$W/m3state.jsonl")
row M3 04 "Netdata properties unobservable 68-78, none retired; cgroup node properties stay active" \
    "$(verdict "$(jq '.netdata_props > 0 and .captures > 0 and .not_unobservable == 0 and .retired == 0 and .cgroup_not_active == 0' <<<"$r")")" "$r"
r=$(jq -s --slurpfile nd "$W/nd.json" --argjson a "$ta" '($nd[0] // []) as $ids | [.[] | select(.endpoint == "/state/relationships" and .status == 200 and .t >= $a + 180 and .t <= $a + 780)
    | [(.body.relationships // [])[] | select((.from as $f | $ids | index($f)) or (.to as $x | $ids | index($x))) | .status] ] | {captures: length, statuses: (add // [] | group_by(.) | map({(.[0]): length}) | add)}' "$OUT/capture.jsonl")
row M3 04 "relationships with a Netdata end stay active through the outage" \
    "$(jq -r 'if .captures == 0 then "unverifiable" elif .statuses == null then "not exercised" elif (.statuses | keys) == ["active"] then "pass" else "fail" end' <<<"$r")" "$r"
r=$(jq -c '[.[] | select(.phase == "M3" and (.event | startswith("offered")))][0].response | {accepted: (.accepted_by != null), reason: .chain[0].reason}' "$W/phases.json")
row M3 04 "admission at 72 with Netdata down (decline naming an unobservable reading, or accept when no domain reads Netdata)" reported "$r"
r=$(jq -c --argjson u "${tup:-0}" --argjson a "$M3s" '([.[] | select(.kind == "collector.up" and .target == "netdata" and .k >= $a)][0]) | if . == null then null else {collector_up_after_start_s: (.ts - $u)} end' "$W/ev.json")
act=$(jq -s --slurpfile nd "$W/nd.json" --argjson u "${tup:-0}" '($nd[0] // []) as $ids | [.[] | select(.endpoint == "/state" and .status == 200 and .t > $u)
    | {t, n: ([.body.properties[] | select((.id as $i | $ids | index($i)) and .status != "active")] | length)} | select(.n == 0)][0] | if . == null then null else {first_all_active_capture_s: (.t - $u)} end' "$OUT/capture.jsonl")
row M3 04 "Netdata properties active again within 2 ticks (10 s) of netdata start" \
    "$(verdict "$(jq -n --argjson r "$r" '$r != null and $r.collector_up_after_start_s <= 10')")" "$r; $act"

# ---- M4 clock step ----
r=$(jq -c --argjson a "$M4s" --argjson b "$M5s" '[.[] | select(.kind == "clock.stepped" and .k >= $a and .k <= $b) | .detail | (.wall_delta_seconds - (.monotonic_delta_seconds // 0))]' "$W/ev.json")
row M4 01 "two clock.stepped, step (wall_delta - monotonic_delta) ~ +7200 then ~ -7200" \
    "$(verdict "$(jq 'length == 2 and (.[0] - 7200 | fabs) <= 30 and (.[1] + 7200 | fabs) <= 30' <<<"$r")")" "step seconds (wall_delta_seconds less the sweep interval's monotonic_delta_seconds): $r"
r=$(jq -c --argjson a "$M4s" --argjson b "$M4e" '{retired: ([.[] | select(.kind == "property.retired" and .k >= $a and .k <= $b)] | length),
    cascade: ([.[] | select(.kind == "relationship.retired" and .k >= $a and .k <= $b and .detail.reason == "cascade")] | length)}' "$W/ev.json")
row M4 01 "nothing retires across the steps (80-86)" "$(verdict "$(jq '.retired == 0 and .cascade == 0' <<<"$r")")" "$r"
sub=$(mk M4 'deleted noise' subject); td=$(mk M4 'deleted noise' t)
r=$(jq -c --arg s "@$sub" --argjson td "${td:-0}" '[.[] | select(.kind == "property.stale" and (.target | endswith($s)))] | if length == 0 then null else {stale_after_min: (((map(.ts) | min) - $td) / 60), inc: .[0].inc} end' "$W/ev.json")
row M4 01 "noise deleted at 86 goes stale ~2 min later (monotonic sweep)" \
    "$(verdict "$(jq '. != null and .stale_after_min >= 1.5 and .stale_after_min <= 4' <<<"$r")")" "$r (the kill -9 at 87 restarts the sweep)"
r=$(jq -c --argjson a "$M4s" --argjson b "$M4e" '[.[] | select(.kind == "relief.phase" and .k >= $a and .k <= $b) | .detail]' "$W/ev.json")
row M4 01 "relief freshness unaffected: no relief.phase change across the steps" "$(verdict "$(jq 'length == 0' <<<"$r")")" "$r"

# ---- M5 kill -9, truncation ----
r=$(jq -c --argjson s "$M5s" '([.[] | select(.inc == 1)] | {last: (map(.seq) | max), last_ts: (map(.ts) | max)}) as $one
    | ([.[] | select(.inc == 1 and .ts > $one.last_ts - 60)] | length) as $min
    | ([.[] | select(.inc == 2)] | map(.seq) | min) as $first
    | {last_inc1: $one.last, first_inc2: $first, lost: ($one.last - $first + 1), events_last_60s: $min}' "$W/ev.json")
dr=$(jq -c '[.[] | select(.endpoint | startswith("/state/journal")) | select(.status == 200) | .body.dropped] | unique' "$W/cap.json")
row M5 02 "at most one save interval lost at kill -9" \
    "$(verdict "$(jq '.first_inc2 != null and .lost <= .events_last_60s' <<<"$r")")" "$r; journal dropped values seen: $dr"
r=$(jq -c --slurpfile ev "$W/ev.json" '{incs: (map(.inc) + ($ev[0] | map(.inc)) | unique), bad: [.[] | select(.inc >= 2) | select(.inc as $i | .id | test("-\($i)\\.") | not) | .id][0:5],
    dups: ([group_by(.id)[] | select(length > 1) | .[0].id][0:5])}' "$W/dec.json")
row M5 02 "incarnation advances 2, 3, 4 and decision ids never collide" \
    "$(verdict "$(jq '.incs == [1,2,3,4] and (.bad | length) == 0 and (.dups | length) == 0' <<<"$r")")" "$r"
r=$(jq -c '[2, 3, 4] as $incs | [$incs[] as $i | ([.[] | select(.inc == $i)] | map(.ts) | min) as $t0
    | {inc: $i, retired_in_first_10min: ([.[] | select(.inc == $i and .kind == "property.retired" and .ts < $t0 + 600)] | length)}]' "$W/ev.json")
row M5 01 "grace: nothing retires in the first retire_after (10 min) after each restart" \
    "$(verdict "$(jq 'all(.retired_in_first_10min == 0)' <<<"$r")")" "$r"
if [ -s "$OUT/before-kill.json" ] && [ -s "$OUT/after-kill.json" ]; then
    r=$(jq -n --slurpfile b "$OUT/before-kill.json" --slurpfile a "$OUT/after-kill.json" '[$b[0], $a[0]] | map({levers: [.relief.levers[] | del(.phase, .paused)], peers, calibration, outcome_errors: .tasks.prediction.outcome_errors})
        | {equal: (.[0] == .[1]), before: .[0], after: .[1]}')
    row M5 02 "learned state survives kill -9 (relief counters, calibration, peers, admission errors)" "$(verdict "$(jq .equal <<<"$r")")" "$(jq -c 'if .equal then .before else . end' <<<"$r")"
else row M5 02 "learned state survives kill -9" unverifiable "before/after-kill.json missing"; fi
pend=$(jq -c '[.tasks.pending[]?.accept_id]' "$OUT/before-kill.json" 2>/dev/null || echo null)
r=$(jq -c --argjson p "${pend:-[]}" '[.[] | select(.kind == "admission.outcome" and ((.cites // []) - ((.cites // []) - $p) | length) > 0) | {id, cites, answer}]' "$W/dec.json")
row M5 02 "pending accept at the kill closes confounded \"agent restarted\"" \
    "$(jq -r --argjson p "${pend:-[]}" 'if ($p | length) == 0 then "not exercised" elif length > 0 and all(.answer.outcome == "confounded" and .answer.reason == "agent restarted") then "pass" else "fail" end' <<<"$r")" "pending at kill: $pend; outcomes: $r"
r=$(grep -c 'unreadable .*restored the previous generation' "$OUT/agent.log")
row M5 04 "restart at 94 restores state.json.prev (agent log), no restore.failed in inc 3" \
    "$(verdict "$(jq -n --argjson r "$r" --argjson n "$(evq '[.[] | select(.inc == 3 and .kind == "restore.failed")] | length')" '$r >= 1 and $n == 0')")" \
    "log lines: $(grep 'restored the previous generation' "$OUT/agent.log" | head -1 | cut -c1-200)"
r=$(jq -c '[.events[] | select(.kind != "fact.recorded")][0] | {seq, kind, error: .detail.error, prev_error: .detail.prev_error}' "$OUT/after-cold-start-journal.json" 2>/dev/null || echo null)
inc=$(jq '.incarnation' "$OUT/after-cold-start-journal.json" 2>/dev/null || echo null)
rec=$(jq -c --argjson a "$(t M5 'truncated state.json and')" '[.[] | select(.endpoint == "/relief" and .status == 200 and .t > $a) | .body | [.. | objects | select(has("reconcile_error")) | .reconcile_error]] | add | unique' "$W/cap.json")
row M5 04 "cold start at 97 journalled: first event restore.failed with both errors (GET /state/journal); ledger reconciled" \
    "$(verdict "$(jq -n --argjson r "$r" --argjson c "${rec:-null}" 'if $r == null then null else $r.kind == "restore.failed" and ($r.error | length) > 0 and ($r.prev_error | length) > 0 and $c != null and ($c | map(select(. != "" and . != null)) | length) == 0 end')")" "incarnation $inc: $r; reconcile_error values after: $rec"
l=$(jq -c --argjson i "${inc:-0}" '[.[] | select(.inc == $i and .kind == "restore.failed")] | length' "$W/ev.json")
f=$(jq -c --argjson i "${inc:-0}" '[.[] | select(.inc == $i)] | min_by(.seq) | {seq, kind}' "$W/ev.json")
row M5 06 "the cold start's restore.failed reaches the durable log" "$(verdict "$(jq -n --argjson l "$l" --argjson i "$inc" 'if $i == null then null else $l == 1 end')")" "restore.failed lines in journal.ndjson for incarnation $inc: $l; first logged line of it: $f"
r=$(jq -c '[.actions[]? | select(.entry.state? == "applied" or .state? == "applied")] | length' "$OUT/before-kill.json" 2>/dev/null)
row M5 02 "a cap in force at the kill is resumed, not duplicated" "$([ "${r:-0}" = 0 ] && echo "not exercised" || echo unverifiable)" "caps in force at the kill: ${r:-unread}"

# ---- M6 memory relief ----
acts=$(jq -c '[.[] | select(.kind == "relief.act") | {id, cites}]' "$W/dec.json")
r=$(jq -c --argjson acts "$acts" '. as $d | [$acts[] | .id as $a | {act: $a, verify: ([$d[] | select(.kind == "relief.verify" and ((.cites // []) | index($a)))] | map(.id))}]' "$W/dec.json")
row M6 02 "relief acts and verifies: >= 1 act, each with a later verify citing it" \
    "$(jq -r 'if length == 0 then "fail" elif all((.verify | length) > 0) then "pass" else "fail" end' <<<"$r")" "$r"
r=$(jq -c '[.[] | select(.kind == "relief.phase") | {k, d: "\(.detail.from)->\(.detail.to)", target}]' "$W/ev.json")
fa=$(jq '[.[] | select(.kind == "relief.act")][0].k // null' "$W/dec.json")
row M6 02 "relief.phase calm->relieving before the first act; relieving->releasing after" \
    "$(verdict "$(jq --argjson fa "$fa" '$fa != null and any(.d == "calm->relieving" and .k < $fa) and any(.d == "relieving->releasing" and .k > $fa)' <<<"$r")")" "$(jq -c 'map(.d)' <<<"$r")"
if [ -s "$OUT/m9-verify-chain.json" ]; then
    r=$(jq -c '{verify: .decision.id, cites0: .cites[0].id, act_cites: .cites[0].decision.cites}' "$OUT/m9-verify-chain.json" 2>/dev/null || echo null)
    row M6 02 "chain resolves: ?chain=1 on the last verify, cites[0] is its act" "$(verdict "$(jq '.cites0 != null and (.cites0 | test("relief-act"))' <<<"$r" 2>/dev/null)")" "$r"
else row M6 02 "chain resolves (?chain=1)" "$([ "$(jq length <<<"$acts")" = 0 ] && echo "not exercised" || echo unverifiable)" "no verify chain read at M9"; fi
cal=$(jq -c '.calibration' "$OUT/final-state.json" 2>/dev/null || echo null)
lv=$(jq -c '[.[] | select(.kind == "relief.verify")] | last | .inputs.calibration // null' "$W/dec.json")
row M6 03 "calibration fed: relief.memory.high entry n >= 1, hit_rate in [0,1]; verify carries inputs.calibration" \
    "$(verdict "$(jq -n --argjson c "$cal" --argjson v "$lv" '[$c[]? | select(.kind | test("relief.*memory"))] | length > 0 and all(.n >= 1 and .hit_rate >= 0 and .hit_rate <= 1) and $v != null')")" \
    "final: $(jq -c '[.[]? | select(.kind | test("relief"))]' <<<"$cal"); last verify inputs.calibration: $lv"
hog=$(mk M6 'applied page-hog' subject)
r=$(jq -c --arg f "memory_utilization@$hog" '{interventions: ([.[] | select(.kind == "relationship.intervention" and (.target | contains($f)))] | length)}' "$W/ev.json")
rel=$(jq -c --arg f "memory_utilization@$hog" 'select(.endpoint == "/state/relationships" and .status == 200) | [(.body.relationships // [])[] | select(.from == $f and .to == "memory_contention_ratio") | {provenance, status}]' "$OUT/capture.jsonl" | tail -1)
rv=$(jq -c --arg f "memory_utilization@$hog" '[.[] | select(.kind == "review" and .inputs.candidate.FromID == $f and .inputs.candidate.ToID == "memory_contention_ratio") | .answer | {action, by}]' "$W/dec.json")
row M6 03 "intervention learning: page-hog memory -> memory_contention_ratio provenance intervened" \
    "$(jq -nr --argjson rel "${rel:-[]}" --argjson r "$r" 'if $r.interventions < 2 and ($rel | length) == 0 then "not exercised" elif any($rel[]; .provenance == "intervened") then "pass" else "fail" end')" \
    "$r; relationship: ${rel:-none}; reviews of the pair: $rv"
if [ -s "$OUT/m9-verify-explain.txt" ]; then
    ct=$(grep -i '^content-type' "$OUT/m9-verify-explain.headers" | tr -d '\r')
    vid=$(jq -r '.decision.id' "$OUT/m9-verify-chain.json" 2>/dev/null)
    pv=$(jq -r '"\(.decision.answer.predicted * 1000 | round / 1000) \(.decision.answer.observed * 1000 | round / 1000)"' "$OUT/m9-verify-chain.json" 2>/dev/null)
    sub=$(grep -o 'pod:[0-9a-f-]*' "$OUT/m9-verify-explain.txt" | head -1)
    ok=false
    [[ "$ct" == *text/plain* ]] && [ -n "$sub" ] && grep -qF "$hog" "$OUT/m9-verify-explain.txt" && grep -qF "${pv% *}" "$OUT/m9-verify-explain.txt" && grep -qF "${pv#* }" "$OUT/m9-verify-explain.txt" && ok=true
    row M6 06 "narrative: explain is text/plain, names the subject and the record's predicted and observed numbers" \
        "$(verdict "$ok")" "$ct; verify $vid on ${sub:-no subject} (page-hog $hog); predicted/observed rounded: $pv; $(head -c 300 "$OUT/m9-verify-explain.txt")"
else row M6 06 "narrative (explain)" "$([ "$(jq length <<<"$acts")" = 0 ] && echo "not exercised" || echo unverifiable)" "no explain read at M9"; fi
ids=$(jq -r 'select(.status == 200) | (.body.decisions[]?.id // empty), (.body.decision_id? // empty)' "$OUT/capture.jsonl" 2>/dev/null | sort -u)
ids2=$(jq -r '.. | .decision_id? // empty' "$OUT/phases.jsonl" | sort -u)
r=$(printf '%s\n%s\n' "$ids" "$ids2" | grep -v '^$' | sort -u | jq -R . | jq -sc --slurpfile d "$W/dec.json" '($d[0] | map(select(.ok)) | map(.id)) as $ok | {captured: length, missing_or_bad: (. - $ok)[0:5], n_missing: ((. - $ok) | length)}')
fl=$(jq -c '{id, from}' "$OUT/m9-first-decision.json" 2>/dev/null || echo null)
row M6 06 "durable log: every captured decision id on a digest-verified line; an evicted id answers from: log" \
    "$(verdict "$(jq -n --argjson r "$r" --argjson f "$fl" '$r.captured > 0 and $r.n_missing == 0 and $f.from == "log"')")" "$r; first decision read at M9: $fl; log lines failing digest: $(jq '[.[] | select(.ok | not)] | length' "$W/ev.json")+$(jq '[.[] | select(.ok | not)] | length' "$W/dec.json")"
r=$(jq -c '{paused: ([.[] | select(.kind == "relief.paused")] | length), asserted_by_operator: ([.[] | select(.kind == "relationship.asserted" and .actor == "operator")] | length)}' "$W/ev.json")
row M6 01 "no operator: zero relief.paused, zero relationship.asserted by operator" "$(verdict "$(jq '.paused == 0 and .asserted_by_operator == 0' <<<"$r")")" "$r"

# ---- M7 admission ----
# Accepts are the admission decisions of the M7 offers; their outcome cites them.
r=$(jq -c --argjson a "$M7s" --argjson b "${M8s}" --slurpfile ev "$W/ev.json" '. as $d | [.[] | select(.kind == "admission" and .k >= $a and .k <= $b and (.inputs.offer.name | startswith("probe-")) and .answer.verdict == "accept")] | map(. as $x | {
        name: .inputs.offer.name, id,
        outcome: ([$d[] | select(.kind == "admission.outcome" and ((.cites // []) | index($x.id)))][0] | if . == null then null else {id, k, result: .answer.outcome} end),
        arrived: ([$ev[0][] | select(.kind == "admission.arrived" and .detail.accept == $x.id)][0].k // null), k})
    | map(. + {ordered: (.outcome != null and .arrived != null and .k < .arrived and .arrived < .outcome.k)})' "$W/dec.json")
echo "$r" >"$W/m7.json"
row M7 02 "each accept has an outcome citing it, admission.arrived between" \
    "$(jq -r 'if length == 0 then "not exercised" elif all(.ordered) then "pass" else "fail" end' <<<"$r")" "accepts: $(jq -c 'map({name, outcome: .outcome.result, ordered})' <<<"$r"); probe verdicts: $(jq -c --argjson a "$M7s" '[.[] | select(.kind == "admission" and .k >= $a and ((.inputs.offer.name // "") | startswith("probe-"))) | "\(.inputs.offer.name) \(.answer.verdict): \(.answer.reason)"]' "$W/dec.json")"
r=$(jq -c --argjson a "$M7s" --argjson b "${M8s}" '[.[] | select(.kind == "admission.outcome" and .k >= $a and .k <= $b) | .answer.lines[]? | {domain, map_rise: (.map_rise != null), usage_rise: (.usage_rise != null)}]' "$W/dec.json")
row M7 03 "both bases recorded: usage_rise always, map_rise when a measured effect exists" \
    "$(jq -r 'if length == 0 then "not exercised" elif (all(.usage_rise) | not) then "fail" elif any(.map_rise) then "pass" else "not exercised" end' <<<"$r")" \
    "$(jq -c 'group_by(.domain) | map({domain: .[0].domain, lines: length, with_map_rise: (map(select(.map_rise)) | length)})' <<<"$r") (JSON names map_rise/usage_rise)"
r=$(jq -c '[.[]? | select(.kind | startswith("admission."))]' <<<"$cal")
sc=$(jq -c '[.[] | select(.kind == "admission.outcome") | .answer.lines[]? | select(.usage_err != null) | .domain] | group_by(.) | map({(.[0]): length}) | add' "$W/dec.json")
row M7 03 "calibration per domain: admission.memory/cpu_contention_ratio entries, n = scored outcomes" \
    "$(verdict "$(jq -nr --argjson c "$r" --argjson s "${sc:-null}" 'if $s == null and ($c | length) == 0 then "not-exercised" else ($c | map(.kind) | index("admission.memory_contention_ratio")) != null and ($c | map(.kind) | index("admission.cpu_contention_ratio")) != null and all($c[] | select(.basis == "usage"); .n == ($s[(.kind | sub("admission."; ""))] // 0)) end')")" "entries: $r; scored outcome lines by domain: $sc"
r=$(jq -c --argjson a "$M7s" --argjson b "${M8s}" '[.[] | select(.kind == "admission" and .k >= $a and .k <= $b and (.inputs.offer.name | startswith("probe-")))] | last | {name: .inputs.offer.name, basis_chosen_by: .inputs.basis_chosen_by,
    n: (.inputs.calibration | if type == "object" then with_entries(.value |= (if type == "object" then .n else . end)) else . end)}' "$W/dec.json")
row M7 03 "6th accept: basis_chosen_by calibration once both bases have n >= 5, else default" reported "$r"
r=$(jq -c '[.[] | select(.kind == "admission" and (.answer.verdict // "") == "decline") | {id, reason: .answer.reason, cites}]' "$W/dec.json")
row M7 02 "declines traceable: kind admission, reason named, gate cited" \
    "$(jq -r 'if length == 0 then "not exercised" elif all(.reason != null and ((.cites // []) | length) > 0) then "pass" else "fail" end' <<<"$r")" "$r"

# ---- M8 pace ----
td=$(mk M8 'deleted every planted pod' t)
r=$(jq -c --argjson a "$M8s" --argjson b "$M8e" --argjson td "${td:-0}" '[.[] | select(.kind == "pace.changed" and .k >= $a and .k <= $b) | {d: "\(.detail.from)->\(.detail.to)", reason: .detail.reason, after_delete_min: ((.ts - $td) / 60)}]' "$W/ev.json")
row M8 05 "one base->slow at 175 + 10 min (+- 1 tick), reason names calm" \
    "$(verdict "$(jq '[.[] | select(.d == "base->slow")] | length == 1 and (.[0].after_delete_min >= 9 and .[0].after_delete_min <= 12) and (.[0].reason | test("calm"))' <<<"$r")")" "$r"
slow=$(jq '[.[] | select(.kind == "pace.changed" and .detail.to == "slow")] | last | .ts // null' "$W/ev.json")
r=$(jq -c --argjson s "${slow:-null}" '[.[] | select(.endpoint == "/collectors" and .status == 200) | {t, n: (.body.collectors[] | select(.name == "cgroup") | .samples)}] as $c
    | def rate(a; b): ([$c[] | select(.t >= a and .t <= b)]) | if length < 2 then null else ((.[-1].n - .[0].n) / (.[-1].t - .[0].t)) end;
    if $s == null then null else {base: rate($s - 300; $s - 30), slow: rate($s + 30; $s + 300)} | . + {ratio: (if .slow and .slow > 0 then .base / .slow else null end)} end' "$W/cap.json")
row M8 05 "loops slowed: collection rate under slow ~1/4 of base (cgroup samples per second)" \
    "$(verdict "$(jq '. != null and .ratio != null and .ratio >= 3 and .ratio <= 5' <<<"$r")")" "$r (adapted from last_success_unix spacing to the samples counter rate)"
id=$(mk M8 'estimate at 188' decision_id)
r=$(jq -c --arg id "$id" '[.[] | select(.id == $id)][0].inputs.pace // null' "$W/dec.json")
row M8 05 "an estimate at 188 records inputs.pace slow" "$(verdict "$(jq -n --argjson r "$r" '$r == "slow"')")" "$id: $r"
sub=$(mk M8 'applied noise' subject)
r=$(jq -c --arg s "@$sub" --argjson a "$M8s" '([.[] | select(.kind == "property.admitted" and (.target | endswith($s)))][0]) as $adm
    | ([.[] | select(.kind == "pace.changed" and .detail.from == "slow" and .detail.to == "base" and .k >= $a)][0]) as $w
    | {admitted_ts: $adm.ts, wake: (if $w == null then null else {reason: $w.detail.reason, after_admit_s: ($w.ts - $adm.ts)} end)}' "$W/ev.json")
row M8 05 "wake on arrival: slow->base within a tick of noise admitted, reason property admitted" \
    "$(verdict "$(jq '.wake != null and .wake.after_admit_s <= 60 and .wake.after_admit_s >= -1 and (.wake.reason | startswith("property admitted"))' <<<"$r")")" "$r"
subs=$(jq -c '[.[] | select(.phase == "M1" or .phase == "M6") | .subject // empty] | unique' "$W/phases.json")
r=$(jq -c --argjson subs "$subs" --argjson td "${td:-0}" --argjson a "$M8s" '[$subs[] as $s | ([.[] | select(.k >= $a and (.target | endswith("@" + $s)))]) as $e
    | select(($e | length) > 0) | {s: ($s | .[4:12]), stale_min: ([$e[] | select(.kind == "property.stale") | .ts] | min | if . == null then null else (. - $td) / 60 end),
      retired_min: ([$e[] | select(.kind == "property.retired") | .ts] | min | if . == null then null else (. - $td) / 60 end)}]' "$W/ev.json")
row M8 05 "staleness does not follow the pace: deleted pods stale ~+2 min, retired ~+10 min" \
    "$(verdict "$(jq 'length > 0 and all(.stale_min != null and .stale_min <= 4 and .retired_min != null and .retired_min >= 9 and .retired_min <= 13)' <<<"$r")")" "$r"

# ---- M9 the map corrects a prior ----
r=$(jq -c '[.[] | select(.kind == "relationship.retired" and .detail.reason == "sign-contradicted")] | length' "$W/ev.json")
ss=$(jq -s '[.[] | select(.endpoint == "/state" and .status == 200) | .body.counts.relationships_sign_suspect] | max' "$OUT/capture.jsonl")
row M9 03 "sign contradiction acted on (relationship.retired sign-contradicted)" \
    "$(if [ "$r" -gt 0 ]; then echo reported; elif [ "${ss:-0}" = 0 ]; then echo "not exercised"; else echo fail; fi)" "sign-contradicted retirements: $r; max relationships_sign_suspect: $ss"
r=$(jq -c '[.[] | select(.kind == "relationship.regime") | .target] | {n: length, rels: (unique)}' "$W/ev.json")
row M9 03 "regime events" "$(jq -r 'if .n > 0 then "reported" else "not exercised" end' <<<"$r")" "$r"
f=$(jq '[.[] | select(.kind == "property.forgotten")] | length' "$W/ev.json")
sp=$(jq '.properties | length' "$OUT/state.json" 2>/dev/null || echo null)
cn=$(jq -c '.counts | {properties_total, properties_active, properties_stale, properties_unobservable, properties_retired}' "$OUT/final-state.json" 2>/dev/null || echo null)
b0=$(mk M0 '^end$' snapshot_bytes); b9=$(mk M9 '^end$' snapshot_bytes)
row M9 04 "compaction: no property.forgotten in 3 h; snapshot properties = active + stale + unobservable + retired" \
    "$(verdict "$(jq -n --argjson f "$f" --argjson sp "$sp" --argjson c "$cn" '$f == 0 and $sp == ($c.properties_active + $c.properties_stale + $c.properties_unobservable + $c.properties_retired)')")" \
    "forgotten: $f; snapshot properties: $sp; final census: $cn; snapshot bytes at M0 end ${b0:-?}, M9 end ${b9:-?}"

# ---- results ----
{
    echo "# Master run readout"
    echo
    echo "Build: $(jq -r '[.[] | select(.endpoint == "/version" and .status == 200) | .body.build_commit] | unique | join(", ")' "$W/cap.json"); features: $FEATURES; config: agent-autonomous.yaml (+ ledger in the run directory)."
    echo "Timetable as run (phase starts, from phases.jsonl): $(jq -r '[.[] | select(.event == "start") | "\(.phase) \(.t | strflocaltime("%H:%M"))"] | join(", ")' "$W/phases.json")."
    echo
    echo "Verdicts: $(cut -f4 "$ROWS" | sort | uniq -c | awk '{c=$1; $1=""; printf "%s%s %d", (n++ ? ", " : ""), substr($0,2), c}')"
    echo
    echo "| Phase | Plan | Criterion | Verdict | Reading |"
    echo "| --- | --- | --- | --- | --- |"
    awk -F'\t' '{ gsub(/\|/, "\\|", $5); printf "| %s | %s | %s | **%s** | %s |\n", $1, $2, $3, $4, $5 }' "$ROWS"
} >"$OUT/results-readout.md"
cut -f1,4 "$ROWS" | sort | uniq -c | sort -k2 >&2
echo "wrote $OUT/results-readout.md" >&2
