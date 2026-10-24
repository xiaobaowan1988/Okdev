package main

import (
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

func testCfg() buildConfig {
	return buildConfig{namespace: "deployer-build", registryPush: "registry.deployer-build.svc:5000", registryPull: "localhost:30500",
		kanikoImage: "kaniko:test", gitImage: "git:test", caConfigMap: "build-ca", buildTimeout: 15 * time.Minute, deployTimeout: 10 * time.Minute, concurrency: 2}
}

func TestValidateRepoRef(t *testing.T) {
	for _, ok := range [][2]string{{"https://github.com/o/r.git", "master"}, {"http://git.local/o/r", "feature/x"}, {"https://github.com/o/r", "refs/tags/v1"}, {"https://github.com/o/r", "0a1b2c3d4e5f"}} {
		if err := validateRepoRef(ok[0], ok[1]); err != nil {
			t.Errorf("%v should be valid: %v", ok, err)
		}
	}
	for _, bad := range [][2]string{{"git@github.com:o/r.git", "master"}, {"ftp://x/y", "master"}, {"https://", "master"}, {"https://github.com/o/r", "a b"}, {"https://github.com/o/r", "a/../b"}, {"https://github.com/o/r", "x;rm"}, {"https://github.com/o/r", "--upload-pack=x"}, {"https://github.com/o/r", ""}} {
		if err := validateRepoRef(bad[0], bad[1]); err == nil {
			t.Errorf("expected error for %v", bad)
		}
	}
}

func container(cs []corev1.Container, name string) *corev1.Container {
	for i := range cs {
		if cs[i].Name == name {
			return &cs[i]
		}
	}
	return nil
}

func envMap(c *corev1.Container) map[string]string {
	m := map[string]string{}
	for _, e := range c.Env {
		m[e.Name] = e.Value
	}
	return m
}

func appRun() Run {
	return Run{ID: 7, Service: "api", Env: "dev3", Ref: "master", RepoURL: "https://github.com/o/r.git", ContextPath: "src/api",
		Dockerfile: "FROM scratch\nCOPY dist/api /api\n", Profile: "prod", BuilderImage: "golang:1.24-alpine", Script: "echo \"$HOME\" $(date)\ngo build -o dist/api .",
		BuildArgs: map[string]string{"FOO": "bar", "VERSION": "user-supplied", "PROFILE": "user-profile"}}
}

// builder 配了脚本:三步(git → script → kaniko),脚本在构建环境镜像里运行,Dockerfile 来自 builder。
func TestBuildJobWithScript(t *testing.T) {
	job, err := buildJob(appRun(), testCfg())
	if err != nil {
		t.Fatal(err)
	}
	if job.Name != "build-7" || job.Namespace != "deployer-build" || job.Labels["deployer/run"] != "7" {
		t.Fatalf("bad identity: %s/%s %v", job.Namespace, job.Name, job.Labels)
	}
	pod := job.Spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("build pods run arbitrary scripts and must not get a service account token")
	}
	if len(pod.InitContainers) != 2 || pod.InitContainers[0].Name != "git" || pod.InitContainers[1].Name != "script" {
		t.Fatalf("expected init containers [git script], got %+v", pod.InitContainers)
	}
	git, script, kaniko := &pod.InitContainers[0], &pod.InitContainers[1], &pod.Containers[0]
	if git.Image != "git:test" || script.Image != "golang:1.24-alpine" || kaniko.Image != "kaniko:test" {
		t.Errorf("images: %s %s %s", git.Image, script.Image, kaniko.Image)
	}
	if script.WorkingDir != "/workspace/src/api" {
		t.Errorf("script must run in the context dir, got %q", script.WorkingDir)
	}
	// 脚本内容存在数据库里:由 git 步骤写成文件,script 步骤以 sh -e 运行它(内容不进命令行,也不进脚本容器的环境变量)
	if strings.Join(script.Command, "|") != "sh|-e|/workspace/.deployer/build.sh" {
		t.Errorf("script command: %v", script.Command)
	}
	if _, ok := envMap(script)["BUILD_SCRIPT_TEXT"]; ok {
		t.Error("script content must only be visible to the git step that writes it")
	}
	// 环境变量:保留名由执行器注入,覆盖用户同名参数;用户参数同时可见
	env := envMap(script)
	for k, want := range map[string]string{"VERSION": "r7", "PROFILE": "prod", "GIT_REF": "master", "SERVICE": "api", "DEPLOY_ENV": "dev3",
		"WORKSPACE": "/workspace/src/api", "BUILD_EXTRA_CA_FILE": "/etc/build-ca/extra-ca.crt", "FOO": "bar"} {
		if env[k] != want {
			t.Errorf("script env %s = %q, want %q", k, env[k], want)
		}
	}
	// kaniko:上下文是共享目录,Dockerfile 用绝对路径,构建参数与脚本环境一致
	args := strings.Join(kaniko.Args, " ")
	for _, want := range []string{"--context=dir:///workspace/src/api", "--dockerfile=/workspace/.deployer/Dockerfile",
		"--destination=registry.deployer-build.svc:5000/api:r7", "--insecure", "--build-arg=FOO=bar", "--build-arg=PROFILE=prod",
		"--build-arg=VERSION=r7", "--build-arg=GIT_REF=master"} {
		if !strings.Contains(args, want) {
			t.Errorf("kaniko args missing %q in: %s", want, args)
		}
	}
	if strings.Contains(args, "user-supplied") || strings.Contains(args, "user-profile") {
		t.Error("reserved vars must not be overridable by build_args")
	}
	// git 步骤的仓库和 ref 通过环境变量传入(不拼进脚本),避免注入
	if e := envMap(git); e["REPO"] != "https://github.com/o/r.git" || e["REF"] != "master" || strings.Contains(git.Command[2], "o/r.git") {
		t.Errorf("git env/command: %v / %q", e, git.Command[2])
	}
	// Dockerfile 和脚本的内容经环境变量传给 git 步骤,由 printf '%s' 写文件(不拼进命令,避免引号/$ 注入)
	ge := envMap(git)
	if ge["BUILD_DOCKERFILE_TEXT"] != appRun().Dockerfile || ge["BUILD_SCRIPT_TEXT"] != appRun().Script {
		t.Errorf("recipe content env: %v", ge)
	}
	if strings.Contains(git.Command[2], "COPY dist") || strings.Contains(git.Command[2], "go build") ||
		!strings.Contains(git.Command[2], `printf '%s\n' "$BUILD_DOCKERFILE_TEXT" > /workspace/.deployer/Dockerfile`) {
		t.Errorf("git command should only reference the env vars: %q", git.Command[2])
	}
	if *job.Spec.ActiveDeadlineSeconds != 900 || *job.Spec.BackoffLimit != 0 {
		t.Errorf("bad deadline/backoff: %d %d", *job.Spec.ActiveDeadlineSeconds, *job.Spec.BackoffLimit)
	}
	var ca *corev1.Volume
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == "ca" {
			ca = &pod.Volumes[i]
		}
	}
	if ca == nil || !*ca.ConfigMap.Optional {
		t.Errorf("CA volume must exist and be optional: %+v", pod.Volumes)
	}
}

