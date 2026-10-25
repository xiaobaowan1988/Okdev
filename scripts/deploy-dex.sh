#!/bin/bash
# 部署 Dex(账号存 MySQL)。用法:
#   ISSUER=https://dex.example.com REDIRECT_URI=https://headlamp.example.com/oidc-callback \
#   TLS_CERT=tls.crt TLS_KEY=tls.key \
#   GRPC_CERT=grpc.crt GRPC_KEY=grpc.key GRPC_CLIENT_CA=admin-ca.crt \
#   MYSQL_HOST=mysql.example.com MYSQL_USER=dex MYSQL_PASSWORD=... \
#   scripts/deploy-dex.sh
# 可选:MYSQL_PORT(3306)、MYSQL_DATABASE(dex)、MYSQL_SSL_MODE(false;生产建议 true 或 skip-verify)、OVERLAY(默认 deploy/dex;沙箱用 deploy/dex-sandbox)。
# 账号不在这里配置:部署后用 dex-users 导入和管理(见 docs/zadig-accounts-dex.md)。
set -euo pipefail
export KUBECONFIG=${KUBECONFIG:-/etc/kubernetes/admin.conf}
DIR=$(cd "$(dirname "$0")/.." && pwd)
: "${ISSUER:?}" "${REDIRECT_URI:?}" "${TLS_CERT:?}" "${TLS_KEY:?}" "${GRPC_CERT:?}" "${GRPC_KEY:?}" "${GRPC_CLIENT_CA:?}"
: "${MYSQL_HOST:?}" "${MYSQL_USER:?}" "${MYSQL_PASSWORD:?}"
MYSQL_PORT=${MYSQL_PORT:-3306}; MYSQL_DATABASE=${MYSQL_DATABASE:-dex}; MYSQL_SSL_MODE=${MYSQL_SSL_MODE:-false}

kubectl create ns okdev-system --dry-run=client -o yaml | kubectl apply -f -
# 配置 = 模板 + 实际值。密码放进双引号 YAML 字符串,先转义反斜杠和双引号;sed 的分隔符/特殊字符也要转义
esc() { printf '%s' "$1" | sed -e 's/[\/&|]/\\&/g'; }
pw=${MYSQL_PASSWORD//\\/\\\\}; pw=${pw//\"/\\\"}
cfg=$(mktemp); trap 'rm -f "$cfg"' EXIT
sed -e "s|__ISSUER__|$(esc "$ISSUER")|" -e "s|__REDIRECT_URI__|$(esc "$REDIRECT_URI")|" \
    -e "s|__MYSQL_HOST__|$(esc "$MYSQL_HOST")|" -e "s|__MYSQL_PORT__|$MYSQL_PORT|" \
    -e "s|__MYSQL_DATABASE__|$(esc "$MYSQL_DATABASE")|" -e "s|__MYSQL_USER__|$(esc "$MYSQL_USER")|" \
    -e "s|__MYSQL_SSL_MODE__|$(esc "$MYSQL_SSL_MODE")|" \
    -e "s|password: __MYSQL_PASSWORD__|password: \"$(esc "$pw")\"|" "$DIR/deploy/dex/config.tmpl.yaml" > "$cfg"
kubectl -n okdev-system create secret generic dex-config --from-file=config.yaml="$cfg" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n okdev-system create secret tls dex-tls --cert="$TLS_CERT" --key="$TLS_KEY" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n okdev-system create secret generic dex-grpc-tls --from-file=tls.crt="$GRPC_CERT" --from-file=tls.key="$GRPC_KEY" --from-file=ca.crt="$GRPC_CLIENT_CA" --dry-run=client -o yaml | kubectl apply -f -
# Headlamp 客户端密钥只在首次生成,之后保持不变;Headlamp 那边的 Secret 用同一个值
if ! kubectl -n okdev-system get secret dex-client >/dev/null 2>&1; then
  kubectl -n okdev-system create secret generic dex-client --from-literal=headlamp-secret="$(openssl rand -hex 24)"
fi
kubectl apply -k "${OVERLAY:-$DIR/deploy/dex}"
kubectl -n okdev-system rollout restart deploy/dex >/dev/null   # 配置或证书变化后重启生效(账号变化不需要重启)
kubectl -n okdev-system rollout status deploy/dex --timeout=180s
