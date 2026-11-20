package controller

import (
	"context"
	"errors"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func doStream(t *testing.T, s *Server, method, bearer, path string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, body)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, r)
	return rec
}

func daysOut(m cache.Meta) int { return int(math.Round(time.Until(m.ExpiresAt).Hours() / 24)) }

func metaOf(dataKey string) string { return strings.TrimSuffix(dataKey, ".tar.zst") + ".meta" }

func TestObjectStream_CachePutCommitsMetaServerSide(t *testing.T) {
	s, st, fake := presignTestServer(t, Config{})
	token := issueAgentAccessForTest(t, st, "agent-a", nil, nil)
	spec := `{"steps":[{"name":"c","cache":{"key":"k","path":"p","ttlDays":7}}]}`
	runID := seedClaimedRun(t, st, "job-a", "agent-a", spec)

	body := strings.Repeat("archive-", 100)
	rec := doStream(t, s, http.MethodPut, token, "/api/v1/runs/"+runID+"/objects/caches?key=deps-v1&ttlDays=3", strings.NewReader(body))
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	dataKey := cache.DataKey("job-a", "deps-v1")
	rc, err := fake.Get(t.Context(), metaOf(dataKey))
	require.NoError(t, err)
	var meta cache.Meta
	require.NoError(t, json.NewDecoder(rc).Decode(&meta))
	rc.Close()
	assert.Equal(t, "job-a", meta.OwnerJob)
	assert.Equal(t, int64(len(body)), meta.Size)
	assert.Equal(t, "deps-v1", meta.OriginalKey)
	assert.Equal(t, 3, daysOut(meta), "ttlDays=3 is below the declared 7 and is honoured")

	rec = doStream(t, s, http.MethodPut, token, "/api/v1/runs/"+runID+"/objects/caches?key=deps-v2&ttlDays=99", strings.NewReader("x"))
	require.Equal(t, http.StatusNoContent, rec.Code)
	rc, err = fake.Get(t.Context(), metaOf(cache.DataKey("job-a", "deps-v2")))
	require.NoError(t, err)
	require.NoError(t, json.NewDecoder(rc).Decode(&meta))
	rc.Close()
	assert.Equal(t, 7, daysOut(meta), "capped at the declared 7 days")

	rec = doStream(t, s, http.MethodPut, token, "/api/v1/runs/"+runID+"/objects/caches", strings.NewReader("x"))
	requireObjectError(t, rec, http.StatusBadRequest, "invalid")
}

func TestObjectStream_CacheGetRestoreKeyHeader(t *testing.T) {
	s, st, _ := presignTestServer(t, Config{})
	token := issueAgentAccessForTest(t, st, "agent-a", nil, nil)
	runID := seedClaimedRun(t, st, "job-a", "agent-a", "")

	rec := doStream(t, s, http.MethodPut, token, "/api/v1/runs/"+runID+"/objects/caches?key=deps-v1-abc", strings.NewReader("payload"))
	require.Equal(t, http.StatusNoContent, rec.Code)

	q := url.Values{"key": {"deps-v2-zzz"}, "restoreKey": {"nope-", "deps-v1-"}}
	rec = doStream(t, s, http.MethodGet, token, "/api/v1/runs/"+runID+"/objects/caches?"+q.Encode(), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "deps-v1-abc", rec.Header().Get("X-Unified-Cache-Key"))
	assert.Equal(t, "payload", rec.Body.String())

	q = url.Values{"key": {"deps-v1-abc"}}
	rec = doStream(t, s, http.MethodGet, token, "/api/v1/runs/"+runID+"/objects/caches?"+q.Encode(), nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "deps-v1-abc", rec.Header().Get("X-Unified-Cache-Key"))

	q = url.Values{"key": {"other"}, "restoreKey": {"nothing-"}}
	rec = doStream(t, s, http.MethodGet, token, "/api/v1/runs/"+runID+"/objects/caches?"+q.Encode(), nil)
	requireObjectError(t, rec, http.StatusNotFound, "cache-miss")

	// Another job's run does not see the entry.
	_ = issueAgentAccessForTest(t, st, "agent-b", nil, nil)
	tokenB := issueAgentAccessForTest(t, st, "agent-c", nil, nil)
	runB := seedClaimedRun(t, st, "job-b", "agent-c", "")
	q = url.Values{"key": {"deps-v1-abc"}}
	rec = doStream(t, s, http.MethodGet, tokenB, "/api/v1/runs/"+runB+"/objects/caches?"+q.Encode(), nil)
	requireObjectError(t, rec, http.StatusNotFound, "cache-miss")
}

