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
# Unset takes the default; set to empty runs without Netdata.
NETDATA_URL=${NETDATA_URL-"http://localhost:19999"}
DOMAIN_SPEC=${DOMAIN_SPEC:-"/etc/di-agent/domain_spec.json"}
DATASET_COLLECTOR_URL=${DATASET_COLLECTOR_URL:-}
CGROUP_ROOT=${CGROUP_ROOT:-"/sys/fs/cgroup"}

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
echo "  DATASET_COLLECTOR_URL=${DATASET_COLLECTOR_URL}"
echo "  CGROUP_ROOT=${CGROUP_ROOT}"

# The agent is described by a spec file; write this container's from the variables.
SPEC_FILE=${SPEC_FILE:-"/tmp/agent.yaml"}
cat > "${SPEC_FILE}" <<SPEC
node:
  addr: ":${PORT}"
  id: "${NODE_ID}"
profile:
  name: "${PROFILE}"
  kd: "${KD}"
  regime: "${REGIME}"
domain:
  path: "${DOMAIN_SPEC}"
sources:
  interval: ${COLLECT_INTERVAL}
  collectors:
    - name: cgroup
      type: cgroup
      config:
        root: "${CGROUP_ROOT}"
SPEC
# Netdata after cgroup, which supplies the node's own types; collapse: false keeps
# Netdata's system charts from feeding cpu_utilization a second time. An empty
# NETDATA_URL runs cgroup alone.
if [ -n "${NETDATA_URL}" ]; then
cat >> "${SPEC_FILE}" <<SPEC
    - name: netdata
      type: netdata
      config:
        url: "${NETDATA_URL}"
        collapse: false
SPEC
fi
if [ -n "${DATASET_COLLECTOR_URL}" ]; then
cat >> "${SPEC_FILE}" <<SPEC
    - name: dataset
      http:
        url: "${DATASET_COLLECTOR_URL}"
SPEC
fi
cat >> "${SPEC_FILE}" <<SPEC
map:
  proposer:
    enabled: ${PROPOSER}
SPEC

exec /usr/local/bin/di-agent -config "${SPEC_FILE}" "$@"
