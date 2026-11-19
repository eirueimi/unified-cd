package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/artifact"
	"github.com/eirueimi/unified-cd/internal/cache"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/objtransfer"
)

// defaultPodRun is the run a test client acts for when the args carry no
// --pod-run.
const defaultPodRun = "r1"

// seenRequest is one request the fake controller received.
type seenRequest struct {
	Method, Path, Query, Auth string
	Presign                   *api.PresignRequest
}

// fakeController stands in for the controller. By default it answers every
// presign with mode "controller" and serves the streaming routes from a local
// store, which exercises the whole client through a real HTTP server. With
// direct set it answers presign gets with a URL to its own /blob endpoint.
type fakeController struct {
	srv   *httptest.Server
	store objectstore.ObjectStore
	// jobName is the namespace the cache routes use (the run's job).
	jobName string
	// direct makes presign gets return a presigned URL to blobs[...] instead
	// of mode "controller".
	direct bool
	// blobs serves a direct-mode get: it maps a cache key to the archive.
	blobs map[string][]byte

	mu   sync.Mutex
	seen []seenRequest
}

func newFakeController(t *testing.T, store objectstore.ObjectStore) *fakeController {
	t.Helper()
	f := &fakeController{store: store, jobName: "test-job", blobs: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// requests returns a copy of what the fake has seen.
func (f *fakeController) requests() []seenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seenRequest(nil), f.seen...)
}

func (f *fakeController) serve(w http.ResponseWriter, r *http.Request) {
	rec := seenRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Auth: r.Header.Get("Authorization")}
	var presign *api.PresignRequest
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/objects/presign") {
		presign = new(api.PresignRequest)
		if err := json.NewDecoder(r.Body).Decode(presign); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rec.Presign = presign
	}
	f.mu.Lock()
	f.seen = append(f.seen, rec)
	f.mu.Unlock()

	switch {
	case presign != nil:
		f.presign(w, presign)
	case strings.HasPrefix(r.URL.Path, "/blob/"):
		b, ok := f.blobs[strings.TrimPrefix(r.URL.Path, "/blob/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	case strings.Contains(r.URL.Path, "/objects/artifacts/"):
		f.artifact(w, r)
	case strings.HasSuffix(r.URL.Path, "/objects/caches"):
		f.cache(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeController) presign(w http.ResponseWriter, req *api.PresignRequest) {
	w.Header().Set("Content-Type", "application/json")
	if f.direct && req.Op == api.ObjectOpGet && req.Kind == api.ObjectKindCache {
		blob, ok := f.blobs[req.Key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(api.ObjectError{Error: "no entry", Code: api.ObjectErrCacheMiss})
			return
		}
		_ = json.NewEncoder(w).Encode(api.PresignResponse{
			Mode:       api.ObjectModeDirect,
			Request:    &api.PresignedRequest{Method: http.MethodGet, URL: f.srv.URL + "/blob/" + url.PathEscape(req.Key)},
			MatchedKey: req.Key,
			Size:       int64(len(blob)),
		})
		return
	}
	_ = json.NewEncoder(w).Encode(api.PresignResponse{Mode: api.ObjectModeController})
}

func (f *fakeController) artifact(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/") // "", api, v1, runs, <run>, objects, artifacts, <name>
	if len(parts) < 8 {
		http.NotFound(w, r)
		return
	}
	runID, _ := url.PathUnescape(parts[4])
	name, _ := url.PathUnescape(parts[7])
	if src := r.URL.Query().Get("sourceRunId"); src != "" {
		runID = src
	}
	key, err := artifact.ArtifactKey(runID, name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodPut:
		if err := f.store.Put(r.Context(), key, r.Body, -1); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		rc, err := f.store.Get(r.Context(), key)
		if errors.Is(err, objectstore.ErrNotFound) {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(api.ObjectError{Error: "no such artifact", Code: api.ObjectErrNotFound})
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rc.Close()
		_, _ = io.Copy(w, rc)
	}
}

func (f *fakeController) cache(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	remote := &cache.StoreRemote{Store: f.store, JobName: f.jobName}
	switch r.Method {
	case http.MethodPut:
		ttl := 30
		if v := q.Get("ttlDays"); v != "" {
			ttl = 0
			for _, c := range v {
				ttl = ttl*10 + int(c-'0')
			}
		}
		if err := remote.Save(r.Context(), q.Get("key"), ttl, r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		rc, matched, err := remote.Open(r.Context(), q.Get("key"), q["restoreKey"])
		if errors.Is(err, cache.ErrCacheMiss) {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(api.ObjectError{Error: "cache miss", Code: api.ObjectErrCacheMiss})
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rc.Close()
		w.Header().Set("X-Unified-Cache-Key", url.PathEscape(matched))
		_, _ = io.Copy(w, rc)
	}
}

// provider returns a clientProvider for this fake. The token is what the
// token file holds.
func (f *fakeController) provider(t *testing.T, token string) clientProvider {
	t.Helper()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	return func(runID string) (*objtransfer.Client, error) {
		if runID == "" {
			runID = defaultPodRun
		}
		return &objtransfer.Client{
			BaseURL: f.srv.URL,
			RunID:   runID,
			Auth: func(_ context.Context, r *http.Request) error {
				b, err := os.ReadFile(tokenFile)
				if err != nil {
					return err
				}
				r.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(b)))
				return nil
			},
		}, nil
	}
}

// runCmd runs the sidecar with os.Stdout (where cache markers go) captured
// into stdout.
func runCmd(t *testing.T, prov clientProvider, stdout, stderr io.Writer, args ...string) int {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan struct{})
	go func() { _, _ = io.Copy(stdout, r); close(done) }()
	code := run(context.Background(), prov, args, stderr)
	w.Close()
	os.Stdout = old
	<-done
	return code
}
