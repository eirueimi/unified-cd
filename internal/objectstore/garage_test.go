//go:build garage

package objectstore_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/objectstore/objectstoretest"
)

func testKey(name string) string {
	return fmt.Sprintf("garage-test/%d/%s", time.Now().UnixNano(), name)
}

func do(t *testing.T, method, u string, body []byte, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, u, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestGarage_PresignedPutGetHead(t *testing.T) {
	s := objectstoretest.S3FromEnv(t)
	ctx := context.Background()
	key := testKey("hello")
	t.Cleanup(func() { _ = s.Delete(ctx, key) })

	put, err := s.PresignPut(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if resp, b := do(t, http.MethodPut, put.String(), []byte("hello"), nil); resp.StatusCode/100 != 2 {
		t.Fatalf("PUT status %d: %s", resp.StatusCode, b)
	}
	get, err := s.PresignGet(ctx, key, "")
	if err != nil {
		t.Fatal(err)
	}
	resp, b := do(t, http.MethodGet, get.String(), nil, nil)
	if resp.StatusCode != 200 || string(b) != "hello" {
		t.Fatalf("GET = %d %q", resp.StatusCode, b)
	}
	head, err := s.PresignHead(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if resp, _ := do(t, http.MethodHead, head.String(), nil, nil); resp.StatusCode != 200 {
		t.Fatalf("HEAD status %d", resp.StatusCode)
	}
}

func TestGarage_PresignedMultipartTwoParts(t *testing.T) {
	s := objectstoretest.S3FromEnv(t)
	ctx := context.Background()
	key := testKey("multipart")
	t.Cleanup(func() { _ = s.Delete(ctx, key) })

	id, err := s.StartMultipart(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	const mib5 = 5 << 20
	bodies := [][]byte{bytes.Repeat([]byte("a"), mib5), []byte("b")}
	var parts []objectstore.CompletedPart
	for i, body := range bodies {
		u, err := s.PresignUploadPart(ctx, key, id, i+1)
		if err != nil {
			t.Fatal(err)
		}
		resp, b := do(t, http.MethodPut, u.String(), body, nil)
		if resp.StatusCode/100 != 2 {
			t.Fatalf("part %d status %d: %s", i+1, resp.StatusCode, b)
		}
		etag := strings.Trim(resp.Header.Get("ETag"), "\"")
		if etag == "" {
			t.Fatalf("part %d returned no ETag", i+1)
		}
		parts = append(parts, objectstore.CompletedPart{PartNumber: i + 1, ETag: etag})
	}
	if err := s.CompleteMultipart(ctx, key, id, parts); err != nil {
		t.Fatal(err)
	}
	info, err := s.Stat(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != mib5+1 {
		t.Fatalf("size = %d, want %d", info.Size, mib5+1)
	}
}

func TestGarage_AbortAndList(t *testing.T) {
	s := objectstoretest.S3FromEnv(t)
	ctx := context.Background()
	key := testKey("abort")
	prefix := key[:strings.LastIndex(key, "/")+1]

	id, err := s.StartMultipart(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	has := func() bool {
		ups, err := s.ListMultipartUploads(ctx, prefix)
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range ups {
			if u.Key == key && u.UploadID == id {
				return true
			}
		}
		return false
	}
	if !has() {
		t.Fatal("started upload not listed")
	}
	if err := s.AbortMultipart(ctx, key, id); err != nil {
		t.Fatal(err)
	}
	if has() {
		t.Fatal("upload still listed after abort")
	}
}

func TestGarage_RangeGet(t *testing.T) {
	s := objectstoretest.S3FromEnv(t)
	ctx := context.Background()
	key := testKey("range")
	t.Cleanup(func() { _ = s.Delete(ctx, key) })
	if err := s.Put(ctx, key, strings.NewReader("hello"), 5); err != nil {
		t.Fatal(err)
	}
	get, err := s.PresignGet(ctx, key, "")
	if err != nil {
		t.Fatal(err)
	}
	resp, b := do(t, http.MethodGet, get.String(), nil, map[string]string{"Range": "bytes=2-"})
	if resp.StatusCode != http.StatusPartialContent || string(b) != "llo" {
		t.Fatalf("range GET = %d %q", resp.StatusCode, b)
	}
}
