package zadigexport

import (
	"strings"
	"testing"
)

const hash = "$2a$10$abcdefghijklmnopqrstuuabcdefghijklmnopqrstuvwxyz01234"

func TestConvert(t *testing.T) {
	in := []Account{
		{UID: "u1", Account: "Bob", Name: "Bobby", Email: "Bob@OkCoin.jp ", Password: hash},
		{UID: "u2", Account: "alice", Email: "alice@okcoin.jp", Password: hash},
		{UID: "u3", Account: "nopass", Email: "n@okcoin.jp", Password: ""},
		{UID: "u4", Account: "plain", Email: "p@okcoin.jp", Password: "not-a-hash"},
		{UID: "u5", Account: "noemail", Email: "", Password: hash},
		{UID: "u6", Account: "bademail", Email: "bademail", Password: hash},
		{UID: "u7", Account: "bob2", Email: "bob@okcoin.jp", Password: hash},
		{UID: "u8", Account: "carol@okcoin.jp", Email: "carol@okcoin.jp", Password: hash},
	}
	got, skipped := Convert(in)

	logins := map[string]StaticPassword{}
	for _, g := range got {
		logins[g.Email] = g
	}
	// 能用用户名登录的:有哈希的全部账号;能用邮箱登录的:邮箱有效且不冲突
	for _, want := range []string{"alice", "alice@okcoin.jp", "bob", "bob@okcoin.jp", "noemail", "bademail", "bob2", "carol@okcoin.jp"} {
		if _, ok := logins[want]; !ok {
			t.Fatalf("login %q missing, got %v", want, logins)
		}
	}
	for _, not := range []string{"nopass", "plain", "n@okcoin.jp", "p@okcoin.jp"} {
		if _, ok := logins[not]; ok {
			t.Fatalf("login %q must not be exported", not)
		}
	}
	// 用用户名或邮箱登录,是同一个人:username(= preferred_username)和 userID 一致
	a1, a2 := logins["alice"], logins["alice@okcoin.jp"]
	if a1.Username != "alice" || a1.Username != a2.Username || a1.UserID != a2.UserID || a1.Hash != hash {
		t.Fatalf("alice logins diverge: %+v vs %+v", a1, a2)
	}
	// 邮箱登录名按小写比较;用户名保持 Zadig 里的写法
	if logins["bob"].Username != "Bob" {
		t.Fatalf("username casing lost: %+v", logins["bob"])
	}
	// 用户名恰好等于邮箱时只生成一条
	count := 0
	for _, g := range got {
		if g.Email == "carol@okcoin.jp" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("carol@okcoin.jp exported %d times", count)
	}

	reasons := map[string]string{}
	for _, s := range skipped {
		reasons[s.Account] += s.Reason + ";"
	}
	for acct, frag := range map[string]string{
		"nopass": "no bcrypt", "plain": "no bcrypt", "noemail": "no email",
		"bademail": "invalid email", "bob2": "already used by Bob",
	} {
		if !strings.Contains(reasons[acct], frag) {
			t.Fatalf("%s: want reason containing %q, got %q", acct, frag, reasons[acct])
		}
	}
}

func TestRenderEmpty(t *testing.T) {
	b, err := Render(nil)
	if err != nil || !strings.Contains(string(b), "staticPasswords: []") {
		t.Fatalf("%q %v", b, err)
	}
}
