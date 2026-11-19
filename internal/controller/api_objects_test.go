package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/cache"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePresignCall records one URL the fake signed.
type fakePresignCall struct {
	Op       string
	Key      string
	Filename string
}

type fakeObject struct {
	data    []byte
	modTime time.Time
}

// fakePresigner is an in-memory objectstore.ObjectStore and
// objectstore.Presigner. Signed URLs look like https://store/<key>?op=<op>.
type fakePresigner struct {
	mu        sync.Mutex
	objects   map[string]fakeObject
	multipart bool
	calls     []fakePresignCall
	uploads   []objectstore.Upload
	aborted   []objectstore.Upload
	completed []string
	nextID    int
}

var (
	_ objectstore.ObjectStore = (*fakePresigner)(nil)
	_ objectstore.Presigner   = (*fakePresigner)(nil)
)

func newFakePresigner() *fakePresigner {
	return &fakePresigner{objects: map[string]fakeObject{}, multipart: true}
}

func (f *fakePresigner) Put(_ context.Context, key string, content io.Reader, _ int64) error {
	b, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = fakeObject{data: b, modTime: time.Now()}
	return nil
}

func (f *fakePresigner) Get(_ context.Context, key string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", objectstore.ErrNotFound, key)
	}
	return io.NopCloser(bytes.NewReader(o.data)), nil
}

func (f *fakePresigner) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
	return nil
}

func (f *fakePresigner) List(_ context.Context, prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (f *fakePresigner) Stat(_ context.Context, key string) (objectstore.ObjectInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[key]
	if !ok {
		return objectstore.ObjectInfo{}, fmt.Errorf("%w: %s", objectstore.ErrNotFound, key)
	}
	return objectstore.ObjectInfo{Key: key, Size: int64(len(o.data)), ETag: "etag-" + key, LastModified: o.modTime}, nil
}

func (f *fakePresigner) sign(op, key, filename string) (*url.URL, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakePresignCall{Op: op, Key: key, Filename: filename})
	return url.Parse("https://store/" + key + "?op=" + op)
}

func (f *fakePresigner) PresignGet(_ context.Context, key, filename string) (*url.URL, error) {
	return f.sign("get", key, filename)
}
func (f *fakePresigner) PresignHead(_ context.Context, key string) (*url.URL, error) {
	return f.sign("head", key, "")
}
func (f *fakePresigner) PresignPut(_ context.Context, key string) (*url.URL, error) {
	return f.sign("put", key, "")
}
func (f *fakePresigner) PresignUploadPart(_ context.Context, key, uploadID string, part int) (*url.URL, error) {
	u, err := f.sign("part", key, "")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("uploadId", uploadID)
	q.Set("partNumber", fmt.Sprint(part))
	u.RawQuery = q.Encode()
	return u, nil
}

func (f *fakePresigner) StartMultipart(_ context.Context, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("upload-%d", f.nextID)
	f.calls = append(f.calls, fakePresignCall{Op: "multipart-start", Key: key})
	f.uploads = append(f.uploads, objectstore.Upload{Key: key, UploadID: id, Initiated: time.Now()})
	return id, nil
}

func (f *fakePresigner) CompleteMultipart(_ context.Context, key, uploadID string, _ []objectstore.CompletedPart) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakePresignCall{Op: "multipart-complete", Key: key})
	f.completed = append(f.completed, key+"#"+uploadID)
	return nil
}

func (f *fakePresigner) AbortMultipart(_ context.Context, key, uploadID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakePresignCall{Op: "multipart-abort", Key: key})
	for i, u := range f.uploads {
		if u.Key == key && u.UploadID == uploadID {
			f.aborted = append(f.aborted, u)
			f.uploads = append(f.uploads[:i], f.uploads[i+1:]...)
			break
		}
	}
	return nil
}

func (f *fakePresigner) ListMultipartUploads(_ context.Context, prefix string) ([]objectstore.Upload, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []objectstore.Upload
	for _, u := range f.uploads {
		if strings.HasPrefix(u.Key, prefix) {
			out = append(out, u)
		}
	}
	return out, nil
}

