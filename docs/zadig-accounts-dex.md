# 用原来的 Zadig 账号登录(账号迁到 Dex + MySQL)

Zadig 账号一次性导出后,Zadig 下线,**Dex 成为账号的唯一来源**。开发继续用 Zadig 的用户名(或邮箱)和原密码登录 Headlamp 和 Kubernetes;
管理员用命令行 `dex-users` 增、改、删账号,不用改配置、不用重启 Dex。

```
浏览器 ─> Headlamp ─> Dex(登录页) ─ID Token─> Headlamp ─Bearer─> kube-apiserver ─> RBAC
                       │  账号、令牌、签名密钥都存在 MySQL
                       └─ gRPC 管理接口(双向 TLS) <── dex-users(管理员)
一次性迁移:Zadig MySQL ── zadig-export ──> static-passwords.yaml ── dex-users import ──> Dex
```

## 为什么这样做
Zadig 的本地登录由它自己的 user 服务完成:用 bcrypt 校验 `user_login.password`,签发一个用内部共享密钥签名的 HS256 JWT
(`pkg/microservice/user/core/service/login/{local,token}.go`)。它没有 OIDC 发现地址和公钥,Kubernetes 无法验证,所以不能直接用。
Zadig 和 Dex 用的都是 bcrypt,哈希可以原样复制,开发的密码不用重设。

## 一次性迁移
1. **导出**(只读 MySQL;输出含密码哈希,按密钥处理,不要提交):
   ```bash
   go run ./cmd/zadig-export -dsn 'readonly:***@tcp(zadig-mysql:3306)/<用户库名>' > static-passwords.yaml
   ```
   只导出 `identity_type='system'` 的本地账号(LDAP/OAuth 登录的账号没有本地密码)。每个账号生成两个登录名:**Zadig 用户名**和**邮箱**
   (不区分大小写,同一个人;K8s 里的身份是用户名)。没有 bcrypt 哈希的账号整个跳过;邮箱缺失、格式不对或与别人重复时,只有邮箱登录不可用。
2. **部署 Dex**(见下一节),然后**导入**:
   ```bash
   go build -o dex-users ./cmd/dex-users
   kubectl -n okdev-system port-forward svc/dex 5557:5557 &
   export DEX_GRPC_CA=ca.crt DEX_GRPC_CERT=admin.crt DEX_GRPC_KEY=admin.key
   ./dex-users import static-passwords.yaml     # 可重复执行:已存在的更新哈希,不存在的创建,不会删除别的账号
   ./dex-users list
   ```
3. 验证后下线 Zadig。**下线前再导一次**,把迁移期间的账号变动带过来。

## 部署 Dex
```bash
ISSUER=https://dex.example.com REDIRECT_URI=https://headlamp.example.com/oidc-callback \
TLS_CERT=tls.crt TLS_KEY=tls.key \
GRPC_CERT=grpc.crt GRPC_KEY=grpc.key GRPC_CLIENT_CA=admin-ca.crt \
MYSQL_HOST=mysql.example.com MYSQL_USER=dex MYSQL_PASSWORD=... MYSQL_SSL_MODE=true \
scripts/deploy-dex.sh
```
- **MySQL**:账号只在这里,Zadig 下线后没有第二份。**必须做定期备份并演练恢复**;生产用托管或主从 MySQL(沙箱的单实例在 `deploy/dex-sandbox/mysql.yaml`)。
- **管理接口(gRPC,端口 5557)**:能创建任意账号,等于后门,所以要求双向 TLS——只有 `GRPC_CLIENT_CA` 签发的客户端证书能调用。
  这个 CA 应该专用于管理员,证书只发给管理员;Service 只在集群内可达,管理员用 `kubectl port-forward` 访问。
- ID Token 有效期 8 小时(`deploy/dex/config.tmpl.yaml`)。
- 沙箱证书脚本:`test/zadig/gen-certs.sh`(自签,仅测试)。

