package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestThrottleLocksAfterMaxFailuresAndExpires(t *testing.T) {
	now := time.Now()
	th := NewThrottle(3, 15*time.Minute, 5*time.Minute)
	th.Now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		th.Fail("user:a")
		if ok, _ := th.Allow("user:a"); !ok {
			t.Fatalf("locked too early after %d failures", i+1)
		}
	}
	th.Fail("user:a")
	if ok, wait := th.Allow("user:a"); ok || wait <= 0 || wait > 5*time.Minute {
		t.Fatalf("must be locked after 3 failures (ok=%v wait=%v)", ok, wait)
	}
	if ok, _ := th.Allow("user:b"); !ok {
		t.Fatal("another key must be unaffected")
	}
	now = now.Add(5*time.Minute + time.Second)
	if ok, _ := th.Allow("user:a"); !ok {
		t.Fatal("lock must expire")
	}
	// 锁定结束后重新计数:再失败两次不会立刻再锁
	th.Fail("user:a")
	th.Fail("user:a")
	if ok, _ := th.Allow("user:a"); !ok {
		t.Fatal("counter must restart after a lock")
	}
}

func TestThrottleWindowAndReset(t *testing.T) {
	now := time.Now()
	th := NewThrottle(3, time.Minute, time.Minute)
	th.Now = func() time.Time { return now }
	th.Fail("k")
	th.Fail("k")
	now = now.Add(2 * time.Minute) // 窗口过去了,之前的失败不再算
	th.Fail("k")
	if ok, _ := th.Allow("k"); !ok {
		t.Fatal("failures outside the window must not count")
	}
	th.Fail("k")
	th.Reset("k")
	th.Fail("k")
	th.Fail("k")
	if ok, _ := th.Allow("k"); !ok {
		t.Fatal("a successful login must reset the counter")
	}
}

// fakeTokens 模拟 Dex 的令牌接口。
type fakeTokens struct {
	calls   int32
	refresh int32
}

func (f *fakeTokens) Password(_ context.Context, u, p string) (*Tokens, error) {
	atomic.AddInt32(&f.calls, 1)
	if strings.ToLower(u) == "alice" && p == "right" {
		return &Tokens{IDToken: "id-token", RefreshToken: "refresh-1"}, nil
	}
	if u == "boom" {
		return nil, io.ErrUnexpectedEOF
	}
	return nil, ErrBadCredentials
}
func (f *fakeTokens) Refresh(_ context.Context, rt string) (*Tokens, error) {
	atomic.AddInt32(&f.refresh, 1)
	if rt != "refresh-1" {
		return nil, ErrBadCredentials
	}
	return &Tokens{IDToken: "id-token-2", RefreshToken: "refresh-2"}, nil
}

type loginEnv struct {
	gw  *httptest.Server
	tok *fakeTokens
	now *time.Time
}

func newLoginEnv(t *testing.T, ipHeader string) *loginEnv {
	t.Helper()
	now := time.Now()
	th := NewThrottle(5, 15*time.Minute, 5*time.Minute)
	th.Now = func() time.Time { return now }
	e := &loginEnv{tok: &fakeTokens{}, now: &now}
	du, _ := url.Parse("http://127.0.0.1:1")
	s := &Server{Verifier: nil, Policy: NewPolicy(nil), Deployer: du, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Tokens: e.tok, Throttle: th, ClientIPHeader: ipHeader}
	e.gw = httptest.NewServer(s.Handler())
	t.Cleanup(e.gw.Close)
	return e
}

func (e *loginEnv) login(t *testing.T, user, pass string, hdr ...string) (int, string, http.Header) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": user, "password": pass})
	req, _ := http.NewRequest("POST", e.gw.URL+"/auth/login", strings.NewReader(string(body)))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func TestLoginSuccessAndFailure(t *testing.T) {
	e := newLoginEnv(t, "")
	code, body, _ := e.login(t, "Alice", "right")
	if code != 200 || !strings.Contains(body, `"id_token":"id-token"`) || !strings.Contains(body, `"refresh_token":"refresh-1"`) {
		t.Fatalf("%d %s", code, body)
	}
	if code, body, _ := e.login(t, "alice", "wrong"); code != 401 || strings.Contains(body, "right") {
		t.Fatalf("%d %s", code, body)
	}
	if code, _, _ := e.login(t, "boom", "x"); code != 502 {
		t.Fatalf("identity provider failure must be 502, got %d", code)
	}
	for _, bad := range []string{`{}`, `{"username":"a"}`, `not json`} {
		resp, _ := http.Post(e.gw.URL+"/auth/login", "application/json", strings.NewReader(bad))
		if resp.StatusCode != 400 {
			t.Fatalf("%q: %d", bad, resp.StatusCode)
		}
	}
	resp, _ := http.Get(e.gw.URL + "/auth/login")
	if resp.StatusCode != 405 {
		t.Fatalf("GET must be 405, got %d", resp.StatusCode)
	}
}

