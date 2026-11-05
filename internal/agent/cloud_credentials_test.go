package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var cloudTestNow = time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)

// fakeMetadata serves GCP, Azure and AWS IMDS endpoints and counts proof fetches.
type fakeMetadata struct {
	srv   *httptest.Server
	mu    sync.Mutex
	hits  int
	proof int
}

func newFakeMetadata(t *testing.T) *fakeMetadata {
	t.Helper()
	m := &fakeMetadata{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.hits++
		m.mu.Unlock()
		switch {
		case r.URL.Path == "/computeMetadata/v1/instance/service-accounts/default/identity":
			if r.Header.Get("Metadata-Flavor") != "Google" || r.URL.Query().Get("audience") != "unified-cd" || r.URL.Query().Get("format") != "full" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			m.count()
			_, _ = w.Write([]byte("gcp.jwt.token\n"))
		case r.URL.Path == "/metadata/identity/oauth2/token":
			if r.Header.Get("Metadata") != "true" || r.URL.Query().Get("resource") != "unified-cd" || r.URL.Query().Get("api-version") != "2018-02-01" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			m.count()
			_, _ = w.Write([]byte(`{"access_token":"azure.jwt.token"}`))
		case r.URL.Path == "/latest/api/token":
			if r.Method != http.MethodPut || r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds") != "60" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte("imds-token"))
		case strings.HasPrefix(r.URL.Path, "/latest/meta-data/"):
			if r.Header.Get("X-aws-ec2-metadata-token") != "imds-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			switch r.URL.Path {
			case "/latest/meta-data/placement/region":
				_, _ = w.Write([]byte("eu-west-1"))
			case "/latest/meta-data/iam/security-credentials/":
				_, _ = w.Write([]byte("ci-agent\n"))
			case "/latest/meta-data/iam/security-credentials/ci-agent":
				m.count()
				_, _ = w.Write([]byte(`{"AccessKeyId":"AKIDEXAMPLE","SecretAccessKey":"wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY","Token":"session-token"}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *fakeMetadata) count() { m.mu.Lock(); m.proof++; m.mu.Unlock() }
func (m *fakeMetadata) proofs() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.proof
}

type fakeController struct {
	srv      *httptest.Server
	mu       sync.Mutex
	requests []api.AgentEnrollRequest
	auths    []string
	statuses []int // consumed per request; empty = 200
	expires  time.Time
}

func newFakeController(t *testing.T) *fakeController {
	t.Helper()
	c := &fakeController{expires: cloudTestNow.Add(time.Hour)}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req api.AgentEnrollRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		c.mu.Lock()
		c.requests = append(c.requests, req)
		c.auths = append(c.auths, r.Header.Get("Authorization"))
		status := http.StatusOK
		if len(c.statuses) > 0 {
			status, c.statuses = c.statuses[0], c.statuses[1:]
		}
		n := len(c.requests)
		expires := c.expires
		c.mu.Unlock()
		if r.URL.Path != "/api/v1/agents/enroll" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_ = json.NewEncoder(w).Encode(api.AgentTokenResponse{AgentID: "agent-cloud01", AccessToken: "access-" + string(rune('0'+n)), AccessExpiresAt: expires, Labels: []string{"kind:linux"}})
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *fakeController) snapshot() ([]api.AgentEnrollRequest, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]api.AgentEnrollRequest(nil), c.requests...), append([]string(nil), c.auths...)
}

func newTestCloudSource(t *testing.T, provider string, m *fakeMetadata, c *fakeController, now *time.Time) *CloudCredentialSource {
	t.Helper()
	s, err := NewCloudCredentialSource(CloudCredentialSourceConfig{
		Server: c.srv.URL, Provider: provider, Policy: "ci", Audience: "unified-cd",
		Labels: []string{"kind:linux"}, Capabilities: []string{"docker"},
		MetadataBaseURL: m.srv.URL,
		Now:             func() time.Time { return *now },
		Jitter:          func() time.Duration { return time.Minute },
	})
	require.NoError(t, err)
	s.sleep = func(context.Context, time.Duration) error { return nil }
	return s
}

func TestCloudCredentialSource_Providers(t *testing.T) {
	for _, tc := range []struct{ provider, bearer string }{{"gcp", "Bearer gcp.jwt.token"}, {"azure", "Bearer azure.jwt.token"}, {"aws", ""}} {
		t.Run(tc.provider, func(t *testing.T) {
			m, c, now := newFakeMetadata(t), newFakeController(t), cloudTestNow
			s := newTestCloudSource(t, tc.provider, m, c, &now)
			tok, err := s.Token(context.Background())
			require.NoError(t, err)
			assert.Equal(t, "access-1", tok)
			assert.Equal(t, "agent-cloud01", s.AgentID())
			assert.Equal(t, []string{"kind:linux"}, s.Labels())

			reqs, auths := c.snapshot()
			require.Len(t, reqs, 1)
			assert.Equal(t, tc.provider, reqs[0].Provider)
			assert.Equal(t, "ci", reqs[0].Policy)
			assert.Equal(t, []string{"kind:linux"}, reqs[0].Labels)
			assert.Equal(t, []string{"docker"}, reqs[0].Capabilities)
			assert.Equal(t, tc.bearer, auths[0])
			if tc.provider == "aws" {
				require.NotNil(t, reqs[0].AWS)
				assert.Equal(t, "POST", reqs[0].AWS.Method)
				assert.Equal(t, "https://sts.eu-west-1.amazonaws.com/", reqs[0].AWS.URL)
				body, err := base64.StdEncoding.DecodeString(reqs[0].AWS.Body)
				require.NoError(t, err)
				assert.Equal(t, "Action=GetCallerIdentity&Version=2011-06-15", string(body))
				assert.Equal(t, []string{"unified-cd"}, reqs[0].AWS.Headers["X-Unified-CD-Audience"])
				assert.Equal(t, []string{"session-token"}, reqs[0].AWS.Headers["X-Amz-Security-Token"])
				auth := reqs[0].AWS.Headers["Authorization"]
				require.Len(t, auth, 1)
				assert.Contains(t, auth[0], "SignedHeaders=content-type;host;x-amz-date;x-amz-security-token;x-unified-cd-audience,")
				assert.Contains(t, auth[0], "Credential=AKIDEXAMPLE/20261103/eu-west-1/sts/aws4_request")
				assert.NotContains(t, reqs[0].AWS.Headers, "Host")
			} else {
				assert.Nil(t, reqs[0].AWS)
			}
		})
	}
}

func TestCloudCredentialSource_CachingAndInvalidate(t *testing.T) {
	m, c, now := newFakeMetadata(t), newFakeController(t), cloudTestNow
	s := newTestCloudSource(t, "gcp", m, c, &now)
	ctx := context.Background()

	first, err := s.Token(ctx)
	require.NoError(t, err)
	now = now.Add(30 * time.Minute) // 30m left > 15m lead + 1m jitter
	again, err := s.Token(ctx)
	require.NoError(t, err)
	assert.Equal(t, first, again)
	assert.Equal(t, 1, m.proofs())

	now = now.Add(20 * time.Minute) // 10m left < lead time
	refreshed, err := s.Token(ctx)
	require.NoError(t, err)
	assert.NotEqual(t, first, refreshed)
	assert.Equal(t, 2, m.proofs(), "a fresh proof is fetched for every exchange")

	s.Invalidate()
	c.mu.Lock()
	c.expires = now.Add(time.Hour)
	c.mu.Unlock()
	_, err = s.Token(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, m.proofs())
}

func TestCloudCredentialSource_RetryPolicy(t *testing.T) {
	t.Run("401 is retried", func(t *testing.T) {
		m, c, now := newFakeMetadata(t), newFakeController(t), cloudTestNow
		c.statuses = []int{http.StatusUnauthorized, http.StatusOK}
		s := newTestCloudSource(t, "azure", m, c, &now)
		_, err := s.Token(context.Background())
		require.NoError(t, err)
		reqs, _ := c.snapshot()
		assert.Len(t, reqs, 2)
		assert.Equal(t, 2, m.proofs(), "each retry fetches a new proof")
	})
	t.Run("403 is not retried", func(t *testing.T) {
		m, c, now := newFakeMetadata(t), newFakeController(t), cloudTestNow
		c.statuses = []int{http.StatusForbidden, http.StatusOK}
		s := newTestCloudSource(t, "gcp", m, c, &now)
		_, err := s.Token(context.Background())
		require.Error(t, err)
		reqs, _ := c.snapshot()
		assert.Len(t, reqs, 1)
		assert.Equal(t, "gcp enrollment request failed", err.Error())
	})
	t.Run("gives up after three attempts", func(t *testing.T) {
		m, c, now := newFakeMetadata(t), newFakeController(t), cloudTestNow
		c.statuses = []int{503, 503, 503, 200}
		s := newTestCloudSource(t, "gcp", m, c, &now)
		_, err := s.Token(context.Background())
		require.Error(t, err)
		reqs, _ := c.snapshot()
		assert.Len(t, reqs, 3)
	})
}

func TestCloudCredentialSource_GenericProofError(t *testing.T) {
	c, now := newFakeController(t), cloudTestNow
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	s, err := NewCloudCredentialSource(CloudCredentialSourceConfig{Server: c.srv.URL, Provider: "gcp", Policy: "ci", Audience: "unified-cd", MetadataBaseURL: dead.URL, Now: func() time.Time { return now }})
	require.NoError(t, err)
	_, err = s.Token(context.Background())
	require.Error(t, err)
	assert.Equal(t, "fetch cloud identity proof failed", err.Error())
	reqs, _ := c.snapshot()
	assert.Empty(t, reqs)
}

func TestNewCloudCredentialSource_Validation(t *testing.T) {
	ok := CloudCredentialSourceConfig{Server: "http://x", Provider: "gcp", Policy: "p", Audience: "a"}
	_, err := NewCloudCredentialSource(ok)
	require.NoError(t, err)
	for name, mutate := range map[string]func(*CloudCredentialSourceConfig){
		"unknown provider": func(c *CloudCredentialSourceConfig) { c.Provider = "kubernetes" },
		"empty policy":     func(c *CloudCredentialSourceConfig) { c.Policy = "" },
		"empty audience":   func(c *CloudCredentialSourceConfig) { c.Audience = "" },
	} {
		cfg := ok
		mutate(&cfg)
		_, err := NewCloudCredentialSource(cfg)
		assert.Error(t, err, name)
	}
}
