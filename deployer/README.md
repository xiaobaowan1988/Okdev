# deployer(原型)

用 **MySQL 存"哪个服务部署到哪个环境、副本数多少"**,Go 程序的对账循环把它翻译成 Argo CD 的
`Application`,由 Argo CD 负责同步、漂移修复、级联清理。设计参考 Zadig(环境是一等公民,期望状态在数据库里),
但执行层交给 Argo CD,不自己写部署引擎。

```
 HTTP API ──写入──▶ MySQL(期望状态 + 历史) ◀──读/回写观测状态── 对账循环 ──创建/更新/删除──▶ Application ──▶ Argo CD ──▶ 集群
```

## 数据模型(`store.go`)
| 表 | 作用 |
|---|---|
| `environment` | 环境(名称唯一),就是几行记录 |
| `service` | 服务:仓库、路径、要调副本数的 Deployment 名、镜像名 |
| `env_service` | 服务在环境里的部署:期望状态(`desired_state`/`replicas`/`image`)、`version`(乐观锁)、`applied_version`、观测状态(`observed_*`);(环境,服务) 唯一 |
| `builder` | 构建器:**关联一个服务**(外键),描述怎么构建它的镜像——源码仓库、分支、构建目录、Dockerfile、构建参数 |
| `pipeline` | 流水线:命名的步骤序列,`["build"]` 或 `["build","deploy"]`(默认后者) |
| `pipeline_run` | 流水线运行:运行时选 builder 和 env;状态机(`pending→building→deploying→succeeded/failed/cancelled`)持久化在这里 |
| `deploy_history` | 每次变更的前后快照与操作人;按名字记录、无外键,部署被清理后审计仍在 |

原型**没有**做 Zadig 那样的通用 `variables` JSON,只有 `replicas` 和 `image` 两个字段。

## API
```
POST   /v1/envs                                {"name":"dev3"}           # 名字 ≤20,[a-z0-9-]
DELETE /v1/envs/{env}                                                         # 还有部署时 409
POST   /v1/services                            {"name","repo_url","path","revision?","workload?","image_name?"}
PUT    /v1/services/{name}                     {"repo_url","path","revision?","workload?","image_name?"}   # 改服务定义(名字不可改);它所有部署的 version +1 并各写一条 service-update 历史
DELETE /v1/services/{name}                                                                                  # 还有部署时 409
PUT    /v1/envs/{env}/services/{svc}           {"replicas?","image?","expected_version?"}   # 部署/更新(幂等)
POST   /v1/envs/{env}/services/{svc}/scale     {"replicas","expected_version?"}             # 副本数 0..50,必须已存在
DELETE /v1/envs/{env}/services/{svc}                                                         # 下线
GET    /v1/envs/{env}/services                                                               # 期望 + 观测状态
GET    /v1/envs/{env}/services/{svc}/history

POST   /v1/builders                            {"name","service","repo_url","ref?","context_path?","dockerfile?","build_args?"}   # service 必填
GET    /v1/builders[?service=]   GET/PUT/DELETE /v1/builders/{name}                       # 名字和关联的服务不可改;有运行记录的构建器不能删(409)
POST   /v1/pipelines             {"name","description?","steps?"}                           # steps 默认 ["build","deploy"]
GET    /v1/pipelines             GET/PUT/DELETE /v1/pipelines/{name}                      # 有运行记录的流水线不能删(409)
POST   /v1/pipelines/{name}/runs {"builder","env","ref?","replicas?"}                      # 运行流水线(202),选 builder 和 env
GET    /v1/runs[?pipeline=&builder=&env=&status=&limit=]   GET /v1/runs/{id}   GET /v1/runs/{id}/log   POST /v1/runs/{id}/cancel
```
`X-Actor` 头记录操作人;配置了 `API_TOKEN`(Secret `deployer-api`)则要求 `Authorization: Bearer`,**原型默认不开鉴权**。

