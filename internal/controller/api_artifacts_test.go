package controller

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newArtifactTestServer returns a Server whose router is wired and whose objStore
// is a usable local object store, plus the agent token. Unlike newTestServer (which
// wires a real Postgres test DB), this keeps the DB nil since these tests only
// exercise object-store + agent-token paths.
func newArtifactTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	agentToken := "agent-secret"
	s := NewServer(Config{LegacyAgentToken: agentToken}, nil)
	s.SetObjectStore(objectstore.NewLocalObjectStore(t.TempDir()))
	return s, agentToken
}

func TestArtifact_ObjectStoreNil_Upload_Returns503(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/runs/run1/artifacts/myartifact", nil)
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestArtifact_ObjectStoreNil_Download_Returns503(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/run1/artifacts/myartifact", nil)
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestArtifact_UploadDownload_RoundTrip(t *testing.T) {
	s, st := newTestServer(t)
	obj := objectstore.NewLocalObjectStore(t.TempDir())
	s.SetObjectStore(obj)

	// handleArtifactUpload now 404s for runs the store doesn't know about, so
	// seed a real job+run rather than uploading to a bare literal like "run1".
	_, err := st.UpsertJob(t.Context(), "artifact-roundtrip-job", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := st.CreateRun(t.Context(), "artifact-roundtrip-job", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)

	payload := []byte("hello artifact data")

	// Upload
	uploadReq := httptest.NewRequest(http.MethodPut, "/api/v1/runs/"+run.ID+"/artifacts/myartifact", bytes.NewReader(payload))
	uploadReq.Header.Set("Authorization", "Bearer agent-secret")
	uploadReq.Header.Set("Content-Type", "application/octet-stream")
	uploadRec := httptest.NewRecorder()
	s.Router().ServeHTTP(uploadRec, uploadReq)
	require.Equal(t, http.StatusNoContent, uploadRec.Code, uploadRec.Body.String())

	// Download
	downloadReq := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+run.ID+"/artifacts/myartifact", nil)
	downloadReq.Header.Set("Authorization", "Bearer agent-secret")
	downloadRec := httptest.NewRecorder()
	s.Router().ServeHTTP(downloadRec, downloadReq)
	require.Equal(t, http.StatusOK, downloadRec.Code, downloadRec.Body.String())

	got, err := io.ReadAll(downloadRec.Body)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(payload, got), "downloaded body does not match uploaded payload")
}

func TestArtifactList_ReturnsNames(t *testing.T) {
	s, agentToken := newArtifactTestServer(t)
	// upload two artifacts via the agent PUT path
	for _, name := range []string{"build", "logs"} {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/runs/run1/artifacts/"+name, strings.NewReader("x"))
		req.Header.Set("Authorization", "Bearer "+agentToken)
		rr := httptest.NewRecorder()
		s.r.ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("put %s: %d", name, rr.Code)
		}
	}
	// list with the agent token (combined auth accepts it)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/run1/artifacts", nil)
	req.Header.Set("Authorization", "Bearer "+agentToken)
	rr := httptest.NewRecorder()
	s.r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d (%s)", rr.Code, rr.Body.String())
	}
	var got []api.ArtifactInfo
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	names := map[string]bool{}
	for _, a := range got {
		names[a.Name] = true
	}
	if !names["build"] || !names["logs"] {
		t.Fatalf("missing names: %v", got)
	}
}

func TestArtifactList_EmptyIsArrayNotNull(t *testing.T) {
	s, agentToken := newArtifactTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/empty/artifacts", nil)
	req.Header.Set("Authorization", "Bearer "+agentToken)
	rr := httptest.NewRecorder()
	s.r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d", rr.Code)
	}
	if strings.TrimSpace(rr.Body.String()) != "[]" {
		t.Fatalf("empty list body = %q, want []", rr.Body.String())
	}
}

func TestArtifactDownload_RejectsNoAuth(t *testing.T) {
	s, _ := newArtifactTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/run1/artifacts/build", nil)
	rr := httptest.NewRecorder()
	s.r.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no-auth download = %d, want 401", rr.Code)
	}
}

func TestArtifactUpload_RejectsNonAgentToken(t *testing.T) {
	s, _ := newArtifactTestServer(t)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/runs/run1/artifacts/build", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer not-the-agent-token")
	rr := httptest.NewRecorder()
	s.r.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad-token upload = %d, want 401", rr.Code)
	}
}

func TestArtifactUpload_RejectsNonOwnerPrincipal(t *testing.T) {
	s, st := newTestServer(t)
	s.SetObjectStore(objectstore.NewLocalObjectStore(t.TempDir()))
	intruderToken := issueAgentAccessForTest(t, st, "intruder", nil, nil)
	_, err := st.UpsertJob(t.Context(), "artifact-owner", "unified-cd/v1", []byte(`{"steps":[]}`))
	require.NoError(t, err)
	run, err := st.CreateRun(t.Context(), "artifact-owner", nil, []byte(`{"steps":[]}`), nil, nil, "")
	require.NoError(t, err)
	_, err = st.TransitionPendingToQueued(t.Context(), 1)
	require.NoError(t, err)
	claimed, err := st.ClaimNextRun(t.Context(), "owner", nil)
	require.NoError(t, err)
	require.Equal(t, run.ID, claimed.ID)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/runs/"+run.ID+"/artifacts/build", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer "+intruderToken)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

func TestArtifactDownload_MissingArtifact_Returns404(t *testing.T) {
	s, agentToken := newArtifactTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/run1/artifacts/nope", nil)
	req.Header.Set("Authorization", "Bearer "+agentToken)
	rr := httptest.NewRecorder()
	s.r.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("missing-artifact download = %d, want 404 (body=%q)", rr.Code, rr.Body.String())
	}
}

// TestLogsArchive_MissingObject_Returns404 covers handleLogsArchive's
// ErrNotFound branch: the archive record exists in the DB but the underlying
// object is gone from the store — the client gets a clean 404 instead of a
// broken stream (possible only since ObjectStore.Get detects missing keys
// eagerly).
func TestLogsArchive_MissingObject_Returns404(t *testing.T) {
	s, st := newTestServer(t)
	s.SetObjectStore(objectstore.NewLocalObjectStore(t.TempDir()))

	_, _ = st.UpsertJob(t.Context(), "arch404-job", "unified-cd/v1", []byte(`{"steps":[{"name":"s","run":"echo x"}]}`))
	run, err := st.CreateRun(t.Context(), "arch404-job", nil, []byte(`{}`), nil, nil, "test")
	require.NoError(t, err)
	require.NoError(t, st.CreateLogArchive(t.Context(), run.ID, "logs/"+run.ID+".ndjson", 42, 0, 0))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+run.ID+"/logs/archive", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "not found")
}
