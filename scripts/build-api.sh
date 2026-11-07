#!/bin/bash
# 编译 API 网关并导入本机 k8s 的 containerd(原型:单节点集群,不经过镜像仓库);同时编出命令行工具 bin/okdev。
set -euo pipefail
DIR=$(cd "$(dirname "$0")/.." && pwd)
TAG=${1:-okdev-api:dev}
cd "$DIR"
GOTOOLCHAIN=${GOTOOLCHAIN:-local} go test ./cmd/... ./internal/...
mkdir -p bin
CGO_ENABLED=0 GOTOOLCHAIN=${GOTOOLCHAIN:-local} go build -trimpath -ldflags="-s -w" -o bin/okdev ./cmd/okdev
CGO_ENABLED=0 GOTOOLCHAIN=${GOTOOLCHAIN:-local} go build -trimpath -ldflags="-s -w" -o okdev-api ./cmd/api
pgrep -x dockerd >/dev/null || { setsid nohup dockerd >/var/log/dockerd.log 2>&1 </dev/null & sleep 8; }
docker build -q -t "$TAG" .
docker save "$TAG" | ctr -a /run/k8s-containerd/containerd.sock -n k8s.io images import - >/dev/null
rm -f okdev-api
echo "imported $TAG into k8s containerd; CLI at $DIR/bin/okdev"
