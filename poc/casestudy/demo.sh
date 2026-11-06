#!/usr/bin/env bash
# demo.sh — a 48-minute compressed run to watch the map evolve: fresh map, planted
# workloads arrive and the review loop (level full, decision model on the node's Ollama)
# settles what discovery proposes; the planted pods leave, a hog runs beside a victim
# (the kernel gives each its CPU share, so CPU relief has nothing to do), everything
# leaves and retires.
set -uo pipefail
cd "$(dirname "$0")"
OUT="$(realpath -m "${1:?usage: $0 <out-dir>}")"; mkdir -p "$OUT"
K=(k0s kubectl); API=http://localhost:9090; T0=$(date +%s)
mark() { jq -cn --argjson t "$(date +%s.%N)" --arg e "$1" '{t:$t, phase:"demo", event:$e}' >>"$OUT/phases.jsonl"; echo "[$(date +%T)] $1"; }
at() { local w=$((T0 + $1*60 - $(date +%s))); [ $w -gt 0 ] && sleep $w; return 0; }
apply() { "${K[@]}" apply -f "workloads/$1.yaml" >/dev/null && mark "applied $1"; }
pkill -TERM -x agent; for _ in $(seq 60); do pgrep -x agent >/dev/null || break; sleep 1; done
"${K[@]}" delete pod cycler mem-wave mem-squeeze io-wave twin-a twin-b noise hog victim --ignore-not-found --wait=true >/dev/null
[ -e /var/lib/di-agent/state.json ] && mv /var/lib/di-agent/state.json "/var/lib/di-agent/state.json.pre-demo-$(date +%H%M%S)"
[ -e agent.new ] && mv agent.new agent
systemctl reset-failed di-agent.service 2>/dev/null || true
systemd-run --unit=di-agent --collect --quiet -p AllowedCPUs=0-5 -p WorkingDirectory="$PWD" \
    -p StandardOutput="append:$OUT/agent.log" -p StandardError="append:$OUT/agent.log" "$PWD/agent" -config "$PWD/agent-demo.yaml"
for _ in $(seq 60); do curl -sf $API/healthz >/dev/null && break; sleep 1; done
mark "agent started, fresh map, relief on, review full ($(curl -s $API/version | jq -r .build_commit))"
setsid nohup ./capture.sh "$OUT" 30 </dev/null >"$OUT/capture.err" 2>&1 &
echo $! >"$OUT/capture.pid"
at 2;  for p in cycler mem-wave mem-squeeze io-wave twin-a twin-b noise; do apply $p; sleep 30; done
at 12; "${K[@]}" delete pod cycler mem-wave mem-squeeze io-wave twin-a twin-b noise --wait=true >/dev/null; mark "deleted the planted pods"
at 14; apply hog; apply victim
at 36; "${K[@]}" delete pod hog victim --ignore-not-found --wait=true >/dev/null; mark "deleted hog and victim"
at 48; mark "demo ends"; kill "$(cat "$OUT/capture.pid")"; echo done
