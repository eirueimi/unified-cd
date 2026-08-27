package dsl

import (
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestSpecDisplayNameUnmarshalsFromYAML(t *testing.T) {
	yamlSrc := `
displayName: "deploy {{ .Params.env }} @ {{ .Params.ref }}"
steps:
  - name: build
    run: echo hi
`
	var spec Spec
	require.NoError(t, yaml.Unmarshal([]byte(yamlSrc), &spec))
	require.Equal(t, `deploy {{ .Params.env }} @ {{ .Params.ref }}`, spec.DisplayName)
}

func TestSpecDisplayNameOmittedByDefault(t *testing.T) {
	yamlSrc := `
steps:
  - name: build
    run: echo hi
`
	var spec Spec
	require.NoError(t, yaml.Unmarshal([]byte(yamlSrc), &spec))
	require.Empty(t, spec.DisplayName)
}
