# 用原来的 Zadig 账号登录(导出到 Dex)

开发人员继续用 Zadig 的账号密码登录 Headlamp 和 Kubernetes,不需要新账号、不需要重设密码。

```
浏览器 ─> Headlamp ─> Dex(登录页:邮箱+密码) ─ID Token─> Headlamp ─Bearer─> kube-apiserver ─> RBAC
                         ▲
        Zadig 本地账号(MySQL)── cmd/zadig-export ──> staticPasswords(bcrypt 哈希原样复制)
```

## 为什么要这样做
Zadig 的本地登录由它自己的 user 服务完成:用 bcrypt 校验 `user_login.password`,签发一个用内部共享密钥签名的 HS256 JWT
(`pkg/microservice/user/core/service/login/{local,token}.go`)。它没有 OIDC 发现地址和公钥,Kubernetes 无法验证,所以不能直接用。
Zadig 和 Dex 用的都是 bcrypt,因此把哈希复制进 Dex 的静态账号,原密码就继续有效。

## 步骤
1. **导出账号**(只读 MySQL;输出含密码哈希,按密钥处理,不要提交):
   ```bash
   go run ./cmd/zadig-export -dsn 'readonly:***@tcp(zadig-mysql:3306)/<用户库名>' > static-passwords.yaml
   ```
   只导出 `identity_type='system'` 的本地账号(LDAP/OAuth 登录的账号没有本地密码)。不能安全导出的账号会跳过并在 stderr 说明原因:
   没有 bcrypt 哈希、没有邮箱、邮箱与已导出账号重复。**开发人员登录时输入的是邮箱,不是 Zadig 账号名。**
2. **部署 Dex**(需要一个 https 地址和证书,kube-apiserver 要求 issuer 是 https):
   ```bash
   ISSUER=https://dex.example.com REDIRECT_URI=https://headlamp.example.com/oidc-callback \
   STATIC_PASSWORDS=static-passwords.yaml TLS_CERT=tls.crt TLS_KEY=tls.key scripts/deploy-dex.sh
   ```
   Dex 用 Kubernetes 存储,重启不丢登录态;ID Token 有效期 8 小时(`deploy/dex/config.tmpl.yaml`)。
3. **kube-apiserver 信任 Dex**(自建集群要改控制面配置并重启 apiserver;GKE/EKS 等托管集群用各自的 OIDC 配置入口):
   ```
   --oidc-issuer-url=https://dex.example.com
   --oidc-client-id=headlamp
   --oidc-username-claim=email
   --oidc-username-prefix=dex:
   --oidc-ca-file=<Dex 证书的 CA,公共 CA 签发则不需要>
   ```
   **kubeadm 静态 Pod 清单里,`dex:` 结尾带冒号,必须加引号**(`- "--oidc-username-prefix=dex:"`),否则 YAML 把它解析成映射,
   kubelet 会拒绝整个清单,apiserver 起不来。
4. **Headlamp 改用 Dex 登录**:改 `deploy/headlamp-dex/kustomization.yaml` 里的 issuer、callback-url,把 Dex 的 CA 放进 ConfigMap `dex-ca`,
   然后 `HEADLAMP_OVERLAY=deploy/headlamp-dex scripts/deploy-headlamp.sh`。
5. **授权**:登录只证明身份,权限由 RBAC 决定。用户名是 `dex:` + 邮箱,见 `deploy/dex-sandbox/developers.yaml`。

## 沙箱实测(myk8s 集群,Zadig 真实建表脚本 + 模拟账号)
| 场景 | 结果 |
|---|---|
| alice 用 Zadig 密码登录 Dex,已被授权 | 进入 Headlamp,选 dev、选服务、运行工作流,任务成功;`can-i`:能创建发布任务、能 exec、能看日志、不能读 Secret |
| bob 输错密码 | Dex 拒绝:`Invalid Email Address and password.` |
| bob 密码正确,但没授权 | 能登录,但环境下拉为空、不能创建任务、不能 exec(登录 ≠ 授权) |
| carol(Zadig 里没有邮箱) | 导出时跳过,不能登录 |
| dave(邮箱与 bob 重复) | 导出时跳过 |
| erin(LDAP 来源) | 不导出,不能登录 |
导出工具读的是 Zadig 源码里的真实 `mysql.sql` 建出的库(`test/zadig/seed.sh`),哈希是按 Zadig 的方式(`bcrypt.DefaultCost`)生成的。

## 局限(上线前请评估)
- **一次性复制,不同步**:之后 Zadig 里新增、改密码、禁用、删除账号,Dex 都不知道。需要重新导出、`deploy-dex.sh` 重启 Dex。
  **离职的人只要还在 Dex 里就还能登录**,所以必须有人负责定期重新导出。
- **绕过 Zadig 的保护**:Zadig 登录失败 5 次出验证码、授权人数检查、以后若开启 MFA,在这条路径上都不生效。
  Dex 对静态账号是否有失败次数限制,我没有测过,不要假定有。
- **没有用户组**:Dex 静态账号没有组字段,只能按邮箱逐个授权。
- 登录用邮箱;邮箱缺失或重复的 Zadig 账号无法导出,需要先在 Zadig 里补全。
- 密码哈希在 Dex 配置里(K8s Secret `dex-config`),要限制谁能读这个 Secret。
- 沙箱用自签证书和 NodePort;生产要正式域名和证书、Dex 前面的 Ingress。
- 若需要实时同步、验证码、MFA,应改成自定义 Dex Connector 去调 Zadig 登录接口,见之前的讨论。
