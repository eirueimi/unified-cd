package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// jobPod is a job Pod running in the "ci" namespace under a job
// ServiceAccount, distinct from the agent's own
// "unified-cd"/"unified-cd-k8s-agent" identity that boundPod() represents:
// the two are different trust domains, which the audience tests below
// enforce.
func jobPod() *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "ucd-run-abc", Namespace: "ci", UID: "job-pod-uid"}, Spec: corev1.PodSpec{ServiceAccountName: "ci-runner"}}
}

// storeCredsFakeClient returns a fake Kubernetes clientset that authenticates
// any token as jobPod()'s identity ("ci"/"ci-runner", Pod "ucd-run-abc" /
// UID "job-pod-uid") against api.KubernetesStoreCredentialAudience, the
// audience the sidecar's projected token carries.
func storeCredsFakeClient() *fake.Clientset {
	client := fake.NewSimpleClientset(jobPod())
	client.Fake.PrependReactor("create", "tokenreviews", tokenReviewReactor(true, []string{api.KubernetesStoreCredentialAudience}, "system:serviceaccount:ci:ci-runner", "sa-uid"))
	return client
}

// The removed broker endpoint has no route and no handler. An old sidecar
// sends no Authorization header (its token travels in the body), so the
// /api/v1 group's auth answers 401 before routing can say 404; that is the
// string docs/operator-manual/migrations/2026-11-20-run-scoped-object-access.md
// quotes. The old handler's own answer for this body was 400 "invalid request".
func TestStoreCredentialsRouteGone(t *testing.T) {
	s := NewServer(Config{}, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/store-credentials", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, "unauthorized\n", rec.Body.String())
}

// An enrollment-audience token must not authenticate a sidecar to the
// presign endpoint.
func TestPresign_RejectsAnEnrollmentAudienceToken(t *testing.T) {
	client := fake.NewSimpleClientset(jobPod())
	client.Fake.PrependReactor("create", "tokenreviews", tokenReviewReactor(true, []string{KubernetesEnrollmentAudience}, "system:serviceaccount:ci:ci-runner", "sa-uid"))
	cfg := Config{StoreCredentialClusters: []StoreCredentialCluster{{Cluster: "prod", Client: client, Namespaces: []string{"ci"}, ServiceAccounts: []string{"ci-runner"}}}}
	s, st, _ := presignTestServer(t, cfg)
	runID := seedRun(t, st, "job-a", "")
	require.NoError(t, st.UpsertRunPodBinding(t.Context(), runID, "ucd-run-abc", "job-pod-uid"))
	rec := doPresign(t, s, sidecarToken(), runID, api.PresignRequest{Kind: "artifact", Op: "put", Name: "a"})
	requireObjectError(t, rec, http.StatusUnauthorized, "unauthenticated")
}

// And the converse, asserted against the enrollment endpoint.
func TestAgentEnrollment_RejectsAStoreCredentialAudienceToken(t *testing.T) {
	policy := kubernetesEnrollmentPolicy()
	policy.Name = "prod-policy"
	policy.Enabled = true
	policy.ProviderConfig = json.RawMessage(`{"cluster":"prod"}`)

	client := fake.NewSimpleClientset(boundPod())
	client.Fake.PrependReactor("create", "tokenreviews", tokenReviewReactor(true, []string{api.KubernetesStoreCredentialAudience}, "system:serviceaccount:unified-cd:unified-cd-k8s-agent", "sa-uid"))
	verifier := NewKubernetesEnrollmentVerifier("prod", client)

	s := NewServer(Config{KubernetesEnrollmentVerifiers: map[string]KubernetesEnrollmentVerifier{"prod": verifier}}, unavailableKubernetesPolicyStore{policy: policy})

	body, err := json.Marshal(api.AgentEnrollRequest{Provider: "kubernetes", Policy: "prod-policy"})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/enroll", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+projectedToken("unified-cd", "unified-cd-k8s-agent", "sa-uid", "agent-0", "pod-uid"))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

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

// Succeeded and Failed runs refuse the sidecar even with a valid binding:
// cancel, the reaper and the scheduler leave bindings in place, so the status
// is what ends access. Each refusal is audited as a 409.
func TestPresign_SidecarRefusedOnceRunSucceededOrFailed(t *testing.T) {
	for _, status := range []api.RunStatus{api.RunFailed, api.RunSucceeded} {
		t.Run(string(status), func(t *testing.T) {
			s, st, _ := presignTestServer(t, sidecarPresignConfig([]string{"ci"}))
			runID := seedRun(t, st, "job-a", "")
			require.NoError(t, st.UpsertRunPodBinding(t.Context(), runID, "ucd-run-abc", "job-pod-uid"))
			// Valid and running first.
			out := decodePresign(t, doPresign(t, s, sidecarToken(), runID, api.PresignRequest{Kind: "artifact", Op: "put", Name: "a"}))
			require.NotNil(t, out.Request)

			updated, err := st.FinishRun(t.Context(), runID, status)
			require.NoError(t, err)
			require.True(t, updated)
			rec := doPresign(t, s, sidecarToken(), runID, api.PresignRequest{Kind: "artifact", Op: "put", Name: "a"})
			requireObjectError(t, rec, http.StatusConflict, "run-finished")
			assert.Empty(t, rec.Header().Get("Retry-After"))

			// Without a binding the answer is the same final one, never binding-pending.
			require.NoError(t, st.DeleteRunPodBindings(t.Context(), runID))
			rec = doPresign(t, s, sidecarToken(), runID, api.PresignRequest{Kind: "artifact", Op: "put", Name: "a"})
			requireObjectError(t, rec, http.StatusConflict, "run-finished")

			rows, err := st.ListAuditLogs(t.Context(), 1000, 0)
			require.NoError(t, err)
			n := 0
			for _, row := range rows {
				if row.Action == "objects.presign" && row.Status == http.StatusConflict {
					n++
					assert.Equal(t, runID, row.Resource)
				}
			}
			assert.Equal(t, 2, n, "each run-finished refusal is audited")
		})
	}
}

// A cancelled run's cleanup (hook drains, finally:) still transfers through
// the sidecar: access continues while the Pod is bound, for a bounded window.
func TestPresign_SidecarCancelledRunGrace(t *testing.T) {
	s, st, _ := presignTestServer(t, sidecarPresignConfig([]string{"ci"}))
	runID := seedRun(t, st, "job-a", "")
	require.NoError(t, st.UpsertRunPodBinding(t.Context(), runID, "ucd-run-abc", "job-pod-uid"))
	updated, err := st.FinishRun(t.Context(), runID, api.RunCancelled)
	require.NoError(t, err)
	require.True(t, updated)
	req := api.PresignRequest{Kind: "artifact", Op: "put", Name: "a"}

	out := decodePresign(t, doPresign(t, s, sidecarToken(), runID, req))
	require.NotNil(t, out.Request, "within the window a bound Pod may still upload")

	// A Pod that is not the bound one is still refused.
	require.NoError(t, st.ReplaceRunPodBindings(t.Context(), runID, []store.RunPodBinding{{PodName: "ucd-run-other", PodUID: "other-pod-uid"}}))
	requireObjectError(t, doPresign(t, s, sidecarToken(), runID, req), http.StatusForbidden, "forbidden")
	require.NoError(t, st.UpsertRunPodBinding(t.Context(), runID, "ucd-run-abc", "job-pod-uid"))

	// Past the window the binding no longer helps.
	s.objectNow = func() time.Time { return time.Now().Add(cancelledRunObjectGrace + time.Minute) }
	requireObjectError(t, doPresign(t, s, sidecarToken(), runID, req), http.StatusConflict, "run-finished")
}

// Once the claiming agent finishes the cancelled run its bindings are gone
// (handleAgentFinishRun's alreadyFinalized path), and a cancelled run with no
// binding is refused as finished, not as binding-pending.
func TestPresign_SidecarCancelledRunAfterAgentFinish(t *testing.T) {
	s, st, _ := presignTestServer(t, sidecarPresignConfig([]string{"ci"}))
	token := issueAgentAccessForTest(t, st, "agent-a", nil, nil)
	runID := seedClaimedRun(t, st, "job-a", "agent-a", "")
	require.NoError(t, st.UpsertRunPodBinding(t.Context(), runID, "ucd-run-abc", "job-pod-uid"))
	updated, err := st.FinishRun(t.Context(), runID, api.RunCancelled)
	require.NoError(t, err)
	require.True(t, updated)
	req := api.PresignRequest{Kind: "artifact", Op: "put", Name: "a"}
	require.NotNil(t, decodePresign(t, doPresign(t, s, sidecarToken(), runID, req)).Request)

	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-a/runs/"+runID+"/finish", bytes.NewReader([]byte(`{"status":"Cancelled"}`)))
	r.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, r)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "alreadyFinalized")

	bindings, err := st.ListRunPodBindings(t.Context(), runID)
	require.NoError(t, err)
	assert.Empty(t, bindings)
	requireObjectError(t, doPresign(t, s, sidecarToken(), runID, req), http.StatusConflict, "run-finished")
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

// A run holds its claim Pod plus its scope Pods: the sidecar of any of them
// presigns for the run.
func TestPresign_ScopePodBoundAlongsideClaimPodSucceeds(t *testing.T) {
	s, st, _ := presignTestServer(t, sidecarPresignConfig([]string{"ci"}))
	runID := seedRun(t, st, "job-a", "")
	require.NoError(t, st.ReplaceRunPodBindings(t.Context(), runID, []store.RunPodBinding{
		{PodName: "ucd-run-claim", PodUID: "claim-pod-uid"},
		{PodName: "ucd-scope-1", PodUID: "job-pod-uid"}, // the presenting sidecar's Pod
	}))
	out := decodePresign(t, doPresign(t, s, sidecarToken(), runID, api.PresignRequest{Kind: "artifact", Op: "put", Name: "a"}))
	require.NotNil(t, out.Request)
}

// A Pod bound to run A stays unable to presign for run B, whether B has no
// binding yet (409) or other Pods bound (403).
func TestPresign_PodBoundToRunACannotPresignRunB(t *testing.T) {
	s, st, _ := presignTestServer(t, sidecarPresignConfig([]string{"ci"}))
	runA := seedRun(t, st, "job-a", "")
	runB := seedRun(t, st, "job-b", "")
	require.NoError(t, st.UpsertRunPodBinding(t.Context(), runA, "ucd-run-a", "job-pod-uid"))

	rec := doPresign(t, s, sidecarToken(), runB, api.PresignRequest{Kind: "artifact", Op: "put", Name: "a"})
	requireObjectError(t, rec, http.StatusConflict, "binding-pending")

	require.NoError(t, st.ReplaceRunPodBindings(t.Context(), runB, []store.RunPodBinding{
		{PodName: "ucd-run-b", PodUID: "b-uid"}, {PodName: "ucd-scope-b", PodUID: "b-scope-uid"},
	}))
	rec = doPresign(t, s, sidecarToken(), runB, api.PresignRequest{Kind: "artifact", Op: "put", Name: "a"})
	requireObjectError(t, rec, http.StatusForbidden, "forbidden")
}
