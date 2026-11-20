package objtransfer

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/cache"
	"github.com/eirueimi/unified-cd/internal/objectstore"
)

// secretQuery marks every presigned URL the fake controller issues; it must
// never show up in an error.
const secretQuery = "sig=SECRET"

type putRecord struct {
	path          string
	query         string
	contentLength int64
	body          []byte
}

// fakeEnv is a fake controller (the presign endpoint) and a fake S3 on
// httptest. The fake S3 enforces method and path, counts requests, and answers
// an ETag per part.
type fakeEnv struct {
	t      *testing.T
	client *Client
	ctrl   *httptest.Server
	s3     *httptest.Server

	mu         sync.Mutex
	reqs       []api.PresignRequest
	puts       []putRecord
	gets       []http.Header
	objects    map[string][]byte
	parts      map[string]map[int][]byte
	completed  []api.ObjectPart
	ctrlHook   func(n int, req api.PresignRequest, w http.ResponseWriter) bool
	s3Hook     func(w http.ResponseWriter, r *http.Request, body []byte) bool
	noMultiple bool
	mode       string
	s3Auth     []string
}

func newFakeEnv(t *testing.T) *fakeEnv {
	t.Helper()
	e := &fakeEnv{t: t, objects: map[string][]byte{}, parts: map[string]map[int][]byte{}}
	e.s3 = httptest.NewServer(http.HandlerFunc(e.serveS3))
	e.ctrl = httptest.NewServer(http.HandlerFunc(e.serveCtrl))
	t.Cleanup(e.ctrl.Close)
	t.Cleanup(e.s3.Close)
	e.client = &Client{
		BaseURL: e.ctrl.URL,
		RunID:   "run-1",
		Auth: func(_ context.Context, r *http.Request) error {
			r.Header.Set("Authorization", "Bearer test-token")
			return nil
		},
		TempDir: t.TempDir(),
	}
	return e
}

// noSleep replaces the retry sleep for one test and records the delays.
func noSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var mu sync.Mutex
	var got []time.Duration
	old := sleep
	sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		got = append(got, d)
		mu.Unlock()
		return ctx.Err()
	}
	t.Cleanup(func() { sleep = old })
	return &got
}

// tinyParts makes parts of min bytes doubling every period parts.
func tinyParts(t *testing.T, min int64, period int) {
	t.Helper()
	oldMin, oldPeriod := minPartSize, partDoublingEvery
	minPartSize, partDoublingEvery = min, period
	t.Cleanup(func() { minPartSize, partDoublingEvery = oldMin, oldPeriod })
}

func etagOf(b []byte) string {
	s := md5.Sum(b)
	return hex.EncodeToString(s[:])
}

func (e *fakeEnv) keyFor(req api.PresignRequest) string {
	if req.Kind == api.ObjectKindCache {
		return "caches/" + req.Key
	}
	run := "run-1"
	if req.SourceRunID != "" {
		run = req.SourceRunID
	}
	return "artifacts/" + run + "/" + req.Name + ".tar.gz"
}

func (e *fakeEnv) url(path, query string) string {
	u := e.s3.URL + "/" + path
	if query != "" {
		u += "?" + query + "&" + secretQuery
	} else {
		u += "?" + secretQuery
	}
	return u
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code string) {
	writeJSONStatus(w, status, api.ObjectError{Error: "fake " + code, Code: code})
}

