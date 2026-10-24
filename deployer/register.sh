#!/bin/bash
# 把 examples/ 里的脚本和 Dockerfile 的内容注册成 builder(内容存进 MySQL,仓库里不放)。
# 用法:A=http://127.0.0.1:18080 ./register.sh   (builder 已存在则更新)
set -euo pipefail
A=${A:-http://127.0.0.1:18080}
R=${REPO:-https://github.com/xiaobaowan1988/argocd-example-apps.git}
cd "$(dirname "$0")"
put() { # 名字 服务 目录 构建镜像 默认profile 示例目录
  body=$(jq -n --arg name "$1" --arg svc "$2" --arg repo "$R" --arg ctx "$3" --arg img "$4" --arg prof "$5" \
    --rawfile df "examples/$6/Dockerfile" --rawfile sc "examples/$6/build.sh" \
    '{name:$name,service:$svc,repo_url:$repo,ref:"master",context_path:$ctx,image:$img,profile:$prof,dockerfile:$df,script:$sc}')
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$A/v1/builders" -d "$body")
  [ "$code" = 409 ] && code=$(curl -s -o /dev/null -w '%{http_code}' -X PUT "$A/v1/builders/$1" -d "$body")
  echo "$1 -> $code"
}
put api-main    api      src/api         mirror.gcr.io/library/golang:1.24-alpine                  prod    api
put java-plain  java-api src/java-plain  mirror.gcr.io/library/eclipse-temurin:21-jdk              default java-plain
put java-maven  java-api src/java-maven  mirror.gcr.io/library/maven:3.9-eclipse-temurin-21        dev     java-maven
