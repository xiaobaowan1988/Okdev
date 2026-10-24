#!/bin/bash
# 把 examples/ 里的脚本和 Dockerfile 的内容登记成构建模板(内容存进 MySQL,仓库里不放),再让 builder 引用模板。
# 多个服务可以引用同一个模板:改模板,它们下次构建都用新内容。
# 用法:A=http://127.0.0.1:18080 ./register.sh   (已存在则更新)
set -euo pipefail
A=${A:-http://127.0.0.1:18080}
R=${REPO:-https://github.com/xiaobaowan1988/argocd-example-apps.git}
cd "$(dirname "$0")"
upsert() { # 资源路径(templates|builders) 名字 JSON
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$A/v1/$1" -d "$3")
  [ "$code" = 409 ] && code=$(curl -s -o /dev/null -w '%{http_code}' -X PUT "$A/v1/$1/$2" -d "$3")
  echo "$1/$2 -> $code"
}
template() { # 名字 构建环境镜像 默认profile 示例目录
  upsert templates "$1" "$(jq -n --arg name "$1" --arg img "$2" --arg prof "$3" \
    --rawfile df "examples/$4/Dockerfile" --rawfile sc "examples/$4/build.sh" \
    '{name:$name,image:$img,profile:$prof,dockerfile:$df,script:$sc}')"
}
builder() { # 名字 服务 构建目录 模板 [profile覆盖]
  upsert builders "$1" "$(jq -n --arg name "$1" --arg svc "$2" --arg repo "$R" --arg ctx "$3" --arg tpl "$4" --arg prof "${5:-}" \
    '{name:$name,service:$svc,repo_url:$repo,ref:"master",context_path:$ctx,template:$tpl,profile:$prof}')"
}
template go-template         mirror.gcr.io/library/golang:1.24-alpine           prod    api
template java-plain-template mirror.gcr.io/library/eclipse-temurin:21-jdk       default java-plain
template java-maven-template mirror.gcr.io/library/maven:3.9-eclipse-temurin-21 dev     java-maven
builder api-main   api      src/api         go-template
builder java-plain java-api src/java-plain  java-plain-template
builder java-maven java-api src/java-maven  java-maven-template
