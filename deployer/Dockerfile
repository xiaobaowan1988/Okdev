# 二进制由 scripts/build-deployer.sh 在宿主机上静态编译后拷入,镜像里只有这一个文件
FROM scratch
COPY deployer /deployer
USER 65532:65532
ENTRYPOINT ["/deployer"]
