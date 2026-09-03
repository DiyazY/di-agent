#!/usr/bin/env bash
# build-push-images.sh — build the chart-managed service images and push
#                         them to a container registry, for use with the
#                         Helm chart in poc2/helm/di-agent-system (see
#                         values.yaml: global.imageRegistry/global.imageTag).
#
# Usage: REGISTRY=ghcr.io/myorg TAG=v1 ./build-push-images.sh [service ...]
#   With no service names given, builds+pushes all of them. di-agent is
#   intentionally not included here; it is still built/deployed via
#   scripts/04-agent.sh.

set -euo pipefail

GREEN=$(tput setaf 2 2>/dev/null || echo "")
YELLOW=$(tput setaf 3 2>/dev/null || echo "")
RED=$(tput setaf 1 2>/dev/null || echo "")
RESET=$(tput sgr0 2>/dev/null || echo "")

info() { echo "${YELLOW}[images] $*${RESET}"; }
ok()   { echo "${GREEN}[images] $*${RESET}"; }
err()  { echo "${RED}[images] $*${RESET}" >&2; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
POC_DIR="$(dirname "$SCRIPT_DIR")"
SYSTEM_DIR="$POC_DIR/system"

REGISTRY="${REGISTRY:-ghcr.io/chuducanh242002}"
TAG="${TAG:-latest}"

service_dir() {
    case "$1" in
        genset) printf '%s\n' "$SYSTEM_DIR/genset" ;;
        switchboard) printf '%s\n' "$SYSTEM_DIR/switchboard" ;;
        propulsion) printf '%s\n' "$SYSTEM_DIR/propulsion" ;;
        battery) printf '%s\n' "$SYSTEM_DIR/battery" ;;
        auxload) printf '%s\n' "$SYSTEM_DIR/auxiliary-load" ;;
        telemetry-writer) printf '%s\n' "$SYSTEM_DIR/telemetry-writer" ;;
        playground) printf '%s\n' "$POC_DIR/playground" ;;
        *) return 1 ;;
    esac
}

if [ "$#" -eq 0 ]; then
    SERVICES=(genset switchboard propulsion battery auxload telemetry-writer playground)
else
    SERVICES=("$@")
fi

for name in "${SERVICES[@]}"; do
    if ! dir="$(service_dir "$name")"; then
        err "Unknown service: $name"
        exit 1
    fi
    if [ ! -d "$dir" ]; then
        err "Build context not found: $dir"
        exit 1
    fi

    image="${REGISTRY}/${name}:${TAG}"
    info "Building ${image} from ${dir} ..."
    docker build -t "$image" "$dir"
    info "Pushing ${image} ..."
    docker push "$image"
    ok "Pushed ${image}"
done

ok "Done. Deploy with: helm upgrade --install di-agent-system poc2/helm/di-agent-system \\
    --set global.imageRegistry=${REGISTRY} --set global.imageTag=${TAG}"
