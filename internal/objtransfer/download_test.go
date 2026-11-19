package objtransfer

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
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
