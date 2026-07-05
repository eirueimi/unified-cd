package k8sagent

import (
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecContainer_FromRunsIn(t *testing.T) {
	// RunsIn.Container が exec 先コンテナ名になる（正規化後の唯一の真実源）
	assert.Equal(t, "tools", execContainer(api.ClaimStep{RunsIn: &dsl.RunsIn{Container: "tools"}}))
	// RunsIn 未指定はデフォルトコンテナ（空文字）
	assert.Equal(t, "", execContainer(api.ClaimStep{}))
	// RunsIn.Image のみ（named container ではない）も空 = デフォルト
	assert.Equal(t, "", execContainer(api.ClaimStep{RunsIn: &dsl.RunsIn{Image: "golang:1.22"}}))
}

func TestRunsInImageUnsupported_OnK8s(t *testing.T) {
	// runsIn.image は k8s-agent 未対応（Plan B: 使い捨て pod）。
	// デフォルトコンテナで黙って実行せず、ハードエラーにする。
	err := runsInImageUnsupported(api.ClaimStep{RunsIn: &dsl.RunsIn{Image: "golang:1.22"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runsIn.image")
	assert.Contains(t, err.Error(), "not supported on the k8s agent")

	// 名前付きコンテナ（②）はエラーにしない
	assert.NoError(t, runsInImageUnsupported(api.ClaimStep{RunsIn: &dsl.RunsIn{Container: "tools"}}))
	// runsIn 省略（①デフォルト）もエラーにしない
	assert.NoError(t, runsInImageUnsupported(api.ClaimStep{}))
}
