package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/go-sql-driver/mysql"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("version conflict")
)

// 期望状态(desired_state/replicas/image)由 API 写入,观测状态(observed_*)由对账循环写入。
// version 每次修改 +1,既是乐观锁,也是对账循环判断"是否已应用最新期望"的依据(applied_version)。
type EnvService struct {
	ID              int64     `json:"id"`
	Env             string    `json:"env"`
	Service         string    `json:"service"`
	DesiredState    string    `json:"desired_state"`
	Replicas        int       `json:"replicas"`
	Image           string    `json:"image,omitempty"`
	Version         int64     `json:"version"`
	AppliedVersion  int64     `json:"applied_version"`
	ObservedSync    string    `json:"observed_sync"`
	ObservedHealth  string    `json:"observed_health"`
	ObservedMessage string    `json:"observed_message,omitempty"`
	UpdatedAt       time.Time `json:"updated_at"`

	// 来自 service 表,对账用
	Repo, Path, Revision, Workload, ImageName string `json:"-"`
}

type Service struct {
	Name      string `json:"name"`
	RepoURL   string `json:"repo_url"`
	Path      string `json:"path"`
	Revision  string `json:"revision"`
	Workload  string `json:"workload"`   // kustomize 里要调副本数的 Deployment 名
	ImageName string `json:"image_name"` // 清单里的镜像名(不带 tag),用于镜像覆盖;可空
}

type HistoryEntry struct {
	ID        int64           `json:"id"`
	Actor     string          `json:"actor"`
	Action    string          `json:"action"`
	Before    json.RawMessage `json:"before,omitempty"`
	After     json.RawMessage `json:"after,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

type Store struct{ db *sql.DB }

var ddl = []string{
	`CREATE TABLE IF NOT EXISTS environment (
	  id BIGINT AUTO_INCREMENT PRIMARY KEY,
	  name VARCHAR(63) NOT NULL UNIQUE,
	  created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
	`CREATE TABLE IF NOT EXISTS service (
	  id BIGINT AUTO_INCREMENT PRIMARY KEY,
	  name VARCHAR(63) NOT NULL UNIQUE,
	  repo_url VARCHAR(255) NOT NULL,
	  path VARCHAR(255) NOT NULL,
	  revision VARCHAR(255) NOT NULL DEFAULT 'HEAD',
	  workload VARCHAR(63) NOT NULL,
	  image_name VARCHAR(255) NOT NULL DEFAULT '',
	  created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
	`CREATE TABLE IF NOT EXISTS env_service (
	  id BIGINT AUTO_INCREMENT PRIMARY KEY,
	  env_id BIGINT NOT NULL,
	  service_id BIGINT NOT NULL,
	  desired_state ENUM('running','deleted') NOT NULL DEFAULT 'running',
	  replicas INT NOT NULL DEFAULT 1,
	  image VARCHAR(255) NOT NULL DEFAULT '',
	  version BIGINT NOT NULL DEFAULT 1,
	  applied_version BIGINT NOT NULL DEFAULT 0,
	  observed_sync VARCHAR(32) NOT NULL DEFAULT '',
	  observed_health VARCHAR(32) NOT NULL DEFAULT '',
	  observed_message TEXT NULL,
	  updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
	  UNIQUE KEY uq_env_service (env_id, service_id),
	  CONSTRAINT fk_es_env FOREIGN KEY (env_id) REFERENCES environment(id),
	  CONSTRAINT fk_es_svc FOREIGN KEY (service_id) REFERENCES service(id))`,
	// 历史按名字记录且不设外键:env_service 行被清理后审计记录仍在
	`CREATE TABLE IF NOT EXISTS deploy_history (
	  id BIGINT AUTO_INCREMENT PRIMARY KEY,
	  env_name VARCHAR(63) NOT NULL,
	  service_name VARCHAR(63) NOT NULL,
	  actor VARCHAR(64) NOT NULL,
	  action VARCHAR(16) NOT NULL,
	  before_state JSON NULL,
	  after_state JSON NULL,
	  created_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
	  KEY idx_hist (env_name, service_name, id))`,
}

// Migrate 在 GET_LOCK 保护下建表,多个副本同时启动也安全。
func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var got int
	if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK('deployer-migrate', 30)`).Scan(&got); err != nil || got != 1 {
		return fmt.Errorf("migrate lock: %v (got=%d)", err, got)
	}
	defer conn.ExecContext(context.Background(), `SELECT RELEASE_LOCK('deployer-migrate')`)
	for _, stmt := range ddl {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	// 老库去掉已废弃的 env_group 列(曾用于按 group 分命名空间/AppProject,现统一为一个)
	var n int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.COLUMNS
	  WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='environment' AND COLUMN_NAME='env_group'`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		if _, err := conn.ExecContext(ctx, `ALTER TABLE environment DROP COLUMN env_group`); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) CreateEnv(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO environment(name) VALUES (?)`, name)
	return err
}

