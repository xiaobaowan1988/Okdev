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

## Argo CD(kustomize 清单)
```
manifests/
├── argocd/base/               官方 install.yaml(v3.5.6)+ 开启 Apps in Any Namespace
├── argocd/overlays/sandbox/   沙箱专用:repo-server 信任出口网关 CA
└── apps/dev/                  argocd-apps-dev 命名空间、AppProject、仓库登记、Application
```
```bash
./scripts/install-argocd.sh     # 检测到 /root/.ccr/ca-bundle.crt 时自动用 sandbox overlay
./scripts/deploy-apps.sh dev    # 部署 AppProject + Application
kubectl get app -n argocd-apps-dev
kubectl -n argocd port-forward svc/argocd-server 8080:443   # https://localhost:8080, 用户 admin
```
`argocd-server` 保持 ClusterIP,通过 port-forward 访问。

要点:
- AppProject 默认不允许任何源仓库,必须写 `sourceRepos`;集群级资源只白名单放行 `Namespace`
  (`CreateNamespace=true` 需要),其余默认禁止。
- 沙箱里 Pod 访问 GitHub 经过出口网关 CA(与宿主机代理 CA 不同),所以把 CA bundle 覆盖挂载为
  repo-server 的系统信任库。**不要**往 `argocd-tls-certs-cm` 放 `github.com` 条目,否则 Argo CD
  对该主机只认那里的证书,仍会报 `unknown authority`。`ca-bundle.crt` 由脚本复制,已 gitignore。
- 镜像在共享出口 IP 下可能被 Docker Hub / public.ecr.aws 限流(429),
  出现 ImagePullBackOff 时手动 `crictl pull <镜像>` 后删除对应 Pod 即可。

## Kustomize
```bash
./scripts/install-kustomize.sh              # 默认 v5.8.1,可用 KUSTOMIZE_VERSION 覆盖
```
