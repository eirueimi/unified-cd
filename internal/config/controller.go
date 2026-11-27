package config

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/eirueimi/unified-cd/internal/store"
	"gopkg.in/yaml.v3"
)

type ControllerAgentAuthConfig struct {
	KubernetesClusters           []ControllerKubernetesClusterConfig          `yaml:"kubernetesClusters"`
	KubernetesEnrollmentPolicies []ControllerKubernetesEnrollmentPolicyConfig `yaml:"kubernetesEnrollmentPolicies"`

	// KubernetesStoreCredentialPolicies declares which namespaces and
	// ServiceAccounts, per cluster, may authenticate a job Pod's sidecar to
	// the object endpoints with a projected ServiceAccount token (presigned
	// object URLs; see docs/operator-manual/migrations/2026-11-20-run-scoped-object-access.md).
	// Deliberately its own list rather than a reuse of
	// KubernetesEnrollmentPolicies: that list authorizes the AGENT's own
	// identity (e.g. namespace "unified-cd", ServiceAccount "k8s-agent" in
	// the shipped manifests), while this one authorizes JOB POD identities
	// (e.g. namespace "ci") — a different namespace by design, and
	// deliberately so (see kubernetes-integration.md). See
	// internal/controller/object_requester.go's StoreCredentialCluster
	// doc comment for the full reasoning.
	KubernetesStoreCredentialPolicies []ControllerKubernetesStoreCredentialPolicyConfig `yaml:"kubernetesStoreCredentialPolicies"`
}
type ControllerKubernetesClusterConfig struct {
	Name       string `yaml:"name"`
	Kubeconfig string `yaml:"kubeconfig"`
}

