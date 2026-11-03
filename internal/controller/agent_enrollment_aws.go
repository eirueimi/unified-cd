package controller

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/eirueimi/unified-cd/internal/store"
)

const (
	awsAudienceHeader      = "X-Unified-CD-Audience"
	awsGetCallerIdentity   = "Action=GetCallerIdentity&Version=2011-06-15"
	awsGlobalSTSHost       = "sts.amazonaws.com"
	awsGlobalSTSRegion     = "us-east-1"
	awsMaxSTSResponseBytes = 64 << 10
	awsMaxLoggedBodyBytes  = 512
)

var (
	awsSTSRegionalHost = regexp.MustCompile(`^sts\.([a-z]{2}(?:-[a-z]+)+-[0-9])\.amazonaws\.com$`)
	awsAssumedRoleARN  = regexp.MustCompile(`^arn:(?:aws|aws-us-gov|aws-cn):sts::([0-9]{12}):assumed-role/([^/]+)/([^/]+)$`)
	awsInstanceSession = regexp.MustCompile(`^i-[0-9a-f]{8,17}$`)
	awsSignedHeaders   = regexp.MustCompile(`(?:^|[\s,])SignedHeaders=([^,\s]+)`)
)

type awsEnrollmentVerifier struct {
	client      *http.Client
	resolveHost func(host string) string
}

// NewAWSEnrollmentVerifier verifies a pre-signed STS GetCallerIdentity request
// by forwarding it, unchanged, to the validated STS host. A nil client uses a
// default client with a request timeout. resolveHost is nil in production; tests
// use it to map a validated STS host to the host:port of a fake. It only changes
// where the connection is made: the Host header and the signature stay bound to
// the validated host.
func NewAWSEnrollmentVerifier(client *http.Client, resolveHost func(host string) string) CloudEnrollmentVerifier {
	if client == nil {
		client = &http.Client{Timeout: cloudEnrollmentRequestTimeout}
	}
	c := *client
	// The signed request must be answered by the host that was validated.
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &awsEnrollmentVerifier{client: &c, resolveHost: resolveHost}
}

func awsRejected(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrCloudEnrollmentRejected, fmt.Sprintf(format, args...))
}

// headerValues returns the values of the named header, matched case-insensitively.
func headerValues(h map[string][]string, name string) []string {
	var out []string
	for k, v := range h {
		if strings.EqualFold(k, name) {
			out = append(out, v...)
		}
	}
	return out
}

