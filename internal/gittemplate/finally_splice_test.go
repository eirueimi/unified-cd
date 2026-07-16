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

const innerPlainTpl = `
apiVersion: unified-cd/v1
kind: JobTemplate
metadata: {name: inner}
spec:
  steps:
    - {name: deep, run: echo d}
`

const innerWithFinallyTpl = `
apiVersion: unified-cd/v1
kind: JobTemplate
metadata: {name: inner}
spec:
  steps:
    - {name: deep, run: echo d}
  finally:
    - {name: mop, run: echo m}
`

const outerFinallyUsesTpl = `
apiVersion: unified-cd/v1
kind: JobTemplate
metadata: {name: outer}
spec:
  steps:
    - {name: work, run: echo w}
  finally:
    - {name: cleanup, uses: {job: git://github.com/org/repo/inner.yaml@v1}}
`

const outerBodyUsesInnerFinallyTpl = `
apiVersion: unified-cd/v1
kind: JobTemplate
metadata: {name: outer}
spec:
  steps:
    - {name: x, uses: {job: git://github.com/org/repo/inner.yaml@v1}}
`

func finallyNamesOf(s dsl.Spec) map[string]bool {
	names := map[string]bool{}
	for _, e := range s.Finally {
		names[e.Name] = true
	}
	return names
}

func TestResolveSpec_NestedUsesInsideTemplateFinally_Resolved(t *testing.T) {
	// The outer template's OWN finally: contains a uses: step. It must be
	// recursively resolved (not hit expandUsesStep's unresolved-uses guard),
	// and the inner expansion must carry the full prefix chain.
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{{Name: "u", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/outer.yaml@v1"}}},
	})
	fetcher := &mapFetcher{byURI: map[string][]byte{
		"git://github.com/org/repo/outer.yaml@v1": []byte(outerFinallyUsesTpl),
		"git://github.com/org/repo/inner.yaml@v1": []byte(innerPlainTpl),
	}}
	s, err := resolveToSpec(t, fetcher, specJSON)
	require.NoError(t, err)
	for _, e := range s.Finally {
		require.Nil(t, e.Uses, "no unresolved uses may survive in finally")
	}
	names := finallyNamesOf(s)
	require.True(t, names["u__cleanup__inputs"], "inner inputs step, doubly prefixed; got %v", names)
	require.True(t, names["u__cleanup__deep"], "inner body step, doubly prefixed; got %v", names)
	require.True(t, names["u__cleanup"], "inner capture step, outer-prefixed; got %v", names)
}

func TestResolveSpec_NestedUsesInsideTemplateFinally_InnerFinallyBubbles(t *testing.T) {
	// The uses inside the outer template's finally itself carries a template
	// finally — it must bubble into the caller's finally with the full prefix
	// chain applied (not leak un-prefixed).
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{{Name: "u", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/outer.yaml@v1"}}},
	})
	fetcher := &mapFetcher{byURI: map[string][]byte{
		"git://github.com/org/repo/outer.yaml@v1": []byte(outerFinallyUsesTpl),
		"git://github.com/org/repo/inner.yaml@v1": []byte(innerWithFinallyTpl),
	}}
	s, err := resolveToSpec(t, fetcher, specJSON)
	require.NoError(t, err)
	names := finallyNamesOf(s)
	require.True(t, names["u__cleanup__deep"], "inner body expansion present; got %v", names)
	require.True(t, names["u__cleanup__mop"], "inner template finally fully prefixed; got %v", names)
	require.False(t, names["cleanup__mop"], "inner template finally must not leak un-prefixed; got %v", names)
}

func TestResolveSpec_NestedUsesInBody_InnerFinallyFullyPrefixed(t *testing.T) {
	// A uses in the outer template's BODY whose inner template has finally:
	// the inner finally must reach the caller's finally with the full prefix
	// chain (u__x__mop), so its refs stay valid and two uses of the same
	// outer template cannot collide.
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{{Name: "u", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/outer.yaml@v1"}}},
	})
	fetcher := &mapFetcher{byURI: map[string][]byte{
		"git://github.com/org/repo/outer.yaml@v1": []byte(outerBodyUsesInnerFinallyTpl),
		"git://github.com/org/repo/inner.yaml@v1": []byte(innerWithFinallyTpl),
	}}
	s, err := resolveToSpec(t, fetcher, specJSON)
	require.NoError(t, err)
	names := finallyNamesOf(s)
	require.True(t, names["u__x__mop"], "nested-in-body template finally fully prefixed; got %v", names)
	require.False(t, names["x__mop"], "nested-in-body template finally must not leak un-prefixed; got %v", names)
}

func TestResolveSpec_TwoSiblingUsesOfSameTemplate_DistinctFinallyNames(t *testing.T) {
	// Two uses: steps ("a" and "b") both point at the SAME template URI. The
	// template has body steps and a finally: cleanup step. Each expansion must
	// be prefixed by its own usesName, so the spliced finally steps land as
	// distinct a__cleanup / b__cleanup entries with no resolve error (no
	// collision even though both derive from the identical template).
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{
			{Name: "a", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/t.yaml@v1"}},
			{Name: "b", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/t.yaml@v1"}},
		},
	})
	s, err := resolveToSpec(t, &stubFetcher{data: []byte(finallyTpl)}, specJSON)
	require.NoError(t, err)
	names := finallyNamesOf(s)
	require.True(t, names["a__cleanup"], "a's spliced finally step must be present; got %v", names)
	require.True(t, names["b__cleanup"], "b's spliced finally step must be present; got %v", names)
	require.Len(t, s.Finally, 2, "both distinct, no collision; got %v", names)
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
