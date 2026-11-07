# 部署到阿里云 ACK(托管 Kubernetes)

**没有在 ACK 上实测过**,下面区分"官方文档说的"和"需要你们核实的"。

## 与沙箱的差别,需要改的
1. **镜像仓库**:沙箱用集群里单实例、无鉴权的 registry(`localhost:30500`),多节点不可用,要换成阿里云 ACR。
   deployer 的推送/拉取地址可配(`REGISTRY_PUSH_HOST`、`REGISTRY_PULL_HOST`),**但 deployer 目前不支持仓库鉴权**
   (构建 Job 的 kaniko 没有凭据可带,部署出来的 Pod 也没有拉取私有镜像的凭据),这是要新加的功能。
2. **MySQL**:沙箱是单实例、数据放节点目录,换成 RDS。deployer 用 `MYSQL_ADDR`,Dex 用 `MYSQL_HOST`,都是配置。
3. **镜像来源**:Argo CD、Dex、kaniko、构建模板里的 golang 等镜像来自 `ghcr.io`、`gcr.io`、`docker.io`,国内节点可能拉不下来,要同步到 ACR 并改地址(构建模板的镜像是数据,要一起改)。
4. **代码仓库**:Argo CD、deployer 的构建 Job、网关列分支都要访问 Git 仓库;在 GitHub 的话国内节点可能慢或不通,可以用云效 Codeup / Gitee。
5. **对外访问**:网关和 Dex 需要 https 域名(Ingress 或 ALB + 阿里云证书)。**国内域名要先备案**。deployer 不要对外暴露。
   **Dex 对外只开放 `/.well-known/openid-configuration` 和 `/keys`**(API Server 和网关取公钥要用),令牌接口(`/token`)只给网关:网关用 `--dex-token-url` 指到集群内地址。
   Dex 对密码登录没有失败次数限制,令牌接口一旦让用户直接访问,网关的登录锁定就被绕过了。
6. 集群安装脚本(kubeadm 等)用不上,只用 `platform/manifests`;沙箱里为绕限制做的处理(无 systemd、cgroup v1、出口 CA)也都不需要。

## 让 API Server 信任 Dex(OIDC)
托管集群不能改控制面静态 Pod,`platform/scripts/enable-oidc.sh` 用不了。官方文档说 ACK 可以在控制台配置:
**集群 → 组件管理 → 核心组件 → Kube API Server → 配置**
([部署 Keycloak 作为 ACK 集群的 OIDC 身份认证](https://help.aliyun.com/zh/ack/ack-managed-and-ack-dedicated/security-and-compliance/use-keycloak-oidc-provider-for-identity-authentication)、
[自定义 Pro 版控制面组件参数](https://help.aliyun.com/zh/ack/ack-managed-and-ack-dedicated/user-guide/customize-ack-pro-control-plane-component-parameters-1693464061811))。

文档里出现的参数:`oidcIssuerURL`(必须 https,集群版本 ≥ 1.18)、`oidcClientId`、`oidcUsernameClaim`(默认 `sub`)、`oidcUsernamePrefix`(`-` 表示不加前缀)、`oidcCAContent`(Base64 的 PEM)。
我们的取值:

| 参数 | 值 |
|---|---|
| `oidcIssuerURL` | Dex 的 issuer(https) |
| `oidcClientId` | `okdev` |
| `oidcUsernameClaim` | `name`(Dex 密码库账号没有 `preferred_username`,用它会让所有人 401) |
| `oidcUsernamePrefix` | `dex:` |
| `oidcCAContent` | Dex 证书的 CA(Base64);公共 CA 签发则不需要 |

**需要核实的**:
- 不同文档版本列出的参数不一致(有的只有 `oidcIssuerURL`,有的还有 `oidcCAContent`、`oidcGroupsPrefix`,Serverless 版又不同)。**以你们集群在控制台里实际出现的参数为准**。
  如果没有 `oidcUsernameClaim`,用户名只能是默认的 `sub`(一串编码),RBAC 很难写,需要改 Dex 输出能用的声明。
- API Server 会去访问 `oidcIssuerURL`:Dex 用公网域名时,集群要开通公网访问。
- ACK 的 RRSA OIDC 是另一回事(让 Pod 扮演 RAM 角色),与这里无关。

## 规模
十几个环境、几百个服务会产生几千个 Argo CD Application(deployer 每个"环境×服务"一个),需要给 Argo CD 调资源和分片。没有在这个规模下测过。