func (f *fakePresigner) MultipartPresign() bool { return f.multipart }

func (f *fakePresigner) signedKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.Key)
	}
	return out
}

func (f *fakePresigner) abortedKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, u := range f.aborted {
		out = append(out, u.Key)
	}
	sort.Strings(out)
	return out
}

// presignTestServer wires a Postgres-backed server with a fake presigning store.
func presignTestServer(t *testing.T, cfg Config) (*Server, store.Store, *fakePresigner) {
	t.Helper()
	s, st := newTestServerWithConfig(t, cfg)
	fake := newFakePresigner()
	s.SetObjectStore(fake)
	return s, st, fake
}

// seedClaimedRun creates jobName (if needed) with spec and one run, then has
// agentID claim it. It returns the run ID.
func seedClaimedRun(t *testing.T, st store.Store, jobName, agentID, spec string) string {
	t.Helper()
	runID := seedRun(t, st, jobName, spec)
	_, err := st.TransitionPendingToQueued(t.Context(), 100)
	require.NoError(t, err)
	claimed, err := st.ClaimNextRun(t.Context(), agentID, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, runID, claimed.ID)
	return runID
}

// seedRun creates jobName (if needed) with spec and one Pending run.
func seedRun(t *testing.T, st store.Store, jobName, spec string) string {
	t.Helper()
	if spec == "" {
		spec = `{}`
	}
	_, err := st.UpsertJob(t.Context(), jobName, "unified-cd/v1", []byte(spec))
	require.NoError(t, err)
	run, err := st.CreateRun(t.Context(), jobName, nil, []byte(spec), nil, nil, "", "")
	require.NoError(t, err)
	return run.ID
}

func doPresign(t *testing.T, s *Server, bearer, runID string, req api.PresignRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+runID+"/objects/presign", bytes.NewReader(body))
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, r)
	return rec
}

func decodePresign(t *testing.T, rec *httptest.ResponseRecorder) api.PresignResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out api.PresignResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

func requireObjectError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	require.Equal(t, status, rec.Code, rec.Body.String())
	var e api.ObjectError
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &e), rec.Body.String())
	assert.Equal(t, code, e.Code)
}

func urlKey(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return strings.TrimPrefix(u.Path, "/")
}

func TestPresign_ArtifactPutOnlyOwnRun(t *testing.T) {
	s, st, _ := presignTestServer(t, Config{})
	token := issueAgentAccessForTest(t, st, "agent-a", nil, nil)
	runID := seedClaimedRun(t, st, "job-a", "agent-a", "")

	out := decodePresign(t, doPresign(t, s, token, runID, api.PresignRequest{Kind: "artifact", Op: "put", Name: "a"}))
	assert.Equal(t, "direct", out.Mode)
	require.NotNil(t, out.Request)
	assert.Equal(t, http.MethodPut, out.Request.Method)
	assert.Equal(t, "artifacts/"+runID+"/a.tar.gz", urlKey(t, out.Request.URL))
	assert.WithinDuration(t, time.Now().Add(objectstore.PresignExpiry), out.Request.ExpiresAt, time.Minute)

	other := seedRun(t, st, "job-b", "")
	rec := doPresign(t, s, token, runID, api.PresignRequest{Kind: "artifact", Op: "put", Name: "a", SourceRunID: other})
	requireObjectError(t, rec, http.StatusBadRequest, "invalid")
}

