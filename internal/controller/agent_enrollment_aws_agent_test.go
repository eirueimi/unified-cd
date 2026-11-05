package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	hostagent "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const awsAgentTestSecret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"

var awsAgentTestAuth = regexp.MustCompile(`^AWS4-HMAC-SHA256 Credential=([^/]+)/(\d{8})/([a-z0-9-]+)/sts/aws4_request, SignedHeaders=([^,]+), Signature=[0-9a-f]+$`)

// TestAWSVerifier_AcceptsHostAgentProof drives the host agent's real proof
// through the real AWS verifier. The fake STS recomputes the SigV4 signature
// from the forwarded request, so a header the verifier drops (or one the agent
// forgot to sign) fails the exchange.
func TestAWSVerifier_AcceptsHostAgentProof(t *testing.T) {
	var gotHost, gotAuth string
	sts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotHost, gotAuth = r.Host, r.Header.Get("Authorization")
		m := awsAgentTestAuth.FindStringSubmatch(gotAuth)
		if m == nil {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		headers := map[string]string{}
		for _, name := range strings.Split(m[4], ";") {
			if name == "host" {
				headers[name] = r.Host
			} else {
				headers[name] = r.Header.Get(name)
			}
		}
		ts, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
		if err != nil || string(body) != "Action=GetCallerIdentity&Version=2011-06-15" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		want := hostagent.SignV4(hostagent.SigV4Params{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Headers: headers, Body: body,
			AccessKeyID: m[1], SecretAccessKey: awsAgentTestSecret, Region: m[3], Service: "sts", Time: ts})
		if want != gotAuth || r.Header.Get("X-Amz-Security-Token") != "session-token" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		fmt.Fprintf(w, `<GetCallerIdentityResponse><GetCallerIdentityResult><Arn>arn:aws:sts::%s:assumed-role/%s/%s</Arn></GetCallerIdentityResult></GetCallerIdentityResponse>`, awsTestAccount, awsTestRole, awsTestSession)
	}))
	defer sts.Close()

	imds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest/api/token":
			fmt.Fprint(w, "t")
		case "/latest/meta-data/placement/region":
			fmt.Fprint(w, "us-east-1")
		case "/latest/meta-data/iam/security-credentials/":
			fmt.Fprint(w, "ci-agent")
		default:
			fmt.Fprint(w, `{"AccessKeyId":"AKIDEXAMPLE","SecretAccessKey":"`+awsAgentTestSecret+`","Token":"session-token"}`)
		}
	}))
	defer imds.Close()

	verifier := NewAWSEnrollmentVerifier(sts.Client(), func(string) string { return strings.TrimPrefix(sts.URL, "https://") })
	policy := awsTestPolicy(t, nil)

	// The fake controller verifies whatever the agent posts.
	ctrl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req api.AgentEnrollRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		id, err := verifier.Verify(r.Context(), CloudEnrollmentProof{AWS: req.AWS}, policy)
		if err != nil {
			t.Logf("verifier rejected the agent proof: %v", err)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(api.AgentTokenResponse{AgentID: id.AgentID, AccessToken: "access", AccessExpiresAt: time.Now().Add(time.Hour)})
	}))
	defer ctrl.Close()

	for name, override := range map[string]string{"regional host": "", "STS host override": "sts.us-east-1.amazonaws.com"} {
		t.Run(name, func(t *testing.T) {
			src, err := hostagent.NewCloudCredentialSource(hostagent.CloudCredentialSourceConfig{
				Server: ctrl.URL, Provider: "aws", Policy: "aws-ci", Audience: awsTestAudience,
				MetadataBaseURL: imds.URL, STSHostOverride: override,
			})
			require.NoError(t, err)
			tok, err := src.Token(context.Background())
			require.NoError(t, err)
			assert.Equal(t, "access", tok)
			assert.Equal(t, "aws:123456789012:i-0abc1234def567890", src.AgentID())
			assert.Equal(t, "sts.us-east-1.amazonaws.com", gotHost)
			assert.Contains(t, gotAuth, "SignedHeaders=content-type;host;x-amz-date;x-amz-security-token;x-unified-cd-audience,")
		})
	}
}
