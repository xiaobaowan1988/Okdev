#!/bin/bash
# 单节点 kubeadm 安装(Ubuntu 24.04, root, 无 systemd, cgroup v1, 缺少 CAP_SYS_RESOURCE 的沙箱)
set -euo pipefail
DIR=$(cd "$(dirname "$0")/.." && pwd)
K8S_VERSION=${K8S_VERSION:-v1.37.1}
NODE_IP=${NODE_IP:-$(hostname -I | awk '{print $1}')}
export NO_PROXY=${NO_PROXY:-},$NODE_IP,10.96.0.0/12,10.244.0.0/16 no_proxy=${no_proxy:-},$NODE_IP,10.96.0.0/12,10.244.0.0/16

# 1. 二进制与依赖
for b in kubeadm kubelet kubectl; do
  curl -fsSL -o /usr/local/bin/$b https://dl.k8s.io/release/$K8S_VERSION/bin/linux/amd64/$b && chmod +x /usr/local/bin/$b
done
curl -fsSL https://github.com/kubernetes-sigs/cri-tools/releases/download/v1.34.0/crictl-v1.34.0-linux-amd64.tar.gz | tar xz -C /usr/local/bin
mkdir -p /opt/cni/bin
curl -fsSL https://github.com/containernetworking/plugins/releases/download/v1.7.1/cni-plugins-linux-amd64-v1.7.1.tgz | tar xz -C /opt/cni/bin
apt-get update -qq && apt-get install -y -qq conntrack socat ethtool jq
sysctl -w net.bridge.bridge-nf-call-iptables=1 net.ipv4.ip_forward=1 || true

# 2. 独立 containerd(开启 CRI, cgroupfs, runc 包装脚本)
install -m755 "$DIR/scripts/runc-nooom" /usr/local/bin/runc-nooom
mkdir -p /etc/k8s-containerd /var/lib/k8s-containerd /run/k8s-containerd
containerd config default > /etc/k8s-containerd/config.toml
sed -i -e 's#^root = .*#root = "/var/lib/k8s-containerd"#' -e 's#^state = .*#state = "/run/k8s-containerd"#' \
       -e 's#address = .*containerd.sock.*#address = "/run/k8s-containerd/containerd.sock"#' \
       -e "s#^\(\s*\)BinaryName = ''#\1BinaryName = '/usr/local/bin/runc-nooom'#" /etc/k8s-containerd/config.toml
echo 'runtime-endpoint: unix:///run/k8s-containerd/containerd.sock' > /etc/crictl.yaml
nohup containerd --config /etc/k8s-containerd/config.toml > /var/log/k8s-containerd.log 2>&1 &
sleep 3

# 3. kubeadm init(kubelet 由后台脚本拉起)
nohup "$DIR/scripts/run-kubelet.sh" >/dev/null 2>&1 &
kubeadm init --config "$DIR/config/kubeadm.yaml"

# 4. 沙箱里 kubelet 读不到 imagefs 容量,会误报 disk-pressure:关闭驱逐阈值并重启 kubelet
cat >> /var/lib/kubelet/config.yaml <<'EOT'
evictionHard:
  memory.available: "10Mi"
  nodefs.available: "0%"
  nodefs.inodesFree: "0%"
  imagefs.available: "0%"
  imagefs.inodesFree: "0%"
EOT
kill "$(pgrep -x kubelet)"; sleep 3
nohup "$DIR/scripts/run-kubelet.sh" >/dev/null 2>&1 &

# 5. 单节点去污点 + flannel
export KUBECONFIG=/etc/kubernetes/admin.conf
kubectl taint nodes --all node-role.kubernetes.io/control-plane- || true
kubectl apply -f https://github.com/flannel-io/flannel/releases/latest/download/kube-flannel.yml
kubectl get nodes
