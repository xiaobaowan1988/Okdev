// dex-users 管理 Dex 里的登录账号(通过 Dex 的 gRPC 管理接口,双向 TLS)。
//
//	dex-users list
//	dex-users add alice --email alice@okcoin.jp          # 密码交互输入;脚本里用 --password-stdin
//	dex-users passwd alice
//	dex-users delete alice
//	dex-users import static-passwords.yaml                # zadig-export 的输出,可重复执行
//
// 连接参数:--addr/--ca/--cert/--key/--server-name,或环境变量 DEX_GRPC_ADDR、DEX_GRPC_CA、DEX_GRPC_CERT、DEX_GRPC_KEY。
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/dexidp/dex/api/v2"
	"golang.org/x/term"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"sigs.k8s.io/yaml"

	"github.com/xiaobaowan1988/okdev/internal/dexusers"
)

type adapter struct{ c api.DexClient }

func (a adapter) CreatePassword(ctx context.Context, in *api.CreatePasswordReq) (*api.CreatePasswordResp, error) {
	return a.c.CreatePassword(ctx, in)
}
func (a adapter) UpdatePassword(ctx context.Context, in *api.UpdatePasswordReq) (*api.UpdatePasswordResp, error) {
	return a.c.UpdatePassword(ctx, in)
}
func (a adapter) DeletePassword(ctx context.Context, in *api.DeletePasswordReq) (*api.DeletePasswordResp, error) {
	return a.c.DeletePassword(ctx, in)
}
func (a adapter) ListPasswords(ctx context.Context, in *api.ListPasswordReq) (*api.ListPasswordResp, error) {
	return a.c.ListPasswords(ctx, in)
}
func (a adapter) ListRefresh(ctx context.Context, in *api.ListRefreshReq) (*api.ListRefreshResp, error) {
	return a.c.ListRefresh(ctx, in)
}
func (a adapter) RevokeRefresh(ctx context.Context, in *api.RevokeRefreshReq) (*api.RevokeRefreshResp, error) {
	return a.c.RevokeRefresh(ctx, in)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	fs := flag.NewFlagSet("dex-users", flag.ExitOnError)
	addr := fs.String("addr", env("DEX_GRPC_ADDR", "localhost:5557"), "Dex gRPC 地址")
	ca := fs.String("ca", env("DEX_GRPC_CA", ""), "校验 Dex gRPC 服务端证书的 CA")
	cert := fs.String("cert", env("DEX_GRPC_CERT", ""), "管理员客户端证书")
	key := fs.String("key", env("DEX_GRPC_KEY", ""), "管理员客户端私钥")
	serverName := fs.String("server-name", env("DEX_GRPC_SERVER_NAME", "dex"), "服务端证书里的名字(通过 port-forward 连接时需要)")
	email := fs.String("email", "", "add:邮箱(可选,提供后也可用邮箱登录)")
	pwStdin := fs.Bool("password-stdin", false, "从标准输入读密码,而不是交互提示")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: dex-users [flags] <list | add <username> | passwd <username> | delete <username> | import <file>>")
		fs.PrintDefaults()
	}
	// 允许 flag 出现在子命令之前或之后
	var pos []string
	args := os.Args[1:]
	for len(args) > 0 {
		_ = fs.Parse(args)
		args = fs.Args()
		if len(args) > 0 {
			pos = append(pos, args[0])
			args = args[1:]
		}
	}
	if len(pos) == 0 {
		fs.Usage()
		os.Exit(2)
	}
	if *ca == "" || *cert == "" || *key == "" {
		fatal(errors.New("--ca, --cert and --key are required (or DEX_GRPC_CA/CERT/KEY)"))
	}

	conn, err := dial(*addr, *ca, *cert, *key, *serverName)
	if err != nil {
		fatal(err)
	}
	defer conn.Close()
	m := &dexusers.Manager{API: adapter{api.NewDexClient(conn)}}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	switch cmd := pos[0]; cmd {
	case "list":
		users, err := m.Users(ctx)
		check(err)
		fmt.Printf("%-20s %-45s %s\n", "USERNAME", "LOGIN NAMES", "USER ID")
		for _, u := range users {
			fmt.Printf("%-20s %-45s %s\n", u.Username, strings.Join(u.Logins, ", "), u.UserID)
		}
		fmt.Printf("%d user(s)\n", len(users))
	case "add":
		need(pos, 2, "add <username>")
		pw := readPassword(*pwStdin, true)
		u, err := m.Add(ctx, pos[1], *email, pw)
		check(err)
		fmt.Printf("added %s (login names: %s)\n", u.Username, strings.Join(u.Logins, ", "))
	case "passwd":
		need(pos, 2, "passwd <username>")
		check(m.SetPassword(ctx, pos[1], readPassword(*pwStdin, true)))
		fmt.Printf("password updated for %s; refresh tokens revoked\n", pos[1])
	case "delete":
		need(pos, 2, "delete <username>")
		check(m.Delete(ctx, pos[1]))
		fmt.Printf("deleted %s; refresh tokens revoked (already issued ID tokens stay valid until they expire)\n", pos[1])
	case "import":
		need(pos, 2, "import <file>")
		entries, err := readEntries(pos[1])
		check(err)
		res, err := m.Import(ctx, entries)
		check(err)
		fmt.Printf("imported %d login names: %d created, %d updated\n", len(entries), res.Created, res.Updated)
	default:
		fs.Usage()
		os.Exit(2)
	}
}

func dial(addr, caFile, certFile, keyFile, serverName string) (*grpc.ClientConn, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load client cert: %w", err)
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("no certificates in --ca")
	}
	creds := credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{pair}, RootCAs: pool, ServerName: serverName, MinVersion: tls.VersionTLS12})
	return grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
}

func readEntries(path string) ([]dexusers.Entry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		StaticPasswords []struct {
			Email    string `json:"email"`
			Hash     string `json:"hash"`
			Username string `json:"username"`
			UserID   string `json:"userID"`
		} `json:"staticPasswords"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	var out []dexusers.Entry
	for _, p := range doc.StaticPasswords {
		out = append(out, dexusers.Entry{Email: p.Email, Hash: p.Hash, Username: p.Username, UserID: p.UserID})
	}
	return out, nil
}

func readPassword(fromStdin, confirm bool) string {
	if fromStdin {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && err != io.EOF {
			fatal(err)
		}
		return strings.TrimRight(line, "\r\n")
	}
	fmt.Fprint(os.Stderr, "Password: ")
	p1, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Fprintln(os.Stderr)
	check(err)
	if confirm {
		fmt.Fprint(os.Stderr, "Repeat password: ")
		p2, err := term.ReadPassword(int(syscall.Stdin))
		fmt.Fprintln(os.Stderr)
		check(err)
		if string(p1) != string(p2) {
			fatal(errors.New("passwords do not match"))
		}
	}
	return string(p1)
}

func need(pos []string, n int, usage string) {
	if len(pos) < n {
		fatal(fmt.Errorf("usage: dex-users %s", usage))
	}
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
