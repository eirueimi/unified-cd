package controller

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExpandRunDisplayName_Empty(t *testing.T) {
	got, err := expandRunDisplayName("", map[string]string{"env": "prod"})
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestExpandRunDisplayName_InterpolatesParams(t *testing.T) {
	got, err := expandRunDisplayName("deploy {{ .Params.env }} @ {{ .Params.ref }}", map[string]string{"env": "prod", "ref": "abc123"})
	require.NoError(t, err)
	require.Equal(t, "deploy prod @ abc123", got)
}

func TestExpandRunDisplayName_UndeclaredParamExpandsToEmpty(t *testing.T) {
	got, err := expandRunDisplayName("deploy {{ .Params.typo }}", map[string]string{"env": "prod"})
	require.NoError(t, err)
	require.Equal(t, "deploy ", got)
}

func TestExpandRunDisplayName_MalformedTemplateErrors(t *testing.T) {
	_, err := expandRunDisplayName("deploy {{ .Params.env", map[string]string{"env": "prod"})
	require.Error(t, err)
}

func TestExpandRunDisplayName_SanitizesNUL(t *testing.T) {
	got, err := expandRunDisplayName("deploy {{ .Params.env }}", map[string]string{"env": "prod\x00staging"})
	require.NoError(t, err)
	require.NotContains(t, got, "\x00")
	require.Contains(t, got, "�")
}

func TestExpandRunDisplayName_TruncatesAtCap(t *testing.T) {
	long := strings.Repeat("x", maxDisplayNameLength+50)
	got, err := expandRunDisplayName("{{ .Params.long }}", map[string]string{"long": long})
	require.NoError(t, err)
	require.Equal(t, maxDisplayNameLength+1, len([]rune(got))) // cap runes + trailing "…"
	require.True(t, strings.HasSuffix(got, "…"))
}

func TestExpandRunDisplayName_ShortStringNotTruncated(t *testing.T) {
	got, err := expandRunDisplayName("{{ .Params.short }}", map[string]string{"short": "deploy prod"})
	require.NoError(t, err)
	require.Equal(t, "deploy prod", got)
	require.False(t, strings.HasSuffix(got, "…"))
}
