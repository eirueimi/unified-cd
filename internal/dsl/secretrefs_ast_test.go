package dsl

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSecretReferencePolicyAllowsOnlyStaticForms(t *testing.T) {
	tests := []struct {
		name      string
		tpl       string
		params    map[string]string
		wantNames []string
	}{
		{
			name:      "canonical literal",
			tpl:       `{{ index .Secrets "gitlab-token" }}`,
			wantNames: []string{"gitlab-token"},
		},
		{
			name:      "parameter resolves before validation",
			tpl:       `{{ index .Secrets .Params.token_secret }}`,
			params:    map[string]string{"token_secret": "gitlab-token"},
			wantNames: []string{"gitlab-token"},
		},
		{
			name:   "empty optional parameter",
			tpl:    `{{ if .Params.token_secret }}{{ index .Secrets .Params.token_secret }}{{ end }}`,
			params: map[string]string{"token_secret": ""},
		},
		{
			name:      "direct underscore name",
			tpl:       `{{ .Secrets.API_TOKEN }}`,
			wantNames: []string{"API_TOKEN"},
		},
		{
			name:      "normalized hyphen name",
			tpl:       `{{ .Secrets.unity-license }}`,
			wantNames: []string{"unity-license"},
		},
		{
			name:      "normalized no-dot form",
			tpl:       `{{ secrets.API_TOKEN }}`,
			wantNames: []string{"API_TOKEN"},
		},
		{
			name:      "static secret value nested in a non-secret function",
			tpl:       `{{ printf "%s" (index .Secrets "gitlab-token") }}`,
			wantNames: []string{"gitlab-token"},
		},
		{
			name: "ordinary non-secret index",
			tpl:  `{{ index .Params.values 0 }}`,
		},
		{
			name: "secret-looking text in string and comment",
			tpl:  `{{ printf ".Secrets" }}{{/* index .Secrets .Steps.pick.Outputs.name */}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolved, err := ResolveSecretNameParams(tt.tpl, tt.params)
			require.NoError(t, err)

			names, err := ReferencedSecretNames(resolved)
			require.NoError(t, err)
			assert.ElementsMatch(t, tt.wantNames, names)
		})
	}
}

func TestSecretReferencePolicyRejectsNonCanonicalSecretsNamespaceUses(t *testing.T) {
	tests := []struct {
		name string
		tpl  string
	}{
		{
			name: "runtime key",
			tpl:  `{{ index .Secrets .Steps.pick.Outputs.name }}`,
		},
		{
			name: "parenthesized receiver",
			tpl:  `{{ index (.Secrets) "gitlab-token" }}`,
		},
		{
			name: "direct alias",
			tpl:  `{{ $secretMap := .Secrets }}{{ index $secretMap "gitlab-token" }}`,
		},
		{
			name: "or alias",
			tpl:  `{{ $secretMap := or .Secrets .Secrets }}{{ index $secretMap .Steps.pick.Outputs.name }}`,
		},
		{
			name: "and argument",
			tpl:  `{{ and .Params.enabled .Secrets }}`,
		},
		{
			name: "function argument",
			tpl:  `{{ printf "%v" .Secrets }}`,
		},
		{
			name: "with dot",
			tpl:  `{{ with .Secrets }}{{ index . "gitlab-token" }}{{ end }}`,
		},
		{
			name: "range source",
			tpl:  `{{ range .Secrets }}{{ . }}{{ end }}`,
		},
		{
			name: "named template argument",
			tpl:  `{{ define "helper" }}{{ index . "gitlab-token" }}{{ end }}{{ template "helper" .Secrets }}`,
		},
		{
			name: "root alias selection",
			tpl:  `{{ $root := . }}{{ index $root.Secrets "gitlab-token" }}`,
		},
		{
			name: "nested reserved selection",
			tpl:  `{{ index .Payload.Secrets "gitlab-token" }}`,
		},
		{
			name: "long direct field chain",
			tpl:  `{{ .Secrets.API_TOKEN.Value }}`,
		},
		{
			name: "computed key",
			tpl:  `{{ index .Secrets (printf "%s-token" .Params.environment) }}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, resolveErr := ResolveSecretNameParams(tt.tpl, nil)
			require.ErrorIs(t, resolveErr, errDynamicSecretName)

			_, extractErr := ReferencedSecretNames(tt.tpl)
			require.ErrorIs(t, extractErr, errDynamicSecretName)
		})
	}
}

func TestSecretReferencePolicyRejectsUnparseableTemplate(t *testing.T) {
	tpl := `{{ index .Secrets "gitlab-token" `

	_, resolveErr := ResolveSecretNameParams(tpl, nil)
	require.ErrorContains(t, resolveErr, "parse secret references")

	_, extractErr := ReferencedSecretNames(tpl)
	require.ErrorContains(t, extractErr, "parse secret references")
}

func TestSecretReferencePolicyAllowsNoArgumentNamedTemplate(t *testing.T) {
	tpl := `{{ define "helper" }}ok{{ end }}{{ template "helper" }}`

	t.Run("parameter resolution", func(t *testing.T) {
		var resolved string
		var err error
		require.NotPanics(t, func() {
			resolved, err = ResolveSecretNameParams(tpl, nil)
		})
		require.NoError(t, err)
		assert.Equal(t, tpl, resolved)
	})

	t.Run("name extraction", func(t *testing.T) {
		var names []string
		var err error
		require.NotPanics(t, func() {
			names, err = ReferencedSecretNames(tpl)
		})
		require.NoError(t, err)
		assert.Empty(t, names)
	})
}

func TestReferencedSecretNamesCollectsOnlyTemplateReferences(t *testing.T) {
	tests := []struct {
		name      string
		tpl       string
		wantNames []string
	}{
		{
			name: "secret reference text in template string",
			tpl:  `{{ printf ".Secrets.API_TOKEN" }}`,
		},
		{
			name: "secret reference text outside template action",
			tpl:  `echo .Secrets.API_TOKEN`,
		},
		{
			name:      "direct dot reference",
			tpl:       `{{ .Secrets.API_TOKEN }}`,
			wantNames: []string{"API_TOKEN"},
		},
		{
			name:      "normalized no-dot reference",
			tpl:       `{{ secrets.API_TOKEN }}`,
			wantNames: []string{"API_TOKEN"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			names, err := ReferencedSecretNames(tt.tpl)
			require.NoError(t, err)
			assert.ElementsMatch(t, tt.wantNames, names)
		})
	}
}
