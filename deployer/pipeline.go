package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// buildConfig 是流水线执行器的配置。
type buildConfig struct {
	namespace     string        // 构建任务(Job)所在命名空间
	registryPush  string        // 构建任务推送镜像用的仓库地址(集群内 Service)
	registryPull  string        // 节点拉取镜像用的仓库地址(NodePort,localhost)
	kanikoImage   string        // 构建器镜像
	caConfigMap   string        // 可选:包含额外 CA 证书(key: build-ca.crt)的 ConfigMap,出口 HTTPS 被中间人时用
	buildTimeout  time.Duration // 构建超时
	deployTimeout time.Duration // 部署步骤等待超时
	concurrency   int           // 同时构建的上限
}

func runTag(id int64) string  { return "r" + strconv.FormatInt(id, 10) }
func jobName(id int64) string { return "build-" + strconv.FormatInt(id, 10) }
func pushImage(c buildConfig, r Run) string {
	return c.registryPush + "/" + r.Service + ":" + runTag(r.ID)
}

// pullImage 是部署用的镜像地址:同一个镜像,节点用 NodePort 地址拉取。
func pullImage(c buildConfig, r Run) string {
	return c.registryPull + "/" + r.Service + ":" + runTag(r.ID)
}

var (
	refRe     = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
	argNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	ansiRe    = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
)

// gitContext 把仓库地址和 ref 转成 kaniko 的 git 构建上下文:git://host/path.git#refs/heads/<ref>。
// ref 不以 refs/ 开头时当作分支名。(原型不支持直接用 commit SHA 构建。)
func gitContext(repoURL, ref string) (string, error) {
	u, err := url.Parse(repoURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", fmt.Errorf("repo_url must be an http(s) URL: %q", repoURL)
	}
	if !refRe.MatchString(ref) || strings.Contains(ref, "..") {
		return "", fmt.Errorf("invalid ref %q", ref)
	}
	if !strings.HasPrefix(ref, "refs/") {
		ref = "refs/heads/" + ref
	}
	path := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git")
	return "git://" + u.Host + "/" + path + ".git#" + ref, nil
}

// buildJob 生成这次运行的 kaniko 构建任务。VERSION、GIT_REF 两个构建参数由执行器注入(会覆盖 builder 里同名的)。
func buildJob(r Run, c buildConfig) (*batchv1.Job, error) {
	gctx, err := gitContext(r.RepoURL, r.Ref)
	if err != nil {
		return nil, err
	}
	args := []string{
		"--context=" + gctx,
		"--dockerfile=" + r.Dockerfile,
		"--destination=" + pushImage(c, r),
		"--insecure", // 集群内仓库是明文 HTTP
		"--cache=false",
	}
	if r.ContextPath != "" && r.ContextPath != "." {
		args = append(args, "--context-sub-path="+r.ContextPath)
	}
	ba := map[string]string{}
	for k, v := range r.BuildArgs {
		if !argNameRe.MatchString(k) {
			return nil, fmt.Errorf("invalid build arg name %q", k)
		}
		ba[k] = v
	}
	ba["VERSION"], ba["GIT_REF"] = runTag(r.ID), r.Ref
	keys := make([]string, 0, len(ba))
	for k := range ba {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--build-arg="+k+"="+ba[k])
	}

	deadline := int64(c.buildTimeout.Seconds())
	backoff, ttl := int32(0), int32(3600)
	optional := true
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: jobName(r.ID), Namespace: c.namespace,
			Labels: map[string]string{labelManagedBy: managedByValue, "deployer/run": strconv.FormatInt(r.ID, 10)},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff, ActiveDeadlineSeconds: &deadline, TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"deployer/run": strconv.FormatInt(r.ID, 10)}},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name: "kaniko", Image: c.kanikoImage, Args: args,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: mustQty("100m"), corev1.ResourceMemory: mustQty("256Mi")},
							Limits:   corev1.ResourceList{corev1.ResourceMemory: mustQty("2Gi")},
						},
					}},
				},
			},
		},
	}
	if c.caConfigMap != "" { // 可选的额外 CA:ConfigMap 不存在时不影响(optional)
		job.Spec.Template.Spec.Volumes = []corev1.Volume{{Name: "ca", VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: c.caConfigMap}, Optional: &optional}}}}
		job.Spec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{
			Name: "ca", MountPath: "/kaniko/ssl/certs/build-ca.crt", SubPath: "build-ca.crt", ReadOnly: true}}
	}
	return job, nil
}

