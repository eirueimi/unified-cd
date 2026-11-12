package dsl

import (
	"fmt"
	"regexp"
	"strings"
)

var repoSegmentRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ParseRepositoryPatterns validates jobAccess repository patterns ("owner/name",
// "owner/*" or "owner/prefix*") and returns the shared owner (as written in the
// first pattern) and the name part of each pattern. Owners are compared
// case-insensitively; patterns naming different owners are rejected. Errors are
// prefixed with the offending field path ("repositories...").
func ParseRepositoryPatterns(patterns []string) (owner string, names []string, err error) {
	if len(patterns) == 0 {
		return "", nil, fmt.Errorf("repositories must not be empty")
	}
	for i, p := range patterns {
		o, n, ok := strings.Cut(p, "/")
		if !ok || !repoSegmentRe.MatchString(o) || !validRepoNamePattern(n) {
			return "", nil, fmt.Errorf("repositories[%d] %q: want \"owner/name\", \"owner/*\" or \"owner/prefix*\"", i, p)
		}
		if i == 0 {
			owner = o
		} else if !strings.EqualFold(owner, o) {
			return "", nil, fmt.Errorf("repositories must name a single owner, got %q and %q", owner, o)
		}
		names = append(names, n)
	}
	return owner, names, nil
}

func validRepoNamePattern(s string) bool {
	if s == "*" {
		return true
	}
	return repoSegmentRe.MatchString(strings.TrimSuffix(s, "*"))
}

// MatchRepositoryName reports whether name matches a repository name pattern:
// "*" matches every name, "prefix*" matches names starting with prefix, and any
// other pattern must equal name. Matching is case-insensitive.
func MatchRepositoryName(pattern, name string) bool {
	if pattern == "*" {
		return true
	}
	pattern, name = strings.ToLower(pattern), strings.ToLower(name)
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(name, prefix)
	}
	return pattern == name
}

// RepositoryAllowed reports whether owner/repo is covered by any of the
// "owner/name" patterns. The owner is compared case-insensitively.
func RepositoryAllowed(patterns []string, owner, repo string) bool {
	for _, p := range patterns {
		o, n, ok := strings.Cut(p, "/")
		if ok && strings.EqualFold(o, owner) && MatchRepositoryName(n, repo) {
			return true
		}
	}
	return false
}
