package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/store"
	"gopkg.in/yaml.v3"
)

type ControllerAgentAuthConfig struct {
	LegacySharedToken            string                                       `yaml:"legacySharedToken"`
	KubernetesClusters           []ControllerKubernetesClusterConfig          `yaml:"kubernetesClusters"`
	KubernetesEnrollmentPolicies []ControllerKubernetesEnrollmentPolicyConfig `yaml:"kubernetesEnrollmentPolicies"`
}
type ControllerKubernetesClusterConfig struct {
	Name       string `yaml:"name"`
	Kubeconfig string `yaml:"kubeconfig"`
}

// ControllerKubernetesEnrollmentPolicyConfig declares one bounded workload
// identity policy that the controller upserts before it starts serving.
type ControllerKubernetesEnrollmentPolicyConfig struct {
	Name            string   `yaml:"name"`
	Cluster         string   `yaml:"cluster"`
	Namespaces      []string `yaml:"namespaces"`
	ServiceAccounts []string `yaml:"serviceAccounts"`
	AllowedLabels   []string `yaml:"allowedLabels"`
	RequiredLabels  []string `yaml:"requiredLabels"`
	Capabilities    []string `yaml:"capabilities"`
	AccessTokenTTL  string   `yaml:"accessTokenTTL"`
	Enabled         bool     `yaml:"enabled"`
}

// StorePolicy converts a declarative controller config entry into the
// controller's persisted enrollment-policy representation.
func (c ControllerKubernetesEnrollmentPolicyConfig) StorePolicy() (store.AgentEnrollmentPolicy, error) {
	if strings.TrimSpace(c.Name) == "" || strings.TrimSpace(c.Cluster) == "" || len(c.Namespaces) == 0 || len(c.ServiceAccounts) == 0 {
		return store.AgentEnrollmentPolicy{}, fmt.Errorf("kubernetes enrollment policy requires name, cluster, namespaces, and serviceAccounts")
	}
	ttl, err := time.ParseDuration(c.AccessTokenTTL)
	if err != nil {
		return store.AgentEnrollmentPolicy{}, fmt.Errorf("kubernetes enrollment policy accessTokenTTL: %w", err)
	}
	providerConfig, err := json.Marshal(struct {
		Cluster string `json:"cluster"`
	}{Cluster: c.Cluster})
	if err != nil {
		return store.AgentEnrollmentPolicy{}, err
	}
	constraints, err := json.Marshal(struct {
		Namespaces      []string `json:"namespaces"`
		ServiceAccounts []string `json:"serviceAccounts"`
	}{Namespaces: c.Namespaces, ServiceAccounts: c.ServiceAccounts})
	if err != nil {
		return store.AgentEnrollmentPolicy{}, err
	}
	return store.AgentEnrollmentPolicy{
		Name: c.Name, Provider: "kubernetes", ProviderConfig: providerConfig, SubjectConstraints: constraints,
		AgentIDTemplate: "k8s:{cluster}:{namespace}:{podUID}", AllowedLabels: append([]string(nil), c.AllowedLabels...),
		RequiredLabels: append([]string(nil), c.RequiredLabels...), AuthorizedCapabilities: append([]string(nil), c.Capabilities...),
		AccessTokenTTL: ttl, Enabled: c.Enabled,
	}, nil
}

// ControllerOIDCConfig holds OIDC provider settings.
type ControllerOIDCConfig struct {
	Issuer         string `yaml:"issuer"`
	IssuerInternal string `yaml:"issuerInternal"`
	// ExternalURL is the base URL for browser SSO redirect URIs (e.g. http://localhost:8080).
	// Must be set explicitly when accessed through a reverse proxy such as Vite, because r.Host
	// will be the container-internal name. Falls back to the request's Host header when not set.
	ExternalURL  string `yaml:"externalUrl"`
	ClientID     string `yaml:"clientId"`
	ClientSecret string `yaml:"clientSecret"`
	// DeviceClientID is the public client ID used for the CLI device flow.
	// Using a separate secret-less public client distinct from the confidential ClientID (for browser SSO)
	// allows the device flow to work without exposing secrets to the CLI. Falls back to ClientID when not set.
	DeviceClientID string `yaml:"deviceClientId"`

	// Role resolution (see docs/authorization.md).
	RolesClaim  string            `yaml:"rolesClaim"`
	RoleMap     map[string]string `yaml:"roleMap"`
	UserMap     map[string]string `yaml:"userMap"`
	DefaultRole string            `yaml:"defaultRole"`
}

