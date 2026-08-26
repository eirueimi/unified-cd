package objectstore

import (
	"fmt"
	"os"
	"strings"
)

// S3ConfigFromEnv builds an S3Config from the UNIFIED_S3_* environment variables.
// Endpoint and bucket are always required. Credentials are provider-dependent —
// see the precedence comment on the credential block below. UseSSL parses
// UNIFIED_S3_USE_SSL ("true"/"1" ⇒ true); Region is optional.
func S3ConfigFromEnv() (S3Config, error) {
	cfg := S3Config{
		Endpoint: os.Getenv("UNIFIED_S3_ENDPOINT"),
		Bucket:   os.Getenv("UNIFIED_S3_BUCKET"),
		Region:   os.Getenv("UNIFIED_S3_REGION"),
	}
	switch strings.ToLower(os.Getenv("UNIFIED_S3_USE_SSL")) {
	case "true", "1", "yes":
		cfg.UseSSL = true
	}

	var missing []string
	if cfg.Endpoint == "" {
		missing = append(missing, "UNIFIED_S3_ENDPOINT")
	}
	if cfg.Bucket == "" {
		missing = append(missing, "UNIFIED_S3_BUCKET")
	}

	// Credential provider selection, most specific first. Static stays last:
	// that is what keeps every deployment that only ever set UNIFIED_S3_KEY /
	// UNIFIED_S3_SECRET working exactly as before, with no new env var to
	// notice or opt out of.
	//
	// A third slot — UNIFIED_S3_WEB_IDENTITY_TOKEN_FILE, selecting
	// credentials.NewSTSWebIdentity — is deliberately not implemented here.
	// It needs an STS endpoint and a per-cloud decision that spec §5.4
	// (docs/superpowers/specs/2026-08-26-sidecar-credential-delivery-design.md)
	// has not settled; this is the slot a future provider takes.
	if credFile := os.Getenv("UNIFIED_S3_CREDENTIAL_FILE"); credFile != "" {
		cfg.Creds = NewFileCredentials(credFile)
	} else {
		cfg.AccessKeyID = os.Getenv("UNIFIED_S3_KEY")
		cfg.SecretAccessKey = os.Getenv("UNIFIED_S3_SECRET")
		// Name BOTH routes to configure credentials in the error, not just
		// the one that was almost satisfied: an operator who mounted a file
		// at the wrong path and got "missing UNIFIED_S3_KEY" would reasonably
		// conclude the file path is not a real option, rather than that
		// theirs isn't being read.
		if cfg.AccessKeyID == "" {
			missing = append(missing, "UNIFIED_S3_KEY")
		}
		if cfg.SecretAccessKey == "" {
			missing = append(missing, "UNIFIED_S3_SECRET")
		}
		if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
			missing = append(missing, "UNIFIED_S3_CREDENTIAL_FILE")
		}
	}

	if len(missing) > 0 {
		return cfg, fmt.Errorf("missing required S3 env vars: %s", strings.Join(missing, ", "))
	}
	return cfg, nil
}