func TestBruteForceIsLockedOutAndDexIsNotContactedAnymore(t *testing.T) {
	e := newLoginEnv(t, "")
	for i := 0; i < 5; i++ {
		if code, _, _ := e.login(t, "alice", "guess"); code != 401 {
			t.Fatalf("attempt %d: %d", i, code)
		}
	}
	before := atomic.LoadInt32(&e.tok.calls)
	code, body, hdr := e.login(t, "alice", "right") // 即使这次密码对了,锁定期内也不放行
	if code != 429 || hdr.Get("Retry-After") == "" {
		t.Fatalf("expected 429 with Retry-After, got %d %s", code, body)
	}
	if atomic.LoadInt32(&e.tok.calls) != before {
		t.Fatal("while locked, the identity provider must not be contacted (the guess must not even be evaluated)")
	}
	if code, _, _ := e.login(t, "ALICE ", "right"); code != 429 {
		t.Fatalf("case/space variants of the username must hit the same lock, got %d", code)
	}
	if code, _, _ := e.login(t, "bob", "guess"); code != 401 {
		t.Fatalf("another user is not locked, got %d", code)
	}
	*e.now = e.now.Add(5*time.Minute + time.Second)
	if code, _, _ := e.login(t, "alice", "right"); code != 200 {
		t.Fatalf("after the lock expires the right password works, got %d", code)
	}
}

func TestSuccessfulLoginResetsTheCounter(t *testing.T) {
	e := newLoginEnv(t, "")
	for round := 0; round < 3; round++ { // 4 次失败 + 1 次成功,重复 3 轮:永远到不了 5 次连续失败
		for i := 0; i < 4; i++ {
			e.login(t, "alice", "guess")
		}
		if code, _, _ := e.login(t, "alice", "right"); code != 200 {
			t.Fatalf("round %d: %d", round, code)
		}
	}
}

func TestPasswordSprayingIsLockedByIPOnlyWhenHeaderIsTrusted(t *testing.T) {
	e := newLoginEnv(t, "X-Forwarded-For")
	for i := 0; i < 5; i++ { // 同一个 IP 对 5 个不同账号各试一次
		e.login(t, "user"+string(rune('a'+i)), "guess", "X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	}
	if code, _, _ := e.login(t, "alice", "right", "X-Forwarded-For", "203.0.113.9"); code != 429 {
		t.Fatalf("the spraying IP must be locked, got %d", code)
	}
	if code, _, _ := e.login(t, "alice", "right", "X-Forwarded-For", "198.51.100.7"); code != 200 {
		t.Fatalf("other IPs are unaffected, got %d", code)
	}

	// 没配可信请求头:客户端自己带的 X-Forwarded-For 不能用来绕过或伪造
	e2 := newLoginEnv(t, "")
	for i := 0; i < 5; i++ {
		e2.login(t, "alice", "guess", "X-Forwarded-For", "1.1.1."+string(rune('0'+i)))
	}
	if code, _, _ := e2.login(t, "alice", "right", "X-Forwarded-For", "9.9.9.9"); code != 429 {
		t.Fatalf("rotating X-Forwarded-For must not evade the per-user lock, got %d", code)
	}
}

func TestRefreshProxy(t *testing.T) {
	e := newLoginEnv(t, "")
	post := func(body string) (int, string) {
		resp, err := http.Post(e.gw.URL+"/auth/refresh", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := post(`{"refresh_token":"refresh-1"}`); code != 200 || !strings.Contains(body, "refresh-2") {
		t.Fatalf("%d %s", code, body)
	}
	if code, _ := post(`{"refresh_token":"revoked"}`); code != 401 {
		t.Fatalf("revoked refresh token: %d", code)
	}
	if code, _ := post(`{}`); code != 400 {
		t.Fatalf("missing token: %d", code)
	}
}
