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
| `deploy_history` | 每次变更的前后快照与操作人;按名字记录、无外键,部署被清理后审计仍在 |

原型**没有**做 Zadig 那样的通用 `variables` JSON,只有 `replicas` 和 `image` 两个字段。

## API
```
POST   /v1/envs                                {"name":"dev3"}           # 名字 ≤20,[a-z0-9-]
DELETE /v1/envs/{env}                                                         # 还有部署时 409
POST   /v1/services                            {"name","repo_url","path","revision?","workload?","image_name?"}
PUT    /v1/envs/{env}/services/{svc}           {"replicas?","image?","expected_version?"}   # 部署/更新(幂等)
POST   /v1/envs/{env}/services/{svc}/scale     {"replicas","expected_version?"}             # 副本数 0..50,必须已存在
DELETE /v1/envs/{env}/services/{svc}                                                         # 下线
GET    /v1/envs/{env}/services                                                               # 期望 + 观测状态
GET    /v1/envs/{env}/services/{svc}/history
```
`X-Actor` 头记录操作人;配置了 `API_TOKEN`(Secret `deployer-api`)则要求 `Authorization: Bearer`,**原型默认不开鉴权**。

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
