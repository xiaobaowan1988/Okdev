#!/bin/bash
# 复现实验:一个 trial = 同时创建 N 个 Application,观察渲染结果是否正确。结果追加到 results.tsv。
# 用法: run-trial.sh <条件 A|B|C|D|E> <N> <trial-id>
#   A 共享路径 + 仅 kustomize.replicas(我们的生产模式)
#   B 同 A,另加 kustomize.namespace=目标命名空间(语义无变化,但会让 Argo CD 取排他锁)
#   C 每个应用各用一个独立副本目录(没有共享文件)               — 对照
#   D 共享路径,不带任何 kustomize 参数(没有 kustomize edit)       — 对照
#   E 同 A,但每个应用间隔 6 秒创建(没有并发)                      — 对照
set -uo pipefail
COND=$1; N=$2; T=$3
HERE=$(cd "$(dirname "$0")" && pwd); OUT=$HERE/results.tsv
export KUBECONFIG=${KUBECONFIG:-/etc/kubernetes/admin.conf}
REPO=https://github.com/xiaobaowan1988/argocd-example-apps.git; BRANCH=race-experiment
grep -q "^$T	" "$OUT" 2>/dev/null && { echo "skip $T (done)"; exit 0; }

app_yaml() { # i
  local i=$1 want=$(( i % 4 )) path=race/shared kz="" ns="${T}n${i}-env-race"
  [ "$COND" = C ] && path=$(printf 'race/copy-%02d' "$i")
  case $COND in
    A|C|E) kz="    kustomize:
      replicas: [{name: web, count: $want}]" ;;
    B)     kz="    kustomize:
      replicas: [{name: web, count: $want}]
      namespace: $ns" ;;
    D)     want=1 ;;
  esac
  cat <<YAML
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: $T-$i
  namespace: argocd
  labels: {experiment: race, trial: "$T"}
  annotations: {race/desired: "$want"}
  finalizers: [resources-finalizer.argocd.argoproj.io]
spec:
  project: apps
  source:
    repoURL: $REPO
    targetRevision: $BRANCH
    path: $path
$kz
  destination: {server: https://kubernetes.default.svc, namespace: $ns}
  syncPolicy: {automated: {}, syncOptions: [CreateNamespace=true]}
YAML
}

snapshot() { # → JSON 数组:每个应用的 期望/实际/分类
  local apps deps
  apps=$(kubectl -n argocd get app -l trial=$T -o json) || return 1
  deps=$(kubectl get deploy -A -o json | jq '[.items[] | select(.metadata.name=="web" and (.metadata.namespace|endswith("-env-race"))) | {ns:.metadata.namespace, r:.spec.replicas}]')
  jq -n --argjson a "$apps" --argjson d "$deps" '
    [$a.items[] | {name:.metadata.name, want:(.metadata.annotations["race/desired"]|tonumber), ns:.spec.destination.namespace,
        sync:(.status.sync.status // ""), msg:((.status.conditions // [])[0].message // ""),
        err:(((.status.conditions // []) | map(select(.type|test("Error"))) | length) > 0)}
      | . as $x | . + {actual: (($d | map(select(.ns==$x.ns)) | .[0].r) // null)}
      | . + {cls: (if .err then "ERR" elif .actual==null then "MISSING" elif .actual==.want then "OK" else "WRONG" end)}]'
}
count() { jq -r --arg c "$1" '[.[] | select(.cls==$c)] | length' <<<"$2"; }

START=$(date -u +%Y-%m-%dT%H:%M:%SZ)
echo "[$T] cond=$COND N=$N 开始 $START"
if [ "$COND" = E ]; then
  for i in $(seq 1 $N); do app_yaml $i | kubectl apply -f - >/dev/null; sleep 6; done
else
  for i in $(seq 1 $N); do app_yaml $i; done | kubectl apply -f - >/dev/null
fi
T0=$(date +%s)
# 等所有应用要么出了 Deployment 要么报错,最多 180 秒
S=""; for k in $(seq 1 36); do sleep 5; S=$(snapshot) || continue
  [ "$(jq '[.[] | select(.cls=="MISSING")] | length' <<<"$S")" = 0 ] && break; done
SETTLE=$(( $(date +%s) - T0 )); sleep 10; S1=$(snapshot)
OK=$(count OK "$S1"); WR=$(count WRONG "$S1"); ER=$(count ERR "$S1"); MI=$(count MISSING "$S1")
echo "[$T] T1: ok=$OK wrong=$WR err=$ER missing=$MI settle=${SETTLE}s"
# 有问题就再等 2 分钟,看会不会自愈
if [ $((WR+ER)) -gt 0 ]; then
  jq -r '.[] | select(.cls!="OK") | "    \(.name) want=\(.want) actual=\(.actual) sync=\(.sync) \(.cls) \(.msg|.[0:90]|gsub("\n";" "))"' <<<"$S1" | head -8
  sleep 120; S2=$(snapshot); OK2=$(count OK "$S2"); WR2=$(count WRONG "$S2"); ER2=$(count ERR "$S2")
else OK2=$OK; WR2=$WR; ER2=$ER; fi
LOGERR=$(kubectl -n argocd logs deploy/argocd-repo-server --since-time=$START 2>/dev/null | grep -ciE "does not match|kustomize.*(failed|error)")
printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$T" "$COND" "$N" "$OK" "$WR" "$ER" "$MI" "$SETTLE" "$OK2" "$WR2" "$ER2" "$LOGERR" >> "$OUT"
# 清理:删应用(finalizer 级联删 Deployment),再删命名空间
kubectl -n argocd delete app -l trial=$T --wait=false >/dev/null 2>&1
for k in $(seq 1 40); do [ "$(kubectl -n argocd get app -l trial=$T --no-headers 2>/dev/null | wc -l)" = 0 ] && break; sleep 5; done
kubectl get ns --no-headers 2>/dev/null | awk '{print $1}' | grep -E "^${T}n[0-9]+-env-race$" | xargs -r kubectl delete ns --wait=false >/dev/null 2>&1
echo "[$T] 完成"
