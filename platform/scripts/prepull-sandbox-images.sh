#!/bin/bash
# 沙箱里共享出口 IP 容易被 public.ecr.aws / Docker Hub 限流(429)。把 Argo CD 要用的 redis 镜像先从 mirror.gcr.io 拉下来,
# 再打成清单里写的名字,kubelet 就不用再去拉。可重复执行。
set -euo pipefail
SOCK=${CONTAINERD_SOCK:-/run/k8s-containerd/containerd.sock}
REDIS_TAG=${REDIS_TAG:-8.2.3-alpine}   # 与 manifests/argocd/base 里 install.yaml 的 redis 版本一致
crictl pull "mirror.gcr.io/library/redis:$REDIS_TAG" >/dev/null
ctr -a "$SOCK" -n k8s.io images tag "mirror.gcr.io/library/redis:$REDIS_TAG" "public.ecr.aws/docker/library/redis:$REDIS_TAG" 2>/dev/null || true
echo "prepulled redis:$REDIS_TAG"
