<!-- 提交前请先在 https://github.com/argoproj/argo-cd/issues 搜索 "AllowsConcurrentProcessing" 和 "kustomize replicas" 确认没有重复(我的会话无法搜索上游 issue)。 -->
<!-- 提交方式:https://github.com/argoproj/argo-cd/issues/new?template=bug_report.md ,标题和正文如下。 -->

**Title:** Concurrent manifest generation corrupts `kustomize.replicas` overrides: `ApplicationSourceKustomize.AllowsConcurrentProcessing()` ignores `Replicas`

Checklist:

- [ ] I've searched in the docs and FAQ for my answer: https://bit.ly/argocd-faq.
- [x] I've included steps to reproduce the bug.
- [ ] I've pasted the output of `argocd version`. <!-- 请你补上 CLI 的输出;我只有服务端版本 v3.5.6 -->

**Describe the bug**

When several Applications that share the same repo path each set `spec.source.kustomize.replicas` (and nothing else) are rendered at the same time, repo-server renders **wrong output**:

- some Applications get another Application's replica count, **and are reported `Synced` / `Healthy`** (silent, and it does not self-heal);
- others fail with `kustomize build ... Error: kustomization.yaml is empty` or `Error: resource with name <x> does not match a config with the following GVK [...]`.

Root cause (as far as I can tell from reading the code at `v3.5.6`; `master` is identical here):

- `Build()` runs `kustomize edit set replicas ...` with `cmd.Dir = k.path`, i.e. it **rewrites `kustomization.yaml` in the shared checkout** ([kustomize.go#L201-L219](https://github.com/argoproj/argo-cd/blob/v3.5.6/util/kustomize/kustomize.go#L201-L219)).
- Whether a source may be rendered concurrently (without the exclusive repo lock) is decided by `ApplicationSource.AllowsConcurrentProcessing()` ([repository.go#L684](https://github.com/argoproj/argo-cd/blob/v3.5.6/reposerver/repository/repository.go#L684)), which delegates to `ApplicationSourceKustomize.AllowsConcurrentProcessing()` ([types.go#L840](https://github.com/argoproj/argo-cd/blob/v3.5.6/pkg/apis/application/v1alpha1/types.go#L840)).
- That function checks `Images`, `CommonLabels`, `CommonAnnotations`, `NamePrefix`, `Namespace`, `NameSuffix`, `Patches` and `Components` — but **not `Replicas`**, although `replicas` goes through `kustomize edit` exactly like `images` does. The existing test `TestSourceAllowsConcurrentProcessing_KustomizeParams` also has no `Replicas` case.

So Applications that only override `replicas` are treated as safe to render concurrently, and concurrent `kustomize edit` / `kustomize build` processes read, modify and write the same `kustomization.yaml`: last writer wins (wrong replica counts), or a build reads a half-written file (the errors above).

**To Reproduce**

1. Repo with one kustomize dir `race/shared/` containing a `Deployment` named `web` (`replicas: 1`) and a `kustomization.yaml` listing it. (A `nodeSelector` that matches no node keeps the pods Pending, so nothing needs to run; `spec.replicas` is still observable.)
2. Create N=16 Applications at once, all with `path: race/shared`, `syncPolicy.automated`, `CreateNamespace=true`, a distinct destination namespace each, and `source.kustomize.replicas: [{name: web, count: <i % 4>}]`:

   ```bash
   for i in $(seq 1 16); do cat <<YAML
   ---
   apiVersion: argoproj.io/v1alpha1
   kind: Application
   metadata: {name: race-$i, namespace: argocd, annotations: {race/desired: "$((i % 4))"}}
   spec:
     project: default
     source:
       repoURL: <REPO_URL>
       targetRevision: HEAD
       path: race/shared
       kustomize:
         replicas: [{name: web, count: $((i % 4))}]
     destination: {server: https://kubernetes.default.svc, namespace: race-$i}
     syncPolicy: {automated: {}, syncOptions: [CreateNamespace=true]}
   YAML
   done | kubectl apply -f -
   ```
3. Wait ~1 minute, then compare `kubectl -n race-$i get deploy web -o jsonpath='{.spec.replicas}'` with the count in each Application, and look at `kubectl -n argocd get app` for `ComparisonError`s.

**Observations** (Argo CD v3.5.6, single repo-server replica, default parallelism; 16 or 32 Applications created at the same moment):

| condition | trials | apps | correct | silently wrong (reported Synced) | render error | trials with a failure |
|---|---|---|---|---|---|---|
| shared path, `replicas` only (as above) | 6 | 112 | 32 | **33** | **47** | **6 / 6** |
| same, plus `kustomize.namespace` = destination namespace | 3 | 48 | 48 | 0 | 0 | 0 / 3 |
| one separate copy of the dir per Application | 3 | 48 | 48 | 0 | 0 | 0 / 3 |
| shared path, no kustomize parameters at all | 2 | 32 | 32 | 0 | 0 | 0 / 2 |
| shared path, `replicas` only, created 6 s apart (no concurrency) | 2 | 32 | 32 | 0 | 0 | 0 / 2 |

Two minutes later none of the 33 wrong results had changed, and only 3 of the 47 errors had recovered. Setting any field that **is** in the `AllowsConcurrentProcessing()` list (here `namespace`, set to the destination namespace, which is a no-op for the rendered resources) makes the problem disappear, which is consistent with the diagnosis above. The failure needs all of: a shared path, a parameter that triggers `kustomize edit`, no exclusive lock, and concurrency.

**Expected behavior**

An Application with `kustomize.replicas` renders its own replica count regardless of how many other Applications use the same path at the same time — i.e. `Replicas` should be treated like `Images` and the other `kustomize edit`-based options.

**Proposed fix** (one line + test; I ran the new test against current `master` with Go 1.27.0: it fails without the change and passes with it, and the whole `pkg/apis/application/v1alpha1` package passes):

```diff
 func (k *ApplicationSourceKustomize) AllowsConcurrentProcessing() bool {
 	return len(k.Images) == 0 &&
+		len(k.Replicas) == 0 &&
 		len(k.CommonLabels) == 0 &&
```

**Workaround**: add a field that is already in the list and is a no-op, e.g. `kustomize.namespace` equal to `destination.namespace`.

**Caveats / not verified**

- I did not observe the repo lock itself (no log of lock acquisition) nor capture a torn `kustomization.yaml`; the diagnosis comes from the code plus the controlled comparison above.
- Tested only on v3.5.6 at runtime (source also checked on `master`), with one repo-server replica, N = 16 and 32, one repository.
- I could not determine in which release the omission was introduced (my history window starts 2025-09-29 and both pieces of code already existed then).

**Version**

```shell
Server: v3.5.6   <!-- argocd version 的完整输出请你补上 -->
```

**Logs**

```
rpc error: code = Unknown desc = `kustomize build <path to cached source>/race/shared` failed exit status 1: Error: kustomization.yaml is empty
rpc error: code = Unknown desc = `kustomize build <path to cached source>/race/shared` failed exit status 1: Error: resource with name web does not match a config with the following GVK [Deployment.[noVer].[noGrp] StatefulSet.[noVer].[noGrp] ReplicaSet.[noVer].[noGrp] ReplicationController.[noVer].[noGrp]]
```

---
<!-- 可选,按你的意愿保留或删除:上游对 AI 辅助贡献可能有要求,建议如实说明 -->
_The reproduction data and the patch were produced with the help of an AI coding assistant (Claude Code)._