func TestObjectStream_ArtifactPutOtherRunForbidden(t *testing.T) {
	s, st, fake := presignTestServer(t, Config{})
	_ = issueAgentAccessForTest(t, st, "agent-a", nil, nil)
	tokenB := issueAgentAccessForTest(t, st, "agent-b", nil, nil)
	runA := seedClaimedRun(t, st, "job-a", "agent-a", "")

	// agent-b did not claim runA.
	rec := doStream(t, s, http.MethodPut, tokenB, "/api/v1/runs/"+runA+"/objects/artifacts/a", strings.NewReader("x"))
	requireObjectError(t, rec, http.StatusForbidden, "forbidden")
	_, err := fake.Get(t.Context(), "artifacts/"+runA+"/a.tar.gz")
	require.Error(t, err)
}

func TestObjectStream_ArtifactRoundTripAndCrossRunRead(t *testing.T) {
	s, st, fake := presignTestServer(t, Config{})
	token := issueAgentAccessForTest(t, st, "agent-a", nil, nil)
	runA := seedClaimedRun(t, st, "job-a", "agent-a", "")
	other := seedRun(t, st, "job-b", "")

	rec := doStream(t, s, http.MethodPut, token, "/api/v1/runs/"+runA+"/objects/artifacts/a", strings.NewReader("hello"))
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	require.NoError(t, fake.Put(t.Context(), "artifacts/"+other+"/b.tar.gz", strings.NewReader("theirs"), 6))

	rec = doStream(t, s, http.MethodGet, token, "/api/v1/runs/"+runA+"/objects/artifacts/a", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "hello", rec.Body.String())

	rec = doStream(t, s, http.MethodGet, token, "/api/v1/runs/"+runA+"/objects/artifacts/b?sourceRunId="+other, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "theirs", rec.Body.String())

	rec = doStream(t, s, http.MethodGet, token, "/api/v1/runs/"+runA+"/objects/artifacts/missing", nil)
	requireObjectError(t, rec, http.StatusNotFound, "not-found")
	rec = doStream(t, s, http.MethodGet, token, "/api/v1/runs/"+runA+"/objects/artifacts/b?sourceRunId=00000000-0000-0000-0000-000000000000", nil)
	requireObjectError(t, rec, http.StatusNotFound, "not-found")
	rec = doStream(t, s, http.MethodGet, "", "/api/v1/runs/"+runA+"/objects/artifacts/a", nil)
	requireObjectError(t, rec, http.StatusUnauthorized, "unauthenticated")
}

