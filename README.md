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
kubectl apply -k config
kubectl create -f config/samples/releasetask.yaml
kubectl -n dev get rt -w        # PHASE / POD 列
```
控制器参数:`--executor-image`(必填,执行器镜像)、`--git-image`、`--default-timeout`、`--namespace`、`--leader-elect`。

## 执行器约定
执行器镜像在 `/workspace/src`(已 checkout 到 `spec.git.ref`)下启动,通过环境变量拿输入:
`TASK_NAME SERVICE_NAME SERVICE_KIND ENVIRONMENT GIT_REPO GIT_REF GITOPS_REPO GITOPS_BRANCH GITOPS_PATH`。
退出码 0 即 Success,非 0 为 Failed(`backoffLimit: 0`,不自动重试)。用户输入只走环境变量,不拼进 shell。

## 设计取舍
- **spec 不可变**(CRD 校验 `self == oldSelf`):重跑 = 新建一个 `ReleaseTask`,保证每个任务和一次执行一一对应。
- **Job 被删不重建**:已创建过 Job 却找不到时直接置 Failed,避免重复发布。
- **终态不再改写**;任务删除时 Job/Pod 由 ownerReference 级联回收,Pod 日志保留到那时。
- 只用 client-go(dynamic + informer),不依赖 controller-runtime 和代码生成。
- 前端只需 `create/get/list/watch` ReleaseTask(见 `okdev-releasetask-editor` ClusterRole),`status` 只有控制器能写。

## 验证情况
单元/集成测试用 fake client 覆盖:状态机各分支、Job 生成、注入安全、完整生命周期、非法 spec、Job 被删。
**尚未在真实集群上验证**(沙箱里没有 kubectl/集群),CRD 与 RBAC 清单只做了 YAML 语法检查。
