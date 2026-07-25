package gittemplate_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/gittemplate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const secretRefTemplate = `
apiVersion: unified-cd/v1
kind: JobTemplate
metadata:
  name: checkout
spec:
  params:
    inputs:
      - name: token_secret
        type: string
        default: ""
  steps:
    - name: checkout
      env:
        GIT_TOKEN: "{{ if .Params.token_secret }}{{ index .Secrets .Params.token_secret }}{{ end }}"
      run: "true"
`

func TestResolveSpecResolvesCheckoutSecretReference(t *testing.T) {
	caller := dsl.Spec{Steps: []dsl.StepEntry{{
		Name: "checkout",
		Uses: &dsl.UsesStep{
			Job:  "git://github.com/org/repo/checkout.yaml@v1",
			With: map[string]any{"token_secret": "gitlab-token"},
		},
	}}}

	resolver := gittemplate.NewResolver(&stubFetcher{data: []byte(secretRefTemplate)}, nil)
	resolved, err := resolver.ResolveSpec(context.Background(), mustMarshalSpec(caller), noCred)
	require.NoError(t, err)

	var expanded dsl.Spec
	require.NoError(t, json.Unmarshal(resolved, &expanded))
	require.Len(t, expanded.Steps, 3)
	require.Equal(t, "checkout__checkout", expanded.Steps[1].Name)
	assert.Contains(t, expanded.Steps[1].Env["GIT_TOKEN"], `index .Secrets "gitlab-token"`)
	assert.NotContains(t, expanded.Steps[1].Env["GIT_TOKEN"], "index .Secrets .Steps")
}
