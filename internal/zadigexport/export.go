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

// StaticPassword 是 Dex 配置里 staticPasswords 的一项。Email 是登录名,可以是用户名也可以是邮箱。
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

// Convert 把账号转成 Dex 静态账号。Dex 把静态账号的 email 字段当登录名,所以每个 Zadig 账号最多生成两条:
// 一条以用户名登录,一条以邮箱登录;两条的 username 都是 Zadig 账号名、userID 相同,
// kube-apiserver 用 preferred_username(= 账号名)当用户名,所以无论用哪个登录,RBAC 里都是同一个人。
//
// 不能安全导出的会跳过并给出原因,而不是悄悄丢掉或导出坏数据:
//   - 没有 bcrypt 密码哈希(例如密码为空):整个账号跳过;
//   - 用户名/邮箱与已导出的登录名冲突(Dex 要求登录名唯一):该登录名跳过;邮箱缺失或格式不对同理(此时仍可用用户名登录)。
func Convert(accounts []Account) ([]StaticPassword, []Skipped) {
	var out []StaticPassword
	var skipped []Skipped
	seen := map[string]string{} // 登录名(小写) -> 占用它的 Zadig 账号
	add := func(a Account, login, kind string) {
		login = strings.ToLower(strings.TrimSpace(login))
		if prev, dup := seen[login]; dup {
			skipped = append(skipped, Skipped{a.Account, fmt.Sprintf("%s %q already used by %s, login by %s not available", kind, login, prev, kind)})
			return
		}
		seen[login] = a.Account
		out = append(out, StaticPassword{Email: login, Hash: a.Password, Username: a.Account, UserID: a.UID})
	}
	for _, a := range accounts {
		if !isBcrypt(a.Password) {
			skipped = append(skipped, Skipped{a.Account, "no bcrypt password hash"})
			continue
		}
		if strings.TrimSpace(a.Account) == "" {
			skipped = append(skipped, Skipped{a.UID, "empty account name"})
			continue
		}
		add(a, a.Account, "username")
		email := strings.TrimSpace(a.Email)
		switch {
		case email == "":
			skipped = append(skipped, Skipped{a.Account, "no email, login by email not available"})
		case !strings.Contains(email, "@"):
			skipped = append(skipped, Skipped{a.Account, "invalid email, login by email not available"})
		case strings.EqualFold(email, a.Account):
			// 用户名就是邮箱,上面已经生成
		default:
			add(a, email, "email")
		}
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
