# okdev

没有前端:**用 Zadig 账号登录一次,人或 AI 通过 API 构建、发布服务到环境,同一个登录也能用 kubectl 访问集群。**
构建、部署、环境、历史由 `platform/`(原 myk8s)里的 deployer 负责;这里负责登录、授权和对外的 API。

```
okdev login ──Zadig 用户名(或邮箱)+ 密码──> API 网关 /auth/login(失败 5 次锁定)──> Dex ──> ID Token(8 小时,自动续期)
     │
     ├─ Bearer ──> API 网关(验证令牌、授权、X-Actor=账号名)──> deployer(MySQL 存 环境×服务 的期望状态)──> Argo CD ──> 集群
     └─ kubectl ──────────────────────────────────────────────> kube-apiserver(信任同一个 Dex)──> RBAC
Dex:账号和令牌存在 MySQL;管理员用 dex-users(gRPC + 双向 TLS)增删改账号;账号从 Zadig 一次性导出(zadig-export),Zadig 随后下线。
```

## 为什么不"每个环境一个目录"
十几个环境、几百个服务,按环境建目录是几千份文件。deployer 的做法:每个服务只有**一份**清单,"哪个服务部署到哪些环境、副本数、镜像"是 MySQL 里的几行数据。

## 布局
| 路径 | 内容 |
|---|---|
| `platform/` | 原 myk8s:集群(kubeadm)、Argo CD、镜像仓库、deployer(独立 Go 模块)。见 [platform/README.md](platform/README.md)。原仓库历史保留(旧路径,如 `git log -- deployer/pipeline.go`;原仓库是浅克隆,只带过来最近 50 个提交) |
| `cmd/api`、`internal/api` | API 网关:验证 Dex 令牌、开发者/管理员授权、转发到 deployer、敏感字段过滤、`/refs`、`/openapi.yaml` |
| `cmd/okdev`、`internal/cliauth` | 命令行:用户名密码登录(经网关)、取令牌、kubectl 凭证插件 |
| `cmd/dex-users`、`internal/dexusers` | 管理员管理 Dex 里的账号(增、改密码、删、导入) |
| `cmd/zadig-export`、`internal/zadigexport` | 把 Zadig 本地账号导出成 Dex 账号(bcrypt 哈希原样复制,原密码继续有效) |
| `deploy/` | `api`(网关)、`dex`、`rbac`(Kubernetes 权限)、`*-sandbox`(沙箱覆盖) |
| `scripts/` | `install-sandbox.sh`(底座)、`sandbox-api.sh`(登录 + 网关)、`deploy-dex.sh`、`build-api.sh` |
| `test/e2e/api.sh` | 端到端验证;`test/zadig/` 沙箱造账号和证书 |

## 文档
- [docs/api-and-cli.md](docs/api-and-cli.md):命令行、API 角色和审计、发布流程、部署参数、局限。接口清单见网关的 `/openapi.yaml`(给 AI 工具用)。
- [docs/zadig-accounts-dex.md](docs/zadig-accounts-dex.md):账号从 Zadig 迁到 Dex、账号管理、Dex 部署。
- [docs/deploy-ack.md](docs/deploy-ack.md):阿里云 ACK 上要改什么(未实测)。

## 运行(沙箱)
```bash
scripts/install-sandbox.sh            # 集群 -> Argo CD -> deployer -> 演示数据(环境 dev3/dev5,服务 api/api-b)
ZADIG_SRC=<koderover/zadig 检出目录> scripts/sandbox-api.sh   # Dex + 让 apiserver 信任它 + 导入测试账号 + API 网关
bin/okdev login --api http://<节点IP>:30080     # Zadig 用户名(或邮箱)和密码
curl -H "Authorization: Bearer $(bin/okdev token)" http://<节点IP>:30080/v1/envs
test/e2e/api.sh                       # 端到端验证
```
测试账号:alice、bob 是开发者(alice 另有 Kubernetes 只读权限),carol 是管理员。

## 权限一句话
所有登录用户都是**开发者**:读全部,**可以发布到任何环境**(默认,没有按环境限制);**管理员**(网关 `--admins`)另可管理环境、服务、构建器、模板、流水线和下线部署。
构建脚本、Dockerfile、构建参数只给管理员。Kubernetes 权限单独由 RBAC 决定(默认只读;进容器执行命令是单独的 `okdev-debugger` 角色)。

## 验证情况
单元测试(`go test ./cmd/... ./internal/...`):令牌验证(签名/过期/签发者/受众/缺 name)、授权矩阵、伪造 X-Actor、敏感字段过滤、识别服务不可达(503/502)、登录锁定(含大小写变体、按 IP、重启后宽限)、密码登录各种结果、令牌续期和并发续期、账号管理。
**沙箱端到端 `test/e2e/api.sh`(全部通过)**:
- 三个测试账号用用户名密码登录(用户名或邮箱都行,身份相同),密码不落盘,凭证文件权限 0600,错误密码不留凭证;
- 暴力破解防护:连续 5 次错误密码后,即使第六次密码正确也返回 429,期间不会去问 Dex;其他账号不受影响;直接调 HTTP 同样被锁;
- 无令牌/乱令牌 401;开发者读、列分支、发布到 dev5/dev3,运行成功,`triggered_by` 是真实账号,伪造 `X-Actor` 无效;不存在的分支 → 失败并带原因;
- 开发者创建构建器/环境、下线部署 → 403,看不到构建配方;管理员能;
- 令牌过期自动续期;账号被删除后刷新令牌失效、无法续期;
- 同一个令牌用 kubectl:alice(已授权)能读 Pod、身份是 `dex:alice`、不能读 Secret、不能进容器;bob(未授权)被拒绝但身份是 `dex:bob`。
**没验证的**:ACK 等托管集群(见 deploy-ack.md);没有测过 Dex 前面加 Ingress、只对外开放发现文档和公钥的部署方式;
网关多副本;几千个 Argo CD Application 的规模;deployer 开启 `API_TOKEN` 后网关带令牌访问(代码有,没实测)。
