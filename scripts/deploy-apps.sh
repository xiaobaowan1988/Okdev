#!/bin/bash
# 部署 Application 的共用底座:配置命名空间 argocd-apps、AppProject apps、仓库登记(需先运行 install-argocd.sh)。
# 与环境无关,只有这一份;应用到环境的映射由 deployer(MySQL)管理。
set -euo pipefail
export KUBECONFIG=${KUBECONFIG:-/etc/kubernetes/admin.conf}
DIR=$(cd "$(dirname "$0")/.." && pwd)
kubectl apply -k "$DIR/manifests/apps"
