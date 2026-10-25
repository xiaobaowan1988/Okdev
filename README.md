# okdev

研发控制台:Headlamp 插件 + `ReleaseTask` CRD 控制器 + 登录(Dex)。**构建、部署、环境、历史都由 [myk8s](../myk8s) 的 deployer 负责**,
okdev 只是它面向开发的、K8s 原生的入口。

```
研发 ─> Headlamp(Environment / Workflow 两个页面)
          │ 创建 ReleaseTask(命名空间 okdev-releases;RBAC 管谁能发布)
          ▼
   okdev 控制器 ──提交流水线运行──> deployer(MySQL 存 环境×服务 的期望状态)──> Argo CD ──> 集群
          ▲  轮询运行状态(Pending→Running→Success/Failed)         │
          └──────────── 状态、镜像、失败原因、运行号写回 status ◀──┘
   Headlamp 读环境/构建器/分支/日志 ──经 API Server 服务代理──> 控制器只读接口 ──> deployer
```

## 为什么不自己部署
十几个环境、几百个服务,"每个环境一个目录/一份清单"是几千份文件。deployer 的做法是:每个服务只有**一份**清单,
"哪个服务部署到哪些环境、副本数、镜像"是 MySQL 里的几行数据。okdev 不重复这套,只做入口。

## 布局
- `cmd/controller`、`internal/controller`:`ReleaseTask` 控制器。提交运行、轮询状态、写回 `status`、发 Events。
- `internal/deployer`:deployer 的 HTTP 客户端。
- `internal/gateway`:控制器给界面用的**只读**接口(环境、构建器、分支/Tag、环境里的服务、运行日志),只暴露界面需要的字段。
- `internal/gitrefs`:列仓库的分支/Tag(go-git 的 `ls-remote`,30 秒缓存);仓库地址只来自 deployer 里登记的构建器,调用方不能指定任意 URL。
- `headlamp-plugin/okdev-console`:Headlamp 插件(英文界面)。`headlamp-plugin/e2e`:Playwright 端到端脚本。
- `cmd/zadig-export`、`cmd/dex-users`、`deploy/dex`、`deploy/headlamp-dex`:登录,见 [docs/zadig-accounts-dex.md](docs/zadig-accounts-dex.md);Google 登录见 [docs/google-auth.md](docs/google-auth.md)。
- `config/`:CRD、RBAC、控制器;`deploy/`:沙箱和 Headlamp/Dex 的部署;`test/zadig`:沙箱造数据和证书。

## ReleaseTask
```yaml
apiVersion: ci.okdev.io/v1
kind: ReleaseTask
metadata: {generateName: api-, namespace: okdev-releases}
spec:
  service: api            # deployer 里登记的服务
  builder: api-main       # 关联该服务的构建器
  environment: dev3       # deployer 里的环境(不是 K8s 命名空间)
  ref: master             # 分支或 Tag;留空用构建器默认值
```
- **spec 不可变**:重跑 = 新建一个 `ReleaseTask`,一个任务对应一次 deployer 运行。
- 状态:`phase`(Pending/Running/Success/Failed)、`stage`(deployer 的原始状态:pending/building/deploying/succeeded/failed/cancelled)、
  `runID`、`image`、`message`(失败原因,直接取自 deployer)、起止时间。取消的运行显示为 Failed。
- **不会重复提交**:先写"提交中"标记再提交。提交结果不明(请求可能已到达 deployer 时出错、或提交过程中控制器重启)时,任务直接置 Failed 并写明原因,
  让人去 deployer 里确认,不自动重试。deployer 不可达时不写标记、不失败,等它恢复后再提交。
- 状态以 deployer 为准:控制器重启不影响进行中的运行,恢复后继续轮询。
- 删除 `ReleaseTask` **不会取消**deployer 里的运行。
- 控制器只监听 `okdev-releases` 命名空间(`--namespace`),其他命名空间里的任务不处理。

## 界面(Headlamp 插件)
侧边栏只保留两项,其余原生菜单隐藏(路由还在,从环境页点进去仍能用原生详情、日志、终端;隐藏不是权限控制)。
- **Environment**:环境下拉来自 deployer;表格是该环境里的服务(镜像、副本数、Argo CD 同步/健康状态,每 5 秒刷新)。
  服务名跳到原生工作负载页。**每个(环境,服务)在自己的命名空间里:`<环境>-env-<服务>`**,不是"环境 = 命名空间"。
- **Workflow**:选环境,**多选服务**;每个服务一行:构建器(多个时可选)、仓库(只读)、**分支/Tag 下拉**(实时来自仓库,默认取构建器的默认分支)。
  没有自由输入,不能发布不存在的分支。点 Run 为每个服务各创建一个 `ReleaseTask`;只选一个服务时直接进详情页,选多个时留在本页,下方列表看进度;
  个别服务创建失败时,成功的移出表单,失败的保留并显示原因。
