package controller

import (
	"net/http"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sidecarPresignConfig trusts storeCredsFakeClient's identity: Pod
// "ucd-run-abc" (UID "job-pod-uid") in namespace "ci" as ServiceAccount
// "ci-runner".
func sidecarPresignConfig(namespaces []string) Config {
	return Config{StoreCredentialClusters: []StoreCredentialCluster{{Cluster: "prod", Client: storeCredsFakeClient(), Namespaces: namespaces, ServiceAccounts: []string{"ci-runner"}}}}
}

func sidecarToken() string {
	return projectedToken("ci", "ci-runner", "sa-uid", "ucd-run-abc", "job-pod-uid")
}

func TestPresign_AgentMustOwnRun(t *testing.T) {
	s, st, _ := presignTestServer(t, Config{})
	_ = issueAgentAccessForTest(t, st, "agent-a", nil, nil)
	tokenB := issueAgentAccessForTest(t, st, "agent-b", nil, nil)
	runID := seedClaimedRun(t, st, "job-a", "agent-a", "")

	rec := doPresign(t, s, tokenB, runID, api.PresignRequest{Kind: "artifact", Op: "put", Name: "a"})
	requireObjectError(t, rec, http.StatusForbidden, "forbidden")

	rec = doPresign(t, s, tokenB, "00000000-0000-0000-0000-000000000000", api.PresignRequest{Kind: "artifact", Op: "put", Name: "a"})
	requireObjectError(t, rec, http.StatusNotFound, "not-found")
	rec = doPresign(t, s, tokenB, "not-a-uuid", api.PresignRequest{Kind: "artifact", Op: "put", Name: "a"})
	requireObjectError(t, rec, http.StatusNotFound, "not-found")
}

func TestPresign_RejectsMissingAndBadBearer(t *testing.T) {
	s, st, _ := presignTestServer(t, Config{})
	runID := seedRun(t, st, "job-a", "")
	rec := doPresign(t, s, "", runID, api.PresignRequest{Kind: "artifact", Op: "put", Name: "a"})
	requireObjectError(t, rec, http.StatusUnauthorized, "unauthenticated")
	rec = doPresign(t, s, "uca_garbage", runID, api.PresignRequest{Kind: "artifact", Op: "put", Name: "a"})
	requireObjectError(t, rec, http.StatusUnauthorized, "unauthenticated")
}

func TestPresign_SidecarBoundSucceeds(t *testing.T) {
	s, st, _ := presignTestServer(t, sidecarPresignConfig([]string{"ci"}))
	runID := seedRun(t, st, "job-a", "")
	require.NoError(t, st.UpsertRunPodBinding(t.Context(), runID, "ucd-run-abc", "job-pod-uid"))
	out := decodePresign(t, doPresign(t, s, sidecarToken(), runID, api.PresignRequest{Kind: "artifact", Op: "put", Name: "a"}))
	require.NotNil(t, out.Request)
	assert.Equal(t, "artifacts/"+runID+"/a.tar.gz", urlKey(t, out.Request.URL))
}

func TestPresign_SidecarWithoutBindingGets409(t *testing.T) {
	s, st, _ := presignTestServer(t, sidecarPresignConfig([]string{"ci"}))
	runID := seedRun(t, st, "job-a", "")
	rec := doPresign(t, s, sidecarToken(), runID, api.PresignRequest{Kind: "artifact", Op: "put", Name: "a"})
	requireObjectError(t, rec, http.StatusConflict, "binding-pending")
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))
}

func TestPresign_SidecarBoundToOtherPodGets403(t *testing.T) {
	s, st, _ := presignTestServer(t, sidecarPresignConfig([]string{"ci"}))
	runID := seedRun(t, st, "job-a", "")
	require.NoError(t, st.UpsertRunPodBinding(t.Context(), runID, "ucd-run-other", "other-pod-uid"))
	rec := doPresign(t, s, sidecarToken(), runID, api.PresignRequest{Kind: "artifact", Op: "put", Name: "a"})
	requireObjectError(t, rec, http.StatusForbidden, "forbidden")
}

func TestPresign_SidecarNamespaceNotAllowedGets403(t *testing.T) {
	s, st, _ := presignTestServer(t, sidecarPresignConfig([]string{"other-ns"}))
	runID := seedRun(t, st, "job-a", "")
	require.NoError(t, st.UpsertRunPodBinding(t.Context(), runID, "ucd-run-abc", "job-pod-uid"))
	rec := doPresign(t, s, sidecarToken(), runID, api.PresignRequest{Kind: "artifact", Op: "put", Name: "a"})
	requireObjectError(t, rec, http.StatusForbidden, "forbidden")
}

func TestPresign_RequireRunBindingSettingIsIgnored(t *testing.T) {
	cfg := sidecarPresignConfig([]string{"ci"})
	cfg.RequireRunBinding = false
	s, st, _ := presignTestServer(t, cfg)
	runID := seedRun(t, st, "job-a", "")
	rec := doPresign(t, s, sidecarToken(), runID, api.PresignRequest{Kind: "cache", Op: "put", Key: "k"})
	requireObjectError(t, rec, http.StatusConflict, "binding-pending")
}
