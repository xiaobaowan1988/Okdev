# 基于带 git 的最小镜像;kustomize 二进制由 scripts/build-executor.sh 预先下载到构建上下文
FROM mirror.gcr.io/alpine/git:latest
COPY kustomize /usr/local/bin/kustomize
COPY okdev-executor.sh /usr/local/bin/okdev-executor
ENTRYPOINT ["okdev-executor"]
