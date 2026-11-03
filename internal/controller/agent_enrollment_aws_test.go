package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	awsTestAudience = "unified-cd"
	awsTestAccount  = "123456789012"
	awsTestRole     = "ci-agent"
	awsTestSession  = "i-0abc1234def567890"
)

type fakeSTS struct {
	srv    *httptest.Server
	mu     sync.Mutex
	hits   int
	hosts  []string
	status int
	arn    string
}

func newFakeSTS(t *testing.T) *fakeSTS {
	t.Helper()
	f := &fakeSTS{status: http.StatusOK, arn: fmt.Sprintf("arn:aws:sts::%s:assumed-role/%s/%s", awsTestAccount, awsTestRole, awsTestSession)}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hits++
		f.hosts = append(f.hosts, r.Host)
		status, arn := f.status, f.arn
		f.mu.Unlock()
		w.WriteHeader(status)
		if status == http.StatusOK {
			fmt.Fprintf(w, `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult><Arn>%s</Arn><UserId>AROAEXAMPLE:%s</UserId><Account>%s</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`, arn, awsTestSession, awsTestAccount)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSTS) verifier() CloudEnrollmentVerifier {
	addr := strings.TrimPrefix(f.srv.URL, "https://")
	return NewAWSEnrollmentVerifier(f.srv.Client(), func(string) string { return addr })
}

func (f *fakeSTS) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

func awsTestPolicy(t *testing.T, regions []string) store.AgentEnrollmentPolicy {
	t.Helper()
	cfg, _ := json.Marshal(store.AWSPolicyConfig{Audience: awsTestAudience, STSRegions: regions})
	cons, _ := json.Marshal(store.AWSSubjectConstraints{AccountIDs: []string{awsTestAccount}, RoleNames: []string{awsTestRole}})
	return store.AgentEnrollmentPolicy{Name: "aws-ci", Provider: "aws", ProviderConfig: cfg, SubjectConstraints: cons, AgentIDTemplate: "aws:{account}:{instanceId}",
		AllowedLabels: []string{"kind:linux", "pool:ci"}, RequiredLabels: []string{"kind:linux"}, AccessTokenTTL: time.Hour, Enabled: true}
}

func awsTestProof() *api.AWSSignedRequest {
	return &api.AWSSignedRequest{
		Method: http.MethodPost,
		URL:    "https://sts.amazonaws.com/",
		Headers: map[string][]string{
			"Content-Type":          {"application/x-www-form-urlencoded; charset=utf-8"},
			"X-Amz-Date":            {"20261103T000000Z"},
			"X-Unified-Cd-Audience": {awsTestAudience},
			"Authorization":         {"AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE/20261103/us-east-1/sts/aws4_request, SignedHeaders=content-type;host;x-amz-date;x-unified-cd-audience, Signature=abcd"},
		},
		Body: base64.StdEncoding.EncodeToString([]byte("Action=GetCallerIdentity&Version=2011-06-15")),
	}
}

func TestAWSVerifier_ValidProof(t *testing.T) {
	sts := newFakeSTS(t)
	id, err := sts.verifier().Verify(context.Background(), CloudEnrollmentProof{AWS: awsTestProof()}, awsTestPolicy(t, nil))
	require.NoError(t, err)
	assert.Equal(t, "aws:123456789012:i-0abc1234def567890", id.AgentID)
	require.Equal(t, 1, sts.requests())
	assert.Equal(t, []string{"sts.amazonaws.com"}, sts.hosts, "request is bound to the validated host")
}

func TestAWSVerifier_OtherPartitions(t *testing.T) {
	for _, partition := range []string{"aws-us-gov", "aws-cn"} {
		sts := newFakeSTS(t)
		sts.arn = fmt.Sprintf("arn:%s:sts::%s:assumed-role/%s/%s", partition, awsTestAccount, awsTestRole, awsTestSession)
		_, err := sts.verifier().Verify(context.Background(), CloudEnrollmentProof{AWS: awsTestProof()}, awsTestPolicy(t, nil))
		require.NoError(t, err, partition)
	}
}

func TestAWSVerifier_Hosts(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		regions []string
		ok      bool
	}{
		{"regional, no restriction", "https://sts.eu-west-1.amazonaws.com/", nil, true},
		{"regional, listed", "https://sts.eu-west-1.amazonaws.com/", []string{"eu-west-1"}, true},
		{"regional, not listed", "https://sts.eu-west-1.amazonaws.com/", []string{"us-east-1"}, false},
		{"global counts as us-east-1", "https://sts.amazonaws.com/", []string{"us-east-1"}, true},
		{"global, region list excludes us-east-1", "https://sts.amazonaws.com/", []string{"eu-west-1"}, false},
		{"empty path", "https://sts.amazonaws.com", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sts := newFakeSTS(t)
			p := awsTestProof()
			p.URL = tc.url
			_, err := sts.verifier().Verify(context.Background(), CloudEnrollmentProof{AWS: p}, awsTestPolicy(t, tc.regions))
			if tc.ok {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrCloudEnrollmentRejected)
			assert.Zero(t, sts.requests())
		})
	}
}

