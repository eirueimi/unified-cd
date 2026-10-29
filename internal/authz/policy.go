package authz

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// PolicyConfig is the decoded oidc.permissions block. Keys keep file order so
// startup errors and explain output are deterministic.
type PolicyConfig struct {
	Global  PolicyEntry
	Folders []FolderEntry
}

// PolicyEntry holds the grants of one scope, keyed by group name or user
// identity (email or sub).
type PolicyEntry struct {
	Groups []GrantEntry
	Users  []GrantEntry
}

// FolderEntry is one entry of oidc.permissions.folders; Key is the raw key.
type FolderEntry struct {
	Key string
	PolicyEntry
}

// GrantEntry is one key with the role and permission names granted to it.
type GrantEntry struct {
	Key    string
	Grants []string
}

const policyPath = "oidc.permissions"

// UnmarshalYAML walks the node by hand so that errors carry the exact path of
// the offending key.
func (c *PolicyConfig) UnmarshalYAML(n *yaml.Node) error {
	pairs, err := mappingPairs(n, policyPath)
	if err != nil {
		return err
	}
	var out PolicyConfig
	for _, kv := range pairs {
		switch kv.key {
		case "global":
			out.Global, err = decodePolicyEntry(kv.val, policyPath+".global")
		case "folders":
			out.Folders, err = decodeFolders(kv.val)
		default:
			err = fmt.Errorf("%s: unknown field %q", policyPath, kv.key)
		}
		if err != nil {
			return err
		}
	}
	*c = out
	return nil
}

type pair struct {
	key string
	val *yaml.Node
}

func resolveAlias(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

func isNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Tag == "!!null"
}

// mappingPairs returns the key/value pairs of a mapping node in file order. A
// null node is an empty mapping.
func mappingPairs(n *yaml.Node, path string) ([]pair, error) {
	n = resolveAlias(n)
	if n == nil || isNull(n) {
		return nil, nil
	}
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: must be a mapping", path)
	}
	var out []pair
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := resolveAlias(n.Content[i])
		if k.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("%s: keys must be scalars", path)
		}
		if seen[k.Value] {
			return nil, fmt.Errorf("%s: duplicate key %q", path, k.Value)
		}
		seen[k.Value] = true
		out = append(out, pair{key: k.Value, val: n.Content[i+1]})
	}
	return out, nil
}

func decodeFolders(n *yaml.Node) ([]FolderEntry, error) {
	path := policyPath + ".folders"
	pairs, err := mappingPairs(n, path)
	if err != nil {
		return nil, err
	}
	var out []FolderEntry
	for _, kv := range pairs {
		e, err := decodePolicyEntry(kv.val, path+"["+strconv.Quote(kv.key)+"]")
		if err != nil {
			return nil, err
		}
		out = append(out, FolderEntry{Key: kv.key, PolicyEntry: e})
	}
	return out, nil
}

func decodePolicyEntry(n *yaml.Node, path string) (PolicyEntry, error) {
	pairs, err := mappingPairs(n, path)
	if err != nil {
		return PolicyEntry{}, err
	}
	var e PolicyEntry
	for _, kv := range pairs {
		switch kv.key {
		case "groups":
			e.Groups, err = decodeGrantEntries(kv.val, path+".groups")
		case "users":
			e.Users, err = decodeGrantEntries(kv.val, path+".users")
		default:
			err = fmt.Errorf("%s: unknown field %q", path, kv.key)
		}
		if err != nil {
			return PolicyEntry{}, err
		}
	}
	return e, nil
}

func decodeGrantEntries(n *yaml.Node, path string) ([]GrantEntry, error) {
	pairs, err := mappingPairs(n, path)
	if err != nil {
		return nil, err
	}
	var out []GrantEntry
	for _, kv := range pairs {
		p := path + "[" + strconv.Quote(kv.key) + "]"
		grants, err := decodeGrant(kv.val, p)
		if err != nil {
			return nil, err
		}
		out = append(out, GrantEntry{Key: kv.key, Grants: grants})
	}
	return out, nil
}

// decodeGrant accepts a scalar, a sequence of scalars, or null (an empty
// grant, reported by Compile).
func decodeGrant(n *yaml.Node, path string) ([]string, error) {
	bad := fmt.Errorf("%s: grant must be a role or permission name or a list of them", path)
	n = resolveAlias(n)
	switch {
	case isNull(n):
		return nil, nil
	case n.Kind == yaml.ScalarNode:
		return []string{n.Value}, nil
	case n.Kind == yaml.SequenceNode:
		out := []string{}
		for _, item := range n.Content {
			item = resolveAlias(item)
			if item.Kind != yaml.ScalarNode || isNull(item) {
				return nil, bad
			}
			out = append(out, item.Value)
		}
		return out, nil
	}
	return nil, bad
}

// Legacy carries the pre-existing role-resolution settings so Compile can
// validate them next to the new block.
type Legacy struct {
	RolesClaim  string
	RoleMap     map[string]string
	UserMap     map[string]string
	DefaultRole string
}

// Identity is what Match compares policy keys against.
type Identity struct {
	Values     []string // values of the configured roles claim
	Email, Sub string
}

type policyGrant struct {
	key string
	set Set
}

type policyScope struct {
	folder string // "" = global
	groups []policyGrant
	users  []policyGrant
}

// Policy is the compiled oidc.permissions block. A nil Policy is empty.
type Policy struct {
	scopes []policyScope // global first, then folders in file order
}