## 添加一个服务(以 `api` 为例)
不需要写 Application、ApplicationSet,也不需要按环境建文件,三步:**放清单 → 登记服务 → 部署到环境**。

1. **清单**:在 fork 的 `demo/<服务>/` 放一个 **kustomize 目录**(`kustomization.yaml` + Deployment + Service),一份,不分环境。约束:
   必须有 `kustomization.yaml`(副本数/镜像是通过 kustomize 参数传的);工作负载必须是 **Deployment**,名字默认等于服务名(否则登记时给 `workload`);
   只放带命名空间的资源(AppProject 只放行 `Namespace` 这一种集群级资源);清单里写默认镜像和副本数,环境差异由 deployer 覆盖。
2. **登记**:`POST /v1/services`。要按环境覆盖镜像,`image_name` 填清单里的镜像名(不带 tag)。登记错了用 `PUT /v1/services/{name}` 改。
3. **部署**:`PUT /v1/envs/{env}/services/{svc}`,带 `replicas` 和/或 `image`;环境不存在先 `POST /v1/envs`。几秒后看 `GET /v1/envs/{env}/services` 的 `observed_*`。

演示服务 `api`(源码和 Dockerfile 在 fork 的 `src/api`,一个返回版本号和 Pod 名的 Go 服务;清单在 fork 的 `demo/api`)。镜像由下面「构建与流水线」构建,不再手工导入:
```bash
A=http://127.0.0.1:18080
curl -X POST $A/v1/services -d '{"name":"api","repo_url":"https://github.com/xiaobaowan1988/argocd-example-apps.git","path":"demo/api","image_name":"api"}'
curl -X POST $A/v1/envs -d '{"name":"dev3"}'
curl -X PUT  $A/v1/envs/dev3/services/api -d '{"replicas":1,"image":"localhost:30500/api:r1"}'   # 只在 dev3 调试(镜像也可以由流水线自动写入)
curl -X PUT  $A/v1/envs/dev7/services/api -d '{"replicas":0}'                                    # 部署了,但副本数 0
```
**实测**(在本集群,用手工导入的镜像 `api:v1/v2` 时):dev3 响应 `v1`、dev5(2 副本)响应 `v2`、dev7(0 副本)连接被拒;单独把 dev3 的镜像改成 `v2`,滚动升级后它响应 `v2`,不影响其它环境;dev5 扩到 3 副本后 3/3 就绪。
故意把 `path` 写成 `demo/apii`:错误 `app path does not exist` 回显在 `observed_message`,用 `PUT /v1/services/api` 改正后该部署的 `version` 1→2、自动恢复 Synced/Healthy,
历史里有 `create` 和 `service-update` 两条。服务删除:有部署或构建器时 409,先下线再删得 204,再删得 404;改名 400;缺 `repo_url`/`path` 400;不存在的服务 404。

**注意**:
- 服务在别的仓库时,AppProject `apps` 的 `sourceRepos`(`manifests/apps/appproject.yaml`)目前只有 fork 一个地址,要先加上;私有仓库还要登记凭据(现在的 `repo.yaml` 只适用公共仓库)。
- 镜像必须能被集群拉到。可以用「构建与流水线」在集群里构建并推到集群内仓库,也可以用你们自己的镜像仓库。(早期我是把本机构建的镜像直接导入 containerd,实测会被 kubelet 的镜像回收清掉没人用的镜像,不可靠,所以改成了仓库。)
- 命名限制:服务名 ≤30、环境名 ≤20,`[a-z0-9-]`;命名空间 `<环境>-env-<服务>` 总长 ≤63。
- 服务定义一改,它所有环境的部署都会跟着变(期望状态里的仓库/路径/镜像名/工作负载都来自服务定义)。

## 构建与流水线
**三个概念**:`builder` 描述"怎么构建某个服务的镜像"(**必须关联服务**);`pipeline` 是命名的步骤序列(`build`,可选 `deploy`);**运行流水线时选 builder 和 env**——
构建成功后**自动调用 deployer 的部署逻辑**,把新镜像部署到所选环境,等它真正 Healthy 才算成功。

