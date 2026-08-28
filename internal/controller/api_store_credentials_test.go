package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// jobPod is the store-credentials analogue of boundPod(): a job Pod running
// in the "ci" namespace under a job ServiceAccount, distinct from the
// agent's own "unified-cd"/"unified-cd-k8s-agent" identity that boundPod()
// represents — the two are different trust domains, which is exactly the
// distinction TestStoreCredentials_RejectsAnEnrollmentToken and
// TestAgentEnrollment_RejectsAStoreCredentialToken exist to enforce.
func jobPod() *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "ucd-run-abc", Namespace: "ci", UID: "job-pod-uid"}, Spec: corev1.PodSpec{ServiceAccountName: "ci-runner"}}
}

func storeCredentialTestServer(clusters []StoreCredentialCluster, s3 *objectstore.S3Config) *Server {
	return NewServer(Config{StoreCredentialClusters: clusters, StoreCredentialS3: s3}, nil)
}

// storeCredsFakeClient returns a fake Kubernetes clientset that authenticates
// any token as jobPod()'s identity ("ci"/"ci-runner", Pod "ucd-run-abc" /
// UID "job-pod-uid") against api.KubernetesStoreCredentialAudience — the
// same fixture TestStoreCredentials_ReturnsCredentialsForAValidToken above
// builds inline, factored out so the run-binding tests below (which also
// need a real *store.Postgres, via newTestServerWithConfig, rather than the
// nil store storeCredentialTestServer passes) don't each repeat it.
func storeCredsFakeClient() *fake.Clientset {
	client := fake.NewSimpleClientset(jobPod())
	client.Fake.PrependReactor("create", "tokenreviews", tokenReviewReactor(true, []string{api.KubernetesStoreCredentialAudience}, "system:serviceaccount:ci:ci-runner", "sa-uid"))
	return client
}

