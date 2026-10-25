# okdev

Headlamp 开发者控制台的后端:`ReleaseTask` CRD + Go 控制器(设计见架构文档第 3.2 节)。

研发在前端创建 `ReleaseTask`,控制器为它拉起一个 K8s `Job`(init 容器克隆代码,主容器执行构建/GitOps 更新),
并把 `phase` 和 `executorPodName` 写回 `status`,前端据此展示进度条并读取该 Pod 的日志。

```
ReleaseTask 创建 ──> Controller 创建 Job(ownerReference 指向任务)
                          │ Job / Pod 事件
                          ▼
         status.phase: Pending -> Running -> Success | Failed
         status.executorPodName / jobName / startTime / completionTime / message
         Events: JobCreated / Running / Succeeded / Failed / InvalidSpec / JobLost
```

## 布局
- `internal/api/v1`:类型、GVR、spec 校验(`ci.okdev.io/v1`,改组名只需改常量和 CRD)
- `internal/controller`:`BuildJob`(生成 Job)、`ComputeStatus`(纯函数,算阶段)、`Controller`(informer + 工作队列 + 调谐)
- `config/`:CRD、RBAC、Deployment、示例;`kubectl apply -k config`

## 运行
```bash
go test ./...
kubectl apply -k config   # 沙箱见下文 deploy/sandbox
kubectl create -f config/samples/releasetask.yaml
kubectl -n dev get rt -w        # PHASE / POD 列
```
控制器参数:`--executor-image`(必填)、`--default-timeout`、`--namespace`、`--leader-elect`、`--kaniko-image`、`--executor-env`、`--ca-configmap`、`--gitops-secret`。

## 执行器
`executor/` 是一个带 git + kustomize 的小镜像,入口 `okdev-executor <checkout|prepare|release>`。控制器把一个任务编排成一个 Pod 里的四步:

| 步骤 | 类型 | 镜像 | 做什么 |
|---|---|---|---|
| `checkout` | init | executor | clone 并 checkout 到 `spec.git.ref` |
| `prepare` | init | executor | 校验 Dockerfile,用提交短 SHA 作镜像 tag,写出 kaniko 参数 |
| `build` | init | kaniko(debug 版) | 构建并推送到 `REGISTRY_PUSH` |
| `release` | 主容器 | executor | clone GitOps 仓库,`kustomize edit set image <服务名>=<REGISTRY_PULL>/<服务名>:<sha>`,commit 并 push(冲突自动 rebase 重试) |

kaniko 构建时会改写自己容器的根文件系统,所以必须是独立容器。同一提交重复发布时配置无变化,不产生新 commit。
GitOps 里 kustomization 的 `images` 按**服务名**匹配工作负载里的镜像名。

执行器的输入全部走环境变量:`TASK_NAME SERVICE_NAME SERVICE_KIND ENVIRONMENT GIT_REPO GIT_REF GITOPS_*` 由控制器按任务填,
`REGISTRY_PUSH`(必填)、`REGISTRY_PULL`、`REGISTRY_INSECURE`、`DOCKERFILE`、`BUILD_CONTEXT` 由控制器的 `--executor-env KEY=VALUE` 提供。
推送 GitOps 仓库的凭据放 Secret(key `token`),用 `--gitops-secret` 指定,以 HTTP header 传递。
Pod 不挂载 ServiceAccount token。步骤失败时,`status.message` 取容器日志的最后一行(如 `fatal: invalid reference: nope`)。
`spec.executor.image/command` 只覆盖 `release` 步骤。

## 在 myk8s 集群上验证
```bash
(cd ../myk8s && sudo ./scripts/install.sh && ./scripts/deploy-build.sh)   # 集群 + 集群内 registry
./scripts/build-executor.sh && ./scripts/build-controller.sh               # 构建并导入 containerd
kubectl apply -k deploy/sandbox                                            # 控制器(沙箱参数)
kubectl create ns dev   # 并把 build-ca ConfigMap 复制进 dev(沙箱出口 HTTPS 被中间人时需要)
```
实测见下文"验证情况"。

## 设计取舍
- **spec 不可变**(CRD 校验 `self == oldSelf`):重跑 = 新建一个 `ReleaseTask`,保证每个任务和一次执行一一对应。
- **Job 被删不重建**:已创建过 Job 却找不到时直接置 Failed,避免重复发布。
- **终态不再改写**;任务删除时 Job/Pod 由 ownerReference 级联回收,Pod 日志保留到那时。
- 只用 client-go(dynamic + informer),不依赖 controller-runtime 和代码生成。
- 前端只需 `create/get/list/watch` ReleaseTask(见 `okdev-releasetask-editor` ClusterRole),`status` 只有控制器能写。

## 验证情况
- fake client 测试(`go test -race`):状态机、Job 生成、注入安全、完整生命周期、非法 spec、Job 被删。
- 在 myk8s 单节点集群(k8s v1.37.1)上端到端实测,git 源用宿主机 `git daemon`:
  - 发布成功:`Pending → Running → Success`,`executorPodName` 正确写回;kaniko 推镜像到集群内 registry;GitOps 仓库多一个 commit,`images` 指向 `localhost:30500/hello:<sha>`;按该配置 `kubectl apply -k` 后 Pod 跑起来并输出 `built-by-okdev`。
  - 同一提交再发布:`Success`,GitOps 仓库不产生新 commit。
  - 不存在的 ref:`Failed`,`message` 为 `container checkout exited with code 128: fatal: invalid reference: nope`。
- 实测前发现并修复了两个缓存滞后导致的竞态:informer 缓存里任务/Job 落后于刚写的 status/刚建的 Job 时,会把刚建的 Job 误判为丢失、或在 Job 被删后重复创建。现在判定"丢失"和"创建"前都向 API server 确认。
- 未验证:GitOps 走 HTTPS + token 推送(沙箱只测了 git:// 协议)、Kubernetes 在多副本选主切换下的行为、`spec.timeoutSeconds` 超时路径。