func TestPresign_ArtifactGetCrossRun(t *testing.T) {
	s, st, fake := presignTestServer(t, Config{})
	token := issueAgentAccessForTest(t, st, "agent-a", nil, nil)
	runID := seedClaimedRun(t, st, "job-a", "agent-a", "")
	other := seedRun(t, st, "job-b", "")
	require.NoError(t, fake.Put(t.Context(), "artifacts/"+other+"/a.tar.gz", strings.NewReader("hello"), 5))

	out := decodePresign(t, doPresign(t, s, token, runID, api.PresignRequest{Kind: "artifact", Op: "get", Name: "a", SourceRunID: other}))
	require.NotNil(t, out.Request)
	assert.Equal(t, http.MethodGet, out.Request.Method)
	assert.Equal(t, "artifacts/"+other+"/a.tar.gz", urlKey(t, out.Request.URL))
	assert.Equal(t, int64(5), out.Size)
	assert.NotEmpty(t, out.ETag)
	fake.mu.Lock()
	last := fake.calls[len(fake.calls)-1]
	fake.mu.Unlock()
	assert.Equal(t, "a.tar.gz", last.Filename)

	rec := doPresign(t, s, token, runID, api.PresignRequest{Kind: "artifact", Op: "get", Name: "a", SourceRunID: "00000000-0000-0000-0000-000000000000"})
	requireObjectError(t, rec, http.StatusNotFound, "not-found")

	// A missing artifact in an existing run is also a 404.
	rec = doPresign(t, s, token, runID, api.PresignRequest{Kind: "artifact", Op: "get", Name: "missing"})
	requireObjectError(t, rec, http.StatusNotFound, "not-found")
}

func TestPresign_CacheNamespaceFromRunRecord(t *testing.T) {
	s, st, _ := presignTestServer(t, Config{})
	token := issueAgentAccessForTest(t, st, "agent-a", nil, nil)
	runID := seedClaimedRun(t, st, "team-a/build", "agent-a", "")

	// The body carries no job; an unknown field claiming one is ignored.
	body := []byte(`{"kind":"cache","op":"put","key":"k","job":"someone-else"}`)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+runID+"/objects/presign", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, r)
	out := decodePresign(t, rec)
	require.NotNil(t, out.Request)
	assert.Equal(t, cache.DataKey("team-a/build", "k"), urlKey(t, out.Request.URL))
}

func TestPresign_NeverSignsMeta(t *testing.T) {
	s, st, fake := presignTestServer(t, Config{})
	token := issueAgentAccessForTest(t, st, "agent-a", nil, nil)
	runID := seedClaimedRun(t, st, "job-a", "agent-a", "")
	// Make every get/head a hit: the artifact exists, and the cache entry has
	// both a data object and a committed .meta.
	require.NoError(t, fake.Put(t.Context(), "artifacts/"+runID+"/a.tar.gz", strings.NewReader("x"), 1))
	require.NoError(t, fake.Put(t.Context(), cache.DataKey("job-a", "k"), strings.NewReader("x"), 1))
	require.NoError(t, cache.Commit(t.Context(), fake, "job-a", "k", 1, time.Now().Add(time.Hour)))

	ops := []string{"get", "head", "put", "multipart-start", "multipart-parts", "multipart-complete", "multipart-abort", "commit"}
	succeeded := 0
	for _, kind := range []string{"artifact", "cache"} {
		for _, op := range ops {
			for _, name := range []string{"a", "a.meta", "k.meta"} {
				req := api.PresignRequest{Kind: kind, Op: op, UploadID: "u", PartNumbers: []int{1}, Parts: []api.ObjectPart{{PartNumber: 1, ETag: "e"}}}
				if kind == "artifact" {
					req.Name = name
				} else {
					req.Key = strings.Replace(name, "a", "k", 1)
				}
				rec := doPresign(t, s, token, runID, req)
				if rec.Code != http.StatusOK {
					continue
				}
				succeeded++
				var out api.PresignResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
				var urls []string
				if out.Request != nil {
					urls = append(urls, out.Request.URL)
				}
				for _, p := range out.Parts {
					urls = append(urls, p.URL)
				}
				for _, u := range urls {
					assert.False(t, strings.HasSuffix(urlKey(t, u), ".meta"), "%s/%s signed %s", kind, op, u)
				}
			}
		}
	}
	// Most combinations succeed (every op of both kinds for the plain names),
	// so the loop above really inspected signed URLs.
	require.GreaterOrEqual(t, succeeded, 15)
	for _, k := range fake.signedKeys() {
		assert.False(t, strings.HasSuffix(k, ".meta"), "signed or drove %s", k)
	}
}

