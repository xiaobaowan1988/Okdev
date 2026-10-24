#!/bin/bash
# 部署流水线需要的集群内组件:构建命名空间 deployer-build 和镜像仓库(registry,NodePort 30500)。
# 沙箱里出口 HTTPS 被中间人,构建任务需要信任它的 CA:存在 /root/.ccr/ca-bundle.crt 时做成 ConfigMap build-ca(没有就跳过,Job 里的挂载是 optional 的)。
set -euo pipefail
export KUBECONFIG=${KUBECONFIG:-/etc/kubernetes/admin.conf}
DIR=$(cd "$(dirname "$0")/.." && pwd)
CA=${BUILD_CA:-/root/.ccr/ca-bundle.crt}
kubectl apply -k "$DIR/manifests/deployer/build"
if [ -f "$CA" ]; then
  # build-ca.crt:完整信任链,给 git 用;extra-ca.crt:只有沙箱出口网关的根证书,构建脚本用它导入 JDK 等自带信任库的工具(见 $BUILD_EXTRA_CA_FILE)
  EXTRA=$(mktemp)
  trap 'rm -f "$EXTRA"' EXIT
  awk '/BEGIN CERTIFICATE/{n++} {print > "'"$EXTRA".'"sprintf("%03d",n)}' "$CA"
  for f in "$EXTRA".*; do
    openssl x509 -in "$f" -noout -subject 2>/dev/null | grep -q 'O *= *Anthropic' && cat "$f" >> "$EXTRA"
    rm -f "$f"
  done
  kubectl -n deployer-build create configmap build-ca --from-file=build-ca.crt="$CA" --from-file=extra-ca.crt="$EXTRA" --dry-run=client -o yaml | kubectl apply -f -
fi
kubectl -n deployer-build rollout status deploy/registry --timeout=180s
echo "仓库:集群内 registry.deployer-build.svc:5000,节点上 localhost:30500"
