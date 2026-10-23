#!/bin/bash
# 部署 AppProject / Application(需先运行 install-argocd.sh)。用法: deploy-apps.sh [env],默认 dev
set -euo pipefail
export KUBECONFIG=${KUBECONFIG:-/etc/kubernetes/admin.conf}
ENV=${1:-dev}
DIR=$(cd "$(dirname "$0")/.." && pwd)
kubectl apply -k "$DIR/manifests/apps/$ENV"
echo "查看状态: kubectl get app -n argocd-apps-$ENV"
