package dsl

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// volumesJob wraps a spec fragment (the lines under `spec:`, already indented
// by two spaces) in a minimal valid Job so the whole parse+validate path runs.
func volumesJob(specFragment string) string {
	return `
apiVersion: unified-cd/v1
kind: Job
metadata: { name: vol }
spec:
` + specFragment + `
  steps:
    - name: s
      run: echo hi
`
}

func TestValidateVolumes(t *testing.T) {
	accepted := []struct {
		name string
		spec string
	}{
		{"single literal name, relative mountPath", `
  volumes:
    - name: go-cache
      mountPath: .ucd-cache/go`},
		{"candidate list with as", `
  volumes:
    - name: [go-cache-1, go-cache-2]
      as: go-cache
      mountPath: .ucd-cache/go`},
		{"templated name with as", `
  volumes:
    - name: "cache-{{ .Params.os }}"
      as: cache
      mountPath: cache`},
		{"one-element list is a single literal", `
  volumes:
    - name: [go-cache]
      mountPath: cache`},
		{"mountPath dot alone", `
  volumes:
    - name: unity-main
      mountPath: "."`},
		{"mountPath dot plus a second entry", `
  volumes:
    - name: unity-main
      mountPath: "."
    - name: go-cache
      mountPath: cache`},
		{"absolute mountPath outside the workspace", `
  volumes:
    - name: go-mod
      mountPath: /go/pkg/mod`},
		{"sibling mountPaths sharing a name prefix are not nested", `
  volumes:
    - name: a
      mountPath: cache
    - name: b
      mountPath: cache2`},
		{"absolute mountPath beside a custom workspace root", `
  podTemplate:
    workspace:
      mountPath: /src
  volumes:
    - name: go-mod
      mountPath: /workspace/cache`},
	}
	for _, tc := range accepted {
		t.Run("accepts/"+tc.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(volumesJob(tc.spec)))
			require.NoError(t, err)
		})
	}

	rejected := []struct {
		name string
		spec string
		want string
	}{
		{"candidate list without as", `
  volumes:
    - name: [a, b]
      mountPath: cache`, "requires `as`"},
		{"templated name without as", `
  volumes:
    - name: "cache-{{ .Params.os }}"
      mountPath: cache`, "requires `as`"},
		{"empty name", `
  volumes:
    - name: ""
      mountPath: cache`, "name is required"},
		{"empty mountPath", `
  volumes:
    - name: a`, "mountPath is required"},
		{"mountPath dot-dot", `
  volumes:
    - name: a
      mountPath: ..`, "escapes the workspace"},
		{"mountPath that climbs out", `
  volumes:
    - name: a
      mountPath: a/../..`, "escapes the workspace"},
		{"absolute mountPath equal to the workspace", `
  volumes:
    - name: a
      mountPath: /workspace`, "is inside the workspace"},
		{"absolute mountPath inside the workspace", `
  volumes:
    - name: a
      mountPath: /workspace/cache`, "is inside the workspace"},
		{"absolute mountPath that cleans into the workspace", `
  volumes:
    - name: a
      mountPath: /tmp/../workspace/cache`, "is inside the workspace"},
		{"absolute mountPath inside a custom workspace root", `
  podTemplate:
    workspace:
      mountPath: /src
  volumes:
    - name: a
      mountPath: /src/cache`, "is inside the workspace"},
		{"duplicate mountPath", `
  volumes:
    - name: a
      mountPath: cache
    - name: b
      mountPath: cache`, "duplicate mountPath"},
		{"duplicate mountPath after cleaning", `
  volumes:
    - name: a
      mountPath: cache
    - name: b
      mountPath: ./cache/`, "duplicate mountPath"},
		{"nested relative mountPaths", `
  volumes:
    - name: a
      mountPath: cache
    - name: b
      mountPath: cache/go`, "nested mountPath"},
		{"nested absolute mountPaths", `
  volumes:
    - name: a
      mountPath: /go
    - name: b
      mountPath: /go/pkg/mod`, "nested mountPath"},
		{"volumes on a detached job", `
  detached: true
  volumes:
    - name: a
      mountPath: cache`, "not supported on a detached job"},
		{"volumes with podTemplate.reuse", `
  podTemplate:
    reuse: true
  volumes:
    - name: a
      mountPath: cache`, "not supported with podTemplate.reuse"},
	}
	for _, tc := range rejected {
		t.Run("rejects/"+tc.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(volumesJob(tc.spec)))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// A job that declares no volumes must be unaffected by the new rules, even on
// a detached job or with podTemplate.reuse.
func TestValidateVolumes_NoVolumesNoRestrictions(t *testing.T) {
	for _, frag := range []string{
		"  detached: true",
		"  podTemplate:\n    reuse: true",
	} {
		_, err := Parse(strings.NewReader(volumesJob(frag)))
		require.NoError(t, err, frag)
	}
}

func TestNormalizeVolumes_WorkspaceVolumeSugar(t *testing.T) {
	t.Run("workspace.volume becomes a dot entry", func(t *testing.T) {
		j, err := Parse(strings.NewReader(volumesJob(`
  podTemplate:
    workspace:
      volume: unity-main`)))
		require.NoError(t, err)

		got, err := NormalizeVolumes(j.Spec)
		require.NoError(t, err)
		assert.Equal(t, []VolumeMount{
			{Name: StringOrSlice{"unity-main"}, MountPath: "."},
		}, got)
	})

	t.Run("sugar is listed before explicit entries", func(t *testing.T) {
		j, err := Parse(strings.NewReader(volumesJob(`
  podTemplate:
    workspace:
      volume: unity-main
  volumes:
    - name: go-cache
      mountPath: cache`)))
		require.NoError(t, err)

		got, err := NormalizeVolumes(j.Spec)
		require.NoError(t, err)
		require.Len(t, got, 2)
		assert.Equal(t, ".", got[0].MountPath)
		assert.Equal(t, "cache", got[1].MountPath)
	})

	t.Run("no volumes yields none", func(t *testing.T) {
		j, err := Parse(strings.NewReader(volumesJob("")))
		require.NoError(t, err)
		got, err := NormalizeVolumes(j.Spec)
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("workspace.volume together with an explicit dot entry is an error", func(t *testing.T) {
		for _, dot := range []string{`"."`, `"./"`} {
			_, err := Parse(strings.NewReader(volumesJob(`
  podTemplate:
    workspace:
      volume: unity-main
  volumes:
    - name: other
      mountPath: ` + dot)))
			require.Error(t, err, dot)
			assert.Contains(t, err.Error(), "workspace.volume")
		}
	})

	t.Run("workspace.volume as a candidate list has no place for an alias", func(t *testing.T) {
		_, err := Parse(strings.NewReader(volumesJob(`
  podTemplate:
    workspace:
      volume: [a, b]`)))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "requires `as`")
		assert.Contains(t, err.Error(), "workspace.volume")
	})
}

func TestVolumeAlias(t *testing.T) {
	assert.Equal(t, "go-cache", VolumeAlias(VolumeMount{Name: StringOrSlice{"go-cache"}}))
	assert.Equal(t, "go-cache", VolumeAlias(VolumeMount{Name: StringOrSlice{"a", "b"}, As: "go-cache"}))
	// An explicit alias wins even when the name is a single literal.
	assert.Equal(t, "gc", VolumeAlias(VolumeMount{Name: StringOrSlice{"go-cache"}, As: "gc"}))
	// No alias and no single literal name: there is nothing to return.
	assert.Equal(t, "", VolumeAlias(VolumeMount{Name: StringOrSlice{"a", "b"}}))
	assert.Equal(t, "", VolumeAlias(VolumeMount{Name: StringOrSlice{"cache-{{ .Params.os }}"}}))
}

func TestStringOrSlice_YAMLAndJSON(t *testing.T) {
	type holder struct {
		Name StringOrSlice `yaml:"name" json:"name"`
	}
	for _, tc := range []struct {
		in   string
		want StringOrSlice
	}{
		{"name: foo", StringOrSlice{"foo"}},
		{"name: [a, b]", StringOrSlice{"a", "b"}},
		{"name:\n  - a\n  - b", StringOrSlice{"a", "b"}},
	} {
		var h holder
		require.NoError(t, yaml.Unmarshal([]byte(tc.in), &h), tc.in)
		assert.Equal(t, tc.want, h.Name, tc.in)
	}

	var bad holder
	err := yaml.Unmarshal([]byte("name: {a: b}"), &bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be a string or a list of strings")

	// The store persists Spec as JSON, so the value must survive a JSON
	// round-trip, and a YAML marshal must re-read as the same value.
	in := holder{Name: StringOrSlice{"a", "b"}}
	b, err := json.Marshal(in)
	require.NoError(t, err)
	var fromJSON holder
	require.NoError(t, json.Unmarshal(b, &fromJSON))
	assert.Equal(t, in, fromJSON)

	for _, v := range []StringOrSlice{{"solo"}, {"a", "b"}} {
		out, err := yaml.Marshal(holder{Name: v})
		require.NoError(t, err)
		var back holder
		require.NoError(t, yaml.Unmarshal(out, &back))
		assert.Equal(t, v, back.Name)
	}
}
