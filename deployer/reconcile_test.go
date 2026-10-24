package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestSubsetEqual(t *testing.T) {
	want := map[string]any{"a": int64(0), "b": []any{map[string]any{"n": "x", "c": int64(3)}}}
	cases := []struct {
		name string
		have any
		ok   bool
	}{
		{"equal with extra fields", map[string]any{"a": int64(0), "z": "extra", "b": []any{map[string]any{"n": "x", "c": int64(3), "d": 1}}}, true},
		{"zero value drift is detected", map[string]any{"a": int64(1), "b": []any{map[string]any{"n": "x", "c": int64(3)}}}, false},
		{"missing key", map[string]any{"b": []any{map[string]any{"n": "x", "c": int64(3)}}}, false},
		{"slice length differs", map[string]any{"a": int64(0), "b": []any{}}, false},
	}
	for _, c := range cases {
		if got := subsetEqual(want, c.have); got != c.ok {
			t.Errorf("%s: got %v want %v", c.name, got, c.ok)
		}
	}
}

func TestDesiredApp(t *testing.T) {
	r := &Reconciler{cfg: config{appNamespace: "argocd", project: "apps", destServer: "https://kubernetes.default.svc"}}
	x := EnvService{Env: "dev1", Service: "web", Replicas: 0, Image: "r/nginx:1", Repo: "https://x/y.git", Path: "demo/web", Revision: "HEAD", Workload: "web", ImageName: "r/nginx"}
	app := r.desiredApp(x)
	if app.GetName() != "web-dev1" || app.GetNamespace() != "argocd" {
		t.Fatalf("bad identity %s/%s", app.GetNamespace(), app.GetName())
	}
	spec := app.Object["spec"].(map[string]any)
	if spec["project"] != "apps" {
		t.Fatalf("bad project %v", spec["project"])
	}
	if spec["destination"].(map[string]any)["namespace"] != "dev1-env-web" {
		t.Fatalf("bad dest %v", spec["destination"])
	}
	k := spec["source"].(map[string]any)["kustomize"].(map[string]any)
	if k["replicas"].([]any)[0].(map[string]any)["count"] != int64(0) || k["images"].([]any)[0] != "r/nginx=r/nginx:1" {
		t.Fatalf("bad kustomize %v", k)
	}
}

func TestReplicaVerdict(t *testing.T) {
	if v := replicaVerdict(1, 1, true, "web", "dev1-env-web"); v != "" {
		t.Errorf("equal should be ok, got %q", v)
	}
	if v := replicaVerdict(0, 0, true, "web", "ns"); v != "" {
		t.Errorf("0==0 should be ok (zero must not be treated as unset), got %q", v)
	}
	if v := replicaVerdict(1, 0, true, "web", "ns"); v != "replica mismatch: desired 1, actual 0" {
		t.Errorf("got %q", v)
	}
	if v := replicaVerdict(2, 0, false, "web", "ns"); v == "" {
		t.Error("missing workload must be reported")
	}
}

func newVerifyFixture(t *testing.T, actualReplicas int64) (*Reconciler, EnvService, dynamic.Interface) {
	t.Helper()
	app := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"name": "web-dev1", "namespace": "argocd"},
	}}
	dep := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "web", "namespace": "dev1-env-web"},
		"spec":     map[string]any{"replicas": actualReplicas},
	}}
	kube := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{appGVR: "ApplicationList", deployGVR: "DeploymentList"}, app, dep)
	r := NewReconciler(nil, nil, kube, config{appNamespace: "argocd"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	x := EnvService{Env: "dev1", Service: "web", Workload: "web", Replicas: 1, Version: 3, AppliedVersion: 3}
	return r, x, kube
}

func refreshAnnotation(t *testing.T, kube dynamic.Interface) string {
	t.Helper()
	a, err := kube.Resource(appGVR).Namespace("argocd").Get(context.Background(), "web-dev1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return a.GetAnnotations()["argocd.argoproj.io/refresh"]
}

func TestVerifyMismatchTriggersRefreshWithBackoff(t *testing.T) {
	r, x, kube := newVerifyFixture(t, 3)
	health, msg := r.verify(context.Background(), x, "Synced", "Healthy", "")
	if health != "ReplicaMismatch" || msg != "replica mismatch: desired 1, actual 3" {
		t.Fatalf("got %q / %q", health, msg)
	}
	if got := refreshAnnotation(t, kube); got != "hard" {
		t.Fatalf("expected hard refresh annotation, got %q", got)
	}
	// 退避窗口内不会重复刷新:模拟 Argo CD 已消费并移除了注解,再核对一次
	a, _ := kube.Resource(appGVR).Namespace("argocd").Get(context.Background(), "web-dev1", metav1.GetOptions{})
	a.SetAnnotations(nil)
	kube.Resource(appGVR).Namespace("argocd").Update(context.Background(), a, metav1.UpdateOptions{})
	r.verify(context.Background(), x, "Synced", "Healthy", "")
	if got := refreshAnnotation(t, kube); got != "" {
		t.Fatalf("refresh must be rate limited, got %q", got)
	}
	if r.mismatchRuns["web-dev1"] != 2 {
		t.Fatalf("mismatch runs = %d, want 2", r.mismatchRuns["web-dev1"])
	}
}

func TestVerifyOKAndSkips(t *testing.T) {
	r, x, kube := newVerifyFixture(t, 1)
	if h, m := r.verify(context.Background(), x, "Synced", "Healthy", "m"); h != "Healthy" || m != "m" {
		t.Fatalf("consistent state must pass through, got %q / %q", h, m)
	}
	// 刚改完期望(applied != version)时不核对,避免误报
	r2, x2, _ := newVerifyFixture(t, 3)
	x2.AppliedVersion = 2
	if h, _ := r2.verify(context.Background(), x2, "Synced", "Healthy", ""); h != "Healthy" {
		t.Fatalf("unstable version must be skipped, got %q", h)
	}
	// 不是 Synced(如 OutOfSync,Argo CD 正在处理)也不核对
	if h, _ := r2.verify(context.Background(), x, "OutOfSync", "Healthy", ""); h != "Healthy" {
		t.Fatalf("OutOfSync must be skipped, got %q", h)
	}
	if got := refreshAnnotation(t, kube); got != "" {
		t.Fatalf("no refresh expected, got %q", got)
	}
}

func TestVerifyStuckUnknownTriggersRefresh(t *testing.T) {
	r, x, kube := newVerifyFixture(t, 1)
	r.verify(context.Background(), x, "Unknown", "Healthy", "boom") // 第一次只记录开始时间
	if got := refreshAnnotation(t, kube); got != "" {
		t.Fatalf("must wait for grace period, got %q", got)
	}
	r.unknownSince["web-dev1"] = time.Now().Add(-2 * unknownGrace)
	r.verify(context.Background(), x, "Unknown", "Healthy", "boom")
	if got := refreshAnnotation(t, kube); got != "hard" {
		t.Fatalf("expected refresh after grace, got %q", got)
	}
}
