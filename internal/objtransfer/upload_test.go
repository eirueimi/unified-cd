package objtransfer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
)

const testKey = "artifacts/run-1/out.tar.gz"

func TestUpload_SmallStreamIsSinglePut(t *testing.T) {
	e := newFakeEnv(t)
	data := bytes.Repeat([]byte("a"), 1024)
	if err := e.client.Artifacts().Put(t.Context(), testKey, bytes.NewReader(data), -1); err != nil {
		t.Fatal(err)
	}
	if got := e.ops(); !slicesEqual(got, []string{"artifact/put"}) {
		t.Fatalf("ops = %v, want one put", got)
	}
	if len(e.puts) != 1 || e.puts[0].contentLength != 1024 {
		t.Fatalf("puts = %+v, want one PUT with Content-Length 1024", e.puts)
	}
	if !bytes.Equal(e.objects[testKey], data) {
		t.Fatal("stored object differs")
	}
}

func TestUpload_SendsPresignedHeadersAsIs(t *testing.T) {
	e := newFakeEnv(t)
	var got string
	e.s3Hook = func(_ http.ResponseWriter, r *http.Request, _ []byte) bool {
		got = r.Header.Get("X-Fake-Header")
		return false
	}
	if err := e.client.Artifacts().Put(t.Context(), testKey, strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}
	if got != "as-is" {
		t.Fatalf("X-Fake-Header = %q", got)
	}
}

func TestUpload_EmptyStream(t *testing.T) {
	e := newFakeEnv(t)
	if err := e.client.Artifacts().Put(t.Context(), testKey, strings.NewReader(""), -1); err != nil {
		t.Fatal(err)
	}
	if len(e.puts) != 1 || e.puts[0].contentLength != 0 {
		t.Fatalf("puts = %+v", e.puts)
	}
}

func TestUpload_StreamEndingExactlyOnPartBoundaryIsSinglePut(t *testing.T) {
	tinyParts(t, 4, 2)
	e := newFakeEnv(t)
	if err := e.client.Artifacts().Put(t.Context(), testKey, strings.NewReader("abcd"), -1); err != nil {
		t.Fatal(err)
	}
	if got := e.ops(); !slicesEqual(got, []string{"artifact/put"}) {
		t.Fatalf("ops = %v", got)
	}
}

func TestUpload_MultipartGrowingParts(t *testing.T) {
	tinyParts(t, 4, 2)
	e := newFakeEnv(t)
	data := []byte("0123456789abcdefghijklmnopqrst") // 30 bytes
	if err := e.client.Artifacts().Put(t.Context(), testKey, bytes.NewReader(data), -1); err != nil {
		t.Fatal(err)
	}
	var sizes []int
	for _, p := range e.puts {
		sizes = append(sizes, len(p.body))
		if int64(len(p.body)) != p.contentLength {
			t.Errorf("part Content-Length %d, body %d", p.contentLength, len(p.body))
		}
	}
	want := []int{4, 4, 8, 8, 6}
	if len(sizes) != len(want) {
		t.Fatalf("part sizes = %v, want %v", sizes, want)
	}
	for i := range want {
		if sizes[i] != want[i] {
			t.Fatalf("part sizes = %v, want %v", sizes, want)
		}
	}
	complete := e.requestsOf(api.ObjectOpMultipartComplete)
	if len(complete) != 1 || len(complete[0].Parts) != 5 {
		t.Fatalf("complete = %+v", complete)
	}
	for i, p := range complete[0].Parts {
		if p.PartNumber != i+1 || strings.Trim(p.ETag, `"`) != etagOf(e.puts[i].body) {
			t.Fatalf("complete part %d = %+v", i, p)
		}
	}
	if !bytes.Equal(e.objects[testKey], data) {
		t.Fatalf("assembled object = %q", e.objects[testKey])
	}
	if len(e.requestsOf(api.ObjectOpPut)) != 0 {
		t.Fatal("multipart upload used a single put")
	}
	if parts := e.requestsOf(api.ObjectOpMultipartParts); len(parts) != 1 || len(parts[0].PartNumbers) != partURLBatch {
		t.Fatalf("part URL requests = %+v, want one batch of %d", parts, partURLBatch)
	}
}

