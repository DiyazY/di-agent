#!/usr/bin/env bash
# teardown.sh — destroy all PoC infrastructure using Terraform
#
# Usage: ./teardown.sh
#   Destroys all resources defined in the Terraform configuration

set -euo pipefail

POC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TF_DIR="$POC_DIR/infra/libvirt"
if [[ -e "$POC_DIR/terraform.tfstate" || -d "$POC_DIR/terraform.tfstate.d" ]]; then
    echo "Libvirt state still exists in $POC_DIR. See the PoC2 README before teardown." >&2
    exit 1
fi

# ── colours ─────────────────────────────────────────────────────────────────
GREEN=$(tput setaf 2 2>/dev/null || echo "")
YELLOW=$(tput setaf 3 2>/dev/null || echo "")
RED=$(tput setaf 1 2>/dev/null || echo "")
RESET=$(tput sgr0 2>/dev/null || echo "")

info() { echo "${YELLOW}[teardown] $*${RESET}"; }
ok()   { echo "${GREEN}[teardown] $*${RESET}"; }
err()  { echo "${RED}[teardown] $*${RESET}" >&2; }

# ── terraform destroy ─────────────────────────────────────────────────────
info "Destroying all Terraform-managed infrastructure ..."

# Check if terraform is available
if ! command -v terraform &> /dev/null; then
    err "terraform command not found. Please install Terraform."
    exit 1
fi

# Run terraform destroy with auto-approve to skip confirmation
terraform -chdir="$TF_DIR" destroy -auto-approve

ok "Terraform destroy complete"

echo ""
ok "Teardown done — all infrastructure destroyed."