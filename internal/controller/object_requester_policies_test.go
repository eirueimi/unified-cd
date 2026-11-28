package controller

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/kubernetes/fake"
)

// countingStoreClient is storeCredsFakeClient with a TokenReview counter.
func countingStoreClient(count *atomic.Int32) *fake.Clientset {
	client := fake.NewSimpleClientset(jobPod())
	inner := tokenReviewReactor(true, []string{api.KubernetesStoreCredentialAudience}, "system:serviceaccount:ci:ci-runner", "sa-uid")
	client.Fake.PrependReactor("create", "tokenreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		count.Add(1)
		return inner(a)
	})
	return client
}

func presignAs(t *testing.T, cfg Config) *httpResult {
	t.Helper()
	s, st, _ := presignTestServer(t, cfg)
	runID := seedRun(t, st, "job-a", "")
	require.NoError(t, st.UpsertRunPodBinding(t.Context(), runID, "ucd-run-abc", "job-pod-uid"))
	rec := doPresign(t, s, sidecarToken(), runID, api.PresignRequest{Kind: "artifact", Op: "put", Name: "a"})
	return &httpResult{code: rec.Code, body: rec.Body.String()}
}

type httpResult struct {
	code int
	body string
}

func captureTextLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// F1: a Pod allowed only by a later entry of the same cluster is allowed.
func TestPresign_SecondPolicyEntryOnSameClusterAllows(t *testing.T) {
	var reviews atomic.Int32
	client := countingStoreClient(&reviews)
	cfg := Config{StoreCredentialClusters: []StoreCredentialCluster{
		{Cluster: "prod", Client: client, Namespaces: []string{"ci"}, ServiceAccounts: []string{"default"}},
		{Cluster: "prod", Client: client, Namespaces: []string{"ci"}, ServiceAccounts: []string{"ci-runner"}},
	}}
	res := presignAs(t, cfg)
	assert.Equal(t, http.StatusOK, res.code, res.body)
	assert.EqualValues(t, 1, reviews.Load(), "one TokenReview per cluster per request")
}

// F1: no entry of the cluster matches: 403, and it is logged with the cause.
func TestPresign_NoPolicyEntryMatchesIs403AndLogged(t *testing.T) {
	logs := captureTextLogs(t)
	var reviews atomic.Int32
	client := countingStoreClient(&reviews)
	cfg := Config{StoreCredentialClusters: []StoreCredentialCluster{
		{Cluster: "prod", Client: client, Namespaces: []string{"ci"}, ServiceAccounts: []string{"default"}},
		{Cluster: "prod", Client: client, Namespaces: []string{"other"}, ServiceAccounts: []string{"ci-runner"}},
	}}
	res := presignAs(t, cfg)
	assert.Equal(t, http.StatusForbidden, res.code, res.body)
	assert.Contains(t, res.body, "this namespace or service account may not request object URLs")
	assert.EqualValues(t, 1, reviews.Load())
	out := logs.String()
	assert.Contains(t, out, "level=WARN")
	assert.Contains(t, out, "namespace=ci")
	assert.Contains(t, out, "serviceAccount=ci-runner")
	assert.Contains(t, out, "pod=ucd-run-abc")
	assert.Contains(t, out, "cluster=prod")
}

// Multi-cluster: an entry on the first cluster does not authenticate; the
// second cluster's entries decide.
func TestPresign_MultiClusterOrdering(t *testing.T) {
	rejecting := fake.NewSimpleClientset(jobPod())
	rejecting.Fake.PrependReactor("create", "tokenreviews", tokenReviewReactor(false, nil, "", ""))
	var reviews atomic.Int32
	good := countingStoreClient(&reviews)
	cfg := Config{StoreCredentialClusters: []StoreCredentialCluster{
		{Cluster: "a", Client: rejecting, Namespaces: []string{"ci"}, ServiceAccounts: []string{"ci-runner"}},
		{Cluster: "a", Client: rejecting, Namespaces: []string{"ci"}, ServiceAccounts: []string{"other"}},
		{Cluster: "b", Client: good, Namespaces: []string{"ci"}, ServiceAccounts: []string{"default"}},
		{Cluster: "b", Client: good, Namespaces: []string{"ci"}, ServiceAccounts: []string{"ci-runner"}},
	}}
	res := presignAs(t, cfg)
	assert.Equal(t, http.StatusOK, res.code, res.body)
	assert.EqualValues(t, 1, reviews.Load())
}

