package dexusers

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/dexidp/dex/api/v2"
	"golang.org/x/crypto/bcrypt"
)

// fakeAPI 在内存里模拟 Dex 的密码库和刷新令牌。
type fakeAPI struct {
	pw      map[string]*api.Password
	refresh map[string][]string // userID -> clientIDs
	revoked []string
}

func newFake() *fakeAPI {
	return &fakeAPI{pw: map[string]*api.Password{}, refresh: map[string][]string{}}
}

func (f *fakeAPI) CreatePassword(_ context.Context, in *api.CreatePasswordReq) (*api.CreatePasswordResp, error) {
	k := strings.ToLower(in.Password.Email)
	if _, ok := f.pw[k]; ok {
		return &api.CreatePasswordResp{AlreadyExists: true}, nil
	}
	pw := in.Password
	f.pw[k] = &api.Password{Email: k, Hash: pw.Hash, Username: pw.Username, UserId: pw.UserId}
	return &api.CreatePasswordResp{}, nil
}
func (f *fakeAPI) UpdatePassword(_ context.Context, in *api.UpdatePasswordReq) (*api.UpdatePasswordResp, error) {
	p, ok := f.pw[strings.ToLower(in.Email)]
	if !ok {
		return &api.UpdatePasswordResp{NotFound: true}, nil
	}
	p.Hash = in.NewHash
	if in.NewUsername != "" {
		p.Username = in.NewUsername
	}
	return &api.UpdatePasswordResp{}, nil
}
func (f *fakeAPI) DeletePassword(_ context.Context, in *api.DeletePasswordReq) (*api.DeletePasswordResp, error) {
	delete(f.pw, strings.ToLower(in.Email))
	return &api.DeletePasswordResp{}, nil
}
func (f *fakeAPI) ListPasswords(context.Context, *api.ListPasswordReq) (*api.ListPasswordResp, error) {
	var ps []*api.Password
	for _, p := range f.pw {
		ps = append(ps, p)
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].Email < ps[j].Email })
	return &api.ListPasswordResp{Passwords: ps}, nil
}
func (f *fakeAPI) ListRefresh(_ context.Context, in *api.ListRefreshReq) (*api.ListRefreshResp, error) {
	var refs []*api.RefreshTokenRef
	for _, c := range f.refresh[in.UserId] {
		refs = append(refs, &api.RefreshTokenRef{ClientId: c})
	}
	return &api.ListRefreshResp{RefreshTokens: refs}, nil
}
func (f *fakeAPI) RevokeRefresh(_ context.Context, in *api.RevokeRefreshReq) (*api.RevokeRefreshResp, error) {
	f.revoked = append(f.revoked, in.UserId+"/"+in.ClientId)
	return &api.RevokeRefreshResp{}, nil
}

var ctx = context.Background()

func TestAddCreatesBothLoginsForOneUser(t *testing.T) {
	f := newFake()
	m := &Manager{API: f}
	u, err := m.Add(ctx, "Alice", "Alice@OkCoin.jp", "correct-horse")
	if err != nil {
		t.Fatal(err)
	}
	a, b := f.pw["alice"], f.pw["alice@okcoin.jp"]
	if a == nil || b == nil {
		t.Fatalf("logins: %v", f.pw)
	}
	if a.UserId != u.UserID || a.UserId != b.UserId || a.Username != "Alice" || b.Username != "Alice" {
		t.Fatalf("identity must match across logins: %+v %+v", a, b)
	}
	if bcrypt.CompareHashAndPassword(a.Hash, []byte("correct-horse")) != nil {
		t.Fatal("stored hash does not match the password")
	}
	if string(a.Hash) == "correct-horse" {
		t.Fatal("plaintext stored")
	}
}

func TestAddRejectsBadInputAndDuplicates(t *testing.T) {
	m := &Manager{API: newFake()}
	if _, err := m.Add(ctx, "alice", "", "correct-horse"); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, user, email, pw string }{
		{"duplicate username", "ALICE", "", "correct-horse"},
		{"username with @", "a@b", "", "correct-horse"},
		{"username with space", "a b", "", "correct-horse"},
		{"empty username", "", "", "correct-horse"},
		{"short password", "bob", "", "short"},
		{"bad email", "bob", "not-an-email", "correct-horse"},
	}
	for _, c := range cases {
		if _, err := m.Add(ctx, c.user, c.email, c.pw); err == nil {
			t.Errorf("%s: expected error", c.name)
		}
	}
	// 邮箱已被别人占用
	if _, err := m.Add(ctx, "carol", "x@okcoin.jp", "correct-horse"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add(ctx, "dave", "X@okcoin.jp", "correct-horse"); err == nil {
		t.Error("email already in use should be rejected")
	}
}

