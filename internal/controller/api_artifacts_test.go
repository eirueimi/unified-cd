package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/artifact"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArtifactUpload_RejectsUnsafeName(t *testing.T) {
	// A name that is not a plain single path segment must be rejected before
	// it reaches the object store, by the same guard every other artifact
	// path uses (isSafeArtifactPathSegment via artifactKey).
	for _, name := range []string{"..", ".", ""} {
		_, err := artifact.ArtifactKey("run1", name)
		require.Error(t, err, "name %q must be rejected", name)
	}
	key, err := artifact.ArtifactKey("run1", "build-output")
	require.NoError(t, err)
	assert.Equal(t, "artifacts/run1/build-output.tar.gz", key,
		"valid names must produce the exact same key as before, so existing artifacts still resolve")
}

// newArtifactTestServer returns a Server whose router is wired and whose objStore
// is a usable local object store. Unlike newTestServer (which wires a real
// Postgres test DB), this keeps the DB nil since these tests only exercise
// object-store-only paths.
//
// A nil store means s.agentAuth's uca_ branch itself always 503s ("authentication
// unavailable") before ever reaching a handler — there is no credential to look
// up. So a real HTTP round trip through s.Router() can never exercise a handler's
// own behavior against a nil store. Tests that need that (handleArtifactUpload's
// nil-store fail-closed check, and handleArtifactList/handleArtifactDownload,
// neither of which touch s.store at all) call the handler directly via
// artifactPrincipalRequest instead, bypassing the HTTP auth layer on purpose —
// they are about the handler, not about auth. Tests that ARE about auth
// rejection (TestArtifactDownload_RejectsNoAuth, TestArtifactUpload_RejectsNonAgentToken)
// still go through s.r.ServeHTTP for a real round trip.
func newArtifactTestServer(t *testing.T) *Server {
	t.Helper()
	s := NewServer(Config{}, nil)
	s.SetObjectStore(objectstore.NewLocalObjectStore(t.TempDir()))
	return s
}

// artifactPrincipalRequest builds a request pre-authenticated as an agent
// principal, with the given chi URL params attached exactly as the router
// would inject them — for invoking an artifact handler function directly,
// skipping s.Router()'s auth middleware (see newArtifactTestServer).
func artifactPrincipalRequest(method, target string, body io.Reader, params map[string]string) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req = withAgentPrincipal(req, AgentPrincipal{AgentID: "artifact-test-agent", AuthMethod: "bearer"})
	rctx := chi.NewRouteContext()
	for k, v := range params {
		rctx.URLParams.Add(k, v)
	}
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func TestArtifact_ObjectStoreNil_Upload_Returns503(t *testing.T) {
	s, st := newTestServer(t)
	token := issueAgentAccessForTest(t, st, "objstore-nil-agent", nil, nil)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/runs/run1/artifacts/myartifact", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestArtifact_ObjectStoreNil_Download_Returns503(t *testing.T) {
	s, st := newTestServer(t)
	token := issueAgentAccessForTest(t, st, "objstore-nil-agent", nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/run1/artifacts/myartifact", nil)
	req.Header.Set("Authorization", "Bearer "+token)
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

	// The upload route has no {agentId} path segment, so the ownership check
	// is made directly against the AgentID carried by the caller's credential
	// (see handleArtifactUpload) — this round trip must upload as the agent
	// that actually claimed the run, via a real per-agent credential.
	ownerToken := issueAgentAccessForTest(t, st, "artifact-owner", nil, nil)
	_, err = st.TransitionPendingToQueued(t.Context(), 1)
	require.NoError(t, err)
	claimed, err := st.ClaimNextRun(t.Context(), "artifact-owner", nil)
	require.NoError(t, err)
	require.Equal(t, run.ID, claimed.ID)

	payload := []byte("hello artifact data")

	// Upload
	uploadReq := httptest.NewRequest(http.MethodPut, "/api/v1/runs/"+run.ID+"/artifacts/myartifact", bytes.NewReader(payload))
	uploadReq.Header.Set("Authorization", "Bearer "+ownerToken)
	uploadReq.Header.Set("Content-Type", "application/octet-stream")
	uploadRec := httptest.NewRecorder()
	s.Router().ServeHTTP(uploadRec, uploadReq)
	require.Equal(t, http.StatusNoContent, uploadRec.Code, uploadRec.Body.String())

	// Download. Listing/download carry no ownership check, so any valid
	// agent credential works here — reuse the owner's token.
	downloadReq := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+run.ID+"/artifacts/myartifact", nil)
	downloadReq.Header.Set("Authorization", "Bearer "+ownerToken)
	downloadRec := httptest.NewRecorder()
	s.Router().ServeHTTP(downloadRec, downloadReq)
	require.Equal(t, http.StatusOK, downloadRec.Code, downloadRec.Body.String())

	got, err := io.ReadAll(downloadRec.Body)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(payload, got), "downloaded body does not match uploaded payload")
}

