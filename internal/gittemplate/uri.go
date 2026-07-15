package gittemplate

import (
	"fmt"
	"regexp"
	"strings"
)

var refAllowed = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/+-]*$`)

// URI represents a parsed git:// template URI.
type URI struct {
	Host  string // e.g. github.com
	Owner string // e.g. org
	Repo  string // e.g. repo
	Path  string // e.g. jobs/build.yaml
	Ref   string // e.g. v1.2.3, main, abc123...
	Raw   string // original string
}

// ParseURI parses "git://host/owner/repo/path@ref" into a URI.
// Returns an error if the URI is malformed or contains path traversal.
func ParseURI(raw string) (URI, error) {
	if !strings.HasPrefix(raw, "git://") {
		return URI{}, fmt.Errorf("git URI must start with git://, got %q", raw)
	}
	rest := strings.TrimPrefix(raw, "git://")

	// Split on "@" to get ref
	atIdx := strings.LastIndex(rest, "@")
	if atIdx < 0 {
		return URI{}, fmt.Errorf("git URI must contain @ref, got %q", raw)
	}
	ref := rest[atIdx+1:]
	hostAndPath := rest[:atIdx]

	if ref == "" {
		return URI{}, fmt.Errorf("git URI has empty ref in %q", raw)
	}
	if !refAllowed.MatchString(ref) {
		return URI{}, fmt.Errorf("git URI ref %q contains invalid characters (must match %s)", ref, refAllowed.String())
	}

	// Split host/owner/repo/path (minimum 4 segments: host, owner, repo, file)
	parts := strings.SplitN(hostAndPath, "/", 4)
	if len(parts) < 4 {
		return URI{}, fmt.Errorf("git URI must be git://host/owner/repo/path@ref, got %q", raw)
	}
	host, owner, repo, filePath := parts[0], parts[1], parts[2], parts[3]

	if host == "" || owner == "" || repo == "" || filePath == "" {
		return URI{}, fmt.Errorf("git URI has empty component in %q", raw)
	}

	// Path traversal protection
	if strings.Contains(filePath, "..") {
		return URI{}, fmt.Errorf("git URI path must not contain .., got %q", raw)
	}

	return URI{Host: host, Owner: owner, Repo: repo, Path: filePath, Ref: ref, Raw: raw}, nil
}

// IsFixed reports whether this URI refers to an immutable git ref.
// Semver tags (v*) and 40-character hex SHAs are considered fixed.
// Branch names (e.g. main, feature-branch) are mutable and return false.
func (u URI) IsFixed() bool {
	r := u.Ref
	// 40-character hex SHA
	if len(r) == 40 && isHexString(r) {
		return true
	}
	// semver-style tag: starts with 'v' followed by a digit
	if len(r) > 1 && r[0] == 'v' && r[1] >= '0' && r[1] <= '9' {
		return true
	}
	return false
}

func isHexString(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
