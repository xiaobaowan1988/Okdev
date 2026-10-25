// Package dexusers 通过 Dex 的 gRPC 管理接口管理登录账号(新增、改密码、删除、导入)。
//
// 一个"用户"在 Dex 里是一组密码记录:Dex 把记录的 email 字段当登录名,所以用户名和邮箱各一条,
// 它们的 username(= K8s 里的身份)、userID、密码哈希相同。
package dexusers

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/dexidp/dex/api/v2"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/protobuf/encoding/protowire"
)

// API 是用到的那部分 Dex gRPC 客户端,便于测试替换。
type API interface {
	CreatePassword(ctx context.Context, in *api.CreatePasswordReq) (*api.CreatePasswordResp, error)
	UpdatePassword(ctx context.Context, in *api.UpdatePasswordReq) (*api.UpdatePasswordResp, error)
	DeletePassword(ctx context.Context, in *api.DeletePasswordReq) (*api.DeletePasswordResp, error)
	ListPasswords(ctx context.Context, in *api.ListPasswordReq) (*api.ListPasswordResp, error)
	ListRefresh(ctx context.Context, in *api.ListRefreshReq) (*api.ListRefreshResp, error)
	RevokeRefresh(ctx context.Context, in *api.RevokeRefreshReq) (*api.RevokeRefreshResp, error)
}

// Manager 在 API 之上实现账号管理。
type Manager struct{ API API }

// User 是按 userID 聚合后的一个用户。
type User struct {
	UserID   string
	Username string
	Logins   []string // 小写登录名:用户名、邮箱
}

const minPasswordLen = 8

var ErrNotFound = errors.New("user not found")

// Users 列出所有用户(按用户名排序)。
func (m *Manager) Users(ctx context.Context) ([]User, error) {
	resp, err := m.API.ListPasswords(ctx, &api.ListPasswordReq{})
	if err != nil {
		return nil, err
	}
	return group(resp.Passwords), nil
}

func group(ps []*api.Password) []User {
	byID := map[string]*User{}
	for _, p := range ps {
		u := byID[p.UserId]
		if u == nil {
			u = &User{UserID: p.UserId, Username: p.Username}
			byID[p.UserId] = u
		}
		u.Logins = append(u.Logins, p.Email)
	}
	out := make([]User, 0, len(byID))
	for _, u := range byID {
		sort.Strings(u.Logins)
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out
}

func find(users []User, username string) (User, bool) {
	for _, u := range users {
		if strings.EqualFold(u.Username, username) {
			return u, true
		}
	}
	return User{}, false
}

// ValidateUsername:用户名就是登录名也是 K8s 里的身份。不允许 @(会和邮箱登录名混淆)和空白。
func ValidateUsername(s string) error {
	switch {
	case s == "":
		return errors.New("username is required")
	case strings.ContainsAny(s, "@ \t\r\n"):
		return errors.New("username must not contain '@' or whitespace")
	}
	return nil
}

func hash(password string) ([]byte, error) {
	if len(password) < minPasswordLen {
		return nil, fmt.Errorf("password must be at least %d characters", minPasswordLen)
	}
	return bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
}

// Add 新增用户:email 可为空,此时只能用用户名登录。
func (m *Manager) Add(ctx context.Context, username, email, password string) (User, error) {
	if err := ValidateUsername(username); err != nil {
		return User{}, err
	}
	email = strings.TrimSpace(email)
	if email != "" && !strings.Contains(email, "@") {
		return User{}, fmt.Errorf("invalid email %q", email)
	}
	h, err := hash(password)
	if err != nil {
		return User{}, err
	}
	logins := []string{strings.ToLower(username)}
	if email != "" && !strings.EqualFold(email, username) {
		logins = append(logins, strings.ToLower(email))
	}
	existing, err := m.API.ListPasswords(ctx, &api.ListPasswordReq{})
	if err != nil {
		return User{}, err
	}
	taken := map[string]bool{}
	for _, p := range existing.Passwords {
		taken[strings.ToLower(p.Email)] = true
	}
	for _, l := range logins {
		if taken[l] {
			return User{}, fmt.Errorf("login name %q is already in use", l)
		}
	}
	u := User{UserID: uuid.NewString(), Username: username, Logins: logins}
	for _, l := range logins {
		resp, err := m.API.CreatePassword(ctx, &api.CreatePasswordReq{Password: &api.Password{Email: l, Hash: h, Username: username, UserId: u.UserID}})
		if err != nil {
			return User{}, fmt.Errorf("create %q (earlier logins of this user may already exist, check with list): %w", l, err)
		}
		if resp.AlreadyExists {
			return User{}, fmt.Errorf("login name %q is already in use", l)
		}
	}
	return u, nil
}

// SetPassword 重置密码,并撤销该用户已有的刷新令牌,让旧会话无法续期。
func (m *Manager) SetPassword(ctx context.Context, username, password string) error {
	h, err := hash(password)
	if err != nil {
		return err
	}
	users, err := m.Users(ctx)
	if err != nil {
		return err
	}
	u, ok := find(users, username)
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, username)
	}
	for _, l := range u.Logins {
		resp, err := m.API.UpdatePassword(ctx, &api.UpdatePasswordReq{Email: l, NewHash: h, NewUsername: u.Username})
		if err != nil {
			return fmt.Errorf("update %q: %w", l, err)
		}
		if resp.NotFound {
			return fmt.Errorf("login %q disappeared while updating", l)
		}
	}
	return m.revokeRefresh(ctx, u.UserID)
}