// ControllerKubernetesStoreCredentialPolicyConfig declares one cluster's
// namespace/ServiceAccount allowlist for the store-credential broker. See
// ControllerAgentAuthConfig.KubernetesStoreCredentialPolicies.
type ControllerKubernetesStoreCredentialPolicyConfig struct {
	Cluster         string   `yaml:"cluster"`
	Namespaces      []string `yaml:"namespaces"`
	ServiceAccounts []string `yaml:"serviceAccounts"`
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
	// Capabilities are auto-detected and self-reported by the agent at register
	// time (a technical advertisement of what it can run), not admin-authorized
	// via the enrollment policy — so no AuthorizedCapabilities are seeded here.
	// Labels, by contrast, remain the admin-authorized boundary.
	return store.AgentEnrollmentPolicy{
		Name: c.Name, Provider: "kubernetes", ProviderConfig: providerConfig, SubjectConstraints: constraints,
		AgentIDTemplate: "k8s:{cluster}:{namespace}:{podUID}", AllowedLabels: append([]string(nil), c.AllowedLabels...),
		RequiredLabels: append([]string(nil), c.RequiredLabels...),
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
	// DeviceClientSecret is the secret of the device client. When set, the controller
	// relays the CLI device-flow login, refresh and revoke so the secret never leaves
	// it. Browser logins (CLI and web) use ClientSecret, not this. Requires
	// DeviceClientID. Never returned by any endpoint.
	DeviceClientSecret string `yaml:"deviceClientSecret"`
	// AuthParams are extra query parameters added to the browser login's authorization
	// request (e.g. access_type: offline). Config file only. Keys the controller sets
	// itself are rejected.
	AuthParams map[string]string `yaml:"authParams"`
	// Scopes, when non-empty, replaces the scopes requested from the IdP by the
	// browser login and the CLI device flow (openid is prepended if missing and the
	// automatic rolesClaim scope is not added). Entries must not contain whitespace.
	Scopes []string `yaml:"scopes"`

	// Role resolution (see docs/operator-manual/authorization.md).
	RolesClaim  string            `yaml:"rolesClaim"`
	RoleMap     map[string]string `yaml:"roleMap"`
	UserMap     map[string]string `yaml:"userMap"`
	DefaultRole string            `yaml:"defaultRole"`

	// Permissions is the optional matrix-authorization block (config file only).
	Permissions *authz.PolicyConfig `yaml:"permissions"`
	// Policy is Permissions compiled at startup; PolicyWarnings are the
	// startup warnings Compile produced. Neither is read from YAML.
	Policy         *authz.Policy `yaml:"-"`
	PolicyWarnings []string      `yaml:"-"`
}

// ControllerConfig holds all configuration for the controller binary.
// Populated from a YAML file via LoadController; zero-value fields mean "not set".
type ControllerConfig struct {
	DSN  string `yaml:"dsn"`
	Addr string `yaml:"addr"`
	// ExternalURL is the controller's public base URL (env: UNIFIED_EXTERNAL_URL).
	// It is used to build run links handed to agents (UNIFIED_RUN_URL). When unset,
	// ControllerEffective falls back to oidc.externalUrl.
	ExternalURL string `yaml:"externalUrl"`
	// Token is a legacy key (env: UNIFIED_TOKEN). The controller no longer
	// authenticates with it; the value is only kept so startup can log that it
	// is ignored.
	Token      string `yaml:"token"`
	S3Endpoint string `yaml:"s3Endpoint"`
	S3Bucket   string `yaml:"s3Bucket"`
	S3Key      string `yaml:"s3Key"`
	S3Secret   string `yaml:"s3Secret"`
	// S3UseSSL makes the controller reach the store over https (env: UNIFIED_S3_USE_SSL).
	// Presigned URLs travel to clients, so S3 and GCS need it.
	S3UseSSL bool `yaml:"s3UseSSL"`
	// S3Region is the bucket region (env: UNIFIED_S3_REGION); "auto" for GCS.
	S3Region      string    `yaml:"s3Region"`
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
	// StoreCredentialRequireRunBinding is DEPRECATED and ignored: run binding
	// is always required for presigned object access. Still parsed so an
	// existing config keeps loading; setting it true adds a startup warning
	// (env: UNIFIED_STORE_CREDENTIAL_REQUIRE_RUN_BINDING).
	StoreCredentialRequireRunBinding bool `yaml:"storeCredentialRequireRunBinding"`
	// Warnings are startup notices produced while resolving the effective
	// config (deprecated settings); not read from YAML.
	Warnings []string `yaml:"-"`
	// AllowDefaultToken is a legacy key, accepted and ignored so that an
	// existing config file keeps loading (env: UNIFIED_ALLOW_DEFAULT_TOKEN).
	// The controller no longer has a default admin token.
	AllowDefaultToken bool `yaml:"allowDefaultToken"`
	// TrustedProxies lists the CIDRs (or bare IPs) of reverse proxies whose
	// X-Forwarded-For / X-Real-IP headers the controller believes. Empty (the
	// default) ignores those headers and uses the TCP peer address as the
	// client IP (env: UNIFIED_TRUSTED_PROXIES, comma-separated).
	TrustedProxies []string `yaml:"trustedProxies"`
	// StoreCredentialRateLimit bounds the object endpoints
	// (POST /api/v1/runs/{runID}/objects/presign): per agent, or per client
	// address for job Pod sidecars. Zero fields mean the defaults (burst 200, 20 requests/second).
	// Env: UNIFIED_STORE_CREDENTIAL_RATE_BURST, UNIFIED_STORE_CREDENTIAL_RATE_PER_SECOND.
	StoreCredentialRateLimit StoreCredentialRateLimitConfig `yaml:"storeCredentialRateLimit"`
	// MaxRequestBodyBytes caps the body of an API request (agent log routes,
	// webhook ingress and artifact upload have their own limits); a larger
	// body gets 413. 0 means the built-in default, 1 MiB
	// (env: UNIFIED_MAX_REQUEST_BODY_BYTES).
	MaxRequestBodyBytes int64 `yaml:"maxRequestBodyBytes"`
	// ObjectStore holds settings about how the controller uses its object store
	// beyond the connection (S3*) settings.
	ObjectStore *ControllerObjectStoreConfig `yaml:"objectStore"`
	// CustomMetrics configures job-defined Prometheus metrics (the metrics:
	// blocks of a Job).
	CustomMetrics ControllerCustomMetricsConfig `yaml:"customMetrics"`
	// ServiceAccounts configures service accounts by name; the built-in
	// "admin" account accepts only "enabled".
	ServiceAccounts map[string]ServiceAccountConfig `yaml:"serviceAccounts"`
	// ServiceAccountGrants is ServiceAccounts compiled at startup, without the
	// built-in admin account. Not read from YAML.
	ServiceAccountGrants map[string]authz.Grants `yaml:"-"`
	// Tokens configures personal access token lifetimes.
	Tokens TokensConfig `yaml:"tokens"`
	// UI configures web UI behaviour.
	UI UIConfig `yaml:"ui"`
}

// ServiceAccountConfig is one entry of serviceAccounts.
type ServiceAccountConfig struct {
	// Enabled defaults to true.
	Enabled     *bool                            `yaml:"enabled"`
	Permissions *authz.ServiceAccountPermissions `yaml:"permissions"`
}

// Defaults of the tokens section.
const (
	DefaultTokenLifetime    = 2160 * time.Hour // 90 days
	DefaultTokenMaxLifetime = 8760 * time.Hour // 365 days
)

// TokensConfig is the tokens block. A nil field means "not set"; use the
// accessors for effective values.
// Env: UNIFIED_TOKENS_DEFAULT_LIFETIME, UNIFIED_TOKENS_MAX_LIFETIME.
type TokensConfig struct {
	// DefaultLifetime is the lifetime of a new PAT when none is requested
	// (default 2160h).
	DefaultLifetime *time.Duration `yaml:"defaultLifetime"`
	// MaxLifetime caps the lifetime of a new PAT (default 8760h; 0 = unlimited).
	MaxLifetime *time.Duration `yaml:"maxLifetime"`
}

// UnmarshalYAML accepts Go duration strings and the bare integer 0 (yaml.v3
// refuses an integer for a time.Duration, but "maxLifetime: 0" is the
// documented way to allow non-expiring tokens).
func (t *TokensConfig) UnmarshalYAML(n *yaml.Node) error {
	for i := 0; n.Kind == yaml.MappingNode && i+1 < len(n.Content); i += 2 {
		if k := n.Content[i].Value; k != "defaultLifetime" && k != "maxLifetime" {
			return fmt.Errorf("tokens: unknown field %q", k)
		}
	}
	var raw struct {
		DefaultLifetime yaml.Node `yaml:"defaultLifetime"`
		MaxLifetime     yaml.Node `yaml:"maxLifetime"`
	}
	if err := n.Decode(&raw); err != nil {
		return err
	}
	parse := func(field string, node *yaml.Node) (*time.Duration, error) {
		if node.IsZero() || (node.Kind == yaml.ScalarNode && node.Tag == "!!null") {
			return nil, nil
		}
		var d time.Duration
		if node.Kind == yaml.ScalarNode && node.Tag == "!!int" && node.Value == "0" {
			return &d, nil
		}
		if err := node.Decode(&d); err != nil {
			return nil, fmt.Errorf("tokens.%s must be a duration such as 2160h (or 0): %w", field, err)
		}
		return &d, nil
	}
	var err error
	if t.DefaultLifetime, err = parse("defaultLifetime", &raw.DefaultLifetime); err != nil {
		return err
	}
	t.MaxLifetime, err = parse("maxLifetime", &raw.MaxLifetime)
	return err
}

// UIConfig is the ui block. Env: UNIFIED_UI_ALLOW_TOKEN_ENTRY.
type UIConfig struct {
	// AllowTokenEntry keeps the manual token form in the web UI while browser SSO is
	// configured.
	AllowTokenEntry bool `yaml:"allowTokenEntry"`
}

// TokenDefaultLifetime returns the effective default PAT lifetime (2160h).
func (c *ControllerConfig) TokenDefaultLifetime() time.Duration {
	if c.Tokens.DefaultLifetime == nil {
		return DefaultTokenLifetime
	}
	return *c.Tokens.DefaultLifetime
}

// TokenMaxLifetime returns the effective maximum PAT lifetime (8760h; 0 means
// unlimited).
func (c *ControllerConfig) TokenMaxLifetime() time.Duration {
	if c.Tokens.MaxLifetime == nil {
		return DefaultTokenMaxLifetime
	}
	return *c.Tokens.MaxLifetime
}

// AdminAccountEnabled reports whether the built-in admin account is enabled
// (default true).
func (c *ControllerConfig) AdminAccountEnabled() bool {
	a, ok := c.ServiceAccounts[authz.BuiltinAdminAccount]
	return !ok || a.Enabled == nil || *a.Enabled
}

func (c TokensConfig) validate() error {
	if c.DefaultLifetime != nil && *c.DefaultLifetime <= 0 {
		return fmt.Errorf("tokens.defaultLifetime must be positive")
	}
	if c.MaxLifetime != nil && *c.MaxLifetime < 0 {
		return fmt.Errorf("tokens.maxLifetime must not be negative")
	}
	return nil
}

// tokensFromEnv reads the UNIFIED_TOKENS_* variables.
func tokensFromEnv() (TokensConfig, error) {
	var t TokensConfig
	for _, e := range []struct {
		name string
		dst  **time.Duration
	}{
		{"UNIFIED_TOKENS_DEFAULT_LIFETIME", &t.DefaultLifetime},
		{"UNIFIED_TOKENS_MAX_LIFETIME", &t.MaxLifetime},
	} {
		v := strings.TrimSpace(os.Getenv(e.name))
		if v == "" {
			continue
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			return t, fmt.Errorf("%s must be a duration such as 2160h, got %q", e.name, v)
		}
		*e.dst = &d
	}
	return t, nil
}

var serviceAccountNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// compileServiceAccounts validates the serviceAccounts block and compiles the
// grants of every account except the built-in admin.
func (c *ControllerConfig) compileServiceAccounts() error {
	c.ServiceAccountGrants = map[string]authz.Grants{}
	names := make([]string, 0, len(c.ServiceAccounts))
	for name := range c.ServiceAccounts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		sa := c.ServiceAccounts[name]
		path := "serviceAccounts[" + strconv.Quote(name) + "]"
		if name == authz.BuiltinAdminAccount {
			if sa.Permissions != nil {
				return fmt.Errorf(`%s: only "enabled" may be set for the built-in admin account`, path)
			}
			continue
		}
		if !serviceAccountNameRE.MatchString(name) || strings.HasPrefix(name, "env:") || strings.HasPrefix(name, "system:") {
			return fmt.Errorf(`%s: invalid name: must be a DNS label and must not start with "env:" or "system:"`, path)
		}
		g, err := authz.CompileServiceAccount(name, sa.Permissions)
		if err != nil {
			return err
		}
		c.ServiceAccountGrants[name] = g
	}
	return nil
}