// F8: one cluster unavailable, another rejects: retryable 503, not 401.
func TestPresign_UnavailableClusterBeatsRejection(t *testing.T) {
	down := fake.NewSimpleClientset()
	down.Fake.PrependReactor("create", "tokenreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})
	rejecting := fake.NewSimpleClientset()
	rejecting.Fake.PrependReactor("create", "tokenreviews", tokenReviewReactor(false, nil, "", ""))
	for _, order := range [][]string{{"down", "rej"}, {"rej", "down"}} {
		var cs []StoreCredentialCluster
		for _, n := range order {
			c := down
			if n == "rej" {
				c = rejecting
			}
			cs = append(cs, StoreCredentialCluster{Cluster: n, Client: c, Namespaces: []string{"ci"}, ServiceAccounts: []string{"ci-runner"}})
		}
		res := presignAs(t, Config{StoreCredentialClusters: cs})
		assert.Equal(t, http.StatusServiceUnavailable, res.code, "%v: %s", order, res.body)
	}
}

// F4: every cluster rejects: 401 with the per-cluster reason logged.
func TestPresign_AllClustersRejectLogsReason(t *testing.T) {
	logs := captureTextLogs(t)
	rejecting := fake.NewSimpleClientset()
	rejecting.Fake.PrependReactor("create", "tokenreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.TokenReview{Status: authv1.TokenReviewStatus{Authenticated: false}}, nil
	})
	res := presignAs(t, Config{StoreCredentialClusters: []StoreCredentialCluster{{Cluster: "prod", Client: rejecting, Namespaces: []string{"ci"}, ServiceAccounts: []string{"ci-runner"}}}})
	assert.Equal(t, http.StatusUnauthorized, res.code)
	out := logs.String()
	assert.Contains(t, out, "level=WARN")
	assert.Contains(t, out, "cluster=prod")
	assert.Contains(t, out, "token review")
	assert.NotContains(t, out, sidecarToken())
}

// F4: no cluster configured: 401 and a WARN naming the missing configuration.
func TestPresign_NoClusterConfiguredLogs(t *testing.T) {
	logs := captureTextLogs(t)
	res := presignAs(t, Config{})
	assert.Equal(t, http.StatusUnauthorized, res.code)
	assert.Contains(t, logs.String(), "kubernetesStoreCredentialPolicies")
}

// F4: the enrollment 403 for a namespace/ServiceAccount the policy does not
// list is logged with the cause and the identity, never the token.
func TestAgentEnrollment_RejectedLogsReason(t *testing.T) {
	logs := captureTextLogs(t)
	policy := kubernetesEnrollmentPolicy()
	policy.Name = "prod-policy"
	policy.Enabled = true
	policy.ProviderConfig = []byte(`{"cluster":"prod"}`)

	pod := boundPod()
	pod.Spec.ServiceAccountName = "other-sa"
	client := fake.NewSimpleClientset(pod)
	client.Fake.PrependReactor("create", "tokenreviews", tokenReviewReactor(true, []string{KubernetesEnrollmentAudience}, "system:serviceaccount:unified-cd:other-sa", "sa-uid"))
	verifier := NewKubernetesEnrollmentVerifier("prod", client)
	s := NewServer(Config{KubernetesEnrollmentVerifiers: map[string]KubernetesEnrollmentVerifier{"prod": verifier}}, unavailableKubernetesPolicyStore{policy: policy})

	token := projectedToken("unified-cd", "other-sa", "sa-uid", "agent-0", "pod-uid")
	body := []byte(`{"provider":"kubernetes","policy":"prod-policy"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/enroll", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	out := logs.String()
	assert.Contains(t, out, "level=WARN")
	assert.Contains(t, out, "policy=prod-policy")
	assert.Contains(t, out, "cluster=prod")
	assert.Contains(t, out, "other-sa")
	assert.NotContains(t, out, token)
}
