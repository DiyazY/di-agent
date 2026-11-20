#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TF_DIR="$(dirname "$SCRIPT_DIR")/cloud/eks"

if [[ ! -s "$TF_DIR/terraform.tfstate" ]]; then
    echo "[eks] No local Terraform state in $TF_DIR; restore it before teardown." >&2
    exit 1
fi

terraform -chdir="$TF_DIR" init -input=false
terraform -chdir="$TF_DIR" destroy
