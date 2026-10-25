package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/tools/record"

	v1 "github.com/xiaobaowan1988/okdev/internal/api/v1"
	"github.com/xiaobaowan1988/okdev/internal/deployer"
)

var now = metav1.NewTime(time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC))

// fakeDeployer 在内存里模拟 deployer。
type fakeDeployer struct {
	mu        sync.Mutex
	healthy   error
	builders  map[string]*deployer.Builder
	createErr error
	runs      map[int64]*deployer.Run
	created   []string // 每次创建运行的 "pipeline/builder/env/ref"
	nextID    int64
}

func newFakeDeployer() *fakeDeployer {
	return &fakeDeployer{
		builders: map[string]*deployer.Builder{"api-main": {Name: "api-main", Service: "api"}},
		runs:     map[int64]*deployer.Run{}, nextID: 100,
	}
}

func (f *fakeDeployer) Healthy(context.Context) error { return f.healthy }
func (f *fakeDeployer) GetBuilder(_ context.Context, name string) (*deployer.Builder, error) {
	if b, ok := f.builders[name]; ok {
		return b, nil
	}
	return nil, &deployer.APIError{Status: 404, Message: "not found"}
}
func (f *fakeDeployer) CreateRun(_ context.Context, pipeline, builder, env, ref string, _ *int, _ string) (*deployer.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.nextID++
	r := &deployer.Run{ID: f.nextID, Pipeline: pipeline, Builder: builder, Env: env, Ref: ref, Status: deployer.StatusPending}
	f.runs[r.ID] = r
	f.created = append(f.created, pipeline+"/"+builder+"/"+env+"/"+ref)
	cp := *r
	return &cp, nil
}
func (f *fakeDeployer) GetRun(_ context.Context, id int64) (*deployer.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.runs[id]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, &deployer.APIError{Status: 404, Message: "not found"}
}
func (f *fakeDeployer) setStatus(id int64, status, image, msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.runs[id]
	r.Status, r.Image, r.Message = status, image, msg
	t := now.Time
	if status != deployer.StatusPending {
		r.StartedAt = &t
	}
	if status == deployer.StatusSucceeded || status == deployer.StatusFailed {
		r.FinishedAt = &t
	}
}

func task(name string) *v1.ReleaseTask {
	return &v1.ReleaseTask{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "okdev-releases"},
		Spec:       v1.ReleaseTaskSpec{Service: "api", Builder: "api-main", Environment: "dev3", Ref: "main"},
	}
}

func object(t *v1.ReleaseTask) *unstructured.Unstructured {
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
	c   *Controller
	dyn *dynfake.FakeDynamicClient
	dep *fakeDeployer
	ctx context.Context
}

func newEnv(t *testing.T, tasks ...*v1.ReleaseTask) *env {
	t.Helper()
	var objs []runtime.Object
	for _, tk := range tasks {
		objs = append(objs, object(tk))
	}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{v1.GVR: "ReleaseTaskList"}, objs...)
	dep := newFakeDeployer()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := New(Config{DefaultPipeline: "build-and-deploy", PollInterval: time.Hour}, dyn, dep, record.NewFakeRecorder(100), "", 0)
	c.now = func() metav1.Time { return now }
	c.start(ctx.Done())
	deadline := time.After(5 * time.Second)
	for !c.synced() {
		select {
		case <-deadline:
			t.Fatal("informer did not sync")
		case <-time.After(5 * time.Millisecond):
		}
	}
	return &env{c: c, dyn: dyn, dep: dep, ctx: ctx}
}