// 删除环境:还有部署(env_service 外键)时数据库会拒绝,由 API 转成 409。
func (s *Store) DeleteEnv(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM environment WHERE name=?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListEnvs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM environment ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) CreateService(ctx context.Context, v Service) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO service(name, repo_url, path, revision, workload, image_name) VALUES (?,?,?,?,?,?)`,
		v.Name, v.RepoURL, v.Path, v.Revision, v.Workload, v.ImageName)
	return err
}

// UpdateService 修改服务定义(名字不可改)。服务定义变了,它所有部署的期望状态也就变了:
// 提升这些部署的 version(使旧的 expected_version 失效,也让对账循环重新核对),并给每个部署写一条历史。
func (s *Store) UpdateService(ctx context.Context, actor string, v Service) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var id int64
	var before Service
	before.Name = v.Name
	if err := tx.QueryRowContext(ctx,
		`SELECT id, repo_url, path, revision, workload, image_name FROM service WHERE name=? FOR UPDATE`, v.Name,
	).Scan(&id, &before.RepoURL, &before.Path, &before.Revision, &before.Workload, &before.ImageName); err != nil {
		return notFound(err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE service SET repo_url=?, path=?, revision=?, workload=?, image_name=? WHERE id=?`,
		v.RepoURL, v.Path, v.Revision, v.Workload, v.ImageName, id); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT e.name FROM env_service es JOIN environment e ON e.id=es.env_id WHERE es.service_id=?`, id)
	if err != nil {
		return err
	}
	var envs []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		envs = append(envs, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	bj, _ := json.Marshal(before)
	aj, _ := json.Marshal(v)
	for _, env := range envs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO deploy_history(env_name, service_name, actor, action, before_state, after_state) VALUES (?,?,?,?,?,?)`,
			env, v.Name, actor, "service-update", string(bj), string(aj)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE env_service SET version=version+1 WHERE service_id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteService 删除服务定义:还有部署引用它时数据库(外键)会拒绝,由 API 转成 409。