// ControllerConfig holds all configuration for the controller binary.
// Populated from a YAML file via LoadController; zero-value fields mean "not set".
type ControllerConfig struct {
	DSN           string    `yaml:"dsn"`
	Addr          string    `yaml:"addr"`
	Token         string    `yaml:"token"`
	S3Endpoint    string    `yaml:"s3Endpoint"`
	S3Bucket      string    `yaml:"s3Bucket"`
	S3Key         string    `yaml:"s3Key"`
	S3Secret      string    `yaml:"s3Secret"`
	DataDir       string    `yaml:"dataDir"`
	KeySource     KeySource `yaml:"-"`
	WebDir        string    `yaml:"webDir"`
	UIProxyTarget string    `yaml:"uiProxyTarget"`
	// StderrPlain, when true, tells the web UI to render step stderr in the run
	// log with the same color as stdout instead of red. Default (false) = red.
	StderrPlain bool                       `yaml:"stderrPlain"`
	OIDC        *ControllerOIDCConfig      `yaml:"oidc"`
	AgentAuth   *ControllerAgentAuthConfig `yaml:"agentAuth"`
	// InsecureCookies disables the Secure attribute on session cookies (env: UNIFIED_INSECURE_COOKIES).
	InsecureCookies bool `yaml:"insecureCookies"`
}

// envBool parses a boolean environment variable (strconv.ParseBool semantics);
// unset or malformed yields false.
func envBool(name string) bool {
	b, _ := strconv.ParseBool(os.Getenv(name))
	return b
}

// OIDCConfigured returns whether SSO (OIDC) is configured to a usable state.
// Both issuer and clientId are required because the login flow cannot start without them.
func OIDCConfigured(o *ControllerOIDCConfig) bool {
	return o != nil && o.Issuer != "" && o.ClientID != ""
}

// LoadController reads a YAML config file and returns a ControllerConfig.
// Unknown fields are rejected.
func LoadController(path string) (*ControllerConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open controller config: %w", err)
	}
	defer f.Close()

	var cfg ControllerConfig
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse controller config %s: %w", path, err)
	}
	return &cfg, nil
}

