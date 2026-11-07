package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/xiaobaowan1988/okdev/internal/deployer"
	"github.com/xiaobaowan1988/okdev/internal/gitrefs"
)

//go:embed openapi.yaml
var openapiYAML []byte

// Builders 查构建器(取仓库地址列分支用)。
type Builders interface {
	GetBuilder(ctx context.Context, name string) (*deployer.Builder, error)
}

// Refs 列仓库的分支和 Tag。
type Refs interface {
	List(ctx context.Context, repo string) (gitrefs.Refs, error)
}

type Server struct {
	Verifier      Verifier
	Policy        Policy
	Deployer      *url.URL
	DeployerToken string // 网关持有,调用方拿不到
	Builders      Builders
	Refs          Refs
	Log           *slog.Logger
}

const maxBody = 1 << 20

func (s *Server) Handler() http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(s.Deployer)
			id := identityOf(pr.In.Context())
			// 调用方的令牌不转给 deployer;X-Actor 一律以令牌里的用户为准,调用方自己带的会被覆盖
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("X-Actor")
			if s.DeployerToken != "" {
				pr.Out.Header.Set("Authorization", "Bearer "+s.DeployerToken)
			}
			pr.Out.Header.Set("X-Actor", id.Username)
		},
		ModifyResponse: func(resp *http.Response) error {
			if isAdmin(resp.Request.Context()) || resp.StatusCode/100 != 2 || !strings.Contains(resp.Header.Get("Content-Type"), "json") {
				return nil
			}
			b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
			if err != nil {
				return err
			}
			resp.Body.Close()
			b = stripSensitive(b)
			resp.Body = io.NopCloser(strings.NewReader(string(b)))
			resp.ContentLength = int64(len(b))
			resp.Header.Set("Content-Length", strconv.Itoa(len(b)))
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			writeErr(w, http.StatusBadGateway, "deployer unreachable: "+err.Error())
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		user := "-"
		defer func() {
			if r.URL.Path != "/healthz" {
				s.Log.Info("api", "user", user, "method", r.Method, "path", r.URL.Path, "status", sw.status, "ms", time.Since(start).Milliseconds())
			}
		}()

		switch r.URL.Path {
		case "/healthz":
			_, _ = sw.Write([]byte("ok"))
			return
		case "/openapi.yaml":
			sw.Header().Set("Content-Type", "application/yaml")
			_, _ = sw.Write(openapiYAML)
			return
		}

		raw, ok := bearer(r)
		if !ok {
			sw.Header().Set("WWW-Authenticate", `Bearer realm="okdev"`)
			writeErr(sw, http.StatusUnauthorized, "missing bearer token; log in with `okdev login`")
			return
		}
		id, err := s.Verifier.Verify(r.Context(), raw)
		switch {
		case errors.Is(err, ErrUnauthorized):
			sw.Header().Set("WWW-Authenticate", `Bearer realm="okdev", error="invalid_token"`)
			writeErr(sw, http.StatusUnauthorized, "invalid token: "+strings.TrimPrefix(err.Error(), ErrUnauthorized.Error()+": "))
			return
		case err != nil:
			writeErr(sw, http.StatusServiceUnavailable, "identity provider unavailable: "+err.Error())
			return
		}
		user = id.Username
		admin := s.Policy.IsAdmin(id.Username)
		if !s.Policy.Allow(admin, r.Method, r.URL.Path) {
			writeErr(sw, http.StatusForbidden, "this operation requires an administrator")
			return
		}

		r.Body = http.MaxBytesReader(sw, r.Body, maxBody)
		r = r.WithContext(withIdentity(r.Context(), id, admin))
		if r.URL.Path == "/refs" {
			s.refs(sw, r)
			return
		}
		proxy.ServeHTTP(sw, r)
	})
}

// refs 列出某个构建器的仓库里可选的分支和 Tag。仓库地址只取自 deployer 里登记的构建器,调用方不能指定任意 URL。
func (s *Server) refs(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("builder")
	if name == "" {
		writeErr(w, http.StatusBadRequest, "builder is required")
		return
	}
	b, err := s.Builders.GetBuilder(r.Context(), name)
	switch {
	case deployer.IsNotFound(err):
		writeErr(w, http.StatusNotFound, "builder not found")
		return
	case err != nil:
		writeErr(w, http.StatusBadGateway, "deployer: "+err.Error())
		return
	}
	refs, err := s.Refs.List(r.Context(), b.RepoURL)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list refs: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, struct {
		gitrefs.Refs
		BuilderRef string `json:"builder_ref,omitempty"`
	}{refs, b.Ref})
}

type ctxKey int

const (
	keyIdentity ctxKey = iota
	keyAdmin
)

func withIdentity(ctx context.Context, id Identity, admin bool) context.Context {
	return context.WithValue(context.WithValue(ctx, keyIdentity, id), keyAdmin, admin)
}
func identityOf(ctx context.Context) Identity { id, _ := ctx.Value(keyIdentity).(Identity); return id }
func isAdmin(ctx context.Context) bool        { a, _ := ctx.Value(keyAdmin).(bool); return a }

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) { w.status = code; w.ResponseWriter.WriteHeader(code) }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
