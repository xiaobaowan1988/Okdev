package controller

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
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
	cfg := Config{ExecutorImage: "exec:1", KanikoImage: "kaniko:1", DefaultTimeout: time.Minute}
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
	var steps []string
	for _, c := range pod.InitContainers {
		steps = append(steps, c.Name)
	}
	if len(steps) != 3 || steps[0] != "checkout" || steps[1] != "prepare" || steps[2] != "build" {
		t.Fatalf("unexpected init steps %v", steps)
	}
	if pod.InitContainers[2].Image != "kaniko:1" || pod.Containers[0].Name != "release" || pod.Containers[0].Image != "exec:1" {
		t.Fatal("wrong images")
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatal("user build code must not get a service account token")
	}
	if j.Spec.Template.Annotations[AnnotationTask] != "t1" || j.Spec.Template.Labels[LabelManagedBy] != ManagedByValue {
		t.Fatal("pod template must carry task annotation and managed-by label")
	}
}

func TestBuildJobExtraEnvCAAndSecret(t *testing.T) {
	cfg := Config{ExecutorImage: "exec:1", KanikoImage: "kaniko:1", DefaultTimeout: time.Minute,
		ExtraEnv:    []corev1.EnvVar{{Name: "REGISTRY_PUSH", Value: "reg:5000"}},
		CAConfigMap: "build-ca", GitOpsSecret: "gitops-token"}
	pod := BuildJob(sampleTask(), cfg).Spec.Template.Spec
	for _, c := range append(append([]corev1.Container{}, pod.InitContainers[:2]...), pod.Containers...) {
		found := false
		for _, e := range c.Env {
			found = found || (e.Name == "REGISTRY_PUSH" && e.Value == "reg:5000")
		}
		if !found {
			t.Fatalf("%s missing extra env", c.Name)
		}
	}
	if len(pod.Volumes) != 3 {
		t.Fatalf("want workspace+ca+gitops volumes, got %d", len(pod.Volumes))
	}
	if len(pod.InitContainers[2].VolumeMounts) != 2 {
		t.Fatal("kaniko must get the CA mount")
	}
}

func TestBuildJobOverridesAndInjectionSafety(t *testing.T) {
	task := sampleTask()
	task.Spec.TimeoutSeconds = 120
	task.Spec.Executor = &v1.ExecutorSpec{Image: "custom:2", Command: []string{"/run"}}
	task.Spec.Git.Ref = `main; rm -rf /`
	j := BuildJob(task, Config{ExecutorImage: "exec:1", KanikoImage: "kaniko:1", DefaultTimeout: time.Minute})

	if *j.Spec.ActiveDeadlineSeconds != 120 {
		t.Fatal("spec timeout must win")
	}
	c := j.Spec.Template.Spec.Containers[0]
	if c.Image != "custom:2" || c.Command[0] != "/run" || c.Args != nil {
		t.Fatal("executor override ignored")
	}
	for _, ic := range j.Spec.Template.Spec.InitContainers {
		for _, a := range append(append([]string{}, ic.Command...), ic.Args...) {
			if contains(a, "rm -rf") {
				t.Fatal("user input leaked into command line")
			}
		}
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