// 没配脚本:只有 git → kaniko,Dockerfile 内容照样来自数据库,没有 script 步骤。
func TestBuildJobLegacyNoApp(t *testing.T) {
	r := Run{ID: 3, Service: "api", Ref: "master", RepoURL: "https://github.com/o/r.git", ContextPath: ".", Dockerfile: "FROM busybox\n"}
	cfg := testCfg()
	cfg.caConfigMap = ""
	job, err := buildJob(r, cfg)
	if err != nil {
		t.Fatal(err)
	}
	pod := job.Spec.Template.Spec
	if len(pod.InitContainers) != 1 || pod.InitContainers[0].Name != "git" {
		t.Fatalf("legacy builder must have only the git step: %+v", pod.InitContainers)
	}
	a := strings.Join(pod.Containers[0].Args, " ")
	if !strings.Contains(a, "--context=dir:///workspace ") || !strings.Contains(a, "--dockerfile=/workspace/.deployer/Dockerfile ") || strings.Contains(a, "--build-arg=PROFILE") {
		t.Errorf("legacy args: %s", a)
	}
	// 没配 CA:不挂 CA 卷,git 容器不需要那个挂载
	if _, ok := envMap(&pod.InitContainers[0])["BUILD_SCRIPT_TEXT"]; ok {
		t.Error("no script: BUILD_SCRIPT_TEXT must not be set")
	}
	if len(pod.Volumes) != 1 || len(pod.InitContainers[0].VolumeMounts) != 1 {
		t.Errorf("without CA only the workspace volume is expected: %+v", pod.Volumes)
	}
}

