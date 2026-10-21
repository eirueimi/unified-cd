package dsl

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVolumeEnvName(t *testing.T) {
	assert.Equal(t, "UNIFIED_VOLUME_GO_CACHE", VolumeEnvName("go-cache"))
	assert.Equal(t, "UNIFIED_VOLUME_GO_CACHE", VolumeEnvName("go_cache"))
	assert.Equal(t, "UNIFIED_VOLUME_GO_CACHE", VolumeEnvName("Go-Cache"))
	assert.Equal(t, "UNIFIED_VOLUME_X1", VolumeEnvName("x1"))
}

// Every name VolumeEnvName generates must be refused as a variable, whatever
// the alias: that is the property the runtime relies on to keep a Vars
// manifest from shadowing a volume's path.
func TestReservedVarNames_CoversVolumePrefix(t *testing.T) {
	for _, alias := range []string{"go-cache", "go_cache", "a", "WORKSPACE", "agent-os"} {
		name := VolumeEnvName(alias)
		assert.True(t, IsReservedVarName(name), "%s is generated for a volume and must be reserved", name)
		assert.True(t, IsReservedVarName(strings.ToLower(name)), "%s must be refused case-insensitively", strings.ToLower(name))
		assert.Error(t, ValidateVarKeys(map[string]string{name: "x"}), "%s must be refused at apply time", name)
		assert.Error(t, ValidateVarKeys(map[string]string{strings.ToLower(name): "x"}))
	}

	// The prefix is the rule, not the names in use: a volume nobody has
	// declared yet is reserved too.
	assert.Error(t, ValidateVarKeys(map[string]string{"UNIFIED_VOLUME_ANYTHING": "x"}))
	assert.Error(t, ValidateVarKeys(map[string]string{"unified_volume_anything": "x"}))

	// The prefix match must not swallow neighbours that merely resemble it.
	assert.NoError(t, ValidateVarKeys(map[string]string{"UNIFIED_VOLUMES": "x", "UNIFIED_VOLUME": "x", "MY_UNIFIED_VOLUME_X": "x"}))

	// The prefixes are the single source: they are reported, and
	// VolumeEnvName builds from the same constant.
	require.NotEmpty(t, ReservedVarPrefixes())
	assert.Contains(t, ReservedVarPrefixes(), VolumeEnvPrefix)
	assert.True(t, strings.HasPrefix(VolumeEnvName("x"), VolumeEnvPrefix))
}

// ReservedVarNames keeps its shape: exact names only. A prefix entry in that
// map would be a name no variable could ever have, and would be exact-matched
// by callers that do not know it is a prefix.
func TestReservedVarNames_HoldsExactNamesOnly(t *testing.T) {
	for name := range ReservedVarNames() {
		assert.False(t, strings.HasSuffix(name, "_"), "%s looks like a prefix; prefixes belong in ReservedVarPrefixes", name)
	}
}

func TestValidateVolumes_RejectsCollidingAliases(t *testing.T) {
	for name, spec := range map[string]string{
		"hyphen and underscore": `
  volumes:
    - name: go-cache
      mountPath: a
    - name: go_cache
      mountPath: b`,
		"case only": `
  volumes:
    - name: [x-1, x-2]
      as: Go-Cache
      mountPath: a
    - name: [y-1, y-2]
      as: go-cache
      mountPath: b`,
		"same alias twice": `
  volumes:
    - name: [x-1, x-2]
      as: c
      mountPath: a
    - name: [y-1, y-2]
      as: c
      mountPath: b`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(volumesJob(spec)))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "UNIFIED_VOLUME_")
			assert.Contains(t, err.Error(), "spec.volumes[1]")
			assert.Contains(t, err.Error(), "spec.volumes[0]")
		})
	}
}

func TestValidateVolumes_AcceptsDistinctAliases(t *testing.T) {
	_, err := Parse(strings.NewReader(volumesJob(`
  volumes:
    - name: go-cache
      mountPath: a
    - name: [npm-1, npm-2]
      as: npm
      mountPath: b`)))
	require.NoError(t, err)
}

// An alias becomes part of an environment variable name, so one that cannot
// be written in a shell is refused rather than silently exposed under a name
// no step can read.
func TestValidateVolumes_RejectsUnusableAlias(t *testing.T) {
	for _, alias := range []string{"go.cache", "go cache", "go/cache", "go$cache", "caché"} {
		t.Run(alias, func(t *testing.T) {
			_, err := Parse(strings.NewReader(volumesJob(`
  volumes:
    - name: [a-1, a-2]
      as: "` + alias + `"
      mountPath: a`)))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "spec.volumes[0]")
			assert.Contains(t, err.Error(), "`as`")
		})
	}
}

// With the UNIFIED_VOLUME_ prefix itself reserved, no alias can land on
// UNIFIED_WORKSPACE or UNIFIED_AGENT_OS, so the exact-name guard cannot be
// reached through a real alias today. It is shown to fire by reserving an
// exact name an alias does generate; if it were removed, this fails.
func TestValidateVolumes_RejectsReservedAlias(t *testing.T) {
	reservedVarNames["UNIFIED_VOLUME_FORBIDDEN"] = true
	t.Cleanup(func() { delete(reservedVarNames, "UNIFIED_VOLUME_FORBIDDEN") })

	err := ValidateVolumes(Spec{Volumes: []VolumeMount{
		{Name: StringOrSlice{"forbidden"}, MountPath: "a"},
	}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.volumes[0]")
	assert.Contains(t, err.Error(), "UNIFIED_VOLUME_FORBIDDEN")
	assert.Contains(t, err.Error(), "reserved")
}