func TestSetPasswordUpdatesAllLoginsAndRevokesRefresh(t *testing.T) {
	f := newFake()
	m := &Manager{API: f}
	u, _ := m.Add(ctx, "alice", "alice@okcoin.jp", "old-password")
	f.refresh[subject(u.UserID)] = []string{"headlamp"}

	if err := m.SetPassword(ctx, "ALICE", "new-password"); err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{"alice", "alice@okcoin.jp"} {
		if bcrypt.CompareHashAndPassword(f.pw[l].Hash, []byte("new-password")) != nil {
			t.Fatalf("%s still has old password", l)
		}
	}
	if len(f.revoked) != 1 || f.revoked[0] != subject(u.UserID)+"/headlamp" {
		t.Fatalf("refresh tokens not revoked: %v", f.revoked)
	}
	if err := m.SetPassword(ctx, "nobody", "new-password"); err == nil {
		t.Fatal("unknown user should fail")
	}
}

func TestDeleteRemovesAllLoginsAndRevokes(t *testing.T) {
	f := newFake()
	m := &Manager{API: f}
	u, _ := m.Add(ctx, "alice", "alice@okcoin.jp", "old-password")
	m.Add(ctx, "bob", "", "old-password")
	f.refresh[subject(u.UserID)] = []string{"headlamp"}

	if err := m.Delete(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if len(f.pw) != 1 || f.pw["bob"] == nil {
		t.Fatalf("wrong records left: %v", f.pw)
	}
	if len(f.revoked) != 1 {
		t.Fatalf("revoked %v", f.revoked)
	}
	if err := m.Delete(ctx, "alice"); err == nil {
		t.Fatal("deleting twice should report not found")
	}
}

func TestImportIsIdempotentAndValidates(t *testing.T) {
	f := newFake()
	m := &Manager{API: f}
	h1, _ := bcrypt.GenerateFromPassword([]byte("pw-one-1234"), bcrypt.MinCost)
	h2, _ := bcrypt.GenerateFromPassword([]byte("pw-two-1234"), bcrypt.MinCost)
	entries := []Entry{
		{Email: "alice", Hash: string(h1), Username: "alice", UserID: "u1"},
		{Email: "Alice@okcoin.jp", Hash: string(h1), Username: "alice", UserID: "u1"},
	}
	res, err := m.Import(ctx, entries)
	if err != nil || res.Created != 2 || res.Updated != 0 {
		t.Fatalf("first import: %+v %v", res, err)
	}
	entries[0].Hash = string(h2) // Zadig 里改过密码,再导入一次
	res, err = m.Import(ctx, entries)
	if err != nil || res.Created != 0 || res.Updated != 2 {
		t.Fatalf("second import: %+v %v", res, err)
	}
	if bcrypt.CompareHashAndPassword(f.pw["alice"].Hash, []byte("pw-two-1234")) != nil {
		t.Fatal("re-import must update the hash")
	}
	if _, err := m.Import(ctx, []Entry{{Email: "x", Hash: "plaintext", Username: "x", UserID: "u9"}}); err == nil {
		t.Fatal("non-bcrypt hash must be rejected")
	}
}

func TestUsersGroupsByUserID(t *testing.T) {
	m := &Manager{API: newFake()}
	m.Add(ctx, "bob", "bob@okcoin.jp", "old-password")
	m.Add(ctx, "alice", "", "old-password")
	us, _ := m.Users(ctx)
	if len(us) != 2 || us[0].Username != "alice" || len(us[1].Logins) != 2 {
		t.Fatalf("%+v", us)
	}
}

// Dex 把 sub 编成 base64url(IDTokenSubject{user_id, conn_id});这里固定一个已知值,防止编码被改坏。
func TestSubjectEncoding(t *testing.T) {
	got := subject("uid-alice")
	want := "Cgl1aWQtYWxpY2USBWxvY2Fs" // 0x0a 09 "uid-alice" 0x12 05 "local"
	if got != want {
		t.Fatalf("subject = %q, want %q", got, want)
	}
}
