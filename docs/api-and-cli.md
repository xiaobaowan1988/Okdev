# API 网关和 `okdev` 命令行

没有前端:用 Zadig 账号登录一次,之后人或 AI 都通过 API 操作;同一个登录也能访问 kube-apiserver(kubectl)。

```
okdev login ──(Zadig 用户名或邮箱 + 密码)──> API 网关 /auth/login ──> Dex ──> ID Token + 刷新令牌(存在本地,权限 0600)
okdev token ──> 一个有效的 ID Token(快过期自动续期)
   ├─ curl / AI 工具:Authorization: Bearer $(okdev token) ──> API 网关 ──> deployer
   └─ kubectl:kubeconfig 的 exec 凭证插件调用 okdev token --exec-credential ──> kube-apiserver
```

## 命令行
```bash
okdev login --api https://api.example.com [--ca-file api-ca.crt] [--username alice]
                                 # 密码交互输入(不回显);用户名可以是 Zadig 用户名或邮箱
echo "$PASSWORD" | okdev login --api ... --username alice --password-stdin    # 脚本里用;没有 --password 参数
okdev token                      # 打印 ID Token;剩余不足 1 分钟会先用刷新令牌续期
okdev token --exec-credential    # 输出 kubectl 的 ExecCredential JSON
okdev whoami                     # 当前账号和令牌有效期
okdev logout
```
- **密码只在登录那一刻发给网关**,不保存;之后只用令牌和刷新令牌。命令行不接受密码参数和环境变量(参数会出现在进程列表里,环境变量会被子进程继承),
  只能交互输入或从标准输入读。**登录请你自己在终端里做,再让 AI 用 `okdev token`,不要把密码交给 AI。**
- 凭证文件默认 `~/.config/okdev/credentials.json`(`OKDEV_CREDENTIALS` 可改),里面有刷新令牌,**按密钥对待**。
- 并发调用安全:续期用文件锁串行化(Dex 每次刷新会轮换刷新令牌,并发续期会让后到的失败;`kubectl` 常会同时起多个进程)。
- **一个账号同一时间只有一份有效的刷新令牌**:在另一台机器或另一个终端再登录一次,先前那份凭证的刷新令牌就失效了(已签发的令牌到期前仍可用),
  先前那份到期后要重新 `okdev login`。(沙箱里实测:3 秒的重用宽限之后失效。)
- 管理员改密码或删除账号后,刷新令牌立刻失效,`okdev token` 会提示重新登录。**已签发的 ID Token 无法撤销**,到期(8 小时)前仍然有效。

### kubectl
kubeconfig 里的用户这样写(服务器地址和 CA 换成你的集群):
```yaml
users:
- name: me
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: okdev
      args: [token, --exec-credential]
      interactiveMode: Never
```
用户名是 `dex:` + Zadig 账号名。权限由 RBAC 决定:见 `deploy/rbac/developer.yaml`(默认只读 + 列命名空间;`pods/exec` 单独放在 `okdev-debugger`,给 AI 用要慎重)。

## API 网关
- 地址:`/openapi.yaml`(不需要令牌)描述了所有接口,可以直接交给 AI 工具。
- 登录:`POST /auth/login {username, password}` 返回 `{id_token, refresh_token}`;`POST /auth/refresh {refresh_token}` 续期。这两个不需要令牌。
- 认证:`Authorization: Bearer <ID Token>`。令牌的签名、过期、签发者、受众(`okdev`)都会验证,用户名取令牌里的 `name`。
  认证服务不可达返回 503(不是 401);deployer 不可达返回 502。
- **角色**:
  - **开发者**(所有登录用户,**默认**):读全部;**可以发布到任何环境**(没有按环境限制);触发/取消流水线运行、更新环境里的部署(镜像、副本数)、调副本数。
  - **管理员**(`--admins` 里的账号名):另外可以创建/删除环境、服务、构建器、模板、流水线,以及下线部署。
  - 构建脚本、Dockerfile、构建参数只返回给管理员(脚本是任意代码,谁能创建构建器谁就能在集群里执行代码)。
- **审计**:网关把令牌里的用户名作为 `X-Actor` 发给 deployer(调用方自己带的 `X-Actor` 会被覆盖),网关自己也记录每个请求的用户、方法、路径、状态码。
  deployer 里**流水线运行**的发起人在运行记录的 `triggered_by`(真实账号名);部署历史里由流水线触发的变更,操作人是 `pipeline-run-<运行号>`,再按运行号回查 `triggered_by`。
