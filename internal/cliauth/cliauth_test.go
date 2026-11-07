package cliauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func jwtWithExp(name string, exp time.Time) string {
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	return enc(map[string]string{"alg": "none"}) + "." + enc(map[string]any{"name": name, "exp": exp.Unix()}) + ".sig"
}

// fakeGateway 模拟网关的 /auth/login 和 /auth/refresh;刷新令牌像 Dex 一样每次轮换,旧的不能再用。
type fakeGateway struct {
	srv          *httptest.Server
	locked       bool // 模拟登录被锁定
	refreshCalls int32
	mu           sync.Mutex
	validRefresh string
	lastLogin    map[string]string
}

func newDex(t *testing.T) *fakeGateway {
	d := &fakeGateway{validRefresh: "refresh-1"}
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		d.mu.Lock()
		d.lastLogin = in
		d.mu.Unlock()
		if d.locked {
			w.Header().Set("Retry-After", "240")
			reply(w, 429, map[string]string{"error": "too many failed attempts"})
			return
		}
		u := strings.ToLower(in["username"])
		if (u != "alice" && u != "alice@okcoin.jp") || in["password"] != "alice-pass-1" {
			reply(w, 401, map[string]string{"error": "invalid username or password"})
			return
		}
		reply(w, 200, map[string]string{"id_token": jwtWithExp("alice", time.Now().Add(8*time.Hour)), "refresh_token": "refresh-1"})
	})
	mux.HandleFunc("/auth/refresh", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		atomic.AddInt32(&d.refreshCalls, 1)
		d.mu.Lock()
		defer d.mu.Unlock()
		if in["refresh_token"] != d.validRefresh {
			reply(w, 401, map[string]string{"error": "refresh token is invalid or has been revoked; log in again"})
			return
		}
		d.validRefresh = "refresh-" + time.Now().Format("150405.000000000")
		reply(w, 200, map[string]string{"id_token": jwtWithExp("alice", time.Now().Add(8*time.Hour)), "refresh_token": d.validRefresh})
	})
	d.srv = httptest.NewServer(mux)
	t.Cleanup(d.srv.Close)
	return d
}

func newClient() *Client { return &Client{HTTP: http.DefaultClient, Now: time.Now} }

func TestLoginWithUsernameOrEmailAndSavesPrivately(t *testing.T) {
	d := newDex(t)
	for _, login := range []string{"alice", "alice@okcoin.jp"} {
		cr, err := newClient().Login(context.Background(), d.srv.URL, "", login, "alice-pass-1")
		if err != nil {
			t.Fatalf("%s: %v", login, err)
		}
		if cr.RefreshToken != "refresh-1" || cr.API != d.srv.URL || time.Until(cr.Expiry) < 7*time.Hour {
			t.Fatalf("%+v", cr)
		}
	}
	cr, _ := newClient().Login(context.Background(), d.srv.URL, "", "alice", "alice-pass-1")
	path := filepath.Join(t.TempDir(), "sub", "credentials.json")
	if err := Save(path, cr); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("credentials must be private (they hold a refresh token): %v", st.Mode().Perm())
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "alice-pass-1") {
		t.Fatal("the password must never be written to disk")
	}
	got, err := Load(path)
	if err != nil || got.IDToken != cr.IDToken {
		t.Fatalf("round trip: %v", err)
	}
}

func TestLoginFailures(t *testing.T) {
	d := newDex(t)
	for name, tc := range map[string]struct{ user, pass, want string }{
		"wrong password": {"alice", "nope", "invalid username or password"},
		"unknown user":   {"mallory", "alice-pass-1", "invalid username or password"},
	} {
		if _, err := newClient().Login(context.Background(), d.srv.URL, "", tc.user, tc.pass); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	d.locked = true
	if _, err := newClient().Login(context.Background(), d.srv.URL, "", "alice", "alice-pass-1"); err == nil || !strings.Contains(err.Error(), "too many failed attempts") || !strings.Contains(err.Error(), "240") {
		t.Errorf("lockout should say so and how long: %v", err)
	}
	if _, err := newClient().Login(context.Background(), "http://127.0.0.1:1", "", "alice", "x"); err == nil {
		t.Error("unreachable gateway must fail")
	}
}

func saved(t *testing.T, d *fakeGateway, exp time.Time) string {
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := Save(path, &Credentials{API: d.srv.URL, IDToken: jwtWithExp("alice", exp), RefreshToken: "refresh-1", Expiry: exp}); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTokenValidIsReturnedWithoutNetwork(t *testing.T) {
	d := newDex(t)
	path := saved(t, d, time.Now().Add(2*time.Hour))
	c := newClient()
	if _, err := c.Token(context.Background(), path, time.Minute); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&d.refreshCalls) != 0 {
		t.Fatal("a valid token must not trigger a refresh")
	}
}

func TestTokenRefreshesWhenExpiredAndStoresRotatedRefreshToken(t *testing.T) {
	d := newDex(t)
	path := saved(t, d, time.Now().Add(20*time.Second)) // 不足 1 分钟,视为需要续期
	c := newClient()
	cr, err := c.Token(context.Background(), path, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(cr.Expiry) < time.Hour || cr.RefreshToken == "refresh-1" {
		t.Fatalf("not refreshed / refresh token not rotated: %+v", cr)
	}
	disk, _ := Load(path)
	if disk.RefreshToken != cr.RefreshToken {
		t.Fatal("the rotated refresh token must be saved, otherwise the next refresh fails")
	}
}

func TestConcurrentTokenCallsRefreshOnce(t *testing.T) {
	d := newDex(t)
	path := saved(t, d, time.Now().Add(-time.Minute))
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ { // kubectl 可能同时起很多个进程
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := newClient()
			_, err := c.Token(context.Background(), path, time.Minute)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a concurrent caller failed (refresh token rotation race): %v", err)
		}
	}
	if n := atomic.LoadInt32(&d.refreshCalls); n != 1 {
		t.Fatalf("want exactly 1 refresh, got %d", n)
	}
}

func TestTokenNeedsLogin(t *testing.T) {
	c := newClient()
	if _, err := c.Token(context.Background(), filepath.Join(t.TempDir(), "none.json"), time.Minute); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("not logged in: %v", err)
	}
	d := newDex(t)
	d.validRefresh = "something-else" // 刷新令牌被撤销(例如管理员改了密码或删了账号)
	path := saved(t, d, time.Now().Add(-time.Minute))
	if _, err := c.Token(context.Background(), path, time.Minute); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("revoked refresh token must ask for a new login: %v", err)
	}
}
