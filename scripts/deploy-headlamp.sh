#!/bin/bash
# 构建插件,把产物放进 ConfigMap,部署 Headlamp。访问:kubectl -n okdev-system port-forward svc/headlamp 4466:80
set -euo pipefail
export KUBECONFIG=${KUBECONFIG:-/etc/kubernetes/admin.conf}
DIR=$(cd "$(dirname "$0")/.." && pwd)
P=$DIR/headlamp-plugin/okdev-console
(cd "$P" && { [ -d node_modules ] || npm install; } && npm run build)
kubectl create ns okdev-system --dry-run=client -o yaml | kubectl apply -f -
kubectl -n okdev-system create configmap okdev-plugin \
  --from-file=main.js="$P/dist/main.js" --from-file=package.json="$P/package.json" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f "$DIR/deploy/headlamp/headlamp.yaml"
kubectl -n okdev-system rollout restart deploy/headlamp >/dev/null
kubectl -n okdev-system rollout status deploy/headlamp --timeout=180s
