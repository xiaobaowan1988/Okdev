#!/bin/bash
# 在已有集群上安装 Argo CD(官方 stable manifest)
set -euo pipefail
export KUBECONFIG=${KUBECONFIG:-/etc/kubernetes/admin.conf}
ARGOCD_MANIFEST=${ARGOCD_MANIFEST:-https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml}

kubectl create namespace argocd --dry-run=client -o yaml | kubectl apply -f -
# CRD 过大,必须用 server-side apply
kubectl apply -n argocd --server-side --force-conflicts -f "$ARGOCD_MANIFEST"
kubectl -n argocd wait --for=condition=available deploy --all --timeout=300s
kubectl -n argocd rollout status sts/argocd-application-controller --timeout=120s
kubectl -n argocd get pods

cat <<'MSG'

初始 admin 密码:
  kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d
临时访问 UI:
  kubectl -n argocd port-forward svc/argocd-server 8080:443   # https://localhost:8080
MSG
