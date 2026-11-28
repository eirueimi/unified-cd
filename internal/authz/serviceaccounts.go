package authz

import (
	"fmt"
	"strconv"

	"gopkg.in/yaml.v3"
)

// BuiltinAdminAccount is the name of the built-in service account that holds
// the admin bundle globally. Only its "enabled" setting is configurable.
const BuiltinAdminAccount = "admin"

// ServiceAccountPermissions is the permissions block of one service account.
// Each grant list accepts the same forms as one users: entry of
// oidc.permissions: a role or permission name, or a list of them.
type ServiceAccountPermissions struct {
	Global  []string
	Folders []FolderGrant
}

// FolderGrant is the grants of one folder, in file order.
type FolderGrant struct {
	Folder string
	Grants []string
}

// UnmarshalYAML walks the node by hand, reusing the oidc.permissions grant
// decoding.
func (p *ServiceAccountPermissions) UnmarshalYAML(n *yaml.Node) error {
	const path = "permissions"
	pairs, err := mappingPairs(n, path)
	if err != nil {
		return err
	}
	var out ServiceAccountPermissions
	for _, kv := range pairs {
		switch kv.key {
		case "global":
			out.Global, err = decodeGrant(kv.val, path+".global")
		case "folders":
			out.Folders, err = decodeFolderGrants(kv.val, path+".folders")
		default:
			err = fmt.Errorf("%s: unknown field %q", path, kv.key)
		}
		if err != nil {
			return err
		}
	}
	*p = out
	return nil
}

func decodeFolderGrants(n *yaml.Node, path string) ([]FolderGrant, error) {
	pairs, err := mappingPairs(n, path)
	if err != nil {
		return nil, err
	}
	var out []FolderGrant
	for _, kv := range pairs {
		grants, err := decodeGrant(kv.val, path+"["+strconv.Quote(kv.key)+"]")
		if err != nil {
			return nil, err
		}
		out = append(out, FolderGrant{Folder: kv.key, Grants: grants})
	}
	return out, nil
}

// CompileServiceAccount validates p and returns the grants of service account
// name. A nil p yields empty grants. Errors are prefixed with
// serviceAccounts["<name>"].permissions.
func CompileServiceAccount(name string, p *ServiceAccountPermissions) (Grants, error) {
	var g Grants
	if p == nil {
		return g, nil
	}
	path := "serviceAccounts[" + strconv.Quote(name) + "].permissions"
	by := "sa:" + name
	if len(p.Global) > 0 {
		set, err := grantSet("", path+".global", p.Global)
		if err != nil {
			return Grants{}, err
		}
		g.Add("", set, by)
	}
	seen := map[string]string{} // normalised folder -> first raw key
	for _, f := range p.Folders {
		fp := path + ".folders[" + strconv.Quote(f.Folder) + "]"
		folder, err := NormalizeFolder(f.Folder)
		if err != nil {
			return Grants{}, fmt.Errorf("%s: %w", fp, err)
		}
		if first, dup := seen[folder]; dup {
			return Grants{}, fmt.Errorf("%s.folders: %q and %q are the same folder", path, first, f.Folder)
		}
		seen[folder] = f.Folder
		if len(f.Grants) == 0 {
			return Grants{}, fmt.Errorf("%s: empty grant", fp)
		}
		set, err := grantSet(folder, fp, f.Grants)
		if err != nil {
			return Grants{}, err
		}
		g.Add(folder, set, by)
	}
	return g, nil
}
