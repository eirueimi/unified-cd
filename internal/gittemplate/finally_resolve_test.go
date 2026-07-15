package gittemplate_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/gittemplate"
)

const finallyTmpl = `
apiVersion: unified-cd/v1
kind: JobTemplate
metadata: {name: tmpl}
spec:
  podTemplate:
    spec:
      containers: [{name: tools, image: alpine:3}]
  steps:
    - {name: cleanup, container: tools, run: echo bye}
`

func TestHasGitURIs_FinallyOnly(t *testing.T) {
	spec := mustMarshalSpec(dsl.Spec{
		Steps:   []dsl.StepEntry{{Name: "main", Run: "echo hi"}},
		Finally: []dsl.StepEntry{{Name: "fin", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/t.yaml@v1"}}},
	})
	require.True(t, gittemplate.HasGitURIs(spec), "a finally-only uses spec must be seen by the resolver")
}

func TestResolveSpec_ResolvesFinallyUses(t *testing.T) {
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps:   []dsl.StepEntry{{Name: "main", Run: "echo hi"}},
		Finally: []dsl.StepEntry{{Name: "fin", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/t.yaml@v1"}}},
	})
	s, err := resolveToSpec(t, &stubFetcher{data: []byte(finallyTmpl)}, specJSON)
	require.NoError(t, err)
	// The finally uses step must be expanded (no Uses left) and prefixed steps present.
	for _, e := range s.Finally {
		require.Nil(t, e.Uses, "finally uses step must be expanded")
	}
	names := map[string]bool{}
	for _, e := range s.Finally {
		names[e.Name] = true
	}
	require.True(t, names["fin__cleanup"], "template step must be inlined into finally with the uses prefix; got %v", names)
	// Its pod-shape contribution merges into the caller's podTemplate.
	require.Contains(t, defNames(dsl.PodTemplateContainers(s.PodTemplate)), "tools")
}

func TestResolveSpec_FinallyNameCollisionWithSteps_Errors(t *testing.T) {
	// The caller already has a main step named `fin__cleanup`; the finally
	// expansion would produce the same name -> deterministic error.
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{
			{Name: "fin__cleanup", Run: "echo clash"},
		},
		Finally: []dsl.StepEntry{{Name: "fin", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/t.yaml@v1"}}},
	})
	_, err := resolveToSpec(t, &stubFetcher{data: []byte(finallyTmpl)}, specJSON)
	require.Error(t, err)
	require.True(t, gittemplate.IsResolveError(err))
	require.Contains(t, err.Error(), "fin__cleanup")
}
