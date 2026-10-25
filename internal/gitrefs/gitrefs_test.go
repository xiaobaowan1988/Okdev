package gitrefs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
)

func ref(name string) *plumbing.Reference {
	return plumbing.NewHashReference(plumbing.ReferenceName(name), plumbing.NewHash("1111111111111111111111111111111111111111"))
}

func newTestLister(calls *int, err error) *Lister {
	l := NewLister("")
	l.list = func(context.Context, string, string) ([]*plumbing.Reference, string, error) {
		*calls++
		if err != nil {
			return nil, "", err
		}
		return []*plumbing.Reference{
			ref("refs/heads/main"), ref("refs/heads/feature"), ref("refs/tags/v1.0"), ref("refs/tags/v1.1"),
			ref("refs/pull/1/head"), // 既不是分支也不是 tag,应被忽略
		}, "main", nil
	}
	return l
}

func TestListSortsAndFilters(t *testing.T) {
	calls := 0
	refs, err := newTestLister(&calls, nil).List(context.Background(), "git://git.local/app.git")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs.Branches) != 2 || refs.Branches[0] != "feature" || refs.Branches[1] != "main" {
		t.Fatalf("branches %v", refs.Branches)
	}
	if len(refs.Tags) != 2 || refs.Tags[0] != "v1.1" {
		t.Fatalf("tags should be newest first: %v", refs.Tags)
	}
	if refs.Default != "main" {
		t.Fatalf("default %q", refs.Default)
	}
}

func TestCacheAndErrors(t *testing.T) {
	calls := 0
	l := newTestLister(&calls, nil)
	now := time.Now()
	l.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if _, err := l.List(context.Background(), "git://git.local/app.git"); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("want 1 upstream call within TTL, got %d", calls)
	}
	now = now.Add(time.Minute)
	l.List(context.Background(), "git://git.local/app.git")
	if calls != 2 {
		t.Fatalf("cache should expire, calls=%d", calls)
	}

	calls = 0
	if _, err := newTestLister(&calls, errors.New("boom")).List(context.Background(), "git://git.local/app.git"); err == nil {
		t.Fatal("upstream error must be returned")
	}
}
