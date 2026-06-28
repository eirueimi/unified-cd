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