func TestUpload_ReRequestsPartURLOn403(t *testing.T) {
	tinyParts(t, 4, 2)
	noSleep(t)
	e := newFakeEnv(t)
	var mu sync.Mutex
	failed := false
	var part2 [][]byte
	e.s3Hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.URL.Query().Get("partNumber") != "2" {
			return false
		}
		mu.Lock()
		defer mu.Unlock()
		part2 = append(part2, body)
		if !failed {
			failed = true
			w.WriteHeader(http.StatusForbidden)
			return true
		}
		return false
	}
	data := []byte("0123456789abcdefghijklmnopqrst")
	if err := e.client.Artifacts().Put(t.Context(), testKey, bytes.NewReader(data), -1); err != nil {
		t.Fatal(err)
	}
	var refetched bool
	for _, r := range e.requestsOf(api.ObjectOpMultipartParts)[1:] {
		if len(r.PartNumbers) == 1 && r.PartNumbers[0] == 2 {
			refetched = true
		}
	}
	if !refetched {
		t.Fatalf("no multipart-parts request for [2]: %+v", e.requestsOf(api.ObjectOpMultipartParts))
	}
	if len(part2) != 2 || !bytes.Equal(part2[0], part2[1]) || string(part2[0]) != "4567" {
		t.Fatalf("part 2 attempts = %q, want two identical bodies", part2)
	}
	if !bytes.Equal(e.objects[testKey], data) {
		t.Fatal("assembled object differs")
	}
}

type failingReader struct {
	data []byte
	err  error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestUpload_AbortsOnFailure(t *testing.T) {
	tinyParts(t, 4, 2)
	e := newFakeEnv(t)
	boom := errors.New("boom")
	err := e.client.Artifacts().Put(t.Context(), testKey, &failingReader{data: []byte("0123456789"), err: boom}, -1)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if aborts := e.requestsOf(api.ObjectOpMultipartAbort); len(aborts) != 1 || aborts[0].UploadID != "upload-1" {
		t.Fatalf("aborts = %+v", aborts)
	}
	if len(e.requestsOf(api.ObjectOpMultipartComplete)) != 0 {
		t.Fatal("a failed upload was completed")
	}
}

func TestUpload_MultipartUnavailableSpoolsToTempFile(t *testing.T) {
	tinyParts(t, 4, 2)
	e := newFakeEnv(t)
	e.noMultiple = true
	data := []byte("0123456789")
	if err := e.client.Artifacts().Put(t.Context(), testKey, bytes.NewReader(data), -1); err != nil {
		t.Fatal(err)
	}
	if len(e.puts) != 1 || e.puts[0].contentLength != 10 || !bytes.Equal(e.puts[0].body, data) {
		t.Fatalf("puts = %+v, want one PUT of all 10 bytes", e.puts)
	}
	if got := e.ops(); !slicesEqual(got, []string{"artifact/multipart-start", "artifact/put"}) {
		t.Fatalf("ops = %v", got)
	}
	assertEmptyDir(t, e.client.TempDir)
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("%s holds %d leftover files", dir, len(entries))
	}
}

func TestUpload_TempFilesStayInTempDir(t *testing.T) {
	tinyParts(t, 4, 2)
	e := newFakeEnv(t)
	var seen int
	e.s3Hook = func(_ http.ResponseWriter, _ *http.Request, _ []byte) bool {
		entries, _ := os.ReadDir(e.client.TempDir)
		if len(entries) != 1 {
			t.Errorf("during upload TempDir holds %d files, want 1", len(entries))
		}
		seen++
		return false
	}
	if err := e.client.Artifacts().Put(t.Context(), testKey, strings.NewReader("0123456789abcdefghij"), -1); err != nil {
		t.Fatal(err)
	}
	if seen == 0 {
		t.Fatal("the store was never called")
	}
	assertEmptyDir(t, e.client.TempDir)

	// A failed upload cleans up too.
	err := e.client.Artifacts().Put(t.Context(), testKey, &failingReader{data: []byte("0123456789"), err: io.ErrClosedPipe}, -1)
	if err == nil {
		t.Fatal("want an error")
	}
	assertEmptyDir(t, e.client.TempDir)
}