- **任务详情** `/workflows/okdev-releases/<name>`:状态条、阶段、构建器/分支/镜像/运行号、Events 时间线、构建日志
  (来自 deployer:构建中是实时日志,结束后是保存的完整日志;未结束每 3 秒刷新)。
- 侧边栏底部的 "Create" 按钮也隐藏了(无官方开关,用 `MutationObserver`,展开和收起两种形态都处理)。

## 权限
- 控制器:只在 `okdev-releases` 里读写 `ReleaseTask` 和 `status`、写 Events;**不需要 Job/Pod/Secret 权限**(构建部署由 deployer 做)。
- 研发(`config/rbac/developer.yaml`,绑定示例见 `deploy/dex-sandbox/developers.yaml`):
  - `okdev-release-submitter`(Role,仅 `okdev-releases`):创建/查看/删除发布任务和事件;
  - `okdev-developer`(ClusterRole):列命名空间、进容器终端;
  - `okdev-console-reader`(Role,仅 `okdev-system`):读控制器只读接口;
  - 再加内置 `view`(只读工作负载、Pod、日志,不含 Secret)。
- **没有环境级授权**:谁能创建 `ReleaseTask`,就能发布到 deployer 里的任意环境(包括生产)。deployer 自己也没有按用户鉴权(只有可选的 `API_TOKEN` 和 `X-Actor`)。
  需要"某人只能发某些环境"时,要在 `ReleaseTask` 上加准入策略(例如 ValidatingAdmissionPolicy 按用户限制 `spec.environment`),**目前没做**。
- **操作人**:deployer 历史里的操作人是控制器(`--actor`),不是真人;真人在 `ReleaseTask` 的创建者和 K8s 审计日志里。
- 控制器的只读接口读 deployer 用的是控制器自己的令牌,所以能访问该接口的人可以看到 deployer 里所有环境和服务,不受环境限制。

## 运行(沙箱,myk8s 集群)
```bash
cd ../myk8s
sudo ./scripts/install.sh
./scripts/install-argocd.sh && ./scripts/deploy-apps.sh && ./scripts/deploy-mysql.sh && ./scripts/deploy-build.sh
./scripts/build-deployer.sh && kubectl apply -k manifests/deployer/app          # deployer + 构建/仓库
# 在 deployer 里登记环境、服务、构建模板、构建器、流水线(见 myk8s/deployer/README.md)
cd ../okdev
./scripts/build-controller.sh && kubectl apply -k deploy/sandbox                  # 控制器
kubectl create -f config/samples/releasetask.yaml
kubectl -n okdev-releases get rt -w
```
`deploy/sandbox` 里的沙箱设置:控制器镜像用本地导入的,并信任沙箱出口网关的 CA(ConfigMap `build-ca` 复制到 `okdev-system`)。
Headlamp:`scripts/deploy-headlamp.sh`(`HEADLAMP_OVERLAY` 选登录方式)。登录:默认粘贴 ServiceAccount token;Zadig 账号和 Google 登录见 docs。

控制器参数:`--deployer-url`、`--deployer-token-file`、`--default-pipeline`(`build-and-deploy`)、`--poll-interval`、`--actor`、`--namespace`、
`--http-addr`、`--git-token-file`、`--leader-elect`。

## 验证情况
单元测试(`go test ./cmd/... ./internal/...`):状态映射、提交流程、不重复提交的各种情形、deployer 不可达、网关不泄露构建脚本、分支列表缓存。
**沙箱端到端**(myk8s 集群 + deployer + Argo CD,Dex 登录,Playwright):
- Environment 页显示 deployer 里的服务和 Argo CD 状态,服务名跳到 `…/deployments/dev3-env-api/api`;
- Workflow 页多选 `api` 和 `api-b`,下拉里是真实仓库的分支和 Tag,Run 后两个任务都 Success,集群里 `dev5-env-api`(2 副本)和 `dev5-env-api-b` 起来了;
- 单服务运行进详情页:状态 Running→Success,Events(Submitted/Running/Succeeded),构建日志是 kaniko 的输出;
- 不存在的分支(用 kubectl 创建):任务 Failed,message 是 `build failed at step "checkout" (exit code 128) — fatal: couldn't find remote ref …`;
- 运行进行中删除控制器 Pod:任务继续,最终 Success,deployer 里没有多出运行;
- 权限:alice(已授权)能读接口、能在 `okdev-releases` 创建任务、不能写 status;bob(只登录)全部被拒。
**没验证的**:ReleaseTask 因 deployer 超时失败的路径(只有 deployer 单测);Argo CD 在几千个 Application 下的表现;
deployer 开启 `API_TOKEN` 后控制器带令牌访问(代码有,没实测);多副本控制器;窄屏下的布局。
