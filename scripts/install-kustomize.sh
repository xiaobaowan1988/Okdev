#!/bin/bash
# 安装独立的 kustomize 二进制(默认 v5.8.1,与 kubectl 内置版本一致)
set -euo pipefail
V=${KUSTOMIZE_VERSION:-v5.8.1}
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
curl -fsSL -o "$TMP/k.tgz" "https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize%2F$V/kustomize_${V}_linux_amd64.tar.gz"
tar xzf "$TMP/k.tgz" -C "$TMP" kustomize
install -m755 "$TMP/kustomize" /usr/local/bin/kustomize
kustomize version
