package authz

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const describeExample = `
permissions:
  global:
    groups:
      "my-org:engineering": [agent.read, appsource.read, webhook.read, vars.read, token.create]
      "my-org:auditors": [viewer, audit.read]
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
`

func describeLegacy() Legacy {
	return Legacy{
		RolesClaim:  "groups",
		RoleMap:     map[string]string{"my-org:platform": "admin", "a-first": "viewer"},
		UserMap:     map[string]string{"oncall-lead@example.com": "admin"},
		DefaultRole: "deny",
	}
}

func compileDescribe(t *testing.T, legacy Legacy) *Policy {
	t.Helper()
	cfg, err := decodePolicy(t, describeExample)
	require.NoError(t, err)
	p, _, err := Compile(cfg, legacy, "https://idp.example.com")
	require.NoError(t, err)
	return p
}

func TestDescribeListsEveryEntryInFileOrder(t *testing.T) {
	d := compileDescribe(t, describeLegacy()).Describe(describeLegacy())

	assert.True(t, d.OIDC)
	assert.Equal(t, "groups", d.RolesClaim)
	assert.Equal(t, []string{"admin", "developer", "viewer"}, sortedKeys(d.Bundles))

	require.Len(t, d.Global, 2)
	assert.Equal(t, "group", d.Global[0].Subject)
	assert.Equal(t, "my-org:engineering", d.Global[0].Name)
	assert.Equal(t, []string{"agent.read", "appsource.read", "webhook.read", "vars.read", "token.create"}, d.Global[0].Grant)
	assert.Equal(t, "my-org:auditors", d.Global[1].Name)
	assert.Equal(t, []string{"viewer", "audit.read"}, d.Global[1].Grant)
	assert.Contains(t, d.Global[1].Permissions, "audit.read")
	assert.Contains(t, d.Global[1].Permissions, "job.read")

	require.Len(t, d.Folders, 3)
	assert.Equal(t, "team-a", d.Folders[0].Folder)
	assert.Equal(t, "team-a/prod", d.Folders[1].Folder)
	assert.Equal(t, "team-b", d.Folders[2].Folder)

	assert.Equal(t, []string{"developer"}, d.Folders[0].Entries[0].Grant)
}

func TestDescribeFolderBundleExpandsToFolderPart(t *testing.T) {
	d := compileDescribe(t, describeLegacy()).Describe(describeLegacy())
	dev := d.Folders[0].Entries[0]
	assert.ElementsMatch(t, []string{
		"job.read", "job.configure", "job.delete", "run.trigger", "run.replay",
		"run.cancel", "run.delete", "run.approve", "schedule.configure",
	}, dev.Permissions)
	for _, p := range dev.Permissions {
		perm, ok := Parse(p)
		require.True(t, ok)
		assert.True(t, FolderScopable(perm), p)
	}
}

func TestDescribeTeamBHasGroupAndUserEntries(t *testing.T) {
	d := compileDescribe(t, describeLegacy()).Describe(describeLegacy())
	b := d.Folders[2].Entries
	require.Len(t, b, 2)
	assert.Equal(t, "group", b[0].Subject)
	assert.Equal(t, "my-org:team-b", b[0].Name)
	assert.Equal(t, "user", b[1].Subject)
	assert.Equal(t, "contractor@example.com", b[1].Name)
	assert.Equal(t, []string{"job.read", "run.trigger"}, b[1].Grant)
	assert.Equal(t, []string{"job.read", "run.trigger"}, b[1].Permissions)
}

func TestDescribeLegacyMapsSortedByKey(t *testing.T) {
	d := compileDescribe(t, describeLegacy()).Describe(describeLegacy())
	require.Len(t, d.Legacy.RoleMap, 2)
	assert.Equal(t, "a-first", d.Legacy.RoleMap[0].Key)
	assert.Equal(t, "viewer", d.Legacy.RoleMap[0].Role)
	assert.Contains(t, d.Legacy.RoleMap[0].Permissions, "job.read")
	assert.Equal(t, "my-org:platform", d.Legacy.RoleMap[1].Key)
	assert.Len(t, d.Legacy.RoleMap[1].Permissions, 25)
	require.Len(t, d.Legacy.UserMap, 1)
	assert.Equal(t, "deny", d.Legacy.DefaultRole.Role)
	assert.False(t, d.Legacy.DefaultRole.Ignored)
	assert.Empty(t, d.Legacy.DefaultRole.Permissions)
}

