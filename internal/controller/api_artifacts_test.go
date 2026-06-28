package controller

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/eirueimi/unified-cd/internal/objectstore"
)

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
	s, _ := newTestServer(t)
	obj := objectstore.NewLocalObjectStore(t.TempDir())
	s.SetObjectStore(obj)

	payload := []byte("hello artifact data")

	// Upload
	uploadReq := httptest.NewRequest(http.MethodPut, "/api/v1/runs/run1/artifacts/myartifact", bytes.NewReader(payload))
	uploadReq.Header.Set("Authorization", "Bearer agent-secret")
	uploadReq.Header.Set("Content-Type", "application/octet-stream")
	uploadRec := httptest.NewRecorder()
	s.Router().ServeHTTP(uploadRec, uploadReq)
	require.Equal(t, http.StatusNoContent, uploadRec.Code, uploadRec.Body.String())

	// Download
	downloadReq := httptest.NewRequest(http.MethodGet, "/api/v1/runs/run1/artifacts/myartifact", nil)
	downloadReq.Header.Set("Authorization", "Bearer agent-secret")
	downloadRec := httptest.NewRecorder()
	s.Router().ServeHTTP(downloadRec, downloadReq)
	require.Equal(t, http.StatusOK, downloadRec.Code, downloadRec.Body.String())

	got, err := io.ReadAll(downloadRec.Body)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(payload, got), "downloaded body does not match uploaded payload")
}
