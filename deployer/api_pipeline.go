package main

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-sql-driver/mysql"
)

func (a *API) pipelineRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /v1/templates", a.createTemplate)
	m.HandleFunc("GET /v1/templates", a.listTemplates)
	m.HandleFunc("GET /v1/templates/{name}", a.getTemplate)
	m.HandleFunc("PUT /v1/templates/{name}", a.updateTemplate)
	m.HandleFunc("DELETE /v1/templates/{name}", a.deleteTemplate)

	m.HandleFunc("POST /v1/builders", a.createBuilder)
	m.HandleFunc("GET /v1/builders", a.listBuilders)
	m.HandleFunc("GET /v1/builders/{name}", a.getBuilder)
	m.HandleFunc("PUT /v1/builders/{name}", a.updateBuilder)
	m.HandleFunc("DELETE /v1/builders/{name}", a.deleteBuilder)

	m.HandleFunc("POST /v1/pipelines", a.createPipeline)
	m.HandleFunc("GET /v1/pipelines", a.listPipelines)
	m.HandleFunc("GET /v1/pipelines/{name}", a.getPipeline)
	m.HandleFunc("PUT /v1/pipelines/{name}", a.updatePipeline)
	m.HandleFunc("DELETE /v1/pipelines/{name}", a.deletePipeline)
	m.HandleFunc("POST /v1/pipelines/{name}/runs", a.createRun)

	m.HandleFunc("GET /v1/runs", a.listRuns)
	m.HandleFunc("GET /v1/runs/{id}", a.getRun)
	m.HandleFunc("GET /v1/runs/{id}/log", a.runLog)
	m.HandleFunc("POST /v1/runs/{id}/cancel", a.cancelRun)
}

// 外键拒绝删除(1451)统一转成 409。
func conflictOnFK(w http.ResponseWriter, err error, msg string) bool {
	var me *mysql.MySQLError
	if errors.As(err, &me) && me.Number == 1451 {
		writeErr(w, http.StatusConflict, msg)
		return true
	}
	return false
}

var imageRefRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9._/-]*)(:[A-Za-z0-9._-]+)?(@sha256:[a-f0-9]{64})?$`)

var profileRe = regexp.MustCompile(`^[A-Za-z0-9._-]{0,63}$`)

// validRelPath 校验"相对构建目录的路径":不能为绝对路径、不能含 ..、不能含空白和控制字符。
func validRelPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "..") {
		return false
	}
	for _, r := range p {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

const maxScriptBytes = 64 << 10

// validateRecipe 校验一套构建配方(构建模板,或没引用模板的 builder 自带的内容)。
// Dockerfile 的内容必填;构建脚本可选(不填就不跑脚本);有脚本就要有构建环境镜像,没脚本就不能填镜像。
func validateRecipe(dockerfile, script, image, profile string) string {
	if strings.TrimSpace(dockerfile) == "" {
		return "dockerfile (the content of a single-stage Dockerfile) is required"
	}
	if len(dockerfile) > maxScriptBytes {
		return "dockerfile is too large (max 64KB)"
	}
	if len(script) > maxScriptBytes {
		return "script is too large (max 64KB)"
	}
	if script != "" {
		if image == "" {
			return "image (the build environment the script runs in, e.g. a maven or golang image) is required when a build script is set"
		}
	} else if image != "" {
		return "image is only used together with a build script (it is the environment the script runs in)"
	}
	if image != "" && !imageRefRe.MatchString(image) {
		return "invalid image reference: " + image
	}
	if !profileRe.MatchString(profile) {
		return "profile may only contain [A-Za-z0-9._-], max 63 characters"
	}
	return ""
}

func normalizeBuilder(b *Builder) string {
	if b.Ref == "" {
		b.Ref = "master"
	}
	if b.ContextPath == "" {
		b.ContextPath = "."
	}
	if b.Template != "" {
		// 引用模板:脚本、Dockerfile、构建环境镜像都来自模板,builder 自己不能再写(避免"到底用哪个"的歧义);profile 可以覆盖模板的默认值
		if b.Dockerfile != "" || b.Script != "" || b.Image != "" {
			return "dockerfile, script and image come from the template: leave them empty when template is set (profile may still override)"
		}
		if !profileRe.MatchString(b.Profile) {
			return "profile may only contain [A-Za-z0-9._-], max 63 characters"
		}
	} else if msg := validateRecipe(b.Dockerfile, b.Script, b.Image, b.Profile); msg != "" {
		return msg
	}
	if !validRelPath(strings.TrimPrefix(b.ContextPath, "./")) && b.ContextPath != "." {
		return "context_path must be a relative path inside the repo (no .., no spaces)"
	}
	if b.RepoURL == "" {
		return "repo_url is required"
	}
	if err := validateRepoRef(b.RepoURL, b.Ref); err != nil { // 同时校验 repo_url 和 ref
		return err.Error()
	}
	for k := range b.BuildArgs {
		if !argNameRe.MatchString(k) {
			return "invalid build arg name: " + k
		}
	}
	return ""
}

func (a *API) createBuilder(w http.ResponseWriter, r *http.Request) {
	var in Builder
	if !decode(w, r, &in) || !validName(w, "builder", in.Name, 63) {
		return
	}
	if in.Service == "" {
		writeErr(w, http.StatusBadRequest, "service is required: a builder must be linked to a service")
		return
	}
	if msg := normalizeBuilder(&in); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	if err := a.store.CreateBuilder(r.Context(), in); err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, in)
}

func (a *API) listBuilders(w http.ResponseWriter, r *http.Request) {
	v, err := a.store.ListBuilders(r.Context(), r.URL.Query().Get("service"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) getBuilder(w http.ResponseWriter, r *http.Request) {
	v, err := a.store.GetBuilder(r.Context(), r.PathValue("name"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) updateBuilder(w http.ResponseWriter, r *http.Request) {
	var in Builder
	if !decode(w, r, &in) {
		return
	}
	name := r.PathValue("name")
	if in.Name != "" && in.Name != name {
		writeErr(w, http.StatusBadRequest, "builder name cannot be changed")
		return
	}
	cur, err := a.store.GetBuilder(r.Context(), name)
	if err != nil {
		a.fail(w, err)
		return
	}
	if in.Service != "" && in.Service != cur.Service {
		writeErr(w, http.StatusBadRequest, "the linked service cannot be changed (past runs refer to it); create a new builder instead")
		return
	}
	in.Name, in.Service = name, cur.Service
	if msg := normalizeBuilder(&in); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	if err := a.store.UpdateBuilder(r.Context(), in); err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, in)
}

func (a *API) deleteBuilder(w http.ResponseWriter, r *http.Request) {
	err := a.store.DeleteBuilder(r.Context(), r.PathValue("name"))
	if err != nil && conflictOnFK(w, err, "builder is referenced by pipeline runs; it cannot be deleted") {
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func normalizePipeline(p *Pipeline) string {
	if len(p.Steps) == 0 {
		p.Steps = []string{"build", "deploy"}
	}
	if err := validateSteps(p.Steps); err != nil {
		return err.Error()
	}
	return ""
}

func (a *API) createPipeline(w http.ResponseWriter, r *http.Request) {
	var in Pipeline
	if !decode(w, r, &in) || !validName(w, "pipeline", in.Name, 63) {
		return
	}
	if msg := normalizePipeline(&in); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	if err := a.store.CreatePipeline(r.Context(), in); err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, in)
}

func (a *API) listPipelines(w http.ResponseWriter, r *http.Request) {
	v, err := a.store.ListPipelines(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) getPipeline(w http.ResponseWriter, r *http.Request) {
	v, err := a.store.GetPipeline(r.Context(), r.PathValue("name"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) updatePipeline(w http.ResponseWriter, r *http.Request) {
	var in Pipeline
	if !decode(w, r, &in) {
		return
	}
	name := r.PathValue("name")
	if in.Name != "" && in.Name != name {
		writeErr(w, http.StatusBadRequest, "pipeline name cannot be changed")
		return
	}
	in.Name = name
	if msg := normalizePipeline(&in); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	if err := a.store.UpdatePipeline(r.Context(), in); err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, in)
}

func (a *API) deletePipeline(w http.ResponseWriter, r *http.Request) {
	err := a.store.DeletePipeline(r.Context(), r.PathValue("name"))
	if err != nil && conflictOnFK(w, err, "pipeline has runs; it cannot be deleted") {
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// createRun:运行一条流水线,选 builder 和 env。流水线跑完(build 成功)会自动部署到所选环境。
func (a *API) createRun(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Builder  string `json:"builder"`
		Env      string `json:"env"`
		Ref      string `json:"ref"`
		Profile  string `json:"profile"` // 不填则用 builder 的默认 profile
		Replicas *int   `json:"replicas"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Builder == "" || in.Env == "" {
		writeErr(w, http.StatusBadRequest, "builder and env are required")
		return
	}
	if in.Replicas != nil && (*in.Replicas < 0 || *in.Replicas > 50) {
		writeErr(w, http.StatusBadRequest, "replicas must be 0..50")
		return
	}
	if !profileRe.MatchString(in.Profile) {
		writeErr(w, http.StatusBadRequest, "profile may only contain [A-Za-z0-9._-], max 63 characters")
		return
	}
	if in.Ref != "" {
		if err := validateRepoRef("https://example.invalid/x.git", in.Ref); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	run, err := a.store.CreateRun(r.Context(), actor(r), r.PathValue("name"), in.Builder, in.Env, in.Ref, in.Profile, in.Replicas)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.run.Kick()
	writeJSON(w, http.StatusAccepted, run)
}

