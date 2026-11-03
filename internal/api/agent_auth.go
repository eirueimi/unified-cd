package api

import "time"

// AgentEnrollRequest supplies optional bounded policy context for an agent
// credential exchange. It deliberately contains no agent identity.
type AgentEnrollRequest struct {
	Provider     string   `json:"provider"`
	Policy       string   `json:"policy,omitempty"`
	Labels       []string `json:"labels,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	// AWS carries the caller's pre-signed sts:GetCallerIdentity request for
	// provider "aws". The controller forwards it to STS without credentials.
	AWS *AWSSignedRequest `json:"aws,omitempty"`
}

// AWSSignedRequest is a SigV4-signed STS GetCallerIdentity request.
type AWSSignedRequest struct {
	Method  string              `json:"method"`
	URL     string              `json:"url"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"` // base64
}

type AgentEnrollmentPolicyRequest struct {
	Name       string   `json:"name,omitempty"`
	Provider   string   `json:"provider"`
	Cluster    string   `json:"cluster,omitempty"`
	Namespaces []string `json:"namespaces,omitempty"`
	// ServiceAccounts is a Kubernetes ServiceAccount list for a kubernetes
	// policy and a service account email list for a gcp policy.
	ServiceAccounts []string `json:"serviceAccounts,omitempty"`
	// Cloud provider fields. Audience applies to aws, gcp and azure.
	Audience                 string   `json:"audience,omitempty"`
	STSRegions               []string `json:"stsRegions,omitempty"`
	AccountIDs               []string `json:"accountIds,omitempty"`
	RoleNames                []string `json:"roleNames,omitempty"`
	ProjectIDs               []string `json:"projectIds,omitempty"`
	TenantID                 string   `json:"tenantId,omitempty"`
	SubscriptionIDs          []string `json:"subscriptionIds,omitempty"`
	ResourceGroups           []string `json:"resourceGroups,omitempty"`
	ManagedIdentityObjectIDs []string `json:"managedIdentityObjectIds,omitempty"`
	AgentIDTemplate          string   `json:"agentIdTemplate"`
	AllowedLabels            []string `json:"allowedLabels"`
	RequiredLabels           []string `json:"requiredLabels"`
	AccessTokenTTL           string   `json:"accessTokenTTL"`
	Enabled                  bool     `json:"enabled"`
}

// AgentTokenResponse is returned only by successful credential issuance.
// Plaintext credentials are never persisted, logged, or included in metadata.
type AgentTokenResponse struct {
	AgentID          string     `json:"agentId"`
	AccessToken      string     `json:"accessToken"`
	AccessExpiresAt  time.Time  `json:"accessExpiresAt"`
	RefreshToken     string     `json:"refreshToken,omitempty"`
	RefreshExpiresAt *time.Time `json:"refreshExpiresAt,omitempty"`
	Labels           []string   `json:"labels"`
	Capabilities     []string   `json:"capabilities"`
}

// CreateAgentEnrollmentRequest requests a one-time enrollment token for one
// agent identity. AgentID is optional: when empty, the server auto-generates a
// canonical ID (agent-XXXXXXXX) and returns it in CreateAgentEnrollmentResponse.
type CreateAgentEnrollmentRequest struct {
	AgentID   string   `json:"agentId,omitempty"`
	ExpiresIn string   `json:"expiresIn,omitempty"`
	Labels    []string `json:"labels,omitempty"`
}

// CreateAgentEnrollmentResponse is returned only when an enrollment token is
// created. Token is never included in metadata responses.
type CreateAgentEnrollmentResponse struct {
	ID        string    `json:"id"`
	AgentID   string    `json:"agentId"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// AgentEnrollmentMeta contains enrollment-token metadata without credential
// material or its hash.
type AgentEnrollmentMeta struct {
	ID        string     `json:"id"`
	AgentID   string     `json:"agentId"`
	CreatedBy string     `json:"createdBy"`
	ExpiresAt time.Time  `json:"expiresAt"`
	CreatedAt time.Time  `json:"createdAt"`
	UsedAt    *time.Time `json:"usedAt,omitempty"`
	RevokedAt *time.Time `json:"revokedAt,omitempty"`
}

// AgentIdentityMeta contains identity lifecycle metadata without any
// credential material or hashes.
type AgentIdentityMeta struct {
	ID                     string     `json:"id"`
	AgentID                string     `json:"agentId"`
	Status                 string     `json:"status"`
	EnrollmentMethod       string     `json:"enrollmentMethod"`
	AuthorizedLabels       []string   `json:"authorizedLabels,omitempty"`
	AuthorizedCapabilities []string   `json:"authorizedCapabilities,omitempty"`
	CreatedAt              time.Time  `json:"createdAt"`
	DisabledAt             *time.Time `json:"disabledAt,omitempty"`
	LastAuthenticatedAt    *time.Time `json:"lastAuthenticatedAt,omitempty"`
}
