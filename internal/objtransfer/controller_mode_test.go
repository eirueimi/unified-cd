package objtransfer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/cache"
	"github.com/eirueimi/unified-cd/internal/objectstore"
)

// streamPut is one upload the fake controller received on a streaming route.
type streamPut struct {
	path      string
	query     url.Values
	body      []byte
	chunked   bool
	declLen   int64
	hasAuth   bool
	committed bool
}

// streamEnv is a fake controller that answers the presign endpoint (with a
// configurable answer) and implements the streaming routes in memory. There is
// no store URL at all: a request to anything but the controller cannot happen.
type streamEnv struct {
	t      *testing.T
	srv    *httptest.Server
	client *Client

	mu       sync.Mutex
	presigns []api.PresignRequest
	puts     []streamPut
	objects  map[string][]byte // path+key -> body
	// presignAnswer answers every presign request; nil means mode controller.
	presignAnswer func(n int, req api.PresignRequest, w http.ResponseWriter) bool
}

func newStreamEnv(t *testing.T) *streamEnv {
	t.Helper()
	e := &streamEnv{t: t, objects: map[string][]byte{}}
	e.srv = httptest.NewServer(http.HandlerFunc(e.serve))
	t.Cleanup(e.srv.Close)
	e.client = &Client{
		BaseURL: e.srv.URL, RunID: "run-1", TempDir: t.TempDir(),
		Auth: func(_ context.Context, r *http.Request) error {
			r.Header.Set("Authorization", "Bearer test-token")
			return nil
		},
	}
	return e
}

func (e *streamEnv) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer test-token" {
		writeErr(w, http.StatusUnauthorized, api.ObjectErrUnauthenticated)
		return
	}
	const base = "/api/v1/runs/run-1/objects/"
	switch {
	case r.URL.Path == base+"presign":
		var req api.PresignRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		e.mu.Lock()
		e.presigns = append(e.presigns, req)
		n := len(e.presigns)
		e.mu.Unlock()
		if e.presignAnswer != nil && e.presignAnswer(n, req, w) {
			return
		}
		writeJSONStatus(w, http.StatusOK, api.PresignResponse{Mode: api.ObjectModeController})
	case strings.HasPrefix(r.URL.Path, base+"artifacts/") || r.URL.Path == base+"caches":
		id := strings.TrimPrefix(r.URL.Path, base)
		if id == "caches" {
			id += "/" + r.URL.Query().Get("key")
		}
		switch r.Method {
		case http.MethodPut:
			data, _ := io.ReadAll(r.Body)
			chunked := len(r.TransferEncoding) > 0 && r.TransferEncoding[0] == "chunked"
			e.mu.Lock()
			e.objects[id] = data
			e.puts = append(e.puts, streamPut{path: r.URL.Path, query: r.URL.Query(), body: data, chunked: chunked,
				declLen: r.ContentLength, hasAuth: r.Header.Get("Authorization") != ""})
			e.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case http.MethodGet:
			e.mu.Lock()
			data, ok := e.objects[id]
			e.mu.Unlock()
			if !ok {
				code := api.ObjectErrNotFound
				if strings.HasPrefix(id, "caches/") {
					code = api.ObjectErrCacheMiss
				}
				writeErr(w, http.StatusNotFound, code)
				return
			}
			if strings.HasPrefix(id, "caches/") {
				w.Header().Set(cacheKeyHeader, url.PathEscape(r.URL.Query().Get("key")))
			}
			_, _ = w.Write(data)
		}
	default:
		http.NotFound(w, r)
	}
}

func (e *streamEnv) presignOps() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, p := range e.presigns {
		out = append(out, p.Kind+"/"+p.Op)
	}
	return out
}

