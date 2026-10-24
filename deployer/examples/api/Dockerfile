# 单阶段:二进制由构建脚本(ci/build.sh)先编译好放在 dist/,这里只负责打镜像。
FROM scratch
COPY dist/api /api
USER 65532:65532
ENTRYPOINT ["/api"]
