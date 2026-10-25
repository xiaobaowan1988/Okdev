# platform(原 myk8s)

> 这是 okdev 项目的底座层:集群、Argo CD、镜像仓库、deployer。上层的研发控制台(Headlamp 插件、`ReleaseTask` 控制器、登录)在仓库根目录。一次装起整套:`scripts/install-sandbox.sh`(在仓库根目录)。
> 下面所有路径都相对 `platform/`,脚本可以从任意目录运行。

用 kubeadm 在单机(Ubuntu 24.04)上安装 Kubernetes v1.37.1:containerd + flannel,单节点。

```bash
sudo ./scripts/install.sh
export KUBECONFIG=/etc/kubernetes/admin.conf
export NO_PROXY=$NO_PROXY,<节点IP>   # 有 HTTPS 代理时,否则 kubectl 会超时
kubectl get pods -A
```

## 脚本一览(`scripts/`)
| 脚本 | 作用 |
|---|---|
| `install.sh` / `restart-cluster.sh` | 装单节点集群 / 重启后拉起进程(无 systemd) |
| `prepull-sandbox-images.sh` | 沙箱限流(429)时,把 Argo CD 的 redis 镜像从 mirror.gcr.io 拉下来并打成清单里的名字 |
| `install-argocd.sh`、`deploy-apps.sh` | Argo CD、AppProject 和仓库登记 |
| `deploy-mysql.sh`、`deploy-build.sh`、`build-deployer.sh` | deployer 的 MySQL、构建命名空间和镜像仓库、编译并导入 deployer |
| `register-demo.sh` | 在 deployer 里登记演示数据(环境、服务、构建模板、构建器、流水线),可重复执行 |
| `enable-oidc.sh` | 让 kube-apiserver 信任 Dex(OIDC);改静态 Pod 清单,可重复执行,会备份原清单 |

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
├── argocd/base/               官方 install.yaml(v3.5.6),不做任何定制
├── argocd/overlays/sandbox/   沙箱专用:repo-server 信任出口网关 CA
├── apps/                      Application 的共用底座,**只有一份、与环境无关**:AppProject apps、仓库登记
└── deployer/                  MySQL + deployer 程序(见下),应用到环境的映射都在这里
```
```bash
./scripts/install-argocd.sh     # 检测到 /root/.ccr/ca-bundle.crt 时自动用 sandbox overlay
./scripts/deploy-apps.sh        # 共用底座:AppProject apps、仓库登记
kubectl -n argocd port-forward svc/argocd-server 8080:443   # https://localhost:8080, 用户 admin
```
`argocd-server` 保持 ClusterIP,通过 port-forward 访问。

要点:
- AppProject 默认不允许任何源仓库,必须写 `sourceRepos`;集群级资源只白名单放行 `Namespace`
  (`CreateNamespace=true` 需要),其余默认禁止。
- 沙箱里 Pod 访问 GitHub 经过出口网关 CA(与宿主机代理 CA 不同),所以把 CA bundle 覆盖挂载为
  repo-server 的系统信任库。**不要**往 `argocd-tls-certs-cm` 放 `github.com` 条目,否则 Argo CD
  对该主机只认那里的证书,仍会报 `unknown authority`。`ca-bundle.crt` 由脚本复制,已 gitignore。
- 命名空间由 Application 的 `CreateNamespace=true` 创建,**不属于应用管理**;Application 带
  `resources-finalizer`,下线时它部署的资源级联删除,但命名空间保留(空的),需要时手动删除。
  (曾试过把 Namespace 清单放进应用目录让它随应用删除,但会连命名空间里所有内容一起删,已放弃。)
- 机器重启后进程不会自动恢复(无 systemd),运行 `./scripts/restart-cluster.sh` 即可,数据不丢。
- 镜像在共享出口 IP 下可能被 Docker Hub / public.ecr.aws 限流(429),可改用 `mirror.gcr.io/library/...`,
  或手动 `crictl pull <镜像>` 后删除对应 Pod。

## 应用到环境的映射:deployer(MySQL 存期望状态)
代码仓库(fork `xiaobaowan1988/argocd-example-apps` 的 `demo/`)里**每个应用只有一份清单,不区分环境**。
"哪个应用部署到哪些环境、副本数多少、镜像是什么"存在 MySQL,Go 程序 deployer 对账后创建 Argo CD Application。
环境是数据(几行记录),不是文件:新增环境、把应用推广/迁移到别的环境都是一次 API 调用,不用改仓库。
详见 [deployer/README.md](deployer/README.md)(设计、API、实测结果、发现的问题、局限)。
**加一个服务**:放一份 kustomize 清单 → `POST /v1/services` 登记 → `PUT /v1/envs/{env}/services/{svc}` 部署到环境,步骤和演示见 [deployer/README.md 的「添加一个服务」](deployer/README.md)。
**构建与流水线**:builder(关联服务)描述怎么构建镜像,pipeline 运行时选 builder 和 env,构建完成后自动部署到所选环境(kaniko 在集群里构建,镜像推到集群内仓库),见 [deployer/README.md 的「构建与流水线」](deployer/README.md)。
```bash
./scripts/deploy-mysql.sh && ./scripts/deploy-build.sh && ./scripts/build-deployer.sh && kubectl apply -k manifests/deployer/app
```

**演进过程**:最初用 ApplicationSet + 每个应用每个环境一个标记文件(`envs/<环境>.yaml`)。能用,但环境一多
文件就按"应用 × 环境"增长,所以迁到了 deployer;标记文件、ApplicationSet 和导入脚本都已删除(需要时从 git 历史取回)。

## Kustomize
```bash
./scripts/install-kustomize.sh              # 默认 v5.8.1,可用 KUSTOMIZE_VERSION 覆盖
```
