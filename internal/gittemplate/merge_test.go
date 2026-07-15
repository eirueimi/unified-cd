package gittemplate_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/gittemplate"
)

// resolveToSpec resolves specJSON with the given fetcher and unmarshals the result.
func resolveToSpec(t *testing.T, fetcher gittemplate.FetcherInterface, specJSON []byte) (dsl.Spec, error) {
	t.Helper()
	resolver := gittemplate.NewResolver(fetcher, nil)
	out, err := resolver.ResolveSpec(context.Background(), specJSON, noCred)
	if err != nil {
		return dsl.Spec{}, err
	}
	var s dsl.Spec
	require.NoError(t, json.Unmarshal(out, &s))
	return s, nil
}

func defNames(defs []map[string]any) []string {
	var out []string
	for _, d := range defs {
		out = append(out, dsl.DefName(d))
	}
	return out
}

// Template with its own container + the volume it mounts, and a step targeting it.
const tmplWithToolsAndVolume = `
apiVersion: unified-cd/v1
kind: JobTemplate
metadata: {name: tmpl}
spec:
  podTemplate:
    spec:
      containers:
        - name: tools
          image: alpine:3
          volumeMounts: [{name: toolcache, mountPath: /tc}]
      volumes:
        - {name: toolcache, emptyDir: {}}
  steps:
    - name: run-in-tools
      container: tools
      run: echo hi
`

func TestResolveSpec_MergesTemplateContainerAndVolume(t *testing.T) {
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{{Name: "u", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/tmpl.yaml@v1"}}},
	})
	s, err := resolveToSpec(t, &stubFetcher{data: []byte(tmplWithToolsAndVolume)}, specJSON)
	require.NoError(t, err)
	require.Contains(t, defNames(dsl.PodTemplateContainers(s.PodTemplate)), "tools", "template container must be merged")
	require.Contains(t, defNames(dsl.PodTemplateVolumes(s.PodTemplate)), "toolcache", "template volume must be merged")
}

