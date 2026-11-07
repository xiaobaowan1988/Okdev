// okdev-api 是放在 deployer 前面的 API 网关:验证 Dex 签发的 ID Token(Zadig 账号登录),授权,再转发给 deployer。
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/xiaobaowan1988/okdev/internal/api"
	"github.com/xiaobaowan1988/okdev/internal/deployer"
	"github.com/xiaobaowan1988/okdev/internal/gitrefs"
)

func main() {
	var (
		listen       = flag.String("listen", ":8080", "监听地址")
		issuer       = flag.String("issuer-url", "", "Dex 的 issuer 地址(必填),必须与 kube-apiserver 的 --oidc-issuer-url 一致")
		audience     = flag.String("audience", "okdev", "令牌的受众(Dex 客户端 ID),必须与 kube-apiserver 的 --oidc-client-id 一致")
		issuerCA     = flag.String("issuer-ca-file", "", "信任 Dex 证书的 CA(Dex 用私有 CA 时)")
		deployerURL  = flag.String("deployer-url", "http://deployer.deployer.svc:8080", "deployer 的地址")
		deployerTok  = flag.String("deployer-token-file", "", "deployer 的 API_TOKEN 文件(deployer 开启鉴权时必填)")
		admins       = flag.String("admins", "", "管理员的 Zadig 账号名,逗号分隔;其余登录用户都是开发者(能发布到任何环境)")
		gitTokenFile = flag.String("git-token-file", "", "列分支访问私有 http(s) 仓库用的 token 文件(可选)")
		dexTokenURL  = flag.String("dex-token-url", "", "Dex 令牌接口的地址;留空取发现文档里的 token_endpoint。Dex 对外只开放发现文档和公钥时,指到集群内地址")
		maxFails     = flag.Int("login-max-failures", 5, "同一个用户名(或来源 IP)在窗口内登录失败这么多次就锁定")
		failWindow   = flag.Duration("login-failure-window", 15*time.Minute, "统计登录失败的时间窗口")
		lockFor      = flag.Duration("login-lock", 5*time.Minute, "锁定时长")
		clientIPHdr  = flag.String("client-ip-header", "", "可信反向代理设置的来源 IP 请求头(如 X-Forwarded-For);设置后还会按来源 IP 限制(防止对多个账号试密码)。不要在没有反向代理时设置,否则客户端可以伪造")
	)
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if *issuer == "" {
		log.Error("--issuer-url is required")
		os.Exit(2)
	}
	verifier, err := api.NewOIDCVerifier(*issuer, *audience, *issuerCA)
	if err != nil {
		log.Error("oidc verifier", "err", err)
		os.Exit(1)
	}
	du, err := url.Parse(*deployerURL)
	if err != nil || du.Host == "" {
		log.Error("invalid --deployer-url", "value", *deployerURL)
		os.Exit(2)
	}
	token := readTrim(log, *deployerTok)
	s := &api.Server{
		Verifier: verifier, Policy: api.NewPolicy(strings.Split(*admins, ",")),
		Deployer: du, DeployerToken: token,
		Builders: deployer.New(*deployerURL, token),
		Refs:     gitrefs.NewLister(readTrim(log, *gitTokenFile)),
		Log:      log,

		Tokens:         &api.DexTokens{ClientID: *audience, URL: *dexTokenURL, Verifier: verifier},
		Throttle:       api.NewThrottle(*maxFails, *failWindow, *lockFor),
		ClientIPHeader: *clientIPHdr,
	}
	srv := &http.Server{Addr: *listen, Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("listening", "addr", *listen, "issuer", *issuer, "audience", *audience, "deployer", *deployerURL)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}

// readTrim 读文件内容并去掉首尾空白;路径为空返回空串,配置了却读不到就退出。
func readTrim(log *slog.Logger, path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		log.Error("read file", "path", path, "err", err)
		os.Exit(1)
	}
	return strings.TrimSpace(string(b))
}
