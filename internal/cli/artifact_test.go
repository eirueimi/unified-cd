package cli

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/klauspost/compress/zstd"
)

func tarZstd(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, _ := zstd.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(content)), Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte(content))
	_ = tw.Close()
	_ = zw.Close()
	return buf.Bytes()
}

func TestArtifactList_PrintsNames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/runs/run1/artifacts" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]api.ArtifactInfo{{Name: "build"}, {Name: "logs"}})
	}))
	defer srv.Close()

	resolve := func() (Config, error) { return Config{Server: srv.URL, Token: "t"}, nil }
	cmd := newArtifactCmdWithClient(resolve, srv.Client())
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"list", "run1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "build") || !strings.Contains(out.String(), "logs") {
		t.Fatalf("out = %q", out.String())
	}
}

func TestArtifactDownload_ExtractsToDest(t *testing.T) {
	payload := tarZstd(t, "hello.txt", "hi")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/runs/run1/artifacts/build" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	dest := t.TempDir()
	resolve := func() (Config, error) { return Config{Server: srv.URL, Token: "t"}, nil }
	cmd := newArtifactCmdWithClient(resolve, srv.Client())
	cmd.SetArgs([]string{"download", "run1", "build", "--dest", dest})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "hello.txt"))
	if err != nil || string(got) != "hi" {
		t.Fatalf("hello.txt = %q, %v", got, err)
	}
}
