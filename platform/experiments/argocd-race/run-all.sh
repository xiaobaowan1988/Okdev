#!/bin/bash
# 按计划跑全部 trial;可重复运行(已完成的 trial 会跳过)。
cd "$(dirname "$0")"
[ -f results.tsv ] || printf 'trial\tcond\tN\tok\twrong\terr\tmissing\tsettle_s\tok_T2\twrong_T2\terr_T2\trepo_log_errs\n' > results.tsv
for t in a1 a2 a3 a4 a5; do ./run-trial.sh A 16 $t; done      # 生产模式,重复 5 次估计频率
./run-trial.sh A 32 a6                                          # 更高并发
for t in b1 b2 b3; do ./run-trial.sh B 16 $t; done              # 加 kustomize.namespace(预测:修复)
for t in c1 c2 c3; do ./run-trial.sh C 16 $t; done              # 独立目录(预测:正常)
for t in d1 d2; do ./run-trial.sh D 16 $t; done                 # 无 kustomize 参数(预测:正常)
for t in e1 e2; do ./run-trial.sh E 16 $t; done                 # 错开创建(预测:正常)
echo ALL-DONE
