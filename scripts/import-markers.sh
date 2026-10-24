#!/bin/bash
# 把 ApplicationSet 时代的标记文件 demo/<应用>/envs/<环境>.yaml 导入 deployer(MySQL)。幂等,可重复运行。
# 环境 group 取环境名去掉末尾数字(dev3 → dev,test → test),决定 Application 放哪个命名空间/绑哪个 AppProject。
# 用法: import-markers.sh <fork 仓库目录> [API 地址,默认 http://127.0.0.1:18080]
set -euo pipefail
FORK=${1:?用法: import-markers.sh <fork 仓库目录> [API]}; A=${2:-http://127.0.0.1:18080}
REPO=${REPO_URL:-$(git -C "$FORK" remote get-url origin | sed 's#\.git$##').git}
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
for dir in "$FORK"/demo/*/; do
  app=$(basename "$dir"); [ -d "$dir/envs" ] || continue
  c=$(code -X POST $A/v1/services -d "{\"name\":\"$app\",\"repo_url\":\"$REPO\",\"path\":\"demo/$app\",\"image_name\":\"mirror.gcr.io/library/nginx\"}")
  echo "service $app: $c"                                    # 201 新建 / 409 已存在
  for f in "$dir"/envs/*.yaml; do
    env=$(basename "$f" .yaml); group=$(echo "$env" | sed 's/[0-9]*$//')
    code -X POST $A/v1/envs -d "{\"name\":\"$env\",\"group\":\"$group\"}" >/dev/null
    echo "  $app -> $env (group=$group): $(code -X PUT $A/v1/envs/$env/services/$app -d '{"replicas":1}')"
  done
done
