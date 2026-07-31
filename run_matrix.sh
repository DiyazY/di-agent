#!/usr/bin/env bash
# Drives the full convergence matrix: every KD × workload, 5 runs each.
# Each cell gets its own port so a stale daemon from a failed cell cannot be
# mistaken for a fresh one, and its own log for post-hoc auditing.
#
# Pass --relational to measure the paired-endpoint updater; results land with a
# _relational suffix so the two modes never overwrite each other.
set -u
MODE_ARGS=""
SUFFIX=""
BASE_PORT=18300
if [ "${1:-}" = "--relational" ]; then
  MODE_ARGS="--relational"
  SUFFIX="_relational"
  BASE_PORT=18400
fi

KDS=(k0s k3s k8s kubeEdge openYurt)
TESTS=(idle cp_light_1client cp_heavy_8client cp_heavy_12client dp_redis_density \
       reliability-control reliability-worker \
       reliability-control-no-pressure-long reliability-worker-no-pressure-long)
port=$BASE_PORT
for kd in "${KDS[@]}"; do
  for test in "${TESTS[@]}"; do
    port=$((port + 1))
    log="log_rq3_${kd}_${test}${SUFFIX}.txt"
    echo "=== ${kd} / ${test}${SUFFIX} (port ${port}) ==="
    python3 measure.py --kd "$kd" --test "$test" --runs 5 --port "$port" \
      --daemon-bin ./agent_bin $MODE_ARGS > "$log" 2>&1
    tail -2 "$log" | sed 's/^/    /'
  done
done