// validateAccounts checks the tokens block and compiles service accounts.
func (c *ControllerConfig) validateAccounts() error {
	if err := c.Tokens.validate(); err != nil {
		return err
	}
	if max := c.TokenMaxLifetime(); max != 0 && c.TokenDefaultLifetime() > max {
		return fmt.Errorf("tokens.defaultLifetime must not exceed tokens.maxLifetime")
	}
	return c.compileServiceAccounts()
}

// Defaults of the customMetrics section.
const (
	DefaultCustomMetricsMaxSeries = 1000
	DefaultCustomMetricsTTL       = 24 * time.Hour
)

// ControllerCustomMetricsConfig is the customMetrics block of the controller
// config. A nil field means "not set"; use the accessors for effective values.
// Env: UNIFIED_CUSTOM_METRICS_ENABLED, UNIFIED_CUSTOM_METRICS_MAX_SERIES_PER_METRIC,
// UNIFIED_CUSTOM_METRICS_TTL.
type ControllerCustomMetricsConfig struct {
	// Enabled turns emission and export of job-defined metrics on (default true).
	Enabled *bool `yaml:"enabled"`
	// MaxSeriesPerMetric caps label combinations per exposed metric name per
	// replica (default 1000; 0 = unlimited).
	MaxSeriesPerMetric *int `yaml:"maxSeriesPerMetric"`
	// TTL is how long a series may go without an update before it is dropped
	// (default 24h; 0 disables expiry).
	TTL *time.Duration `yaml:"ttl"`
}

