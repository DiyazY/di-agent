#!/usr/bin/env bash
# verify.sh — P3 to P5 of the timetable on a fresh map, nothing before: hog beside
# victim, where cpu.limit must stay calm (README, P4). agent.new is the build under
# test. Runs on the node, as root, from this directory.
#
# Usage: sudo ./verify.sh <out-dir>
set -uo pipefail
cd "$(dirname "$0")"
OUT="$(realpath -m "${1:?usage: $0 <out-dir>}")"
mkdir -p "$OUT"
pkill -TERM -x agent; for _ in $(seq 60); do pgrep -x agent >/dev/null || break; sleep 1; done
# A fresh map: the snapshot is moved aside, not deleted. P3's mark then records
# from_snapshot false.
[ -e /var/lib/di-agent/state.json ] && mv /var/lib/di-agent/state.json "/var/lib/di-agent/state.json.pre-verify-$(date +%H%M%S)"
[ -e agent.new ] && mv agent.new agent
k0s kubectl delete pod hog victim mem-wave --ignore-not-found --wait=true >/dev/null
setsid nohup ./capture.sh "$OUT" 30 </dev/null >"$OUT/capture.err" 2>&1 &
echo $! >"$OUT/capture.pid"
./schedule.sh "$OUT" --phase P3
./schedule.sh "$OUT" --phase P4
sleep 1020
./schedule.sh "$OUT" --phase P5
sleep 240
kill "$(cat "$OUT/capture.pid")"
echo done