func TestObjectStream_SidecarAuth409WithoutBinding(t *testing.T) {
	s, st, _ := presignTestServer(t, sidecarPresignConfig([]string{"ci"}))
	runID := seedRun(t, st, "job-a", "")

	for _, c := range []struct{ method, path string }{
		{http.MethodPut, "/objects/artifacts/a"},
		{http.MethodGet, "/objects/artifacts/a"},
		{http.MethodPut, "/objects/caches?key=k"},
		{http.MethodGet, "/objects/caches?key=k"},
	} {
		rec := doStream(t, s, c.method, sidecarToken(), "/api/v1/runs/"+runID+c.path, strings.NewReader("x"))
		requireObjectError(t, rec, http.StatusConflict, "binding-pending")
		assert.Equal(t, "1", rec.Header().Get("Retry-After"))
	}

	require.NoError(t, st.UpsertRunPodBinding(t.Context(), runID, "ucd-run-abc", "job-pod-uid"))
	rec := doStream(t, s, http.MethodPut, sidecarToken(), "/api/v1/runs/"+runID+"/objects/artifacts/a", strings.NewReader("x"))
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

func TestObjectStream_ArtifactNameWithCommaMatchesDirectMode(t *testing.T) {
	s, st, fake := presignTestServer(t, Config{})
	token := issueAgentAccessForTest(t, st, "agent-a", nil, nil)
	runID := seedClaimedRun(t, st, "job-a", "agent-a", "")

	rec := doStream(t, s, http.MethodPut, token, "/api/v1/runs/"+runID+"/objects/artifacts/"+url.PathEscape("report,v1;x"), strings.NewReader("data"))
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	rc, err := fake.Get(t.Context(), "artifacts/"+runID+"/report,v1;x.tar.gz")
	require.NoError(t, err, "controller mode must store the plain name")
	rc.Close()

	out := decodePresign(t, doPresign(t, s, token, runID, api.PresignRequest{Kind: "artifact", Op: "get", Name: "report,v1;x"}))
	require.NotNil(t, out.Request)
	assert.Equal(t, "artifacts/"+runID+"/report,v1;x.tar.gz", urlKey(t, out.Request.URL))

	rec = doStream(t, s, http.MethodGet, token, "/api/v1/runs/"+runID+"/objects/artifacts/"+url.PathEscape("report,v1;x"), nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "data", rec.Body.String())
}

func TestObjectStream_CacheGetSkipsMetaWithoutArchive(t *testing.T) {
	s, st, fake := presignTestServer(t, Config{})
	token := issueAgentAccessForTest(t, st, "agent-a", nil, nil)
	runID := seedClaimedRun(t, st, "job-a", "agent-a", "")
	for _, k := range []string{"deps-exact", "deps-older"} {
		rec := doStream(t, s, http.MethodPut, token, "/api/v1/runs/"+runID+"/objects/caches?key="+k, strings.NewReader("body-"+k))
		require.Equal(t, http.StatusNoContent, rec.Code)
	}
	// The exact entry keeps its .meta but loses its archive.
	require.NoError(t, fake.Delete(t.Context(), cache.DataKey("job-a", "deps-exact")))

	q := url.Values{"key": {"deps-exact"}, "restoreKey": {"deps-"}}
	rec := doStream(t, s, http.MethodGet, token, "/api/v1/runs/"+runID+"/objects/caches?"+q.Encode(), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "deps-older", rec.Header().Get("X-Unified-Cache-Key"))
	assert.Equal(t, "body-deps-older", rec.Body.String())
	assert.Equal(t, "15", rec.Header().Get("Content-Length"))
}

func TestObjectStream_DenialsAuditedAsStream(t *testing.T) {
	s, st, _ := presignTestServer(t, Config{})
	_ = issueAgentAccessForTest(t, st, "agent-a", nil, nil)
	other := issueAgentAccessForTest(t, st, "agent-b", nil, nil)
	runID := seedClaimedRun(t, st, "job-a", "agent-a", "")

	rec := doStream(t, s, http.MethodGet, other, "/api/v1/runs/"+runID+"/objects/artifacts/a", nil)
	requireObjectError(t, rec, http.StatusForbidden, "forbidden")
	rows, err := st.ListAuditLogs(t.Context(), 1000, 0)
	require.NoError(t, err)
	var stream, presign int
	for _, row := range rows {
		switch row.Action {
		case "objects.stream":
			stream++
			assert.Equal(t, http.StatusForbidden, row.Status)
		case "objects.presign":
			presign++
		}
	}
	assert.Equal(t, 1, stream)
	assert.Equal(t, 0, presign)
}

// failingGetStore serves an archive whose reader fails after a few bytes.
type failingGetStore struct {
	*fakePresigner
}

type failAfter struct {
	r    io.Reader
	left int
}

func (f *failAfter) Read(p []byte) (int, error) {
	if f.left <= 0 {
		return 0, errors.New("store read failed")
	}
	if len(p) > f.left {
		p = p[:f.left]
	}
	n, err := f.r.Read(p)
	f.left -= n
	return n, err
}
func (f *failAfter) Close() error { return nil }

func (f failingGetStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := f.fakePresigner.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	return &failAfter{r: rc, left: 4}, nil
}

func TestObjectStream_StoreReadFailureAbortsDownload(t *testing.T) {
	s, st := newTestServerWithConfig(t, Config{})
	fake := newFakePresigner()
	s.SetObjectStore(failingGetStore{fake})
	token := issueAgentAccessForTest(t, st, "agent-a", nil, nil)
	runID := seedClaimedRun(t, st, "job-a", "agent-a", "")
	require.NoError(t, fake.Put(t.Context(), "artifacts/"+runID+"/a.tar.gz", strings.NewReader("0123456789"), 10))

	ts := httptest.NewServer(s.Router())
	t.Cleanup(ts.Close)
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/runs/"+runID+"/objects/artifacts/a", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	// The small response is still buffered when the copy fails, so the
	// connection is reset before or while the body is read; either way the
	// client must see an error and never a complete-looking download.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	assert.Equal(t, int64(10), resp.ContentLength, "the known size is declared")
	_, err = io.ReadAll(resp.Body)
	require.Error(t, err, "a truncated download must not look complete")
}