func TestDescribeMarksUnknownLegacyRoleIgnored(t *testing.T) {
	legacy := Legacy{
		RoleMap:     map[string]string{"my-org:devs": "develper", "my-org:ok": "viewer", "my-org:none": "deny"},
		UserMap:     map[string]string{"x@example.com": "superuser"},
		DefaultRole: "",
	}
	p, warnings, err := Compile(nil, legacy, "")
	require.NoError(t, err)
	require.Len(t, warnings, 1)
	d := p.Describe(legacy)

	byKey := map[string]bool{}
	for _, e := range d.Legacy.RoleMap {
		byKey[e.Key] = e.Ignored
	}
	assert.Equal(t, map[string]bool{"my-org:devs": true, "my-org:ok": false, "my-org:none": false}, byKey)
	assert.False(t, d.Legacy.UserMap[0].Ignored, "an unknown userMap role is not ignored")
	assert.True(t, d.Legacy.UserMap[0].Denies, "it denies the user at runtime")
	assert.Empty(t, d.Legacy.UserMap[0].Permissions)
	assert.False(t, d.Legacy.DefaultRole.Ignored)
	assert.Equal(t, "groups", d.RolesClaim, "an empty rolesClaim defaults to groups")
}

func TestDescribeNilPolicy(t *testing.T) {
	var p *Policy
	d := p.Describe(Legacy{})
	assert.Empty(t, d.Global)
	assert.Empty(t, d.Folders)
	b, err := json.Marshal(d)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"global":[]`)
	assert.Contains(t, string(b), `"folders":[]`)
}

// TestDescribeHasNoSecretFields checks the JSON of a described policy: every
// object key is one of the documented policy fields (so a client secret, token
// or password field cannot appear without this test failing), the secret field
// names never occur, and "@" occurs only inside configured user keys. The
// controller test also feeds real secret values through the server config.
func TestDescribeHasNoSecretFields(t *testing.T) {
	legacy := Legacy{
		RolesClaim:  "groups",
		RoleMap:     map[string]string{"my-org:platform": "admin"},
		UserMap:     map[string]string{"oncall-lead@example.com": "admin"},
		DefaultRole: "deny",
	}
	p := compileDescribe(t, legacy)
	d := p.Describe(legacy)
	b, err := json.Marshal(d)
	require.NoError(t, err)
	out := string(b)

	for _, banned := range []string{"clientsecret", "password", `"token"`, `"secret"`} {
		assert.NotContainsf(t, strings.ToLower(out), banned, "output contains %q", banned)
	}

	// Strip every configured user key, then no "@" may remain.
	stripped := out
	for _, k := range []string{"oncall-lead@example.com", "contractor@example.com"} {
		assert.Contains(t, out, k, "configured user key must be listed")
		stripped = strings.ReplaceAll(stripped, k, "")
	}
	assert.NotContains(t, stripped, "@")

	// Every JSON object key is one of the documented field names.
	var generic any
	require.NoError(t, json.Unmarshal(b, &generic))
	allowed := map[string]bool{
		"oidc": true, "rolesClaim": true, "bundles": true, "legacy": true, "global": true, "folders": true,
		"roleMap": true, "userMap": true, "defaultRole": true, "key": true, "role": true, "ignored": true,
		"permissions": true, "subject": true, "name": true, "grant": true, "folder": true, "entries": true,
		"admin": true, "developer": true, "viewer": true,
	}
	var walk func(v any)
	walk = func(v any) {
		switch t2 := v.(type) {
		case map[string]any:
			for k, c := range t2 {
				if !allowed[k] {
					t.Errorf("unexpected JSON field %q", k)
				}
				walk(c)
			}
		case []any:
			for _, c := range t2 {
				walk(c)
			}
		}
	}
	walk(generic)
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
