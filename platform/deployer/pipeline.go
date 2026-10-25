package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path"
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
	kanikoImage   string        // 构建镜像用的 kaniko
	gitImage      string        // 拉源码用的镜像(需要有 git 和 sh)
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

// validateRepoRef 校验仓库地址和 ref:仓库必须是 http(s) URL;ref 可以是分支、tag、完整 ref(refs/...)或 commit SHA。
func validateRepoRef(repoURL, ref string) error {
	u, err := url.Parse(repoURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("repo_url must be an http(s) URL: %q", repoURL)
	}
	if !refRe.MatchString(ref) || strings.Contains(ref, "..") || strings.HasPrefix(ref, "-") {
		return fmt.Errorf("invalid ref %q", ref)
	}
	return nil
}

const (
	workspaceDir = "/workspace"            // 三个步骤共享的工作目录(emptyDir)
	caDir        = "/etc/build-ca"         // 可选的额外 CA(ConfigMap)挂载目录
	extraCAFile  = caDir + "/extra-ca.crt" // 只含需要额外信任的根证书,脚本里可用它导入 JDK 等的信任库
)

// 这些变量由执行器注入,builder 的 build_args 不能覆盖。
var reservedVars = map[string]bool{"VERSION": true, "GIT_REF": true, "PROFILE": true, "SERVICE": true, "DEPLOY_ENV": true, "WORKSPACE": true, "BUILD_EXTRA_CA_FILE": true}

// 拉源码:用 init + fetch 而不是 clone,这样分支、tag、完整 ref、commit SHA 都能用同一条命令取到。
const checkoutScript = `if [ -f ` + caDir + `/build-ca.crt ]; then export GIT_SSL_CAINFO=` + caDir + `/build-ca.crt; fi
git init -q ` + workspaceDir + `
cd ` + workspaceDir + `
git remote add origin "$REPO"
git fetch -q --depth 1 origin "$REF"
git checkout -q FETCH_HEAD
echo "checked out $(git rev-parse --short HEAD) from $REF"`

// 构建脚本和 Dockerfile 的内容存在数据库里,由 git 步骤在拉完源码后写进共享目录(放在 .deployer 里,不会和仓库里的文件冲突)。
// 内容通过环境变量传入、用 printf 写文件,不拼进命令,避免引号和 $ 带来的注入。
const (
	recipeDir      = workspaceDir + "/.deployer"
	scriptFile     = recipeDir + "/build.sh"
	dockerfileFile = recipeDir + "/Dockerfile"
	writeRecipe    = `
mkdir -p ` + recipeDir + `
printf '%s\n' "$BUILD_DOCKERFILE_TEXT" > ` + dockerfileFile + `
if [ -n "${BUILD_SCRIPT_TEXT-}" ]; then printf '%s\n' "$BUILD_SCRIPT_TEXT" > ` + scriptFile + `; fi`
)

func envVars(m map[string]string) []corev1.EnvVar {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]corev1.EnvVar, 0, len(keys))
	for _, k := range keys {
		out = append(out, corev1.EnvVar{Name: k, Value: m[k]})
	}
	return out
}

