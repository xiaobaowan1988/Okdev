package zadigexport

import (
	"strings"
	"testing"
)

const hash = "$2a$10$abcdefghijklmnopqrstuuabcdefghijklmnopqrstuvwxyz01234"

func TestConvert(t *testing.T) {
	in := []Account{
		{UID: "u1", Account: "bob", Name: "Bob", Email: "Bob@OkCoin.jp ", Password: hash},
		{UID: "u2", Account: "alice", Name: "", Email: "alice@okcoin.jp", Password: hash},
		{UID: "u3", Account: "nopass", Email: "n@okcoin.jp", Password: ""},
		{UID: "u4", Account: "plain", Email: "p@okcoin.jp", Password: "not-a-hash"},
		{UID: "u5", Account: "noemail", Email: "", Password: hash},
		{UID: "u6", Account: "bademail", Email: "bademail", Password: hash},
		{UID: "u7", Account: "bob2", Email: "bob@okcoin.jp", Password: hash},
	}
	got, skipped := Convert(in)

	if len(got) != 2 || got[0].Email != "alice@okcoin.jp" || got[1].Email != "bob@okcoin.jp" {
		t.Fatalf("exported %+v", got)
	}
	if got[0].Username != "alice" || got[0].UserID != "u2" || got[0].Hash != hash {
		t.Fatalf("alice mapping wrong: %+v", got[0])
	}
	reasons := map[string]string{}
	for _, s := range skipped {
		reasons[s.Account] = s.Reason
	}
	for _, acct := range []string{"nopass", "plain", "noemail", "bademail", "bob2"} {
		if reasons[acct] == "" {
			t.Fatalf("%s should have been skipped, skipped=%v", acct, skipped)
		}
	}
	if !strings.Contains(reasons["bob2"], "already used by bob") {
		t.Fatalf("duplicate reason: %q", reasons["bob2"])
	}
}

func TestRenderEmpty(t *testing.T) {
	b, err := Render(nil)
	if err != nil || !strings.Contains(string(b), "staticPasswords: []") {
		t.Fatalf("%q %v", b, err)
	}
}
