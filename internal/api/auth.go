// Package api 是 okdev 的 API 网关:放在 deployer 前面,用 Zadig 账号(经 Dex 登录)访问。
//
//	客户端 ──Bearer <Dex ID Token>──> 网关(验证令牌、授权、记审计)──deployer 的令牌 + X-Actor=用户名──> deployer
//
// 同一个 ID Token 也可以直接访问 kube-apiserver(apiserver 信任同一个 Dex),所以 kubectl 和 API 用的是同一个登录。
package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Identity 是令牌里的用户。Username 就是 Zadig 账号名(Dex 的 name 声明),和 kube-apiserver 里 dex:<账号名> 的账号名是同一个。
type Identity struct {
	Username string
	Email    string
}

// Verifier 验证 Bearer 令牌。
type Verifier interface {
	Verify(ctx context.Context, rawToken string) (Identity, error)
}

// ErrUnauthorized 表示令牌无效(过期、签名不对、受众不对、缺少用户名)。
var ErrUnauthorized = errors.New("unauthorized")

// OIDCVerifier 用 Dex 的发现地址和公钥验证 ID Token。公钥轮换由 go-oidc 处理(遇到不认识的 kid 会重新拉取)。
// 创建时不要求 Dex 已经可达:第一次需要验证时才去发现,失败了下次再试,所以网关可以先于 Dex 启动。
type OIDCVerifier struct {
	Issuer   string
	Audience string
	HTTP     *http.Client // 可选,用于信任私有 CA

	mu       sync.Mutex
	verifier *oidc.IDTokenVerifier
	tokenURL string
}

func NewOIDCVerifier(issuer, audience, caFile string) (*OIDCVerifier, error) {
	v := &OIDCVerifier{Issuer: issuer, Audience: audience}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates in %s", caFile)
		}
		v.HTTP = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	}
	return v, nil
}

func (v *OIDCVerifier) get(ctx context.Context) (*oidc.IDTokenVerifier, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.verifier != nil {
		return v.verifier, nil
	}
	if v.HTTP != nil {
		ctx = oidc.ClientContext(ctx, v.HTTP)
	}
	p, err := oidc.NewProvider(ctx, v.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	v.verifier = p.Verifier(&oidc.Config{ClientID: v.Audience})
	v.tokenURL = p.Endpoint().TokenURL
	return v.verifier, nil
}

// TokenEndpoint 返回 OIDC 发现文档里的令牌接口地址。
func (v *OIDCVerifier) TokenEndpoint(ctx context.Context) (string, error) {
	if _, err := v.get(ctx); err != nil {
		return "", err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.tokenURL, nil
}

func (v *OIDCVerifier) Verify(ctx context.Context, raw string) (Identity, error) {
	ver, err := v.get(ctx)
	if err != nil {
		return Identity{}, err // 认证服务不可达:不是令牌的问题,调用方应返回 503 而不是 401
	}
	if v.HTTP != nil {
		ctx = oidc.ClientContext(ctx, v.HTTP)
	}
	tok, err := ver.Verify(ctx, raw)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	var c struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := tok.Claims(&c); err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	if strings.TrimSpace(c.Name) == "" {
		return Identity{}, fmt.Errorf("%w: token has no name claim", ErrUnauthorized)
	}
	return Identity{Username: c.Name, Email: c.Email}, nil
}

func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):]), true
	}
	return "", false
}
