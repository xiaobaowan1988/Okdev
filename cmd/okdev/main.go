// okdev 是命令行登录工具:用 Zadig 账号(经 Dex)登录一次,之后随时取令牌访问 API 或 kubectl。
//
//	okdev login --issuer https://dex.example.com      浏览器里登录一次(设备授权流程)
//	okdev token                                       打印有效的 ID Token(快过期会自动续期),给 curl / AI 工具用
//	okdev token --exec-credential                     输出 kubectl 的 ExecCredential,用在 kubeconfig 的 exec 里
//	okdev whoami                                      显示当前登录的账号和令牌有效期
//	okdev logout                                      删除本地凭证
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/xiaobaowan1988/okdev/internal/cliauth"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	issuer := fs.String("issuer", os.Getenv("OKDEV_ISSUER"), "login: Dex 的 issuer 地址(也可用环境变量 OKDEV_ISSUER)")
	clientID := fs.String("client-id", envOr("OKDEV_CLIENT_ID", "okdev"), "login: Dex 客户端 ID(必须是允许设备授权的公开客户端)")
	caFile := fs.String("ca-file", os.Getenv("OKDEV_CA_FILE"), "login: 信任 Dex 证书的 CA(Dex 用私有 CA 时)")
	execCred := fs.Bool("exec-credential", false, "token: 输出 kubectl 的 ExecCredential JSON")
	path := fs.String("credentials", cliauth.DefaultPath(), "凭证文件路径")
	_ = fs.Parse(args)
	ctx := context.Background()

	switch cmd {
	case "login":
		if *issuer == "" {
			fatal(fmt.Errorf("--issuer is required"))
		}
		c, err := cliauth.NewClient(*caFile, os.Stderr)
		check(err)
		cr, err := c.Login(ctx, *issuer, *clientID, *caFile)
		check(err)
		check(cliauth.Save(*path, cr))
		fmt.Fprintf(os.Stderr, "\nLogged in as %s (token valid until %s).\n", claim(cr.IDToken, "name"), cr.Expiry.Local().Format(time.RFC3339))
	case "token":
		cr := mustToken(ctx, *path)
		if *execCred {
			out := map[string]any{
				"apiVersion": "client.authentication.k8s.io/v1", "kind": "ExecCredential",
				"status": map[string]any{"token": cr.IDToken, "expirationTimestamp": cr.Expiry.UTC().Format(time.RFC3339)},
			}
			check(json.NewEncoder(os.Stdout).Encode(out))
			return
		}
		fmt.Println(cr.IDToken)
	case "whoami":
		cr, err := cliauth.Load(*path)
		check(err)
		fmt.Printf("user:    %s\nemail:   %s\nissuer:  %s\nexpires: %s\n", claim(cr.IDToken, "name"), claim(cr.IDToken, "email"), cr.Issuer, cr.Expiry.Local().Format(time.RFC3339))
	case "logout":
		if err := os.Remove(*path); err != nil && !os.IsNotExist(err) {
			fatal(err)
		}
		fmt.Fprintln(os.Stderr, "logged out")
	default:
		usage()
	}
}

func mustToken(ctx context.Context, path string) *cliauth.Credentials {
	cr, err := cliauth.Load(path)
	check(err)
	c, err := cliauth.NewClient(cr.CAFile, os.Stderr)
	check(err)
	// 剩余不足 1 分钟就续期,避免拿到手的令牌马上过期
	cr, err = c.Token(ctx, path, time.Minute)
	check(err)
	return cr
}

// claim 读 ID Token 里的一个字符串声明,仅用于显示。
func claim(jwt, name string) string {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return "?"
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "?"
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return "?"
	}
	s, _ := m[name].(string)
	return s
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: okdev <login --issuer URL | token [--exec-credential] | whoami | logout>")
	os.Exit(2)
}
func check(err error) {
	if err != nil {
		fatal(err)
	}
}
func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
