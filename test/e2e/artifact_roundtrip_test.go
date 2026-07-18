package e2e

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/agentauth"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/artifact"
	"github.com/eirueimi/unified-cd/internal/controller"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

// startArtifactController builds a controller Server backed by a real Postgres
// store (like every other e2e test in this package) plus a local object
// store, and a legacy agent token for auth on non-ownership routes (list,
// download).
//
// A store-less controller (object store, no Postgres) is not a configuration
// that can occur in production (cmd/controller/main.go always wires a store),
// and the upload route now fails closed with 503 when s.store == nil — that
// nil-store branch used to skip the run-ownership guard for everyone, which
// was the actual security hole the final-review fix closed. So this harness
// now mirrors production: a real store, a real claimed run, and a real
// per-agent credential for the uploading agent, exactly like
// TestArtifact_UploadDownload_RoundTrip in internal/controller/api_artifacts_test.go.
func startArtifactController(t *testing.T) (baseURL, listenAgentToken string, pg *store.Postgres) {
	t.Helper()
	const legacyAgentToken = "e2e-token"

	pg = store.NewTestPostgres(t)
	srv := controller.NewServer(controller.Config{Token: "t", LegacyAgentToken: legacyAgentToken}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, "t"))
	srv.SetObjectStore(objectstore.NewLocalObjectStore(t.TempDir()))

	httpSrv := httptest.NewServer(srv.Router())
	t.Cleanup(httpSrv.Close)

	return httpSrv.URL, legacyAgentToken, pg
}

// issueAgentAccessToken mints a real per-agent credential the same way
// internal/controller/agent_auth_test.go's issueAgentAccessForTest does,
// so the e2e upload exercises the real ownership path (agentRunGuard)
// instead of the removed nil-store bypass.
func issueAgentAccessToken(t *testing.T, pg *store.Postgres, agentID string) string {
	t.Helper()
	issued, err := agentauth.Generate(agentauth.AccessToken)
	require.NoError(t, err)
	_, err = pg.IssueExternalAgentAccess(context.Background(), store.AgentCredentialIssue{
		AgentID:          agentID,
		EnrollmentMethod: "test",
		ExternalSubject:  "test:" + agentID,
		Access: store.NewAgentCredential{
			ID:        issued.ID,
			Kind:      "access",
			TokenHash: issued.Hash,
			ExpiresAt: time.Now().Add(time.Hour),
		},
	})
	require.NoError(t, err)
	return issued.Plaintext
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
	if runtime.GOOS == "windows" {
		t.Skip("e2e harness (dockertest postgres) is linux/mac only")
	}

	baseURL, legacyAgentToken, pg := startArtifactController(t)
	ctx := context.Background()

	// Seed a real job+run and claim it as the agent that will upload, so the
	// upload passes agentRunGuard's ownership check instead of relying on a
	// nil-store bypass that no longer exists.
	_, err := pg.UpsertJob(ctx, "artifact-roundtrip-job", "unified-cd/v1", []byte(`{"steps":[]}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(ctx, "artifact-roundtrip-job", nil, []byte(`{"steps":[]}`), nil, nil, "")
	require.NoError(t, err)
	_, err = pg.TransitionPendingToQueued(ctx, 1)
	require.NoError(t, err)
	claimed, err := pg.ClaimNextRun(ctx, "artifact-owner", nil)
	require.NoError(t, err)
	require.Equal(t, run.ID, claimed.ID)

	ownerToken := issueAgentAccessToken(t, pg, "artifact-owner")

	// 1. Upload a tar+zstd artifact via the agent PUT path, authenticated as
	// the agent that claimed the run.
	payload := makeArtifactTarZstd(t, "out.txt", "round-trip-ok")
	putReq, _ := http.NewRequest(http.MethodPut, baseURL+"/api/v1/runs/"+run.ID+"/artifacts/build", bytes.NewReader(payload))
	putReq.Header.Set("Authorization", "Bearer "+ownerToken)
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil || code(putResp) != http.StatusNoContent {
		t.Fatalf("upload: %v code=%d", err, code(putResp))
	}
	putResp.Body.Close()

	// 2. List and assert the name appears. Listing carries no ownership
	// check, so the legacy shared token is fine here.
	listReq, _ := http.NewRequest(http.MethodGet, baseURL+"/api/v1/runs/"+run.ID+"/artifacts", nil)
	listReq.Header.Set("Authorization", "Bearer "+legacyAgentToken)
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

	// 3. Download and extract; assert content round-trips. Downloading also
	// carries no ownership check.
	getReq, _ := http.NewRequest(http.MethodGet, baseURL+"/api/v1/runs/"+run.ID+"/artifacts/build", nil)
	getReq.Header.Set("Authorization", "Bearer "+legacyAgentToken)
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
