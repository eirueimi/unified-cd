package authz

import (
	"sort"

	"github.com/eirueimi/unified-cd/internal/api"
)

// Bundles returns every built-in role with its global permission list.
func Bundles() map[string][]string {
	out := make(map[string][]string, len(RoleNames))
	for _, r := range RoleNames {
		set, _ := Bundle(r)
		out[r] = set.Names()
	}
	return out
}

// Describe returns the policy as resolved at startup, for the admin policy view
// (spec 5.18). It holds only role, group, user and permission names taken from
// the configuration: no client secret, token or session data. Policy entries
// keep file order; the legacy maps are sorted by key. A nil Policy is empty.
func (p *Policy) Describe(legacy Legacy) api.AuthzPolicy {
	claim := legacy.RolesClaim
	if claim == "" {
		claim = "groups"
	}
	out := api.AuthzPolicy{
		OIDC:       true,
		RolesClaim: claim,
		Bundles:    Bundles(),
		Legacy: api.AuthzLegacy{
			RoleMap:     legacyEntries(legacy.RoleMap),
			UserMap:     legacyEntries(legacy.UserMap),
			DefaultRole: legacyEntry("defaultRole", legacy.DefaultRole),
		},
		Global:  []api.AuthzGrantEntry{},
		Folders: []api.AuthzFolderEntry{},
	}
	if p == nil {
		return out
	}
	for _, s := range p.scopes {
		entries := describeGrants("group", s.groups)
		entries = append(entries, describeGrants("user", s.users)...)
		if s.folder == "" {
			out.Global = entries
			continue
		}
		out.Folders = append(out.Folders, api.AuthzFolderEntry{Folder: s.folder, Entries: entries})
	}
	return out
}

func describeGrants(subject string, grants []policyGrant) []api.AuthzGrantEntry {
	out := []api.AuthzGrantEntry{}
	for _, g := range grants {
		out = append(out, api.AuthzGrantEntry{
			Subject:     subject,
			Name:        g.key,
			Grant:       append([]string{}, g.grant...),
			Permissions: g.set.Names(),
		})
	}
	return out
}

func legacyEntries(m map[string]string) []api.AuthzLegacyEntry {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := []api.AuthzLegacyEntry{}
	for _, k := range keys {
		out = append(out, legacyEntry(k, m[k]))
	}
	return out
}

// legacyEntry resolves a legacy role value. "" and "deny" grant nothing and
// are not ignored; any other value that is not a built-in role is ignored.
func legacyEntry(key, role string) api.AuthzLegacyEntry {
	e := api.AuthzLegacyEntry{Key: key, Role: role, Permissions: []string{}}
	if set, ok := Bundle(role); ok {
		e.Permissions = set.Names()
	} else if role != "" && role != "deny" {
		e.Ignored = true
	}
	return e
}

// NoOIDCPolicy is the policy view of a controller without OIDC: only the
// built-in bundles, everything else empty.
func NoOIDCPolicy() api.AuthzPolicy {
	return api.AuthzPolicy{
		Bundles: Bundles(),
		Legacy: api.AuthzLegacy{
			RoleMap:     []api.AuthzLegacyEntry{},
			UserMap:     []api.AuthzLegacyEntry{},
			DefaultRole: api.AuthzLegacyEntry{Key: "defaultRole", Permissions: []string{}},
		},
		Global:  []api.AuthzGrantEntry{},
		Folders: []api.AuthzFolderEntry{},
	}
}
