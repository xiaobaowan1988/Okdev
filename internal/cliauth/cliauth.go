// Package cliauth 是命令行登录:用 Dex 的设备授权流程(OAuth 2.0 Device Authorization Grant)拿到 ID Token 和刷新令牌,
// 保存在本地,需要时自动续期。密码只在浏览器里的 Dex 登录页输入,命令行和 AI 拿不到。
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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Credentials 是保存在本地的登录状态。
type Credentials struct {
	Issuer       string    `json:"issuer"`
	ClientID     string    `json:"client_id"`
	CAFile       string    `json:"ca_file,omitempty"`
	IDToken      string    `json:"id_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expiry"`
}

// ErrLoginRequired 表示没有可用的登录状态,或续期失败,需要重新 `okdev login`。
var ErrLoginRequired = errors.New("login required: run `okdev login`")

// Client 做设备授权登录和续期。
type Client struct {
	HTTP  *http.Client
	Sleep func(time.Duration) // 测试里替换,避免真的等待
	Now   func() time.Time
	Out   io.Writer // 提示信息(网址、验证码)输出到这里
}

func NewClient(caFile string, out io.Writer) (*Client, error) {
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
	return &Client{HTTP: hc, Sleep: time.Sleep, Now: time.Now, Out: out}, nil
}

type discovery struct {
	TokenEndpoint  string `json:"token_endpoint"`
	DeviceEndpoint string `json:"device_authorization_endpoint"`
}

type tokenResp struct {
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

func (c *Client) postForm(ctx context.Context, endpoint string, form url.Values, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(b, out); err != nil {
		return resp.StatusCode, fmt.Errorf("unexpected response (%d): %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return resp.StatusCode, nil
}

func (c *Client) discover(ctx context.Context, issuer string) (discovery, error) {
	var d discovery
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(issuer, "/")+"/.well-known/openid-configuration", nil)
	if err != nil {
		return d, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return d, fmt.Errorf("cannot reach the identity provider: %w", err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil || d.TokenEndpoint == "" {
		return d, fmt.Errorf("invalid discovery document from %s", issuer)
	}
	if d.DeviceEndpoint == "" {
		return d, errors.New("the identity provider does not support the device authorization flow")
	}
	return d, nil
}

// Login 走设备授权流程:打印网址和验证码,等用户在浏览器里完成登录,返回凭证。
func (c *Client) Login(ctx context.Context, issuer, clientID, caFile string) (*Credentials, error) {
	d, err := c.discover(ctx, issuer)
	if err != nil {
		return nil, err
	}
	var dev struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
		Error                   string `json:"error"`
		ErrorDesc               string `json:"error_description"`
	}
	form := url.Values{"client_id": {clientID}, "scope": {"openid email profile offline_access"}}
	if _, err := c.postForm(ctx, d.DeviceEndpoint, form, &dev); err != nil {
		return nil, err
	}
	if dev.Error != "" || dev.DeviceCode == "" {
		return nil, fmt.Errorf("device authorization failed: %s %s", dev.Error, dev.ErrorDesc)
	}
	link := dev.VerificationURIComplete
	if link == "" {
		link = dev.VerificationURI
	}
	fmt.Fprintf(c.Out, "Open this URL in a browser and log in with your Zadig account:\n\n  %s\n\n", link)
	if dev.VerificationURIComplete == "" || dev.UserCode != "" {
		fmt.Fprintf(c.Out, "If asked for a code, enter: %s\n\n", dev.UserCode)
	}
	fmt.Fprintln(c.Out, "Waiting for you to finish in the browser...")

	interval := time.Duration(dev.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	deadline := c.Now().Add(time.Duration(dev.ExpiresIn) * time.Second)
	if dev.ExpiresIn <= 0 {
		deadline = c.Now().Add(10 * time.Minute)
	}
	for {
		if c.Now().After(deadline) {
			return nil, errors.New("the login code expired; run `okdev login` again")
		}
		c.Sleep(interval)
		var tr tokenResp
		_, err := c.postForm(ctx, d.TokenEndpoint, url.Values{
			"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {dev.DeviceCode}, "client_id": {clientID},
		}, &tr)
		if err != nil {
			return nil, err
		}
		switch tr.Error {
		case "":
			return c.credsFrom(issuer, clientID, caFile, tr, "")
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
		case "access_denied":
			return nil, errors.New("login was denied in the browser")
		case "expired_token":
			return nil, errors.New("the login code expired; run `okdev login` again")
		default:
			return nil, fmt.Errorf("login failed: %s %s", tr.Error, tr.ErrorDesc)
		}
	}
}

func (c *Client) credsFrom(issuer, clientID, caFile string, tr tokenResp, oldRefresh string) (*Credentials, error) {
	exp, err := tokenExpiry(tr.IDToken)
	if err != nil {
		return nil, err
	}
	rt := tr.RefreshToken
	if rt == "" {
		rt = oldRefresh
	}
	return &Credentials{Issuer: issuer, ClientID: clientID, CAFile: caFile, IDToken: tr.IDToken, RefreshToken: rt, Expiry: exp}, nil
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
	d, err := c.discover(ctx, cr.Issuer)
	if err != nil {
		return nil, err
	}
	var tr tokenResp
	if _, err := c.postForm(ctx, d.TokenEndpoint, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {cr.RefreshToken}, "client_id": {cr.ClientID}}, &tr); err != nil {
		return nil, err
	}
	if tr.Error != "" {
		return nil, fmt.Errorf("%w (%s: %s)", ErrLoginRequired, tr.Error, tr.ErrorDesc)
	}
	return c.credsFrom(cr.Issuer, cr.ClientID, cr.CAFile, tr, cr.RefreshToken)
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