func TestArtifactList_ReturnsNames(t *testing.T) {
	// handleArtifactUpload now fails closed (503) when s.store is nil (see
	// handleArtifactUpload's ownership guard), so uploading requires a real
	// store-backed server with a claimed run to authenticate the ownership
	// check against — newArtifactTestServer's nil store can no longer upload,
	// only list/download, which don't consult s.store at all.
	s, st := newTestServer(t)
	s.SetObjectStore(objectstore.NewLocalObjectStore(t.TempDir()))
	_, err := st.UpsertJob(t.Context(), "artifact-list-job", "unified-cd/v1", []byte(`{"steps":[]}`))
	require.NoError(t, err)
	run, err := st.CreateRun(t.Context(), "artifact-list-job", nil, []byte(`{"steps":[]}`), nil, nil, "")
	require.NoError(t, err)
	ownerToken := issueAgentAccessForTest(t, st, "artifact-list-owner", nil, nil)
	_, err = st.TransitionPendingToQueued(t.Context(), 1)
	require.NoError(t, err)
	claimed, err := st.ClaimNextRun(t.Context(), "artifact-list-owner", nil)
	require.NoError(t, err)
	require.Equal(t, run.ID, claimed.ID)

	// upload two artifacts via the agent PUT path, as the owning agent
	for _, name := range []string{"build", "logs"} {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/runs/"+run.ID+"/artifacts/"+name, strings.NewReader("x"))
		req.Header.Set("Authorization", "Bearer "+ownerToken)
		rr := httptest.NewRecorder()
		s.Router().ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("put %s: %d (%s)", name, rr.Code, rr.Body.String())
		}
	}
	// list — listing carries no ownership check, so the owner's token works fine.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+run.ID+"/artifacts", nil)
	req.Header.Set("Authorization", "Bearer "+ownerToken)
	rr := httptest.NewRecorder()
	s.Router().ServeHTTP(rr, req)
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

// TestArtifactUpload_NilStore_FailsClosed is the regression test for the
// "s.store != nil gates the ownership guard itself" finding: a server with an
// object store but no run store must refuse artifact uploads outright (503),
// not silently skip the ownership check and accept the upload from anyone.
func TestArtifactUpload_NilStore_FailsClosed(t *testing.T) {
	s := newArtifactTestServer(t)
	req := artifactPrincipalRequest(http.MethodPut, "/api/v1/runs/run1/artifacts/build", strings.NewReader("x"),
		map[string]string{"runID": "run1", "name": "build"})
	rr := httptest.NewRecorder()
	s.handleArtifactUpload(rr, req)
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code, rr.Body.String())
}

func TestArtifactList_EmptyIsArrayNotNull(t *testing.T) {
	s := newArtifactTestServer(t)
	req := artifactPrincipalRequest(http.MethodGet, "/api/v1/runs/empty/artifacts", nil,
		map[string]string{"runID": "empty"})
	rr := httptest.NewRecorder()
	s.handleArtifactList(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d", rr.Code)
	}
	if strings.TrimSpace(rr.Body.String()) != "[]" {
		t.Fatalf("empty list body = %q, want []", rr.Body.String())
	}
}

func TestArtifactDownload_RejectsNoAuth(t *testing.T) {
	s := newArtifactTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/run1/artifacts/build", nil)
	rr := httptest.NewRecorder()
	s.r.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no-auth download = %d, want 401", rr.Code)
	}
}

func TestArtifactUpload_RejectsNonAgentToken(t *testing.T) {
	s := newArtifactTestServer(t)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/runs/run1/artifacts/build", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer not-the-agent-token")
	rr := httptest.NewRecorder()
	s.r.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad-token upload = %d, want 401", rr.Code)
	}
}

// TestArtifactUpload_RejectsMismatchedOwnerPrincipal proves the upload route
// has no {agentId} path segment, so a bearer caller whose identity does not
// match the run's claimed_by must always be rejected, claimed or not — the
// same fail-closed outcome as the secrets-fetch path, reached via the shared
// ownership guard.
func TestArtifactUpload_RejectsMismatchedOwnerPrincipal(t *testing.T) {
	s, st := newTestServer(t)
	s.SetObjectStore(objectstore.NewLocalObjectStore(t.TempDir()))
	_, err := st.UpsertJob(t.Context(), "artifact-legacy-reject", "unified-cd/v1", []byte(`{"steps":[]}`))
	require.NoError(t, err)
	run, err := st.CreateRun(t.Context(), "artifact-legacy-reject", nil, []byte(`{"steps":[]}`), nil, nil, "")
	require.NoError(t, err)
	_, err = st.TransitionPendingToQueued(t.Context(), 1)
	require.NoError(t, err)
	claimed, err := st.ClaimNextRun(t.Context(), "run-claimer", nil)
	require.NoError(t, err)
	require.Equal(t, run.ID, claimed.ID)

	strangerToken := issueAgentAccessForTest(t, st, "stranger", nil, nil)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/runs/"+run.ID+"/artifacts/build", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer "+strangerToken)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
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
	s := newArtifactTestServer(t)
	req := artifactPrincipalRequest(http.MethodGet, "/api/v1/runs/run1/artifacts/nope", nil,
		map[string]string{"runID": "run1", "name": "nope"})
	rr := httptest.NewRecorder()
	s.handleArtifactDownload(rr, req)
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