// CustomMetricsEnabled returns the effective enabled flag (default true).
func (c *ControllerConfig) CustomMetricsEnabled() bool {
	return c.CustomMetrics.Enabled == nil || *c.CustomMetrics.Enabled
}

// CustomMetricsMaxSeries returns the effective series cap (default 1000).
func (c *ControllerConfig) CustomMetricsMaxSeries() int {
	if c.CustomMetrics.MaxSeriesPerMetric == nil {
		return DefaultCustomMetricsMaxSeries
	}
	return *c.CustomMetrics.MaxSeriesPerMetric
}

// CustomMetricsTTL returns the effective series TTL (default 24h).
func (c *ControllerConfig) CustomMetricsTTL() time.Duration {
	if c.CustomMetrics.TTL == nil {
		return DefaultCustomMetricsTTL
	}
	return *c.CustomMetrics.TTL
}

func (c ControllerCustomMetricsConfig) validate() error {
	if c.MaxSeriesPerMetric != nil && *c.MaxSeriesPerMetric < 0 {
		return fmt.Errorf("customMetrics.maxSeriesPerMetric must not be negative")
	}
	if c.TTL != nil && *c.TTL < 0 {
		return fmt.Errorf("customMetrics.ttl must not be negative")
	}
	return nil
}

// customMetricsFromEnv reads the UNIFIED_CUSTOM_METRICS_* variables.
func customMetricsFromEnv() (ControllerCustomMetricsConfig, error) {
	var c ControllerCustomMetricsConfig
	if v := strings.TrimSpace(os.Getenv("UNIFIED_CUSTOM_METRICS_ENABLED")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return c, fmt.Errorf("UNIFIED_CUSTOM_METRICS_ENABLED must be a boolean, got %q", v)
		}
		c.Enabled = &b
	}
	if v := strings.TrimSpace(os.Getenv("UNIFIED_CUSTOM_METRICS_MAX_SERIES_PER_METRIC")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return c, fmt.Errorf("UNIFIED_CUSTOM_METRICS_MAX_SERIES_PER_METRIC must be a non-negative integer, got %q", v)
		}
		c.MaxSeriesPerMetric = &n
	}
	if v := strings.TrimSpace(os.Getenv("UNIFIED_CUSTOM_METRICS_TTL")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return c, fmt.Errorf("UNIFIED_CUSTOM_METRICS_TTL must be a non-negative duration such as 24h, got %q", v)
		}
		c.TTL = &d
	}
	return c, nil
}