```bash
A=http://127.0.0.1:18080
curl -X POST $A/v1/builders  -d '{"name":"api-main","service":"api","repo_url":"https://github.com/xiaobaowan1988/argocd-example-apps.git","ref":"master","context_path":"src/api"}'
curl -X POST $A/v1/pipelines -d '{"name":"build-and-deploy","description":"构建镜像并部署到所选环境"}'
curl -X POST $A/v1/pipelines/build-and-deploy/runs -d '{"builder":"api-main","env":"dev3"}'                    # 运行:选 builder 和 env
curl -X POST $A/v1/pipelines/build-and-deploy/runs -d '{"builder":"api-main","env":"dev9","replicas":2}'       # 新环境,指定副本数
curl $A/v1/runs/1                  # 状态、镜像、失败原因
curl $A/v1/runs/1/log              # 构建日志
curl -X POST $A/v1/runs/1/cancel   # 取消
```

**一次运行经过的步骤**(状态机持久化在 `pipeline_run`,执行器用 MySQL `GET_LOCK` 选主,多副本只有一个推进):
1. `pending`:提交后排队。**同一个 (环境, 服务) 的运行严格按提交顺序串行**:前面还有没结束的(排队中、构建中、部署中),就继续排队,`message` 里写明 `queued: waiting for run N`;前一个结束(成功、失败或取消)后下一个才开始。不同环境或不同服务之间仍然并行,同时构建的总上限是 `BUILD_CONCURRENCY`(默认 2)。
2. `building`:在 `deployer-build` 命名空间起一个 **kaniko Job**:从 git 拉 `ref`(分支或 `refs/...`)的源码,按 `context_path` 和 `dockerfile` 构建,
   推到集群内仓库 `registry.deployer-build.svc:5000/<服务>:r<运行号>`。执行器**总是注入**构建参数 `VERSION=r<运行号>` 和 `GIT_REF=<ref>`(覆盖 builder 里同名的),
   所以服务可以把版本编进二进制。构建超时 `BUILD_TIMEOUT`(默认 15 分钟)由 Job 的 `activeDeadlineSeconds` 保证。
3. 构建成功:读 Pod 日志存进数据库,删掉 Job,然后进入**部署步骤**:以操作人 `pipeline-run-<id>` 调用与 `PUT /v1/envs/{env}/services/{svc}` 相同的逻辑,
   把该环境里这个服务的镜像设为 `localhost:30500/<服务>:r<运行号>`(副本数沿用现有部署;还没有部署就用运行里指定的 `replicas`,再没有就是 1),状态 `deploying`。
4. `deploying`:等对账循环把这一版**应用并且 Synced/Healthy**(`applied_version >= 写入时的 version` 且镜像等于本次的)才算 `succeeded`。
   超过 `DEPLOY_TIMEOUT`(默认 10 分钟)→ `failed`;期间部署被别的变更改了镜像 → `failed`(superseded);部署被下线 → `failed`。
5. 失败时 `message` 带上构建日志里最后一条 `Error:`,完整日志在 `/v1/runs/{id}/log`。取消会删掉构建 Job;**已经写进环境的部署不回滚**。
- 只含 `["build"]` 的流水线只构建不部署,环境不受影响。镜像 tag 是运行号,不可变。

**镜像仓库**(`manifests/deployer/build`,`scripts/deploy-build.sh`):原型是单实例、无鉴权、明文 HTTP,数据在节点 hostPath。推送和拉取用**不同的地址**:构建任务用集群内
`registry.deployer-build.svc:5000`,节点用 NodePort `localhost:30500`(containerd 的 CRI 对 localhost 允许明文 HTTP,所以不用改 containerd 配置;已验证)。
沙箱里 Pod 出口 HTTPS 被中间人,构建任务需要信任它的 CA:`deploy-build.sh` 在存在 `/root/.ccr/ca-bundle.crt` 时建 ConfigMap `build-ca`,Job 以 optional 方式挂载,没有也不影响。

