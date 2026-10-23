# myk8s

用 kubeadm 在单机(Ubuntu 24.04)上安装 Kubernetes v1.37.1:containerd + flannel,单节点。

```bash
sudo ./scripts/install.sh
export KUBECONFIG=/etc/kubernetes/admin.conf
export NO_PROXY=$NO_PROXY,<节点IP>   # 有 HTTPS 代理时,否则 kubectl 会超时
kubectl get pods -A
```

## 针对受限沙箱的处理
- **无 systemd**:containerd、kubelet 手动后台启动(`scripts/run-kubelet.sh`),日志在 `/var/log`。
- **cgroup v1**:KubeletConfiguration 设 `failCgroupV1: false`、`cgroupDriver: cgroupfs`。
- **缺 `CAP_SYS_RESOURCE`**:kubelet 设负 `oom_score_adj` 会让 runc 失败,`scripts/runc-nooom`
  在创建容器前删掉该字段(代价是失去 OOM 保护)。
- **误报 disk-pressure**:kubelet 读不到 imagefs 容量,安装脚本把驱逐阈值调为 0。

在有 systemd、cgroup v2 的正常主机上不需要后三项处理。