// Artifact download modes for objectStore.artifactDownload.
const (
	ArtifactDownloadProxy    = "proxy"
	ArtifactDownloadRedirect = "redirect"
)

// ControllerObjectStoreConfig holds the objectStore block of the controller config.
type ControllerObjectStoreConfig struct {
	// ArtifactDownload chooses how human artifact downloads are served after
	// the usual authentication, permission check and audit: "proxy" (default)
	// streams the bytes through the controller, "redirect" answers with a
	// 15-minute presigned GET URL. Agents are always proxied.
	// Env: UNIFIED_ARTIFACT_DOWNLOAD.
	ArtifactDownload string `yaml:"artifactDownload"`
}

// ArtifactDownloadMode returns the effective artifact download mode ("proxy"
// when unset).
func (c *ControllerConfig) ArtifactDownloadMode() string {
	if c.ObjectStore == nil || c.ObjectStore.ArtifactDownload == "" {
		return ArtifactDownloadProxy
	}
	return c.ObjectStore.ArtifactDownload
}

func validateArtifactDownload(mode string) error {
	switch mode {
	case "", ArtifactDownloadProxy, ArtifactDownloadRedirect:
		return nil
	}
	return fmt.Errorf("objectStore.artifactDownload: must be %q or %q, got %q", ArtifactDownloadProxy, ArtifactDownloadRedirect, mode)
}

// CheckArtifactDownload validates an artifact download mode against the object
// store in use. canPresign is true when the store issues presigned URLs (an
// S3-compatible store); "redirect" is refused without one.
func CheckArtifactDownload(mode string, canPresign bool) error {
	if err := validateArtifactDownload(mode); err != nil {
		return err
	}
	if mode == ArtifactDownloadRedirect && !canPresign {
		return fmt.Errorf("objectStore.artifactDownload: redirect requires an S3-compatible object store")
	}
	return nil
}

// RequireRunBindingDeprecatedWarning is the startup warning for the
// deprecated storeCredentialRequireRunBinding setting.
const RequireRunBindingDeprecatedWarning = "storeCredentialRequireRunBinding is deprecated and ignored: run binding is always required for presigned object access"

// StoreCredentialRateLimitConfig is the per-client-address token bucket for the
// object endpoints (the presign endpoint).
type StoreCredentialRateLimitConfig struct {
	Burst     int     `yaml:"burst"`
	PerSecond float64 `yaml:"perSecond"`
}

func (c StoreCredentialRateLimitConfig) validate() error {
	if c.Burst < 0 || c.PerSecond < 0 {
		return fmt.Errorf("storeCredentialRateLimit burst and perSecond must not be negative")
	}
	return nil
}

