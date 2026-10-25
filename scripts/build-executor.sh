#!/bin/bash
# 构建执行器镜像并导入本机 k8s 的 containerd(原型:单节点集群,不经过镜像仓库)。
set -euo pipefail
DIR=$(cd "$(dirname "$0")/../executor" && pwd)
TAG=${1:-okdev-executor:dev}
V=${KUSTOMIZE_VERSION:-v5.8.1}
sh -n "$DIR/okdev-executor.sh"
if [ ! -x "$DIR/kustomize" ]; then
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  curl -fsSL -o "$tmp/k.tgz" "https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize%2F$V/kustomize_${V}_linux_amd64.tar.gz"
  tar xzf "$tmp/k.tgz" -C "$DIR" kustomize
fi
pgrep -x dockerd >/dev/null || { setsid nohup dockerd >/var/log/dockerd.log 2>&1 </dev/null & sleep 8; }
docker build -q -t "$TAG" "$DIR"
docker save "$TAG" | ctr -a /run/k8s-containerd/containerd.sock -n k8s.io images import - >/dev/null
echo "imported $TAG into k8s containerd"
