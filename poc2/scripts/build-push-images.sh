#!/usr/bin/env bash
# build-push-images.sh — build the PoC service images and push
#                         them to a container registry, for use with the
#                         Helm chart in poc2/helm/di-agent-system (see
#                         values.yaml: global.imageRegistry/global.imageTag).
#
# Usage: REGISTRY=ghcr.io/myorg TAG=v1 ./build-push-images.sh [options] [service ...]
#   With no service names given, builds+pushes all of them.
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
#
# telemetry-writer-cloud (helm: telemetryWriterCloud) is not listed below:
# it runs the telemetry-writer image unchanged, just against Event Hubs/a
# cloud InfluxDB instead of the local broker, so no separate build is needed.

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
SEMANTIC_MAP_DIR="$(dirname "$POC_DIR")/semantic-map"

REGISTRY="${REGISTRY:-ghcr.io/chuducanh242002}"
TAG="${TAG:-latest}"

ALL_SERVICES=(genset switchboard propulsion battery shore-power auxload navigation telemetry-writer telemetry-cloud-sync playground data-ingest replay-streamer dataset-collector di-agent)

usage() {
    sed -n '2,22p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

service_dir() {
    case "$1" in
        genset) printf '%s\n' "$SYSTEM_DIR/genset" ;;
        switchboard) printf '%s\n' "$SYSTEM_DIR/switchboard" ;;
        propulsion) printf '%s\n' "$SYSTEM_DIR/propulsion" ;;
        battery) printf '%s\n' "$SYSTEM_DIR/battery" ;;
        shore-power) printf '%s\n' "$SYSTEM_DIR/shore-power" ;;
        auxload) printf '%s\n' "$SYSTEM_DIR/auxiliary-load" ;;
        navigation) printf '%s\n' "$SYSTEM_DIR/navigation" ;;
        telemetry-writer) printf '%s\n' "$SYSTEM_DIR/telemetry-writer" ;;
        telemetry-cloud-sync) printf '%s\n' "$SYSTEM_DIR/telemetry-cloud-sync" ;;
        playground) printf '%s\n' "$POC_DIR/playground" ;;
        data-ingest) printf '%s\n' "$POC_DIR/data-ingest" ;;
        replay-streamer) printf '%s\n' "$SYSTEM_DIR/replay-streamer" ;;
        dataset-collector) printf '%s\n' "$SYSTEM_DIR/dataset-collector" ;;
        di-agent) printf '%s\n' "$SEMANTIC_MAP_DIR" ;;
        *) return 1 ;;
    esac
}

build_di_agent() {
    local output="cmd/agent/tmp/di-agent-poc"

    (
        cd "$SEMANTIC_MAP_DIR/go"
        mkdir -p "$(dirname "$output")"
        if command -v go >/dev/null 2>&1; then
            GOOS="${BUILD_GOOS:-linux}" GOARCH="${BUILD_GOARCH:-amd64}" \
                go build -o "$output" ./cmd/agent/
        else
            info "go not found on host; building di-agent inside a golang container"
            docker run --rm -u "$(id -u):$(id -g)" -e HOME=/tmp -e GOCACHE=/tmp/gocache \
                -e GOOS="${BUILD_GOOS:-linux}" -e GOARCH="${BUILD_GOARCH:-amd64}" -e CGO_ENABLED=0 \
                -v "$SEMANTIC_MAP_DIR/go":/src -w /src "${GO_IMAGE:-golang:1.26.0}" \
                go build -o "$output" ./cmd/agent/
        fi
    )
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
        if [ "$name" = "di-agent" ]; then
            build_di_agent
        fi
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
