package dsl

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseVars_Valid(t *testing.T) {
	v, err := ParseVars(strings.NewReader(`
apiVersion: unified-cd/v1
kind: Vars
metadata:
  name: org-defaults
spec:
  vars:
    REGISTRY: ghcr.io/myorg
    GO_VERSION: "1.24"
`))
	require.NoError(t, err)
	assert.Equal(t, "org-defaults", v.Metadata.Name)
	assert.Equal(t, "ghcr.io/myorg", v.Spec.Vars["REGISTRY"])
	assert.Equal(t, "1.24", v.Spec.Vars["GO_VERSION"])
}

func TestValidateVarKeys(t *testing.T) {
	tests := []struct {
		name    string
		vars    map[string]string
		wantErr string
	}{
		{"plain", map[string]string{"REGISTRY": "x"}, ""},
		{"leading underscore", map[string]string{"_X": "x"}, ""},
		{"digits after first", map[string]string{"A1_B2": "x"}, ""},
		{"empty map", map[string]string{}, ""},
		{"leading digit", map[string]string{"1BAD": "x"}, "1BAD"},
		{"hyphen", map[string]string{"BAD-KEY": "x"}, "BAD-KEY"},
		{"dot", map[string]string{"BAD.KEY": "x"}, "BAD.KEY"},
		{"space", map[string]string{"BAD KEY": "x"}, "BAD KEY"},
		{"empty key", map[string]string{"": "x"}, "empty"},
		{"reserved token", map[string]string{"UNIFIED_TOKEN": "x"}, "UNIFIED_TOKEN"},
		{"reserved cache secret", map[string]string{"UNIFIED_CACHE_SECRET": "x"}, "UNIFIED_CACHE_SECRET"},
		{"reserved PATH", map[string]string{"PATH": "/x"}, "PATH"},
		{"reserved HOME", map[string]string{"HOME": "/x"}, "HOME"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateVarKeys(tc.vars)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr,
				"the error must name the offending key so the author can find it")
		})
	}
}

// A reserved name is refused whatever its case, because environment variable
// lookup is case-sensitive on Linux but not on Windows, and an agent fleet can
// be mixed.
func TestValidateVarKeys_ReservedIsCaseInsensitive(t *testing.T) {
	require.Error(t, ValidateVarKeys(map[string]string{"Path": "/x"}))
	require.Error(t, ValidateVarKeys(map[string]string{"unified_token": "x"}))
}

func TestCheckVarsCollision(t *testing.T) {
	a := map[string]string{"REGISTRY": "one", "SHARED": "x"}
	b := map[string]string{"SHARED": "y", "OTHER": "z"}

	err := CheckVarsCollision(a, "org-defaults", b, "team-defaults")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SHARED")
	assert.Contains(t, err.Error(), "org-defaults", "the error must name both manifests")
	assert.Contains(t, err.Error(), "team-defaults", "the error must name both manifests")

	// Same key, same manifest name: this is a re-apply of one manifest, not a
	// collision between two.
	assert.NoError(t, CheckVarsCollision(a, "org-defaults", b, "org-defaults"))

	// Disjoint keys never collide.
	assert.NoError(t, CheckVarsCollision(
		map[string]string{"A": "1"}, "m1",
		map[string]string{"B": "2"}, "m2"))
}

func TestParseVars_RejectsBadKey(t *testing.T) {
	_, err := ParseVars(strings.NewReader(`
apiVersion: unified-cd/v1
kind: Vars
metadata:
  name: bad
spec:
  vars:
    "BAD-KEY": x
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "BAD-KEY")
}

func TestParseVars_RequiresName(t *testing.T) {
	_, err := ParseVars(strings.NewReader(`
apiVersion: unified-cd/v1
kind: Vars
spec:
  vars:
    A: b
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "name")
}

// Job spec.vars round-trips through JSON, not only YAML: the store persists the
// spec as JSON and reads it back, so a yaml-only tag would silently lose it.
func TestJobSpecVars_RoundTripsThroughJSON(t *testing.T) {
	j, err := Parse(strings.NewReader(`
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: j
spec:
  vars:
    APP_NAME: myapp
  steps:
    - name: s
      run: "echo hi"
`))
	require.NoError(t, err)
	require.Equal(t, "myapp", j.Spec.Vars["APP_NAME"])

	blob, err := json.Marshal(j.Spec)
	require.NoError(t, err)
	var back Spec
	require.NoError(t, json.Unmarshal(blob, &back))
	assert.Equal(t, "myapp", back.Vars["APP_NAME"],
		"spec.vars needs a json tag as well as a yaml tag")
}
