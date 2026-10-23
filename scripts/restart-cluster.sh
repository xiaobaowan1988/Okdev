#!/bin/bash
# 机器/容器重启后,把已安装的单节点集群重新拉起(没有 systemd,进程不会自动恢复;磁盘上的数据和配置不受影响)
set -euo pipefail
DIR=$(cd "$(dirname "$0")/.." && pwd)
sysctl -w net.bridge.bridge-nf-call-iptables=1 net.ipv4.ip_forward=1 >/dev/null || true
mkdir -p /run/k8s-containerd
# 后台进程必须彻底脱离当前 shell 的输出,否则脚本被管道(如 | tail)调用时不会返回
if ! pgrep -f '^containerd --config /etc/k8s-containerd' >/dev/null; then
  setsid nohup containerd --config /etc/k8s-containerd/config.toml >/var/log/k8s-containerd.log 2>&1 </dev/null &
fi
until crictl version >/dev/null 2>&1; do sleep 1; done
NODE_IP=${NODE_IP:-$(hostname -I | awk '{print $1}')}
export NO_PROXY=${NO_PROXY:-},$NODE_IP no_proxy=${no_proxy:-},$NODE_IP
if ! pgrep -x kubelet >/dev/null; then
  setsid nohup "$DIR/scripts/run-kubelet.sh" >/dev/null 2>&1 </dev/null &
fi
export KUBECONFIG=/etc/kubernetes/admin.conf
until kubectl get nodes >/dev/null 2>&1; do sleep 2; done
kubectl get nodes
