package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/eirueimi/unified-cd/internal/store"
)

// azureTenantGUID mirrors the store's policy validation: the tenant ID goes into
// the JWKS URL path.
var azureTenantGUID = regexp.MustCompile(`^[0-9a-fA-F]{8}(-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}$`)

// maxLoggedOIDCErrorBytes bounds go-oidc error text kept in errors (and so in
// logs): it can embed a slice of the JWKS response body.
const maxLoggedOIDCErrorBytes = 512

// oidcKeyFetchErrorPrefix is the text go-oidc v3.18 starts a failed JWKS fetch with
// (verify.go wraps the key set's "fetching keys ..." error).
const oidcKeyFetchErrorPrefix = "failed to verify signature: fetching keys"

// verifyOIDCToken verifies raw with verifier and classifies the failure: a
// failed JWKS fetch (go-oidc wraps it as "fetching keys ...") wraps
// ErrCloudEnrollmentUnavailable, any other failure (signature, audience,
// expiry, unknown kid, malformed token) wraps ErrCloudEnrollmentRejected. The
// go-oidc text is truncated.
func verifyOIDCToken(ctx context.Context, verifier *oidc.IDTokenVerifier, raw string) (*oidc.IDToken, error) {
	token, err := verifier.Verify(ctx, raw)
	if err == nil {
		return token, nil
	}
	msg := err.Error()
	if len(msg) > maxLoggedOIDCErrorBytes {
		// Cut at a rune boundary; ToValidUTF8 drops a rune split by the cut.
		msg = strings.ToValidUTF8(msg[:maxLoggedOIDCErrorBytes], "") + "...(truncated)"
	}
	// go-oidc emits exactly this prefix when the key set cannot be fetched. Other
	// failures can embed attacker-controlled iss/aud text (checked before the
	// signature), so only a prefix match is trusted.
	if strings.HasPrefix(err.Error(), oidcKeyFetchErrorPrefix) {
		return nil, fmt.Errorf("%w: %s", ErrCloudEnrollmentUnavailable, msg)
	}
	return nil, fmt.Errorf("%w: %s", ErrCloudEnrollmentRejected, msg)
}

type azureEnrollmentVerifier struct {
	authority string
	client    *http.Client
	timeout   time.Duration

	mu      sync.Mutex
	keySets map[string]oidc.KeySet // by tenant ID; bounded by the number of policy tenants
}

// NewAzureEnrollmentVerifier verifies Azure managed identity access tokens of
// plain VMs with a system-assigned identity. Keys come from
// <authority>/<tenantId>/discovery/v2.0/keys. A nil client uses a default
// client with a request timeout.
func NewAzureEnrollmentVerifier(authority string, client *http.Client) CloudEnrollmentVerifier {
	if client == nil {
		client = &http.Client{Timeout: cloudEnrollmentRequestTimeout}
	}
	return &azureEnrollmentVerifier{
		authority: strings.TrimRight(authority, "/"), client: client,
		timeout: cloudEnrollmentRequestTimeout, keySets: map[string]oidc.KeySet{},
	}
}

func (v *azureEnrollmentVerifier) keySet(tenant string) oidc.KeySet {
	v.mu.Lock()
	defer v.mu.Unlock()
	ks, ok := v.keySets[tenant]
	if !ok {
		// The JWKS fetch is bounded by the http.Client timeout, not the request context.
		ctx := oidc.ClientContext(context.Background(), v.client)
		ks = oidc.NewRemoteKeySet(ctx, v.authority+"/"+url.PathEscape(tenant)+"/discovery/v2.0/keys")
		v.keySets[tenant] = ks
	}
	return ks
}

func (v *azureEnrollmentVerifier) Verify(ctx context.Context, proof CloudEnrollmentProof, policy store.AgentEnrollmentPolicy) (CloudEnrollmentIdentity, error) {
	cfg, constraints, err := store.DecodeCloudPolicy[store.AzurePolicyConfig, store.AzureSubjectConstraints](policy)
	if err != nil || cfg.Audience == "" || !azureTenantGUID.MatchString(cfg.TenantID) {
		return CloudEnrollmentIdentity{}, fmt.Errorf("%w: policy decode", ErrCloudEnrollmentRejected)
	}
	ctx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()
	// A tenant has two valid issuers (v1 and v2 tokens) signed by the same
	// keys, and go-oidc checks only one, so the issuer is checked below.
	verifier := oidc.NewVerifier("", v.keySet(cfg.TenantID), &oidc.Config{ClientID: cfg.Audience, SkipIssuerCheck: true})
	token, err := verifyOIDCToken(ctx, verifier, proof.BearerToken)
	if err != nil {
		return CloudEnrollmentIdentity{}, err
	}
	// The tenant GUID in a policy may be upper case while Entra issues it lower case.
	if !strings.EqualFold(token.Issuer, "https://sts.windows.net/"+cfg.TenantID+"/") && !strings.EqualFold(token.Issuer, v.authority+"/"+cfg.TenantID+"/v2.0") {
		return CloudEnrollmentIdentity{}, fmt.Errorf("%w: issuer %q", ErrCloudEnrollmentRejected, token.Issuer)
	}
	var claims struct {
		TenantID string `json:"tid"`
		ObjectID string `json:"oid"`
		MIRID    string `json:"xms_mirid"`
	}
	if err := token.Claims(&claims); err != nil {
		return CloudEnrollmentIdentity{}, fmt.Errorf("%w: claims: %v", ErrCloudEnrollmentRejected, err)
	}
	if !strings.EqualFold(claims.TenantID, cfg.TenantID) || claims.ObjectID == "" {
		return CloudEnrollmentIdentity{}, fmt.Errorf("%w: tenant or object id", ErrCloudEnrollmentRejected)
	}
	sub, rg, vm, ok := parseAzureVMResourceID(claims.MIRID)
	if !ok {
		return CloudEnrollmentIdentity{}, fmt.Errorf("%w: xms_mirid is not a plain virtual machine", ErrCloudEnrollmentRejected)
	}
	if !containsFold(constraints.ManagedIdentityObjectIDs, claims.ObjectID) ||
		!containsFold(constraints.SubscriptionIDs, sub) ||
		(len(constraints.ResourceGroups) > 0 && !containsFold(constraints.ResourceGroups, rg)) {
		return CloudEnrollmentIdentity{}, fmt.Errorf("%w: policy subject (object %q, subscription %q, resource group %q)", ErrCloudEnrollmentRejected, claims.ObjectID, sub, rg)
	}
	return CloudEnrollmentIdentity{AgentID: strings.ToLower("azure:" + sub + ":" + rg + ":" + vm)}, nil
}

// parseAzureVMResourceID accepts only
// /subscriptions/<sub>/resourceGroups/<rg>/providers/Microsoft.Compute/virtualMachines/<name>
// (segment names case-insensitive). VM scale sets, user-assigned identities
// and anything else are not accepted.
func parseAzureVMResourceID(id string) (sub, rg, vm string, ok bool) {
	parts := strings.Split(id, "/")
	if len(parts) != 9 || parts[0] != "" {
		return "", "", "", false
	}
	if !strings.EqualFold(parts[1], "subscriptions") || !strings.EqualFold(parts[3], "resourceGroups") ||
		!strings.EqualFold(parts[5], "providers") || !strings.EqualFold(parts[6], "Microsoft.Compute") ||
		!strings.EqualFold(parts[7], "virtualMachines") {
		return "", "", "", false
	}
	if parts[2] == "" || parts[4] == "" || parts[8] == "" {
		return "", "", "", false
	}
	return parts[2], parts[4], parts[8], true
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}
