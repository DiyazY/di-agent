#!/usr/bin/env bash
# Register the Helm-managed worker agents with one another, with initial trust 0.8.
# Usage: HELM_NAMESPACE=default HELM_RELEASE=di-agent-system ./04-peers.sh

set -euo pipefail

NAMESPACE="${HELM_NAMESPACE:-default}"
RELEASE="${HELM_RELEASE:-di-agent-system}"
SELECTOR="app=di-agent,app.kubernetes.io/instance=${RELEASE}"

for tool in kubectl python3; do
    command -v "$tool" >/dev/null || { echo "[peers] Missing $tool" >&2; exit 1; }
done
if [ "$#" -ne 0 ]; then
    echo "[peers] VM arguments are no longer supported; use HELM_NAMESPACE and HELM_RELEASE." >&2
    exit 1
fi

kubectl -n "$NAMESPACE" rollout status daemonset/di-agent --timeout=180s
work_dir=$(mktemp -d)
trap 'rm -f "$work_dir/pods.json" "$work_dir/agents.tsv"; rmdir "$work_dir"' EXIT
kubectl -n "$NAMESPACE" get pods -l "$SELECTOR" -o json > "$work_dir/pods.json"
python3 - "$work_dir/pods.json" > "$work_dir/agents.tsv" <<'PY'
import json
import sys

with open(sys.argv[1]) as stream:
    pods = json.load(stream)["items"]
agents = {}
dataset_nodes = set()
simulation_nodes = set()
for pod in pods:
    if pod["metadata"].get("deletionTimestamp"):
        continue
    node = pod["spec"].get("nodeName")
    ready = any(c["type"] == "Ready" and c["status"] == "True"
                for c in pod.get("status", {}).get("conditions", []))
    if not node or not ready or not pod.get("status", {}).get("hostIP"):
        sys.exit("[peers] Every agent must be scheduled and Ready before registration.")
    if node in agents:
        sys.exit(f"[peers] Multiple agents on {node}; remove legacy deployments first.")
    container = next(c for c in pod["spec"]["containers"] if c["name"] == "di-agent")
    dataset_nodes.update(e["value"] for e in container.get("env", [])
                         if e["name"] == "DATASET_NODE_NAME" and e.get("value"))
    simulation_nodes.update(e["value"] for e in container.get("env", [])
                            if e["name"] == "SIMULATION_NODE_NAME" and e.get("value"))
    port = next(p["containerPort"] for p in container["ports"] if p["name"] == "http")
    host = pod["status"]["hostIP"]
    if ":" in host:
        host = f"[{host}]"
    agents[node] = (pod["metadata"]["name"], port, f"http://{host}:{port}")
if not agents:
    sys.exit("[peers] No Helm-managed worker agents found.")
if len(dataset_nodes) > 1 or not dataset_nodes.issubset(agents):
    sys.exit(f"[peers] Dataset node must name one ready worker: {dataset_nodes}")
if len(simulation_nodes) > 1 or not simulation_nodes.issubset(agents):
    sys.exit(f"[peers] Simulation node must name one ready worker: {simulation_nodes}")
for node, (pod, port, url) in sorted(agents.items()):
    print(node, pod, port, url, sep="\t")
PY

NODES=()
PODS=()
PORTS=()
URLS=()
while IFS=$'\t' read -r node pod port url; do
    NODES+=("$node")
    PODS+=("$pod")
    PORTS+=("$port")
    URLS+=("$url")
done < "$work_dir/agents.tsv"

request() {
    local index="$1" method="$2" path="$3"; shift 3
    local url="http://localhost:${PORTS[$index]}${path}"
    if [[ "$path" == http://* ]]; then
        url="$path"
    fi
    kubectl -n "$NAMESPACE" exec "${PODS[$index]}" -c di-agent -- \
        curl --fail-with-body --silent --show-error --connect-timeout 5 --max-time 15 \
        -X "$method" -H "Content-Type: application/json" \
        "$url" "$@"
}

for i in "${!NODES[@]}"; do
    for j in "${!NODES[@]}"; do
        [ "$i" -eq "$j" ] && continue
        echo "[peers] ${NODES[$i]} <- ${NODES[$j]} (${URLS[$j]})"
        request "$i" GET "${URLS[$j]}/healthz" >/dev/null
        response=$(request "$i" POST /peers -d "{\"url\":\"${URLS[$j]}\"}")
        id=$(printf '%s' "$response" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
        [ -n "$id" ] || { echo "[peers] Empty peer ID" >&2; exit 1; }
        request "$i" POST "/peers/${id}/trust" -d '{"value":0.8}' >/dev/null
    done
    response=$(request "$i" GET /peers)
    printf '%s' "$response" | python3 -c '
import json, sys
peers = json.load(sys.stdin)
expected = set(sys.argv[2:])
actual = {p["url"] for p in peers}
if actual != expected:
    sys.exit(f"[peers] {sys.argv[1]}: expected {expected}, received {actual}")
print(f"[peers] {sys.argv[1]}: verified {len(peers)} peers")
' "${NODES[$i]}" "${URLS[@]:0:$i}" "${URLS[@]:$((i + 1))}"
done