func TestBuildJobRejectsBadInput(t *testing.T) {
	for name, mutate := range map[string]func(*Run){
		"bad context path":   func(r *Run) { r.ContextPath = "../etc" },
		"absolute context":   func(r *Run) { r.ContextPath = "/etc" },
		"empty dockerfile":   func(r *Run) { r.Dockerfile = "  \n" },
		"bad build arg name": func(r *Run) { r.BuildArgs = map[string]string{"bad name": "x"} },
		"bad repo":           func(r *Run) { r.RepoURL = "git@github.com:o/r.git" },
		"bad ref":            func(r *Run) { r.Ref = "a;b" },
	} {
		r := appRun()
		mutate(&r)
		if _, err := buildJob(r, testCfg()); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestImages(t *testing.T) {
	r := Run{ID: 12, Service: "api"}
	c := testCfg()
	if pushImage(c, r) != "registry.deployer-build.svc:5000/api:r12" || pullImage(c, r) != "localhost:30500/api:r12" {
		t.Errorf("push=%s pull=%s", pushImage(c, r), pullImage(c, r))
	}
}

func TestValidateSteps(t *testing.T) {
	for _, ok := range [][]string{{"build"}, {"build", "deploy"}} {
		if err := validateSteps(ok); err != nil {
			t.Errorf("%v should be valid: %v", ok, err)
		}
	}
	for _, bad := range [][]string{{}, {"deploy"}, {"deploy", "build"}, {"build", "build"}, {"build", "deploy", "deploy"}, {"build", "test"}} {
		if err := validateSteps(bad); err == nil {
			t.Errorf("%v should be invalid", bad)
		}
	}
}

func TestJobOutcome(t *testing.T) {
	cond := func(t batchv1.JobConditionType) []batchv1.JobCondition {
		return []batchv1.JobCondition{{Type: t, Status: corev1.ConditionTrue, Reason: "R", Message: "M"}}
	}
	if done, _, _ := jobOutcome(&batchv1.Job{}); done {
		t.Error("empty job is not done")
	}
	if d, ok, _ := jobOutcome(&batchv1.Job{Status: batchv1.JobStatus{Conditions: cond(batchv1.JobComplete)}}); !d || !ok {
		t.Error("Complete condition = success")
	}
	if d, ok, why := jobOutcome(&batchv1.Job{Status: batchv1.JobStatus{Conditions: cond(batchv1.JobFailed)}}); !d || ok || why != "R: M" {
		t.Errorf("Failed condition = failure with reason, got %v %v %q", d, ok, why)
	}
	if d, ok, _ := jobOutcome(&batchv1.Job{Status: batchv1.JobStatus{Succeeded: 1}}); !d || !ok {
		t.Error("succeeded counter = success")
	}
	if d, ok, _ := jobOutcome(&batchv1.Job{Status: batchv1.JobStatus{Failed: 1}}); !d || ok {
		t.Error("failed counter = failure")
	}
}

func TestDeployVerdict(t *testing.T) {
	now := time.Now()
	started := now.Add(-time.Minute)
	r := Run{ID: 3, Image: "localhost:30500/api:r3", DeployVersion: 5, PhaseStartedAt: &started}
	ready := EnvService{Image: r.Image, Version: 5, AppliedVersion: 5, ObservedSync: "Synced", ObservedHealth: "Healthy", DesiredState: "running"}
	if s, _ := deployVerdict(r, ready, now, 10*time.Minute); s != "ok" {
		t.Errorf("ready deployment should be ok, got %s", s)
	}
	notApplied := ready
	notApplied.AppliedVersion = 4 // 对账循环还没应用这一版:即使旧状态是 Synced/Healthy 也不能算成功
	if s, _ := deployVerdict(r, notApplied, now, 10*time.Minute); s != "wait" {
		t.Errorf("unapplied version must wait, got %s", s)
	}
	progressing := ready
	progressing.ObservedHealth = "Progressing"
	if s, _ := deployVerdict(r, progressing, now, 10*time.Minute); s != "wait" {
		t.Errorf("progressing must wait, got %s", s)
	}
	if s, msg := deployVerdict(r, progressing, now.Add(20*time.Minute), 10*time.Minute); s != "fail" || !strings.Contains(msg, "timed out") {
		t.Errorf("timeout must fail, got %s %q", s, msg)
	}
	superseded := ready
	superseded.Image, superseded.Version = "other:image", 6
	if s, _ := deployVerdict(r, superseded, now, 10*time.Minute); s != "fail" {
		t.Errorf("superseded must fail, got %s", s)
	}
	removed := ready
	removed.DesiredState = "deleted"
	if s, _ := deployVerdict(r, removed, now, 10*time.Minute); s != "fail" {
		t.Errorf("removed must fail, got %s", s)
	}
}

func TestLastError(t *testing.T) {
	log := "INFO building\nError: error resolving source context: reference not found\nUsage:\n  executor [flags]\n      --push-ignore-immutable-tag-errors  If true, known errors are ignored\n"
	if got := lastError(log); got != "Error: error resolving source context: reference not found" {
		t.Errorf("got %q", got)
	}
	if lastError("INFO ok\nINFO done") != "" {
		t.Error("no error line must give empty string")
	}
	if got := lastError("Error: " + strings.Repeat("x", 500)); len(got) > 210 {
		t.Errorf("must be truncated, len=%d", len(got))
	}
	if got := lastError("Error: first\nError: second"); got != "Error: second" {
		t.Errorf("must pick the last error, got %q", got)
	}
}

func TestBlockingRun(t *testing.T) {
	runs := []Run{
		{ID: 3, Env: "dev3", Service: "api"},
		{ID: 4, Env: "dev5", Service: "api"},                        // 不同环境:互不影响
		{ID: 5, Env: "dev3", Service: "web"},                        // 不同服务:互不影响
		{ID: 6, Env: "dev3", Service: "api"},                        // 被 3 阻塞
		{ID: 7, Env: "dev3", Service: "api"},                        // 被 3 阻塞(最先的那个)
		{ID: 8, Env: "dev3", Service: "api", CancelRequested: true}, // 已请求取消:不阻塞别人
		{ID: 9, Env: "dev3", Service: "api"},
	}
	byID := func(id int64) Run {
		for _, r := range runs {
			if r.ID == id {
				return r
			}
		}
		t.Fatalf("no run %d", id)
		return Run{}
	}
	for id, want := range map[int64]int64{3: 0, 4: 0, 5: 0, 6: 3, 7: 3, 9: 3} {
		if got := blockingRun(runs, byID(id)); got != want {
			t.Errorf("run %d blocked by %d, want %d", id, got, want)
		}
	}
	// 排在前面的运行结束(从活动列表里消失)后,下一个就不再被阻塞
	if got := blockingRun(runs[1:], byID(6)); got != 0 {
		t.Errorf("run 6 should be free once run 3 is gone, got %d", got)
	}
	// 只有已请求取消的运行在前面时,不阻塞
	if got := blockingRun([]Run{{ID: 1, Env: "e", Service: "s", CancelRequested: true}, {ID: 2, Env: "e", Service: "s"}}, Run{ID: 2, Env: "e", Service: "s"}); got != 0 {
		t.Errorf("a cancelled blocker must not block, got %d", got)
	}
}

func TestLastErrorMaven(t *testing.T) {
	mvn := "[INFO] Building java-api 1.0\n[INFO] BUILD FAILURE\n[INFO] ------\n[ERROR] Plugin org.apache.maven.plugins:maven-resources-plugin:3.4.0 could not be resolved: PKIX path building failed\n[ERROR] \n[ERROR] -> [Help 1]\nerror building image: exit status 1\n"
	if got := lastError(mvn); got != "[ERROR] Plugin org.apache.maven.plugins:maven-resources-plugin:3.4.0 could not be resolved: PKIX path building failed" {
		t.Errorf("got %q", got)
	}
	// kaniko 自己的 Error: 行优先于 Maven 的 [ERROR]
	if got := lastError(mvn + "Error: error resolving dockerfile path\n"); got != "Error: error resolving dockerfile path" {
		t.Errorf("kaniko error must win, got %q", got)
	}
	// 只有 [ERROR] 但没有 BUILD FAILURE:不乱猜
	if lastError("[ERROR] something\n") != "" {
		t.Error("[ERROR] without BUILD FAILURE must be ignored")
	}
	// 空的 [ERROR] 行要跳过
	if got := lastError("BUILD FAILURE\n[ERROR] \n[ERROR] real cause\n"); got != "[ERROR] real cause" {
		t.Errorf("must skip empty [ERROR] lines, got %q", got)
	}
}

func podWith(init, main []corev1.ContainerStatus) *corev1.Pod {
	return &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: init, ContainerStatuses: main}}
}

