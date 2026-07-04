package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// ControllerOIDCConfig holds OIDC provider settings.
type ControllerOIDCConfig struct {
	Issuer         string `yaml:"issuer"`
	IssuerInternal string `yaml:"issuerInternal"`
	// ExternalURL is the base URL for browser SSO redirect URIs (e.g. http://localhost:8080).
	// Must be set explicitly when accessed through a reverse proxy such as Vite, because r.Host
	// will be the container-internal name. Falls back to the request's Host header when not set.
	ExternalURL    string `yaml:"externalUrl"`
	ClientID       string `yaml:"clientId"`
	ClientSecret   string `yaml:"clientSecret"`
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
	DSN           string                `yaml:"dsn"`
	Addr          string                `yaml:"addr"`
	Token         string                `yaml:"token"`
	S3Endpoint    string                `yaml:"s3Endpoint"`
	S3Bucket      string                `yaml:"s3Bucket"`
	S3Key         string                `yaml:"s3Key"`
	S3Secret      string                `yaml:"s3Secret"`
	DataDir       string                `yaml:"dataDir"`
	ControllerKey string                `yaml:"controllerKey"`
	WebDir        string                `yaml:"webDir"`
	UIProxyTarget string                `yaml:"uiProxyTarget"`
	OIDC          *ControllerOIDCConfig `yaml:"oidc"`
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
		DSN:           os.Getenv("UNIFIED_DB_DSN"),
		Token:         os.Getenv("UNIFIED_TOKEN"),
		S3Endpoint:    os.Getenv("UNIFIED_S3_ENDPOINT"),
		S3Bucket:      os.Getenv("UNIFIED_S3_BUCKET"),
		S3Key:         os.Getenv("UNIFIED_S3_KEY"),
		S3Secret:      os.Getenv("UNIFIED_S3_SECRET"),
		DataDir:       os.Getenv("UNIFIED_DATA_DIR"),
		ControllerKey: os.Getenv("UNIFIED_CONTROLLER_KEY"),
		WebDir:        os.Getenv("UNIFIED_WEB_DIR"),
		UIProxyTarget: os.Getenv("UNIFIED_UI_PROXY_TARGET"),
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
	if file.ControllerKey != "" {
		eff.ControllerKey = file.ControllerKey
	}
	if file.WebDir != "" {
		eff.WebDir = file.WebDir
	}
	if file.UIProxyTarget != "" {
		eff.UIProxyTarget = file.UIProxyTarget
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
	return eff, nil
}
