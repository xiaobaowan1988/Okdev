#!/bin/bash
# 沙箱里接上 Dex 登录:自签证书 -> Dex(MySQL 存储)-> kube-apiserver 信任 Dex -> 导入账号 -> Headlamp 改用 Dex 登录 -> 示例授权。
# 账号来源二选一:
#   STATIC_PASSWORDS=<zadig-export 的输出文件>   用你自己导出的账号
#   ZADIG_SRC=<koderover/zadig 的检出目录>        没有就用它的真实建表脚本造几个测试账号(alice/alice-pass-1 等)
# 会修改 kube-apiserver 的静态 Pod 清单(备份在同目录 *.pre-oidc)并让它重启。可重复执行。
set -euo pipefail
DIR=$(cd "$(dirname "$0")/.." && pwd)
export KUBECONFIG=${KUBECONFIG:-/etc/kubernetes/admin.conf}
NODE_IP=${NODE_IP:-$(hostname -I | awk '{print $1}')}
export NO_PROXY=${NO_PROXY:-},$NODE_IP no_proxy=${no_proxy:-},$NODE_IP
CERTS=${CERTS:-/tmp/okdev-dex-certs}
ISSUER=https://$NODE_IP:30556
step() { printf '\n==> %s\n' "$*"; }
# 等本机端口通(port-forward 起来需要一点时间)
wait_port() { for _ in $(seq 1 30); do (exec 3<>/dev/tcp/127.0.0.1/"$1") 2>/dev/null && return 0; sleep 1; done; echo "port $1 not reachable" >&2; return 1; }

step "certificates"
"$DIR/test/zadig/gen-certs.sh" "$CERTS" "$NODE_IP"

step "accounts"
if [ -z "${STATIC_PASSWORDS:-}" ]; then
  : "${ZADIG_SRC:?set STATIC_PASSWORDS or ZADIG_SRC}"
  ZADIG_SRC=$ZADIG_SRC "$DIR/test/zadig/seed.sh"
  kubectl -n zadig-test port-forward svc/mysql 13306:3306 >/dev/null 2>&1 & PF1=$!
  trap 'kill $PF1 ${PF2:-} 2>/dev/null || true' EXIT
  wait_port 13306
  STATIC_PASSWORDS=$CERTS/static-passwords.yaml
  (cd "$DIR" && GOTOOLCHAIN=local go run ./cmd/zadig-export -dsn 'root:test-only@tcp(127.0.0.1:13306)/zadig_user' > "$STATIC_PASSWORDS")
fi

step "Dex (MySQL storage)"
kubectl apply -f "$DIR/deploy/dex-sandbox/mysql.yaml"
kubectl -n okdev-system rollout status deploy/dex-mysql --timeout=240s
ISSUER=$ISSUER REDIRECT_URI=http://localhost:4466/oidc-callback \
TLS_CERT=$CERTS/tls.crt TLS_KEY=$CERTS/tls.key GRPC_CERT=$CERTS/grpc.crt GRPC_KEY=$CERTS/grpc.key GRPC_CLIENT_CA=$CERTS/ca.crt \
MYSQL_HOST=dex-mysql MYSQL_USER=dex MYSQL_PASSWORD=test-only-dex OVERLAY=$DIR/deploy/dex-sandbox \
  "$DIR/scripts/deploy-dex.sh"

step "kube-apiserver trusts Dex"
ISSUER=$ISSUER CA_FILE=$CERTS/ca.crt "$DIR/platform/scripts/enable-oidc.sh"
# apiserver 重启后 kubectl 的 port-forward 都断了,下面重建;Dex 签名密钥若刚换过,apiserver 缓存了旧公钥,需要它重启过才行(上一步已处理)

step "import accounts"
(cd "$DIR" && GOTOOLCHAIN=local go build -o "$CERTS/dex-users" ./cmd/dex-users)
kubectl -n okdev-system port-forward svc/dex 5557:5557 >/dev/null 2>&1 & PF2=$!
trap 'kill ${PF1:-} $PF2 2>/dev/null || true' EXIT
wait_port 5557
DEX_GRPC_CA=$CERTS/ca.crt DEX_GRPC_CERT=$CERTS/admin.crt DEX_GRPC_KEY=$CERTS/admin.key \
  "$CERTS/dex-users" import "$STATIC_PASSWORDS"
DEX_GRPC_CA=$CERTS/ca.crt DEX_GRPC_CERT=$CERTS/admin.crt DEX_GRPC_KEY=$CERTS/admin.key "$CERTS/dex-users" list

step "Headlamp -> Dex login, sample authorization"
kubectl -n okdev-system create configmap dex-ca --from-file=ca.crt="$CERTS/ca.crt" --dry-run=client -o yaml | kubectl apply -f -
HEADLAMP_OVERLAY=$DIR/deploy/headlamp-dex-sandbox "$DIR/scripts/deploy-headlamp.sh"
kubectl apply -f "$DIR/deploy/dex-sandbox/developers.yaml"

cat <<MSG

Done. Open Headlamp:  kubectl -n okdev-system port-forward svc/headlamp 4466:80   # http://localhost:4466
Log in with a Zadig username or email (test accounts: alice / alice-pass-1 is authorized, bob / bob-pass-2 can log in but has no permissions).
Manage accounts: dex-users (built at $CERTS/dex-users); keep the Dex admin certs in $CERTS.
MSG
