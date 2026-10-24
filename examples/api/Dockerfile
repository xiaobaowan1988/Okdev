# 二进制由 scripts/build-example-api.sh 在宿主机上静态编译后拷入
FROM scratch
COPY api /api
USER 65532:65532
ENTRYPOINT ["/api"]
