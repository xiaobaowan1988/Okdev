package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ---- 构建器 ----

type Builder struct {
	Name        string            `json:"name"`
	Service     string            `json:"service"` // 关联的服务(必填)
	RepoURL     string            `json:"repo_url"`
	Ref         string            `json:"ref"`          // 分支或完整 ref(refs/...),运行时可覆盖
	ContextPath string            `json:"context_path"` // 仓库里的构建目录
	Dockerfile  string            `json:"dockerfile"`   // 相对构建目录
	BuildArgs   map[string]string `json:"build_args,omitempty"`
}

const builderSelect = `SELECT b.id, b.name, v.name, b.repo_url, b.ref, b.context_path, b.dockerfile, b.build_args
  FROM builder b JOIN service v ON v.id = b.service_id `

func scanBuilder(r scanner) (int64, Builder, error) {
	var id int64
	var b Builder
	var args []byte
	if err := r.Scan(&id, &b.Name, &b.Service, &b.RepoURL, &b.Ref, &b.ContextPath, &b.Dockerfile, &args); err != nil {
		return 0, b, err
	}
	if len(args) > 0 {
		_ = json.Unmarshal(args, &b.BuildArgs)
	}
	return id, b, nil
}

func argsJSON(m map[string]string) any {
	if len(m) == 0 {
		return nil
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func (s *Store) CreateBuilder(ctx context.Context, b Builder) error {
	var svcID int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM service WHERE name=?`, b.Service).Scan(&svcID); err != nil {
		return notFound(err)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO builder(name, service_id, repo_url, ref, context_path, dockerfile, build_args) VALUES (?,?,?,?,?,?,?)`,
		b.Name, svcID, b.RepoURL, b.Ref, b.ContextPath, b.Dockerfile, argsJSON(b.BuildArgs))
	return err
}

func (s *Store) GetBuilder(ctx context.Context, name string) (Builder, error) {
	_, b, err := scanBuilder(s.db.QueryRowContext(ctx, builderSelect+`WHERE b.name=?`, name))
	return b, notFound(err)
}

func (s *Store) ListBuilders(ctx context.Context, service string) ([]Builder, error) {
	q, args := builderSelect+`ORDER BY b.id`, []any{}
	if service != "" {
		q, args = builderSelect+`WHERE v.name=? ORDER BY b.id`, []any{service}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Builder{}
	for rows.Next() {
		_, b, err := scanBuilder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// UpdateBuilder 修改构建器(名字和关联的服务不可改:历史运行引用着它)。
func (s *Store) UpdateBuilder(ctx context.Context, b Builder) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE builder SET repo_url=?, ref=?, context_path=?, dockerfile=?, build_args=? WHERE name=?`,
		b.RepoURL, b.Ref, b.ContextPath, b.Dockerfile, argsJSON(b.BuildArgs), b.Name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// MySQL 在"值没变"时 RowsAffected 也是 0,所以要再确认是否存在
		var x int64
		if err := s.db.QueryRowContext(ctx, `SELECT id FROM builder WHERE name=?`, b.Name).Scan(&x); err != nil {
			return notFound(err)
		}
	}
	return nil
}

func (s *Store) DeleteBuilder(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM builder WHERE name=?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- 流水线 ----

type Pipeline struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Steps       []string `json:"steps"` // build 必须在最前;deploy 可选,在 build 之后
}

func scanPipeline(r scanner) (Pipeline, error) {
	var p Pipeline
	var steps []byte
	if err := r.Scan(&p.Name, &p.Description, &steps); err != nil {
		return p, err
	}
	_ = json.Unmarshal(steps, &p.Steps)
	return p, nil
}

func (s *Store) CreatePipeline(ctx context.Context, p Pipeline) error {
	steps, _ := json.Marshal(p.Steps)
	_, err := s.db.ExecContext(ctx, `INSERT INTO pipeline(name, description, steps) VALUES (?,?,?)`, p.Name, p.Description, string(steps))
	return err
}

func (s *Store) GetPipeline(ctx context.Context, name string) (Pipeline, error) {
	p, err := scanPipeline(s.db.QueryRowContext(ctx, `SELECT name, description, steps FROM pipeline WHERE name=?`, name))
	return p, notFound(err)
}

func (s *Store) ListPipelines(ctx context.Context) ([]Pipeline, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, description, steps FROM pipeline ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Pipeline{}
	for rows.Next() {
		p, err := scanPipeline(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) UpdatePipeline(ctx context.Context, p Pipeline) error {
	steps, _ := json.Marshal(p.Steps)
	if _, err := s.db.ExecContext(ctx, `UPDATE pipeline SET description=?, steps=? WHERE name=?`, p.Description, string(steps), p.Name); err != nil {
		return err
	}
	var x int64
	return notFound(s.db.QueryRowContext(ctx, `SELECT id FROM pipeline WHERE name=?`, p.Name).Scan(&x))
}

func (s *Store) DeletePipeline(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM pipeline WHERE name=?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// validateSteps:build 必须是第一步且只能出现一次;deploy 可选,只能在 build 之后出现一次。
func validateSteps(steps []string) error {
	if len(steps) == 0 || steps[0] != "build" {
		return errors.New(`steps must start with "build"`)
	}
	if len(steps) > 2 || (len(steps) == 2 && steps[1] != "deploy") {
		return errors.New(`steps must be ["build"] or ["build","deploy"]`)
	}
	return nil
}

func hasStep(steps []string, name string) bool {
	for _, s := range steps {
		if s == name {
			return true
		}
	}
	return false
}

// ---- 运行 ----

const (
	RunPending   = "pending"
	RunBuilding  = "building"
	RunDeploying = "deploying"
	RunSucceeded = "succeeded"
	RunFailed    = "failed"
	RunCancelled = "cancelled"
)

func isTerminal(status string) bool {
	return status == RunSucceeded || status == RunFailed || status == RunCancelled
}

type Run struct {
	ID          int64      `json:"id"`
	Pipeline    string     `json:"pipeline"`
	Builder     string     `json:"builder"`
	Service     string     `json:"service"`
	Env         string     `json:"env"`
	Ref         string     `json:"ref"`
	Replicas    *int       `json:"replicas,omitempty"`
	Status      string     `json:"status"`
	Image       string     `json:"image,omitempty"`
	Message     string     `json:"message,omitempty"`
	TriggeredBy string     `json:"triggered_by"`
	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`

	// 以下仅执行器使用
	CancelRequested bool              `json:"-"`
	JobName         string            `json:"-"`
	DeployVersion   int64             `json:"-"`
	PhaseStartedAt  *time.Time        `json:"-"`
	Steps           []string          `json:"-"`
	RepoURL         string            `json:"-"`
	ContextPath     string            `json:"-"`
	Dockerfile      string            `json:"-"`
	BuildArgs       map[string]string `json:"-"`
}

const runSelect = `SELECT r.id, p.name, b.name, v.name, e.name, r.ref, r.replicas, r.status, r.image, COALESCE(r.message,''),
  r.triggered_by, r.created_at, r.started_at, r.finished_at,
  r.cancel_requested, r.job_name, r.deploy_version, r.phase_started_at, p.steps, b.repo_url, b.context_path, b.dockerfile, b.build_args
  FROM pipeline_run r JOIN pipeline p ON p.id=r.pipeline_id JOIN builder b ON b.id=r.builder_id
  JOIN service v ON v.id=b.service_id JOIN environment e ON e.id=r.env_id `

func scanRun(r scanner) (Run, error) {
	var x Run
	var repl sql.NullInt64
	var started, finished, phase sql.NullTime
	var steps, args []byte
	if err := r.Scan(&x.ID, &x.Pipeline, &x.Builder, &x.Service, &x.Env, &x.Ref, &repl, &x.Status, &x.Image, &x.Message,
		&x.TriggeredBy, &x.CreatedAt, &started, &finished,
		&x.CancelRequested, &x.JobName, &x.DeployVersion, &phase, &steps, &x.RepoURL, &x.ContextPath, &x.Dockerfile, &args); err != nil {
		return x, err
	}
	if repl.Valid {
		v := int(repl.Int64)
		x.Replicas = &v
	}
	if started.Valid {
		x.StartedAt = &started.Time
	}
	if finished.Valid {
		x.FinishedAt = &finished.Time
	}
	if phase.Valid {
		x.PhaseStartedAt = &phase.Time
	}
	_ = json.Unmarshal(steps, &x.Steps)
	if len(args) > 0 {
		_ = json.Unmarshal(args, &x.BuildArgs)
	}
	return x, nil
}

// CreateRun 创建一次运行:选定 builder 和 env;ref 不填则用 builder 的默认 ref。
func (s *Store) CreateRun(ctx context.Context, actor, pipeline, builder, env, ref string, replicas *int) (Run, error) {
	var pID, bID, eID int64
	var defRef string
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM pipeline WHERE name=?`, pipeline).Scan(&pID); err != nil {
		return Run{}, fmt.Errorf("pipeline %q: %w", pipeline, notFound(err))
	}
	if err := s.db.QueryRowContext(ctx, `SELECT id, ref FROM builder WHERE name=?`, builder).Scan(&bID, &defRef); err != nil {
		return Run{}, fmt.Errorf("builder %q: %w", builder, notFound(err))
	}
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM environment WHERE name=?`, env).Scan(&eID); err != nil {
		return Run{}, fmt.Errorf("env %q: %w", env, notFound(err))
	}
	if ref == "" {
		ref = defRef
	}
	var repl any
	if replicas != nil {
		repl = *replicas
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO pipeline_run(pipeline_id, builder_id, env_id, ref, replicas, triggered_by) VALUES (?,?,?,?,?,?)`,
		pID, bID, eID, ref, repl, actor)
	if err != nil {
		return Run{}, err
	}
	id, _ := res.LastInsertId()
	return s.GetRun(ctx, id)
}

func (s *Store) GetRun(ctx context.Context, id int64) (Run, error) {
	r, err := scanRun(s.db.QueryRowContext(ctx, runSelect+`WHERE r.id=?`, id))
	return r, notFound(err)
}

type RunFilter struct {
	Pipeline, Builder, Env, Status string
	Limit                          int
}

func (s *Store) ListRuns(ctx context.Context, f RunFilter) ([]Run, error) {
	q, args := runSelect+`WHERE 1=1`, []any{}
	for col, v := range map[string]string{"p.name": f.Pipeline, "b.name": f.Builder, "e.name": f.Env, "r.status": f.Status} {
		if v != "" {
			q += ` AND ` + col + `=?`
			args = append(args, v)
		}
	}
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	q += fmt.Sprintf(` ORDER BY r.id DESC LIMIT %d`, f.Limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ActiveRuns 执行器每轮要处理的运行(未结束的),按 id 升序,先来的先构建。
func (s *Store) ActiveRuns(ctx context.Context) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, runSelect+`WHERE r.status IN ('pending','building','deploying') ORDER BY r.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunUpdate 是对一次运行的状态迁移;零值字段不更新。
type RunUpdate struct {
	Status        string
	Image         string
	JobName       string
	DeployVersion int64
	Message       *string
	BuildLog      *string
	PhaseStart    bool // 进入新阶段,记录阶段开始时间
	MarkStarted   bool
	Finish        bool
}

// UpdateRun 只在当前状态等于 expect 时才更新(比较并交换),避免并发迁移互相覆盖。返回是否更新成功。
func (s *Store) UpdateRun(ctx context.Context, id int64, expect string, u RunUpdate) (bool, error) {
	set, args := []string{}, []any{}
	add := func(col string, v any) { set = append(set, col+"=?"); args = append(args, v) }
	if u.Status != "" {
		add("status", u.Status)
	}
	if u.Image != "" {
		add("image", u.Image)
	}
	if u.JobName != "" {
		add("job_name", u.JobName)
	}
	if u.DeployVersion != 0 {
		add("deploy_version", u.DeployVersion)
	}
	if u.Message != nil {
		add("message", *u.Message)
	}
	if u.BuildLog != nil {
		add("build_log", *u.BuildLog)
	}
	if u.PhaseStart {
		set = append(set, "phase_started_at=CURRENT_TIMESTAMP(3)")
	}
	if u.MarkStarted {
		set = append(set, "started_at=CURRENT_TIMESTAMP(3)")
	}
	if u.Finish {
		set = append(set, "finished_at=CURRENT_TIMESTAMP(3)")
	}
	if len(set) == 0 {
		return true, nil
	}
	args = append(args, id, expect)
	res, err := s.db.ExecContext(ctx, `UPDATE pipeline_run SET `+strings.Join(set, ", ")+` WHERE id=? AND status=?`, args...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// RequestCancel 请求取消:真正的取消(删构建任务、改状态)由执行器完成。已结束的运行不能取消。
func (s *Store) RequestCancel(ctx context.Context, id int64) (Run, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE pipeline_run SET cancel_requested=TRUE WHERE id=? AND status IN ('pending','building','deploying')`, id)
	if err != nil {
		return Run{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		r, err := s.GetRun(ctx, id)
		if err != nil {
			return Run{}, err
		}
		if isTerminal(r.Status) {
			return r, ErrConflict
		}
	}
	return s.GetRun(ctx, id)
}

func (s *Store) RunLog(ctx context.Context, id int64) (string, error) {
	var log sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT build_log FROM pipeline_run WHERE id=?`, id).Scan(&log); err != nil {
		return "", notFound(err)
	}
	return log.String, nil
}

// GetEnvService 按环境名和服务名读一个部署(部署步骤用它判断是否真正就绪)。
func (s *Store) GetEnvService(ctx context.Context, env, svc string) (EnvService, error) {
	x, err := scanES(s.db.QueryRowContext(ctx, esSelect+`WHERE e.name=? AND v.name=?`, env, svc))
	return x, notFound(err)
}
