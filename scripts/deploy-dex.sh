#!/bin/bash
# 部署 Dex。用法:
#   ISSUER=https://dex.example.com REDIRECT_URI=https://headlamp.example.com/oidc-callback \
#   STATIC_PASSWORDS=static-passwords.yaml TLS_CERT=tls.crt TLS_KEY=tls.key scripts/deploy-dex.sh
# STATIC_PASSWORDS 由 cmd/zadig-export 生成(含密码哈希,按密钥对待)。
# OVERLAY 默认 deploy/dex;沙箱用 deploy/dex-sandbox。
set -euo pipefail
export KUBECONFIG=${KUBECONFIG:-/etc/kubernetes/admin.conf}
DIR=$(cd "$(dirname "$0")/.." && pwd)
: "${ISSUER:?}" "${REDIRECT_URI:?}" "${STATIC_PASSWORDS:?}" "${TLS_CERT:?}" "${TLS_KEY:?}"

kubectl create ns okdev-system --dry-run=client -o yaml | kubectl apply -f -
# 配置 = 模板 + 导出的静态账号(两段是不同的顶层键,直接拼接)
cfg=$(mktemp); trap 'rm -f "$cfg"' EXIT
sed -e "s#__ISSUER__#$ISSUER#" -e "s#__REDIRECT_URI__#$REDIRECT_URI#" "$DIR/deploy/dex/config.tmpl.yaml" > "$cfg"
cat "$STATIC_PASSWORDS" >> "$cfg"
kubectl -n okdev-system create secret generic dex-config --from-file=config.yaml="$cfg" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n okdev-system create secret tls dex-tls --cert="$TLS_CERT" --key="$TLS_KEY" --dry-run=client -o yaml | kubectl apply -f -
# Headlamp 客户端密钥只在首次生成,之后保持不变;Headlamp 那边的 Secret 用同一个值
if ! kubectl -n okdev-system get secret dex-client >/dev/null 2>&1; then
  kubectl -n okdev-system create secret generic dex-client --from-literal=headlamp-secret="$(openssl rand -hex 24)"
fi
kubectl apply -k "${OVERLAY:-$DIR/deploy/dex}"
kubectl -n okdev-system rollout restart deploy/dex >/dev/null   # 账号更新后重启生效
kubectl -n okdev-system rollout status deploy/dex --timeout=180s
