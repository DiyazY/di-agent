#!/bin/bash
set -e

# Read environment variables with defaults
NODE_ID=${NODE_ID:-"node1"}
REGIME=${REGIME:-"bursty"}
PORT=${PORT:-"9090"}
PROFILE=${PROFILE:-"edge-minimal"}
KD=${KD:-"k8s"}
COLLECT_INTERVAL=${COLLECT_INTERVAL:-"5s"}
PROPOSER=${PROPOSER:-"false"}
NETDATA_URL=${NETDATA_URL:-"http://localhost:19999"}
DOMAIN_SPEC=${DOMAIN_SPEC:-"/etc/di-agent/domain_spec.json"}

echo "Starting di-agent with:"
echo "  NODE_ID=${NODE_ID}"
echo "  REGIME=${REGIME}"
echo "  PORT=${PORT}"
echo "  PROFILE=${PROFILE}"
echo "  KD=${KD}"
echo "  COLLECT_INTERVAL=${COLLECT_INTERVAL}"
echo "  PROPOSER=${PROPOSER}"
echo "  NETDATA_URL=${NETDATA_URL}"
echo "  DOMAIN_SPEC=${DOMAIN_SPEC}"

# Execute with ALL required flags
exec /usr/local/bin/di-agent \
  -profile "${PROFILE}" \
  -addr ":${PORT}" \
  -domain "${DOMAIN_SPEC}" \
  -netdata-url "${NETDATA_URL}" \
  -kd "${KD}" \
  -node-id "${NODE_ID}" \
  -regime "${REGIME}" \
  -collect-interval "${COLLECT_INTERVAL}" \
  -proposer="${PROPOSER}" \
  "$@"