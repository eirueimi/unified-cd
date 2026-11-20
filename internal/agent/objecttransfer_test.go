package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// objFake is a controller that issues presigned URLs to its own /blob
// endpoint (direct mode) and also answers the streaming routes. It records
// every request so tests can assert which path a transfer took.
type objFake struct {
	srv *httptest.Server

	mu    sync.Mutex
	seen  []string // "METHOD path" or "presign <kind>/<op>"
	auth  []string
	gets  []api.PresignRequest // cache get requests, in order
	blobs map[string][]byte
}

func newObjFake(t *testing.T) *objFake {
	t.Helper()
	f := &objFake{blobs: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *objFake) record(s string) {
	f.mu.Lock()
	f.seen = append(f.seen, s)
	f.mu.Unlock()
}

func (f *objFake) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

func (f *objFake) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/objects/presign"):
		var req api.PresignRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.record("presign " + req.Kind + "/" + req.Op)
		f.mu.Lock()
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		f.presign(w, req)
	case strings.HasPrefix(r.URL.Path, "/blob/"):
		f.record(r.Method + " /blob")
		id := strings.TrimPrefix(r.URL.Path, "/blob/")
		if r.Method == http.MethodPut {
			b, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.blobs[id] = b
			f.mu.Unlock()
			w.Header().Set("ETag", `"e"`)
			return
		}
		f.mu.Lock()
		b, ok := f.blobs[id]
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	case r.Method == http.MethodPut:
		// A streaming route: the transfer went through the controller.
		f.record(r.Method + " " + r.URL.Path)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	default:
		f.record(r.Method + " " + r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(api.ObjectError{Error: "cache miss", Code: api.ObjectErrCacheMiss})
	}
}

func (f *objFake) presign(w http.ResponseWriter, req api.PresignRequest) {
	w.Header().Set("Content-Type", "application/json")
	id := url.PathEscape(req.Kind + "-" + req.Name + req.Key)
	switch req.Op {
	case api.ObjectOpGet:
		f.mu.Lock()
		if req.Kind == api.ObjectKindCache {
			f.gets = append(f.gets, req)
		}
		_, ok := f.blobs[id]
		matched := req.Key
		if !ok && req.Kind == api.ObjectKindCache {
			for _, rk := range req.RestoreKeys {
				for bid := range f.blobs {
					if strings.HasPrefix(bid, "cache-"+rk) {
						id, ok, matched = bid, true, strings.TrimPrefix(bid, "cache-")
					}
				}
			}
		}
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			code := api.ObjectErrNotFound
			if req.Kind == api.ObjectKindCache {
				code = api.ObjectErrCacheMiss
			}
			_ = json.NewEncoder(w).Encode(api.ObjectError{Error: "missing", Code: code})
			return
		}
		_ = json.NewEncoder(w).Encode(api.PresignResponse{
			Mode: api.ObjectModeDirect, MatchedKey: matched,
			Request: &api.PresignedRequest{Method: http.MethodGet, URL: f.srv.URL + "/blob/" + id},
		})
	case api.ObjectOpCommit:
		_ = json.NewEncoder(w).Encode(api.PresignResponse{Mode: api.ObjectModeDirect})
	default: // put: the client sends a small body as one PUT
		_ = json.NewEncoder(w).Encode(api.PresignResponse{
			Mode:    api.ObjectModeDirect,
			Request: &api.PresignedRequest{Method: http.MethodPut, URL: f.srv.URL + "/blob/" + id},
		})
	}
}

func cacheStepRun(t *testing.T, a *Agent, workDir, key string, restoreKeys ...string) []deferredHook {
	t.Helper()
	sctx := &safeStepCtx{data: dsl.TemplateData{}}
	step := api.ClaimStep{Index: 0, Name: "c", Cache: &dsl.CacheStep{Path: "dir", Key: key, RestoreKeys: restoreKeys}}
	var mu sync.Mutex
	var hooks []deferredHook
	require.NoError(t, executeCacheStep(context.Background(), a.Client, a.ID, step, "r1", sctx, &mu, &hooks,
		newHostBackend(a, "r1", "test-job", workDir, nil), ScopeHandle{}))
	return hooks
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func seedCacheDir(t *testing.T) string {
	t.Helper()
	workDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(workDir, "dir"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "dir", "f.txt"), []byte("x"), 0o644))
	return workDir
}

