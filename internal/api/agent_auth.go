package api

import "time"

// CreateAgentEnrollmentRequest requests a one-time enrollment token for one
// fixed agent identity.
type CreateAgentEnrollmentRequest struct {
	AgentID      string   `json:"agentId"`
	ExpiresIn    string   `json:"expiresIn,omitempty"`
	Labels       []string `json:"labels,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
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