func TestAWSVerifier_RejectsWithoutCallingSTS(t *testing.T) {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	mutate := map[string]func(p *api.AWSSignedRequest){
		"evil host":         func(p *api.AWSSignedRequest) { p.URL = "https://evil.example.com/" },
		"suffix trick":      func(p *api.AWSSignedRequest) { p.URL = "https://sts.amazonaws.com.evil.example.com/" },
		"prefix trick":      func(p *api.AWSSignedRequest) { p.URL = "https://evilsts.amazonaws.com/" },
		"userinfo":          func(p *api.AWSSignedRequest) { p.URL = "https://sts.amazonaws.com@evil.example.com/" },
		"port":              func(p *api.AWSSignedRequest) { p.URL = "https://sts.amazonaws.com:8443/" },
		"http scheme":       func(p *api.AWSSignedRequest) { p.URL = "http://sts.amazonaws.com/" },
		"path":              func(p *api.AWSSignedRequest) { p.URL = "https://sts.amazonaws.com/other" },
		"query":             func(p *api.AWSSignedRequest) { p.URL = "https://sts.amazonaws.com/?Action=AssumeRole" },
		"bad region":        func(p *api.AWSSignedRequest) { p.URL = "https://sts.EU-west-1.amazonaws.com/" },
		"method GET":        func(p *api.AWSSignedRequest) { p.Method = http.MethodGet },
		"other action":      func(p *api.AWSSignedRequest) { p.Body = b64("Action=AssumeRole&Version=2011-06-15") },
		"extra body params": func(p *api.AWSSignedRequest) { p.Body = b64("Action=GetCallerIdentity&Version=2011-06-15&x=1") },
		"body not base64":   func(p *api.AWSSignedRequest) { p.Body = "!!!" },
		"audience missing":  func(p *api.AWSSignedRequest) { delete(p.Headers, "X-Unified-Cd-Audience") },
		"audience mismatch": func(p *api.AWSSignedRequest) { p.Headers["X-Unified-Cd-Audience"] = []string{"other"} },
		"audience not signed": func(p *api.AWSSignedRequest) {
			p.Headers["Authorization"] = []string{"AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE/20261103/us-east-1/sts/aws4_request, SignedHeaders=content-type;host;x-amz-date, Signature=abcd"}
		},
		"authorization absent": func(p *api.AWSSignedRequest) { delete(p.Headers, "Authorization") },
		"host header mismatch": func(p *api.AWSSignedRequest) { p.Headers["Host"] = []string{"evil.example.com"} },
	}
	for name, fn := range mutate {
		t.Run(name, func(t *testing.T) {
			sts := newFakeSTS(t)
			p := awsTestProof()
			fn(p)
			_, err := sts.verifier().Verify(context.Background(), CloudEnrollmentProof{AWS: p}, awsTestPolicy(t, nil))
			require.ErrorIs(t, err, ErrCloudEnrollmentRejected)
			assert.Zero(t, sts.requests(), "no request may reach STS")
		})
	}
	t.Run("nil proof", func(t *testing.T) {
		sts := newFakeSTS(t)
		_, err := sts.verifier().Verify(context.Background(), CloudEnrollmentProof{}, awsTestPolicy(t, nil))
		require.ErrorIs(t, err, ErrCloudEnrollmentRejected)
	})
	t.Run("empty audience policy", func(t *testing.T) {
		sts := newFakeSTS(t)
		pol := awsTestPolicy(t, nil)
		pol.ProviderConfig = json.RawMessage(`{}`)
		_, err := sts.verifier().Verify(context.Background(), CloudEnrollmentProof{AWS: awsTestProof()}, pol)
		require.ErrorIs(t, err, ErrCloudEnrollmentRejected)
	})
}

