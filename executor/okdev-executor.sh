#!/bin/sh
# okdev 执行器:用法 okdev-executor <checkout|prepare|release>
# 三步由控制器放进同一个 Pod,通过 /workspace 共享数据;镜像构建由中间的 kaniko 容器完成
# (kaniko 构建时会改写自己容器的根文件系统,所以不能和本脚本放在同一个容器里)。
#
# 输入(环境变量):
#   TASK_NAME SERVICE_NAME SERVICE_KIND ENVIRONMENT GIT_REPO GIT_REF  必填,由控制器按 ReleaseTask 填写
#   GITOPS_REPO GITOPS_BRANCH GITOPS_PATH                              可选,GITOPS_REPO 为空则只构建不发布
#   REGISTRY_PUSH  构建时推送镜像的仓库地址(必填)
#   REGISTRY_PULL  写进 GitOps 配置、供节点拉取的仓库地址(默认同 REGISTRY_PUSH)
#   DOCKERFILE     源码里 Dockerfile 的相对路径(默认 Dockerfile)
#   BUILD_CONTEXT  构建上下文相对路径(默认 .)
set -eu

WS=/workspace
SRC=$WS/src
CA=/etc/build-ca/build-ca.crt
[ -f "$CA" ] && export GIT_SSL_CAINFO=$CA

log() { echo "[okdev-executor] $*"; }
die() { echo "[okdev-executor] ERROR: $*" >&2; exit 1; }
need() { eval "v=\${$1:-}"; [ -n "$v" ] || die "environment variable $1 is required"; }

# 凭据文件存在时,用 HTTP header 带上,避免 token 出现在 URL 和进程列表里
git_auth() {
  if [ -s /etc/gitops/token ]; then
    b64=$(printf 'x-access-token:%s' "$(cat /etc/gitops/token)" | base64 | tr -d '\n')
    git -c "http.extraHeader=Authorization: Basic $b64" "$@"
  else
    git "$@"
  fi
}

checkout() {
  need GIT_REPO; need GIT_REF
  log "cloning $GIT_REPO @ $GIT_REF"
  git clone --quiet -- "$GIT_REPO" "$SRC"
  cd "$SRC"
  git checkout --quiet "$GIT_REF" --
  git rev-parse HEAD
}

image_tag() { git -C "$SRC" rev-parse --short=12 HEAD; }

prepare() {
  need SERVICE_NAME; need REGISTRY_PUSH
  dockerfile=${DOCKERFILE:-Dockerfile}
  context=${BUILD_CONTEXT:-.}
  [ -f "$SRC/$dockerfile" ] || die "Dockerfile not found in repo: $dockerfile"
  tag=$(image_tag)
  # 仓库地址、服务名里都不会有空白;kaniko 容器按空白切分读取
  cat > $WS/kaniko-args <<ARGS
--context=dir://$SRC/$context
--dockerfile=$SRC/$dockerfile
--destination=$REGISTRY_PUSH/$SERVICE_NAME:$tag
--cache=false
${REGISTRY_INSECURE:+--insecure}
ARGS
  log "will build $REGISTRY_PUSH/$SERVICE_NAME:$tag"
}

release() {
  need SERVICE_NAME; need REGISTRY_PUSH
  tag=$(image_tag)
  image="${REGISTRY_PULL:-$REGISTRY_PUSH}/$SERVICE_NAME:$tag"
  log "image built: $image"
  if [ -z "${GITOPS_REPO:-}" ]; then
    log "no gitops repo configured, skipping deployment config update"
    return 0
  fi
  need GITOPS_PATH
  branch=${GITOPS_BRANCH:-main}
  dir=$WS/gitops
  git_auth clone --quiet --branch "$branch" -- "$GITOPS_REPO" "$dir"
  [ -f "$dir/$GITOPS_PATH/kustomization.yaml" ] || die "no kustomization.yaml at $GITOPS_PATH in gitops repo"
  (cd "$dir/$GITOPS_PATH" && kustomize edit set image "$SERVICE_NAME=$image")
  cd "$dir"
  git add -A
  if git diff --cached --quiet; then
    log "gitops config already points at $image, nothing to commit"
    return 0
  fi
  git -c user.name="${GIT_USER_NAME:-okdev}" -c user.email="${GIT_USER_EMAIL:-okdev@localhost}" \
    commit --quiet -m "release $SERVICE_NAME to $ENVIRONMENT: $tag" -m "task: ${TASK_NAME:-unknown}"
  # 并发发布同一仓库时 push 会被拒,rebase 后重试
  for i in 1 2 3; do
    git_auth push --quiet origin "HEAD:$branch" && { log "pushed $(git rev-parse --short HEAD) to $branch"; return 0; }
    log "push rejected, rebasing (attempt $i)"
    git_auth pull --quiet --rebase origin "$branch"
  done
  die "could not push to $GITOPS_REPO after 3 attempts"
}

case "${1:-}" in
  checkout|prepare|release) "$1" ;;
  *) die "usage: okdev-executor <checkout|prepare|release>" ;;
esac