func TestPresign_CacheGetMissAndHit(t *testing.T) {
	s, st, fake := presignTestServer(t, Config{})
	token := issueAgentAccessForTest(t, st, "agent-a", nil, nil)
	runID := seedClaimedRun(t, st, "job-a", "agent-a", "")

	rec := doPresign(t, s, token, runID, api.PresignRequest{Kind: "cache", Op: "get", Key: "deps-v2", RestoreKeys: []string{"deps-"}})
	requireObjectError(t, rec, http.StatusNotFound, "cache-miss")

	require.NoError(t, fake.Put(t.Context(), cache.DataKey("job-a", "deps-v1"), strings.NewReader("archive"), 7))
	require.NoError(t, cache.Commit(t.Context(), fake, "job-a", "deps-v1", 7, time.Now().Add(time.Hour)))

	out := decodePresign(t, doPresign(t, s, token, runID, api.PresignRequest{Kind: "cache", Op: "get", Key: "deps-v2", RestoreKeys: []string{"deps-"}}))
	require.NotNil(t, out.Request)
	assert.Equal(t, http.MethodGet, out.Request.Method)
	assert.Equal(t, cache.DataKey("job-a", "deps-v1"), urlKey(t, out.Request.URL))
	assert.Equal(t, "deps-v1", out.MatchedKey)
	assert.Equal(t, int64(7), out.Size)
	assert.NotEmpty(t, out.ETag)
}

// An exact .meta whose data object is gone must not produce a dead URL: the
// restoreKeys scan answers instead.
func TestPresign_CacheGetExactMetaWithoutDataFallsBack(t *testing.T) {
	s, st, fake := presignTestServer(t, Config{})
	token := issueAgentAccessForTest(t, st, "agent-a", nil, nil)
	runID := seedClaimedRun(t, st, "job-a", "agent-a", "")
	require.NoError(t, cache.Commit(t.Context(), fake, "job-a", "deps-v2", 7, time.Now().Add(2*time.Hour)))

	rec := doPresign(t, s, token, runID, api.PresignRequest{Kind: "cache", Op: "get", Key: "deps-v2"})
	requireObjectError(t, rec, http.StatusNotFound, "cache-miss")

	require.NoError(t, fake.Put(t.Context(), cache.DataKey("job-a", "deps-v1"), strings.NewReader("archive"), 7))
	require.NoError(t, cache.Commit(t.Context(), fake, "job-a", "deps-v1", 7, time.Now().Add(time.Hour)))
	out := decodePresign(t, doPresign(t, s, token, runID, api.PresignRequest{Kind: "cache", Op: "get", Key: "deps-v2", RestoreKeys: []string{"deps-"}}))
	assert.Equal(t, "deps-v1", out.MatchedKey)
	assert.Equal(t, cache.DataKey("job-a", "deps-v1"), urlKey(t, out.Request.URL))
}