func TestCacheSave_EndsWithCommit(t *testing.T) {
	e := newFakeEnv(t)
	if err := e.client.Caches().Save(t.Context(), "k1", 7, strings.NewReader("data")); err != nil {
		t.Fatal(err)
	}
	if got := e.ops(); !slicesEqual(got, []string{"cache/put", "cache/commit"}) {
		t.Fatalf("ops = %v", got)
	}
	if c := e.requestsOf(api.ObjectOpCommit)[0]; c.TTLDays != 7 || c.Key != "k1" {
		t.Fatalf("commit = %+v", c)
	}

	tinyParts(t, 4, 2)
	e2 := newFakeEnv(t)
	if err := e2.client.Caches().Save(t.Context(), "k2", 3, strings.NewReader("0123456789")); err != nil {
		t.Fatal(err)
	}
	ops := e2.ops()
	if n := len(ops); n < 2 || ops[n-2] != "cache/multipart-complete" || ops[n-1] != "cache/commit" {
		t.Fatalf("ops = %v, want ...multipart-complete then commit", ops)
	}
}

func TestCacheSave_NoCommitWhenUploadFails(t *testing.T) {
	e := newFakeEnv(t)
	err := e.client.Caches().Save(t.Context(), "k1", 7, &failingReader{err: io.ErrUnexpectedEOF, data: nil})
	if err == nil {
		t.Fatal("want an error")
	}
	if len(e.requestsOf(api.ObjectOpCommit)) != 0 {
		t.Fatal("committed a failed upload")
	}
}

func TestUpload_SinglePutRetries5xxAndTransportErrors(t *testing.T) {
	for name, fail := range map[string]func(http.ResponseWriter){
		"503":       func(w http.ResponseWriter) { w.WriteHeader(http.StatusServiceUnavailable) },
		"transport": func(http.ResponseWriter) { panic(http.ErrAbortHandler) },
	} {
		t.Run(name, func(t *testing.T) {
			sleeps := noSleep(t)
			e := newFakeEnv(t)
			failed := false
			e.s3Hook = func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
				if !failed {
					failed = true
					fail(w)
					return true
				}
				return false
			}
			if err := e.client.Artifacts().Put(t.Context(), testKey, strings.NewReader("hello"), -1); err != nil {
				t.Fatal(err)
			}
			if string(e.objects[testKey]) != "hello" || len(*sleeps) != 1 {
				t.Fatalf("object = %q sleeps = %v", e.objects[testKey], *sleeps)
			}
			// Same URL, so no new presign.
			if got := e.ops(); !slicesEqual(got, []string{"artifact/put"}) {
				t.Fatalf("ops = %v", got)
			}
		})
	}
}

func TestUpload_ForbiddenOnLastAttemptDoesNotRefresh(t *testing.T) {
	noSleep(t)
	e := newFakeEnv(t)
	e.s3Hook = func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		w.WriteHeader(http.StatusForbidden)
		return true
	}
	if err := e.client.Artifacts().Put(t.Context(), testKey, strings.NewReader("hello"), -1); err == nil {
		t.Fatal("want an error")
	}
	if n := len(e.requestsOf(api.ObjectOpPut)); n != partAttempts {
		t.Fatalf("put presigns = %d, want %d (one per attempt, none after the last)", n, partAttempts)
	}
}

type cancelingReader struct {
	data   []byte
	cancel context.CancelFunc
}

func (r *cancelingReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		r.cancel()
		return 0, context.Canceled
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestUpload_CancelMidMultipartStillAborts(t *testing.T) {
	tinyParts(t, 4, 2)
	e := newFakeEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := e.client.Artifacts().Put(ctx, testKey, &cancelingReader{data: []byte("0123456789"), cancel: cancel}, -1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if aborts := e.requestsOf(api.ObjectOpMultipartAbort); len(aborts) != 1 {
		t.Fatalf("aborts = %+v, want one even though the context was canceled", aborts)
	}
}
