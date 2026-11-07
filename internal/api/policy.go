package api

import (
	"net/http"
	"regexp"
	"strings"
)

// Policy 决定谁能调哪些接口。默认所有登录用户都是开发者:能看全部、能发布到任何环境;
// 管理员(用户名在名单里)还能管理环境、服务、构建器、模板、流水线,以及下线部署。
type Policy struct {
	admins map[string]bool
}

func NewPolicy(admins []string) Policy {
	p := Policy{admins: map[string]bool{}}
	for _, a := range admins {
		if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
			p.admins[a] = true
		}
	}
	return p
}

func (p Policy) IsAdmin(username string) bool { return p.admins[strings.ToLower(username)] }

type rule struct {
	method string
	path   *regexp.Regexp
}

// 开发者允许的写操作:触发流水线(构建并发布)、取消运行、直接更新环境里的部署(镜像/副本数)、调副本数。
// 注意:这些都不按环境限制,任何登录用户都能发布到任何环境。
var developerWrites = []rule{
	{http.MethodPost, regexp.MustCompile(`^/v1/pipelines/[^/]+/runs$`)},
	{http.MethodPost, regexp.MustCompile(`^/v1/runs/\d+/cancel$`)},
	{http.MethodPut, regexp.MustCompile(`^/v1/envs/[^/]+/services/[^/]+$`)},
	{http.MethodPost, regexp.MustCompile(`^/v1/envs/[^/]+/services/[^/]+/scale$`)},
}

// Allow 判断一个请求是否允许。deployer 的接口全部在 /v1/ 下;/refs 是网关自己的只读接口。
func (p Policy) Allow(admin bool, method, path string) bool {
	switch {
	case method == http.MethodGet || method == http.MethodHead:
		return strings.HasPrefix(path, "/v1/") || path == "/refs"
	case !strings.HasPrefix(path, "/v1/"):
		return false
	case admin:
		return true
	}
	for _, r := range developerWrites {
		if r.method == method && r.path.MatchString(path) {
			return true
		}
	}
	return false
}
