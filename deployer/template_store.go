package main

import (
	"context"
)

// ---- 构建模板:可被多个构建器共用的构建配方 ----

type Template struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Dockerfile  string `json:"dockerfile"`        // Dockerfile 的内容(必填)
	Script      string `json:"script,omitempty"`  // 构建脚本的内容(可选)
	Image       string `json:"image,omitempty"`   // 构建环境镜像(有脚本时必填)
	Profile     string `json:"profile,omitempty"` // 默认 profile;引用它的 builder 或运行可以覆盖
	UsedBy      int    `json:"used_by"`           // 有多少个构建器在引用(只读)
}

const templateSelect = `SELECT t.name, t.description, t.dockerfile, COALESCE(t.script,''), t.image, t.profile,
  (SELECT COUNT(*) FROM builder b WHERE b.template_id=t.id) FROM build_template t `

func scanTemplate(r scanner) (Template, error) {
	var t Template
	err := r.Scan(&t.Name, &t.Description, &t.Dockerfile, &t.Script, &t.Image, &t.Profile, &t.UsedBy)
	return t, err
}

func (s *Store) CreateTemplate(ctx context.Context, t Template) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO build_template(name, description, script, dockerfile, image, profile) VALUES (?,?,?,?,?,?)`,
		t.Name, t.Description, nullStr(t.Script), t.Dockerfile, t.Image, t.Profile)
	return err
}

func (s *Store) GetTemplate(ctx context.Context, name string) (Template, error) {
	t, err := scanTemplate(s.db.QueryRowContext(ctx, templateSelect+`WHERE t.name=?`, name))
	return t, notFound(err)
}

func (s *Store) ListTemplates(ctx context.Context) ([]Template, error) {
	rows, err := s.db.QueryContext(ctx, templateSelect+`ORDER BY t.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Template{}
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpdateTemplate 修改模板:引用它的所有构建器之后的运行都会用新内容(已创建的运行带着创建时的快照,不受影响)。
func (s *Store) UpdateTemplate(ctx context.Context, t Template) error {
	res, err := s.db.ExecContext(ctx, `UPDATE build_template SET description=?, script=?, dockerfile=?, image=?, profile=? WHERE name=?`,
		t.Description, nullStr(t.Script), t.Dockerfile, t.Image, t.Profile, t.Name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 { // 值没变时 RowsAffected 也是 0,再确认是否存在
		var x int64
		if err := s.db.QueryRowContext(ctx, `SELECT id FROM build_template WHERE name=?`, t.Name).Scan(&x); err != nil {
			return notFound(err)
		}
	}
	return nil
}

// DeleteTemplate:还有构建器引用时外键拒绝(API 转成 409)。
func (s *Store) DeleteTemplate(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM build_template WHERE name=?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