// Delete 删除用户的所有登录名并撤销刷新令牌。已签发的 ID Token 不能撤销,到期(默认 8 小时)前仍然有效。
func (m *Manager) Delete(ctx context.Context, username string) error {
	users, err := m.Users(ctx)
	if err != nil {
		return err
	}
	u, ok := find(users, username)
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, username)
	}
	for _, l := range u.Logins {
		if _, err := m.API.DeletePassword(ctx, &api.DeletePasswordReq{Email: l}); err != nil {
			return fmt.Errorf("delete %q: %w", l, err)
		}
	}
	return m.revokeRefresh(ctx, u.UserID)
}

// localConnectorID 是 Dex 内置密码库的连接器 ID。
const localConnectorID = "local"

// subject 把 userID 编码成 Dex 在 ID Token 的 sub 里用的形式。
// ListRefresh / RevokeRefresh 的 user_id 要的是这个 sub,不是原始 userID;传原始值会得到 "cannot parse invalid wire-format data"。
// 编码:protobuf message IDTokenSubject{ string user_id = 1; string conn_id = 2 },再做无填充的 URL-safe base64。
func subject(userID string) string {
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.BytesType)
	b = protowire.AppendString(b, userID)
	b = protowire.AppendTag(b, 2, protowire.BytesType)
	b = protowire.AppendString(b, localConnectorID)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (m *Manager) revokeRefresh(ctx context.Context, userID string) error {
	sub := subject(userID)
	refs, err := m.API.ListRefresh(ctx, &api.ListRefreshReq{UserId: sub})
	if err != nil {
		return fmt.Errorf("list refresh tokens: %w", err)
	}
	for _, r := range refs.RefreshTokens {
		if _, err := m.API.RevokeRefresh(ctx, &api.RevokeRefreshReq{UserId: sub, ClientId: r.ClientId}); err != nil {
			return fmt.Errorf("revoke refresh token: %w", err)
		}
	}
	return nil
}

// Entry 是导入文件(zadig-export 的输出)里的一条登录名。
type Entry struct {
	Email, Hash, Username, UserID string
}

// ImportResult 汇总导入结果。
type ImportResult struct{ Created, Updated int }

// Import 把导出的登录名写进 Dex。可重复执行:已存在的登录名更新哈希和用户名,不存在的创建;不会删除任何已有账号。
func (m *Manager) Import(ctx context.Context, entries []Entry) (ImportResult, error) {
	var res ImportResult
	for _, e := range entries {
		if _, err := bcrypt.Cost([]byte(e.Hash)); err != nil {
			return res, fmt.Errorf("%s: not a bcrypt hash", e.Email)
		}
		resp, err := m.API.CreatePassword(ctx, &api.CreatePasswordReq{Password: &api.Password{
			Email: strings.ToLower(e.Email), Hash: []byte(e.Hash), Username: e.Username, UserId: e.UserID}})
		if err != nil {
			return res, fmt.Errorf("create %q: %w", e.Email, err)
		}
		if !resp.AlreadyExists {
			res.Created++
			continue
		}
		if _, err := m.API.UpdatePassword(ctx, &api.UpdatePasswordReq{Email: strings.ToLower(e.Email), NewHash: []byte(e.Hash), NewUsername: e.Username}); err != nil {
			return res, fmt.Errorf("update %q: %w", e.Email, err)
		}
		res.Updated++
	}
	return res, nil
}