// buildJob 生成这次运行的构建任务:一个 Pod,按顺序执行 ① git(拉源码) ② script(构建脚本,builder 配了脚本才有)
// ③ kaniko(用 Dockerfile 构建镜像并推送)。三步通过 emptyDir 共享源码和脚本产出的产物。
func buildJob(r Run, c buildConfig) (*batchv1.Job, error) {
	if err := validateRepoRef(r.RepoURL, r.Ref); err != nil {
		return nil, err
	}
	ctxDir := workspaceDir
	if r.ContextPath != "" && r.ContextPath != "." {
		if !validRelPath(strings.TrimPrefix(r.ContextPath, "./")) {
			return nil, fmt.Errorf("invalid context_path %q", r.ContextPath)
		}
		ctxDir = workspaceDir + "/" + path.Clean(r.ContextPath)
	}
	tag := runTag(r.ID)

	// 构建参数:builder 里配置的 + 执行器注入的。同时作为脚本的环境变量和 Docker 构建参数。
	user := map[string]string{}
	for k, v := range r.BuildArgs {
		if !argNameRe.MatchString(k) {
			return nil, fmt.Errorf("invalid build arg name %q", k)
		}
		if reservedVars[k] {
			continue // 保留名由执行器注入
		}
		user[k] = v
	}
	dockerArgs := map[string]string{}
	for k, v := range user {
		dockerArgs[k] = v
	}
	dockerArgs["VERSION"], dockerArgs["GIT_REF"] = tag, r.Ref
	if r.Profile != "" {
		dockerArgs["PROFILE"] = r.Profile
	}
	scriptEnv := map[string]string{}
	for k, v := range dockerArgs {
		scriptEnv[k] = v
	}
	scriptEnv["SERVICE"], scriptEnv["DEPLOY_ENV"], scriptEnv["WORKSPACE"], scriptEnv["BUILD_EXTRA_CA_FILE"] = r.Service, r.Env, ctxDir, extraCAFile

	if strings.TrimSpace(r.Dockerfile) == "" {
		return nil, fmt.Errorf("run %d has no dockerfile", r.ID)
	}
	args := []string{
		"--context=dir://" + ctxDir,
		"--dockerfile=" + dockerfileFile,
		"--destination=" + pushImage(c, r),
		"--insecure", // 集群内仓库是明文 HTTP
		"--cache=false",
	}
	keys := make([]string, 0, len(dockerArgs))
	for k := range dockerArgs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--build-arg="+k+"="+dockerArgs[k])
	}

	res := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: mustQty("100m"), corev1.ResourceMemory: mustQty("256Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: mustQty("2Gi")},
	}
	ws := corev1.VolumeMount{Name: "workspace", MountPath: workspaceDir}
	caMount := corev1.VolumeMount{Name: "ca", MountPath: caDir, ReadOnly: true}

	gitEnv := []corev1.EnvVar{{Name: "REPO", Value: r.RepoURL}, {Name: "REF", Value: r.Ref}, {Name: "BUILD_DOCKERFILE_TEXT", Value: r.Dockerfile}}
	if r.Script != "" {
		gitEnv = append(gitEnv, corev1.EnvVar{Name: "BUILD_SCRIPT_TEXT", Value: r.Script})
	}
	inits := []corev1.Container{{
		Name: "git", Image: c.gitImage, Command: []string{"sh", "-ec", checkoutScript + writeRecipe},
		Env: gitEnv, VolumeMounts: []corev1.VolumeMount{ws, caMount}, Resources: res,
	}}
	if r.Script != "" { // 构建脚本在构建环境镜像里以 sh -e 运行,工作目录是 context_path,产物留在那里供 Dockerfile 使用
		inits = append(inits, corev1.Container{
			Name: "script", Image: r.BuilderImage, WorkingDir: ctxDir, Command: []string{"sh", "-c", "exec sh -e " + scriptFile + " 2>&1"}, // stderr 并进 stdout:否则容器日志里两者的先后顺序不固定,失败信息可能取错行
			Env: envVars(scriptEnv), VolumeMounts: []corev1.VolumeMount{ws, caMount}, Resources: res,
		})
	}

	deadline := int64(c.buildTimeout.Seconds())
	backoff, ttl := int32(0), int32(3600)
	optional, noToken := true, false
	sizeLimit := mustQty("4Gi")
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
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: &noToken, // 构建脚本是任意代码,不要给它集群的访问令牌
					InitContainers:               inits,
					Containers: []corev1.Container{{
						Name: "kaniko", Image: c.kanikoImage, Args: args, Resources: res,
						VolumeMounts: []corev1.VolumeMount{ws},
					}},
					Volumes: []corev1.Volume{
						{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &sizeLimit}}},
					},
				},
			},
		},
	}
	pod := &job.Spec.Template.Spec
	if c.caConfigMap != "" { // 可选的额外 CA(沙箱里出口 HTTPS 被中间人):ConfigMap 不存在也不影响
		pod.Volumes = append(pod.Volumes, corev1.Volume{Name: "ca", VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: c.caConfigMap}, Optional: &optional}}})
		pod.Containers[0].VolumeMounts = append(pod.Containers[0].VolumeMounts, corev1.VolumeMount{
			Name: "ca", MountPath: "/kaniko/ssl/certs/build-ca.crt", SubPath: "build-ca.crt", ReadOnly: true})
	} else { // 没配 CA:git 和 script 容器不需要那个挂载
		for i := range pod.InitContainers {
			pod.InitContainers[i].VolumeMounts = pod.InitContainers[i].VolumeMounts[:1]
		}
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
			// 同一个 (环境, 服务) 的运行按提交顺序串行:前面还有没结束的,就继续排队
			if blocker := blockingRun(runs, r); blocker != 0 {
				rn.markQueued(ctx, r, fmt.Sprintf("queued: waiting for run %d (same environment and service)", blocker))
				continue
			}
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

// 构建环境镜像拉取失败等"永远起不来"的问题,超过这个时间就提前失败,不用干等到构建超时。
const pullGrace = 3 * time.Minute

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
		// 超时由 Job 自己的 activeDeadlineSeconds 保证;这里只处理"镜像拉不下来"这类永远起不来的情况
		if pod := rn.buildPod(ctx, r.ID); pod != nil && r.PhaseStartedAt != nil && time.Since(*r.PhaseStartedAt) > pullGrace {
			if prob := pullProblem(pod); prob != "" {
				combined, _ := rn.stepLogs(ctx, pod)
				rn.deleteJob(ctx, r.ID)
				rn.fail(ctx, r, "build cannot start: "+prob, &combined)
			}
		}
		return
	}
	pod := rn.buildPod(ctx, r.ID)
	combined, bySteps := rn.stepLogs(ctx, pod)
	rn.deleteJob(ctx, r.ID)
	if !ok {
		rn.fail(ctx, r, failureMessage(pod, reason, bySteps, combined), &combined)
		return
	}
	log := combined
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