func TestHostBackend_CacheDirectUsesPresign(t *testing.T) {
	f := newObjFake(t)
	a := &Agent{ID: "a1", Client: NewClient(f.srv.URL, "tok")}

	hooks := cacheStepRun(t, a, seedCacheDir(t), "k1") // restore: a miss
	require.Len(t, hooks, 1)
	hooks[0].fn(context.Background()) // deferred save

	reqs := f.requests()
	assert.Contains(t, reqs, "presign cache/get")
	assert.Contains(t, reqs, "presign cache/commit")
	assert.Contains(t, reqs, "PUT /blob")
	for _, r := range reqs {
		assert.NotContains(t, r, "/objects/caches", "direct mode must not stream through the controller")
	}
	assert.Less(t, indexOf(reqs, "PUT /blob"), indexOf(reqs, "presign cache/commit"), "commit follows the archive PUT")
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, h := range f.auth {
		assert.Equal(t, "Bearer tok", h, "presign reuses the agent's own credential")
	}
}

func TestHostBackend_CacheControllerModeStreams(t *testing.T) {
	f := newObjFake(t)
	cli := NewClient(f.srv.URL, "tok")
	cli.SetObjectTransferController(true)
	a := &Agent{ID: "a1", Client: cli}

	hooks := cacheStepRun(t, a, seedCacheDir(t), "k1")
	require.Len(t, hooks, 1)
	hooks[0].fn(context.Background())

	var streamed bool
	for _, r := range f.requests() {
		assert.False(t, strings.HasPrefix(r, "presign"), "controller mode sends no presign: %s", r)
		if strings.Contains(r, "/objects/caches") {
			streamed = true
		}
	}
	assert.True(t, streamed, "caches go through /objects/caches")
}

func writeArtifactSrc(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "a.txt"), []byte("payload"), 0o600))
	return src
}

func TestClient_ArtifactControllerModeUsesExistingRoutes(t *testing.T) {
	f := newObjFake(t)
	c := NewClient(f.srv.URL, "tok")
	c.SetObjectTransferController(true)
	require.NoError(t, c.UploadArtifact(t.Context(), "run1", "art1", writeArtifactSrc(t)))
	assert.Equal(t, []string{"PUT /api/v1/runs/run1/artifacts/art1"}, f.requests())
}

func TestClient_ArtifactDirectModeUsesPresign(t *testing.T) {
	f := newObjFake(t)
	c := NewClient(f.srv.URL, "tok")
	require.NoError(t, c.UploadArtifact(t.Context(), "run1", "art1", writeArtifactSrc(t)))

	dest := t.TempDir()
	require.NoError(t, c.DownloadArtifact(t.Context(), "run1", "run1", "art1", dest))
	got, err := os.ReadFile(filepath.Join(dest, "a.txt"))
	require.NoError(t, err)
	assert.Equal(t, "payload", string(got))

	reqs := f.requests()
	assert.Contains(t, reqs, "presign artifact/get")
	for _, r := range reqs {
		assert.NotContains(t, r, "/api/v1/runs/run1/artifacts/", "no legacy route in direct mode")
	}
}

func TestClient_ArtifactDirectModeCrossRunAsksTheClaimedRun(t *testing.T) {
	f := newObjFake(t)
	c := NewClient(f.srv.URL, "tok")
	err := c.DownloadArtifact(t.Context(), "claimed", "src9", "art1", t.TempDir())
	require.Error(t, err) // the fake holds no such artifact
	assert.Equal(t, "presign artifact/get", f.requests()[0])
}

// A restore that misses the exact key but matches a restoreKeys prefix comes
// back through the presign path with the entry's own key, and the files land
// in the workspace.
func TestHostBackend_CacheDirectRestoreKeysMatch(t *testing.T) {
	f := newObjFake(t)
	a := &Agent{ID: "a1", Client: NewClient(f.srv.URL, "tok")}

	hooks := cacheStepRun(t, a, seedCacheDir(t), "k-old")
	require.Len(t, hooks, 1)
	hooks[0].fn(context.Background())

	workDir := t.TempDir()
	cacheStepRun(t, a, workDir, "k-new", "k-")

	got, err := os.ReadFile(filepath.Join(workDir, "dir", "f.txt"))
	require.NoError(t, err, "the restoreKeys match must restore into the workspace")
	assert.Equal(t, "x", string(got))

	f.mu.Lock()
	defer f.mu.Unlock()
	last := f.gets[len(f.gets)-1]
	assert.Equal(t, "k-new", last.Key)
	assert.Equal(t, []string{"k-"}, last.RestoreKeys)
}