// jobOutcome:构建任务是否结束、是否成功。reason 是失败原因。
func jobOutcome(j *batchv1.Job) (done, ok bool, reason string) {
	for _, c := range j.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return true, true, ""
		case batchv1.JobFailed:
			return true, false, strings.TrimSpace(c.Reason + ": " + c.Message)
		}
	}
	if j.Status.Succeeded > 0 {
		return true, true, ""
	}
	if j.Status.Failed > 0 {
		return true, false, "build pod failed"
	}
	return false, false, ""
}

// deployVerdict 判断部署步骤的结果:"ok" 部署就绪、"fail" 失败、"wait" 继续等。
func deployVerdict(r Run, es EnvService, now time.Time, timeout time.Duration) (state, msg string) {
	if es.DesiredState == "deleted" {
		return "fail", "deployment was removed while the run was deploying"
	}
	if es.Image != r.Image && es.Version > r.DeployVersion {
		return "fail", fmt.Sprintf("superseded: deployment was changed again (version %d, image %q)", es.Version, es.Image)
	}
	if es.AppliedVersion >= r.DeployVersion && es.Image == r.Image && es.ObservedSync == "Synced" && es.ObservedHealth == "Healthy" {
		return "ok", "deployed " + r.Image
	}
	if r.PhaseStartedAt != nil && now.Sub(*r.PhaseStartedAt) > timeout {
		detail := es.ObservedMessage
		if detail == "" {
			detail = fmt.Sprintf("sync=%s health=%s", es.ObservedSync, es.ObservedHealth)
		}
		return "fail", "deploy timed out: " + detail
	}
	return "wait", ""
}

// ---- 执行器 ----

type Runner struct {
	store  *Store
	db     *sql.DB
	kube   kubernetes.Interface
	cfg    buildConfig
	log    *slog.Logger
	kickFn func() // 部署步骤写完期望状态后,让对账循环尽快处理
	kickCh chan struct{}
}

func NewRunner(st *Store, db *sql.DB, kube kubernetes.Interface, cfg buildConfig, log *slog.Logger, kick func()) *Runner {
	return &Runner{store: st, db: db, kube: kube, cfg: cfg, log: log, kickFn: kick, kickCh: make(chan struct{}, 1)}
}

func (rn *Runner) Kick() {
	select {
	case rn.kickCh <- struct{}{}:
	default:
	}
}

