# Argo CD 并发渲染问题复现实验(进行中)

**现象**:多个 Application 同时从同一个路径渲染、各带不同的 `kustomize.replicas` 时,有的渲染出别人的副本数(还报 Synced/Healthy),
有的报 `resource with name ... does not match a config ...` 或 `kustomization.yaml is empty`。

**源码假设**(argo-cd v3.5.6 与 master 一致):`ApplicationSourceKustomize.AllowsConcurrentProcessing()` 决定渲染是否可并发(不取仓库排他锁),
它检查了 `Images/CommonLabels/CommonAnnotations/NamePrefix/Namespace/NameSuffix/Patches/Components`,**没有检查 `Replicas`**;
而 `Build()` 会在共享检出目录里执行 `kustomize edit set replicas`,改写 `kustomization.yaml`。

**实验**:`run-trial.sh <A|B|C|D|E> <N> <trial>`,`run-all.sh` 按计划跑全部;结果在 `results.tsv`。素材在 fork 的 `race-experiment` 分支(`race/shared`、`race/copy-NN`)。
结果与结论见实验完成后的更新。