**配置**(环境变量,都有默认值):`BUILD_NAMESPACE`(`deployer-build`)、`REGISTRY_PUSH_HOST`、`REGISTRY_PULL_HOST`、`KANIKO_IMAGE`(`gcr.io/kaniko-project/executor:v1.23.2`)、
`BUILD_CA_CONFIGMAP`、`BUILD_TIMEOUT`、`DEPLOY_TIMEOUT`、`BUILD_CONCURRENCY`。**权限**:在 `deployer-build` 命名空间里只有管理 Job、读 Pod 和日志,不含 Secret。

**实测**(本集群):
- 运行 `build-and-deploy`(builder `api-main`,env `dev3`):`pending→building→deploying→succeeded` 约 50 秒;dev3 的 Deployment 镜像变成 `localhost:30500/api:r1`,
  访问服务响应 `{"version":"r1"}`(版本号是从源码编译时编进去的);部署历史里操作人是 `pipeline-run-1`;构建日志已保存、构建 Job 已清理。
- 同时提交 3 个运行(并发上限 2):2 个 `building`、1 个 `pending`;结束后每个环境得到各自的镜像:dev5 保持 3 副本、dev7 保持 0 副本(镜像更新但没起 Pod)、新环境 dev9 按指定创建 2 副本。
- 失败路径:不存在的分支 → `Error: error resolving source context: reference not found`;错误的 Dockerfile 路径 → `error resolving dockerfile path`;两次目标环境的镜像都没变,Job 已清理。
- 取消:构建中取消 → Job 被删、状态 `cancelled`,再取消 409。**重启恢复**:构建中把 deployer Pod 删掉,新 Pod 接着把这次运行跑完(`succeeded`)。
- 2 个 deployer 副本:只有一个副本推进了运行。只含 `build` 的流水线:构建了镜像,环境镜像不变。
- 校验:builder 不关联服务/服务不存在/`repo_url` 非 http(s)/非法构建参数名/重名;运行时流水线、builder、env 不存在、缺 env、非法 ref、replicas 越界;
  有运行记录的 builder、pipeline、env 不能删,有构建器的服务不能删(409);改构建器的关联服务 400。纯逻辑(git 上下文、Job 生成、步骤校验、Job 结果判断、部署判断、错误行提取)有 Go 单测。

**没验证的 / 局限**:
- **部署步骤超时失败的路径**只有单测(`deployVerdict`),没有在集群里造过一个永远起不来的镜像去等 10 分钟。
- 只支持 `http(s)` 的**公共**仓库、按**分支或 ref** 构建;私有仓库的凭据、直接用 commit SHA 构建都没做。构建缓存关闭(`--cache=false`),每次从头构建。
- 仓库是单实例、无鉴权、无镜像清理(tag 只增不减);只有单节点验证过,多节点要把 `REGISTRY_PULL_HOST` 换成每个节点都能访问的地址。
- 用过的 builder 因为运行记录的外键**永远删不掉**(保留审计);测试时留下的 `api-badref`、`api-baddockerfile` 两个 builder 就是这样残留的。
- 构建 Pod 以 root 运行(kaniko 的要求),没做更细的隔离;日志只保存末尾约 64KB。
- **同一个 env 同时跑两个运行**:这条原来是个问题,现在已按 (环境, 服务) 串行解决。**修复前的实测**(两个运行都构建同一个分支、目标 dev3):同时提交 3 次,每次一个成功、一个以 `failed`(`superseded`)结束,
  而且谁赢取决于谁的构建先结束、不是谁后提交——3 次里有 1 次是后提交的运行先写入、先提交的后写入并成功,最终环境里是更早提交的镜像,即旧代码可能覆盖新代码;被顶掉的运行构建其实成功了却显示 `failed`。
  **修复后的实测**:同时提交 3 次,每次都是同一时刻只有 1 个构建 Job,后提交的显示 `queued: waiting for run N`,两个依次 `succeeded`(间隔约 5 秒,即前一个结束后下一个才开始),最终环境里是编号更大的、部署历史顺序与提交顺序一致;
  不同环境同时提交仍然并行(2 个 Job 同时构建);取消排在前面的运行后排队的接着开始;前面的运行失败后排队的不会被卡住;排队中途重启 deployer,队列仍然正确(它是从数据库推导的)。
  代价:同一环境同一服务的连续运行变慢(依次各约 50 秒);排在前面的运行如果卡住(构建最长 15 分钟、部署最长 10 分钟),后面的都得等它超时或被取消。
