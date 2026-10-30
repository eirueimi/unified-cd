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
		return fmt.Errorf("repo URL %q must not start with '-'", RedactURLCredentials(url))
	}
	switch {
	case strings.HasPrefix(url, "https://"), strings.HasPrefix(url, "http://"), strings.HasPrefix(url, "ssh://"):
		// Never echo the URL here: it may carry the very credential being rejected.
		if repoURLCarriesCredentials(url) {
			return fmt.Errorf("repo URL must not embed credentials (userinfo is only allowed as a bare user name on ssh:// and scp-like URLs); store the token or key in a GitCredential for the host instead")
		}
		return nil
	}
	// scp-like: git@host:path (no scheme). Require the git@host: shape, and
	// force the host component to start alphanumeric so a dash-leading host
	// (git@-x:...) can't smuggle an ssh option like -oProxyCommand=...
	// (CVE-2017-1000117 shape).
	if m := regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9][A-Za-z0-9._-]*:`).MatchString(url); m {
		return nil
	}
	return fmt.Errorf("repo URL %q must use https://, http://, ssh://, or scp-like user@host: form", RedactURLCredentials(url))
}

// splitRepoURLUserinfo splits an http(s):// or ssh:// URL into the scheme
// prefix (including "://"), the userinfo (without the trailing "@"), and the
// remainder (host and everything after). ok is false for any other shape
// (scp-like URLs, unknown schemes), which are never rewritten.
func splitRepoURLUserinfo(u string) (scheme, userinfo, rest string, ok bool) {
	for _, p := range []string{"https://", "http://", "ssh://"} {
		if strings.HasPrefix(u, p) {
			scheme, u = p, u[len(p):]
			ok = true
			break
		}
	}
	if !ok {
		return "", "", "", false
	}
	authEnd := len(u)
	if i := strings.IndexAny(u, "/?#"); i >= 0 {
		authEnd = i
	}
	auth := u[:authEnd]
	at := strings.LastIndex(auth, "@")
	if at < 0 {
		return scheme, "", u, true
	}
	return scheme, auth[:at], auth[at+1:] + u[authEnd:], true
}

// repoURLCarriesCredentials reports whether a repo URL embeds a secret: any
// userinfo on http(s) (a bare user is usually a token), or a password on
// ssh://. A bare user name on ssh:// and the scp-like git@host:path form
// carry no secret and stay valid.
func repoURLCarriesCredentials(u string) bool {
	scheme, userinfo, _, ok := splitRepoURLUserinfo(u)
	if !ok || userinfo == "" {
		return false
	}
	if scheme == "ssh://" {
		return strings.Contains(userinfo, ":")
	}
	return true
}

// SanitizeRepoURL removes embedded credentials from a repo URL: all userinfo
// on http(s), the password (keeping the user name) on ssh://. Other URLs are
// returned unchanged. changed reports whether anything was removed. Used to
// scrub already-persisted AppSources and every API response.
func SanitizeRepoURL(u string) (clean string, changed bool) {
	if !repoURLCarriesCredentials(u) {
		return u, false
	}
	scheme, userinfo, rest, _ := splitRepoURLUserinfo(u)
	if scheme == "ssh://" {
		user, _, _ := strings.Cut(userinfo, ":")
		if user == "" {
			return scheme + rest, true
		}
		return scheme + user + "@" + rest, true
	}
	return scheme + rest, true
}

// urlUserinfoRe matches the userinfo portion of a URL embedded in a larger
// string: scheme://[user[:pass]@]host.
var urlUserinfoRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)([^/@\s]+)@`)

// RedactURLCredentials replaces the userinfo of every URL substring in s with
// "***" (or "***:***" when a password is present), preserving the rest of the
// text for debugging. Use it on free-form strings (errors, messages).
func RedactURLCredentials(s string) string {
	return urlUserinfoRe.ReplaceAllStringFunc(s, func(match string) string {
		sub := urlUserinfoRe.FindStringSubmatch(match)
		if sub == nil {
			return match
		}
		if strings.Contains(sub[2], ":") {
			return sub[1] + "***:***@"
		}
		return sub[1] + "***@"
	})
}
