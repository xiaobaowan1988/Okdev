#!/bin/bash
# 部署演示 ApplicationSet(需先运行 install-argocd.sh 和 deploy-apps.sh)。用法: deploy-appsets.sh [dev|test],默认两个都部署
set -euo pipefail
export KUBECONFIG=${KUBECONFIG:-/etc/kubernetes/admin.conf}
DIR=$(cd "$(dirname "$0")/.." && pwd)
for env in ${1:-dev test}; do kubectl apply -k "$DIR/manifests/appsets/$env"; done
echo "查看状态: kubectl get appset,app -A"