func (e *fakeEnv) serveCtrl(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer test-token" {
		writeErr(w, http.StatusUnauthorized, api.ObjectErrUnauthenticated)
		return
	}
	if r.URL.Path != "/api/v1/runs/run-1/objects/presign" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	var req api.PresignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, api.ObjectErrInvalid)
		return
	}
	e.mu.Lock()
	e.reqs = append(e.reqs, req)
	n := len(e.reqs)
	hook := e.ctrlHook
	mode := e.mode
	e.mu.Unlock()
	if hook != nil && hook(n, req, w) {
		return
	}
	if mode != "" {
		writeJSONStatus(w, http.StatusOK, api.PresignResponse{Mode: mode})
		return
	}
	key := e.keyFor(req)
	expires := time.Now().Add(15 * time.Minute)
	resp := api.PresignResponse{Mode: api.ObjectModeDirect}
	switch req.Op {
	case api.ObjectOpGet, api.ObjectOpHead:
		e.mu.Lock()
		obj, ok := e.objects[key]
		e.mu.Unlock()
		if !ok {
			code := api.ObjectErrNotFound
			if req.Kind == api.ObjectKindCache {
				code = api.ObjectErrCacheMiss
			}
			writeErr(w, http.StatusNotFound, code)
			return
		}
		resp.Request = &api.PresignedRequest{Method: http.MethodGet, URL: e.url(key, ""), ExpiresAt: expires}
		resp.Size, resp.ETag, resp.MatchedKey = int64(len(obj)), etagOf(obj), req.Key
	case api.ObjectOpPut:
		resp.Request = &api.PresignedRequest{Method: http.MethodPut, URL: e.url(key, ""), ExpiresAt: expires,
			Headers: map[string]string{"X-Fake-Header": "as-is"}}
	case api.ObjectOpMultipartStart:
		if e.noMultiple {
			writeErr(w, http.StatusNotImplemented, api.ObjectErrMultipartUnavailable)
			return
		}
		resp.UploadID = "upload-1"
	case api.ObjectOpMultipartParts:
		for _, p := range req.PartNumbers {
			resp.Parts = append(resp.Parts, api.PresignedRequest{
				Method: http.MethodPut, PartNumber: p, ExpiresAt: expires,
				URL: e.url(key, fmt.Sprintf("uploadId=%s&partNumber=%d", req.UploadID, p)),
			})
		}
	case api.ObjectOpMultipartComplete:
		e.mu.Lock()
		var all []byte
		for _, p := range req.Parts {
			data, ok := e.parts[req.UploadID][p.PartNumber]
			if !ok || etagOf(data) != strings.Trim(p.ETag, `"`) {
				e.mu.Unlock()
				writeErr(w, http.StatusBadRequest, api.ObjectErrInvalid)
				return
			}
			all = append(all, data...)
		}
		e.objects[key] = all
		e.completed = req.Parts
		e.mu.Unlock()
	case api.ObjectOpMultipartAbort, api.ObjectOpCommit:
	}
	writeJSONStatus(w, http.StatusOK, resp)
}

