package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func varsYAML(name string, vars map[string]string) string {
	b, _ := json.Marshal(vars)
	return `
apiVersion: unified-cd/v1
kind: Vars
metadata:
  name: ` + name + `
spec:
  vars: ` + string(b) + `
`
}

func applyVarsReq(t *testing.T, s *Server, name string, vars map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(api.ApplyVarsRequest{YAML: varsYAML(name, vars)})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/vars", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec
}

// TestApplyVars_CollisionRejected verifies that applying two DIFFERENT Vars
// manifests that define the same key is rejected, and that the error names
// the colliding key and both manifests — without both names an operator
// cannot find the other manifest to fix.
func TestApplyVars_CollisionRejected(t *testing.T) {
	s, _ := newTestServer(t)

	rec := applyVarsReq(t, s, "org-defaults", map[string]string{"SHARED": "x"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = applyVarsReq(t, s, "team-defaults", map[string]string{"SHARED": "y"})
	assert.NotEqual(t, http.StatusOK, rec.Code, "collision must be rejected")
	body := rec.Body.String()
	assert.Contains(t, body, "SHARED")
	assert.Contains(t, body, "org-defaults")
	assert.Contains(t, body, "team-defaults")
}

// TestApplyVars_ReapplySameManifest verifies that re-applying the SAME
// manifest with a changed value for a key it already defines is treated as
// an update, not a collision — this is the case a naive "does anyone else
// define this key" check would break.
func TestApplyVars_ReapplySameManifest(t *testing.T) {
	s, pg := newTestServer(t)

	rec := applyVarsReq(t, s, "org-defaults", map[string]string{"SHARED": "x"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = applyVarsReq(t, s, "org-defaults", map[string]string{"SHARED": "y"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	list, err := pg.ListVars(t.Context())
	require.NoError(t, err)
	require.Len(t, list, 1)
	var spec struct {
		Vars map[string]string `json:"vars"`
	}
	require.NoError(t, json.Unmarshal(list[0].Spec, &spec))
	assert.Equal(t, "y", spec.Vars["SHARED"])
}

// TestApplyVars_ReservedNameRejected verifies that a manifest defining a
// reserved variable name (e.g. UNIFIED_TOKEN) is rejected, and the error
// names it.
func TestApplyVars_ReservedNameRejected(t *testing.T) {
	s, _ := newTestServer(t)

	rec := applyVarsReq(t, s, "org-defaults", map[string]string{"UNIFIED_TOKEN": "x"})
	assert.NotEqual(t, http.StatusOK, rec.Code, "reserved name must be rejected")
	assert.Contains(t, rec.Body.String(), "UNIFIED_TOKEN")
}

// TestDeleteVars verifies apply, delete, and that a subsequent list is empty.
func TestDeleteVars(t *testing.T) {
	s, _ := newTestServer(t)

	rec := applyVarsReq(t, s, "org-defaults", map[string]string{"SHARED": "x"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/vars/org-defaults", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	req = httptest.NewRequest(http.MethodGet, "/api/v1/vars", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list []api.VarsMeta
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	assert.Empty(t, list)
}

// TestApplyVars_RejectedWhenManaged verifies that a Vars manifest managed by
// an AppSource cannot be overwritten directly through the interactive apply
// endpoint — same managed-resource guard as every other apply handler.
func TestApplyVars_RejectedWhenManaged(t *testing.T) {
	s, pg := newTestServer(t)
	ctx := context.Background()
	_, err := pg.UpsertAppSource(ctx, "owner", []byte(`{"repoURL":"https://example.com/r.git","targetRevision":"main","path":"vars"}`))
	require.NoError(t, err)
	require.NoError(t, pg.UpdateAppSourceSyncState(ctx, "owner", "sha", time.Now(),
		[]store.ResourceRef{{Kind: "Vars", Name: "org-defaults"}}))

	rec := applyVarsReq(t, s, "org-defaults", map[string]string{"SHARED": "x"})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `managed by AppSource "owner"`)
}
