#!/bin/bash
# 在沙箱集群里造一个模拟 Zadig 的 MySQL,用 Zadig 源码里的真实建表脚本建库,并写入测试账号。
# 用法: ZADIG_SRC=/path/to/koderover/zadig test/zadig/seed.sh
set -euo pipefail
export KUBECONFIG=${KUBECONFIG:-/etc/kubernetes/admin.conf}
DIR=$(cd "$(dirname "$0")" && pwd)
SQL=${ZADIG_SRC:?set ZADIG_SRC to a checkout of koderover/zadig}/pkg/microservice/user/core/init/mysql.sql
kubectl apply -f "$DIR/mysql.yaml"
kubectl -n zadig-test rollout status deploy/mysql --timeout=240s
my() { kubectl -n zadig-test exec -i deploy/mysql -- mysql -uroot -ptest-only "$@" 2>&1 | { grep -v "Using a password" || true; }; }

echo 'DROP DATABASE IF EXISTS zadig_user;' | my
sed 's/%s/zadig_user/g' "$SQL" | my
H() { (cd "$DIR/.." && cd .. && GOTOOLCHAIN=local go run ./test/zadig/hash "$1"); }
now=$(date +%s)
{
  echo "use zadig_user;"
  # 本地账号(identity_type=system):alice/bob 正常;carol 没有邮箱;dave 与 bob 邮箱重复
  echo "INSERT INTO user(uid,account,name,identity_type,email,created_at,updated_at) VALUES
    ('uid-alice','alice','Alice','system','alice@okcoin.jp',$now,$now),
    ('uid-bob','bob','Bob','system','Bob@okcoin.jp',$now,$now),
    ('uid-carol','carol','Carol','system','',$now,$now),
    ('uid-dave','dave','Dave','system','bob@okcoin.jp',$now,$now),
    ('uid-ldap','erin','Erin','ldap','erin@okcoin.jp',$now,$now);"
  echo "INSERT INTO user_login(uid,login_id,login_type,password,created_at,updated_at) VALUES
    ('uid-alice','alice',0,'$(H 'alice-pass-1')',$now,$now),
    ('uid-bob','bob',0,'$(H 'bob-pass-2')',$now,$now),
    ('uid-carol','carol',0,'$(H 'carol-pass-3')',$now,$now),
    ('uid-dave','dave',0,'$(H 'dave-pass-4')',$now,$now);"
  echo "INSERT INTO user_group(group_id,group_name,description,type,created_at,updated_at) VALUES ('g-dev','developers','dev team',2,$now,$now);"
  echo "INSERT INTO group_binding(group_id,uid) VALUES ('g-dev','uid-alice'),('g-dev','uid-bob');"
} | my
echo "seeded: alice/alice-pass-1  bob/bob-pass-2 (正常)  carol(无邮箱)  dave(邮箱与 bob 重复)  erin(ldap,应被忽略)"
