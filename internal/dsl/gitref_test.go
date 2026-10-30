package dsl

import (
	"strings"
	"testing"
)

func TestValidateGitRef(t *testing.T) {
	for _, ok := range []string{"main", "v1.2.3", "feature/x", "abc123DEF", "release-2026.07+build"} {
		if err := ValidateGitRef(ok); err != nil {
			t.Errorf("%q should be a valid ref: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-main", "--upload-pack=x", "HEAD~1", "main@{upstream}", "a b", "^caret"} {
		if err := ValidateGitRef(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestValidateGitRepoURL(t *testing.T) {
	for _, ok := range []string{
		"https://github.com/org/repo.git",
		"http://internal.example/repo.git",
		"ssh://git@host/org/repo.git",
		"git@github.com:org/repo.git",
	} {
		if err := ValidateGitRepoURL(ok); err != nil {
			t.Errorf("%q should be a valid repo URL: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"",
		"--upload-pack=touch /tmp/pwned",
		"-o=x",
		"ext::sh -c whoami",
		"file:///etc",
		"/local/path",
		"github.com/org/repo",       // no scheme
		"git@-x:org/repo.git",       // dash-leading host (CVE-2017-1000117 shape)
		"git@-oProxyCommand=x:repo", // ssh option smuggled as host
	} {
		if err := ValidateGitRepoURL(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestValidateGitRepoURL_Credentials(t *testing.T) {
	for _, bad := range []string{
		"https://user:pass@github.com/org/repo.git",
		"https://ghp_token@github.com/org/repo.git",
		"http://user:pass@host/repo.git",
		"https://:pass@host/repo.git",
		"ssh://git:secret@host/org/repo.git",
	} {
		err := ValidateGitRepoURL(bad)
		if err == nil {
			t.Errorf("%q should be rejected", bad)
			continue
		}
		for _, leak := range []string{"pass", "ghp_token", "secret"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("error for %q leaks the credential: %v", bad, err)
			}
		}
		if !strings.Contains(err.Error(), "GitCredential") {
			t.Errorf("error for %q should point to GitCredential: %v", bad, err)
		}
	}
	for _, ok := range []string{
		"ssh://git@host/org/repo.git",
		"git@github.com:org/repo.git",
		"https://github.com/org/repo@v1.git", // '@' in the path is not userinfo
		"https://host:8443/org/repo.git",
	} {
		if err := ValidateGitRepoURL(ok); err != nil {
			t.Errorf("%q should stay valid: %v", ok, err)
		}
	}
	// Other rejection messages must not echo credentials either.
	if err := ValidateGitRepoURL("ftp://u:topsecret@host/r"); err == nil || strings.Contains(err.Error(), "topsecret") {
		t.Errorf("unsupported-scheme error must redact userinfo: %v", err)
	}
}

func TestSanitizeRepoURL(t *testing.T) {
	cases := []struct {
		in, want string
		changed  bool
	}{
		{"https://user:pass@github.com/org/repo.git", "https://github.com/org/repo.git", true},
		{"https://tok@github.com/org/repo.git", "https://github.com/org/repo.git", true},
		{"http://a:b@host:8080/r?x=1", "http://host:8080/r?x=1", true},
		{"ssh://git:secret@host/org/repo.git", "ssh://git@host/org/repo.git", true},
		{"ssh://git@host/org/repo.git", "ssh://git@host/org/repo.git", false},
		{"git@github.com:org/repo.git", "git@github.com:org/repo.git", false},
		{"https://github.com/org/repo@v1", "https://github.com/org/repo@v1", false},
	}
	for _, c := range cases {
		got, changed := SanitizeRepoURL(c.in)
		if got != c.want || changed != c.changed {
			t.Errorf("SanitizeRepoURL(%q) = %q,%v want %q,%v", c.in, got, changed, c.want, c.changed)
		}
	}
}
