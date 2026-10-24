package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/go-sql-driver/mysql"
)

var nameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

type API struct {
	store *Store
	rec   *Reconciler
	cfg   config
	log   *slog.Logger
}

func (a *API) routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	m.HandleFunc("POST /v1/envs", a.createEnv)
	m.HandleFunc("GET /v1/envs", a.listEnvs)
	m.HandleFunc("POST /v1/services", a.createService)
	m.HandleFunc("GET /v1/services", a.listServices)
	m.HandleFunc("GET /v1/envs/{env}/services", a.listEnvServices)
	m.HandleFunc("PUT /v1/envs/{env}/services/{svc}", a.putEnvService)
	m.HandleFunc("POST /v1/envs/{env}/services/{svc}/scale", a.scale)
	m.HandleFunc("DELETE /v1/envs/{env}/services/{svc}", a.deleteEnvService)
	m.HandleFunc("GET /v1/envs/{env}/services/{svc}/history", a.history)
	return a.auth(m)
}

// 原型级鉴权:配置了 API_TOKEN 就要求 Bearer token(没配则放行,仅限本地试验)。
func (a *API) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.cfg.apiToken != "" && r.URL.Path != "/healthz" {
			want := "Bearer " + a.cfg.apiToken
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(want)) != 1 {
				writeErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func actor(r *http.Request) string {
	if v := r.Header.Get("X-Actor"); v != "" && len(v) <= 64 {
		return v
	}
	return "anonymous"
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (a *API) fail(w http.ResponseWriter, err error) {
	var me *mysql.MySQLError
	switch {
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, "not found")
	case errors.Is(err, ErrConflict):
		writeErr(w, http.StatusConflict, "version conflict: the deployment was modified, re-read and retry")
	case errors.As(err, &me) && me.Number == 1062:
		writeErr(w, http.StatusConflict, "already exists")
	default:
		a.log.Error("request failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return false
	}
	return true
}

func validName(w http.ResponseWriter, kind, s string, max int) bool {
	if len(s) == 0 || len(s) > max || !nameRe.MatchString(s) {
		writeErr(w, http.StatusBadRequest, kind+" name must match [a-z0-9-], start/end alphanumeric, max "+itoa(max))
		return false
	}
	return true
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func (a *API) createEnv(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &in) || !validName(w, "env", in.Name, 20) {
		return
	}
	if err := a.store.CreateEnv(r.Context(), in.Name); err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"name": in.Name})
}

func (a *API) listEnvs(w http.ResponseWriter, r *http.Request) {
	v, err := a.store.ListEnvs(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) createService(w http.ResponseWriter, r *http.Request) {
	var in Service
	if !decode(w, r, &in) || !validName(w, "service", in.Name, 30) {
		return
	}
	if in.RepoURL == "" || in.Path == "" {
		writeErr(w, http.StatusBadRequest, "repo_url and path are required")
		return
	}
	if in.Revision == "" {
		in.Revision = "HEAD"
	}
	if in.Workload == "" {
		in.Workload = in.Name
	}
	if err := a.store.CreateService(r.Context(), in); err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, in)
}

func (a *API) listServices(w http.ResponseWriter, r *http.Request) {
	v, err := a.store.ListServices(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) listEnvServices(w http.ResponseWriter, r *http.Request) {
	v, err := a.store.ListEnvServices(r.Context(), r.PathValue("env"))
	if err != nil {
		a.fail(w, err)
		return
	}
	if v == nil {
		v = []EnvService{}
	}
	writeJSON(w, http.StatusOK, v)
}

type changeIn struct {
	Replicas        *int    `json:"replicas"`
	Image           *string `json:"image"`
	ExpectedVersion *int64  `json:"expected_version"`
}

func (a *API) apply(w http.ResponseWriter, r *http.Request, c Change) {
	if c.Replicas != nil && (*c.Replicas < 0 || *c.Replicas > 50) {
		writeErr(w, http.StatusBadRequest, "replicas must be 0..50")
		return
	}
	out, err := a.store.Apply(r.Context(), actor(r), r.PathValue("env"), r.PathValue("svc"), c)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.rec.Kick()
	writeJSON(w, http.StatusOK, out)
}

func (a *API) putEnvService(w http.ResponseWriter, r *http.Request) {
	var in changeIn
	if !decode(w, r, &in) {
		return
	}
	a.apply(w, r, Change{Replicas: in.Replicas, Image: in.Image, ExpectedVersion: in.ExpectedVersion})
}

func (a *API) scale(w http.ResponseWriter, r *http.Request) {
	var in changeIn
	if !decode(w, r, &in) {
		return
	}
	if in.Replicas == nil {
		writeErr(w, http.StatusBadRequest, "replicas is required")
		return
	}
	a.apply(w, r, Change{Replicas: in.Replicas, MustExist: true, ExpectedVersion: in.ExpectedVersion})
}

func (a *API) deleteEnvService(w http.ResponseWriter, r *http.Request) {
	a.apply(w, r, Change{Delete: true})
}

func (a *API) history(w http.ResponseWriter, r *http.Request) {
	v, err := a.store.History(r.Context(), r.PathValue("env"), r.PathValue("svc"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}
