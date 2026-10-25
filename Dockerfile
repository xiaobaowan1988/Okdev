# 二进制由 scripts/build-controller.sh 在宿主机编译(沙箱容器内无法 go mod download)
FROM gcr.io/distroless/static:nonroot
COPY controller /controller
ENTRYPOINT ["/controller"]
