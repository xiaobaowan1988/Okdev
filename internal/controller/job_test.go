package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/xiaobaowan1988/okdev/internal/api/v1"
)

func sampleTask() *v1.ReleaseTask {
	return &v1.ReleaseTask{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: "dev", UID: "uid-1"},
		Spec: v1.ReleaseTaskSpec{
			Service:     v1.ServiceRef{Name: "web", Kind: "Deployment"},
			Environment: "dev",
			Git:         v1.GitSpec{Repo: "https://example.com/r.git", Ref: "main"},
		},
	}
}

func TestBuildJob(t *testing.T) {
	cfg := Config{ExecutorImage: "exec:1", GitImage: "git:1", DefaultTimeout: time.Minute}
	j := BuildJob(sampleTask(), cfg)

	if j.Namespace != "dev" || j.Name != "rt-t1" {
		t.Fatalf("unexpected identity %s/%s", j.Namespace, j.Name)
	}
	if o := j.OwnerReferences[0]; o.Kind != v1.Kind || o.UID != "uid-1" || !*o.Controller {
		t.Fatalf("bad owner %+v", o)
	}
	if *j.Spec.BackoffLimit != 0 || *j.Spec.ActiveDeadlineSeconds != 60 {
		t.Fatal("job must not retry and must use default timeout")
	}
	pod := j.Spec.Template.Spec
	if pod.Containers[0].Image != "exec:1" || pod.InitContainers[0].Image != "git:1" {
		t.Fatal("wrong images")
	}
	if j.Spec.Template.Annotations[AnnotationTask] != "t1" || j.Spec.Template.Labels[LabelManagedBy] != ManagedByValue {
		t.Fatal("pod template must carry task annotation and managed-by label")
	}
}

func TestBuildJobOverridesAndInjectionSafety(t *testing.T) {
	task := sampleTask()
	task.Spec.TimeoutSeconds = 120
	task.Spec.Executor = &v1.ExecutorSpec{Image: "custom:2", Command: []string{"/run"}}
	task.Spec.Git.Ref = `main; rm -rf /`
	j := BuildJob(task, Config{ExecutorImage: "exec:1", GitImage: "git:1", DefaultTimeout: time.Minute})

	if *j.Spec.ActiveDeadlineSeconds != 120 {
		t.Fatal("spec timeout must win")
	}
	c := j.Spec.Template.Spec.Containers[0]
	if c.Image != "custom:2" || c.Command[0] != "/run" {
		t.Fatal("executor override ignored")
	}
	script := j.Spec.Template.Spec.InitContainers[0].Command[2]
	if contains(script, "rm -rf") {
		t.Fatal("user input leaked into shell script")
	}
	var ref string
	for _, e := range c.Env {
		if e.Name == "GIT_REF" {
			ref = e.Value
		}
	}
	if ref != task.Spec.Git.Ref {
		t.Fatal("ref must be passed via env")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestValidate(t *testing.T) {
	ok := sampleTask().Spec
	if msg := ok.Validate(); msg != "" {
		t.Fatalf("valid spec rejected: %s", msg)
	}
	bad := ok
	bad.Service.Kind = "Pod"
	if bad.Validate() == "" {
		t.Fatal("unknown kind accepted")
	}
	bad = ok
	bad.Git.Ref = "--upload-pack=x"
	if bad.Validate() == "" {
		t.Fatal("option-like ref accepted")
	}
}
