package gittemplate

import (
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExpandUsesStep_LinearChainAndOutputs(t *testing.T) {
	tplSpec := dsl.Spec{
		Params: dsl.Params{
			Outputs: []dsl.Output{{Name: "image_ref", Type: "string"}},
		},
		Steps: []dsl.StepEntry{
			{
				Name: "build",
				Run:  "docker build -t {{ .Params.image }}:{{ .Params.tag }} .",
				Outputs: map[string]string{
					"image_ref": "{{ .Params.image }}:{{ .Params.tag }}",
				},
			},
			{
				Name: "push",
				Run:  "docker push {{ .Steps.build.Outputs.image_ref }}",
			},
		},
	}

	expanded, err := expandUsesStep("buildWithTemplate",
		map[string]string{"image": "myapp", "tag": "latest"}, tplSpec, nil, "")
	require.NoError(t, err)
	require.Len(t, expanded, 4)

	inputs := expanded[0]
	assert.Equal(t, "buildWithTemplate__inputs", inputs.Name)
	assert.Equal(t, "myapp", inputs.Outputs["image"])
	assert.Equal(t, "latest", inputs.Outputs["tag"])

	build := expanded[1]
	assert.Equal(t, "buildWithTemplate__build", build.Name)
	assert.Equal(t,
		"docker build -t {{ .Steps.buildWithTemplate__inputs.Outputs.image }}:{{ .Steps.buildWithTemplate__inputs.Outputs.tag }} .",
		build.Run)
	assert.Equal(t,
		"{{ .Steps.buildWithTemplate__inputs.Outputs.image }}:{{ .Steps.buildWithTemplate__inputs.Outputs.tag }}",
		build.Outputs["image_ref"])

	push := expanded[2]
	assert.Equal(t, "buildWithTemplate__push", push.Name)
	assert.Equal(t, "docker push {{ .Steps.buildWithTemplate__build.Outputs.image_ref }}", push.Run)

	capture := expanded[3]
	assert.Equal(t, "buildWithTemplate", capture.Name)
	assert.Equal(t, "{{ .Steps.buildWithTemplate__build.Outputs.image_ref }}", capture.Outputs["image_ref"])
}

func TestExpandUsesStep_ParallelRootsBothFeedCapture(t *testing.T) {
	tplSpec := dsl.Spec{
		Steps: []dsl.StepEntry{
			{Name: "lint", Run: "golangci-lint run"},
			{Name: "test", Run: "go test ./..."},
		},
	}

	expanded, err := expandUsesStep("ci", nil, tplSpec, nil, "")
	require.NoError(t, err)
	require.Len(t, expanded, 4) // inputs, lint, test, capture

	inputs := expanded[0]
	assert.Equal(t, "ci__inputs", inputs.Name)

	assert.Equal(t, "ci__lint", expanded[1].Name)
	assert.Equal(t, "ci__test", expanded[2].Name)
	assert.Equal(t, "ci", expanded[3].Name)
}

func TestExpandUsesStep_NoDeclaredOutputs_StillProducesCaptureStep(t *testing.T) {
	tplSpec := dsl.Spec{
		Steps: []dsl.StepEntry{{Name: "only", Run: "echo hi"}},
	}
	expanded, err := expandUsesStep("simple", nil, tplSpec, nil, "")
	require.NoError(t, err)
	require.Len(t, expanded, 3)
	capture := expanded[2]
	assert.Equal(t, "simple", capture.Name)
	assert.Empty(t, capture.Outputs)
}

func TestExpandUsesStep_RewritesIfConditionAndEnv(t *testing.T) {
	tplSpec := dsl.Spec{
		Steps: []dsl.StepEntry{
			{
				Name: "maybeDeploy",
				If:   `params.env == "production"`,
				Run:  "deploy.sh",
				Env:  map[string]string{"TARGET_ENV": "{{ .Params.env }}"},
			},
		},
	}
	expanded, err := expandUsesStep("rollout", map[string]string{"env": "production"}, tplSpec, nil, "")
	require.NoError(t, err)
	inner := expanded[1]
	assert.Equal(t, `steps.rollout__inputs.outputs.env == "production"`, inner.If)
	assert.Equal(t, "{{ .Steps.rollout__inputs.Outputs.env }}", inner.Env["TARGET_ENV"])
}

func TestExpandUsesStep_PreservesAndRewritesPostHook(t *testing.T) {
	tplSpec := dsl.Spec{
		Steps: []dsl.StepEntry{
			{
				Name: "checkout",
				Run:  "git clone {{ .Params.repoURL }} /workspace",
				Post: &dsl.PostStep{
					Run: "rm -rf /workspace",
					Env: map[string]string{"REPO": "{{ .Params.repoURL }}"},
				},
			},
		},
	}
	expanded, err := expandUsesStep("fetchRepo", map[string]string{"repoURL": "https://example.com/x.git"}, tplSpec, nil, "")
	require.NoError(t, err)
	inner := expanded[1]
	require.NotNil(t, inner.Post)
	assert.Equal(t, "rm -rf /workspace", inner.Post.Run)
	assert.Equal(t, "{{ .Steps.fetchRepo__inputs.Outputs.repoURL }}", inner.Post.Env["REPO"])
}

func TestExpandUsesStep_OmittedInputWithDefault_InputsStepCarriesDefault(t *testing.T) {
	tplSpec := dsl.Spec{
		Params: dsl.Params{
			Inputs: []dsl.Input{
				{Name: "expect_status", Type: "string", Default: "200"},
			},
		},
		Steps: []dsl.StepEntry{
			{Name: "poll", Run: "curl {{ .Params.expect_status }}"},
		},
	}

	expanded, err := expandUsesStep("smoke", map[string]string{}, tplSpec, nil, "")
	require.NoError(t, err)

	inputs := expanded[0]
	assert.Equal(t, "smoke__inputs", inputs.Name)
	assert.Equal(t, "200", inputs.Outputs["expect_status"])
}

func TestExpandUsesStep_WithValueOverridesDefault(t *testing.T) {
	tplSpec := dsl.Spec{
		Params: dsl.Params{
			Inputs: []dsl.Input{
				{Name: "expect_status", Type: "string", Default: "200"},
			},
		},
		Steps: []dsl.StepEntry{
			{Name: "poll", Run: "curl {{ .Params.expect_status }}"},
		},
	}

	expanded, err := expandUsesStep("smoke", map[string]string{"expect_status": "204"}, tplSpec, nil, "")
	require.NoError(t, err)

	inputs := expanded[0]
	assert.Equal(t, "204", inputs.Outputs["expect_status"])
}

func TestExpandUsesStep_RequiredInputMissingNoDefault_Errors(t *testing.T) {
	tplSpec := dsl.Spec{
		Params: dsl.Params{
			Inputs: []dsl.Input{
				{Name: "url", Type: "string", Required: true},
			},
		},
		Steps: []dsl.StepEntry{
			{Name: "poll", Run: "curl {{ .Params.url }}"},
		},
	}

	_, err := expandUsesStep("smoke", map[string]string{}, tplSpec, nil, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "url")
}

func TestExpandUsesStep_NilDefaultOptionalInputOmitted_KeyAbsent(t *testing.T) {
	tplSpec := dsl.Spec{
		Params: dsl.Params{
			Inputs: []dsl.Input{
				{Name: "note", Type: "string"}, // optional, no default
			},
		},
		Steps: []dsl.StepEntry{
			{Name: "poll", Run: "curl"},
		},
	}

	expanded, err := expandUsesStep("smoke", map[string]string{}, tplSpec, nil, "")
	require.NoError(t, err)

	inputs := expanded[0]
	_, ok := inputs.Outputs["note"]
	assert.False(t, ok, "optional input with nil Default should be absent, not rendered as <nil>")
}

func TestExpandUsesStep_RewritesCacheStep(t *testing.T) {
	tplSpec := dsl.Spec{
		Steps: []dsl.StepEntry{
			{
				Name:  "restore",
				Cache: &dsl.CacheStep{Path: "/go/pkg/mod", Key: "mod-{{ .Params.goVersion }}"},
			},
			{
				Name: "build",
				Run:  "go build ./...",
			},
		},
	}
	expanded, err := expandUsesStep("gobuild", map[string]string{"goVersion": "1.24"}, tplSpec, nil, "")
	require.NoError(t, err)
	restore := expanded[1]
	require.NotNil(t, restore.Cache)
	assert.Equal(t, "mod-{{ .Steps.gobuild__inputs.Outputs.goVersion }}", restore.Cache.Key)
}

func TestExpandUsesStep_EmptyWithValueForRequiredInput_Errors(t *testing.T) {
	tplSpec := dsl.Spec{
		Params: dsl.Params{
			Inputs: []dsl.Input{
				{Name: "url", Type: "string", Required: true},
			},
		},
		Steps: []dsl.StepEntry{
			{Name: "poll", Run: "curl {{ .Params.url }}"},
		},
	}

	// Mirroring resolveParams: an explicit empty string does not satisfy a
	// required input.
	_, err := expandUsesStep("smoke", map[string]string{"url": ""}, tplSpec, nil, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "url")
}

func TestExpandUsesStep_EmptyWithValueFallsBackToDefault(t *testing.T) {
	tplSpec := dsl.Spec{
		Params: dsl.Params{
			Inputs: []dsl.Input{
				{Name: "tags", Type: "string", Default: "latest"},
			},
		},
		Steps: []dsl.StepEntry{
			{Name: "build", Run: "docker build -t app:{{ .Params.tags }} ."},
		},
	}

	// Mirroring resolveParams: an explicit empty string is unset, so the
	// declared default wins.
	expanded, err := expandUsesStep("build", map[string]string{"tags": ""}, tplSpec, nil, "")
	require.NoError(t, err)

	inputs := expanded[0]
	assert.Equal(t, "latest", inputs.Outputs["tags"])
}

func TestExpandUsesStep_EmptyWithValueNoDefault_PassesThrough(t *testing.T) {
	tplSpec := dsl.Spec{
		Params: dsl.Params{
			Inputs: []dsl.Input{
				{Name: "note", Type: "string"}, // optional, no default
			},
		},
		Steps: []dsl.StepEntry{
			{Name: "poll", Run: "curl"},
		},
	}

	// Mirroring resolveParams: with no default to fall back to, an explicit
	// empty string is kept as-is.
	expanded, err := expandUsesStep("smoke", map[string]string{"note": ""}, tplSpec, nil, "")
	require.NoError(t, err)

	inputs := expanded[0]
	v, ok := inputs.Outputs["note"]
	assert.True(t, ok, "explicit empty string without a default should pass through")
	assert.Equal(t, "", v)
}
