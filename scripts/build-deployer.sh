#!/bin/bash
# 编译 deployer 并导入本机 k8s 的 containerd(原型:不经过镜像仓库,单节点集群)。
set -euo pipefail
DIR=$(cd "$(dirname "$0")/../deployer" && pwd)
TAG=${1:-deployer:dev}
cd "$DIR"
go test ./...
CGO_ENABLED=0 GOTOOLCHAIN=local go build -trimpath -ldflags="-s -w" -o deployer .
pgrep -x dockerd >/dev/null || { setsid nohup dockerd >/var/log/dockerd.log 2>&1 </dev/null & sleep 8; }
docker build -q -t "$TAG" .
docker save "$TAG" | ctr -a /run/k8s-containerd/containerd.sock -n k8s.io images import - >/dev/null
rm -f deployer
echo "imported $TAG into k8s containerd"
