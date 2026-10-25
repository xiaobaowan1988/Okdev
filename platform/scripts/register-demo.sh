#!/bin/bash
# 在 deployer 里登记演示数据:环境 dev3/dev5、服务 api/api-b、构建模板 go-template、构建器、流水线 build-and-deploy。
# 服务源码和清单在 fork 仓库 xiaobaowan1988/argocd-example-apps(demo/api、src/api)。可重复执行:已存在的跳过。
# 用法: scripts/register-demo.sh   (经 kubectl port-forward 访问 deployer;DEPLOYER_URL 已设置则直连)
set -euo pipefail
export KUBECONFIG=${KUBECONFIG:-/etc/kubernetes/admin.conf}
REPO=${DEMO_REPO:-https://github.com/xiaobaowan1988/argocd-example-apps.git}
command -v jq >/dev/null || { echo "jq is required" >&2; exit 1; }

if [ -z "${DEPLOYER_URL:-}" ]; then
  PORT=${PORT:-18080}
  kubectl -n deployer rollout status deploy/deployer --timeout=180s >/dev/null
  kubectl -n deployer port-forward svc/deployer "$PORT":8080 >/dev/null 2>&1 &
  PF=$!; trap 'kill $PF 2>/dev/null' EXIT
  DEPLOYER_URL=http://127.0.0.1:$PORT
  for _ in $(seq 1 30); do curl -fs --noproxy '*' "$DEPLOYER_URL/healthz" >/dev/null 2>&1 && break; sleep 1; done
fi

post() { # post <path> <json> <label>
  local code; code=$(curl -s --noproxy '*' -o /tmp/register-demo.out -w '%{http_code}' -X POST "$DEPLOYER_URL$1" -d "$2")
  case $code in
    2*) echo "created  $3" ;;
    409) echo "exists   $3" ;;
    *) echo "FAILED   $3: HTTP $code $(cat /tmp/register-demo.out)" >&2; exit 1 ;;
  esac
}

# 服务:工作负载名是 Deployment 的名字,默认等于服务名;api-b 复用同一份清单(Deployment 叫 api),所以要指定 workload
post /v1/services "$(jq -n --arg r "$REPO" '{name:"api",repo_url:$r,path:"demo/api",image_name:"api"}')" "service api"
post /v1/services "$(jq -n --arg r "$REPO" '{name:"api-b",repo_url:$r,path:"demo/api",image_name:"api",workload:"api"}')" "service api-b"
post /v1/envs '{"name":"dev3"}' "env dev3"
post /v1/envs '{"name":"dev5"}' "env dev5"

# 构建模板:脚本在 golang 镜像里编译,Dockerfile 把产物打成单阶段镜像
DF='FROM mirror.gcr.io/library/alpine:3.20
COPY app /app
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/app"]'
SC='set -eu
CGO_ENABLED=0 go build -ldflags "-X main.version=$VERSION -X main.profile=${PROFILE:-prod}" -o app .'
post /v1/templates "$(jq -n --arg df "$DF" --arg sc "$SC" '{name:"go-template",image:"mirror.gcr.io/library/golang:1.24-alpine",profile:"prod",dockerfile:$df,script:$sc}')" "template go-template"
post /v1/builders "$(jq -n --arg r "$REPO" '{name:"api-main",service:"api",repo_url:$r,ref:"master",context_path:"src/api",template:"go-template"}')" "builder api-main"
post /v1/builders "$(jq -n --arg r "$REPO" '{name:"api-b-main",service:"api-b",repo_url:$r,ref:"master",context_path:"src/api",template:"go-template"}')" "builder api-b-main"
post /v1/pipelines '{"name":"build-and-deploy","description":"build image and deploy to the chosen env"}' "pipeline build-and-deploy"
