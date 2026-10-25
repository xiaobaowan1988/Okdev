// Package zadigexport 把 Zadig 本地账号(MySQL)导出成 Dex 的 staticPasswords。
//
// Zadig 用 bcrypt 存本地密码(user_login.password),Dex 的静态账号要求的也是 bcrypt,
// 所以哈希可以原样复制,开发的原密码继续有效。
package zadigexport

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// Account 是 Zadig 里的一个本地账号。
type Account struct {
	UID      string
	Account  string
	Name     string
	Email    string
	Password string // bcrypt 哈希
}

// StaticPassword 是 Dex 配置里 staticPasswords 的一项。
type StaticPassword struct {
	Email    string `json:"email"`
	Hash     string `json:"hash"`
	Username string `json:"username"`
	UserID   string `json:"userID"`
}

// Skipped 记录没能导出的账号及原因。
type Skipped struct {
	Account string
	Reason  string
}

// Query 读出 identityType 类型的账号,只读,不改库。Zadig 的本地账号是 "system"。
func Query(ctx context.Context, db *sql.DB, identityType string) ([]Account, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT u.uid, u.account, u.name, u.email, COALESCE(l.password, '')
		FROM user u
		LEFT JOIN user_login l ON l.uid = u.uid AND l.login_type = 0
		WHERE u.identity_type = ?
		ORDER BY u.account`, identityType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.UID, &a.Account, &a.Name, &a.Email, &a.Password); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Convert 把账号转成 Dex 静态账号。不能安全导出的账号会被跳过并给出原因,而不是悄悄丢掉或导出坏数据:
//   - 没有 bcrypt 密码哈希(例如密码为空);
//   - 没有邮箱(Dex 用邮箱登录,K8s 也用邮箱当用户名);
//   - 邮箱和已导出的账号重复(Dex 要求邮箱唯一)。
func Convert(accounts []Account) ([]StaticPassword, []Skipped) {
	var out []StaticPassword
	var skipped []Skipped
	seen := map[string]string{}
	for _, a := range accounts {
		email := strings.ToLower(strings.TrimSpace(a.Email))
		switch {
		case !isBcrypt(a.Password):
			skipped = append(skipped, Skipped{a.Account, "no bcrypt password hash"})
			continue
		case email == "" || !strings.Contains(email, "@"):
			skipped = append(skipped, Skipped{a.Account, "no valid email"})
			continue
		}
		if prev, dup := seen[email]; dup {
			skipped = append(skipped, Skipped{a.Account, fmt.Sprintf("email %s already used by %s", email, prev)})
			continue
		}
		seen[email] = a.Account
		name := a.Name
		if name == "" {
			name = a.Account
		}
		out = append(out, StaticPassword{Email: email, Hash: a.Password, Username: name, UserID: a.UID})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out, skipped
}

func isBcrypt(h string) bool {
	return len(h) == 60 && (strings.HasPrefix(h, "$2a$") || strings.HasPrefix(h, "$2b$") || strings.HasPrefix(h, "$2y$"))
}

// Render 生成 Dex 配置里的 staticPasswords 片段。
func Render(pw []StaticPassword) ([]byte, error) {
	if pw == nil {
		pw = []StaticPassword{}
	}
	return yaml.Marshal(map[string]any{"staticPasswords": pw})
}
