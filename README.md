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
├── apps/common/               两个环境共用的仓库登记 Secret
├── apps/dev/                  argocd-apps-dev、project-dev(只能发往 dev-env-*)、frontend
├── apps/test/                 argocd-apps-test、project-test(只能发往 test-env-*)、frontend
└── appsets/{dev,test}/        演示:每个环境一个 ApplicationSet,按目录批量生成 Application
```
```bash
./scripts/install-argocd.sh     # 检测到 /root/.ccr/ca-bundle.crt 时自动用 sandbox overlay
./scripts/deploy-apps.sh dev    # 或 test:部署共享仓库登记 + 该环境的 AppProject + Application
./scripts/deploy-appsets.sh     # 演示 ApplicationSet:每个环境生成 5 个 Application
kubectl get appset,app -A
kubectl -n argocd port-forward svc/argocd-server 8080:443   # https://localhost:8080, 用户 admin
```
`argocd-server` 保持 ClusterIP,通过 port-forward 访问。

要点:
- AppProject 默认不允许任何源仓库,必须写 `sourceRepos`;集群级资源只白名单放行 `Namespace`
  (`CreateNamespace=true` 需要),其余默认禁止。
- 沙箱里 Pod 访问 GitHub 经过出口网关 CA(与宿主机代理 CA 不同),所以把 CA bundle 覆盖挂载为
  repo-server 的系统信任库。**不要**往 `argocd-tls-certs-cm` 放 `github.com` 条目,否则 Argo CD
  对该主机只认那里的证书,仍会报 `unknown authority`。`ca-bundle.crt` 由脚本复制,已 gitignore。
- **ApplicationSet 生成的 Application 与 ApplicationSet 在同一命名空间**,`template.metadata.namespace`
  不会改变落点(会静默落到 ApplicationSet 所在命名空间)。所以每个环境一个 ApplicationSet,放在
  `argocd-apps-<env>` 里,绑定 `project-<env>`。要让 ApplicationSet 跨命名空间,需要
  `applicationsetcontroller.namespaces`,并关闭 SCM provider 生成器(或配置 allowed.scm.providers),
  否则控制器启动失败。
- 仓库地址指向 fork `xiaobaowan1988/argocd-example-apps`(`HEAD` = master)。ApplicationSet 扫描
  `demo/*`,每个子目录是一个 kustomize 应用,**自带 `namespace.yaml`**(占位名 `placeholder`)。
  模板通过 `source.kustomize.namespace: <env>-env-<应用名>` 把它改成实际命名空间(kustomize 的
  namespace 转换会连 Namespace 资源一起改名),并且去掉了 `CreateNamespace=true`,
  所以 Namespace 是 Application 管理的资源,配合 `resources-finalizer.argocd.argoproj.io`,
  删除应用目录时 Application、资源、命名空间会一起清理。
- **按环境选择性部署**:应用目录下的 `envs/<env>.yaml` 是部署标记。dev 的 ApplicationSet 用
  `files: demo/*/envs/dev.yaml`,test 用 `demo/*/envs/test.yaml`,有标记文件才会部署到该环境,
  删文件即从该环境下线(Application、资源、命名空间一并清理)。例如 `app-b` 只有 `envs/dev.yaml`,
  就只在 dev 调试。files 生成器的 `path.path` 是标记文件所在目录(`demo/<应用>/envs`),
  模板里用 `{{ dir .path.path }}` / `{{ base (dir .path.path) }}` 取应用目录和名字。
  已验证:`app-b` 仅在 dev;给它加 `envs/test.yaml` → `app-b-test` 和 `test-env-app-b` 出现;
  删除后消失。标记文件里还可以放该环境的参数,模板用 `{{.参数名}}` 取值。
- 机器重启后进程不会自动恢复(无 systemd),运行 `./scripts/restart-cluster.sh` 即可,数据不丢。
- 早先的验证:往 `demo/` 加 `app-d` → dev/test 各自动出现 Application 和命名空间;删目录 → 全部消失。
  `frontend`(手写 Application)仍用 `CreateNamespace=true`,不受影响。
  ApplicationSet 默认每 3 分钟轮询一次,可用 `argocd.argoproj.io/application-set-refresh=true` 注解立即刷新。
- 规模化:往生成器的 `directories` 加路径(或让仓库目录约定自动被 `apps/*` 之类的通配匹配),
  就会多出对应 Application;删目录则自动清理。
- 镜像在共享出口 IP 下可能被 Docker Hub / public.ecr.aws 限流(429),
  出现 ImagePullBackOff 时手动 `crictl pull <镜像>` 后删除对应 Pod 即可。

## Kustomize
```bash
./scripts/install-kustomize.sh              # 默认 v5.8.1,可用 KUSTOMIZE_VERSION 覆盖
```
