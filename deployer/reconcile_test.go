package main

import "testing"

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