func (e *fakeEnv) serveS3(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	e.s3Auth = append(e.s3Auth, r.Header.Get("Authorization"))
	e.mu.Unlock()
	if r.URL.Query().Get("sig") != "SECRET" {
		http.Error(w, "bad signature", http.StatusForbidden)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/")
	body, _ := io.ReadAll(r.Body)
	e.mu.Lock()
	hook := e.s3Hook
	e.mu.Unlock()
	if hook != nil && hook(w, r, body) {
		return
	}
	switch r.Method {
	case http.MethodPut:
		e.mu.Lock()
		e.puts = append(e.puts, putRecord{path: key, query: r.URL.RawQuery, contentLength: r.ContentLength, body: body})
		if up := r.URL.Query().Get("uploadId"); up != "" {
			n, _ := strconv.Atoi(r.URL.Query().Get("partNumber"))
			if e.parts[up] == nil {
				e.parts[up] = map[int][]byte{}
			}
			e.parts[up][n] = body
		} else {
			e.objects[key] = body
		}
		e.mu.Unlock()
		w.Header().Set("ETag", `"`+etagOf(body)+`"`)
	case http.MethodGet:
		e.mu.Lock()
		obj, ok := e.objects[key]
		e.gets = append(e.gets, r.Header.Clone())
		e.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		if im := r.Header.Get("If-Match"); im != "" && im != `"`+etagOf(obj)+`"` {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		if rg := r.Header.Get("Range"); rg != "" {
			var from int
			if _, err := fmt.Sscanf(rg, "bytes=%d-", &from); err != nil {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, len(obj)-1, len(obj)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(obj[from:])
			return
		}
		_, _ = w.Write(obj)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// ops lists the presign requests as "kind/op", in order.
func (e *fakeEnv) ops() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, r := range e.reqs {
		out = append(out, r.Kind+"/"+r.Op)
	}
	return out
}

func (e *fakeEnv) requestsOf(op string) []api.PresignRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []api.PresignRequest
	for _, r := range e.reqs {
		if r.Op == op {
			out = append(out, r)
		}
	}
	return out
}

func slicesEqual(a, b []string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

func TestPartSize(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want int64
	}{
		{1, 64 << 20}, {1000, 64 << 20}, {1001, 128 << 20}, {2001, 256 << 20}, {9001, 5 << 30}, {10000, 5 << 30},
	} {
		if got := PartSize(tc.n); got != tc.want {
			t.Errorf("PartSize(%d) = %d, want %d", tc.n, got, tc.want)
		}
	}
}

func TestClient_RetriesBindingPendingUpTo30s(t *testing.T) {
	if bindingWait != 30*time.Second {
		t.Fatalf("bindingWait = %s, want 30s", bindingWait)
	}
	sleeps := noSleep(t)
	e := newFakeEnv(t)
	e.objects["artifacts/run-1/a.tar.gz"] = []byte("x")
	e.ctrlHook = func(n int, _ api.PresignRequest, w http.ResponseWriter) bool {
		if n <= 3 {
			w.Header().Set("Retry-After", "1")
			writeErr(w, http.StatusConflict, api.ObjectErrBindingPending)
			return true
		}
		return false
	}
	rc, err := e.client.Artifacts().Get(t.Context(), "artifacts/run-1/a.tar.gz")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	rc.Close()
	if len(*sleeps) != 3 || (*sleeps)[0] != time.Second {
		t.Fatalf("sleeps = %v, want three 1s waits", *sleeps)
	}

	// A binding that never appears fails once bindingWait is used up.
	old := bindingWait
	bindingWait = 3 * time.Second
	t.Cleanup(func() { bindingWait = old })
	e.ctrlHook = func(_ int, _ api.PresignRequest, w http.ResponseWriter) bool {
		w.Header().Set("Retry-After", "1")
		writeErr(w, http.StatusConflict, api.ObjectErrBindingPending)
		return true
	}
	_, err = e.client.Artifacts().Get(t.Context(), "artifacts/run-1/a.tar.gz")
	if err == nil || !strings.Contains(err.Error(), api.ObjectErrBindingPending) {
		t.Fatalf("err = %v, want one naming binding-pending", err)
	}
}

func TestClient_RunFinishedIsFinalNotRetried(t *testing.T) {
	sleeps := noSleep(t)
	e := newFakeEnv(t)
	e.ctrlHook = func(_ int, _ api.PresignRequest, w http.ResponseWriter) bool {
		writeErr(w, http.StatusConflict, api.ObjectErrRunFinished)
		return true
	}
	_, err := e.client.Artifacts().Get(t.Context(), "artifacts/run-1/a.tar.gz")
	if err == nil || !strings.Contains(err.Error(), api.ObjectErrRunFinished) {
		t.Fatalf("err = %v, want one naming run-finished", err)
	}
	if len(*sleeps) != 0 {
		t.Fatalf("sleeps = %v, want no retries", *sleeps)
	}
}

func TestClient_Retries429WithRetryAfter(t *testing.T) {
	sleeps := noSleep(t)
	e := newFakeEnv(t)
	e.objects["artifacts/run-1/a.tar.gz"] = []byte("x")
	e.ctrlHook = func(n int, _ api.PresignRequest, w http.ResponseWriter) bool {
		if n <= 2 {
			w.Header().Set("Retry-After", "2")
			writeErr(w, http.StatusTooManyRequests, api.ObjectErrRateLimited)
			return true
		}
		return false
	}
	rc, err := e.client.Artifacts().Get(t.Context(), "artifacts/run-1/a.tar.gz")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	rc.Close()
	if len(*sleeps) != 2 || (*sleeps)[0] != 2*time.Second || (*sleeps)[1] != 2*time.Second {
		t.Fatalf("sleeps = %v, want [2s 2s]", *sleeps)
	}
}

func TestClient_Retries503UnavailableWithBackoff(t *testing.T) {
	sleeps := noSleep(t)
	e := newFakeEnv(t)
	e.objects["artifacts/run-1/a.tar.gz"] = []byte("x")
	e.ctrlHook = func(n int, _ api.PresignRequest, w http.ResponseWriter) bool {
		if n <= 2 {
			writeErr(w, http.StatusServiceUnavailable, api.ObjectErrUnavailable)
			return true
		}
		return false
	}
	rc, err := e.client.Artifacts().Get(t.Context(), "artifacts/run-1/a.tar.gz")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	rc.Close()
	if len(*sleeps) != 2 || (*sleeps)[1] <= (*sleeps)[0] {
		t.Fatalf("sleeps = %v, want growing backoff", *sleeps)
	}
}

func TestClient_FatalStatusesAreNotRetried(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{
		{400, api.ObjectErrInvalid},
		{403, api.ObjectErrForbidden},
		{500, api.ObjectErrInternal},
		{503, api.ObjectErrNoObjectStore},
	} {
		t.Run(tc.code, func(t *testing.T) {
			sleeps := noSleep(t)
			e := newFakeEnv(t)
			e.ctrlHook = func(_ int, _ api.PresignRequest, w http.ResponseWriter) bool {
				writeErr(w, tc.status, tc.code)
				return true
			}
			_, err := e.client.Artifacts().Get(t.Context(), "artifacts/run-1/a.tar.gz")
			if err == nil || !strings.Contains(err.Error(), tc.code) {
				t.Fatalf("err = %v, want one naming %s", err, tc.code)
			}
			if len(e.ops()) != 1 || len(*sleeps) != 0 {
				t.Fatalf("ops = %v sleeps = %v, want a single attempt", e.ops(), *sleeps)
			}
		})
	}
}

func TestClient_AuthOnlyOnControllerRequests(t *testing.T) {
	e := newFakeEnv(t)
	if err := e.client.Artifacts().Put(t.Context(), "artifacts/run-1/a.tar.gz", strings.NewReader("hello"), 5); err != nil {
		t.Fatal(err)
	}
	rc, err := e.client.Artifacts().Get(t.Context(), "artifacts/run-1/a.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	for _, a := range e.s3Auth {
		if a != "" {
			t.Fatalf("a store request carried controller credentials: %q", a)
		}
	}
	if len(e.s3Auth) == 0 {
		t.Fatal("no store request was made")
	}
}

func TestErrorsNeverContainPresignedQuery(t *testing.T) {
	noSleep(t)
	e := newFakeEnv(t)
	e.s3Hook = func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		w.WriteHeader(http.StatusForbidden)
		return true
	}
	err := e.client.Artifacts().Put(t.Context(), "artifacts/run-1/a.tar.gz", strings.NewReader("hello"), 5)
	if err == nil {
		t.Fatal("want an error")
	}
	assertNoSecret(t, err)

	// A transport error from net/http carries the full URL.
	e.s3.Close()
	err = e.client.Artifacts().Put(t.Context(), "artifacts/run-1/b.tar.gz", strings.NewReader("hello"), 5)
	if err == nil {
		t.Fatal("want an error")
	}
	assertNoSecret(t, err)
}

func assertNoSecret(t *testing.T, err error) {
	t.Helper()
	if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "?") {
		t.Fatalf("error leaks a presigned URL: %v", err)
	}
}

func TestCaches_MissIsErrCacheMiss(t *testing.T) {
	e := newFakeEnv(t)
	_, _, err := e.client.Caches().Open(t.Context(), "k", []string{"k-"})
	if !errors.Is(err, cache.ErrCacheMiss) {
		t.Fatalf("err = %v, want ErrCacheMiss", err)
	}
}

func TestCaches_OpenReturnsMatchedKey(t *testing.T) {
	e := newFakeEnv(t)
	e.objects["caches/k-1"] = []byte("cached")
	rc, matched, err := e.client.Caches().Open(t.Context(), "k-1", []string{"k-"})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if string(b) != "cached" || matched != "k-1" {
		t.Fatalf("got %q matched %q", b, matched)
	}
	got := e.requestsOf(api.ObjectOpGet)[0]
	if got.Key != "k-1" || len(got.RestoreKeys) != 1 || got.RestoreKeys[0] != "k-" {
		t.Fatalf("request = %+v", got)
	}
}

func TestArtifacts_GetNotFoundIsErrNotFound(t *testing.T) {
	e := newFakeEnv(t)
	_, err := e.client.Artifacts().Get(t.Context(), "artifacts/run-1/missing.tar.gz")
	if !errors.Is(err, objectstore.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestArtifacts_CrossRunAndUnsupported(t *testing.T) {
	e := newFakeEnv(t)
	e.objects["artifacts/run-0/a.tar.gz"] = []byte("other")
	rc, err := e.client.Artifacts().Get(t.Context(), "artifacts/run-0/a.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "other" {
		t.Fatalf("got %q", b)
	}
	if got := e.requestsOf(api.ObjectOpGet)[0]; got.SourceRunID != "run-0" || got.Name != "a" {
		t.Fatalf("request = %+v", got)
	}
	// Own run: no sourceRunId.
	e.objects["artifacts/run-1/a.tar.gz"] = []byte("own")
	rc, err = e.client.Artifacts().Get(t.Context(), "artifacts/run-1/a.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if got := e.requestsOf(api.ObjectOpGet)[1]; got.SourceRunID != "" {
		t.Fatalf("own-run request carries sourceRunId: %+v", got)
	}

	if err := e.client.Artifacts().Put(t.Context(), "artifacts/run-0/a.tar.gz", strings.NewReader("x"), 1); err == nil {
		t.Fatal("Put into another run must fail")
	}
	if err := e.client.Artifacts().Put(t.Context(), "weird/key", strings.NewReader("x"), 1); err == nil {
		t.Fatal("Put of a non-artifact key must fail")
	}
	if err := e.client.Artifacts().Delete(t.Context(), "artifacts/run-1/a.tar.gz"); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Delete = %v", err)
	}
	if _, err := e.client.Artifacts().List(t.Context(), ""); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("List = %v", err)
	}
}

func readAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return b
}
