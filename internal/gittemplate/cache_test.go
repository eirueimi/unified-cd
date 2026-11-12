package gittemplate

import (
	"context"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/objectstore"
)

func TestCache_ScopePartitionsEntries(t *testing.T) {
	ctx := context.Background()
	c := NewCache(objectstore.NewLocalObjectStore(t.TempDir()))
	uri := URI{Host: "github.com", Owner: "acme", Repo: "ci-templates", Ref: "v1", Path: "x.yaml"}
	data := []byte("kind: JobTemplate\n")

	c.Put(ctx, uri, "gh-app", data)

	if _, ok := c.Get(ctx, uri, ""); ok {
		t.Fatal("an anonymous lookup must not see an entry cached under a credential scope")
	}
	if _, ok := c.Get(ctx, uri, "other"); ok {
		t.Fatal("a different scope must not see the entry")
	}
	got, ok := c.Get(ctx, uri, "gh-app")
	if !ok || string(got) != string(data) {
		t.Fatalf("same scope must hit: ok=%v got=%q", ok, got)
	}
	if k := cacheKey(uri, "gh-app"); !strings.HasPrefix(k, "git_templates/v2/") {
		t.Fatalf("cache key %q lacks the v2 prefix", k)
	}
	if cacheKey(uri, "") == cacheKey(uri, "gh-app") {
		t.Fatal("scope must change the key")
	}
}
