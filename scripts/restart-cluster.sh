#!/bin/bash
# 机器/容器重启后,把已安装的单节点集群重新拉起(没有 systemd,进程不会自动恢复;磁盘上的数据和配置不受影响)
set -euo pipefail
DIR=$(cd "$(dirname "$0")/.." && pwd)
sysctl -w net.bridge.bridge-nf-call-iptables=1 net.ipv4.ip_forward=1 >/dev/null || true
mkdir -p /run/k8s-containerd
pgrep -f '^containerd --config /etc/k8s-containerd' >/dev/null || \
  nohup containerd --config /etc/k8s-containerd/config.toml > /var/log/k8s-containerd.log 2>&1 &
until crictl version >/dev/null 2>&1; do sleep 1; done
NODE_IP=${NODE_IP:-$(hostname -I | awk '{print $1}')}
export NO_PROXY=${NO_PROXY:-},$NODE_IP no_proxy=${no_proxy:-},$NODE_IP
pgrep -x kubelet >/dev/null || nohup "$DIR/scripts/run-kubelet.sh" >/dev/null 2>&1 &
export KUBECONFIG=/etc/kubernetes/admin.conf
until kubectl get nodes >/dev/null 2>&1; do sleep 2; done
kubectl get nodes
