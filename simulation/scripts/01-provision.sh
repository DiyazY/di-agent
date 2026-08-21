#!/usr/bin/env bash

set -euo pipefail

if ! command -v kind >/dev/null 2>&1; then
	echo "Error: kind is required but was not found in PATH." >&2
	exit 1
fi

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
config_file="${script_dir}/../config/kind-config.yml"

kind create cluster --config "${config_file}"
