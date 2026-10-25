package gitrefs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
)

func ref(name string) *plumbing.Reference {
	return plumbing.NewHashReference(plumbing.ReferenceName(name), plumbing.NewHash("1111111111111111111111111111111111111111"))
}

func newTestLister(calls *int, err error) *Lister {
	l := NewLister([]string{"git://git.local/"}, "")
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

func get(t *testing.T, l *Lister, query string) (int, Refs, map[string]string) {
	t.Helper()
	rec := httptest.NewRecorder()
	l.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/refs"+query, nil))
	var refs Refs
	var errBody map[string]string
	if rec.Code == http.StatusOK {
		_ = json.Unmarshal(rec.Body.Bytes(), &refs)
	} else {
		_ = json.Unmarshal(rec.Body.Bytes(), &errBody)
	}
	return rec.Code, refs, errBody
}

func TestListSortsAndFilters(t *testing.T) {
	calls := 0
	code, refs, _ := get(t, newTestLister(&calls, nil), "?repo=git://git.local/app.git")
	if code != 200 {
		t.Fatalf("status %d", code)
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

func TestAllowListAndValidation(t *testing.T) {
	calls := 0
	l := newTestLister(&calls, nil)
	if code, _, _ := get(t, l, ""); code != http.StatusBadRequest {
		t.Fatalf("missing repo: %d", code)
	}
	for _, repo := range []string{"http://169.254.169.254/", "git://git.local.evil.com/x", "file:///etc"} {
		if code, _, _ := get(t, l, "?repo="+repo); code != http.StatusForbidden {
			t.Fatalf("%s should be forbidden, got %d", repo, code)
		}
	}
	if calls != 0 {
		t.Fatal("forbidden repos must never be contacted")
	}
	if code, _, _ := get(t, NewLister(nil, ""), "?repo=git://git.local/a"); code != http.StatusForbidden {
		t.Fatal("empty allow-list must deny everything")
	}
}

func TestCacheAndErrors(t *testing.T) {
	calls := 0
	l := newTestLister(&calls, nil)
	now := time.Now()
	l.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		get(t, l, "?repo=git://git.local/app.git")
	}
	if calls != 1 {
		t.Fatalf("want 1 upstream call within TTL, got %d", calls)
	}
	now = now.Add(time.Minute)
	get(t, l, "?repo=git://git.local/app.git")
	if calls != 2 {
		t.Fatalf("cache should expire, calls=%d", calls)
	}

	calls = 0
	code, _, body := get(t, newTestLister(&calls, errors.New("boom")), "?repo=git://git.local/app.git")
	if code != http.StatusBadGateway || body["error"] == "" {
		t.Fatalf("upstream error: %d %v", code, body)
	}
}
