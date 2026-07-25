package dsl

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveSecretNameParams(t *testing.T) {
	tests := []struct {
		name    string
		tpl     string
		params  map[string]string
		want    string
		wantErr string
	}{
		{
			name: "literal remains literal",
			tpl:  `{{ index .Secrets "gitlab-token" }}`,
			want: `{{ index .Secrets "gitlab-token" }}`,
		},
		{
			name:   "parameter becomes literal",
			tpl:    `{{ index .Secrets .Params.token_secret }}`,
			params: map[string]string{"token_secret": "gitlab-token"},
			want:   `{{ index .Secrets "gitlab-token" }}`,
		},
		{
			name:   "empty optional parameter becomes empty literal",
			tpl:    `{{ if .Params.token_secret }}{{ index .Secrets .Params.token_secret }}{{ end }}`,
			params: map[string]string{"token_secret": ""},
			want:   `{{ if .Params.token_secret }}{{ index .Secrets "" }}{{ end }}`,
		},
		{
			name:    "templated name is rejected",
			tpl:     `{{ index .Secrets .Params.token_secret }}`,
			params:  map[string]string{"token_secret": "{{ .Params.outer_secret }}"},
			wantErr: `secret name parameter "token_secret" must be a literal secret name`,
		},
		{
			name:    "malformed non-empty parameter is rejected",
			tpl:     `{{ index .Secrets .Params.token_secret }}`,
			params:  map[string]string{"token_secret": "invalid.name"},
			wantErr: `secret name parameter "token_secret" is invalid`,
		},
		{
			name:    "step output is rejected",
			tpl:     `{{ index .Secrets .Steps.detect.Outputs.secret_name }}`,
			wantErr: "dynamic secret name must be resolved from a parameter before execution",
		},
		{
			name:    "matrix value is rejected",
			tpl:     `{{ index .Secrets .Matrix.secret_name }}`,
			wantErr: "dynamic secret name must be resolved from a parameter before execution",
		},
		{
			name:    "foreach value is rejected",
			tpl:     `{{ index .Secrets .Foreach.secret_name }}`,
			wantErr: "dynamic secret name must be resolved from a parameter before execution",
		},
		{
			name:    "multiline step output is rejected",
			tpl:     "{{ index .Secrets\n.Steps.detect.Outputs.secret_name }}",
			wantErr: "dynamic secret name must be resolved from a parameter before execution",
		},
		{
			name:    "pipelined step output is rejected",
			tpl:     `{{ .Steps.detect.Outputs.secret_name | index .Secrets }}`,
			wantErr: "dynamic secret name must be resolved from a parameter before execution",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveSecretNameParams(tt.tpl, tt.params)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestReferencedSecretNames(t *testing.T) {
	got, err := ReferencedSecretNames(
		`echo {{ secrets.API_TOKEN }} {{ .Secrets.DB_PASS }} {{ index .Secrets "gitlab-token" }}`,
	)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"API_TOKEN", "DB_PASS", "gitlab-token"}, got)
}

func TestReferencedSecretNamesRejectsRuntimeOperand(t *testing.T) {
	tests := []struct {
		name string
		tpl  string
	}{
		{
			name: "ordinary index",
			tpl:  `{{ index .Secrets .Steps.pick.Outputs.name }}`,
		},
		{
			name: "multiline index",
			tpl:  "{{ index .Secrets\n.Steps.pick.Outputs.name }}",
		},
		{
			name: "pipelined index",
			tpl:  `{{ .Steps.pick.Outputs.name | index .Secrets }}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ReferencedSecretNames(tt.tpl)
			require.ErrorContains(t, err, "dynamic secret name must be resolved from a parameter before execution")
		})
	}
}

func TestReferencedSecretNamesSkipsEmptyLiteralIndex(t *testing.T) {
	got, err := ReferencedSecretNames(`{{ index .Secrets "" }}`)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestSecretPipelineTextInsideStringIsNotReference(t *testing.T) {
	tpl := `{{ printf "| index .Secrets arbitrary text" }}`

	resolved, err := ResolveSecretNameParams(tpl, nil)
	require.NoError(t, err)
	assert.Equal(t, tpl, resolved)

	names, err := ReferencedSecretNames(tpl)
	require.NoError(t, err)
	assert.Empty(t, names)
}

func TestResolveSecretNameParamsInSpec(t *testing.T) {
	spec := Spec{
		Steps: []StepEntry{
			{Name: "main", Env: map[string]string{
				"TOKEN": `{{ index .Secrets .Params.token_secret }}`,
			}},
			{Parallel: []Step{{
				Name: "parallel",
				Run:  `use {{ index .Secrets .Params.token_secret }}`,
			}}},
		},
		Finally: []StepEntry{{
			Name: "cleanup",
			Env: map[string]string{
				"TOKEN": `{{ index .Secrets .Params.token_secret }}`,
			},
		}},
	}

	require.NoError(t, ResolveSecretNameParamsInSpec(
		&spec,
		map[string]string{"token_secret": "gitlab-token"},
	))
	assert.Contains(t, spec.Steps[0].Env["TOKEN"], `"gitlab-token"`)
	assert.Contains(t, spec.Steps[1].Parallel[0].Run, `"gitlab-token"`)
	assert.Contains(t, spec.Finally[0].Env["TOKEN"], `"gitlab-token"`)
}
