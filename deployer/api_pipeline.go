package main

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-sql-driver/mysql"
)

func (a *API) pipelineRoutes(m *http.ServeMux) {
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

func normalizeBuilder(b *Builder) string {
	if b.Ref == "" {
		b.Ref = "master"
	}
	if b.ContextPath == "" {
		b.ContextPath = "."
	}
	if b.Dockerfile == "" {
		b.Dockerfile = "Dockerfile"
	}
	if b.RepoURL == "" {
		return "repo_url is required"
	}
	if _, err := gitContext(b.RepoURL, b.Ref); err != nil { // 同时校验 repo_url 和 ref
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
	if in.Ref != "" {
		if _, err := gitContext("https://example.invalid/x.git", in.Ref); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	run, err := a.store.CreateRun(r.Context(), actor(r), r.PathValue("name"), in.Builder, in.Env, in.Ref, in.Replicas)
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
