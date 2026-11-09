package config

import "fmt"

// DefaultAdminToken is the admin token value that ships in the documented
// quickstarts, the root dev docker-compose.yaml, .env.example and the Helm
// chart's trial secret. It is public, so the controller refuses to start with
// it unless dev mode or an explicit opt-in is set (see CheckAdminToken). This
// is the single place the value lives in Go code.
const DefaultAdminToken = "dev-token-change-me"

// CheckAdminToken rejects a well-known default admin token. devMode is
// UNIFIED_DEV_MODE; allowDefault is --allow-default-token /
// UNIFIED_ALLOW_DEFAULT_TOKEN. An empty token is handled by the caller.
func CheckAdminToken(token string, devMode, allowDefault bool) error {
	if token != DefaultAdminToken || devMode || allowDefault {
		return nil
	}
	return fmt.Errorf("the admin token (UNIFIED_TOKEN) is set to the publicly documented default value; " +
		"anyone who has read the docs can log in as admin. Set UNIFIED_TOKEN to a long random value " +
		"(for example: openssl rand -hex 32). For a throwaway trial or local development only, set " +
		"UNIFIED_DEV_MODE=1 or UNIFIED_ALLOW_DEFAULT_TOKEN=1 (or pass --allow-default-token) to accept the default")
}
