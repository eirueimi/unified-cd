package dsl

import "testing"

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
