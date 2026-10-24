#!/bin/bash
# 在集群里部署原型用的 MySQL(namespace: deployer)。密码随机生成并只存在集群 Secret 里。
set -euo pipefail
export KUBECONFIG=${KUBECONFIG:-/etc/kubernetes/admin.conf}
DIR=$(cd "$(dirname "$0")/.." && pwd)
kubectl create namespace deployer --dry-run=client -o yaml | kubectl apply -f -
if ! kubectl -n deployer get secret deployer-mysql >/dev/null 2>&1; then
  kubectl -n deployer create secret generic deployer-mysql \
    --from-literal=root-password="$(openssl rand -hex 16)" --from-literal=password="$(openssl rand -hex 16)"
fi
kubectl apply -k "$DIR/manifests/deployer/mysql"
kubectl -n deployer rollout status deploy/mysql --timeout=240s
echo "DSN 示例(密码取自 Secret): deployer:<password>@tcp(mysql.deployer.svc:3306)/deployer"
