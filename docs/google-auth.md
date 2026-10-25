# 用 Google 账号登录

目标:开发人员点"使用 Google 登录"即可进入 Headlamp,不再粘贴 token;权限由 Kubernetes RBAC 按人授权。

```
浏览器 ──登录──> Google ──ID Token──> Headlamp ──Bearer ID Token──> kube-apiserver(验证签名/aud/邮箱域名)──> RBAC
```
Headlamp 把用户的 Google ID Token 原样交给 API Server,所以**真正的校验和授权在 API Server**,Headlamp 本身不保存身份。
控制器的 `/refs` 接口、发布任务的创建同样走用户自己的身份。

## 1. Google Cloud:创建 OAuth 客户端
1. APIs & Services → Credentials → Create OAuth client ID,类型 **Web application**。
2. 已获授权的重定向 URI:`https://<headlamp 对外域名>/oidc-callback`(必须与下面 `HEADLAMP_CONFIG_OIDC_CALLBACK_URL` 完全一致)。
3. OAuth 同意屏幕选 **Internal**(仅组织内账号可用)。
4. 记下 Client ID 和 Client Secret。

## 2. API Server 信任 Google
需要 API Server 能访问 `https://accounts.google.com` 拉取签名公钥。

**kubeadm(旧式参数)**
```yaml
apiServer:
  extraArgs:
    oidc-issuer-url: https://accounts.google.com
    oidc-client-id: <client-id>.apps.googleusercontent.com
    oidc-username-claim: email
    oidc-username-prefix: "google:"
    oidc-required-claim: hd=okcoin.jp      # 只接受公司域名的账号
```
**推荐:`AuthenticationConfiguration`(K8s ≥ 1.30,可校验 `email_verified`)**
```yaml
apiVersion: apiserver.config.k8s.io/v1
kind: AuthenticationConfiguration
jwt:
  - issuer: {url: https://accounts.google.com, audiences: [<client-id>.apps.googleusercontent.com]}
    claimMappings:
      username: {expression: "'google:' + claims.email"}
    claimValidationRules:
      - expression: "claims.email_verified == true && claims.hd == 'okcoin.jp'"
        message: only verified okcoin.jp accounts are allowed
```
(用 `--authentication-config=<文件>` 传给 kube-apiserver;与 `--oidc-*` 参数不能同时使用。)

**GKE** 不需要以上步骤:GKE 原生接受 Google 身份,用 Google 账号的 IAM / RBAC 授权即可;Headlamp 侧改用 `-oidc-use-access-token`,Scope 增加 `https://www.googleapis.com/auth/cloud-platform`。

## 3. 部署 Headlamp(Google 登录)
```bash
kubectl -n okdev-system create secret generic headlamp-google-oidc \
  --from-literal=client-id=<id>.apps.googleusercontent.com --from-literal=client-secret=<secret>
# 把 deploy/headlamp-google-oidc/kustomization.yaml 里的 callback-url 改成真实地址
HEADLAMP_OVERLAY=deploy/headlamp-google-oidc ./scripts/deploy-headlamp.sh
```
overlay 用 PKCE、自动跳转 Google(`-oidc-auto-login`),Secret 通过环境变量注入,不出现在参数里。

## 4. 授权
```bash
kubectl apply -k config            # 包含 ClusterRole okdev-developer
kubectl apply -f deploy/headlamp-google-oidc/developers-example.yaml   # 改成真实邮箱
```
`okdev-developer`(列环境、进终端、创建发布任务、读分支列表)+ 内置 `view`(只读工作负载/Pod/日志/事件,不含 Secret)。
**Google 的 ID Token 不带群组**,只能按邮箱逐个绑定。要按组授权:前面接 Dex(Google connector,可读 Workspace 群组),或用脚本把群组成员同步成 RoleBinding。

## 已知限制
- Google ID Token 有效期约 1 小时,到期后需要重新登录(Headlamp 不使用 Google 的 refresh token 续期)。
- 沙箱里没有 Google 账号和公网可信 CA,**未做完整登录演练**。已验证的只有 Headlamp 一侧:启用 overlay 后访问任意页面会 302 到
  `accounts.google.com/o/oauth2/v2/auth`,带 PKCE(`code_challenge_method=S256`)、`redirect_uri=…/oidc-callback`、`scope=openid email profile`。
  API Server 一侧的 OIDC 参数、授权后的 RBAC 判定均未在真实 Google 登录下验证。
