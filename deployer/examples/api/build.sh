#!/bin/sh
# 构建脚本:在构建环境镜像(golang)里运行,工作目录是仓库里的 src/api。
# 可用环境变量:VERSION(运行编号)、PROFILE、GIT_REF、SERVICE、DEPLOY_ENV。
set -eu
mkdir -p dist
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION} -X main.profile=${PROFILE:-default}" -o dist/api .
ls -l dist/api
