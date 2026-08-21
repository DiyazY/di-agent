#!/usr/bin/env bash

set -euo pipefail

if ! command -v kind >/dev/null 2>&1; then
	echo "Error: kind is required but was not found in PATH." >&2
	exit 1
fi

cluster_name="${KIND_CLUSTER_NAME:-kind}"

kind delete cluster --name "${cluster_name}"
