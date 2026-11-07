package cliauth

import (
	"bytes"
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

// fakeDex 模拟 Dex 的发现文档、设备授权和令牌接口;刷新令牌像 Dex 一样每次轮换,旧的不能再用。
type fakeDex struct {
	srv          *httptest.Server
	pending      int32  // 还要返回几次 authorization_pending
	deviceResult string // "" 成功;或 access_denied / expired_token / slow_down
	refreshCalls int32
	mu           sync.Mutex
	validRefresh string
	slowSeen     int32
}

func newDex(t *testing.T) *fakeDex {
	d := &fakeDex{validRefresh: "refresh-1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"token_endpoint": d.srv.URL + "/token", "device_authorization_endpoint": d.srv.URL + "/device/code"})
	})
	mux.HandleFunc("/device/code", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("client_id") != "okdev" || !strings.Contains(r.Form.Get("scope"), "offline_access") {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"device_code": "dev-code", "user_code": "ABCD-EFGH", "verification_uri": d.srv.URL + "/device",
			"verification_uri_complete": d.srv.URL + "/device?user_code=ABCD-EFGH", "expires_in": 600, "interval": 1})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		switch r.Form.Get("grant_type") {
		case "urn:ietf:params:oauth:grant-type:device_code":
			if d.deviceResult == "slow_down" && atomic.AddInt32(&d.slowSeen, 1) == 1 {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"error": "slow_down"})
				return
			}
			if d.deviceResult != "" && d.deviceResult != "slow_down" {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"error": d.deviceResult})
				return
			}
			if atomic.AddInt32(&d.pending, -1) >= 0 {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"id_token": jwtWithExp("alice", time.Now().Add(8*time.Hour)), "refresh_token": "refresh-1"})
		case "refresh_token":
			atomic.AddInt32(&d.refreshCalls, 1)
			d.mu.Lock()
			defer d.mu.Unlock()
			if r.Form.Get("refresh_token") != d.validRefresh {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"error": "invalid_request", "error_description": "Refresh token is invalid or has already been claimed by another client."})
				return
			}
			d.validRefresh = "refresh-" + time.Now().Format("150405.000000000")
			json.NewEncoder(w).Encode(map[string]string{"id_token": jwtWithExp("alice", time.Now().Add(8*time.Hour)), "refresh_token": d.validRefresh})
		}
	})
	d.srv = httptest.NewServer(mux)
	t.Cleanup(d.srv.Close)
	return d
}

func newClient(out *bytes.Buffer) (*Client, *[]time.Duration) {
	var sleeps []time.Duration
	return &Client{HTTP: http.DefaultClient, Now: time.Now, Out: out, Sleep: func(d time.Duration) { sleeps = append(sleeps, d) }}, &sleeps
}

func TestLoginWaitsThroughPendingThenSaves(t *testing.T) {
	d := newDex(t)
	d.pending = 2
	var out bytes.Buffer
	c, sleeps := newClient(&out)
	cr, err := c.Login(context.Background(), d.srv.URL, "okdev", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(*sleeps) != 3 {
		t.Fatalf("expected 3 polls (2 pending + 1 success), slept %v", *sleeps)
	}
	if !strings.Contains(out.String(), "/device?user_code=ABCD-EFGH") || !strings.Contains(out.String(), "ABCD-EFGH") {
		t.Fatalf("user must be told where to go and which code: %q", out.String())
	}
	if cr.RefreshToken != "refresh-1" || time.Until(cr.Expiry) < 7*time.Hour {
		t.Fatalf("%+v", cr)
	}
	path := filepath.Join(t.TempDir(), "sub", "credentials.json")
	if err := Save(path, cr); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("credentials must be private (they hold a refresh token): %v", st.Mode().Perm())
	}
	got, err := Load(path)
	if err != nil || got.IDToken != cr.IDToken {
		t.Fatalf("round trip: %v", err)
	}
}

func TestLoginFailureModes(t *testing.T) {
	for result, want := range map[string]string{"access_denied": "denied", "expired_token": "expired"} {
		d := newDex(t)
		d.deviceResult = result
		c, _ := newClient(&bytes.Buffer{})
		if _, err := c.Login(context.Background(), d.srv.URL, "okdev", ""); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", result, err)
		}
	}
	// 客户端 ID 不对
	d := newDex(t)
	c, _ := newClient(&bytes.Buffer{})
	if _, err := c.Login(context.Background(), d.srv.URL, "wrong", ""); err == nil {
		t.Error("wrong client id must fail")
	}
}

func TestLoginSlowDownIncreasesInterval(t *testing.T) {
	d := newDex(t)
	d.deviceResult = "slow_down"
	c, sleeps := newClient(&bytes.Buffer{})
	if _, err := c.Login(context.Background(), d.srv.URL, "okdev", ""); err != nil {
		t.Fatal(err)
	}
	if len(*sleeps) < 2 || (*sleeps)[1] <= (*sleeps)[0] {
		t.Fatalf("poll interval must grow after slow_down: %v", *sleeps)
	}
}

func saved(t *testing.T, d *fakeDex, exp time.Time) string {
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := Save(path, &Credentials{Issuer: d.srv.URL, ClientID: "okdev", IDToken: jwtWithExp("alice", exp), RefreshToken: "refresh-1", Expiry: exp}); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTokenValidIsReturnedWithoutNetwork(t *testing.T) {
	d := newDex(t)
	path := saved(t, d, time.Now().Add(2*time.Hour))
	c, _ := newClient(&bytes.Buffer{})
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
	c, _ := newClient(&bytes.Buffer{})
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
			c, _ := newClient(&bytes.Buffer{})
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
	c, _ := newClient(&bytes.Buffer{})
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
