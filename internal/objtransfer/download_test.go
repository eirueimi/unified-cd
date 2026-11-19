package objtransfer

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/objectstore"
)

// cutFirstGet makes the first un-ranged GET send cut bytes of the object and
// then drop the connection.
func cutFirstGet(e *fakeEnv, cut int) {
	cutDone := false
	e.s3Hook = func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.Method != http.MethodGet || r.Header.Get("Range") != "" || cutDone {
			return false
		}
		cutDone = true
		obj := e.objects[strings.TrimPrefix(r.URL.Path, "/")]
		w.Header().Set("Content-Length", strconv.Itoa(len(obj)))
		_, _ = w.Write(obj[:cut])
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}
}

func TestResumingReader_RangeResumeAfterDrop(t *testing.T) {
	noSleep(t)
	e := newFakeEnv(t)
	data := []byte("01234567890123456789")
	e.objects[testKey] = data
	cutFirstGet(e, 10)

	rc, err := e.client.Artifacts().Get(t.Context(), testKey)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got := readAll(t, rc)
	if !bytes.Equal(got, data) {
		t.Fatalf("read %q, want %q", got, data)
	}
	if n := len(e.requestsOf(api.ObjectOpGet)); n != 2 {
		t.Fatalf("get presigns = %d, want 2 (the resume presigns again)", n)
	}
	// The dropped GET is answered by the hook and not recorded; the resumed one is.
	if len(e.gets) != 1 {
		t.Fatalf("recorded store GETs = %d, want 1", len(e.gets))
	}
	h := e.gets[0]
	if h.Get("Range") != "bytes=10-" || h.Get("If-Match") != `"`+etagOf(data)+`"` {
		t.Fatalf("resume headers: Range=%q If-Match=%q", h.Get("Range"), h.Get("If-Match"))
	}
}

func TestResumingReader_ObjectReplacedMidDownloadFails(t *testing.T) {
	noSleep(t)
	e := newFakeEnv(t)
	e.objects[testKey] = []byte("01234567890123456789")
	cutFirstGet(e, 10)
	cut := e.s3Hook
	e.s3Hook = func(w http.ResponseWriter, r *http.Request, b []byte) bool {
		if r.Header.Get("Range") != "" {
			w.WriteHeader(http.StatusPreconditionFailed)
			return true
		}
		return cut(w, r, b)
	}
	rc, err := e.client.Artifacts().Get(t.Context(), testKey)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	_, err = io.ReadAll(rc)
	if err == nil || !strings.Contains(err.Error(), "changed during download") {
		t.Fatalf("err = %v, want one containing %q", err, "changed during download")
	}
}

func TestResumingReader_ETagChangeOnRepresignFails(t *testing.T) {
	noSleep(t)
	e := newFakeEnv(t)
	e.objects[testKey] = []byte("01234567890123456789")
	cutFirstGet(e, 10)
	// Replace the object right before the resume presigns again.
	n := 0
	e.ctrlHook = func(_ int, req api.PresignRequest, _ http.ResponseWriter) bool {
		if req.Op == api.ObjectOpGet {
			n++
			if n == 2 {
				e.mu.Lock()
				e.objects[testKey] = []byte("ABCDEFGHIJABCDEFGHIJ")
				e.mu.Unlock()
			}
		}
		return false
	}
	rc, err := e.client.Artifacts().Get(t.Context(), testKey)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	_, err = io.ReadAll(rc)
	if err == nil || !strings.Contains(err.Error(), "changed during download") {
		t.Fatalf("err = %v", err)
	}
}

func TestResumingReader_GivesUpAfterRepeatedDrops(t *testing.T) {
	noSleep(t)
	e := newFakeEnv(t)
	e.objects[testKey] = []byte("01234567890123456789")
	e.s3Hook = func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		w.Header().Set("Content-Length", "20")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}
	rc, err := e.client.Artifacts().Get(t.Context(), testKey)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if _, err := io.ReadAll(rc); err == nil {
		t.Fatal("want an error")
	} else {
		assertNoSecret(t, err)
	}
}

// failRanged makes the first n ranged GETs fail with fail (a status or, for
// 0, a dropped connection).
func failRanged(e *fakeEnv, n int, status int) *int {
	count := 0
	inner := e.s3Hook
	e.s3Hook = func(w http.ResponseWriter, r *http.Request, b []byte) bool {
		if r.Header.Get("Range") != "" && count < n {
			count++
			if status == 0 {
				panic(http.ErrAbortHandler)
			}
			w.WriteHeader(status)
			return true
		}
		return inner(w, r, b)
	}
	return &count
}

func TestResumingReader_ResumedGetRetriesTransientFailures(t *testing.T) {
	for name, status := range map[string]int{"transport": 0, "503": 503, "429": 429, "403": 403} {
		t.Run(name, func(t *testing.T) {
			noSleep(t)
			e := newFakeEnv(t)
			data := []byte("01234567890123456789")
			e.objects[testKey] = data
			cutFirstGet(e, 10)
			failed := failRanged(e, 1, status)
			rc, err := e.client.Artifacts().Get(t.Context(), testKey)
			if err != nil {
				t.Fatal(err)
			}
			defer rc.Close()
			if got := readAll(t, rc); !bytes.Equal(got, data) {
				t.Fatalf("read %q", got)
			}
			if *failed != 1 {
				t.Fatalf("failures injected = %d", *failed)
			}
		})
	}
}

func TestOpenDownload_FirstGetRetriesOnceOn500(t *testing.T) {
	sleeps := noSleep(t)
	e := newFakeEnv(t)
	data := []byte("hello")
	e.objects[testKey] = data
	failed := false
	e.s3Hook = func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if !failed {
			failed = true
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		return false
	}
	rc, err := e.client.Artifacts().Get(t.Context(), testKey)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if got := readAll(t, rc); !bytes.Equal(got, data) {
		t.Fatalf("read %q", got)
	}
	if len(*sleeps) != 1 || (*sleeps)[0] != 3*time.Second {
		t.Fatalf("sleeps = %v, want the Retry-After of 3s", *sleeps)
	}
	if n := len(e.requestsOf(api.ObjectOpGet)); n != 2 {
		t.Fatalf("get presigns = %d, want 2", n)
	}
}

func TestOpenDownload_FirstGet404IsFatal(t *testing.T) {
	sleeps := noSleep(t)
	e := newFakeEnv(t)
	e.objects[testKey] = []byte("hello")
	hits := 0
	e.s3Hook = func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		hits++
		w.WriteHeader(http.StatusNotFound)
		return true
	}
	_, err := e.client.Artifacts().Get(t.Context(), testKey)
	if !errors.Is(err, objectstore.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if hits != 1 || len(*sleeps) != 0 {
		t.Fatalf("hits = %d sleeps = %v, want no retry", hits, *sleeps)
	}
}

func TestOpenDownload_FirstGetSendsIfMatch(t *testing.T) {
	noSleep(t)
	e := newFakeEnv(t)
	e.objects[testKey] = []byte("hello")
	// The object is replaced between the controller's stat and the first GET.
	var seen string
	e.s3Hook = func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		seen = r.Header.Get("If-Match")
		w.WriteHeader(http.StatusPreconditionFailed)
		return true
	}
	_, err := e.client.Artifacts().Get(t.Context(), testKey)
	if err == nil || !strings.Contains(err.Error(), "changed during download") {
		t.Fatalf("err = %v", err)
	}
	if seen != `"`+etagOf([]byte("hello"))+`"` {
		t.Fatalf("If-Match = %q", seen)
	}
}
