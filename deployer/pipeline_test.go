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
		kanikoImage: "kaniko:test", caConfigMap: "build-ca", buildTimeout: 15 * time.Minute, deployTimeout: 10 * time.Minute, concurrency: 2}
}

func TestGitContext(t *testing.T) {
	cases := []struct{ repo, ref, want string }{
		{"https://github.com/o/r.git", "master", "git://github.com/o/r.git#refs/heads/master"},
		{"https://github.com/o/r", "feature/x", "git://github.com/o/r.git#refs/heads/feature/x"},
		{"https://github.com/o/r.git", "refs/tags/v1", "git://github.com/o/r.git#refs/tags/v1"},
	}
	for _, c := range cases {
		got, err := gitContext(c.repo, c.ref)
		if err != nil || got != c.want {
			t.Errorf("gitContext(%q,%q) = %q, %v; want %q", c.repo, c.ref, got, err, c.want)
		}
	}
	for _, bad := range [][2]string{{"git@github.com:o/r.git", "master"}, {"ftp://x/y", "master"}, {"https://github.com/o/r", "a b"}, {"https://github.com/o/r", "a/../b"}, {"https://github.com/o/r", "x;rm"}} {
		if _, err := gitContext(bad[0], bad[1]); err == nil {
			t.Errorf("expected error for %v", bad)
		}
	}
}

func TestBuildJob(t *testing.T) {
	r := Run{ID: 7, Service: "api", Ref: "master", RepoURL: "https://github.com/o/r.git", ContextPath: "src/api", Dockerfile: "Dockerfile",
		BuildArgs: map[string]string{"FOO": "bar", "VERSION": "user-supplied"}}
	job, err := buildJob(r, testCfg())
	if err != nil {
		t.Fatal(err)
	}
	if job.Name != "build-7" || job.Namespace != "deployer-build" || job.Labels["deployer/run"] != "7" {
		t.Fatalf("bad identity: %s/%s %v", job.Namespace, job.Name, job.Labels)
	}
	args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, " ")
	for _, want := range []string{
		"--context=git://github.com/o/r.git#refs/heads/master", "--context-sub-path=src/api", "--dockerfile=Dockerfile",
		"--destination=registry.deployer-build.svc:5000/api:r7", "--insecure", "--build-arg=FOO=bar", "--build-arg=GIT_REF=master",
		"--build-arg=VERSION=r7", // 执行器注入的 VERSION 必须覆盖用户传的同名参数
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q in: %s", want, args)
		}
	}
	if strings.Contains(args, "user-supplied") {
		t.Error("user-supplied VERSION must be overridden")
	}
	if *job.Spec.ActiveDeadlineSeconds != 900 || *job.Spec.BackoffLimit != 0 {
		t.Errorf("bad deadline/backoff: %d %d", *job.Spec.ActiveDeadlineSeconds, *job.Spec.BackoffLimit)
	}
	if v := job.Spec.Template.Spec.Volumes; len(v) != 1 || !*v[0].ConfigMap.Optional {
		t.Errorf("CA volume must be optional: %+v", v)
	}
	// 构建目录是 "." 时不应带 --context-sub-path;没配 CA 时不挂卷
	r.ContextPath = "."
	cfg := testCfg()
	cfg.caConfigMap = ""
	job, _ = buildJob(r, cfg)
	if a := strings.Join(job.Spec.Template.Spec.Containers[0].Args, " "); strings.Contains(a, "context-sub-path") || len(job.Spec.Template.Spec.Volumes) != 0 {
		t.Errorf("unexpected sub-path/volume: %s", a)
	}
	// 非法构建参数名
	r.BuildArgs = map[string]string{"bad name": "x"}
	if _, err := buildJob(r, testCfg()); err == nil {
		t.Error("invalid build arg name must be rejected")
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