// ParseTrustedProxies parses a list of CIDRs and bare IPs (a bare IP means a
// single-host prefix) into prefixes. Blank entries are rejected.
func ParseTrustedProxies(entries []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			return nil, fmt.Errorf("trustedProxies must not contain an empty entry")
		}
		if p, err := netip.ParsePrefix(e); err == nil {
			if p.Addr().Is4In6() {
				// Peers are unmapped before matching, so a mapped prefix must be
				// expressed in IPv4 form or it would silently never match.
				if p.Bits() < 96 {
					return nil, fmt.Errorf("trustedProxies entry %q is an IPv4-mapped prefix shorter than /96; write it as an IPv4 CIDR", e)
				}
				p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
			}
			out = append(out, p.Masked())
			continue
		}
		addr, err := netip.ParseAddr(e)
		if err != nil {
			return nil, fmt.Errorf("trustedProxies entry %q is not a CIDR or IP address", e)
		}
		addr = addr.Unmap()
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out, nil
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
		DSN:         os.Getenv("UNIFIED_DB_DSN"),
		ExternalURL: os.Getenv("UNIFIED_EXTERNAL_URL"),
		Token:       os.Getenv("UNIFIED_TOKEN"),
		S3Endpoint:  os.Getenv("UNIFIED_S3_ENDPOINT"),
		S3Bucket:    os.Getenv("UNIFIED_S3_BUCKET"),
		S3Key:       os.Getenv("UNIFIED_S3_KEY"),
		S3Secret:    os.Getenv("UNIFIED_S3_SECRET"),
		S3UseSSL:    envBool("UNIFIED_S3_USE_SSL"),
		S3Region:    os.Getenv("UNIFIED_S3_REGION"),
		DataDir:     os.Getenv("UNIFIED_DATA_DIR"),
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
		WebDir:                           os.Getenv("UNIFIED_WEB_DIR"),
		UIProxyTarget:                    os.Getenv("UNIFIED_UI_PROXY_TARGET"),
		StderrPlain:                      envBool("UNIFIED_LOG_STDERR_PLAIN"),
		InsecureCookies:                  envBool("UNIFIED_INSECURE_COOKIES"),
		AllowDefaultToken:                envBool("UNIFIED_ALLOW_DEFAULT_TOKEN"),
		StoreCredentialRequireRunBinding: envBool("UNIFIED_STORE_CREDENTIAL_REQUIRE_RUN_BINDING"),
		TrustedProxies:                   SplitCSV(os.Getenv("UNIFIED_TRUSTED_PROXIES")),
		AgentAuth:                        &ControllerAgentAuthConfig{},
	}
	cm, err := customMetricsFromEnv()
	if err != nil {
		return nil, err
	}
	eff.CustomMetrics = cm
	tk, err := tokensFromEnv()
	if err != nil {
		return nil, err
	}
	eff.Tokens = tk
	eff.UI.AllowTokenEntry = envBool("UNIFIED_UI_ALLOW_TOKEN_ENTRY")
	if v := strings.TrimSpace(os.Getenv("UNIFIED_ARTIFACT_DOWNLOAD")); v != "" {
		eff.ObjectStore = &ControllerObjectStoreConfig{ArtifactDownload: v}
	}
	if v := strings.TrimSpace(os.Getenv("UNIFIED_STORE_CREDENTIAL_RATE_BURST")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("UNIFIED_STORE_CREDENTIAL_RATE_BURST must be a positive integer, got %q", v)
		}
		eff.StoreCredentialRateLimit.Burst = n
	}
	if v := strings.TrimSpace(os.Getenv("UNIFIED_STORE_CREDENTIAL_RATE_PER_SECOND")); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("UNIFIED_STORE_CREDENTIAL_RATE_PER_SECOND must be a positive number, got %q", v)
		}
		eff.StoreCredentialRateLimit.PerSecond = n
	}
	if v := strings.TrimSpace(os.Getenv("UNIFIED_MAX_REQUEST_BODY_BYTES")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("UNIFIED_MAX_REQUEST_BODY_BYTES must be a positive integer, got %q", v)
		}
		eff.MaxRequestBodyBytes = n
	}
	// OIDC from env vars
	oidcIssuer := os.Getenv("UNIFIED_OIDC_ISSUER")
	oidcClientID := os.Getenv("UNIFIED_OIDC_CLIENT_ID")
	if oidcIssuer != "" || oidcClientID != "" {
		eff.OIDC = &ControllerOIDCConfig{
			Issuer:             oidcIssuer,
			IssuerInternal:     os.Getenv("UNIFIED_OIDC_ISSUER_INTERNAL"),
			ExternalURL:        os.Getenv("UNIFIED_OIDC_EXTERNAL_URL"),
			ClientID:           oidcClientID,
			ClientSecret:       os.Getenv("UNIFIED_OIDC_CLIENT_SECRET"),
			DeviceClientID:     os.Getenv("UNIFIED_OIDC_DEVICE_CLIENT_ID"),
			DeviceClientSecret: os.Getenv("UNIFIED_OIDC_DEVICE_CLIENT_SECRET"),
			RolesClaim:         os.Getenv("UNIFIED_OIDC_ROLES_CLAIM"),
			DefaultRole:        os.Getenv("UNIFIED_OIDC_DEFAULT_ROLE"),
			Scopes:             SplitCSV(os.Getenv("UNIFIED_OIDC_SCOPES")),
		}
		if err := validateOIDCScopes(eff.OIDC.Scopes); err != nil {
			return nil, err
		}
	}

	if err := eff.KeySource.Validate(); err != nil {
		return nil, err
	}
	if err := validateArtifactDownload(eff.ArtifactDownloadMode()); err != nil {
		return nil, fmt.Errorf("UNIFIED_ARTIFACT_DOWNLOAD: %w", err)
	}
	if filePath == "" {
		if err := validateOIDCDevice(eff.OIDC); err != nil {
			return nil, err
		}
		if err := eff.validateAccounts(); err != nil {
			return nil, err
		}
		eff.applyExternalURLFallback()
		eff.addDeprecationWarnings()
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
	if file.ExternalURL != "" {
		eff.ExternalURL = file.ExternalURL
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
	if file.S3UseSSL {
		eff.S3UseSSL = true
	}
	if file.S3Region != "" {
		eff.S3Region = file.S3Region
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
	if file.AllowDefaultToken {
		eff.AllowDefaultToken = true
	}
	if file.InsecureCookies {
		eff.InsecureCookies = true
	}
	if file.StoreCredentialRequireRunBinding {
		eff.StoreCredentialRequireRunBinding = true
	}
	if err := file.StoreCredentialRateLimit.validate(); err != nil {
		return nil, err
	}
	if file.StoreCredentialRateLimit.Burst > 0 {
		eff.StoreCredentialRateLimit.Burst = file.StoreCredentialRateLimit.Burst
	}
	if file.StoreCredentialRateLimit.PerSecond > 0 {
		eff.StoreCredentialRateLimit.PerSecond = file.StoreCredentialRateLimit.PerSecond
	}
	if file.ObjectStore != nil && file.ObjectStore.ArtifactDownload != "" {
		if err := validateArtifactDownload(file.ObjectStore.ArtifactDownload); err != nil {
			return nil, err
		}
		eff.ObjectStore = &ControllerObjectStoreConfig{ArtifactDownload: file.ObjectStore.ArtifactDownload}
	}
	if err := file.CustomMetrics.validate(); err != nil {
		return nil, err
	}
	if file.CustomMetrics.Enabled != nil {
		eff.CustomMetrics.Enabled = file.CustomMetrics.Enabled
	}
	if file.CustomMetrics.MaxSeriesPerMetric != nil {
		eff.CustomMetrics.MaxSeriesPerMetric = file.CustomMetrics.MaxSeriesPerMetric
	}
	if file.CustomMetrics.TTL != nil {
		eff.CustomMetrics.TTL = file.CustomMetrics.TTL
	}
	if file.Tokens.DefaultLifetime != nil {
		eff.Tokens.DefaultLifetime = file.Tokens.DefaultLifetime
	}
	if file.Tokens.MaxLifetime != nil {
		eff.Tokens.MaxLifetime = file.Tokens.MaxLifetime
	}
	if file.UI.AllowTokenEntry {
		eff.UI.AllowTokenEntry = true
	}
	eff.ServiceAccounts = file.ServiceAccounts
	if err := eff.validateAccounts(); err != nil {
		return nil, err
	}
	if file.MaxRequestBodyBytes < 0 {
		return nil, fmt.Errorf("maxRequestBodyBytes must not be negative")
	}
	if file.MaxRequestBodyBytes > 0 {
		eff.MaxRequestBodyBytes = file.MaxRequestBodyBytes
	}
	if len(file.TrustedProxies) > 0 {
		eff.TrustedProxies = file.TrustedProxies
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
		if file.OIDC.DeviceClientSecret != "" {
			eff.OIDC.DeviceClientSecret = file.OIDC.DeviceClientSecret
		}
		if len(file.OIDC.AuthParams) > 0 {
			eff.OIDC.AuthParams = file.OIDC.AuthParams
		}
		if len(file.OIDC.Scopes) > 0 {
			eff.OIDC.Scopes = file.OIDC.Scopes
		}
		if err := validateOIDCScopes(eff.OIDC.Scopes); err != nil {
			return nil, err
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
		if file.OIDC.Permissions != nil {
			eff.OIDC.Permissions = file.OIDC.Permissions
		}
	}
	// Compile only here: roleMap, userMap and permissions are config-file only,
	// so the early return above for an empty config path has nothing to compile
	// or warn about.
	if err := validateOIDCDevice(eff.OIDC); err != nil {
		return nil, err
	}
	if eff.OIDC != nil {
		policy, warnings, err := authz.Compile(eff.OIDC.Permissions, authz.Legacy{
			RolesClaim:  eff.OIDC.RolesClaim,
			RoleMap:     eff.OIDC.RoleMap,
			UserMap:     eff.OIDC.UserMap,
			DefaultRole: eff.OIDC.DefaultRole,
		}, eff.OIDC.Issuer)
		if err != nil {
			return nil, err
		}
		eff.OIDC.Policy = policy
		eff.OIDC.PolicyWarnings = warnings
	}
	if file.AgentAuth != nil {
		// Cluster credentials remain YAML-only.
		eff.AgentAuth.KubernetesClusters = append([]ControllerKubernetesClusterConfig(nil), file.AgentAuth.KubernetesClusters...)
		eff.AgentAuth.KubernetesEnrollmentPolicies = append([]ControllerKubernetesEnrollmentPolicyConfig(nil), file.AgentAuth.KubernetesEnrollmentPolicies...)
		eff.AgentAuth.KubernetesStoreCredentialPolicies = append([]ControllerKubernetesStoreCredentialPolicyConfig(nil), file.AgentAuth.KubernetesStoreCredentialPolicies...)
	}
	if err := validateControllerAgentAuth(eff.AgentAuth); err != nil {
		return nil, err
	}
	eff.applyExternalURLFallback()
	eff.addDeprecationWarnings()
	return eff, nil
}

// applyExternalURLFallback makes oidc.externalUrl the effective ExternalURL
// when no top-level one is set.
func (c *ControllerConfig) applyExternalURLFallback() {
	if c.ExternalURL == "" && c.OIDC != nil {
		c.ExternalURL = c.OIDC.ExternalURL
	}
}

// addDeprecationWarnings records one warning per deprecated setting that is
// still set (from the environment or the config file).
func (c *ControllerConfig) addDeprecationWarnings() {
	if c.StoreCredentialRequireRunBinding {
		c.Warnings = append(c.Warnings, RequireRunBindingDeprecatedWarning)
	}
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
	for _, policy := range cfg.KubernetesStoreCredentialPolicies {
		if _, ok := seen[policy.Cluster]; !ok {
			return fmt.Errorf("agentAuth kubernetes store credential policy references an unknown cluster")
		}
		if len(policy.Namespaces) == 0 || len(policy.ServiceAccounts) == 0 {
			return fmt.Errorf("agentAuth kubernetes store credential policy requires namespaces and serviceAccounts")
		}
	}
	return nil
}

// SplitCSV splits a comma-separated list, trimming spaces and dropping empties.
// It returns nil when nothing remains.
func SplitCSV(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// oidcReservedAuthParams are the authorization-request parameters the controller
// sets itself; authParams must not override them.
var oidcReservedAuthParams = []string{
	"client_id", "redirect_uri", "response_type", "scope", "state",
	"code_challenge", "code_challenge_method", "nonce",
}

// validateOIDCDevice checks the device client secret and authParams settings.
func validateOIDCDevice(o *ControllerOIDCConfig) error {
	if o == nil {
		return nil
	}
	if o.DeviceClientSecret != "" && o.DeviceClientID == "" {
		return fmt.Errorf("oidc.deviceClientSecret requires oidc.deviceClientId")
	}
	for _, k := range oidcReservedAuthParams {
		if _, ok := o.AuthParams[k]; ok {
			return fmt.Errorf("oidc.authParams must not set %q: the controller sets it itself", k)
		}
	}
	return nil
}

// validateOIDCScopes rejects a scope entry containing whitespace: scopes are
// space-delimited on the wire, so such an entry is almost always several scopes
// written as one string.
func validateOIDCScopes(scopes []string) error {
	for _, sc := range scopes {
		if strings.ContainsAny(sc, " \t\r\n") {
			return fmt.Errorf("oidc.scopes entry %q contains whitespace; list each scope as a separate entry", sc)
		}
		if sc == "" {
			return fmt.Errorf("oidc.scopes must not contain an empty entry")
		}
	}
	return nil
}
