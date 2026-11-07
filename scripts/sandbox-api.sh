#!/bin/bash
# 沙箱里接上 Zadig 账号登录和 API 网关:自签证书 -> Dex(MySQL 存储)-> kube-apiserver 信任 Dex -> 导入账号 -> API 网关 -> 示例授权。
# 账号来源二选一:
#   STATIC_PASSWORDS=<zadig-export 的输出文件>   用你自己导出的账号
#   ZADIG_SRC=<koderover/zadig 的检出目录>        没有就用它的真实建表脚本造几个测试账号(alice/alice-pass-1 等)
# 会修改 kube-apiserver 的静态 Pod 清单(备份在 /var/backups/okdev,不能放在清单目录里)并让它重启。可重复执行。
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
ISSUER=$ISSUER \
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

step "API gateway"
kubectl -n okdev-system create configmap dex-ca --from-file=ca.crt="$CERTS/ca.crt" --dry-run=client -o yaml | kubectl apply -f -
if kubectl -n deployer-build get cm build-ca >/dev/null 2>&1; then   # 沙箱出口 CA,见 deploy/api-sandbox
  kubectl -n deployer-build get cm build-ca -o json \
    | python3 -c "import json,sys;d=json.load(sys.stdin);d['metadata']={'name':'build-ca','namespace':'okdev-system'};print(json.dumps(d))" | kubectl apply -f -
fi
"$DIR/scripts/build-api.sh"
kubectl apply -k "$DIR/deploy/api-sandbox"
kubectl -n okdev-system rollout restart deploy/okdev-api >/dev/null
kubectl -n okdev-system rollout status deploy/okdev-api --timeout=180s

step "Kubernetes RBAC for developers (sample)"
kubectl apply -f "$DIR/deploy/rbac/developer.yaml"
kubectl apply -f "$DIR/deploy/dex-sandbox/developers.yaml"

cat <<MSG

Done.
  API gateway:  http://$NODE_IP:30080   (GET /openapi.yaml describes it)
  Log in:       $DIR/bin/okdev login --api http://$NODE_IP:30080                    # Zadig username (or email) and password
  Call the API: curl -H "Authorization: Bearer \$(okdev token)" http://$NODE_IP:30080/v1/envs
  Test accounts: alice / alice-pass-1 (developer; also read access to Kubernetes), bob / bob-pass-2 (developer), carol / carol-pass-3 (administrator).
  Manage accounts: $CERTS/dex-users (keep the Dex admin certs in $CERTS).
MSG