func (e *env) status(t *testing.T, name string) v1.ReleaseTaskStatus {
	t.Helper()
	u, err := e.dyn.Resource(v1.GVR).Namespace("okdev-releases").Get(e.ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	tk, err := v1.FromUnstructured(u)
	if err != nil {
		t.Fatal(err)
	}
	return tk.Status
}

// reconcile 调谐一次;informer 缓存可能还没看到最新对象,所以等到条件满足或超时。
func (e *env) reconcileUntil(t *testing.T, name string, ok func(v1.ReleaseTaskStatus) bool) v1.ReleaseTaskStatus {
	t.Helper()
	var st v1.ReleaseTaskStatus
	for i := 0; i < 300; i++ {
		if err := e.c.Reconcile(e.ctx, "okdev-releases/"+name); err != nil {
			t.Fatal(err)
		}
		if st = e.status(t, name); ok(st) {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition never met, last status %+v", st)
	return st
}

func TestLifecycleMirrorsDeployerRun(t *testing.T) {
	e := newEnv(t, task("t1"))

	st := e.reconcileUntil(t, "t1", func(s v1.ReleaseTaskStatus) bool { return s.RunID != 0 })
	if st.Phase != v1.PhasePending || st.Stage != deployer.StatusPending {
		t.Fatalf("after submit: %+v", st)
	}
	if len(e.dep.created) != 1 || e.dep.created[0] != "build-and-deploy/api-main/dev3/main" {
		t.Fatalf("submitted %v (default pipeline must apply)", e.dep.created)
	}

	e.dep.setStatus(st.RunID, deployer.StatusBuilding, "", "")
	st = e.reconcileUntil(t, "t1", func(s v1.ReleaseTaskStatus) bool { return s.Phase == v1.PhaseRunning })
	if st.Stage != deployer.StatusBuilding || st.StartTime == nil {
		t.Fatalf("building: %+v", st)
	}
	e.dep.setStatus(st.RunID, deployer.StatusDeploying, "localhost:30500/api:r7", "")
	st = e.reconcileUntil(t, "t1", func(s v1.ReleaseTaskStatus) bool { return s.Stage == deployer.StatusDeploying })
	if st.Phase != v1.PhaseRunning || st.Image != "localhost:30500/api:r7" {
		t.Fatalf("deploying: %+v", st)
	}
	e.dep.setStatus(st.RunID, deployer.StatusSucceeded, "localhost:30500/api:r7", "")
	st = e.reconcileUntil(t, "t1", func(s v1.ReleaseTaskStatus) bool { return s.Phase == v1.PhaseSuccess })
	if st.CompletionTime == nil {
		t.Fatal("completionTime missing")
	}

	// 终态不再改动,也不会再提交
	if err := e.c.Reconcile(e.ctx, "okdev-releases/t1"); err != nil {
		t.Fatal(err)
	}
	if len(e.dep.created) != 1 {
		t.Fatalf("terminal task resubmitted: %v", e.dep.created)
	}
}

func TestDeployerFailureAndCancelBecomeFailed(t *testing.T) {
	for _, stage := range []string{deployer.StatusFailed, deployer.StatusCancelled} {
		e := newEnv(t, task("t1"))
		st := e.reconcileUntil(t, "t1", func(s v1.ReleaseTaskStatus) bool { return s.RunID != 0 })
		e.dep.setStatus(st.RunID, stage, "", "Error: reference not found")
		st = e.reconcileUntil(t, "t1", func(s v1.ReleaseTaskStatus) bool { return s.Phase.Terminal() })
		if st.Phase != v1.PhaseFailed || st.Message != "Error: reference not found" {
			t.Fatalf("%s: %+v", stage, st)
		}
	}
}

func TestInvalidSpecAndUnknownBuilderFail(t *testing.T) {
	bad := task("bad")
	bad.Spec.Builder = ""
	wrong := task("wrong")
	wrong.Spec.Service = "other" // 构建器属于 api
	missing := task("missing")
	missing.Spec.Builder = "nope"
	e := newEnv(t, bad, wrong, missing)
	for name, frag := range map[string]string{"bad": "spec.builder is required", "wrong": "belongs to service", "missing": "does not exist"} {
		st := e.reconcileUntil(t, name, func(s v1.ReleaseTaskStatus) bool { return s.Phase == v1.PhaseFailed })
		if !contains(st.Message, frag) {
			t.Fatalf("%s: message %q", name, st.Message)
		}
	}
	if len(e.dep.created) != 0 {
		t.Fatalf("nothing should be submitted: %v", e.dep.created)
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

func TestDeployerDownDoesNotMarkOrFail(t *testing.T) {
	e := newEnv(t, task("t1"))
	e.dep.healthy = errors.New("connection refused")
	if err := e.c.Reconcile(e.ctx, "okdev-releases/t1"); err == nil {
		t.Fatal("expected an error so the work queue retries with backoff")
	}
	if st := e.status(t, "t1"); st.Phase != "" || st.Stage != "" {
		t.Fatalf("task must stay untouched while deployer is down: %+v", st)
	}
	e.dep.healthy = nil
	e.reconcileUntil(t, "t1", func(s v1.ReleaseTaskStatus) bool { return s.RunID != 0 })
}

func TestRejectedAndAmbiguousSubmissionFailWithoutRetry(t *testing.T) {
	e := newEnv(t, task("rejected"))
	e.dep.createErr = &deployer.APIError{Status: 400, Message: "builder and env are required"}
	st := e.reconcileUntil(t, "rejected", func(s v1.ReleaseTaskStatus) bool { return s.Phase == v1.PhaseFailed })
	if !contains(st.Message, "deployer rejected the run") {
		t.Fatalf("%q", st.Message)
	}

	e2 := newEnv(t, task("ambiguous"))
	e2.dep.createErr = errors.New("EOF") // 请求可能已经到达 deployer
	st = e2.reconcileUntil(t, "ambiguous", func(s v1.ReleaseTaskStatus) bool { return s.Phase == v1.PhaseFailed })
	if !contains(st.Message, "unknown result") || st.RunID != 0 {
		t.Fatalf("%+v", st)
	}
	e2.dep.createErr = nil
	if err := e2.c.Reconcile(e2.ctx, "okdev-releases/ambiguous"); err != nil || len(e2.dep.created) != 0 {
		t.Fatalf("ambiguous submission must never be retried automatically (created=%v err=%v)", e2.dep.created, err)
	}
}

func TestControllerRestartWhileSubmittingDoesNotResubmit(t *testing.T) {
	tk := task("t1")
	tk.Status = v1.ReleaseTaskStatus{Phase: v1.PhasePending, Stage: v1.StageSubmitting} // 上一个进程写了标记就崩了
	e := newEnv(t, tk)
	st := e.reconcileUntil(t, "t1", func(s v1.ReleaseTaskStatus) bool { return s.Phase == v1.PhaseFailed })
	if !contains(st.Message, "submission state unknown") {
		t.Fatalf("%q", st.Message)
	}
	if len(e.dep.created) != 0 {
		t.Fatal("must not resubmit")
	}
}

func TestRunDeletedInDeployerFails(t *testing.T) {
	e := newEnv(t, task("t1"))
	st := e.reconcileUntil(t, "t1", func(s v1.ReleaseTaskStatus) bool { return s.RunID != 0 })
	delete(e.dep.runs, st.RunID)
	st = e.reconcileUntil(t, "t1", func(s v1.ReleaseTaskStatus) bool { return s.Phase == v1.PhaseFailed })
	if !contains(st.Message, "no longer exists") {
		t.Fatalf("%q", st.Message)
	}
}

func TestStatusFromRunPureFunction(t *testing.T) {
	cases := map[string]v1.Phase{
		deployer.StatusPending: v1.PhasePending, deployer.StatusBuilding: v1.PhaseRunning, deployer.StatusDeploying: v1.PhaseRunning,
		deployer.StatusSucceeded: v1.PhaseSuccess, deployer.StatusFailed: v1.PhaseFailed, deployer.StatusCancelled: v1.PhaseFailed,
		"something-new": v1.PhaseRunning,
	}
	for stage, want := range cases {
		got := StatusFromRun(v1.ReleaseTaskStatus{}, &deployer.Run{ID: 1, Status: stage}, now)
		if got.Phase != want {
			t.Errorf("%s -> %s, want %s", stage, got.Phase, want)
		}
		if want.Terminal() && got.CompletionTime == nil {
			t.Errorf("%s: terminal must carry completionTime", stage)
		}
	}
	// 终态不被改写
	done := v1.ReleaseTaskStatus{Phase: v1.PhaseFailed, Message: "x", CompletionTime: &now}
	if got := StatusFromRun(done, &deployer.Run{ID: 1, Status: deployer.StatusSucceeded}, now); got.Phase != v1.PhaseFailed {
		t.Fatal("terminal status rewritten")
	}
}
