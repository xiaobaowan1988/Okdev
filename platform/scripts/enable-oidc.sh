#!/bin/bash
# 让 kube-apiserver 信任 Dex(OIDC)。kubeadm 单节点集群:改静态 Pod 清单,kubelet 自动重启 apiserver。
# 用法: ISSUER=https://dex.example.com [CLIENT_ID=okdev] [CA_FILE=dex-ca.crt] scripts/enable-oidc.sh
# 可重复执行:已配置则更新参数,没有变化就不动清单(不会重启 apiserver)。
set -euo pipefail
MANIFEST=${MANIFEST:-/etc/kubernetes/manifests/kube-apiserver.yaml}
: "${ISSUER:?set ISSUER, e.g. https://dex.example.com}"
CLIENT_ID=${CLIENT_ID:-okdev}   # 必须与 Dex 客户端 ID 和 API 网关的 --audience 一致
export KUBECONFIG=${KUBECONFIG:-/etc/kubernetes/admin.conf}

CA_ARG=""
if [ -n "${CA_FILE:-}" ]; then
  install -m644 "$CA_FILE" /etc/kubernetes/pki/okdev-dex-ca.crt   # 清单里已经挂载了 /etc/kubernetes/pki
  CA_ARG="--oidc-ca-file=/etc/kubernetes/pki/okdev-dex-ca.crt"
fi
# 备份必须放在 kubelet 的静态 Pod 目录之外:kubelet 会把目录里的每个文件都当成静态 Pod 清单读,
# 同名的备份会和正本抢同一个 Pod,apiserver 可能按旧内容(旧参数)重启。
BACKUP_DIR=${BACKUP_DIR:-/var/backups/okdev}
mkdir -p "$BACKUP_DIR"
BACKUP=$BACKUP_DIR/$(basename "$MANIFEST").pre-oidc
[ -f "$BACKUP" ] || cp "$MANIFEST" "$BACKUP"

changed=$(python3 - "$MANIFEST" "$ISSUER" "$CLIENT_ID" "$CA_ARG" <<'PY'
import sys
path, issuer, client, ca = sys.argv[1:5]
want = {
  '--oidc-issuer-url': issuer,
  '--oidc-client-id': client,
  # 用户名取 name:Dex 密码库账号不输出 preferred_username,用它会让所有人 401
  '--oidc-username-claim': 'name',
  '--oidc-username-prefix': 'dex:',
}
if ca:
  want['--oidc-ca-file'] = ca.split('=', 1)[1]

def fmt(flag, value):
  item = f'{flag}={value}'
  # 值以冒号结尾时,YAML 里必须加引号,否则被解析成映射,kubelet 会拒绝整个清单
  return '    - "%s"' % item if value.endswith(':') else '    - %s' % item

def flag_of(line):
  t = line.strip()
  if not t.startswith('- '):
    return None
  t = t[2:].strip().strip('"')
  return t.split('=')[0] if t.startswith('--oidc-') else None

old = open(path).read().split('\n')
out, seen = [], set()
for line in old:
  f = flag_of(line)
  if f in want:
    out.append(fmt(f, want[f])); seen.add(f)
  elif f is not None:
    continue  # 不在 want 里的旧 --oidc-* 参数(例如以前设置过 --oidc-groups-claim)直接去掉,保证结果只由本脚本决定
  else:
    out.append(line)
missing = [k for k in want if k not in seen]
if missing:
  i = next(n for n, l in enumerate(out) if l.strip().startswith('- --authorization-mode'))
  out[i:i] = [fmt(k, want[k]) for k in missing]
changed = out != old
if changed:
  open(path, 'w').write('\n'.join(out))
print('yes' if changed else 'no')
PY
)
if [ "$changed" = "no" ]; then
  echo "kube-apiserver already trusts $ISSUER; nothing to do"
  exit 0
fi
[ -z "${NO_WAIT:-}" ] || { echo "manifest updated (NO_WAIT set, not waiting)"; exit 0; }
echo "manifest updated; waiting for kube-apiserver to restart..."
sleep 20
for _ in $(seq 1 60); do kubectl get nodes >/dev/null 2>&1 && { echo "kube-apiserver is back"; exit 0; }; sleep 4; done
echo "kube-apiserver did not come back; restore with: cp $BACKUP $MANIFEST" >&2
exit 1