func (v *awsEnrollmentVerifier) Verify(ctx context.Context, proof CloudEnrollmentProof, policy store.AgentEnrollmentPolicy) (CloudEnrollmentIdentity, error) {
	cfg, constraints, err := store.DecodeCloudPolicy[store.AWSPolicyConfig, store.AWSSubjectConstraints](policy)
	if err != nil || cfg.Audience == "" {
		return CloudEnrollmentIdentity{}, awsRejected("policy decode")
	}
	signed := proof.AWS
	if signed == nil {
		return CloudEnrollmentIdentity{}, awsRejected("missing aws proof")
	}
	if signed.Method != http.MethodPost {
		return CloudEnrollmentIdentity{}, awsRejected("method %q", signed.Method)
	}
	u, err := url.Parse(signed.URL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || (u.Path != "" && u.Path != "/") ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return CloudEnrollmentIdentity{}, awsRejected("url %q", signed.URL)
	}
	host := u.Hostname()
	region := awsGlobalSTSRegion
	if host != awsGlobalSTSHost {
		m := awsSTSRegionalHost.FindStringSubmatch(host)
		if m == nil {
			return CloudEnrollmentIdentity{}, awsRejected("sts host %q", host)
		}
		region = m[1]
	}
	if len(cfg.STSRegions) > 0 && !contains(cfg.STSRegions, region) {
		return CloudEnrollmentIdentity{}, awsRejected("sts region %q", region)
	}
	body, err := base64.StdEncoding.DecodeString(signed.Body)
	if err != nil || string(body) != awsGetCallerIdentity {
		return CloudEnrollmentIdentity{}, awsRejected("request body")
	}
	audience := headerValues(signed.Headers, awsAudienceHeader)
	if len(audience) != 1 || audience[0] != cfg.Audience {
		return CloudEnrollmentIdentity{}, awsRejected("audience header")
	}
	if !awsAudienceSigned(signed.Headers) {
		return CloudEnrollmentIdentity{}, awsRejected("audience header not covered by the signature")
	}
	for _, h := range headerValues(signed.Headers, "Host") {
		if h != host {
			return CloudEnrollmentIdentity{}, awsRejected("host header %q", h)
		}
	}

	arn, err := v.callerIdentityARN(ctx, host, signed.Headers, body)
	if err != nil {
		return CloudEnrollmentIdentity{}, err
	}
	m := awsAssumedRoleARN.FindStringSubmatch(arn)
	if m == nil {
		return CloudEnrollmentIdentity{}, awsRejected("arn %q is not an assumed role", arn)
	}
	account, role, session := m[1], m[2], m[3]
	if !awsInstanceSession.MatchString(session) {
		return CloudEnrollmentIdentity{}, awsRejected("session name %q is not an instance id", session)
	}
	if !contains(constraints.AccountIDs, account) || !contains(constraints.RoleNames, role) {
		return CloudEnrollmentIdentity{}, awsRejected("policy subject (account %q, role %q)", account, role)
	}
	return CloudEnrollmentIdentity{AgentID: "aws:" + account + ":" + session}, nil
}

// awsAudienceSigned reports whether the SigV4 Authorization header lists the
// audience header among its signed headers.
func awsAudienceSigned(headers map[string][]string) bool {
	for _, auth := range headerValues(headers, "Authorization") {
		m := awsSignedHeaders.FindStringSubmatch(auth)
		if m == nil {
			continue
		}
		for _, name := range strings.Split(m[1], ";") {
			if strings.EqualFold(name, awsAudienceHeader) {
				return true
			}
		}
	}
	return false
}

// callerIdentityARN forwards the signed request to host and returns the ARN
// from the GetCallerIdentity response.
func (v *awsEnrollmentVerifier) callerIdentityARN(ctx context.Context, host string, headers map[string][]string, body []byte) (string, error) {
	connect := host
	if v.resolveHost != nil {
		connect = v.resolveHost(host)
	}
	ctx, cancel := context.WithTimeout(ctx, cloudEnrollmentRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+connect+"/", bytes.NewReader(body))
	if err != nil {
		return "", awsRejected("build request: %v", err)
	}
	req.Host = host
	for name, values := range headers {
		if strings.EqualFold(name, "Host") || strings.EqualFold(name, "Content-Length") {
			continue
		}
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: sts request: %v", ErrCloudEnrollmentUnavailable, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, awsMaxSTSResponseBytes))
	if err != nil {
		return "", fmt.Errorf("%w: sts response: %v", ErrCloudEnrollmentUnavailable, err)
	}
	if resp.StatusCode >= 500 {
		return "", fmt.Errorf("%w: sts status %d", ErrCloudEnrollmentUnavailable, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		logged := data
		if len(logged) > awsMaxLoggedBodyBytes {
			logged = logged[:awsMaxLoggedBodyBytes]
		}
		slog.Warn("aws sts rejected the signed request", "status", resp.StatusCode, "body", string(logged))
		return "", awsRejected("sts status %d", resp.StatusCode)
	}
	var out struct {
		Result struct {
			Arn string `xml:"Arn"`
		} `xml:"GetCallerIdentityResult"`
	}
	if err := xml.Unmarshal(data, &out); err != nil || out.Result.Arn == "" {
		return "", awsRejected("sts response has no ARN")
	}
	return out.Result.Arn, nil
}