## Java 服务:先打 jar,再构建镜像
**支持,方式是多阶段 Dockerfile**:第一阶段用 JDK/Maven 把源码打成 jar,第二阶段只把 jar 拷进 JRE 镜像。builder 照常指向这个 Dockerfile,不需要任何代码改动。
**不是**流水线里一个独立的"构建 jar"步骤——流水线的步骤目前只有 `build` 和 `deploy`,jar 是在 `build`(kaniko 构建镜像)里面的第一个阶段打出来的,没有单独的产物、也没有单独的状态。

fork 里有两个可运行的示例(都实测过,服务是 `java-api`,清单在 `demo/java-api`):

| 示例 | 打 jar 的方式 | 需要外网 | 实测 |
|---|---|---|---|
| `src/java-plain` | `javac` + `jar`(JDK 自带) | 否 | build-and-deploy 到 dev3,约 75 秒;响应 `{"version":"r37","java":"21.0.12.1"}` |
| `src/java-maven` | `mvn package`(`maven-jar-plugin`) | 是(Maven Central) | 见下 |

```bash
curl -X POST $A/v1/services -d '{"name":"java-api","repo_url":"<仓库>","path":"demo/java-api","image_name":"java-api"}'
curl -X POST $A/v1/builders -d '{"name":"java-maven","service":"java-api","repo_url":"<仓库>","context_path":"src/java-maven"}'
curl -X POST $A/v1/pipelines/build-and-deploy/runs -d '{"builder":"java-maven","env":"dev5"}'
```

**Maven 在这个沙箱里的实测**:
1. **第一次失败**:`PKIX path building failed: unable to find valid certification path`——Maven 访问 Maven Central 时,JDK 自己的信任库不认这个沙箱出口网关的中间人证书
   (不是网络被拦:宿主机带 CA 访问同一个地址是 HTTP 200)。注意 `deployer-build` 里给 kaniko 挂的 `build-ca` 只让 **kaniko 自己**拉镜像、拉源码时信任它,**不影响 Dockerfile 里 `RUN` 出来的进程**。
2. **解决办法(只用现有能力)**:Dockerfile 里加一个可选的 `ARG EXTRA_CA_PEM`,非空时用 `keytool -importcert -cacerts` 导入 JDK 信任库;证书通过 **builder 的 `build_args`** 传入。
   传入网关根证书后:Maven 下载依赖、打 jar、做成镜像、部署到 dev5 全部成功,约 71 秒,响应 `{"version":"r39","java":"21.0.12.1"}`。
3. 这个问题**只在出口 HTTPS 被中间人时出现**(沙箱、企业代理、自签 CA 的内部 Nexus 都会遇到);正常环境不需要 `EXTRA_CA_PEM`,参数为空时那一步什么都不做。
4. 失败原因在 `message` 里:没有 kaniko 的 `Error:` 行时,取 `BUILD FAILURE` 之后第一条有内容的 `[ERROR] ...`(用真实的 Maven 失败验证过)。

