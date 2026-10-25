// 测试辅助:按 Zadig 的方式(bcrypt.DefaultCost)生成密码哈希。
package main

import (
	"fmt"
	"os"

	"golang.org/x/crypto/bcrypt"
)

func main() {
	h, err := bcrypt.GenerateFromPassword([]byte(os.Args[1]), bcrypt.DefaultCost)
	if err != nil {
		panic(err)
	}
	fmt.Print(string(h))
}
