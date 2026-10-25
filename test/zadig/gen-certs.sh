#!/bin/bash
# 沙箱用:生成自签 CA、带 IP SAN 的 Dex 服务端证书,以及 Dex 管理接口(gRPC)的服务端证书和管理员客户端证书。
# 用法: gen-certs.sh <输出目录> <节点IP>
# 生产环境:管理员客户端证书应由单独的 CA 签发(只有这个 CA 签发的证书能调用管理接口),并只发给管理员。
set -euo pipefail
out=$1; ip=$2; mkdir -p "$out"; cd "$out"
# 已有 CA 就复用(kube-apiserver 和 Headlamp 已经信任它)
[ -f ca.crt ] && [ -f ca.key ] || openssl req -x509 -newkey rsa:2048 -nodes -days 30 -subj "/CN=okdev-dex-test-ca" -keyout ca.key -out ca.crt 2>/dev/null
sign() { # sign <name> <CN> <san> <eku>
  openssl req -newkey rsa:2048 -nodes -subj "/CN=$2" -keyout "$1.key" -out "$1.csr" 2>/dev/null
  printf "subjectAltName=%s\nextendedKeyUsage=%s\n" "$3" "$4" > "$1.ext"
  openssl x509 -req -in "$1.csr" -CA ca.crt -CAkey ca.key -CAcreateserial -days 30 -extfile "$1.ext" -out "$1.crt" 2>/dev/null
}
sign tls dex "IP:$ip" serverAuth                                       # OIDC(https)
sign grpc dex "DNS:dex,DNS:localhost,IP:127.0.0.1" serverAuth          # 管理接口服务端
sign admin okdev-admin "DNS:okdev-admin" clientAuth                    # 管理员客户端
echo "certs in $out"
