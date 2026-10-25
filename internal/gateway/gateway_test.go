package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xiaobaowan1988/okdev/internal/deployer"
	"github.com/xiaobaowan1988/okdev/internal/gitrefs"
)

type fakeDep struct{ err error }

func (f fakeDep) GetBuilder(_ context.Context, name string) (*deployer.Builder, error) {
	if f.err != nil {
		return nil, f.err
	}
	if name != "api-main" {
		return nil, &deployer.APIError{Status: 404}
	}
	return &deployer.Builder{Name: name, Service: "api", RepoURL: "https://example.com/api.git", Ref: "master"}, nil
}
func (f fakeDep) ListBuilders(context.Context) ([]deployer.Builder, error) {
	return []deployer.Builder{{Name: "api-main", Service: "api", RepoURL: "https://example.com/api.git", Ref: "master"}}, f.err
}
func (f fakeDep) ListEnvs(context.Context) ([]string, error) { return []string{"dev3", "dev5"}, f.err }
func (f fakeDep) ListEnvServices(_ context.Context, env string) ([]deployer.EnvService, error) {
	return []deployer.EnvService{{Env: env, Service: "api", Replicas: 2}, {Env: env, Service: "web", Replicas: 1}}, f.err
}
func (f fakeDep) ListServices(context.Context) ([]deployer.Service, error) {
	return []deployer.Service{{Name: "api", Workload: "api-deploy"}, {Name: "web"}}, f.err
}
func (f fakeDep) RunLog(_ context.Context, id int64) (string, error) { return "log of run", f.err }

type fakeRefs struct{ repo *string }

func (f fakeRefs) List(_ context.Context, repo string) (gitrefs.Refs, error) {
	*f.repo = repo
	return gitrefs.Refs{Branches: []string{"main"}, Tags: []string{}, Default: "main"}, nil
}

func get(g *Gateway, path string) (int, string) {
	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

func TestRefsRepoComesFromBuilderOnly(t *testing.T) {
	var repo string
	g := &Gateway{Dep: fakeDep{}, Refs: fakeRefs{&repo}}
	code, body := get(g, "/refs?builder=api-main")
	if code != 200 || repo != "https://example.com/api.git" {
		t.Fatalf("%d %s repo=%q", code, body, repo)
	}
	var v RefsView
	json.Unmarshal([]byte(body), &v)
	if v.BuilderRef != "master" || v.Default != "main" {
		t.Fatalf("%+v", v)
	}
	// 调用方不能指定仓库地址:repo 参数被忽略,未登记的构建器是 404
	repo = ""
	if code, _ := get(g, "/refs?repo=http://169.254.169.254/"); code != 400 || repo != "" {
		t.Fatalf("arbitrary repo must not be reachable: code=%d repo=%q", code, repo)
	}
	if code, _ := get(g, "/refs?builder=nope"); code != 404 || repo != "" {
		t.Fatalf("unknown builder: %d", code)
	}
}

func TestBuildersExposeOnlySafeFields(t *testing.T) {
	_, body := get(&Gateway{Dep: fakeDep{}}, "/builders")
	for _, secret := range []string{"dockerfile", "script", "build_args", "template"} {
		if strings.Contains(body, secret) {
			t.Fatalf("builder listing leaks %q: %s", secret, body)
		}
	}
	if !strings.Contains(body, "api-main") || !strings.Contains(body, "repo_url") {
		t.Fatal(body)
	}
}

func TestEnvServicesAddNamespaceAndWorkload(t *testing.T) {
	code, body := get(&Gateway{Dep: fakeDep{}}, "/envs/dev3/services")
	var out []EnvServiceView
	json.Unmarshal([]byte(body), &out)
	if code != 200 || len(out) != 2 {
		t.Fatalf("%d %s", code, body)
	}
	if out[0].Namespace != "dev3-env-api" || out[0].Workload != "api-deploy" {
		t.Fatalf("custom workload: %+v", out[0])
	}
	if out[1].Namespace != "dev3-env-web" || out[1].Workload != "web" {
		t.Fatalf("default workload = service name: %+v", out[1])
	}
}

func TestErrorsAndLog(t *testing.T) {
	if code, _ := get(&Gateway{Dep: fakeDep{err: errors.New("connection refused")}}, "/envs"); code != 502 {
		t.Fatalf("upstream failure should be 502, got %d", code)
	}
	if code, _ := get(&Gateway{Dep: fakeDep{err: &deployer.APIError{Status: 404}}}, "/envs/x/services"); code != 404 {
		t.Fatalf("404 passthrough, got %d", code)
	}
	g := &Gateway{Dep: fakeDep{}}
	if code, body := get(g, "/runs/7/log"); code != 200 || body != "log of run" {
		t.Fatalf("%d %q", code, body)
	}
	if code, _ := get(g, "/runs/abc/log"); code != 400 {
		t.Fatalf("bad id: %d", code)
	}
	if code, _ := get(g, "/healthz"); code != 200 {
		t.Fatal("healthz")
	}
}
