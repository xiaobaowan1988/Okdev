#!/bin/bash
# 用 kustomize 安装 Argo CD(base: 官方 manifest + Apps in Any Namespace)
# 在沙箱里(存在 /root/.ccr/ca-bundle.crt)自动使用 sandbox overlay,让 repo-server 信任出口网关 CA。
set -euo pipefail
export KUBECONFIG=${KUBECONFIG:-/etc/kubernetes/admin.conf}
DIR=$(cd "$(dirname "$0")/.." && pwd)
CA=${SANDBOX_CA:-/root/.ccr/ca-bundle.crt}

if [ -f "$CA" ]; then
  cp "$CA" "$DIR/manifests/argocd/overlays/sandbox/ca-bundle.crt"
  TARGET=$DIR/manifests/argocd/overlays/sandbox
else
  TARGET=$DIR/manifests/argocd/base
fi
echo "使用 $TARGET"

# CRD 过大,必须用 server-side apply
kubectl apply --server-side --force-conflicts -k "$TARGET"
kubectl -n argocd wait --for=condition=available deploy --all --timeout=300s
kubectl -n argocd rollout status sts/argocd-application-controller --timeout=120s
kubectl -n argocd get pods

cat <<'MSG'

初始 admin 密码:
  kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d
临时访问 UI:
  kubectl -n argocd port-forward svc/argocd-server 8080:443   # https://localhost:8080
MSG
