#!/bin/bash
# 在沙箱里装起底座:集群 -> Argo CD -> deployer(含构建/仓库)-> 演示数据。登录和 API 网关用 scripts/sandbox-api.sh。
# 每一步都可重复执行;集群已经在跑就跳过安装。需要 root、网络、docker、jq、Go。
set -euo pipefail
DIR=$(cd "$(dirname "$0")/.." && pwd)
export KUBECONFIG=${KUBECONFIG:-/etc/kubernetes/admin.conf}
NODE_IP=${NODE_IP:-$(hostname -I | awk '{print $1}')}
export NO_PROXY=${NO_PROXY:-},$NODE_IP no_proxy=${no_proxy:-},$NODE_IP
step() { printf '\n==> %s\n' "$*"; }

step "1/4 Kubernetes cluster"
if kubectl get nodes >/dev/null 2>&1; then echo "cluster is up, skipping install"; else "$DIR/platform/scripts/install.sh"; fi
until kubectl get nodes 2>/dev/null | grep -q ' Ready'; do sleep 3; done

step "2/4 Argo CD"
"$DIR/platform/scripts/prepull-sandbox-images.sh"
"$DIR/platform/scripts/install-argocd.sh"
"$DIR/platform/scripts/deploy-apps.sh"

step "3/4 deployer + build registry"
"$DIR/platform/scripts/deploy-mysql.sh"
"$DIR/platform/scripts/deploy-build.sh"
"$DIR/platform/scripts/build-deployer.sh"
kubectl apply -k "$DIR/platform/manifests/deployer/app"
kubectl -n deployer rollout status deploy/deployer --timeout=300s

step "4/4 demo data in deployer"
"$DIR/platform/scripts/register-demo.sh"

cat <<MSG

Done: cluster + Argo CD + deployer + demo data (environments dev3/dev5, services api/api-b).
Next: scripts/sandbox-api.sh   # Zadig-account login (Dex) + API gateway; needs STATIC_PASSWORDS=<zadig-export output> or ZADIG_SRC=<koderover/zadig checkout>
MSG
