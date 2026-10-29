package authz

import (
	"fmt"
	"sort"
	"strings"
)

// source is one grant with the reason it exists. folder == "" means global.
type source struct {
	folder string
	perms  Set
	by     string
}

// Grants is the set of permissions a principal holds, globally and per folder.
// The zero value holds nothing.
type Grants struct {
	global  Set
	folders map[string]Set
	sources []source // insertion order
}

// Add records a grant. folder == "" means global; a folder grant must already
// be reduced with FolderPart by the caller. Ordering for Decide and Explain is
// applied when deciding, not here. Empty grants are ignored.
func (g *Grants) Add(folder string, perms Set, by string) {
	if perms.IsEmpty() {
		return
	}
	if folder == "" {
		g.global = g.global.Union(perms)
	} else {
		if g.folders == nil {
			g.folders = map[string]Set{}
		}
		g.folders[folder] = g.folders[folder].Union(perms)
	}
	g.sources = append(g.sources, source{folder: folder, perms: perms, by: by})
}

// Union returns the grants of g and o combined; neither input is modified.
func (g Grants) Union(o Grants) Grants {
	var out Grants
	for _, s := range g.sources {
		out.Add(s.folder, s.perms, s.by)
	}
	for _, s := range o.sources {
		out.Add(s.folder, s.perms, s.by)
	}
	return out
}

// Can reports whether p is held on job: globally, or on a proper ancestor
// folder of job. Root-level jobs are covered by global grants only.
func (g Grants) Can(p Permission, job string) bool {
	if g.global.Has(p) {
		return true
	}
	for _, f := range Ancestors(job) {
		if g.folders[f].Has(p) {
			return true
		}
	}
	return false
}

// ordered returns the sources that grant p on job: global first, then folders
// by ascending depth, then insertion order.
func (g Grants) ordered(p Permission, job string) []source {
	var global []source
	var folders []source
	for _, s := range g.sources {
		if !s.perms.Has(p) {
			continue
		}
		if s.folder == "" {
			global = append(global, s)
		} else if Covers(s.folder, job) {
			folders = append(folders, s)
		}
	}
	sort.SliceStable(folders, func(i, j int) bool {
		return strings.Count(folders[i].folder, "/") < strings.Count(folders[j].folder, "/")
	})
	return append(global, folders...)
}

// Decide reports whether p is held on job and which source is reported first.
func (g Grants) Decide(p Permission, job string) (ok bool, grantedBy string) {
	srcs := g.ordered(p, job)
	if len(srcs) == 0 {
		return false, ""
	}
	return true, srcs[0].by
}

// Explain lists every source granting p on job, in Decide order.
func (g Grants) Explain(p Permission, job string) []string {
	srcs := g.ordered(p, job)
	out := make([]string, 0, len(srcs))
	for _, s := range srcs {
		out = append(out, s.by)
	}
	return out
}

// HeldAnywhere reports whether p is held globally or on any folder.
func (g Grants) HeldAnywhere(p Permission) bool {
	if g.global.Has(p) {
		return true
	}
	for _, s := range g.folders {
		if s.Has(p) {
			return true
		}
	}
	return false
}

// IsEmpty reports whether nothing is granted anywhere.
func (g Grants) IsEmpty() bool {
	return len(g.sources) == 0
}

// Restrict intersects the global set and every folder set with ceiling and
// appends suffix (when non-empty) to every grantedBy. g is not modified.
func (g Grants) Restrict(ceiling Set, suffix string) Grants {
	var out Grants
	for _, s := range g.sources {
		out.Add(s.folder, s.perms.Intersect(ceiling), s.by+suffix)
	}
	return out
}

// GrantsView is the JSON form of Grants.
type GrantsView struct {
	Global  []string            `json:"global"`
	Folders map[string][]string `json:"folders"`
}

// View returns the JSON form: sorted names, never nil, empty folders omitted.
func (g Grants) View() GrantsView {
	v := GrantsView{Global: g.global.Names(), Folders: map[string][]string{}}
	for f, s := range g.folders {
		if !s.IsEmpty() {
			v.Folders[f] = s.Names()
		}
	}
	return v
}

// FromView rebuilds Grants from its JSON form; the source string is "view".
// Unknown permissions, global-only permissions on a folder and invalid folder
// keys are errors.
func FromView(v GrantsView) (Grants, error) {
	var g Grants
	global, err := parseSet(v.Global)
	if err != nil {
		return Grants{}, fmt.Errorf("global: %w", err)
	}
	g.Add("", global, "view")
	keys := make([]string, 0, len(v.Folders))
	for k := range v.Folders {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		folder, err := NormalizeFolder(k)
		if err != nil {
			return Grants{}, fmt.Errorf("folder %q: %w", k, err)
		}
		set, err := parseSet(v.Folders[k])
		if err != nil {
			return Grants{}, fmt.Errorf("folder %q: %w", k, err)
		}
		if set != set.FolderPart() {
			return Grants{}, fmt.Errorf("folder %q: grants a global-only permission", k)
		}
		g.Add(folder, set, "view")
	}
	return g, nil
}

func parseSet(names []string) (Set, error) {
	var s Set
	for _, n := range names {
		p, ok := Parse(n)
		if !ok {
			return 0, fmt.Errorf("unknown permission %q", n)
		}
		s = s.With(p)
	}
	return s, nil
}

// RoleGrants returns the global bundle of role; empty for an unknown or empty role.
func RoleGrants(role, by string) Grants {
	var g Grants
	if set, ok := Bundle(role); ok {
		g.Add("", set, by)
	}
	return g
}

// AllGrants returns every permission, globally.
func AllGrants(by string) Grants {
	var g Grants
	g.Add("", AllSet(), by)
	return g
}