func testS3Config() *objectstore.S3Config {
	return &objectstore.S3Config{Endpoint: "s3.example.internal:9000", Bucket: "unified-cd-artifacts", AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secretkeyexample"}
}

// The security property, at the endpoint: an enrollment-audience token must
// not buy store credentials.
func TestStoreCredentials_RejectsAnEnrollmentToken(t *testing.T) {
	client := fake.NewSimpleClientset(jobPod())
	client.Fake.PrependReactor("create", "tokenreviews", tokenReviewReactor(true, []string{KubernetesEnrollmentAudience}, "system:serviceaccount:ci:ci-runner", "sa-uid"))
	s := storeCredentialTestServer([]StoreCredentialCluster{{Cluster: "prod", Client: client, Namespaces: []string{"ci"}, ServiceAccounts: []string{"ci-runner"}}}, testS3Config())

	body, err := json.Marshal(api.StoreCredentialsRequest{Token: projectedToken("ci", "ci-runner", "sa-uid", "ucd-run-abc", "job-pod-uid")})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/store-credentials", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// And the converse, asserted against the enrollment endpoint.
func TestAgentEnrollment_RejectsAStoreCredentialToken(t *testing.T) {
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

// A valid token from a Pod in a permitted namespace gets credentials.
func TestStoreCredentials_ReturnsCredentialsForAValidToken(t *testing.T) {
	client := fake.NewSimpleClientset(jobPod())
	client.Fake.PrependReactor("create", "tokenreviews", tokenReviewReactor(true, []string{api.KubernetesStoreCredentialAudience}, "system:serviceaccount:ci:ci-runner", "sa-uid"))
	s3 := testS3Config()
	s := storeCredentialTestServer([]StoreCredentialCluster{{Cluster: "prod", Client: client, Namespaces: []string{"ci"}, ServiceAccounts: []string{"ci-runner"}}}, s3)

	body, err := json.Marshal(api.StoreCredentialsRequest{Token: projectedToken("ci", "ci-runner", "sa-uid", "ucd-run-abc", "job-pod-uid"), RunID: "run-1"})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/store-credentials", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp api.StoreCredentialsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, s3.Endpoint, resp.Endpoint)
	assert.Equal(t, s3.Bucket, resp.Bucket)
	assert.Equal(t, s3.AccessKeyID, resp.AccessKey)
	assert.Equal(t, s3.SecretAccessKey, resp.SecretKey)
	assert.True(t, resp.ExpiresAt.IsZero(), "a passthrough credential does not expire")
}

// A Pod correctly authenticated for a cluster, but outside its configured
// namespace/ServiceAccount allowlist, must still be rejected: the allowlist
// is what "restrict which Pods may ask" means in practice, not merely proof
// that the token was minted somewhere in a trusted cluster.
func TestStoreCredentials_RejectsAnUnlistedNamespace(t *testing.T) {
	client := fake.NewSimpleClientset(jobPod())
	client.Fake.PrependReactor("create", "tokenreviews", tokenReviewReactor(true, []string{api.KubernetesStoreCredentialAudience}, "system:serviceaccount:ci:ci-runner", "sa-uid"))
	s := storeCredentialTestServer([]StoreCredentialCluster{{Cluster: "prod", Client: client, Namespaces: []string{"other-namespace"}, ServiceAccounts: []string{"ci-runner"}}}, testS3Config())

	body, err := json.Marshal(api.StoreCredentialsRequest{Token: projectedToken("ci", "ci-runner", "sa-uid", "ucd-run-abc", "job-pod-uid")})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/store-credentials", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// TestStoreCredentials_BoundPodSucceeds is the positive half of the
// run-binding enforcement: a Pod that IS the one bound to the named run
// (matched on PodUID — see api.PodBinding's doc comment) gets its
// credentials, exactly as before this feature existed.
func TestStoreCredentials_BoundPodSucceeds(t *testing.T) {
	s, pg := newTestServerWithConfig(t, Config{
		StoreCredentialClusters: []StoreCredentialCluster{{Cluster: "prod", Client: storeCredsFakeClient(), Namespaces: []string{"ci"}, ServiceAccounts: []string{"ci-runner"}}},
		StoreCredentialS3:       testS3Config(),
	})
	_, err := pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "", "")
	require.NoError(t, err)
	require.NoError(t, pg.UpsertRunPodBinding(t.Context(), run.ID, "ucd-run-abc", "job-pod-uid"))

	body, err := json.Marshal(api.StoreCredentialsRequest{Token: projectedToken("ci", "ci-runner", "sa-uid", "ucd-run-abc", "job-pod-uid"), RunID: run.ID})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/store-credentials", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// TestStoreCredentials_RejectsAMismatchedPod is the core of this feature: a
// Pod that is fully authenticated (a valid token, in an allowed
// namespace/ServiceAccount) but is NOT the Pod run_pod_bindings names for the
// RunID it asks about must still be refused. This is the "a Pod asking for a
// run it is not executing is refused" requirement, and it holds regardless of
// RequireRunBinding — a mismatch is a contradiction, not an absence of
// information, and the permissive default only ever concerns the latter.
func TestStoreCredentials_RejectsAMismatchedPod(t *testing.T) {
	s, pg := newTestServerWithConfig(t, Config{
		StoreCredentialClusters: []StoreCredentialCluster{{Cluster: "prod", Client: storeCredsFakeClient(), Namespaces: []string{"ci"}, ServiceAccounts: []string{"ci-runner"}}},
		StoreCredentialS3:       testS3Config(),
	})
	_, err := pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "", "")
	require.NoError(t, err)
	// The run is bound to a DIFFERENT Pod's UID than the one presenting the token below.
	require.NoError(t, pg.UpsertRunPodBinding(t.Context(), run.ID, "ucd-run-other", "some-other-pod-uid"))

	body, err := json.Marshal(api.StoreCredentialsRequest{Token: projectedToken("ci", "ci-runner", "sa-uid", "ucd-run-abc", "job-pod-uid"), RunID: run.ID})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/store-credentials", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// TestStoreCredentials_UnknownBindingIsPermissiveByDefault asserts the
// explicit, deliberate behavior for "no k8s agent has ever reported a
// binding for this RunID" (RequireRunBinding's default, false): the request
// is ALLOWED, not rejected — see Config.RequireRunBinding's doc comment for
// why (mixed-version fleets / the startup window before a Pod's first
// heartbeat would otherwise fail fleet-wide).
func TestStoreCredentials_UnknownBindingIsPermissiveByDefault(t *testing.T) {
	s, pg := newTestServerWithConfig(t, Config{
		StoreCredentialClusters: []StoreCredentialCluster{{Cluster: "prod", Client: storeCredsFakeClient(), Namespaces: []string{"ci"}, ServiceAccounts: []string{"ci-runner"}}},
		StoreCredentialS3:       testS3Config(),
		RequireRunBinding:       false,
	})
	_, err := pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "", "")
	require.NoError(t, err)
	// Deliberately no UpsertRunPodBinding call: run.ID has no binding at all.

	body, err := json.Marshal(api.StoreCredentialsRequest{Token: projectedToken("ci", "ci-runner", "sa-uid", "ucd-run-abc", "job-pod-uid"), RunID: run.ID})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/store-credentials", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// TestStoreCredentials_UnknownBindingRejectedWhenRequired flips
// RequireRunBinding to true and asserts the same unbound RunID is now
// refused — the operator-opt-in strict mode for a fleet that is fully
// upgraded and has no host agents calling this endpoint.
func TestStoreCredentials_UnknownBindingRejectedWhenRequired(t *testing.T) {
	s, pg := newTestServerWithConfig(t, Config{
		StoreCredentialClusters: []StoreCredentialCluster{{Cluster: "prod", Client: storeCredsFakeClient(), Namespaces: []string{"ci"}, ServiceAccounts: []string{"ci-runner"}}},
		StoreCredentialS3:       testS3Config(),
		RequireRunBinding:       true,
	})
	_, err := pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "", "")
	require.NoError(t, err)

	body, err := json.Marshal(api.StoreCredentialsRequest{Token: projectedToken("ci", "ci-runner", "sa-uid", "ucd-run-abc", "job-pod-uid"), RunID: run.ID})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/store-credentials", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// TestStoreCredentials_EmptyRunIDIsPermissiveByDefault covers the other
// "cannot enforce" case: an old sidecar (pre-RunID-threading) or one that
// simply sends none. Same bucket as an unknown binding, same default.
func TestStoreCredentials_EmptyRunIDIsPermissiveByDefault(t *testing.T) {
	s, _ := newTestServerWithConfig(t, Config{
		StoreCredentialClusters: []StoreCredentialCluster{{Cluster: "prod", Client: storeCredsFakeClient(), Namespaces: []string{"ci"}, ServiceAccounts: []string{"ci-runner"}}},
		StoreCredentialS3:       testS3Config(),
	})

	body, err := json.Marshal(api.StoreCredentialsRequest{Token: projectedToken("ci", "ci-runner", "sa-uid", "ucd-run-abc", "job-pod-uid")})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/store-credentials", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// The controller has no store configured: a clear error naming what to set,
// not an empty credential the sidecar will fail to sign with later.
func TestStoreCredentials_ErrorsWhenNoStoreIsConfigured(t *testing.T) {
	client := fake.NewSimpleClientset(jobPod())
	client.Fake.PrependReactor("create", "tokenreviews", tokenReviewReactor(true, []string{api.KubernetesStoreCredentialAudience}, "system:serviceaccount:ci:ci-runner", "sa-uid"))
	s := storeCredentialTestServer([]StoreCredentialCluster{{Cluster: "prod", Client: client, Namespaces: []string{"ci"}, ServiceAccounts: []string{"ci-runner"}}}, nil)

	body, err := json.Marshal(api.StoreCredentialsRequest{Token: projectedToken("ci", "ci-runner", "sa-uid", "ucd-run-abc", "job-pod-uid")})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/store-credentials", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "UNIFIED_S3_ENDPOINT")
}
