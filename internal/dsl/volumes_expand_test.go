package dsl

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExpandVolumes(t *testing.T) {
	t.Run("expands a param in a single name", func(t *testing.T) {
		in := []VolumeMount{{Name: StringOrSlice{"{{ .Params.LIB }}"}, As: "lib", MountPath: "Library"}}
		got, err := ExpandVolumes(in, map[string]string{"LIB": "unity-a"})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, StringOrSlice{"unity-a"}, got[0].Name)
		assert.Equal(t, "lib", got[0].As)
		assert.Equal(t, "Library", got[0].MountPath)
	})

	t.Run("expands a list element-wise", func(t *testing.T) {
		in := []VolumeMount{{Name: StringOrSlice{"cache-{{ .Params.A }}", "fixed", "cache-{{ .Params.B }}"}, As: "c", MountPath: "c"}}
		got, err := ExpandVolumes(in, map[string]string{"A": "x", "B": "y"})
		require.NoError(t, err)
		assert.Equal(t, StringOrSlice{"cache-x", "fixed", "cache-y"}, got[0].Name)
	})

	t.Run("does not mutate the input", func(t *testing.T) {
		in := []VolumeMount{{Name: StringOrSlice{"{{ .Params.LIB }}"}, As: "lib", MountPath: "Library"}}
		_, err := ExpandVolumes(in, map[string]string{"LIB": "unity-a"})
		require.NoError(t, err)
		assert.Equal(t, StringOrSlice{"{{ .Params.LIB }}"}, in[0].Name)
	})

	t.Run("no volumes returns no volumes", func(t *testing.T) {
		got, err := ExpandVolumes(nil, nil)
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("malformed template names the entry and candidate", func(t *testing.T) {
		in := []VolumeMount{
			{Name: StringOrSlice{"ok"}, MountPath: "a"},
			{Name: StringOrSlice{"fine", "{{ .Params.X"}, As: "b", MountPath: "b"},
		}
		_, err := ExpandVolumes(in, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "volumes[1].name[1]")
	})

	t.Run("malformed template in a single-name entry names volumes[0].name", func(t *testing.T) {
		in := []VolumeMount{{Name: StringOrSlice{"{{ .Params.X"}, As: "x", MountPath: "x"}}
		_, err := ExpandVolumes(in, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "volumes[0].name")
	})
}

func TestRequiredVolumes(t *testing.T) {
	t.Run("single name", func(t *testing.T) {
		spec := Spec{Volumes: []VolumeMount{{Name: StringOrSlice{"unity-main"}, MountPath: "Library"}}}
		assert.Equal(t, [][]string{{"unity-main"}}, RequiredVolumes(spec))
	})

	t.Run("three candidates form one set", func(t *testing.T) {
		spec := Spec{Volumes: []VolumeMount{{
			Name: StringOrSlice{"go-cache-1", "go-cache-2", "go-cache-3"}, As: "gocache", MountPath: "/cache",
		}}}
		assert.Equal(t, [][]string{{"go-cache-1", "go-cache-2", "go-cache-3"}}, RequiredVolumes(spec))
	})

	t.Run("one set per entry in declaration order", func(t *testing.T) {
		spec := Spec{Volumes: []VolumeMount{
			{Name: StringOrSlice{"b"}, MountPath: "b"},
			{Name: StringOrSlice{"a1", "a2"}, As: "a", MountPath: "a"},
		}}
		assert.Equal(t, [][]string{{"b"}, {"a1", "a2"}}, RequiredVolumes(spec))
	})

	t.Run("no volumes is empty", func(t *testing.T) {
		assert.Empty(t, RequiredVolumes(Spec{}))
	})

	t.Run("result does not alias the spec", func(t *testing.T) {
		spec := Spec{Volumes: []VolumeMount{{Name: StringOrSlice{"a", "b"}, As: "x", MountPath: "x"}}}
		got := RequiredVolumes(spec)
		got[0][0] = "changed"
		assert.Equal(t, StringOrSlice{"a", "b"}, spec.Volumes[0].Name)
	})
}
