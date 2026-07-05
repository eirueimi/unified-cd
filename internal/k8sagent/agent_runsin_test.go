package k8sagent

import (
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
)

func TestExecContainer_FromRunsIn(t *testing.T) {
	// RunsIn.Container が exec 先コンテナ名になる（正規化後の唯一の真実源）
	assert.Equal(t, "tools", execContainer(api.ClaimStep{RunsIn: &dsl.RunsIn{Container: "tools"}}))
	// RunsIn 未指定はデフォルトコンテナ（空文字）
	assert.Equal(t, "", execContainer(api.ClaimStep{}))
	// RunsIn.Image のみ（named container ではない）も空 = デフォルト
	assert.Equal(t, "", execContainer(api.ClaimStep{RunsIn: &dsl.RunsIn{Image: "golang:1.22"}}))
}

func TestExpandStepEnv(t *testing.T) {
	td := dsl.TemplateData{Stdout: "v1"}
	// literal passes through; a template value is expanded
	out := expandStepEnv(map[string]string{
		"LIT": "plain",
		"TPL": "{{ .Stdout }}",
	}, td)
	assert.Equal(t, "plain", out["LIT"])
	assert.Equal(t, "v1", out["TPL"])
	// nil in, nil-safe out
	assert.Nil(t, expandStepEnv(nil, td))
}