func TestPresign_MultipartPartsBatchLimit(t *testing.T) {
	s, st, _ := presignTestServer(t, Config{})
	token := issueAgentAccessForTest(t, st, "agent-a", nil, nil)
	runID := seedClaimedRun(t, st, "job-a", "agent-a", "")

	start := decodePresign(t, doPresign(t, s, token, runID, api.PresignRequest{Kind: "artifact", Op: "multipart-start", Name: "a"}))
	require.NotEmpty(t, start.UploadID)

	many := make([]int, 101)
	for i := range many {
		many[i] = i + 1
	}
	rec := doPresign(t, s, token, runID, api.PresignRequest{Kind: "artifact", Op: "multipart-parts", Name: "a", UploadID: start.UploadID, PartNumbers: many})
	requireObjectError(t, rec, http.StatusBadRequest, "invalid")

	rec = doPresign(t, s, token, runID, api.PresignRequest{Kind: "artifact", Op: "multipart-parts", Name: "a", UploadID: start.UploadID, PartNumbers: []int{0}})
	requireObjectError(t, rec, http.StatusBadRequest, "invalid")
	rec = doPresign(t, s, token, runID, api.PresignRequest{Kind: "artifact", Op: "multipart-parts", Name: "a", UploadID: start.UploadID, PartNumbers: []int{10001}})
	requireObjectError(t, rec, http.StatusBadRequest, "invalid")

	out := decodePresign(t, doPresign(t, s, token, runID, api.PresignRequest{Kind: "artifact", Op: "multipart-parts", Name: "a", UploadID: start.UploadID, PartNumbers: []int{1, 2, 3}}))
	require.Len(t, out.Parts, 3)
	for i, p := range out.Parts {
		assert.Equal(t, i+1, p.PartNumber)
		assert.Equal(t, http.MethodPut, p.Method)
		assert.Equal(t, "artifacts/"+runID+"/a.tar.gz", urlKey(t, p.URL))
	}

	done := doPresign(t, s, token, runID, api.PresignRequest{Kind: "artifact", Op: "multipart-complete", Name: "a", UploadID: start.UploadID, Parts: []api.ObjectPart{{PartNumber: 1, ETag: "e1"}, {PartNumber: 2, ETag: "e2"}}})
	decodePresign(t, done)
}

func TestPresign_MultipartUnavailable(t *testing.T) {
	s, st, fake := presignTestServer(t, Config{})
	fake.multipart = false
	token := issueAgentAccessForTest(t, st, "agent-a", nil, nil)
	runID := seedClaimedRun(t, st, "job-a", "agent-a", "")
	rec := doPresign(t, s, token, runID, api.PresignRequest{Kind: "cache", Op: "multipart-start", Key: "k"})
	requireObjectError(t, rec, http.StatusNotImplemented, "multipart-unavailable")
	// A single PUT still works.
	decodePresign(t, doPresign(t, s, token, runID, api.PresignRequest{Kind: "cache", Op: "put", Key: "k"}))
}

func TestPresign_CacheCommitWritesMetaServerSide(t *testing.T) {
	s, st, fake := presignTestServer(t, Config{})
	token := issueAgentAccessForTest(t, st, "agent-a", nil, nil)
	spec := `{"steps":[{"name":"restore","cache":{"path":"p","key":"k","ttlDays":7}},{"parallel":[{"name":"x","cache":{"path":"q","key":"k2","ttlDays":3}}]}],"finally":[{"name":"f","run":"true"}]}`
	runID := seedClaimedRun(t, st, "job-a", "agent-a", spec)

	rec := doPresign(t, s, token, runID, api.PresignRequest{Kind: "cache", Op: "commit", Key: "k", TTLDays: 400})
	requireObjectError(t, rec, http.StatusNotFound, "cache-miss")

	require.NoError(t, fake.Put(t.Context(), cache.DataKey("job-a", "k"), bytes.NewReader(make([]byte, 42)), 42))
	before := time.Now()
	out := decodePresign(t, doPresign(t, s, token, runID, api.PresignRequest{Kind: "cache", Op: "commit", Key: "k", TTLDays: 400}))
	assert.Equal(t, "direct", out.Mode)
	assert.Nil(t, out.Request)

	rc, err := fake.Get(t.Context(), strings.TrimSuffix(cache.DataKey("job-a", "k"), ".tar.zst")+".meta")
	require.NoError(t, err)
	defer rc.Close()
	var meta cache.Meta
	require.NoError(t, json.NewDecoder(rc).Decode(&meta))
	assert.Equal(t, int64(42), meta.Size)
	assert.Equal(t, "job-a", meta.OwnerJob)
	assert.Equal(t, "k", meta.OriginalKey)
	assert.WithinDuration(t, before.Add(7*24*time.Hour), meta.ExpiresAt, time.Minute)
}

