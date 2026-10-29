package authz

import "sort"

// Set is a bit set of permissions; bit i is All()[i].
type Set uint32

// AllSet is the set holding every catalogue permission.
func AllSet() Set { return Set(1)<<len(catalogue) - 1 }

// Has reports whether p is in the set. Unknown permissions are never held.
func (s Set) Has(p Permission) bool {
	i := bitOf(p)
	return i >= 0 && s&(1<<i) != 0
}

// With returns the set plus ps; unknown permissions are ignored.
func (s Set) With(ps ...Permission) Set {
	for _, p := range ps {
		if i := bitOf(p); i >= 0 {
			s |= 1 << i
		}
	}
	return s
}

// Union returns s plus o.
func (s Set) Union(o Set) Set { return s | o }

// Intersect returns the permissions in both s and o.
func (s Set) Intersect(o Set) Set { return s & o }

// FolderPart returns only the folder-scopable permissions of s.
func (s Set) FolderPart() Set { return s & (Set(1)<<folderScopableCount - 1) }

// IsEmpty reports whether the set holds no permission.
func (s Set) IsEmpty() bool { return s == 0 }

// Names returns the dotted names in ascending order.
func (s Set) Names() []string {
	names := []string{}
	for i, p := range catalogue {
		if s&(1<<i) != 0 {
			names = append(names, string(p))
		}
	}
	sort.Strings(names)
	return names
}