func (s *Store) DeleteService(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM service WHERE name=?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListServices(ctx context.Context) ([]Service, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, repo_url, path, revision, workload, image_name FROM service ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Service
	for rows.Next() {
		var v Service
		if err := rows.Scan(&v.Name, &v.RepoURL, &v.Path, &v.Revision, &v.Workload, &v.ImageName); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

const esSelect = `SELECT es.id, e.name, v.name, es.desired_state, es.replicas, es.image, es.version, es.applied_version,
  es.observed_sync, es.observed_health, COALESCE(es.observed_message,''), es.updated_at,
  v.repo_url, v.path, v.revision, v.workload, v.image_name
  FROM env_service es JOIN environment e ON e.id = es.env_id JOIN service v ON v.id = es.service_id `

type scanner interface{ Scan(...any) error }

func scanES(r scanner) (EnvService, error) {
	var x EnvService
	err := r.Scan(&x.ID, &x.Env, &x.Service, &x.DesiredState, &x.Replicas, &x.Image, &x.Version, &x.AppliedVersion,
		&x.ObservedSync, &x.ObservedHealth, &x.ObservedMessage, &x.UpdatedAt,
		&x.Repo, &x.Path, &x.Revision, &x.Workload, &x.ImageName)
	return x, err
}

func (s *Store) ListEnvServices(ctx context.Context, env string) ([]EnvService, error) {
	q, args := esSelect+`ORDER BY e.id, es.id`, []any{}
	if env != "" {
		q, args = esSelect+`WHERE e.name = ? ORDER BY es.id`, []any{env}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EnvService
	for rows.Next() {
		x, err := scanES(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

type Change struct {
	Replicas        *int
	Image           *string
	Delete          bool
	MustExist       bool   // scale / delete 只能作用于已存在的部署
	ExpectedVersion *int64 // 乐观锁:与当前 version 不一致则 ErrConflict
}

func snapshot(x *EnvService) json.RawMessage {
	if x == nil {
		return nil
	}
	b, _ := json.Marshal(map[string]any{"desired_state": x.DesiredState, "replicas": x.Replicas, "image": x.Image, "version": x.Version})
	return b
}

// Apply 在一个事务里完成"改期望状态 + 写历史"。行锁(FOR UPDATE)保证同一部署的并发修改串行化;
// 遇到死锁/锁等待超时会重试(MySQL 官方建议的处理方式)。
func (s *Store) Apply(ctx context.Context, actor, env, svc string, c Change) (EnvService, error) {
	var out EnvService
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if out, err = s.applyOnce(ctx, actor, env, svc, c); !retryable(err) {
			return out, err
		}
		time.Sleep(time.Duration(attempt+1)*20*time.Millisecond + time.Duration(rand.Intn(30))*time.Millisecond)
	}
	return out, err
}

func retryable(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && (me.Number == 1213 || me.Number == 1205) // deadlock / lock wait timeout
}

func (s *Store) applyOnce(ctx context.Context, actor, env, svc string, c Change) (EnvService, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return EnvService{}, err
	}
	defer tx.Rollback()

	var envID, svcID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM environment WHERE name=?`, env).Scan(&envID); err != nil {
		return EnvService{}, notFound(err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT id FROM service WHERE name=?`, svc).Scan(&svcID); err != nil {
		return EnvService{}, notFound(err)
	}

	// 先"插入或忽略"再锁行:对不存在的行直接 SELECT ... FOR UPDATE 会在唯一索引上加间隙锁,
	// 两个并发的"首次部署"会互相死锁(实测过)。行存在后加的是记录锁,不会。
	created := false
	if !c.MustExist && !c.Delete {
		repl, img := 1, ""
		if c.Replicas != nil {
			repl = *c.Replicas
		}
		if c.Image != nil {
			img = *c.Image
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO env_service(env_id, service_id, replicas, image) VALUES (?,?,?,?) ON DUPLICATE KEY UPDATE id=id`,
			envID, svcID, repl, img)
		if err != nil {
			return EnvService{}, err
		}
		n, _ := res.RowsAffected()
		created = n == 1
	}

	cur, err := scanES(tx.QueryRowContext(ctx, esSelect+`WHERE es.env_id=? AND es.service_id=? FOR UPDATE`, envID, svcID))
	if err != nil {
		return EnvService{}, notFound(err)
	}
	if !created && c.ExpectedVersion != nil && *c.ExpectedVersion != cur.Version {
		return EnvService{}, ErrConflict
	}

	action := "create"
	var before *EnvService
	if !created {
		b := cur
		before = &b
		action = "update"
		next := cur
		if c.Delete {
			action, next.DesiredState = "delete", "deleted"
		} else {
			next.DesiredState = "running" // 对已标记删除的部署再次写入 = 重新部署
			if c.Replicas != nil {
				if *c.Replicas != cur.Replicas {
					action = "scale"
				}
				next.Replicas = *c.Replicas
			}
			if c.Image != nil {
				next.Image = *c.Image
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE env_service SET desired_state=?, replicas=?, image=?, version=version+1 WHERE id=?`,
			next.DesiredState, next.Replicas, next.Image, cur.ID); err != nil {
			return EnvService{}, err
		}
	}

	out, err := scanES(tx.QueryRowContext(ctx, esSelect+`WHERE es.env_id=? AND es.service_id=?`, envID, svcID))
	if err != nil {
		return EnvService{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO deploy_history(env_name, service_name, actor, action, before_state, after_state) VALUES (?,?,?,?,?,?)`,
		env, svc, actor, action, nullJSON(snapshot(before)), nullJSON(snapshot(&out))); err != nil {
		return EnvService{}, err
	}
	return out, tx.Commit()
}

func (s *Store) History(ctx context.Context, env, svc string) ([]HistoryEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, actor, action, before_state, after_state, created_at FROM deploy_history WHERE env_name=? AND service_name=? ORDER BY id`, env, svc)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HistoryEntry
	for rows.Next() {
		var h HistoryEntry
		var b, a []byte
		if err := rows.Scan(&h.ID, &h.Actor, &h.Action, &b, &a, &h.CreatedAt); err != nil {
			return nil, err
		}
		h.Before, h.After = b, a
		out = append(out, h)
	}
	return out, rows.Err()
}

// 对账循环写回观测状态。只在 version 未变化时才更新 applied_version,避免覆盖期间到来的新修改。
func (s *Store) SetObserved(ctx context.Context, x EnvService, sync, health, msg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE env_service SET applied_version=?, observed_sync=?, observed_health=?, observed_message=? WHERE id=?`,
		x.Version, sync, health, msg, x.ID)
	return err
}

func (s *Store) SetObservedOnly(ctx context.Context, x EnvService, sync, health, msg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE env_service SET observed_sync=?, observed_health=?, observed_message=? WHERE id=?`, sync, health, msg, x.ID)
	return err
}

// 仅当部署仍处于"已标记删除"且 version 未变时才物理删除。
func (s *Store) DeleteRow(ctx context.Context, x EnvService) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM env_service WHERE id=? AND desired_state='deleted' AND version=?`, x.ID, x.Version)
	return err
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func nullJSON(b json.RawMessage) any {
	if b == nil {
		return nil
	}
	return string(b)
}
