// okdev 是命令行登录工具:用 Zadig 账号(经 Dex)登录一次,之后随时取令牌访问 API 或 kubectl。
//
//	okdev login --api https://api.example.com         用 Zadig 用户名和密码登录一次(密码交互输入,不回显)
//	okdev token                                       打印有效的 ID Token(快过期会自动续期),给 curl / AI 工具用
//	okdev token --exec-credential                     输出 kubectl 的 ExecCredential,用在 kubeconfig 的 exec 里
//	okdev whoami                                      显示当前登录的账号和令牌有效期
//	okdev logout                                      删除本地凭证
package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/xiaobaowan1988/okdev/internal/cliauth"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	api := fs.String("api", os.Getenv("OKDEV_API"), "login: API 网关地址(也可用环境变量 OKDEV_API)")
	caFile := fs.String("ca-file", os.Getenv("OKDEV_CA_FILE"), "login: 信任 API 网关证书的 CA(网关用私有 CA 时)")
	username := fs.String("username", os.Getenv("OKDEV_USERNAME"), "login: Zadig 用户名或邮箱;不给就交互输入")
	pwStdin := fs.Bool("password-stdin", false, "login: 从标准输入读一行作为密码(脚本里用;不要把密码写在命令行参数里)")
	execCred := fs.Bool("exec-credential", false, "token: 输出 kubectl 的 ExecCredential JSON")
	path := fs.String("credentials", cliauth.DefaultPath(), "凭证文件路径")
	_ = fs.Parse(args)
	ctx := context.Background()

	switch cmd {
	case "login":
		if *api == "" {
			fatal(fmt.Errorf("--api is required (the API gateway URL)"))
		}
		user, pass := credentialsFromUser(*username, *pwStdin)
		c, err := cliauth.NewClient(*caFile)
		check(err)
		cr, err := c.Login(ctx, *api, *caFile, user, pass)
		check(err)
		check(cliauth.Save(*path, cr))
		fmt.Fprintf(os.Stderr, "Logged in as %s (token valid until %s).\n", claim(cr.IDToken, "name"), cr.Expiry.Local().Format(time.RFC3339))
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
		fmt.Printf("user:    %s\nemail:   %s\napi:     %s\nexpires: %s\n", claim(cr.IDToken, "name"), claim(cr.IDToken, "email"), cr.API, cr.Expiry.Local().Format(time.RFC3339))
	case "logout":
		if err := os.Remove(*path); err != nil && !os.IsNotExist(err) {
			fatal(err)
		}
		fmt.Fprintln(os.Stderr, "logged out")
	default:
		usage()
	}
}

// credentialsFromUser 取登录用的用户名和密码:
// 密码来自 --password-stdin(脚本),或终端上不回显的交互输入;既不是终端也没给 --password-stdin 就报错,绝不从参数或环境变量读密码
// (命令行参数会出现在进程列表里,环境变量会被子进程继承)。
func credentialsFromUser(username string, pwStdin bool) (string, string) {
	interactive := term.IsTerminal(int(syscall.Stdin))
	in := bufio.NewReader(os.Stdin)
	if username == "" {
		if !interactive && !pwStdin {
			fatal(fmt.Errorf("--username is required when not running in a terminal"))
		}
		if interactive {
			fmt.Fprint(os.Stderr, "Username: ")
		}
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			fatal(fmt.Errorf("no username given"))
		}
		username = strings.TrimSpace(line)
	}
	var pass string
	switch {
	case pwStdin:
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			fatal(fmt.Errorf("no password on stdin"))
		}
		pass = strings.TrimRight(line, "\r\n")
	case interactive:
		fmt.Fprint(os.Stderr, "Password: ")
		b, err := term.ReadPassword(int(syscall.Stdin))
		fmt.Fprintln(os.Stderr)
		check(err)
		pass = string(b)
	default:
		fatal(fmt.Errorf("not a terminal: pass the password on stdin with --password-stdin"))
	}
	if username == "" || pass == "" {
		fatal(fmt.Errorf("username and password are required"))
	}
	return username, pass
}

func mustToken(ctx context.Context, path string) *cliauth.Credentials {
	cr, err := cliauth.Load(path)
	check(err)
	c, err := cliauth.NewClient(cr.CAFile)
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
	fmt.Fprintln(os.Stderr, "usage: okdev <login --api URL | token [--exec-credential] | whoami | logout>")
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
