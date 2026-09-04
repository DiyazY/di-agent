#!/usr/bin/env bash
# build-push-images.sh — build the chart-managed service images and push
#                         them to a container registry, for use with the
#                         Helm chart in poc2/helm/di-agent-system (see
#                         values.yaml: global.imageRegistry/global.imageTag).
#
# Usage: REGISTRY=ghcr.io/myorg TAG=v1 ./build-push-images.sh [options] [service ...]
#   With no service names given, builds+pushes all of them. di-agent is
#   intentionally not included here; it is still built/deployed via
#   scripts/04-agent.sh.
#
# Options:
#   -h, --help              Show this help and exit.
#   -l, --list              List available service names and exit.
#   -x, --exclude LIST       Comma-separated services to skip (only useful
#                            when no explicit service names are given).
#   --build-only             Build images but skip the docker push step.
#   --push-only              Skip the build step and only push (images must
#                            already exist locally).
#
# Service names may also be given as a single comma-separated argument,
# e.g. ./build-push-images.sh genset,battery

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

ALL_SERVICES=(genset switchboard propulsion battery auxload telemetry-writer playground)

usage() {
    sed -n '2,22p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

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

EXCLUDE=""
DO_BUILD=1
DO_PUSH=1
POSITIONAL=()

while [ "$#" -gt 0 ]; do
    case "$1" in
        -h|--help) usage; exit 0 ;;
        -l|--list) printf '%s\n' "${ALL_SERVICES[@]}"; exit 0 ;;
        -x|--exclude) EXCLUDE="$2"; shift 2 ;;
        --build-only) DO_PUSH=0; shift ;;
        --push-only) DO_BUILD=0; shift ;;
        --) shift; POSITIONAL+=("$@"); break ;;
        -*) err "Unknown option: $1"; usage; exit 1 ;;
        *) POSITIONAL+=("$1"); shift ;;
    esac
done

# allow a single comma-separated argument as well as space-separated ones
IFS=',' read -r -a POSITIONAL <<< "$(IFS=,; echo "${POSITIONAL[*]}")"

if [ "${#POSITIONAL[@]}" -eq 0 ]; then
    SERVICES=("${ALL_SERVICES[@]}")
else
    SERVICES=("${POSITIONAL[@]}")
fi

if [ -n "$EXCLUDE" ]; then
    IFS=',' read -r -a EXCLUDE_LIST <<< "$EXCLUDE"
    FILTERED=()
    for name in "${SERVICES[@]}"; do
        skip=0
        for ex in "${EXCLUDE_LIST[@]}"; do
            [ "$name" = "$ex" ] && skip=1 && break
        done
        [ "$skip" -eq 0 ] && FILTERED+=("$name")
    done
    SERVICES=("${FILTERED[@]}")
fi

for name in "${SERVICES[@]}"; do
    if ! dir="$(service_dir "$name")"; then
        err "Unknown service: $name (available: ${ALL_SERVICES[*]})"
        exit 1
    fi
    if [ ! -d "$dir" ]; then
        err "Build context not found: $dir"
        exit 1
    fi

    image="${REGISTRY}/${name}:${TAG}"
    if [ "$DO_BUILD" -eq 1 ]; then
        info "Building ${image} from ${dir} ..."
        docker build -t "$image" "$dir"
    fi
    if [ "$DO_PUSH" -eq 1 ]; then
        info "Pushing ${image} ..."
        docker push "$image"
        ok "Pushed ${image}"
    fi
done

ok "Done. Deploy with: helm upgrade --install di-agent-system poc2/helm/di-agent-system \\
    --set global.imageRegistry=${REGISTRY} --set global.imageTag=${TAG}"