**边界(没做的,或没验证的)**:
- **没有构建缓存**(`--cache=false`):每次构建都重新下载 Maven 依赖,依赖多的项目会很慢,也依赖外网稳定。可行的改进:内部 Maven 镜像仓库(Nexus/Artifactory)加 `settings.xml`,或给构建 Pod 挂持久卷放 `~/.m2`——两者都没做。
- **Gradle 没测过**,理论上同样是多阶段 Dockerfile,同样会遇到依赖下载和 CA 的问题。
- 构建 Pod 内存上限固定 2Gi、超时 15 分钟(`BUILD_TIMEOUT`),不能按 builder 单独调;大型 Java 项目可能不够。
- 单元测试只有写进 Dockerfile 里才会跑(失败会让构建失败);没有独立的"测试"步骤、没有测试报告。
- 没有把 jar 作为产物发布(比如上传到 Nexus),也没有 jar 的版本管理;镜像 tag 是运行号 `r<id>`,jar 的版本号在 `pom.xml` 里,两者互不关联。
- 想要真正独立的"构建 jar → 构建镜像"两个步骤(各自状态、日志、产物传递),需要扩展:例如新增 builder 类型"自定义 Job"(指定构建镜像和命令),或用 Jib 直接从 Maven 构建并推镜像(不需要 Dockerfile 和 kaniko);这是新功能,没有实现。

## 对账循环(`reconcile.go`)
- 每 10 秒一轮,API 写入后立即触发。多副本时用 MySQL `GET_LOCK` 选主,只有一个干活;leader 挂了锁随连接释放。
- `desired_state=running`:不存在就创建 Application;存在但 `spec` 与期望不符(有人手改)就改回;然后把 Application 的
  sync/health/第一条 condition 回写 `observed_*`。副本数/镜像通过 `source.kustomize.replicas/images` 传给 Argo CD。
- `desired_state=deleted`:删 Application(带 `resources-finalizer`,Argo CD 级联删资源),等它消失后物理删除数据库行。
- **副本数校验**:Application 报 Synced 且这一版期望之前已应用过时,直接读工作负载的实际 `spec.replicas` 与数据库比较。
  不一致 → `observed_health=ReplicaMismatch`,`observed_message` 写明期望/实际,并对 Application 触发硬刷新(同一应用 90 秒内最多一次);
  连续 3 轮仍不一致打 ERROR 日志。`observed_sync` 长时间(60 秒)停在 Unknown(如渲染报错)也会触发硬刷新清掉缓存的错误。
  这是为了补 Argo CD 的盲区:实测出现过渲染出错误副本数却显示 Synced/Healthy。需要一个**集群级只读**权限(`deployer-verify`:只有 `get`、只针对 `deployments`)。
- 只管带 `app.kubernetes.io/managed-by=deployer` 标签的 Application;同名但不是自己创建的会报冲突,不会动它。
- 命名:Application `<服务>-<环境>`,都放 `argocd` 命名空间、绑 AppProject `apps`;目标命名空间 `<环境>-env-<服务>`(由 Argo CD 的 `CreateNamespace=true` 创建,删应用时保留)。
  边界由 AppProject `apps` 约束:只允许发往 `*-env-*` 命名空间(Application 在 `argocd` 命名空间,所以不需要 Apps in Any Namespace),集群级资源只放行 Namespace。环境之间没有 Argo CD 层面的隔离(同一集群、不需要隔离)。
- **服务必须是 kustomize 目录**(有 `kustomization.yaml`):副本数/镜像是通过 `kustomize edit set` 传的,普通 YAML 目录会报
  `Missing kustomization file`(错误会回显在 `observed_message`)。fork 的 `guestbook/` 就是因此补了 `kustomization.yaml`。