func TestAWSVerifier_STSResponses(t *testing.T) {
	t.Run("403 is rejected", func(t *testing.T) {
		sts := newFakeSTS(t)
		sts.status = http.StatusForbidden
		_, err := sts.verifier().Verify(context.Background(), CloudEnrollmentProof{AWS: awsTestProof()}, awsTestPolicy(t, nil))
		require.ErrorIs(t, err, ErrCloudEnrollmentRejected)
	})
	t.Run("500 is unavailable", func(t *testing.T) {
		sts := newFakeSTS(t)
		sts.status = http.StatusInternalServerError
		_, err := sts.verifier().Verify(context.Background(), CloudEnrollmentProof{AWS: awsTestProof()}, awsTestPolicy(t, nil))
		require.ErrorIs(t, err, ErrCloudEnrollmentUnavailable)
	})
	t.Run("unreachable is unavailable", func(t *testing.T) {
		sts := newFakeSTS(t)
		v := sts.verifier()
		sts.srv.Close()
		_, err := v.Verify(context.Background(), CloudEnrollmentProof{AWS: awsTestProof()}, awsTestPolicy(t, nil))
		require.ErrorIs(t, err, ErrCloudEnrollmentUnavailable)
	})
	t.Run("redirect is not followed", func(t *testing.T) {
		target := newFakeSTS(t)
		redirector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.srv.URL, http.StatusFound)
		}))
		defer redirector.Close()
		v := NewAWSEnrollmentVerifier(redirector.Client(), func(string) string { return strings.TrimPrefix(redirector.URL, "https://") })
		_, err := v.Verify(context.Background(), CloudEnrollmentProof{AWS: awsTestProof()}, awsTestPolicy(t, nil))
		require.ErrorIs(t, err, ErrCloudEnrollmentRejected)
		assert.Zero(t, target.requests())
	})
	t.Run("malformed XML is rejected", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "not xml") }))
		defer srv.Close()
		v := NewAWSEnrollmentVerifier(srv.Client(), func(string) string { return strings.TrimPrefix(srv.URL, "https://") })
		_, err := v.Verify(context.Background(), CloudEnrollmentProof{AWS: awsTestProof()}, awsTestPolicy(t, nil))
		require.ErrorIs(t, err, ErrCloudEnrollmentRejected)
	})
}

func TestAWSVerifier_ARNAndPolicy(t *testing.T) {
	cases := map[string]string{
		"iam user":           "arn:aws:iam::123456789012:user/x",
		"root":               "arn:aws:iam::123456789012:root",
		"federated":          "arn:aws:sts::123456789012:federated-user/x",
		"non-instance name":  "arn:aws:sts::123456789012:assumed-role/ci-agent/botocore-session-1",
		"uppercase hex":      "arn:aws:sts::123456789012:assumed-role/ci-agent/i-0ABC1234DEF567890",
		"short instance id":  "arn:aws:sts::123456789012:assumed-role/ci-agent/i-0abc123",
		"account not listed": "arn:aws:sts::999999999999:assumed-role/ci-agent/i-0abc1234def567890",
		"role not listed":    "arn:aws:sts::123456789012:assumed-role/other/i-0abc1234def567890",
		"role case differs":  "arn:aws:sts::123456789012:assumed-role/CI-Agent/i-0abc1234def567890",
		"bad partition":      "arn:aws-evil:sts::123456789012:assumed-role/ci-agent/i-0abc1234def567890",
		"short account":      "arn:aws:sts::12345:assumed-role/ci-agent/i-0abc1234def567890",
	}
	for name, arn := range cases {
		t.Run(name, func(t *testing.T) {
			sts := newFakeSTS(t)
			sts.arn = arn
			_, err := sts.verifier().Verify(context.Background(), CloudEnrollmentProof{AWS: awsTestProof()}, awsTestPolicy(t, nil))
			require.ErrorIs(t, err, ErrCloudEnrollmentRejected)
		})
	}
}

func TestCloudEnroll_AWSThroughHandler(t *testing.T) {
	sts := newFakeSTS(t)
	pg := store.NewTestPostgres(t)
	s := NewServer(Config{CloudEnrollmentVerifiers: map[string]CloudEnrollmentVerifier{"aws": sts.verifier()}}, pg)
	t.Cleanup(s.Close)
	_, err := pg.UpsertAgentEnrollmentPolicy(context.Background(), awsTestPolicy(t, nil))
	require.NoError(t, err)

	post := func(proof *api.AWSSignedRequest, addr string) *httptest.ResponseRecorder {
		body, err := json.Marshal(api.AgentEnrollRequest{Provider: "aws", Policy: "aws-ci", Labels: []string{"pool:ci"}, AWS: proof})
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/enroll", strings.NewReader(string(body)))
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		return rec
	}

	rec := post(awsTestProof(), "192.0.2.1:1234")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp api.AgentTokenResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "aws:123456789012:i-0abc1234def567890", resp.AgentID)
	assert.NotEmpty(t, resp.AccessToken)

	rec = post(nil, "192.0.2.2:1234")
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "enrollment policy rejected\n", rec.Body.String())
}

func TestDefaultCloudEnrollmentVerifiers_IncludesAWS(t *testing.T) {
	assert.Contains(t, defaultCloudEnrollmentVerifiers(), "aws")
}