## 接入 Kubernetes 和 Headlamp
1. **kube-apiserver 信任 Dex**(自建集群要改控制面配置并重启 apiserver;托管集群用各自的 OIDC 配置入口):
   ```
   --oidc-issuer-url=https://dex.example.com
   --oidc-client-id=headlamp
   --oidc-username-claim=name
   --oidc-username-prefix=dex:
   --oidc-ca-file=<Dex 证书的 CA,公共 CA 签发则不需要>
   ```
   - **用户名取 `name`**:Dex 的密码库账号不输出 `preferred_username`(用它会让 apiserver 报 `claim not present`,所有人 401),`name` 才是账号名。
   - **kubeadm 静态 Pod 清单里,`dex:` 结尾带冒号,必须加引号**(`- "--oidc-username-prefix=dex:"`),否则 YAML 把它解析成映射,
     kubelet 会拒绝整个清单,apiserver 起不来。
   - **Dex 的签名密钥换了之后(例如存储换成新的库),apiserver 会一直用缓存的旧公钥,所有人 401**,要重启 kube-apiserver。
     正常的密钥轮换不受影响(旧公钥会继续发布一段时间)。
2. **Headlamp**:改 `deploy/headlamp-dex/kustomization.yaml` 里的 issuer、callback-url,把 Dex 的 CA 放进 ConfigMap `dex-ca`,
   `HEADLAMP_OVERLAY=deploy/headlamp-dex scripts/deploy-headlamp.sh`。
3. **授权**:登录只证明身份,权限由 RBAC 决定。K8s 用户名是 `dex:` + **账号名**(例如 `dex:alice`);需要绑哪些角色见 README 的“权限”一节,示例见 `deploy/dex-sandbox/developers.yaml`。

## 日常管理:`dex-users`
```bash
dex-users list
dex-users add alice --email alice@okcoin.jp      # 密码交互输入两遍;脚本里用 --password-stdin
dex-users passwd alice
dex-users delete alice
```
- 一个用户 = 用户名 + 邮箱两个登录名,`add`/`passwd`/`delete` 一起处理;用户名不能含 `@` 或空白,密码至少 8 位。
- `passwd` 和 `delete` 会**撤销该用户的刷新令牌**,让旧会话不能续期。**已签发的 ID Token 无法撤销**,到期(默认 8 小时)前仍然有效;
  离职要立刻断权的话,还要同时删掉 RBAC 绑定。
- 管理员的客户端证书丢了或人员变动,要重新签发并换掉 `dex-grpc-tls` 里的 CA。

## 沙箱实测(platform/ 集群;Zadig 真实建表脚本 + 模拟账号;Dex 用 MySQL 存储)
| 场景 | 结果 |
|---|---|
| 导出 4 个本地账号 + 1 个 LDAP 账号 | 导出 6 个登录名(4 个账号);LDAP 账号不读;carol 无邮箱、dave 邮箱重复,只能用用户名登录 |
| `dex-users import`,再导入一次 | 第一次 6 个创建;第二次 6 个更新,没有重复 |
| alice 用用户名/邮箱/大写用户名登录 | 都成功,K8s 里都是 `dex:alice` |
| 输错密码、LDAP 账号、carol 用邮箱 | 都被拒绝 |
| `add frank` → 登录;`passwd` → 旧密码失败、新密码成功;`delete` → 立即不能登录 | 全部即时生效,**没有重启 Dex** |
| 带 `offline_access` 取得刷新令牌,再 `passwd` / `delete` | 刷新令牌被撤销(对照:不操作时刷新成功) |
| 加用户后重启 Dex | 用户还在、能登录(账号在 MySQL) |
| 用另一个 CA 签发的客户端证书连管理接口 | 被拒绝:`tls: certificate required` |
| 非法输入:短密码、用户名含 `@`、重名、不存在的用户 | 都报错,不改数据 |

## 局限
- **没有用户组**:Dex 的密码库没有组字段,只能按用户名逐个写 RBAC。
- **没有自助改密码**:只能管理员重置。
- **Dex 对密码库账号有没有登录失败次数限制,我没有测过**,不要假定有。可以在 Dex 前面的 Ingress 上做限流。
- 用户名就是 K8s 里的身份,改名等于换人,RBAC 要跟着改。
- 沙箱用自签证书和 NodePort;生产要正式域名和证书、Dex 前面的 Ingress。
- `dex-users` 用 gRPC v1.66:更新的 grpc-go(≥1.67)强制要求 ALPN,而 Dex 2.41 的管理接口不支持,会握手失败。升级 Dex 或 grpc 时要一起验证。