// buildPod 返回这次运行的构建 Pod(没有则 nil)。
func (rn *Runner) buildPod(ctx context.Context, id int64) *corev1.Pod {
	pods, err := rn.kube.CoreV1().Pods(rn.cfg.namespace).List(ctx, metav1.ListOptions{LabelSelector: "deployer/run=" + strconv.FormatInt(id, 10)})
	if err != nil || len(pods.Items) == 0 {
		return nil
	}
	return &pods.Items[0]
}

// stepLabel 把容器名转成对用户有意义的步骤名。
func stepLabel(container string) string {
	switch container {
	case "git":
		return "checkout"
	case "kaniko":
		return "image"
	}
	return container // script
}

// stepLogs 按步骤(init 容器在前、主容器在后)取日志,各取末尾一段,去掉终端颜色码。
// 返回按步骤分段拼好的完整日志,和每个容器各自的日志(失败诊断用)。
func (rn *Runner) stepLogs(ctx context.Context, pod *corev1.Pod) (string, map[string]string) {
	by := map[string]string{}
	if pod == nil {
		return "", by
	}
	names := []string{}
	for _, c := range pod.Spec.InitContainers {
		names = append(names, c.Name)
	}
	for _, c := range pod.Spec.Containers {
		names = append(names, c.Name)
	}
	var sb strings.Builder
	tail, limit := int64(300), int64(32768)
	for _, n := range names {
		raw, err := rn.kube.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: n, TailLines: &tail, LimitBytes: &limit}).Do(ctx).Raw()
		if err != nil { // 还没启动的步骤(前面的步骤失败了)没有日志
			continue
		}
		text := ansiRe.ReplaceAllString(string(raw), "")
		by[n] = text
		fmt.Fprintf(&sb, "=== step: %s ===\n%s\n", stepLabel(n), strings.TrimRight(text, "\n"))
	}
	return sb.String(), by
}

// failedStep 找出第一个以非 0 退出的步骤(init 容器优先,因为它们按顺序执行)。
func failedStep(pod *corev1.Pod) (name string, code int32, reason string) {
	if pod == nil {
		return "", 0, ""
	}
	for _, group := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, cs := range group {
			if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
				return cs.Name, t.ExitCode, t.Reason
			}
		}
	}
	return "", 0, ""
}

// pullProblem 检查容器是否卡在"镜像拉不下来/配置错误"这类不会自己好的状态。
func pullProblem(pod *corev1.Pod) string {
	for _, group := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, cs := range group {
			if w := cs.State.Waiting; w != nil {
				switch w.Reason {
				case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "CreateContainerConfigError", "CreateContainerError":
					return fmt.Sprintf("step %q: %s: %s", stepLabel(cs.Name), w.Reason, clip(w.Message, 160))
				}
			}
		}
	}
	return ""
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// lastLine 取一段日志里最后一个非空行。
func lastLine(log string) string {
	lines := strings.Split(strings.TrimRight(log, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return clip(l, 200)
		}
	}
	return ""
}