func TestResolveSpec_CallerContainerKept_IdenticalDedup(t *testing.T) {
	specJSON := mustMarshalSpec(dsl.Spec{
		PodTemplate: &dsl.PodTemplate{Spec: map[string]any{"containers": []any{
			map[string]any{"name": "tools", "image": "alpine:3", "volumeMounts": []any{map[string]any{"name": "toolcache", "mountPath": "/tc"}}},
		}}},
		Steps: []dsl.StepEntry{{Name: "u", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/tmpl.yaml@v1"}}},
	})
	s, err := resolveToSpec(t, &stubFetcher{data: []byte(tmplWithToolsAndVolume)}, specJSON)
	require.NoError(t, err)
	n := 0
	for _, name := range defNames(dsl.PodTemplateContainers(s.PodTemplate)) {
		if name == "tools" {
			n++
		}
	}
	require.Equal(t, 1, n, "identical caller+template container must dedup to one")
	require.Contains(t, defNames(dsl.PodTemplateVolumes(s.PodTemplate)), "toolcache", "volume still merged")
}

func TestResolveSpec_CollisionDiffers_Errors(t *testing.T) {
	specJSON := mustMarshalSpec(dsl.Spec{
		PodTemplate: &dsl.PodTemplate{Spec: map[string]any{"containers": []any{
			map[string]any{"name": "tools", "image": "ubuntu:22.04"},
		}}},
		Steps: []dsl.StepEntry{{Name: "u", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/tmpl.yaml@v1"}}},
	})
	_, err := resolveToSpec(t, &stubFetcher{data: []byte(tmplWithToolsAndVolume)}, specJSON)
	require.Error(t, err)
	require.True(t, gittemplate.IsResolveError(err), "differing collision must be a deterministic resolve error")
	require.Contains(t, err.Error(), "tools")
}

const tmplReservedContainer = `
apiVersion: unified-cd/v1
kind: JobTemplate
metadata: {name: tmpl}
spec:
  podTemplate:
    spec:
      containers: [{name: job, image: evil:latest}]
  steps:
    - {name: s, run: echo hi}
`

const tmplReservedVolume = `
apiVersion: unified-cd/v1
kind: JobTemplate
metadata: {name: tmpl}
spec:
  podTemplate:
    spec:
      volumes: [{name: workspace, emptyDir: {}}]
  steps:
    - {name: s, run: echo hi}
`

func TestResolveSpec_ReservedNameInjection_Errors(t *testing.T) {
	for name, tmpl := range map[string]string{"container job": tmplReservedContainer, "volume workspace": tmplReservedVolume} {
		t.Run(name, func(t *testing.T) {
			specJSON := mustMarshalSpec(dsl.Spec{
				Steps: []dsl.StepEntry{{Name: "u", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/tmpl.yaml@v1"}}},
			})
			_, err := resolveToSpec(t, &stubFetcher{data: []byte(tmpl)}, specJSON)
			require.Error(t, err)
			require.True(t, gittemplate.IsResolveError(err))
		})
	}
}

// Kind gate: a kind: Job target is rejected with conversion guidance, and an
// unknown field on a JobTemplate surfaces as a deterministic resolve error.
const tmplKindJob = `
apiVersion: unified-cd/v1
kind: Job
metadata: {name: tmpl}
spec:
  steps:
    - {name: s, run: echo hi}
`

const tmplUnknownField = `
apiVersion: unified-cd/v1
kind: JobTemplate
metadata: {name: tmpl}
spec:
  agentSelector: [gpu]
  steps:
    - {name: s, run: echo hi}
`

func TestResolveSpec_KindGate(t *testing.T) {
	cases := map[string]struct {
		tmpl    string
		errWant string
	}{
		"kind Job rejected":    {tmplKindJob, "kind: JobTemplate"},
		"unknown field errors": {tmplUnknownField, "agentSelector"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			specJSON := mustMarshalSpec(dsl.Spec{
				Steps: []dsl.StepEntry{{Name: "u", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/tmpl.yaml@v1"}}},
			})
			_, err := resolveToSpec(t, &stubFetcher{data: []byte(tc.tmpl)}, specJSON)
			require.Error(t, err)
			require.True(t, gittemplate.IsResolveError(err), "kind/schema violations must be deterministic resolve errors")
			require.Contains(t, err.Error(), tc.errWant)
		})
	}
}

func TestResolveSpec_ScopeMode_PodTemplateRejected(t *testing.T) {
	// A uses step WITH runsIn.image is scope mode: a template podTemplate is an error.
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{{
			Name:   "u",
			Uses:   &dsl.UsesStep{Job: "git://github.com/org/repo/tmpl.yaml@v1"},
			RunsIn: &dsl.RunsIn{Image: "alpine:3"},
		}},
	})
	_, err := resolveToSpec(t, &stubFetcher{data: []byte(tmplWithToolsAndVolume)}, specJSON)
	require.Error(t, err)
	require.True(t, gittemplate.IsResolveError(err))
	require.Contains(t, err.Error(), "podTemplate")
}

func TestResolveSpec_ScopeMode_NoPodTemplate_OK(t *testing.T) {
	// Scope mode with a plain steps-only template still works (no merge, no error).
	const plain = `
apiVersion: unified-cd/v1
kind: JobTemplate
metadata: {name: tmpl}
spec:
  steps:
    - {name: s, run: echo hi}
`
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{{
			Name:   "u",
			Uses:   &dsl.UsesStep{Job: "git://github.com/org/repo/tmpl.yaml@v1"},
			RunsIn: &dsl.RunsIn{Image: "alpine:3"},
		}},
	})
	s, err := resolveToSpec(t, &stubFetcher{data: []byte(plain)}, specJSON)
	require.NoError(t, err)
	require.Nil(t, s.PodTemplate, "scope mode must not create/merge a podTemplate")
}

func TestResolveSpec_NestedUses_BubblesPodShape(t *testing.T) {
	const inner = `
apiVersion: unified-cd/v1
kind: JobTemplate
metadata: {name: inner}
spec:
  podTemplate:
    spec:
      containers: [{name: deep, image: alpine:3}]
      volumes: [{name: deepvol, emptyDir: {}}]
  steps:
    - {name: leaf, container: deep, run: echo hi}
`
	const outer = `
apiVersion: unified-cd/v1
kind: JobTemplate
metadata: {name: outer}
spec:
  steps:
    - {name: mid, uses: {job: "git://github.com/org/repo/inner.yaml@v1"}}
`
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{{Name: "top", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/outer.yaml@v1"}}},
	})
	fetcher := &mapFetcher{byURI: map[string][]byte{
		"git://github.com/org/repo/outer.yaml@v1": []byte(outer),
		"git://github.com/org/repo/inner.yaml@v1": []byte(inner),
	}}
	s, err := resolveToSpec(t, fetcher, specJSON)
	require.NoError(t, err)
	require.Contains(t, defNames(dsl.PodTemplateContainers(s.PodTemplate)), "deep")
	require.Contains(t, defNames(dsl.PodTemplateVolumes(s.PodTemplate)), "deepvol")
}

func TestResolveSpec_MergedK8sOnlyContainer_FlipsRouting(t *testing.T) {
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{{Name: "u", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/tmpl.yaml@v1"}}},
	})
	s, err := resolveToSpec(t, &stubFetcher{data: []byte(tmplWithToolsAndVolume)}, specJSON)
	require.NoError(t, err)
	require.True(t, dsl.PodTemplateNeedsKubernetes(s.PodTemplate),
		"a merged container with volumeMounts (and pod volumes) must make the podTemplate require kubernetes")
}
