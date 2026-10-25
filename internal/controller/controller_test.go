package controller

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"

	v1 "github.com/xiaobaowan1988/okdev/internal/api/v1"
)

func taskObject(t *v1.ReleaseTask) *unstructured.Unstructured {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(t)
	if err != nil {
		panic(err)
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetAPIVersion(v1.Group + "/" + v1.Version)
	u.SetKind(v1.Kind)
	return u
}

type env struct {
	c    *Controller
	dyn  *dynfake.FakeDynamicClient
	kube *kubefake.Clientset
	ctx  context.Context
}

func newEnv(t *testing.T, tasks ...*v1.ReleaseTask) *env {
	t.Helper()
	var objs []runtime.Object
	for _, task := range tasks {
		objs = append(objs, taskObject(task))
	}
	scheme := runtime.NewScheme()
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{v1.GVR: "ReleaseTaskList"}, objs...)
	kube := kubefake.NewSimpleClientset()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	c := New(Config{ExecutorImage: "exec:1", KanikoImage: "kaniko:1", DefaultTimeout: time.Minute},
		dyn, kube, record.NewFakeRecorder(100), "", 0)
	c.now = func() metav1.Time { return now }
	for _, s := range c.startFns {
		s(ctx.Done())
	}
	deadline := time.After(5 * time.Second)
	for _, f := range c.hasSyncs {
		for !f() {
			select {
			case <-deadline:
				t.Fatal("informers did not sync")
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	return &env{c: c, dyn: dyn, kube: kube, ctx: ctx}
}

func (e *env) status(t *testing.T, name string) v1.ReleaseTaskStatus {
	t.Helper()
	u, err := e.dyn.Resource(v1.GVR).Namespace("dev").Get(e.ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	task, err := v1.FromUnstructured(u)
	if err != nil {
		t.Fatal(err)
	}
	return task.Status
}

// waitSynced 等 informer 看到最新对象,再调谐。
func (e *env) reconcile(t *testing.T, name string, wantPhase v1.Phase) v1.ReleaseTaskStatus {
	t.Helper()
	var st v1.ReleaseTaskStatus
	for i := 0; i < 200; i++ {
		if err := e.c.Reconcile(e.ctx, "dev/"+name); err != nil {
			t.Fatal(err)
		}
		if st = e.status(t, name); st.Phase == wantPhase {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("phase never reached %s, last status %+v", wantPhase, st)
	return st
}

func TestReconcileLifecycle(t *testing.T) {
	e := newEnv(t, sampleTask())

	st := e.reconcile(t, "t1", v1.PhasePending)
	if st.JobName != "rt-t1" {
		t.Fatalf("job name not recorded: %+v", st)
	}
	job, err := e.kube.BatchV1().Jobs("dev").Get(e.ctx, "rt-t1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("job not created: %v", err)
	}

	// 执行 Pod 出现并运行 -> Running,并写回 Pod 名
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "rt-t1-abcde", Namespace: "dev",
			Labels:      map[string]string{LabelManagedBy: ManagedByValue, "job-name": "rt-t1"},
			Annotations: map[string]string{AnnotationTask: "t1"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if _, err := e.kube.CoreV1().Pods("dev").Create(e.ctx, p, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	st = e.reconcile(t, "t1", v1.PhaseRunning)
	if st.ExecutorPodName != "rt-t1-abcde" {
		t.Fatalf("executor pod not recorded: %+v", st)
	}

	// Job 完成 -> Success
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if _, err := e.kube.BatchV1().Jobs("dev").UpdateStatus(e.ctx, job, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	st = e.reconcile(t, "t1", v1.PhaseSuccess)
	if st.CompletionTime == nil {
		t.Fatal("completionTime missing")
	}

	// 终态后不再改动,也不重复建 Job
	before := len(e.kube.Actions())
	if err := e.c.Reconcile(e.ctx, "dev/t1"); err != nil {
		t.Fatal(err)
	}
	if len(e.kube.Actions()) != before {
		t.Fatal("terminal task must not trigger API calls")
	}
}

func TestReconcileInvalidSpecFails(t *testing.T) {
	bad := sampleTask()
	bad.Spec.Service.Kind = "Pod"
	e := newEnv(t, bad)

	st := e.reconcile(t, "t1", v1.PhaseFailed)
	if st.Message == "" {
		t.Fatal("failure reason missing")
	}
	if jobs, _ := e.kube.BatchV1().Jobs("dev").List(e.ctx, metav1.ListOptions{}); len(jobs.Items) != 0 {
		t.Fatal("invalid task must not create a job")
	}
}

func TestReconcileJobDeletedMidFlightFails(t *testing.T) {
	e := newEnv(t, sampleTask())
	e.reconcile(t, "t1", v1.PhasePending)
	// 等 informer 观察到 Job 之后再删,否则 fake 客户端可能丢掉还没建立 watch 时的删除事件
	for i := 0; i < 200; i++ {
		if _, err := e.c.jobs.Jobs("dev").Get("rt-t1"); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := e.kube.BatchV1().Jobs("dev").Delete(e.ctx, "rt-t1", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	st := e.reconcile(t, "t1", v1.PhaseFailed)
	if st.Message != "executor job was deleted" {
		t.Fatalf("unexpected message %q", st.Message)
	}
	if jobs, _ := e.kube.BatchV1().Jobs("dev").List(e.ctx, metav1.ListOptions{}); len(jobs.Items) != 0 {
		t.Fatal("a lost job must not be recreated (would double-release)")
	}
}