func terminated(name string, code int32, reason string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Reason: reason}}}
}

func TestFailedStep(t *testing.T) {
	if n, _, _ := failedStep(nil); n != "" {
		t.Error("nil pod has no failed step")
	}
	p := podWith([]corev1.ContainerStatus{terminated("git", 0, "Completed"), terminated("script", 2, "Error")}, []corev1.ContainerStatus{{Name: "kaniko"}})
	if n, c, _ := failedStep(p); n != "script" || c != 2 {
		t.Errorf("got %s %d", n, c)
	}
	if n, c, r := failedStep(podWith([]corev1.ContainerStatus{terminated("git", 0, "Completed"), terminated("script", 0, "Completed")}, []corev1.ContainerStatus{terminated("kaniko", 1, "Error")})); n != "kaniko" || c != 1 || r != "Error" {
		t.Errorf("main container failure: %s %d %s", n, c, r)
	}
	if n, _, r := failedStep(podWith([]corev1.ContainerStatus{terminated("script", 137, "OOMKilled")}, nil)); n != "script" || r != "OOMKilled" {
		t.Errorf("oom: %s %s", n, r)
	}
}

func TestPullProblem(t *testing.T) {
	waiting := func(name, reason string) corev1.ContainerStatus {
		return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: "back-off pulling image"}}}
	}
	if got := pullProblem(podWith([]corev1.ContainerStatus{waiting("script", "ImagePullBackOff")}, nil)); !strings.Contains(got, `step "script"`) || !strings.Contains(got, "ImagePullBackOff") {
		t.Errorf("got %q", got)
	}
	if pullProblem(podWith([]corev1.ContainerStatus{waiting("script", "PodInitializing")}, nil)) != "" {
		t.Error("normal waiting states are not problems")
	}
	if pullProblem(podWith(nil, nil)) != "" {
		t.Error("empty pod has no problem")
	}
}

