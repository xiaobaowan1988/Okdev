// Package cliauth 是命令行登录:用 Zadig 账号的用户名和密码向 API 网关换 ID Token 和刷新令牌(网关代理 Dex 并限制失败次数),
// 保存在本地,需要时自动续期。密码只在登录那一刻发给网关,不保存;之后一直用令牌和刷新令牌。
package cliauth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Credentials 是保存在本地的登录状态。
type Credentials struct {
	API          string    `json:"api"` // API 网关地址;登录和续期都走它
	CAFile       string    `json:"ca_file,omitempty"`
	IDToken      string    `json:"id_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expiry"`
}

// ErrLoginRequired 表示没有可用的登录状态,或续期失败,需要重新 `okdev login`。
var ErrLoginRequired = errors.New("login required: run `okdev login`")

// Client 做密码登录和续期。
type Client struct {
	HTTP *http.Client
	Now  func() time.Time
}

func NewClient(caFile string) (*Client, error) {
	hc := &http.Client{Timeout: 30 * time.Second}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		pool, _ := x509.SystemCertPool()
		if pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates in %s", caFile)
		}
		hc.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	}
	return &Client{HTTP: hc, Now: time.Now}, nil
}

type tokenResp struct {
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	Error        string `json:"error"`
}

// post 向网关发 JSON,返回状态码、响应里的令牌和错误信息、以及 Retry-After。
func (c *Client) post(ctx context.Context, api, path string, body any) (int, tokenResp, string, error) {
	var tr tokenResp
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(api, "/")+path, strings.NewReader(string(b)))
	if err != nil {
		return 0, tr, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, tr, "", fmt.Errorf("cannot reach the API gateway: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(data, &tr); err != nil {
		return resp.StatusCode, tr, "", fmt.Errorf("unexpected response from the API gateway (%d): %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return resp.StatusCode, tr, resp.Header.Get("Retry-After"), nil
}

// Login 用用户名和密码登录。username 可以是 Zadig 用户名,也可以是邮箱。
func (c *Client) Login(ctx context.Context, api, caFile, username, password string) (*Credentials, error) {
	status, tr, retry, err := c.post(ctx, api, "/auth/login", map[string]string{"username": username, "password": password})
	if err != nil {
		return nil, err
	}
	switch {
	case status == http.StatusUnauthorized:
		return nil, errors.New("invalid username or password")
	case status == http.StatusTooManyRequests:
		return nil, fmt.Errorf("too many failed attempts; try again in %s seconds", orDefault(retry, "a few minutes"))
	case status != http.StatusOK:
		return nil, fmt.Errorf("login failed (%d): %s", status, tr.Error)
	}
	return c.credsFrom(api, caFile, tr, "")
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (c *Client) credsFrom(api, caFile string, tr tokenResp, oldRefresh string) (*Credentials, error) {
	exp, err := tokenExpiry(tr.IDToken)
	if err != nil {
		return nil, err
	}
	rt := tr.RefreshToken
	if rt == "" {
		rt = oldRefresh
	}
	return &Credentials{API: api, CAFile: caFile, IDToken: tr.IDToken, RefreshToken: rt, Expiry: exp}, nil
}

// tokenExpiry 读 JWT 里的 exp。只是用来决定何时续期,不做验证(验证是服务端的事)。
func tokenExpiry(jwt string) (time.Time, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return time.Time{}, errors.New("identity provider returned a malformed id_token")
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, err
	}
	var c struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(b, &c); err != nil || c.Exp == 0 {
		return time.Time{}, errors.New("id_token has no exp claim")
	}
	return time.Unix(c.Exp, 0), nil
}

// Refresh 用刷新令牌换新的 ID Token。Dex 每次刷新会轮换刷新令牌,所以必须把新的存回去。
func (c *Client) Refresh(ctx context.Context, cr *Credentials) (*Credentials, error) {
	if cr.RefreshToken == "" {
		return nil, ErrLoginRequired
	}
	status, tr, _, err := c.post(ctx, cr.API, "/auth/refresh", map[string]string{"refresh_token": cr.RefreshToken})
	if err != nil {
		return nil, err
	}
	switch {
	case status == http.StatusUnauthorized:
		return nil, fmt.Errorf("%w (the refresh token was revoked or expired)", ErrLoginRequired)
	case status != http.StatusOK:
		return nil, fmt.Errorf("token refresh failed (%d): %s", status, tr.Error)
	}
	return c.credsFrom(cr.API, cr.CAFile, tr, cr.RefreshToken)
}

// DefaultPath 是凭证文件的默认位置。
func DefaultPath() string {
	if p := os.Getenv("OKDEV_CREDENTIALS"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "okdev", "credentials.json")
}

func Load(path string) (*Credentials, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrLoginRequired
	}
	if err != nil {
		return nil, err
	}
	var cr Credentials
	if err := json.Unmarshal(b, &cr); err != nil {
		return nil, fmt.Errorf("corrupt credentials file %s: %w", path, err)
	}
	return &cr, nil
}

// Save 原子地写入,权限 0600(里面有刷新令牌)。
func Save(path string, cr *Credentials) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(cr, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Token 返回一个有效的 ID Token;快过期(不足 minValid)就先续期。
// 用文件锁串行化续期:kubectl 可能同时起好几个进程,Dex 会轮换刷新令牌,并发续期会让后到的失败。
func (c *Client) Token(ctx context.Context, path string, minValid time.Duration) (*Credentials, error) {
	cr, err := Load(path)
	if err != nil {
		return nil, err
	}
	if c.Now().Add(minValid).Before(cr.Expiry) {
		return cr, nil
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return nil, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	// 拿到锁后重读:可能别的进程刚刚续期过
	if cr, err = Load(path); err != nil {
		return nil, err
	}
	if c.Now().Add(minValid).Before(cr.Expiry) {
		return cr, nil
	}
	next, err := c.Refresh(ctx, cr)
	if err != nil {
		return nil, err
	}
	if err := Save(path, next); err != nil {
		return nil, err
	}
	return next, nil
}