## 运行
```bash
./scripts/deploy-mysql.sh        # 集群里起 MySQL(namespace deployer;密码随机生成,只在集群 Secret 里)
./scripts/deploy-build.sh        # 流水线需要的:构建命名空间 deployer-build 和集群内镜像仓库(NodePort 30500)
./scripts/build-deployer.sh      # 单测 + 静态编译 + 导入本机 k8s containerd(原型不经过镜像仓库)
kubectl apply -k manifests/deployer/app
kubectl -n deployer port-forward svc/deployer 18080:8080
```
程序在集群里跑,用 ServiceAccount + 只覆盖 `argocd` 命名空间里 Application 的 Role + 一个只读的 `deployer-verify`(集群级,只能 `get` deployments);数据库密码经 `secretKeyRef` 注入。
演示用的服务在 fork 的 `demo/`(`web`、`app-a`、`app-b`、`app-c`,各是一个 nginx Deployment)。示例:
```bash
A=http://127.0.0.1:18080
curl -X POST $A/v1/envs -d '{"name":"dev1"}'
curl -X POST $A/v1/services -d '{"name":"web","repo_url":"https://github.com/xiaobaowan1988/argocd-example-apps.git","path":"demo/web","image_name":"mirror.gcr.io/library/nginx"}'
curl -X PUT  $A/v1/envs/dev1/services/web -d '{"replicas":0}'        # 部署,但副本数 0
curl -X POST $A/v1/envs/dev1/services/web/scale -d '{"replicas":2}'
```

## 实测结果(手工在本集群跑的,不是自动化测试;Go 单测只覆盖漂移比较和 Application 生成)
- 3 个环境部署,副本数 1/0/2,Argo CD 同步后集群里与数据库一致;`dev2` 副本数 0 时 Deployment 存在、Pod 为 0。
- **12 个新环境并发首次部署**:全部 200。**同一部署上 20 个并发 scale**:全部 200,version 恰好 +20,历史 21 条。
  过期 `expected_version` → 409。最终 21 个环境期望与实际副本数全部一致。
- 漂移:手改 Application 的副本数 → ~10s 内改回;`kubectl delete` Application → 被重建、Pod 恢复;
  手动 `kubectl scale` 工作负载 → 由 Argo CD selfHeal 改回(不是 deployer)。
- 删除:Application 与数据库行消失,历史保留,命名空间保留(空)。环境名不在 project 允许范围(`prod1`)→ 错误回显在 `observed_message`。
- 2 个副本:只有一个对账;杀掉 leader 后另一个接手。

## 实测中发现的问题
1. **死锁(已修)**:并发首次部署时,对不存在的行 `SELECT ... FOR UPDATE` 会在唯一索引上加间隙锁,两个事务各持锁再 INSERT 互相死锁(MySQL 1213),
   12 个并发里有 2 个返回 500。修法:先 `INSERT ... ON DUPLICATE KEY UPDATE id=id` 让行存在再锁行,并对 1213/1205 做有限次重试。
2. **并发渲染互相覆盖(根因已查明并缓解)**:多个 Application 同时从同一路径渲染、各带不同 `kustomize.replicas` 时,Argo CD 会渲染出别人的副本数(还报 Synced/Healthy)或报错。
   根因是 Argo CD 的 `AllowsConcurrentProcessing()` 漏了 `Replicas`(v3.5.6 与 master 一致),见 [experiments/argocd-race](../experiments/argocd-race/README.md):
   原做法 6/6 次出问题,加 `kustomize.namespace` 后 3/3 次全部正确。缓解已落地:每个 Application 都带 `kustomize.namespace`=目标命名空间(语义无变化,强制独占渲染)。
   副本数校验(见"对账循环")仍保留,作为不依赖该缓解的兜底。结论的强度与未证明的部分见实验报告。

## 原型的局限
- MySQL 单实例、数据在节点 hostPath;生产要托管/主从、备份与恢复演练。
- API 默认无鉴权,没有按环境/服务的权限控制。
- 只支持 kustomize 应用的 `replicas`/`image` 两个参数;变量体系、模板渲染、环境基准/子环境(自测模式)都没做。
- 对账是轮询(10s),没有 watch Application 事件;观测状态有延迟。
- 没有 CI 自动化测试:Go 单测覆盖漂移比较、Application 生成、副本数校验(用 fake 客户端);MySQL 相关逻辑(并发、乐观锁、死锁重试)只做过手工验证。
- 副本数校验只看 `replicas`,不看 `image` 等其它字段;只支持 Deployment。

