package gittemplate_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

const ifTmpl = `
apiVersion: unified-cd/v1
kind: JobTemplate
metadata: {name: t}
spec:
  steps:
    - {name: plain, run: echo a}
    - {name: gated, if: params.x == "1", run: echo b}
    - parallel:
        - {name: p1, run: echo c}
`

func TestResolveSpec_OuterIfPropagates(t *testing.T) {
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{{
			Name: "u",
			If:   `failure()`,
			Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/t.yaml@v1"},
		}},
	})
	s, err := resolveToSpec(t, &stubFetcher{data: []byte(ifTmpl)}, specJSON)
	require.NoError(t, err)

	byName := map[string]dsl.StepEntry{}
	var parallelIf string
	for _, e := range s.Steps {
		if e.Parallel != nil {
			parallelIf = e.Parallel[0].If
			continue
		}
		byName[e.Name] = e
	}
	// Synthetic inputs step, plain body step, capture step: outer if verbatim.
	require.Equal(t, `failure()`, byName["u__inputs"].If, "inputs step must carry the outer if")
	require.Equal(t, `failure()`, byName["u__plain"].If)
	require.Equal(t, `failure()`, byName["u"].If, "capture step must carry the outer if")
	// Gated body step: AND-combined with the rewritten inner if.
	require.Contains(t, byName["u__gated"].If, "failure()")
	require.Contains(t, byName["u__gated"].If, "&&")
	require.Contains(t, byName["u__gated"].If, `== "1"`)
	// Parallel sub-step gated too.
	require.Equal(t, `failure()`, parallelIf)
}

func TestResolveSpec_NoOuterIf_InnerPreserved(t *testing.T) {
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{{
			Name: "u",
			Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/t.yaml@v1"},
		}},
	})
	s, err := resolveToSpec(t, &stubFetcher{data: []byte(ifTmpl)}, specJSON)
	require.NoError(t, err)
	for _, e := range s.Steps {
		if e.Name == "u__gated" {
			require.NotContains(t, e.If, "&&", "no outer if -> inner if unchanged")
			require.Contains(t, e.If, `== "1"`)
		}
		if e.Name == "u__plain" {
			require.Empty(t, e.If)
		}
	}
}