// Compile validates cfg and the legacy maps. It returns a non-nil Policy even
// when cfg is nil. With cfg == nil, unknown legacy roles are returned as
// warnings; with cfg != nil the first one (sorted key order) is an error.
func Compile(cfg *PolicyConfig, legacy Legacy, issuer string) (*Policy, []string, error) {
	lines := legacyRoleLines(legacy)
	if cfg == nil {
		return &Policy{}, lines, nil
	}
	if issuer == "" {
		return nil, nil, fmt.Errorf("%s is set but oidc.issuer is not", policyPath)
	}
	if len(lines) > 0 {
		return nil, nil, fmt.Errorf("%s", lines[0])
	}
	p := &Policy{}
	global, err := compileScope("", policyPath+".global", cfg.Global)
	if err != nil {
		return nil, nil, err
	}
	p.scopes = append(p.scopes, global)

	seen := map[string]string{} // normalised folder -> first raw key
	for _, f := range cfg.Folders {
		path := policyPath + ".folders[" + strconv.Quote(f.Key) + "]"
		folder, err := NormalizeFolder(f.Key)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", path, err)
		}
		if first, dup := seen[folder]; dup {
			return nil, nil, fmt.Errorf("%s.folders: %q and %q are the same folder", policyPath, first, f.Key)
		}
		seen[folder] = f.Key
		if len(f.Groups) == 0 && len(f.Users) == 0 {
			return nil, nil, fmt.Errorf("%s: no groups or users", path)
		}
		scope, err := compileScope(folder, path, f.PolicyEntry)
		if err != nil {
			return nil, nil, err
		}
		p.scopes = append(p.scopes, scope)
	}
	return p, nil, nil
}

func compileScope(folder, path string, e PolicyEntry) (policyScope, error) {
	s := policyScope{folder: folder}
	var err error
	if s.groups, err = compileGrants(folder, path+".groups", e.Groups); err != nil {
		return policyScope{}, err
	}
	if s.users, err = compileGrants(folder, path+".users", e.Users); err != nil {
		return policyScope{}, err
	}
	return s, nil
}

func compileGrants(folder, path string, entries []GrantEntry) ([]policyGrant, error) {
	var out []policyGrant
	for _, e := range entries {
		p := path + "[" + strconv.Quote(e.Key) + "]"
		if len(e.Grants) == 0 {
			return nil, fmt.Errorf("%s: empty grant", p)
		}
		var set Set
		for _, name := range e.Grants {
			if bundle, ok := Bundle(name); ok {
				if folder != "" {
					bundle = bundle.FolderPart()
				}
				set = set.Union(bundle)
				continue
			}
			perm, ok := Parse(name)
			if !ok {
				return nil, fmt.Errorf("%s: unknown permission or role %q", p, name)
			}
			if folder != "" && !FolderScopable(perm) {
				return nil, fmt.Errorf("%s: permission %q is global-only and cannot be granted on a folder", p, name)
			}
			set = set.With(perm)
		}
		out = append(out, policyGrant{key: e.Key, set: set})
	}
	return out, nil
}

// legacyRoleLines reports roleMap/userMap values that are not a built-in role.
// "deny" and "" are meaningful (or harmless) and are not reported.
func legacyRoleLines(l Legacy) []string {
	var lines []string
	check := func(field string, m map[string]string) {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			r := m[k]
			if r == "" || r == "deny" {
				continue
			}
			if _, ok := Bundle(r); !ok {
				lines = append(lines, fmt.Sprintf("oidc.%s[%s]: unknown role %s is ignored (valid: %s)",
					field, strconv.Quote(k), strconv.Quote(r), strings.Join(RoleNames, ", ")))
			}
		}
	}
	check("roleMap", l.RoleMap)
	check("userMap", l.UserMap)
	return lines
}

// Match returns the grants of every entry that matches id. matched is true
// when any entry matched. users keys match the email and the sub, both.
func (p *Policy) Match(id Identity) (Grants, bool) {
	var g Grants
	if p == nil {
		return g, false
	}
	matched := false
	for _, s := range p.scopes {
		scope := "global"
		if s.folder != "" {
			scope = "folder:" + s.folder
		}
		for _, e := range s.groups {
			for _, v := range id.Values {
				if v == e.key {
					matched = true
					g.Add(s.folder, e.set, fmt.Sprintf("%s (groups[%q])", scope, e.key))
					break
				}
			}
		}
		for _, e := range s.users {
			if (id.Email != "" && id.Email == e.key) || (id.Sub != "" && id.Sub == e.key) {
				matched = true
				g.Add(s.folder, e.set, fmt.Sprintf("%s (users[%q])", scope, e.key))
			}
		}
	}
	return g, matched
}

// Configured reports whether any grant entry exists.
func (p *Policy) Configured() bool {
	if p == nil {
		return false
	}
	for _, s := range p.scopes {
		if len(s.groups) > 0 || len(s.users) > 0 {
			return true
		}
	}
	return false
}

// HasGroupKeyedGrants reports whether any permissions.*.groups entry exists.
func (p *Policy) HasGroupKeyedGrants() bool {
	if p == nil {
		return false
	}
	for _, s := range p.scopes {
		if len(s.groups) > 0 {
			return true
		}
	}
	return false
}

// HasFolderGrants reports whether any folder entry exists.
func (p *Policy) HasFolderGrants() bool {
	if p == nil {
		return false
	}
	for _, s := range p.scopes {
		if s.folder != "" {
			return true
		}
	}
	return false
}
