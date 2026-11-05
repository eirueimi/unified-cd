package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
)

const (
	gcpMetadataDefault   = "http://metadata.google.internal"
	azureMetadataDefault = "http://169.254.169.254"
	awsMetadataDefault   = "http://169.254.169.254"
	awsGetCallerIdentity = "Action=GetCallerIdentity&Version=2011-06-15"
	awsAudienceHeader    = "X-Unified-CD-Audience"
	maxMetadataBytes     = 1 << 20
)

// cloudProof is the identity proof for one enrollment exchange. Exactly one of
// bearer or aws is set. Its contents are credentials and must never be logged.
type cloudProof struct {
	bearer string
	aws    *api.AWSSignedRequest
}

// errCloudProof is deliberately generic: metadata responses can embed
// credentials and callers must not see them.
var errCloudProof = errors.New("fetch cloud identity proof failed")

func (s *CloudCredentialSource) fetchProof(ctx context.Context) (cloudProof, error) {
	var (
		proof cloudProof
		err   error
	)
	switch s.provider {
	case "gcp":
		proof.bearer, err = s.gcpProof(ctx)
	case "azure":
		proof.bearer, err = s.azureProof(ctx)
	case "aws":
		proof.aws, err = s.awsProof(ctx)
	default:
		err = errCloudProof
	}
	if err != nil {
		return cloudProof{}, errCloudProof
	}
	return proof, nil
}

func (s *CloudCredentialSource) metadataBase(def string) string {
	if s.metadataBaseURL != "" {
		return strings.TrimRight(s.metadataBaseURL, "/")
	}
	return def
}

// metadataGet performs one metadata request and returns the body of a 200.
func (s *CloudCredentialSource) metadataGet(ctx context.Context, method, target string, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := s.metadata.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metadata status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxMetadataBytes))
}

func (s *CloudCredentialSource) gcpProof(ctx context.Context) (string, error) {
	target := s.metadataBase(gcpMetadataDefault) + "/computeMetadata/v1/instance/service-accounts/default/identity?audience=" + url.QueryEscape(s.audience) + "&format=full"
	body, err := s.metadataGet(ctx, http.MethodGet, target, map[string]string{"Metadata-Flavor": "Google"})
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(body))
	if token == "" {
		return "", errCloudProof
	}
	return token, nil
}

func (s *CloudCredentialSource) azureProof(ctx context.Context) (string, error) {
	target := s.metadataBase(azureMetadataDefault) + "/metadata/identity/oauth2/token?api-version=2018-02-01&resource=" + url.QueryEscape(s.audience)
	body, err := s.metadataGet(ctx, http.MethodGet, target, map[string]string{"Metadata": "true"})
	if err != nil {
		return "", err
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.AccessToken == "" {
		return "", errCloudProof
	}
	return out.AccessToken, nil
}

func (s *CloudCredentialSource) awsProof(ctx context.Context) (*api.AWSSignedRequest, error) {
	base := s.metadataBase(awsMetadataDefault)
	tokenBody, err := s.metadataGet(ctx, http.MethodPut, base+"/latest/api/token", map[string]string{"X-aws-ec2-metadata-token-ttl-seconds": "60"})
	if err != nil {
		return nil, err
	}
	imds := map[string]string{"X-aws-ec2-metadata-token": strings.TrimSpace(string(tokenBody))}
	regionBody, err := s.metadataGet(ctx, http.MethodGet, base+"/latest/meta-data/placement/region", imds)
	if err != nil {
		return nil, err
	}
	region := strings.TrimSpace(string(regionBody))
	roleBody, err := s.metadataGet(ctx, http.MethodGet, base+"/latest/meta-data/iam/security-credentials/", imds)
	if err != nil {
		return nil, err
	}
	role := strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(roleBody)), "\n", 2)[0])
	if region == "" || role == "" {
		return nil, errCloudProof
	}
	credBody, err := s.metadataGet(ctx, http.MethodGet, base+"/latest/meta-data/iam/security-credentials/"+url.PathEscape(role), imds)
	if err != nil {
		return nil, err
	}
	var creds struct {
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string `json:"SecretAccessKey"`
		Token           string `json:"Token"`
	}
	if err := json.Unmarshal(credBody, &creds); err != nil || creds.AccessKeyID == "" || creds.SecretAccessKey == "" || creds.Token == "" {
		return nil, errCloudProof
	}

	host := "sts." + region + ".amazonaws.com"
	if s.stsHostOverride != "" {
		host = s.stsHostOverride
	}
	now := s.now().UTC()
	const contentType = "application/x-www-form-urlencoded; charset=utf-8"
	amzDate := now.Format("20060102T150405Z")
	// Sign host plus exactly the headers sent below. The controller does not
	// forward Host, so it is signed but not placed in the proof.
	auth := SignV4(SigV4Params{
		Method: http.MethodPost, Path: "/",
		Headers: map[string]string{
			"content-type":          contentType,
			"host":                  host,
			"x-amz-date":            amzDate,
			"x-amz-security-token":  creds.Token,
			"x-unified-cd-audience": s.audience,
		},
		Body: []byte(awsGetCallerIdentity), AccessKeyID: creds.AccessKeyID, SecretAccessKey: creds.SecretAccessKey,
		Region: region, Service: "sts", Time: now,
	})
	return &api.AWSSignedRequest{
		Method: http.MethodPost,
		URL:    "https://" + host + "/",
		Headers: map[string][]string{
			"Authorization":        {auth},
			"Content-Type":         {contentType},
			"X-Amz-Date":           {amzDate},
			"X-Amz-Security-Token": {creds.Token},
			awsAudienceHeader:      {s.audience},
		},
		Body: base64.StdEncoding.EncodeToString([]byte(awsGetCallerIdentity)),
	}, nil
}

// newMetadataClient returns the client for instance metadata requests. It
// never uses a proxy and never follows redirects, so metadata credentials and
// headers cannot be sent to another host.
func newMetadataClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{
		Transport:     transport,
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