func TestCacheTTLDays(t *testing.T) {
	spec := func(ttls ...int) []byte {
		var steps []string
		for i, ttl := range ttls {
			steps = append(steps, fmt.Sprintf(`{"name":"s%d","cache":{"path":"p","key":"k","ttlDays":%d}}`, i, ttl))
		}
		return []byte(`{"steps":[` + strings.Join(steps, ",") + `]}`)
	}
	cases := []struct {
		name      string
		spec      []byte
		requested int
		want      int
	}{
		{"zero counts as 30", spec(0), 400, 30},
		{"request below max", spec(7, 60), 10, 10},
		{"request above max", spec(7, 60), 400, 60},
		{"no request means max", spec(7, 60), 0, 60},
		{"negative means max", spec(7), -1, 7},
		{"no cache step", []byte(`{"steps":[{"name":"a","run":"true"}]}`), 0, 30},
		{"finally counts", []byte(`{"steps":[{"name":"a","run":"true"}],"finally":[{"name":"f","cache":{"path":"p","key":"k","ttlDays":90}}]}`), 0, 90},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cacheTTLDays(tc.spec, tc.requested)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestPresign_LocalStoreAnswersControllerMode(t *testing.T) {
	s, st := newTestServerWithConfig(t, Config{})
	s.SetObjectStore(objectstore.NewLocalObjectStore(t.TempDir()))
	token := issueAgentAccessForTest(t, st, "agent-a", nil, nil)
	runID := seedClaimedRun(t, st, "job-a", "agent-a", "")
	rec := doPresign(t, s, token, runID, api.PresignRequest{Kind: "artifact", Op: "put", Name: "a"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"mode":"controller"}`, rec.Body.String())
}

func TestPresign_NoObjectStore(t *testing.T) {
	s, st := newTestServerWithConfig(t, Config{})
	token := issueAgentAccessForTest(t, st, "agent-a", nil, nil)
	runID := seedClaimedRun(t, st, "job-a", "agent-a", "")
	rec := doPresign(t, s, token, runID, api.PresignRequest{Kind: "artifact", Op: "put", Name: "a"})
	requireObjectError(t, rec, http.StatusServiceUnavailable, "no-object-store")
}

func TestPresign_TerminalRunStillAcceptsUpload(t *testing.T) {
	s, st, _ := presignTestServer(t, Config{})
	token := issueAgentAccessForTest(t, st, "agent-a", nil, nil)
	runID := seedClaimedRun(t, st, "job-a", "agent-a", "")
	updated, err := st.FinishRun(t.Context(), runID, api.RunSucceeded)
	require.NoError(t, err)
	require.True(t, updated)
	out := decodePresign(t, doPresign(t, s, token, runID, api.PresignRequest{Kind: "artifact", Op: "put", Name: "a"}))
	require.NotNil(t, out.Request)
}

func TestPresign_RateLimitedPerRequester(t *testing.T) {
	s, st, _ := presignTestServer(t, Config{StoreCredentialRateBurst: 1, StoreCredentialRatePerSecond: 0.001})
	token := issueAgentAccessForTest(t, st, "agent-a", nil, nil)
	runID := seedClaimedRun(t, st, "job-a", "agent-a", "")
	decodePresign(t, doPresign(t, s, token, runID, api.PresignRequest{Kind: "artifact", Op: "put", Name: "a"}))
	rec := doPresign(t, s, token, runID, api.PresignRequest{Kind: "artifact", Op: "put", Name: "a"})
	requireObjectError(t, rec, http.StatusTooManyRequests, "rate-limited")
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))

	// A different agent has its own budget.
	token2 := issueAgentAccessForTest(t, st, "agent-b", nil, nil)
	runID2 := seedClaimedRun(t, st, "job-b", "agent-b", "")
	decodePresign(t, doPresign(t, s, token2, runID2, api.PresignRequest{Kind: "artifact", Op: "put", Name: "a"}))
}

func TestPresign_AuditsWritesAndDenials(t *testing.T) {
	s, st, fake := presignTestServer(t, Config{})
	token := issueAgentAccessForTest(t, st, "agent-a", nil, nil)
	runID := seedClaimedRun(t, st, "job-a", "agent-a", "")
	require.NoError(t, fake.Put(t.Context(), "artifacts/"+runID+"/a.tar.gz", strings.NewReader("x"), 1))
	require.NoError(t, fake.Put(t.Context(), cache.DataKey("job-a", "k"), strings.NewReader("x"), 1))

	countRows := func() int {
		rows, err := st.ListAuditLogs(t.Context(), 1000, 0)
		require.NoError(t, err)
		n := 0
		for _, row := range rows {
			if row.Action == "objects.presign" {
				n++
				assert.NotContains(t, row.Resource, "https://")
			}
		}
		return n
	}

	decodePresign(t, doPresign(t, s, token, runID, api.PresignRequest{Kind: "artifact", Op: "get", Name: "a"}))
	assert.Equal(t, 0, countRows(), "get must not be audited")

	decodePresign(t, doPresign(t, s, token, runID, api.PresignRequest{Kind: "artifact", Op: "put", Name: "a"}))
	assert.Equal(t, 1, countRows())
	decodePresign(t, doPresign(t, s, token, runID, api.PresignRequest{Kind: "artifact", Op: "multipart-start", Name: "a"}))
	assert.Equal(t, 2, countRows())
	decodePresign(t, doPresign(t, s, token, runID, api.PresignRequest{Kind: "cache", Op: "commit", Key: "k"}))
	assert.Equal(t, 3, countRows())

	other := issueAgentAccessForTest(t, st, "agent-b", nil, nil)
	rec := doPresign(t, s, other, runID, api.PresignRequest{Kind: "artifact", Op: "get", Name: "a"})
	requireObjectError(t, rec, http.StatusForbidden, "forbidden")
	assert.Equal(t, 4, countRows(), "a denial is audited")

	rows, err := st.ListAuditLogs(t.Context(), 1000, 0)
	require.NoError(t, err)
	var statuses []int
	for _, row := range rows {
		if row.Action == "objects.presign" {
			statuses = append(statuses, row.Status)
			assert.Equal(t, "/api/v1/runs/"+runID+"/objects/presign", row.Path)
		}
	}
	assert.Contains(t, statuses, http.StatusForbidden)
}

func TestTruncateAuditResource(t *testing.T) {
	assert.Equal(t, "artifact:r/a", truncateAuditResource("artifact:r/a"))
	long := strings.Repeat("a", 1000)
	assert.Len(t, truncateAuditResource(long), maxAuditResourceBytes)
	// A multi-byte rune straddling the limit is dropped whole.
	multi := strings.Repeat("a", maxAuditResourceBytes-1) + "é"
	got := truncateAuditResource(multi)
	assert.LessOrEqual(t, len(got), maxAuditResourceBytes)
	assert.True(t, utf8.ValidString(got))
}

func TestPresign_AuditResourceIsBounded(t *testing.T) {
	s, st, _ := presignTestServer(t, Config{})
	token := issueAgentAccessForTest(t, st, "agent-a", nil, nil)
	runID := seedClaimedRun(t, st, "job-a", "agent-a", "")
	doPresign(t, s, token, runID, api.PresignRequest{Kind: "artifact", Op: "put", Name: strings.Repeat("n", 2000)})
	doPresign(t, s, token, runID, api.PresignRequest{Kind: "cache", Op: "commit", Key: strings.Repeat("k", 2000)})

	rows, err := st.ListAuditLogs(t.Context(), 1000, 0)
	require.NoError(t, err)
	seen := 0
	for _, row := range rows {
		if row.Action == "objects.presign" {
			seen++
			assert.LessOrEqual(t, len(row.Resource), maxAuditResourceBytes)
		}
	}
	assert.Equal(t, 2, seen, "both oversized requests must be audited")
}