- `GET /refs?builder=NAME` 列出构建器仓库的分支和 Tag(仓库地址只取自 deployer 里的构建器,不能指定任意 URL)。

### 发布一个服务(AI 的典型流程)
```bash
T=$(okdev token); A=https://api.example.com
curl -H "Authorization: Bearer $T" $A/v1/builders?service=api                 # 选构建器
curl -H "Authorization: Bearer $T" "$A/refs?builder=api-main"                  # 选分支/Tag
curl -X POST -H "Authorization: Bearer $T" $A/v1/pipelines/build-and-deploy/runs \
     -d '{"builder":"api-main","env":"dev5","ref":"master"}'                   # 202,返回运行
curl -H "Authorization: Bearer $T" $A/v1/runs/42                                # pending→building→deploying→succeeded/failed/cancelled
curl -H "Authorization: Bearer $T" $A/v1/runs/42/log                            # 构建日志;失败时 message 已指出哪一步失败
```
同一个(环境,服务)的运行按提交顺序排队;取消不会回滚已经部署的内容。

## 暴力破解防护
**Dex 对密码登录没有任何失败次数限制**(沙箱实测:60 次错误密码在 5 秒内全部被处理,之后正确密码照样登录,账号不会被锁)。
所以命令行不直接访问 Dex,而是经网关的 `/auth/login`:同一个用户名(忽略大小写和空格)在 15 分钟内失败 5 次,就锁定 5 分钟,锁定期内连正确密码也拒绝(返回 429 和 `Retry-After`),
而且不会再去问 Dex;成功登录清零。参数:`--login-max-failures`、`--login-failure-window`、`--login-lock`。
- 设置 `--client-ip-header`(例如反向代理加的 `X-Forwarded-For`)后,还会按来源 IP 限制,防止同一个来源对很多账号各试一次。**只有在前面确实有反向代理时才设置**,否则客户端可以伪造这个头。
- 锁定按用户名计,所以别人可以故意输错 5 次来把你锁 5 分钟(拒绝服务)。这是这种防护的固有代价;锁定时间短、不累积,以此限制影响。
- 计数在内存里、每个副本各一份:N 个副本时有效上限是 N × 5 次;重启清零。
- **Dex 的令牌接口不能让用户直接访问,否则这个防护被绕过**:生产环境里 Dex 对外只开放 `/.well-known/openid-configuration` 和 `/keys`(kube-apiserver 和网关取公钥要用),
  令牌接口只给网关(集群内地址,用 `--dex-token-url` 指过去)。沙箱里为了简单,Dex 的 NodePort 是全开的,**没有这层保护**。

## 部署
- 网关:`deploy/api`(无状态,可多副本;不访问 Kubernetes API,无 RBAC)。参数:`--issuer-url`、`--audience`、`--issuer-ca-file`、`--deployer-url`、
  `--deployer-token-file`(deployer 开启 `API_TOKEN` 时必填)、`--admins`、`--git-token-file`、`--dex-token-url`、`--login-max-failures`、`--login-failure-window`、`--login-lock`、`--client-ip-header`。
- `--issuer-url` 和 `--audience` 要与 kube-apiserver 的 `--oidc-issuer-url`、`--oidc-client-id` 以及 Dex 的客户端 ID 一致。
- 对外要 https 域名和证书(Ingress/负载均衡);**deployer 不要对外暴露**,它没有按用户鉴权,只信任网关。
- 沙箱:`scripts/sandbox-api.sh` 一次装好 Dex、让 apiserver 信任它、导入账号、部署网关;`test/e2e/api.sh` 是端到端验证。
- 阿里云 ACK 等托管集群:见 [deploy-ack.md](deploy-ack.md)。

## 局限
- **没有环境级授权**:任何登录用户都能发布到任何环境(包括生产)。这是当前的有意选择;要限制时在网关的 `Policy` 里加按用户/环境的规则。
- 没有用户组:Dex 的密码库没有组,管理员名单只能按账号名写。
- deployer 自己没有按用户鉴权,它必须只让网关访问(网络层隔离)。
- 令牌 8 小时有效、无法提前撤销;离职要立刻断权,除了删 Dex 账号,还要删 RBAC 绑定并考虑缩短令牌有效期。
- 网关没有限流和请求体以外的滥用防护;Dex 对密码登录有没有失败次数限制没有测过,建议在 Dex 前面的入口做限流。
