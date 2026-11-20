#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TF_DIR="$(dirname "$SCRIPT_DIR")/cloud/eks"
LOCAL_KUBECONFIG="${LOCAL_KUBECONFIG:-$HOME/.kube/config-poc2-eks}"

for tool in aws terraform; do
    command -v "$tool" >/dev/null || { echo "[eks] Missing $tool" >&2; exit 1; }
done
aws sts get-caller-identity >/dev/null

terraform -chdir="$TF_DIR" init -input=false
terraform -chdir="$TF_DIR" apply

cluster=$(terraform -chdir="$TF_DIR" output -raw cluster_name)
region=$(terraform -chdir="$TF_DIR" output -raw region)
umask 077
mkdir -p "$(dirname "$LOCAL_KUBECONFIG")"
aws eks update-kubeconfig --region "$region" --name "$cluster" --kubeconfig "$LOCAL_KUBECONFIG"
chmod 600 "$LOCAL_KUBECONFIG"
printf 'To use this cluster: export KUBECONFIG=%q\n' "$LOCAL_KUBECONFIG"
echo "See the PoC2 README for app credentials and make deploy-cloud."
