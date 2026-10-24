#!/bin/bash
# 构建演示服务 api 的镜像 api:<版本> 并导入本机 k8s 的 containerd(单节点集群,不经过镜像仓库)。
# 用法: build-example-api.sh v1
set -euo pipefail
VER=${1:?用法: build-example-api.sh <版本,如 v1>}
DIR=$(cd "$(dirname "$0")/../examples/api" && pwd)
cd "$DIR"
CGO_ENABLED=0 GOTOOLCHAIN=local go build -trimpath -ldflags="-s -w -X main.version=$VER" -o api .
pgrep -x dockerd >/dev/null || { setsid nohup dockerd >/var/log/dockerd.log 2>&1 </dev/null & sleep 8; }
docker build -q -t "api:$VER" . >/dev/null
docker save "api:$VER" | ctr -a /run/k8s-containerd/containerd.sock -n k8s.io images import - >/dev/null
rm -f api
echo "imported api:$VER into k8s containerd"