// ControllerEffective resolves the effective configuration from env vars and an
// optional YAML file. The returned struct is used as flag defaults so that
// explicit CLI flags can override both env vars and the config file.
//
// Priority (lowest to highest): env vars → config file → CLI flags.
func ControllerEffective(filePath string) (*ControllerConfig, error) {
	eff := &ControllerConfig{
		DSN:        os.Getenv("UNIFIED_DB_DSN"),
		Token:      os.Getenv("UNIFIED_TOKEN"),
		S3Endpoint: os.Getenv("UNIFIED_S3_ENDPOINT"),
		S3Bucket:   os.Getenv("UNIFIED_S3_BUCKET"),
		S3Key:      os.Getenv("UNIFIED_S3_KEY"),
		S3Secret:   os.Getenv("UNIFIED_S3_SECRET"),
		DataDir:    os.Getenv("UNIFIED_DATA_DIR"),
		KeySource: KeySource{
			KeyFile:        os.Getenv("UNIFIED_CONTROLLER_KEY_FILE"),
			KMSURI:         os.Getenv("UNIFIED_KMS_URI"),
			DevMode:        envBool("UNIFIED_DEV_MODE"),
			VaultAddr:      os.Getenv("UNIFIED_VAULT_ADDR"),
			VaultAuth:      os.Getenv("UNIFIED_VAULT_AUTH"),
			VaultAuthParam: os.Getenv("UNIFIED_VAULT_AUTH_PARAM"),
			VaultToken:     os.Getenv("VAULT_TOKEN"),
			VaultTokenFile: os.Getenv("UNIFIED_VAULT_TOKEN_FILE"),
		},
		WebDir:          os.Getenv("UNIFIED_WEB_DIR"),
		UIProxyTarget:   os.Getenv("UNIFIED_UI_PROXY_TARGET"),
		StderrPlain:     envBool("UNIFIED_LOG_STDERR_PLAIN"),
		InsecureCookies: envBool("UNIFIED_INSECURE_COOKIES"),
		AgentAuth:       &ControllerAgentAuthConfig{LegacySharedToken: os.Getenv("UNIFIED_AGENT_LEGACY_TOKEN")},
	}
	// OIDC from env vars
	oidcIssuer := os.Getenv("UNIFIED_OIDC_ISSUER")
	oidcClientID := os.Getenv("UNIFIED_OIDC_CLIENT_ID")
	if oidcIssuer != "" || oidcClientID != "" {
		eff.OIDC = &ControllerOIDCConfig{
			Issuer:         oidcIssuer,
			IssuerInternal: os.Getenv("UNIFIED_OIDC_ISSUER_INTERNAL"),
			ExternalURL:    os.Getenv("UNIFIED_OIDC_EXTERNAL_URL"),
			ClientID:       oidcClientID,
			ClientSecret:   os.Getenv("UNIFIED_OIDC_CLIENT_SECRET"),
			DeviceClientID: os.Getenv("UNIFIED_OIDC_DEVICE_CLIENT_ID"),
			RolesClaim:     os.Getenv("UNIFIED_OIDC_ROLES_CLAIM"),
			DefaultRole:    os.Getenv("UNIFIED_OIDC_DEFAULT_ROLE"),
		}
	}

	if err := eff.KeySource.Validate(); err != nil {
		return nil, err
	}
	if filePath == "" {
		return eff, nil
	}

	file, err := LoadController(filePath)
	if err != nil {
		return nil, err
	}
	if file.DSN != "" {
		eff.DSN = file.DSN
	}
	if file.Addr != "" {
		eff.Addr = file.Addr
	}
	if file.Token != "" {
		eff.Token = file.Token
	}
	if file.S3Endpoint != "" {
		eff.S3Endpoint = file.S3Endpoint
	}
	if file.S3Bucket != "" {
		eff.S3Bucket = file.S3Bucket
	}
	if file.S3Key != "" {
		eff.S3Key = file.S3Key
	}
	if file.S3Secret != "" {
		eff.S3Secret = file.S3Secret
	}
	if file.DataDir != "" {
		eff.DataDir = file.DataDir
	}
	if file.WebDir != "" {
		eff.WebDir = file.WebDir
	}
	if file.UIProxyTarget != "" {
		eff.UIProxyTarget = file.UIProxyTarget
	}
	if file.StderrPlain {
		eff.StderrPlain = true
	}
	if file.InsecureCookies {
		eff.InsecureCookies = true
	}
	if file.OIDC != nil {
		if eff.OIDC == nil {
			eff.OIDC = &ControllerOIDCConfig{}
		}
		if file.OIDC.Issuer != "" {
			eff.OIDC.Issuer = file.OIDC.Issuer
		}
		if file.OIDC.IssuerInternal != "" {
			eff.OIDC.IssuerInternal = file.OIDC.IssuerInternal
		}
		if file.OIDC.ExternalURL != "" {
			eff.OIDC.ExternalURL = file.OIDC.ExternalURL
		}
		if file.OIDC.ClientID != "" {
			eff.OIDC.ClientID = file.OIDC.ClientID
		}
		if file.OIDC.ClientSecret != "" {
			eff.OIDC.ClientSecret = file.OIDC.ClientSecret
		}
		if file.OIDC.DeviceClientID != "" {
			eff.OIDC.DeviceClientID = file.OIDC.DeviceClientID
		}
		if file.OIDC.RolesClaim != "" {
			eff.OIDC.RolesClaim = file.OIDC.RolesClaim
		}
		if len(file.OIDC.RoleMap) > 0 {
			eff.OIDC.RoleMap = file.OIDC.RoleMap
		}
		if len(file.OIDC.UserMap) > 0 {
			eff.OIDC.UserMap = file.OIDC.UserMap
		}
		if file.OIDC.DefaultRole != "" {
			eff.OIDC.DefaultRole = file.OIDC.DefaultRole
		}
	}
	if file.AgentAuth != nil {
		// YAML has higher precedence than environment variables, including an
		// explicit empty legacySharedToken that disables compatibility mode.
		eff.AgentAuth.LegacySharedToken = file.AgentAuth.LegacySharedToken
		// Cluster credentials remain YAML-only.
		eff.AgentAuth.KubernetesClusters = append([]ControllerKubernetesClusterConfig(nil), file.AgentAuth.KubernetesClusters...)
		eff.AgentAuth.KubernetesEnrollmentPolicies = append([]ControllerKubernetesEnrollmentPolicyConfig(nil), file.AgentAuth.KubernetesEnrollmentPolicies...)
	}
	if err := validateControllerAgentAuth(eff.AgentAuth); err != nil {
		return nil, err
	}
	return eff, nil
}

func validateControllerAgentAuth(cfg *ControllerAgentAuthConfig) error {
	if cfg == nil {
		return nil
	}
	seen := map[string]bool{}
	emptyKubeconfigs := 0
	for _, cluster := range cfg.KubernetesClusters {
		name := strings.TrimSpace(cluster.Name)
		if name == "" || seen[name] {
			return fmt.Errorf("agentAuth kubernetes cluster names must be non-empty and unique")
		}
		seen[name] = true
		if strings.TrimSpace(cluster.Kubeconfig) == "" {
			emptyKubeconfigs++
		}
	}
	if emptyKubeconfigs > 1 {
		return fmt.Errorf("at most one kubernetes cluster may omit kubeconfig")
	}
	policies := map[string]bool{}
	for _, policy := range cfg.KubernetesEnrollmentPolicies {
		if policies[policy.Name] {
			return fmt.Errorf("agentAuth kubernetes enrollment policy names must be unique")
		}
		policies[policy.Name] = true
		if _, ok := seen[policy.Cluster]; !ok {
			return fmt.Errorf("agentAuth kubernetes enrollment policy references an unknown cluster")
		}
		if _, err := policy.StorePolicy(); err != nil {
			return err
		}
	}
	return nil
}
