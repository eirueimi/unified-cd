package dsl

import (
	"fmt"
	"regexp"
	"strings"
)

// gitRefRe is the shared allowlist for user-supplied git refs (branches, tags,
// full SHAs). Anchored to start alphanumeric so a ref can never be parsed as a
// git option (-... / --...), and restricted to a conservative charset that
// excludes relative-ref syntax (HEAD~1, @{upstream}) and shell metacharacters.
// internal/gittemplate's uses:// URI parsing delegates here; AppSource
// targetRevision validation uses it directly.
var gitRefRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/+-]*$`)

// ValidateGitRef rejects a ref that could inject git options or relative-ref
// syntax into a git argv.
func ValidateGitRef(ref string) error {
	if ref == "" {
		return fmt.Errorf("git ref is required")
	}
	if !gitRefRe.MatchString(ref) {
		return fmt.Errorf("git ref %q contains invalid characters (must start alphanumeric; allowed: A-Z a-z 0-9 . _ / + -)", ref)
	}
	return nil
}

// ValidateGitRepoURL restricts a repository URL to network transports the
// controller expects: https://, http://, ssh://, or scp-like git@host:path.
// This blocks git option injection (a URL starting with '-' would be read as
// an option by ls-remote/fetch), local/ext transports (file://, ext:: — which
// can execute commands), and schemeless strings.
func ValidateGitRepoURL(url string) error {
	if url == "" {
		return fmt.Errorf("repo URL is required")
	}
	if strings.HasPrefix(url, "-") {
		return fmt.Errorf("repo URL %q must not start with '-'", url)
	}
	switch {
	case strings.HasPrefix(url, "https://"), strings.HasPrefix(url, "http://"), strings.HasPrefix(url, "ssh://"):
		return nil
	}
	// scp-like: git@host:path (no scheme). Require the git@host: shape.
	if m := regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9._-]+:`).MatchString(url); m {
		return nil
	}
	return fmt.Errorf("repo URL %q must use https://, http://, ssh://, or scp-like user@host: form", url)
}
