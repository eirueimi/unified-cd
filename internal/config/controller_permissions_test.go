package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeControllerConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestControllerEffective_PermissionsBlockCompiles(t *testing.T) {
	path := writeControllerConfig(t, `
oidc:
  issuer: https://idp.example.com
  clientId: unified-cd
  rolesClaim: groups
  roleMap:
    "my-org:platform": admin
  userMap:
    oncall-lead@example.com: admin
  defaultRole: deny
  permissions:
    global:
      groups:
        "my-org:engineering": [agent.read, appsource.read, webhook.read, vars.read, token.create]
        "my-org:auditors":    [viewer, audit.read]
    folders:
      team-a:
        groups:
          "my-org:team-a": developer
      team-a/prod:
        groups:
          "my-org:sre": [job.read, run.approve]
      team-b:
        groups:
          "my-org:team-b": developer
        users:
          contractor@example.com: [job.read, run.trigger]
`)
	cfg, err := ControllerEffective(path)
	require.NoError(t, err)
	require.NotNil(t, cfg.OIDC.Policy)
	assert.True(t, cfg.OIDC.Policy.Configured())
	assert.True(t, cfg.OIDC.Policy.HasFolderGrants())
	assert.True(t, cfg.OIDC.Policy.HasGroupKeyedGrants())
	g, ok := cfg.OIDC.Policy.Match(authz.Identity{Email: "contractor@example.com"})
	require.True(t, ok)
	assert.True(t, g.Can(authz.RunTrigger, "team-b/build"))
	assert.False(t, g.Can(authz.RunTrigger, "team-a/build"))
}

func TestControllerEffective_PermissionsWithoutIssuerFails(t *testing.T) {
	path := writeControllerConfig(t, `
oidc:
  permissions:
    global:
      groups:
        "my-org:auditors": [viewer]
`)
	_, err := ControllerEffective(path)
	require.ErrorContains(t, err, "oidc.permissions is set but oidc.issuer is not")
}

func TestControllerEffective_IssuerFromEnvSatisfiesPermissions(t *testing.T) {
	t.Setenv("UNIFIED_OIDC_ISSUER", "https://idp.example.com")
	path := writeControllerConfig(t, `
oidc:
  permissions:
    global:
      groups:
        "my-org:auditors": [viewer]
`)
	cfg, err := ControllerEffective(path)
	require.NoError(t, err)
	assert.True(t, cfg.OIDC.Policy.Configured())
}

func TestControllerEffective_UnknownPermissionsFieldFails(t *testing.T) {
	path := writeControllerConfig(t, `
oidc:
  issuer: https://idp.example.com
  permissions:
    deny: {}
`)
	_, err := ControllerEffective(path)
	require.ErrorContains(t, err, `oidc.permissions: unknown field "deny"`)
}

func TestControllerEffective_LegacyOnlyConfigUnchanged(t *testing.T) {
	path := writeControllerConfig(t, `
oidc:
  issuer: https://dex.example.com
  clientId: unified-cd
  rolesClaim: groups
  roleMap:
    my-org:platform: admin
    my-org:devs: developer
  userMap:
    alice@example.com: admin
  defaultRole: viewer
`)
	cfg, err := ControllerEffective(path)
	require.NoError(t, err)
	assert.Empty(t, cfg.OIDC.PolicyWarnings)
	require.NotNil(t, cfg.OIDC.Policy)
	assert.False(t, cfg.OIDC.Policy.Configured())
}

func TestControllerEffective_LegacyUnknownRoleWarnsWithoutPermissions(t *testing.T) {
	path := writeControllerConfig(t, `
oidc:
  issuer: https://dex.example.com
  roleMap:
    my-org:devs: develper
`)
	cfg, err := ControllerEffective(path)
	require.NoError(t, err)
	assert.Equal(t, []string{`oidc.roleMap["my-org:devs"]: unknown role "develper" is ignored (valid: admin, developer, viewer)`}, cfg.OIDC.PolicyWarnings)
}
