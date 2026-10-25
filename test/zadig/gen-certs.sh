#!/bin/bash
# 沙箱用:生成自签 CA 和带 IP SAN 的 Dex 服务端证书。用法: gen-certs.sh <输出目录> <节点IP>
set -euo pipefail
out=$1; ip=$2; mkdir -p "$out"; cd "$out"
openssl req -x509 -newkey rsa:2048 -nodes -days 30 -subj "/CN=okdev-dex-test-ca" -keyout ca.key -out ca.crt 2>/dev/null
openssl req -newkey rsa:2048 -nodes -subj "/CN=dex" -keyout tls.key -out tls.csr 2>/dev/null
printf "subjectAltName=IP:%s\nextendedKeyUsage=serverAuth\n" "$ip" > san.ext
openssl x509 -req -in tls.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 30 -extfile san.ext -out tls.crt 2>/dev/null
echo "certs in $out"
