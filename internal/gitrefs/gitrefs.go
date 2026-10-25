// Package gitrefs 列出远程 Git 仓库的分支和 Tag(等价于 git ls-remote),
// 给前端的分支/Tag 下拉选择用。仓库地址只来自 deployer 里登记的构建器,调用方不能指定任意 URL。
package gitrefs

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// Refs 是一个仓库当前的分支和 Tag。
type Refs struct {
	Branches []string `json:"branches"`
	Tags     []string `json:"tags"`
	// Default 是仓库的默认分支,取不到则为空。
	Default string `json:"default,omitempty"`
}

// Lister 带缓存地列出远程引用。
type Lister struct {
	// Token 非空时,以 HTTP Basic(x-access-token)访问 http(s) 仓库。
	Token string
	TTL   time.Duration

	list  func(ctx context.Context, repo, token string) ([]*plumbing.Reference, string, error)
	now   func() time.Time
	mu    sync.Mutex
	cache map[string]entry
}

type entry struct {
	refs Refs
	at   time.Time
}

func NewLister(token string) *Lister {
	return &Lister{Token: token, TTL: 30 * time.Second, list: lsRemote, now: time.Now, cache: map[string]entry{}}
}

// List 返回仓库的引用,同一仓库在 TTL 内走缓存。
func (l *Lister) List(ctx context.Context, repo string) (Refs, error) {
	l.mu.Lock()
	if e, ok := l.cache[repo]; ok && l.now().Sub(e.at) < l.TTL {
		l.mu.Unlock()
		return e.refs, nil
	}
	l.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	refs, head, err := l.list(ctx, repo, l.Token)
	if err != nil {
		return Refs{}, err
	}
	out := Refs{Branches: []string{}, Tags: []string{}}
	for _, r := range refs {
		n := r.Name()
		switch {
		case n.IsBranch():
			out.Branches = append(out.Branches, n.Short())
		case n.IsTag():
			out.Tags = append(out.Tags, n.Short())
		}
	}
	sort.Strings(out.Branches)
	sort.Sort(sort.Reverse(sort.StringSlice(out.Tags))) // 新 tag 在前
	out.Default = head

	l.mu.Lock()
	l.cache[repo] = entry{out, l.now()}
	l.mu.Unlock()
	return out, nil
}

func lsRemote(ctx context.Context, repo, token string) ([]*plumbing.Reference, string, error) {
	rem := git.NewRemote(nil, &config.RemoteConfig{Name: "origin", URLs: []string{repo}})
	opts := &git.ListOptions{}
	if token != "" && (strings.HasPrefix(repo, "https://") || strings.HasPrefix(repo, "http://")) {
		opts.Auth = &githttp.BasicAuth{Username: "x-access-token", Password: token}
	}
	refs, err := rem.ListContext(ctx, opts)
	if err != nil {
		return nil, "", err
	}
	var head string
	for _, r := range refs {
		if r.Name() == plumbing.HEAD && r.Type() == plumbing.SymbolicReference {
			head = r.Target().Short()
		}
	}
	return refs, head, nil
}