func TestControllerMode_ForcedSkipsPresign(t *testing.T) {
	e := newStreamEnv(t)
	e.client.ForceController = true

	if err := e.client.Artifacts().Put(t.Context(), "artifacts/run-1/a.tar.gz", strings.NewReader("hello"), 5); err != nil {
		t.Fatal(err)
	}
	rc, err := e.client.Artifacts().Get(t.Context(), "artifacts/run-1/a.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "hello" {
		t.Fatalf("artifact = %q", got)
	}
	if err := e.client.Caches().Save(t.Context(), "deps-v1", 9, strings.NewReader("archive")); err != nil {
		t.Fatal(err)
	}
	rc, matched, err := e.client.Caches().Open(t.Context(), "deps-v1", []string{"deps-"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(rc)
	rc.Close()
	if string(got) != "archive" || matched != "deps-v1" {
		t.Fatalf("cache = %q matched %q", got, matched)
	}
	if _, _, err := e.client.Caches().Open(t.Context(), "nope", nil); err == nil || !isCacheMiss(err) {
		t.Fatalf("miss: err = %v, want cache.ErrCacheMiss", err)
	}
	if _, err := e.client.Artifacts().Get(t.Context(), "artifacts/run-1/zzz.tar.gz"); !isNotFound(err) {
		t.Fatalf("missing artifact: err = %v, want objectstore.ErrNotFound", err)
	}

	if ops := e.presignOps(); len(ops) != 0 {
		t.Fatalf("ForceController called the presign endpoint: %v", ops)
	}
	// The cache upload carried its TTL and needed no separate commit.
	var cachePut streamPut
	for _, p := range e.puts {
		if strings.HasSuffix(p.path, "/caches") {
			cachePut = p
		}
	}
	if cachePut.query.Get("ttlDays") != "9" || cachePut.query.Get("key") != "deps-v1" {
		t.Fatalf("cache put query = %v", cachePut.query)
	}
	if !cachePut.chunked || !cachePut.hasAuth {
		t.Fatalf("cache put chunked=%v auth=%v, want a chunked authenticated PUT", cachePut.chunked, cachePut.hasAuth)
	}
}

func TestControllerMode_ControllerAnswerIsRemembered(t *testing.T) {
	e := newStreamEnv(t)
	if err := e.client.Artifacts().Put(t.Context(), "artifacts/run-1/a.tar.gz", strings.NewReader("one"), 3); err != nil {
		t.Fatal(err)
	}
	rc, err := e.client.Artifacts().Get(t.Context(), "artifacts/run-1/a.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if err := e.client.Caches().Save(t.Context(), "k", 1, strings.NewReader("two")); err != nil {
		t.Fatal(err)
	}
	// Only the very first transfer asked; its answer was remembered.
	if ops := e.presignOps(); len(ops) != 1 || ops[0] != "artifact/put" {
		t.Fatalf("presign calls = %v, want exactly [artifact/put]", ops)
	}
}

func TestControllerMode_SinglePutReplaysBufferedStream(t *testing.T) {
	tinyParts(t, 64, 1000)
	e := newStreamEnv(t)
	// The whole stream fits in part 1; the controller answers controller mode
	// only when the client asks for the single PUT URL.
	data := bytes.Repeat([]byte("0123456789"), 5) // 50 bytes < 64
	if err := e.client.Artifacts().Put(t.Context(), "artifacts/run-1/a.tar.gz", bytes.NewReader(data), -1); err != nil {
		t.Fatal(err)
	}
	if ops := e.presignOps(); len(ops) != 1 || ops[0] != "artifact/put" {
		t.Fatalf("presign calls = %v", ops)
	}
	if len(e.puts) != 1 || !bytes.Equal(e.puts[0].body, data) {
		t.Fatalf("controller received %d bytes in %d puts, want the %d buffered bytes once", totalBytes(e.puts), len(e.puts), len(data))
	}
}

func TestControllerMode_MultipartStartReplaysPartPlusRemainder(t *testing.T) {
	tinyParts(t, 16, 1000)
	e := newStreamEnv(t)
	// A stream larger than part 1: part 1 is buffered in the temp file, the
	// rest is still unread when multipart-start answers controller mode.
	data := make([]byte, 100)
	for i := range data {
		data[i] = byte('a' + i%26)
	}
	if err := e.client.Caches().Save(t.Context(), "k", 4, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if ops := e.presignOps(); len(ops) != 1 || ops[0] != "cache/multipart-start" {
		t.Fatalf("presign calls = %v, want [cache/multipart-start]", ops)
	}
	if len(e.puts) != 1 {
		t.Fatalf("controller received %d puts, want 1", len(e.puts))
	}
	if !bytes.Equal(e.puts[0].body, data) {
		t.Fatalf("controller received %d bytes that differ from the %d sent (lost or duplicated data)", len(e.puts[0].body), len(data))
	}
	if e.puts[0].query.Get("ttlDays") != "4" {
		t.Fatalf("ttlDays = %q", e.puts[0].query.Get("ttlDays"))
	}
}

func TestControllerMode_MultipartUnavailableThenControllerReplaysSpooledStream(t *testing.T) {
	tinyParts(t, 16, 1000)
	e := newStreamEnv(t)
	e.presignAnswer = func(_ int, req api.PresignRequest, w http.ResponseWriter) bool {
		switch req.Op {
		case api.ObjectOpMultipartStart:
			writeErr(w, http.StatusNotImplemented, api.ObjectErrMultipartUnavailable)
			return true
		}
		return false // put: answer controller mode
	}
	data := bytes.Repeat([]byte("xyz"), 40)
	if err := e.client.Artifacts().Put(t.Context(), "artifacts/run-1/a.tar.gz", bytes.NewReader(data), -1); err != nil {
		t.Fatal(err)
	}
	if len(e.puts) != 1 || !bytes.Equal(e.puts[0].body, data) {
		t.Fatalf("controller received %d bytes in %d puts, want %d once", totalBytes(e.puts), len(e.puts), len(data))
	}
}

func totalBytes(puts []streamPut) int {
	n := 0
	for _, p := range puts {
		n += len(p.body)
	}
	return n
}

func isCacheMiss(err error) bool { return err != nil && errors.Is(err, cache.ErrCacheMiss) }
func isNotFound(err error) bool  { return err != nil && errors.Is(err, objectstore.ErrNotFound) }
