// Package gateway 是控制器给 Headlamp 插件用的只读 HTTP 接口。
// Headlamp 经 API Server 的服务代理访问它(沿用用户自己的身份和 RBAC),它再用控制器持有的令牌去读 deployer。
// 只暴露界面需要的字段:构建器里的构建脚本和 Dockerfile 内容不会出现在这里。
package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/xiaobaowan1988/okdev/internal/deployer"
	"github.com/xiaobaowan1988/okdev/internal/gitrefs"
)

type Deployer interface {
	GetBuilder(ctx context.Context, name string) (*deployer.Builder, error)
	ListBuilders(ctx context.Context) ([]deployer.Builder, error)
	ListEnvs(ctx context.Context) ([]string, error)
	ListEnvServices(ctx context.Context, env string) ([]deployer.EnvService, error)
	ListServices(ctx context.Context) ([]deployer.Service, error)
	RunLog(ctx context.Context, id int64) (string, error)
}

type Refs interface {
	List(ctx context.Context, repo string) (gitrefs.Refs, error)
}

type Gateway struct {
	Dep  Deployer
	Refs Refs
}

// EnvServiceView 是环境页的一行:部署的期望/观测状态,加上它在集群里的位置(命名空间和工作负载名)。
type EnvServiceView struct {
	deployer.EnvService
	Namespace string `json:"namespace"`
	Workload  string `json:"workload"`
}

// RefsView 是某个构建器的仓库里可选的分支和 Tag。
type RefsView struct {
	gitrefs.Refs
	BuilderRef string `json:"builder_ref,omitempty"` // 构建器配置的默认分支
}

func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /builders", g.builders)
	mux.HandleFunc("GET /refs", g.refs)
	mux.HandleFunc("GET /envs", g.envs)
	mux.HandleFunc("GET /envs/{env}/services", g.envServices)
	mux.HandleFunc("GET /runs/{id}/log", g.runLog)
	return mux
}

func (g *Gateway) builders(w http.ResponseWriter, r *http.Request) {
	bs, err := g.Dep.ListBuilders(r.Context())
	if err != nil {
		upstream(w, err)
		return
	}
	out := make([]deployer.Builder, 0, len(bs))
	for _, b := range bs {
		out = append(out, deployer.Builder{Name: b.Name, Service: b.Service, RepoURL: b.RepoURL, Ref: b.Ref})
	}
	writeJSON(w, http.StatusOK, out)
}

func (g *Gateway) refs(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("builder")
	if name == "" {
		writeErr(w, http.StatusBadRequest, "builder is required")
		return
	}
	// 仓库地址只取自 deployer 里登记的构建器,调用方不能指定任意 URL
	b, err := g.Dep.GetBuilder(r.Context(), name)
	if err != nil {
		upstream(w, err)
		return
	}
	refs, err := g.Refs.List(r.Context(), b.RepoURL)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list refs: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, RefsView{Refs: refs, BuilderRef: b.Ref})
}

func (g *Gateway) envs(w http.ResponseWriter, r *http.Request) {
	es, err := g.Dep.ListEnvs(r.Context())
	if err != nil {
		upstream(w, err)
		return
	}
	writeJSON(w, http.StatusOK, es)
}

func (g *Gateway) envServices(w http.ResponseWriter, r *http.Request) {
	env := r.PathValue("env")
	items, err := g.Dep.ListEnvServices(r.Context(), env)
	if err != nil {
		upstream(w, err)
		return
	}
	svcs, err := g.Dep.ListServices(r.Context())
	if err != nil {
		upstream(w, err)
		return
	}
	workload := map[string]string{}
	for _, s := range svcs {
		workload[s.Name] = s.Workload
	}
	out := make([]EnvServiceView, 0, len(items))
	for _, it := range items {
		wl := workload[it.Service]
		if wl == "" {
			wl = it.Service // deployer 的默认:工作负载名等于服务名
		}
		out = append(out, EnvServiceView{EnvService: it, Namespace: deployer.Namespace(env, it.Service), Workload: wl})
	}
	writeJSON(w, http.StatusOK, out)
}

func (g *Gateway) runLog(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid run id")
		return
	}
	log, err := g.Dep.RunLog(r.Context(), id)
	if err != nil {
		upstream(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(log))
}

// upstream 把 deployer 的错误转成响应:404 原样返回,其他 4xx 当成请求错误,其余是网关错误。
func upstream(w http.ResponseWriter, err error) {
	switch {
	case deployer.IsNotFound(err):
		writeErr(w, http.StatusNotFound, "not found")
	case deployer.ClientError(err):
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		writeErr(w, http.StatusBadGateway, "deployer: "+err.Error())
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