func TestFailureMessage(t *testing.T) {
	scriptFailed := podWith([]corev1.ContainerStatus{terminated("git", 0, "Completed"), terminated("script", 1, "Error")}, nil)
	mvn := "[INFO] BUILD FAILURE\n[ERROR] Failed to execute goal compile: Compilation failure\n"
	got := failureMessage(scriptFailed, "BackoffLimitExceeded: x", map[string]string{"script": mvn}, "=== step: script ===\n"+mvn)
	if got != `build failed at step "script" (exit code 1) — [ERROR] Failed to execute goal compile: Compilation failure` {
		t.Errorf("maven script failure: %q", got)
	}
	// 脚本既没有 Error:/fatal:/[ERROR],就用那一步的最后一行
	got = failureMessage(scriptFailed, "x", map[string]string{"script": "npm ERR! missing script: build\n"}, "")
	if got != `build failed at step "script" (exit code 1) — npm ERR! missing script: build` {
		t.Errorf("fallback to last line: %q", got)
	}
	// 拉源码失败
	gitFailed := podWith([]corev1.ContainerStatus{terminated("git", 128, "Error")}, nil)
	got = failureMessage(gitFailed, "x", map[string]string{"git": "fatal: couldn't find remote ref nope\n"}, "")
	if got != `build failed at step "checkout" (exit code 128) — fatal: couldn't find remote ref nope` {
		t.Errorf("git failure: %q", got)
	}
	// 内存不足
	oom := podWith([]corev1.ContainerStatus{terminated("script", 137, "OOMKilled")}, nil)
	if got := failureMessage(oom, "x", map[string]string{"script": "Killed\n"}, ""); !strings.Contains(got, "OOMKilled") || !strings.Contains(got, "137") {
		t.Errorf("oom: %q", got)
	}
	// 超时
	if got := failureMessage(scriptFailed, "DeadlineExceeded: Job was active longer than specified deadline", nil, ""); !strings.HasPrefix(got, "build timed out") {
		t.Errorf("timeout: %q", got)
	}
	// 没有 Pod 信息:退回到 Job 的原因
	if got := failureMessage(nil, "BackoffLimitExceeded: x", nil, "Error: boom\n"); got != "build failed: BackoffLimitExceeded: x — Error: boom" {
		t.Errorf("no pod: %q", got)
	}
}

