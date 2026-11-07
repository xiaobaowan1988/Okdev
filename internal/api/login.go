package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Tokens 是 Dex 签发的令牌。
type Tokens struct {
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

// ErrBadCredentials 表示用户名或密码不对(或刷新令牌失效)。
var ErrBadCredentials = errors.New("invalid credentials")

// TokenSource 向 Dex 换令牌。命令行不直接访问 Dex 的令牌接口:Dex 对密码登录没有失败次数限制,
// 所以网关代理它,在这里做锁定。
type TokenSource interface {
	Password(ctx context.Context, username, password string) (*Tokens, error)
	Refresh(ctx context.Context, refreshToken string) (*Tokens, error)
}

// DexTokens 用 Dex 的令牌接口实现 TokenSource。
type DexTokens struct {
	ClientID string
	// URL 是 Dex 令牌接口的地址。留空就取 OIDC 发现文档里的 token_endpoint(要求网关能通过 issuer 地址访问到它);
	// Dex 对外只开放发现文档和公钥时,把它指到集群内的地址。
	URL      string
	Verifier *OIDCVerifier
}

func (d *DexTokens) endpoint(ctx context.Context) (string, error) {
	if d.URL != "" {
		return d.URL, nil
	}
	return d.Verifier.TokenEndpoint(ctx)
}

func (d *DexTokens) call(ctx context.Context, form url.Values) (*Tokens, error) {
	ep, err := d.endpoint(ctx)
	if err != nil {
		return nil, err
	}
	form.Set("client_id", d.ClientID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	hc := d.Verifier.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("identity provider: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var tr struct {
		Tokens
		Error     string `json:"error"`
		ErrorDesc string `json:"error_description"`
	}
	if err := json.Unmarshal(b, &tr); err != nil {
		return nil, fmt.Errorf("identity provider returned an unexpected response (%d)", resp.StatusCode)
	}
	switch {
	case tr.Error == "":
		if tr.IDToken == "" {
			return nil, errors.New("identity provider returned no id_token")
		}
		return &tr.Tokens, nil
	case tr.Error == "access_denied" || tr.Error == "invalid_grant" || (tr.Error == "invalid_request" && form.Get("grant_type") == "refresh_token"):
		return nil, ErrBadCredentials // Dex 对错误的用户名/密码返回 access_denied;失效的刷新令牌返回 invalid_request
	default:
		return nil, fmt.Errorf("identity provider: %s %s", tr.Error, tr.ErrorDesc)
	}
}

func (d *DexTokens) Password(ctx context.Context, username, password string) (*Tokens, error) {
	return d.call(ctx, url.Values{"grant_type": {"password"}, "username": {username}, "password": {password}, "scope": {"openid email profile offline_access"}})
}

func (d *DexTokens) Refresh(ctx context.Context, refreshToken string) (*Tokens, error) {
	return d.call(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}})
}

// clientIP 取来源 IP。只有配置了可信的请求头(例如反向代理设置的 X-Forwarded-For)才用它;
// 否则返回空,不按 IP 限制(直接用 RemoteAddr 在反向代理后面会把所有人当成同一个 IP)。
func clientIP(r *http.Request, header string) string {
	if header == "" {
		return ""
	}
	v := r.Header.Get(header)
	if i := strings.IndexByte(v, ','); i >= 0 { // X-Forwarded-For 取第一个:最靠近客户端的
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Username) == "" || in.Password == "" {
		writeErr(w, http.StatusBadRequest, "username and password are required")
		return
	}
	user := strings.ToLower(strings.TrimSpace(in.Username))
	keys := []string{"user:" + user}
	if ip := clientIP(r, s.ClientIPHeader); ip != "" {
		keys = append(keys, "ip:"+ip)
	}
	for _, k := range keys {
		if ok, wait := s.Throttle.Allow(k); !ok {
			secs := int(wait.Seconds()) + 1
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			s.Log.Warn("login locked", "user", user, "key", k, "retry_after_s", secs)
			writeErr(w, http.StatusTooManyRequests, fmt.Sprintf("too many failed attempts; try again in %d seconds", secs))
			return
		}
	}
	tok, err := s.Tokens.Password(r.Context(), in.Username, in.Password)
	switch {
	case errors.Is(err, ErrBadCredentials):
		for _, k := range keys {
			s.Throttle.Fail(k)
		}
		s.Log.Warn("login failed", "user", user)
		writeErr(w, http.StatusUnauthorized, "invalid username or password")
		return
	case err != nil:
		s.Log.Error("login: identity provider", "err", err)
		writeErr(w, http.StatusBadGateway, "identity provider unavailable")
		return
	}
	for _, k := range keys {
		s.Throttle.Reset(k)
	}
	writeJSON(w, http.StatusOK, tok)
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var in struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.RefreshToken == "" {
		writeErr(w, http.StatusBadRequest, "refresh_token is required")
		return
	}
	tok, err := s.Tokens.Refresh(r.Context(), in.RefreshToken)
	switch {
	case errors.Is(err, ErrBadCredentials):
		writeErr(w, http.StatusUnauthorized, "refresh token is invalid or has been revoked; log in again")
		return
	case err != nil:
		s.Log.Error("refresh: identity provider", "err", err)
		writeErr(w, http.StatusBadGateway, "identity provider unavailable")
		return
	}
	writeJSON(w, http.StatusOK, tok)
}
