<!-- 在 issue 建好后,从你自己的 argo-cd fork 提交 PR。步骤见 README。DCO 必须由你本人签署(git commit -s),我没有替你勾选或签署。 -->

**Title:** fix: Do not render kustomize apps with `replicas` overrides concurrently

Fixes #<ISSUE NUMBER>

`ApplicationSourceKustomize.AllowsConcurrentProcessing()` checks every kustomize option that makes `Build()` run `kustomize edit` in the shared checkout — except `Replicas`. Applications that only set `kustomize.replicas` were therefore rendered concurrently and overwrote each other's `kustomization.yaml`, producing wrong replica counts (reported as `Synced`) or `kustomization.yaml is empty` / `does not match a config` errors. See the issue for the reproduction and measurements.

This PR adds `len(k.Replicas) == 0` to the check and a `Has Replicas` case (plus a "no params" case that must still allow concurrency) to `TestSourceAllowsConcurrentProcessing_KustomizeParams`. On current `master` the new `Has Replicas` case fails without the change and passes with it; the whole `pkg/apis/application/v1alpha1` package passes, `go vet` and `gofmt` are clean.

Checklist:

* [x] Either (a) I've created an enhancement proposal..., (b) this is a bug fix, or (c) this does not need to be in the release notes.
* [ ] The title of the PR states what changed and the related issues number (used for the release note).
* [ ] The title of the PR conforms to the [Title of the PR](https://argo-cd.readthedocs.io/en/latest/developer-guide/submit-your-pr/#title-of-the-pr) <!-- `fix:` 前缀已按文档写 -->
* [ ] I've included "Closes [ISSUE #]" or "Fixes [ISSUE #]" in the description to automatically close the associated issue.
* [x] I've updated both the CLI and UI to expose my feature, or I plan to submit a second PR with them. <!-- N/A:不是新功能 -->
* [x] Does this PR require documentation updates? No.
* [ ] I have signed off all my commits as required by [DCO](https://github.com/argoproj/argoproj/blob/master/community/CONTRIBUTING.md#legal) <!-- 必须由提交者本人完成:git commit -s -->
* [x] I have written unit and/or e2e tests for my change.
* [ ] My build is green ([troubleshooting builds](https://argo-cd.readthedocs.io/en/latest/developer-guide/ci/)). <!-- 以 CI 结果为准 -->
* [ ] Optional. For bug fixes, I've indicated what older releases this fix should be cherry-picked into. <!-- 我不清楚上游当前支持哪些分支,由维护者/你决定 -->