## 从 ApplicationSet + 标记文件迁移过来(已完成,相关文件已删除)
迁移时把旧的 `demo/<应用>/envs/<环境>.yaml` 标记导入了 deployer,步骤与结果:
1. 给 `app-a/b/c` 加 Deployment(原来只有 ConfigMap,没有副本数可调);
2. 导入标记 → 18 个部署(`app-a`: dev1–12 + test,`app-b`: dev1–3,`app-c`: dev3 + test);
   此时旧 ApplicationSet 生成的同名 Application 还在,deployer 报 `conflict: ... not managed by deployer` 且不动它们;
3. 删两个 ApplicationSet → 它们的 Application 级联删除 → deployer 创建自己的 18 个 Application;
4. 最终 18 个 Application 由 deployer 管理(2 个手写 `frontend` 不变),18 个 Deployment 全部就绪;
5. 删除标记文件、`tools/envs.sh`、ApplicationSet 清单、导入脚本,以及 Argo CD 里不再需要的 ApplicationSet 命名空间参数。
导入脚本和标记文件可从 git 历史取回。

## 再次收敛:去掉按环境组区分的底座(已完成)
迁移后 `manifests/apps/` 里还有 dev/test 两套(各自的命名空间、AppProject、手写 `frontend`),环境组 `group` 也让 deployer 要区分命名空间和 project。
按"同一集群、不需要隔离"收敛成一份:
1. deployer 去掉 `group`:所有 Application 放 `argocd-apps`、绑 `apps`;启动时自动 `DROP COLUMN env_group`;
2. `frontend` 也变成 deployer 里的一个服务(`guestbook` 目录、`guestbook-ui` 工作负载、部署到 `dev`、`test` 两个环境);
3. 线上切换顺序:deployer 缩到 0(否则它会重建被删的 Application)→ 删旧 Application、AppProject、命名空间 → 应用新底座与新版 deployer →
   deployer 按数据库里的记录自动重建全部 Application。
   **注意**:Argo CD 的 `application.namespaces` 参数在启动时读取,改 ConfigMap 后必须重启 application-controller 和 argocd-server,否则新命名空间里的
   Application 不会被处理(状态为空、没有工作负载)。
损失:Argo CD 层面"dev 应用不能发往 test 命名空间"的隔离;现在只靠命名约定和 deployer 的逻辑。

## 再简化:Application 放回 `argocd` 命名空间,不再用 Apps in Any Namespace(已完成)
不需要环境隔离后,`argocd-apps` 命名空间、AppProject 的 `sourceNamespaces`、`application.namespaces` 参数都没有收益,却带来"改参数要重启控制器"的坑。
所以 deployer 默认 `APP_NAMESPACE=argocd`,RBAC 改为 `argocd` 命名空间,`manifests/argocd/base` 不再有任何补丁(只剩官方 install.yaml)。
线上切换同样是:deployer 缩到 0 → 删 `argocd-apps` 里的 Application 并等级联清理 → 删命名空间 → 应用新配置并重启控制器 → 新版 deployer 按数据库重建全部 20 个 Application。

**切换中又观察到一次渲染问题(根因见上文第 2 点)**:重建后 `app-b-dev1`、`app-b-dev3` 一度显示 `Unknown`,错误是
`kustomize build .../demo/app-b failed: resource with name app-b does not match a ...`,而同路径的 `app-b-dev2` 正常;约 2 分钟内自行恢复。
这和前面 `dev1` 渲染成 0 副本是同一类现象,共同点是**多个 Application 同时从同一个路径渲染、各带 kustomize 副本数覆盖**。
我的推断(未验证):repo-server 对同一份检出执行 `kustomize edit set replicas` 再 `kustomize build`,并发时会读到被另一个进程改写的 `kustomization.yaml`。
前一次的症状更糟(渲染出错误的副本数还显示 Synced),这次是报错后自愈。副本数校验就是为这类情况加的(见"对账循环")。
