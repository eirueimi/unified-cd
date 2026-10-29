package authz

import (
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	errWS       = "invalid folder: whitespace is not allowed"
	errGlob     = `invalid folder: folders are literal job-name prefixes, "*", "?", "[" are not allowed`
	errSegments = `invalid folder: segments must be non-empty and must not be "." or ".."`
)

func TestNormalizeFolder(t *testing.T) {
	ok := []struct{ in, want string }{
		{"/team-a/", "team-a"},
		{"team-a/sub", "team-a/sub"},
		{"Team_A", "Team_A"},
	}
	for _, c := range ok {
		got, err := NormalizeFolder(c.in)
		require.NoError(t, err, c.in)
		require.Equal(t, c.want, got, c.in)
	}
	bad := []struct{ in, err string }{
		{"team-a//x", errSegments},
		{"a/../b", errSegments},
		{".", errSegments},
		{"/", errSegments},
		{"", errSegments},
		{"team-*", errGlob},
		{"t?", errGlob},
		{"t[1]", errGlob},
		{"team a", errWS},
		{"team\ta", errWS},
	}
	for _, c := range bad {
		_, err := NormalizeFolder(c.in)
		require.EqualError(t, err, c.err, c.in)
	}
}

func TestCovers(t *testing.T) {
	require.True(t, Covers("team-a", "team-a/build"))
	require.True(t, Covers("team-a", "team-a/sub/build"))
	require.False(t, Covers("team-a", "team-ab/build"))
	require.False(t, Covers("team-a", "team-a"))
	require.False(t, Covers("team-a", "Team-A/x"))
}

func TestAncestors(t *testing.T) {
	require.Equal(t, []string{"a", "a/b"}, Ancestors("a/b/c"))
	require.Nil(t, Ancestors("x"))
	require.Nil(t, Ancestors(""))
}
