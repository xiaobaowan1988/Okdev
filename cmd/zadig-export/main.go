// zadig-export 把 Zadig 的本地账号导出成 Dex 的 staticPasswords(YAML)。只读 MySQL。
//
//	zadig-export -dsn 'user:pass@tcp(host:3306)/zadig_user' > static-passwords.yaml
//
// 输出含密码哈希,按密钥对待(放进 K8s Secret),不要提交到仓库。跳过的账号和原因写到 stderr。
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/xiaobaowan1988/okdev/internal/zadigexport"
)

func main() {
	dsn := flag.String("dsn", os.Getenv("ZADIG_MYSQL_DSN"), "Zadig 用户库的 DSN(也可用环境变量 ZADIG_MYSQL_DSN)")
	identity := flag.String("identity-type", "system", "导出的账号来源;Zadig 本地账号是 system,第三方登录的账号没有本地密码")
	flag.Parse()
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "-dsn is required")
		os.Exit(2)
	}
	db, err := sql.Open("mysql", *dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	accounts, err := zadigexport.Query(ctx, db, *identity)
	if err != nil {
		fmt.Fprintln(os.Stderr, "query:", err)
		os.Exit(1)
	}
	exported, skipped := zadigexport.Convert(accounts)
	out, err := zadigexport.Render(exported)
	if err != nil {
		fmt.Fprintln(os.Stderr, "render:", err)
		os.Exit(1)
	}
	os.Stdout.Write(out)
	logins := map[string]bool{}
	for _, e := range exported {
		logins[e.UserID] = true
	}
	fmt.Fprintf(os.Stderr, "exported %d login names for %d of %d accounts\n", len(exported), len(logins), len(accounts))
	for _, s := range skipped {
		fmt.Fprintf(os.Stderr, "note %s: %s\n", s.Account, s.Reason)
	}
}
