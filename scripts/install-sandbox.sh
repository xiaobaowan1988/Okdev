#!/bin/bash
# 在沙箱里一次装起整套:集群 -> Argo CD -> deployer(含构建/仓库)-> 演示数据 -> okdev 控制器 -> Headlamp。
# 每一步都可重复执行;集群已经在跑就跳过安装。需要 root、网络、docker、jq、Go。
# 登录默认是粘贴 ServiceAccount token;要用 Zadig 账号(Dex)登录,再运行 scripts/sandbox-login.sh。
set -euo pipefail
DIR=$(cd "$(dirname "$0")/.." && pwd)
export KUBECONFIG=${KUBECONFIG:-/etc/kubernetes/admin.conf}
NODE_IP=${NODE_IP:-$(hostname -I | awk '{print $1}')}
export NO_PROXY=${NO_PROXY:-},$NODE_IP no_proxy=${no_proxy:-},$NODE_IP
step() { printf '\n==> %s\n' "$*"; }

step "1/6 Kubernetes cluster"
if kubectl get nodes >/dev/null 2>&1; then echo "cluster is up, skipping install"; else "$DIR/platform/scripts/install.sh"; fi
until kubectl get nodes 2>/dev/null | grep -q ' Ready'; do sleep 3; done

step "2/6 Argo CD"
"$DIR/platform/scripts/prepull-sandbox-images.sh"
"$DIR/platform/scripts/install-argocd.sh"
"$DIR/platform/scripts/deploy-apps.sh"

step "3/6 deployer + build registry"
"$DIR/platform/scripts/deploy-mysql.sh"
"$DIR/platform/scripts/deploy-build.sh"
"$DIR/platform/scripts/build-deployer.sh"
kubectl apply -k "$DIR/platform/manifests/deployer/app"
kubectl -n deployer rollout status deploy/deployer --timeout=300s

step "4/6 demo data in deployer"
"$DIR/platform/scripts/register-demo.sh"

step "5/6 okdev controller"
"$DIR/scripts/build-controller.sh"
kubectl create ns okdev-system --dry-run=client -o yaml | kubectl apply -f -
# 沙箱出口 HTTPS 被中间人:控制器列仓库分支要信任它的 CA(deploy-build.sh 把它放在 deployer-build 命名空间)
if kubectl -n deployer-build get cm build-ca >/dev/null 2>&1; then
  kubectl -n deployer-build get cm build-ca -o json \
    | python3 -c "import json,sys;d=json.load(sys.stdin);d['metadata']={'name':'build-ca','namespace':'okdev-system'};print(json.dumps(d))" \
    | kubectl apply -f -
fi
kubectl apply -k "$DIR/deploy/sandbox"
kubectl -n okdev-system rollout status deploy/okdev-controller --timeout=180s

step "6/6 Headlamp (ServiceAccount token login)"
"$DIR/scripts/deploy-headlamp.sh"

cat <<MSG

Done.
  Headlamp:  kubectl -n okdev-system port-forward svc/headlamp 4466:80   # http://localhost:4466
  token:     kubectl -n okdev-system create sa e2e-admin; kubectl create clusterrolebinding e2e-admin --clusterrole=cluster-admin --serviceaccount=okdev-system:e2e-admin; kubectl -n okdev-system create token e2e-admin
  Zadig-account login (Dex): scripts/sandbox-login.sh
MSG
