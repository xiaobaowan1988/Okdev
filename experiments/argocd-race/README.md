# Argo CD 并发渲染问题:复现与根因

## 问题
多个 Application 同时从**同一个路径**渲染、各带不同的 `kustomize.replicas` 覆盖时:
- 有的渲染出**别人的副本数**,而且 Application 报 **Synced/Healthy**(静默错值,不会自愈);
- 有的报 `resource with name ... does not match a config ...` 或 `kustomization.yaml is empty`(渲染报错,少数会自愈)。

## 根因(源码 + 实验)
argo-cd v3.5.6 与 `master` 的源码一致:
- `util/kustomize/kustomize.go` 的 `Build()` 在 `k.path`(仓库**共享检出目录**)里直接执行 `kustomize edit set replicas ...`,改写 `kustomization.yaml`;
- 但 `pkg/apis/application/v1alpha1/types.go` 里决定"能否并发渲染(不取仓库排他锁)"的
  `ApplicationSourceKustomize.AllowsConcurrentProcessing()` 只检查了 `Images / CommonLabels / CommonAnnotations / NamePrefix / Namespace / NameSuffix / Patches / Components`,
  **漏了 `Replicas`**。
- 于是只带 `replicas` 覆盖的 Application 被当成"可并发",多个进程同时读改写同一个文件:
  最后写入的赢 → 渲染出别人的副本数;读到写了一半的文件 → 报错(`empty` / `does not match`)。

## 实验
`run-trial.sh <条件> <N> <trial>` 同时创建 N 个 Application(期望副本数 0–3 轮换),稳定后读 Deployment 的实际 `spec.replicas` 与期望比较,
有问题的 trial 再等 2 分钟看是否自愈。工作负载的 Pod 永远调度不上(nodeSelector 不存在),不占资源但 `spec.replicas` 可观测。`run-all.sh` 按计划跑全部,原始数据在 `results.tsv`。

| 条件 | 说明 | trial | 应用数 | 正确 | 静默错值 | 报错 | 含失败的 trial |
|---|---|---|---|---|---|---|---|
| **A** | 共享路径 + 仅 `replicas`(deployer 原来的做法) | 6 | 112 | 32 | **33** | **47** | **6 / 6** |
| **B** | 同 A,另加 `kustomize.namespace`=目标命名空间 | 3 | 48 | 48 | 0 | 0 | 0 / 3 |
| C | 每个应用独立目录(无共享文件) | 3 | 48 | 48 | 0 | 0 | 0 / 3 |
| D | 共享路径,无 kustomize 参数(无 `edit`) | 2 | 32 | 32 | 0 | 0 | 0 / 2 |
| E | 同 A,但每个应用间隔 6 秒创建(无并发) | 2 | 32 | 32 | 0 | 0 | 0 / 2 |

- A 的 6 次 trial 全部出问题,其余 4 组共 10 次、160 个应用全部正确(纯属巧合的概率约 1/8000)。
- A 的形态在"整批报错"(a1、a2:16/16 报错)和"报错与静默错值混合"(a3–a6)之间摆动;N=32 与 N=16 的出错比例接近。
- 自愈观察(2 分钟后):A 的 **33 个静默错值一个都没恢复**,47 个报错只恢复了 3 个。报错并不都是暂时的,静默错值不会自己好。
- 要出问题必须**同时**满足:共享同一份文件、参数需要 `kustomize edit`、没有排他锁、并发。C 去掉共享、D 去掉 edit、E 去掉并发、B 补上排他锁,各自都正常。

## 缓解
deployer 给每个 Application 的 `kustomize` 增加 `namespace: <目标命名空间>`(与 `destination.namespace` 相同,对资源没有语义影响),
让 Argo CD 把它当成"需要独占渲染"。已落地(`deployer/reconcile.go`),并在集群里验证:20 个现有 Application 自动更新、全部 Synced/Healthy;
再通过 deployer API **同时**给 12 个新环境部署 `app-a`(副本数 0–3 轮换),12/12 实际副本数与期望一致,副本数校验器无告警。
(注意这只是个绕过;根治需要上游把 `Replicas` 加进 `AllowsConcurrentProcessing()`。)

## 没有证明的(请据此判断结论的可信度)
- **没有直接观察到锁**:没有抓 repo-server 里获取排他锁的日志,也没有抓到被撕裂的 `kustomization.yaml` 内容。结论来自"源码 + 2×2 对照实验"的吻合,是强推断,不是亲眼所见。
- 只在 Argo CD v3.5.6、repo-server 单副本、N=16/32、一个仓库、一个 kustomize 目录上测过;多副本 repo-server(各有各的检出)、更大 N、其它参数组合(如只带 `images`,它在检查清单里所以预期安全)没测。
- B 之所以有效,我的解释是 `namespace` 在检查清单里 → 取排他锁;没有对 `images` / `commonAnnotations` 等其它清单内字段做对照。
- D 的期望副本数固定为 1(没有覆盖就只能是默认值),它排除的是"共享路径本身有问题",不能检出值被串写。
- 第一批数据(a5、a6)因脚本 bug(`jq` 参数过长)作废并重跑;a1–a4 在 bug 出现前,日志里没有该报错,数据有效。

## 复现
素材在 fork 的 `race-experiment` 分支(`race/shared`、`race/copy-01..32`;该分支因 git 代理拒绝删除分支而仍留在远程)。需要先有 `apps` AppProject 与 Argo CD,
然后 `./run-all.sh`(约 1 小时,可重复运行,已完成的 trial 会跳过)。

## 上游反馈材料(`upstream/`)
我的会话没有 `argoproj/argo-cd` 的 push 权限,GitHub API 也被会话绑定拦住(不能建 issue/PR、不能搜索上游 issue、不能建 fork),所以这里是准备好、可直接提交的材料,**尚未提交**:

| 文件 | 内容 |
|---|---|
| `upstream/issue.md` | 按上游 bug_report 模板写好的 issue(英文),含根因、最小复现、对照实验数据、修复建议、未验证的部分 |
| `upstream/fix.patch` | 修复(`AllowsConcurrentProcessing()` 加 `len(k.Replicas) == 0`)+ 单测;已在上游 master(Go 1.27.0)验证:新测试修复前失败、修复后通过,整个包通过,`go vet`/`gofmt` 干净 |
| `upstream/pr.md` | 按上游 PR 模板写好的 PR 描述(DCO 等需要你本人完成的项没有替你勾) |

提交步骤:
1. **先查重**:在 https://github.com/argoproj/argo-cd/issues 搜 `AllowsConcurrentProcessing`、`kustomize replicas`、`kustomization.yaml is empty`(我无法搜索)。
2. 补上 `argocd version` 输出,按 `issue.md` 开 issue。
3. fork `argoproj/argo-cd`,在 fork 里:`git checkout -b fix-kustomize-replicas-concurrency && git apply fix.patch && git commit -s -m "fix: ..."`(`-s` 就是 DCO 签署,必须你本人),推送并按 `pr.md` 开 PR。
