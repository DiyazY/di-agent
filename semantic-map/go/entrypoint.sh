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
REVIEW_LEVEL=${REVIEW_LEVEL:-"full"}
REVIEW_RULES=${REVIEW_RULES-"composition"}
DECISION_BACKEND=${DECISION_BACKEND:-"systemone"}
REASONING_BACKEND=${REASONING_BACKEND:-"openai-tools"}
# Unset takes the default; set to empty runs without Netdata.
NETDATA_URL=${NETDATA_URL-"http://localhost:19999"}
DOMAIN_SPEC=${DOMAIN_SPEC:-"/etc/di-agent/domain_spec.json"}
DATASET_COLLECTOR_URL=${DATASET_COLLECTOR_URL:-}
if [ "${DATASET_NODE_NAME+x}" = x ] && [ "${DATASET_NODE_NAME}" != "${NODE_ID}" ]; then
    DATASET_COLLECTOR_URL=""
fi
SIMULATION_COLLECTOR_URL=${SIMULATION_COLLECTOR_URL:-}
SIMULATION_INCLUDE=${SIMULATION_INCLUDE:-}
if [ "${SIMULATION_NODE_NAME+x}" = x ] && [ "${SIMULATION_NODE_NAME}" != "${NODE_ID}" ]; then
    SIMULATION_COLLECTOR_URL=""
fi
CGROUP_ROOT=${CGROUP_ROOT:-"/sys/fs/cgroup"}
# true serves the web console at /ui/; the agent is headless by default. Only
# true or false: the value is written into the spec as a YAML bool.
UI=${UI:-"false"}
case "${UI}" in
    true|false) ;;
    *) echo "UI must be true or false, got '${UI}'" >&2; exit 1 ;;
esac

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
echo "  SIMULATION_COLLECTOR_URL=${SIMULATION_COLLECTOR_URL}"
echo "  CGROUP_ROOT=${CGROUP_ROOT}"
echo "  UI=${UI}"

# The agent is described by a spec file; write this container's from the variables.
SPEC_FILE=${SPEC_FILE:-"/tmp/agent.yaml"}
cat > "${SPEC_FILE}" <<SPEC
node:
  addr: ":${PORT}"
  id: "${NODE_ID}"
  ui: ${UI}
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
if [ -n "${SIMULATION_COLLECTOR_URL}" ]; then
cat >> "${SPEC_FILE}" <<SPEC
    - name: simulation
      http:
        url: "${SIMULATION_COLLECTOR_URL}"
SPEC
    if [ -n "${SIMULATION_INCLUDE}" ]; then
        printf '      config:\n        include:\n' >> "${SPEC_FILE}"
        IFS=',' read -r -a patterns <<< "${SIMULATION_INCLUDE}"
        for pattern in "${patterns[@]}"; do
            if [[ ! "${pattern}" =~ ^[A-Za-z0-9._*?-]+$ ]]; then
                echo "Invalid SIMULATION_INCLUDE pattern: ${pattern}" >&2
                exit 1
            fi
            printf '          - "%s"\n' "${pattern}" >> "${SPEC_FILE}"
        done
    fi
fi
cat >> "${SPEC_FILE}" <<SPEC
map:
  proposer:
    enabled: ${PROPOSER}
loops:
  review:
    level: "${REVIEW_LEVEL}"
    rules:
SPEC
if [ -n "${REVIEW_RULES}" ]; then
    IFS=',' read -r -a rules <<< "${REVIEW_RULES}"
    for rule in "${rules[@]}"; do
        if [[ ! "${rule}" =~ ^[a-z][a-z0-9-]*$ ]]; then
            echo "Invalid REVIEW_RULES entry: ${rule}" >&2
            exit 1
        fi
        printf '      - %s\n' "${rule}" >> "${SPEC_FILE}"
    done
else
    printf '      []\n' >> "${SPEC_FILE}"
fi
cat >> "${SPEC_FILE}" <<SPEC
    decision:
      backend: "${DECISION_BACKEND}"
    reasoning:
      backend: "${REASONING_BACKEND}"
SPEC

exec /usr/local/bin/di-agent -config "${SPEC_FILE}" "$@"
