// api:演示用的小服务。返回版本号和 Pod 名,用来直观看出每个环境跑的是哪个镜像版本。
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
)

// 构建时用 -ldflags "-X main.version=v1" 注入;同一份代码构建一次,在各环境用不同 tag 部署。
var version = "dev"

func main() {
	host, _ := os.Hostname()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"service": "api", "version": version, "pod": host})
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	log.Printf("api %s listening on :8080", version)
	log.Fatal(http.ListenAndServe(":8080", mux))
}