func TestLastLineAndGitFatal(t *testing.T) {
	if lastLine("a\n\nb \n\n") != "b" || lastLine("") != "" {
		t.Error("lastLine")
	}
	if got := lastError("INFO x\nfatal: couldn't find remote ref nope\n"); got != "fatal: couldn't find remote ref nope" {
		t.Errorf("git fatal: %q", got)
	}
}

func TestEffectiveProfile(t *testing.T) {
	if effectiveProfile("dev", "prod") != "dev" || effectiveProfile("", "prod") != "prod" || effectiveProfile("", "") != "" {
		t.Error("requested profile wins, otherwise the builder default")
	}
}

func TestNormalizeBuilder(t *testing.T) {
	base := func() Builder {
		return Builder{Name: "b", Service: "s", RepoURL: "https://github.com/o/r.git", Dockerfile: "FROM scratch\nCOPY app /app\n"}
	}
	b := base()
	if msg := normalizeBuilder(&b); msg != "" || b.Ref != "master" || b.ContextPath != "." {
		t.Errorf("defaults (no script): %q %+v", msg, b)
	}
	b = base()
	b.Script, b.Image, b.Profile = "go build -o app .", "golang:1.24-alpine", "prod"
	if msg := normalizeBuilder(&b); msg != "" {
		t.Errorf("builder with script: %q", msg)
	}
	for name, mutate := range map[string]func(*Builder){
		"no dockerfile":        func(b *Builder) { b.Dockerfile = "" },
		"blank dockerfile":     func(b *Builder) { b.Dockerfile = " \n\t" },
		"dockerfile too large": func(b *Builder) { b.Dockerfile = strings.Repeat("x", maxScriptBytes+1) },
		"script without image": func(b *Builder) { b.Script = "x" },
		"image without script": func(b *Builder) { b.Image = "golang:1.24" },
		"script too large":     func(b *Builder) { b.Script, b.Image = strings.Repeat("x", maxScriptBytes+1), "golang:1.24" },
		"bad image reference":  func(b *Builder) { b.Script, b.Image = "x", "bad image!" },
		"bad profile":          func(b *Builder) { b.Script, b.Image, b.Profile = "x", "golang:1.24", "a b" },
		"bad context path":     func(b *Builder) { b.ContextPath = "../x" },
		"non-http repo":        func(b *Builder) { b.RepoURL = "git@github.com:o/r.git" },
		"bad build arg name":   func(b *Builder) { b.BuildArgs = map[string]string{"bad name": "x"} },
	} {
		b := base()
		mutate(&b)
		if msg := normalizeBuilder(&b); msg == "" {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

func TestValidRelPath(t *testing.T) {
	for _, ok := range []string{"Dockerfile", "ci/build.sh", "docker/Dockerfile.prod", "a-b_c/d.e"} {
		if !validRelPath(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "/abs", "../x", "a/../b", "a b", "a\tb", "a\nb"} {
		if validRelPath(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}
