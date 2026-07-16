package gittemplate_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/eirueimi/unified-cd/internal/gittemplate"
)

func TestParseURI(t *testing.T) {
	cases := []struct {
		raw       string
		wantHost  string
		wantOwner string
		wantRepo  string
		wantPath  string
		wantRef   string
		wantErr   bool
	}{
		{
			raw:       "git://github.com/org/repo/jobs/build.yaml@v1.2.3",
			wantHost:  "github.com",
			wantOwner: "org",
			wantRepo:  "repo",
			wantPath:  "jobs/build.yaml",
			wantRef:   "v1.2.3",
		},
		{
			raw:       "git://github.com/org/repo/build.yaml@main",
			wantHost:  "github.com",
			wantOwner: "org",
			wantRepo:  "repo",
			wantPath:  "build.yaml",
			wantRef:   "main",
		},
		{
			raw:       "git://github.com/org/repo/build.yaml@" + "abc1234567890123456789012345678901234567890",
			wantHost:  "github.com",
			wantOwner: "org",
			wantRepo:  "repo",
			wantPath:  "build.yaml",
			wantRef:   "abc1234567890123456789012345678901234567890",
		},
		{raw: "https://github.com/org/repo/build.yaml@v1", wantErr: true},
		{raw: "git://github.com/repo/only@v1", wantErr: true},
		{raw: "git://github.com/org/repo/build.yaml", wantErr: true},
		{raw: "git://github.com/org/repo/../../../etc/passwd@v1", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			u, err := gittemplate.ParseURI(tc.raw)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantHost, u.Host)
			assert.Equal(t, tc.wantOwner, u.Owner)
			assert.Equal(t, tc.wantRepo, u.Repo)
			assert.Equal(t, tc.wantPath, u.Path)
			assert.Equal(t, tc.wantRef, u.Ref)
		})
	}
}

func TestParseURI_RefAllowlist(t *testing.T) {
	ok := []string{
		"git://h/o/r/p@main",
		"git://h/o/r/p@v1.2.3",
		"git://h/o/r/p@feature/x",
		"git://h/o/r/p@a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0",
	}
	for _, u := range ok {
		_, err := gittemplate.ParseURI(u)
		require.NoError(t, err, u)
	}
	bad := []string{
		"git://h/o/r/p@-x",
		"git://h/o/r/p@--upload-pack=y",
		"git://h/o/r/p@ref with space",
		"git://h/o/r/p@ref;rm -rf",
	}
	for _, u := range bad {
		_, err := gittemplate.ParseURI(u)
		require.Error(t, err, u)
	}
}

func TestURI_IsFixed(t *testing.T) {
	cases := []struct {
		ref       string
		wantFixed bool
	}{
		{"v1.2.3", true},
		{"v0.0.1-beta", true},
		{"abcdef1234567890abcdef1234567890abcdef12", true}, // 40-hex SHA
		{"main", false},
		{"feature-branch", false},
		{"HEAD", false},
		{"v1", true},
		{"abc123", false}, // not 40 chars
	}
	for _, tc := range cases {
		t.Run(tc.ref, func(t *testing.T) {
			u := gittemplate.URI{Ref: tc.ref}
			assert.Equal(t, tc.wantFixed, u.IsFixed(), "ref=%q", tc.ref)
		})
	}
}

func TestParseURI_RejectsDashLeadingHost(t *testing.T) {
	for _, raw := range []string{
		"git://-oProxyCommand=x/org/repo/f.yaml@main",
		"git://-x/org/repo/f.yaml@main",
	} {
		if _, err := gittemplate.ParseURI(raw); err == nil {
			t.Errorf("%q: dash-leading host must be rejected", raw)
		}
	}
	if _, err := gittemplate.ParseURI("git://github.com/org/repo/jobs/build.yaml@v1"); err != nil {
		t.Errorf("valid host must pass: %v", err)
	}
}
