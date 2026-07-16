package gittemplate_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

const finallyTpl = `
apiVersion: unified-cd/v1
kind: JobTemplate
metadata: {name: t}
spec:
  steps:
    - {name: work, run: echo w}
  finally:
    - {name: cleanup, if: failure(), run: echo bye}
`

func TestResolveSpec_TemplateFinallySplices(t *testing.T) {
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps:   []dsl.StepEntry{{Name: "u", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/t.yaml@v1"}}},
		Finally: []dsl.StepEntry{{Name: "callerFin", Run: "echo mine"}},
	})
	s, err := resolveToSpec(t, &stubFetcher{data: []byte(finallyTpl)}, specJSON)
	require.NoError(t, err)
	require.Len(t, s.Finally, 2)
	require.Equal(t, "callerFin", s.Finally[0].Name, "caller's own finally steps come first")
	require.Equal(t, "u__cleanup", s.Finally[1].Name, "template finally appended, prefixed")
	require.Equal(t, "failure()", s.Finally[1].If)
}

func TestResolveSpec_TemplateFinally_OuterIfCombined(t *testing.T) {
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{{Name: "u", If: `params.go == "1"`, Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/t.yaml@v1"}}},
	})
	s, err := resolveToSpec(t, &stubFetcher{data: []byte(finallyTpl)}, specJSON)
	require.NoError(t, err)
	require.Len(t, s.Finally, 1)
	require.Contains(t, s.Finally[0].If, "&&")
	require.Contains(t, s.Finally[0].If, "failure()")
}

func TestResolveSpec_TemplateFinally_ScopeModeRejected(t *testing.T) {
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{{
			Name:   "u",
			Uses:   &dsl.UsesStep{Job: "git://github.com/org/repo/t.yaml@v1"},
			RunsIn: &dsl.RunsIn{Image: "alpine:3"},
		}},
	})
	_, err := resolveToSpec(t, &stubFetcher{data: []byte(finallyTpl)}, specJSON)
	require.Error(t, err)
	require.Contains(t, err.Error(), "finally")
}

func TestResolveSpec_TemplateFinally_NameCollisionErrors(t *testing.T) {
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps:   []dsl.StepEntry{{Name: "u", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/t.yaml@v1"}}},
		Finally: []dsl.StepEntry{{Name: "u__cleanup", Run: "echo clash"}},
	})
	_, err := resolveToSpec(t, &stubFetcher{data: []byte(finallyTpl)}, specJSON)
	require.Error(t, err)
	require.Contains(t, err.Error(), "u__cleanup")
}

func TestResolveSpec_UsesInFinally_WithTemplateFinally(t *testing.T) {
	// A uses step sitting in the caller's finally whose template ALSO has finally:
	// both the body expansion and the spliced finally land in spec.Finally.
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps:   []dsl.StepEntry{{Name: "main", Run: "echo hi"}},
		Finally: []dsl.StepEntry{{Name: "fin", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/t.yaml@v1"}}},
	})
	s, err := resolveToSpec(t, &stubFetcher{data: []byte(finallyTpl)}, specJSON)
	require.NoError(t, err)
	names := map[string]bool{}
	for _, e := range s.Finally {
		names[e.Name] = true
	}
	require.True(t, names["fin__work"], "body expansion in finally")
	require.True(t, names["fin__cleanup"], "template finally spliced too; got %v", names)
}
