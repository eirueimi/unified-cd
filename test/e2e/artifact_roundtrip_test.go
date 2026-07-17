package e2e

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/artifact"
	"github.com/eirueimi/unified-cd/internal/controller"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/klauspost/compress/zstd"
)

// startArtifactController builds a controller Server backed only by a local
// object store (no Postgres) and an agent token, per the e2e harness used in
// phase3_test.go. It returns the httptest base URL and the agent token.
func startArtifactController(t *testing.T) (string, string) {
	t.Helper()
	const agentToken = "e2e-token"

	srv := controller.NewServer(controller.Config{LegacyAgentToken: agentToken}, nil)
	srv.SetObjectStore(objectstore.NewLocalObjectStore(t.TempDir()))

	httpSrv := httptest.NewServer(srv.Router())
	t.Cleanup(httpSrv.Close)

	return httpSrv.URL, agentToken
}

// makeArtifactTarZstd builds a single-file tar+zstd stream containing name -> content.
func makeArtifactTarZstd(t *testing.T, name, content string) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	tw := tar.NewWriter(zw)

	hdr := &tar.Header{
		Name: name,
		Mode: 0o644,
		Size: int64(len(content)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}

	return buf.Bytes()
}

// code returns resp.StatusCode, or 0 if resp is nil.
func code(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

func TestArtifactRoundTrip(t *testing.T) {
	baseURL, agentToken := startArtifactController(t)

	// 1. Upload a tar+zstd artifact via the agent PUT path.
	payload := makeArtifactTarZstd(t, "out.txt", "round-trip-ok")
	putReq, _ := http.NewRequest(http.MethodPut, baseURL+"/api/v1/runs/r1/artifacts/build", bytes.NewReader(payload))
	putReq.Header.Set("Authorization", "Bearer "+agentToken)
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil || code(putResp) != http.StatusNoContent {
		t.Fatalf("upload: %v code=%d", err, code(putResp))
	}
	putResp.Body.Close()

	// 2. List and assert the name appears.
	listReq, _ := http.NewRequest(http.MethodGet, baseURL+"/api/v1/runs/r1/artifacts", nil)
	listReq.Header.Set("Authorization", "Bearer "+agentToken)
	listResp, err := http.DefaultClient.Do(listReq)
	if err != nil || code(listResp) != http.StatusOK {
		t.Fatalf("list: %v code=%d", err, code(listResp))
	}
	var list []api.ArtifactInfo
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	listResp.Body.Close()
	if len(list) != 1 || list[0].Name != "build" {
		t.Fatalf("list = %v", list)
	}

	// 3. Download and extract; assert content round-trips.
	getReq, _ := http.NewRequest(http.MethodGet, baseURL+"/api/v1/runs/r1/artifacts/build", nil)
	getReq.Header.Set("Authorization", "Bearer "+agentToken)
	getResp, err := http.DefaultClient.Do(getReq)
	if err != nil || code(getResp) != http.StatusOK {
		t.Fatalf("download: %v code=%d", err, code(getResp))
	}
	defer getResp.Body.Close()

	dest := t.TempDir()
	if err := artifact.ExtractTarZstd(getResp.Body, dest); err != nil {
		t.Fatalf("extract: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "out.txt"))
	if err != nil || string(got) != "round-trip-ok" {
		t.Fatalf("out.txt = %q, %v", got, err)
	}
}