func (rn *Runner) Run(ctx context.Context) {
	conn, err := rn.db.Conn(ctx)
	if err != nil {
		rn.log.Error("runner conn", "err", err)
		return
	}
	defer conn.Close()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		var got int // 与对账循环一样:会话级 GET_LOCK 选主,多副本时只有一个在推进流水线
		if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK('deployer-runner', 0)`).Scan(&got); err != nil {
			rn.log.Error("runner leader lock", "err", err)
		} else if got == 1 {
			rn.tick(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-rn.kickCh:
		}
	}
}

func (rn *Runner) tick(ctx context.Context) {
	runs, err := rn.store.ActiveRuns(ctx)
	if err != nil {
		rn.log.Error("list active runs", "err", err)
		return
	}
	building := 0
	for _, r := range runs {
		if r.Status == RunBuilding {
			building++
		}
	}
	for _, r := range runs {
		switch {
		case r.CancelRequested:
			rn.cancel(ctx, r)
		case r.Status == RunPending:
			if building < rn.cfg.concurrency && rn.startBuild(ctx, r) {
				building++
			}
		case r.Status == RunBuilding:
			rn.checkBuild(ctx, r)
		case r.Status == RunDeploying:
			rn.checkDeploy(ctx, r)
		}
	}
	rn.gcJobs(ctx, runs)
}

func strp(s string) *string { return &s }

func (rn *Runner) fail(ctx context.Context, r Run, msg string, log *string) {
	rn.log.Warn("run failed", "run", r.ID, "reason", msg)
	if _, err := rn.store.UpdateRun(ctx, r.ID, r.Status, RunUpdate{Status: RunFailed, Message: strp(msg), BuildLog: log, Finish: true}); err != nil {
		rn.log.Error("update run", "run", r.ID, "err", err)
	}
}

func (rn *Runner) startBuild(ctx context.Context, r Run) bool {
	job, err := buildJob(r, rn.cfg)
	if err != nil {
		rn.fail(ctx, r, "invalid build configuration: "+err.Error(), nil)
		return false
	}
	if _, err := rn.kube.BatchV1().Jobs(rn.cfg.namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		rn.log.Error("create build job", "run", r.ID, "err", err)
		return false // 暂时性错误:下一轮重试
	}
	ok, err := rn.store.UpdateRun(ctx, r.ID, RunPending, RunUpdate{Status: RunBuilding, JobName: job.Name, PhaseStart: true, MarkStarted: true})
	if err != nil {
		rn.log.Error("update run", "run", r.ID, "err", err)
	}
	if ok {
		rn.log.Info("build started", "run", r.ID, "service", r.Service, "env", r.Env, "ref", r.Ref, "job", job.Name)
	}
	return ok
}

func (rn *Runner) checkBuild(ctx context.Context, r Run) {
	job, err := rn.kube.BatchV1().Jobs(rn.cfg.namespace).Get(ctx, jobName(r.ID), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		rn.fail(ctx, r, "build job not found (deleted externally?)", nil)
		return
	}
	if err != nil {
		rn.log.Error("get build job", "run", r.ID, "err", err)
		return
	}
	done, ok, reason := jobOutcome(job)
	if !done {
		return // 超时由 Job 自己的 activeDeadlineSeconds 保证
	}
	log := rn.podLog(ctx, r.ID)
	rn.deleteJob(ctx, r.ID)
	if !ok {
		msg := "build failed: " + reason
		if e := lastError(log); e != "" { // 把日志里的真正原因带出来,不用再去翻日志
			msg += " — " + e
		}
		rn.fail(ctx, r, msg, &log)
		return
	}
	if !hasStep(r.Steps, "deploy") {
		if _, err := rn.store.UpdateRun(ctx, r.ID, RunBuilding, RunUpdate{Status: RunSucceeded, Image: pullImage(rn.cfg, r),
			Message: strp("built " + pullImage(rn.cfg, r)), BuildLog: &log, Finish: true}); err != nil {
			rn.log.Error("update run", "run", r.ID, "err", err)
		}
		return
	}
	// 部署步骤:把新镜像写进该环境里这个服务的期望状态,由对账循环(deployer)部署
	img := pullImage(rn.cfg, r)
	es, err := rn.store.Apply(ctx, fmt.Sprintf("pipeline-run-%d", r.ID), r.Env, r.Service, Change{Image: &img, Replicas: r.Replicas})
	if err != nil {
		rn.fail(ctx, r, "deploy step: "+err.Error(), &log)
		return
	}
	if _, err := rn.store.UpdateRun(ctx, r.ID, RunBuilding, RunUpdate{Status: RunDeploying, Image: img, DeployVersion: es.Version,
		Message: strp("built; deploying " + img), BuildLog: &log, PhaseStart: true}); err != nil {
		rn.log.Error("update run", "run", r.ID, "err", err)
		return
	}
	rn.log.Info("build succeeded, deploying", "run", r.ID, "image", img, "env", r.Env, "version", es.Version)
	if rn.kickFn != nil {
		rn.kickFn()
	}
}

func (rn *Runner) checkDeploy(ctx context.Context, r Run) {
	es, err := rn.store.GetEnvService(ctx, r.Env, r.Service)
	if errors.Is(err, ErrNotFound) {
		rn.fail(ctx, r, "deployment no longer exists", nil)
		return
	}
	if err != nil {
		rn.log.Error("get deployment", "run", r.ID, "err", err)
		return
	}
	switch state, msg := deployVerdict(r, es, time.Now(), rn.cfg.deployTimeout); state {
	case "ok":
		if _, err := rn.store.UpdateRun(ctx, r.ID, RunDeploying, RunUpdate{Status: RunSucceeded, Message: strp(msg), Finish: true}); err != nil {
			rn.log.Error("update run", "run", r.ID, "err", err)
		}
	case "fail":
		rn.fail(ctx, r, msg, nil)
	}
}

func (rn *Runner) cancel(ctx context.Context, r Run) {
	rn.deleteJob(ctx, r.ID)
	msg := "cancelled"
	if r.Status == RunDeploying {
		msg = "cancelled while deploying (the deployment already written is left as is)"
	}
	if _, err := rn.store.UpdateRun(ctx, r.ID, r.Status, RunUpdate{Status: RunCancelled, Message: &msg, Finish: true}); err != nil {
		rn.log.Error("cancel run", "run", r.ID, "err", err)
	}
}

func (rn *Runner) deleteJob(ctx context.Context, id int64) {
	bg := metav1.DeletePropagationBackground
	if err := rn.kube.BatchV1().Jobs(rn.cfg.namespace).Delete(ctx, jobName(id), metav1.DeleteOptions{PropagationPolicy: &bg}); err != nil && !apierrors.IsNotFound(err) {
		rn.log.Warn("delete build job", "run", id, "err", err)
	}
}

// podLog 取构建任务 Pod 日志的末尾(最多约 64KB),去掉终端颜色码。
func (rn *Runner) podLog(ctx context.Context, id int64) string {
	pods, err := rn.kube.CoreV1().Pods(rn.cfg.namespace).List(ctx, metav1.ListOptions{LabelSelector: "deployer/run=" + strconv.FormatInt(id, 10)})
	if err != nil || len(pods.Items) == 0 {
		return ""
	}
	tail, limit := int64(400), int64(65536)
	raw, err := rn.kube.CoreV1().Pods(rn.cfg.namespace).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{TailLines: &tail, LimitBytes: &limit}).Do(ctx).Raw()
	if err != nil {
		return ""
	}
	return ansiRe.ReplaceAllString(string(raw), "")
}

// LiveLog 供 API 使用:构建中返回实时日志尾部,结束后用数据库里保存的。
func (rn *Runner) LiveLog(ctx context.Context, id int64) string { return rn.podLog(ctx, id) }

// gcJobs 清理遗留的构建任务:对应的运行已经结束(或不存在)却还留着的 Job(比如执行器崩溃时来不及删)。
func (rn *Runner) gcJobs(ctx context.Context, active []Run) {
	jobs, err := rn.kube.BatchV1().Jobs(rn.cfg.namespace).List(ctx, metav1.ListOptions{LabelSelector: labelManagedBy + "=" + managedByValue})
	if err != nil {
		return
	}
	live := map[string]bool{}
	for _, r := range active {
		live[strconv.FormatInt(r.ID, 10)] = true
	}
	for _, j := range jobs.Items {
		if id := j.Labels["deployer/run"]; id != "" && !live[id] {
			n, _ := strconv.ParseInt(id, 10, 64)
			rn.log.Info("removing stale build job", "job", j.Name)
			rn.deleteJob(ctx, n)
		}
	}
}

func mustQty(s string) resource.Quantity { return resource.MustParse(s) }

// lastError 取构建日志里最后一条 "Error: ..." 行(kaniko 失败时会打印它),最多 200 个字符。
func lastError(log string) string {
	lines := strings.Split(log, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "Error:") {
			if len(l) > 200 {
				l = l[:200] + "…"
			}
			return l
		}
	}
	return ""
}