func (a *API) listRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	v, err := a.store.ListRuns(r.Context(), RunFilter{Pipeline: q.Get("pipeline"), Builder: q.Get("builder"), Env: q.Get("env"), Status: q.Get("status"), Limit: limit})
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func runID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid run id")
		return 0, false
	}
	return id, true
}

func (a *API) getRun(w http.ResponseWriter, r *http.Request) {
	id, ok := runID(w, r)
	if !ok {
		return
	}
	v, err := a.store.GetRun(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// runLog 返回构建日志(纯文本):构建中取 Pod 的实时日志尾部,结束后用保存下来的。
func (a *API) runLog(w http.ResponseWriter, r *http.Request) {
	id, ok := runID(w, r)
	if !ok {
		return
	}
	run, err := a.store.GetRun(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	log, err := a.store.RunLog(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	if log == "" && run.Status == RunBuilding {
		log = a.run.LiveLog(r.Context(), id)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(strings.TrimRight(log, "\n") + "\n"))
}

func (a *API) cancelRun(w http.ResponseWriter, r *http.Request) {
	id, ok := runID(w, r)
	if !ok {
		return
	}
	run, err := a.store.RequestCancel(r.Context(), id)
	if errors.Is(err, ErrConflict) {
		writeErr(w, http.StatusConflict, "run already finished ("+run.Status+")")
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	a.run.Kick()
	writeJSON(w, http.StatusAccepted, run)
}

// ---- 构建模板 ----

func (a *API) createTemplate(w http.ResponseWriter, r *http.Request) {
	var in Template
	if !decode(w, r, &in) || !validName(w, "template", in.Name, 63) {
		return
	}
	if msg := validateRecipe(in.Dockerfile, in.Script, in.Image, in.Profile); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	if err := a.store.CreateTemplate(r.Context(), in); err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, in)
}

func (a *API) listTemplates(w http.ResponseWriter, r *http.Request) {
	v, err := a.store.ListTemplates(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) getTemplate(w http.ResponseWriter, r *http.Request) {
	v, err := a.store.GetTemplate(r.Context(), r.PathValue("name"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) updateTemplate(w http.ResponseWriter, r *http.Request) {
	var in Template
	if !decode(w, r, &in) {
		return
	}
	name := r.PathValue("name")
	if in.Name != "" && in.Name != name {
		writeErr(w, http.StatusBadRequest, "template name cannot be changed")
		return
	}
	in.Name = name
	if msg := validateRecipe(in.Dockerfile, in.Script, in.Image, in.Profile); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	if err := a.store.UpdateTemplate(r.Context(), in); err != nil {
		a.fail(w, err)
		return
	}
	cur, err := a.store.GetTemplate(r.Context(), name)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cur)
}

func (a *API) deleteTemplate(w http.ResponseWriter, r *http.Request) {
	err := a.store.DeleteTemplate(r.Context(), r.PathValue("name"))
	if err != nil && conflictOnFK(w, err, "template is referenced by builders; it cannot be deleted") {
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