// failureMessage 生成失败说明:挂在哪一步、退出码、以及日志里的真正原因。
// 原因优先取失败那一步自己的日志(kaniko 的 Error:、git 的 fatal:、Maven 的 [ERROR]),取不到再看整份日志,
// 最后退而求其次用那一步的最后一行(比如 npm、make 之类的构建脚本)。
func failureMessage(pod *corev1.Pod, jobReason string, bySteps map[string]string, combined string) string {
	if strings.HasPrefix(jobReason, "DeadlineExceeded") {
		return "build timed out (" + jobReason + ")"
	}
	step, code, reason := failedStep(pod)
	if step == "" {
		msg := "build failed: " + jobReason
		if e := lastError(combined); e != "" {
			msg += " — " + e
		}
		return msg
	}
	msg := fmt.Sprintf("build failed at step %q (exit code %d", stepLabel(step), code)
	if reason != "" && reason != "Error" {
		msg += ", " + reason
	}
	msg += ")"
	detail := lastError(bySteps[step])
	if detail == "" {
		detail = lastError(combined)
	}
	if detail == "" {
		detail = lastLine(bySteps[step])
	}
	if detail != "" {
		msg += " — " + detail
	}
	return msg
}

// LiveLog 供 API 使用:构建中返回各步骤日志的尾部,结束后用数据库里保存的。
func (rn *Runner) LiveLog(ctx context.Context, id int64) string {
	combined, _ := rn.stepLogs(ctx, rn.buildPod(ctx, id))
	return combined
}

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

// lastError 从构建日志里提取失败原因,最多 200 个字符:
//  1. kaniko 自己的错误:最后一条 "Error: ..." 行;
//  2. git 拉源码失败:最后一条 "fatal: ..." 行;
//  3. Maven:没有上面那些行时,取 "BUILD FAILURE" 之后的第一条有内容的 "[ERROR] ..." 行(Java 构建出错主要是这个)。
func lastError(log string) string {
	lines := strings.Split(log, "\n")
	clip := func(l string) string {
		if len(l) > 200 {
			return l[:200] + "…"
		}
		return l
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); strings.HasPrefix(l, "Error:") {
			return clip(l)
		}
	}
	for i := len(lines) - 1; i >= 0; i-- { // git 拉源码失败:fatal: ...
		if l := strings.TrimSpace(lines[i]); strings.HasPrefix(l, "fatal:") {
			return clip(l)
		}
	}
	for i, l := range lines {
		if !strings.Contains(l, "BUILD FAILURE") {
			continue
		}
		for _, m := range lines[i+1:] {
			m = strings.TrimSpace(m)
			if strings.HasPrefix(m, "[ERROR]") && strings.TrimSpace(strings.TrimPrefix(m, "[ERROR]")) != "" {
				return clip(m)
			}
		}
	}
	return ""
}

// blockingRun 返回排在 r 前面、且部署目标与 r 相同((环境, 服务) 一样)的未结束运行的 id,0 表示没有。
// runs 必须是 ActiveRuns 的结果(只含未结束的,按 id 升序)。已请求取消的运行马上会结束,不算阻塞。
// 这样同一个部署上的运行严格按提交顺序依次构建和部署,后提交的一定最后生效,不会出现旧代码覆盖新代码。
func blockingRun(runs []Run, r Run) int64 {
	for _, o := range runs {
		if o.ID >= r.ID {
			break
		}
		if o.Env == r.Env && o.Service == r.Service && !o.CancelRequested {
			return o.ID
		}
	}
	return 0
}

// markQueued 把排队原因写进 message(只在变化时写,避免每轮都写库)。
func (rn *Runner) markQueued(ctx context.Context, r Run, msg string) {
	if r.Message == msg {
		return
	}
	if _, err := rn.store.UpdateRun(ctx, r.ID, RunPending, RunUpdate{Message: &msg}); err != nil {
		rn.log.Error("update run", "run", r.ID, "err", err)
	}
}
