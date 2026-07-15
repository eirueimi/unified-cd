package gittemplate_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/gittemplate"
	"github.com/eirueimi/unified-cd/internal/objectstore"
)

// ---- Cache tests (unchanged from before) ----

func TestCache_GetPut_FixedRef(t *testing.T) {
	store := objectstore.NewLocalObjectStore(t.TempDir())
	c := gittemplate.NewCache(store)
	ctx := context.Background()

	u := gittemplate.URI{Host: "github.com", Owner: "org", Repo: "repo", Path: "job.yaml", Ref: "v1.0.0"}
	data := []byte("apiVersion: unified-cd/v1\nkind: Job\n")

	_, ok := c.Get(ctx, u)
	assert.False(t, ok, "expected cache miss on empty store")

	c.Put(ctx, u, data)

	got, ok := c.Get(ctx, u)
	require.True(t, ok, "expected cache hit after Put")
	assert.Equal(t, data, got)
}

func TestCache_SkipsMutableRef(t *testing.T) {
	store := objectstore.NewLocalObjectStore(t.TempDir())
	c := gittemplate.NewCache(store)
	ctx := context.Background()

	u := gittemplate.URI{Host: "github.com", Owner: "org", Repo: "repo", Path: "job.yaml", Ref: "main"}
	c.Put(ctx, u, []byte("some content")) // should be no-op for mutable refs

	_, ok := c.Get(ctx, u)
	assert.False(t, ok, "expected cache miss for mutable ref")
}

// ---- HasGitURIs ----

func TestHasGitURIs(t *testing.T) {
	specWithGit := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{
			{Name: "fetch", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/job.yaml@v1"}},
		},
	})
	specWithoutGit := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{
			{Name: "build", Run: "echo hi"},
		},
	})

	assert.True(t, gittemplate.HasGitURIs(specWithGit))
	assert.False(t, gittemplate.HasGitURIs(specWithoutGit))
}

func TestHasGitURIs_IgnoresUnrelatedGitProtocolStrings(t *testing.T) {
	// An env value (or any other field) whose content happens to start with
	// the literal "git://" substring must not be mistaken for an unresolved
	// uses step — otherwise a fully-resolved run would be re-"resolved"
	// (harmlessly, but pointlessly) on every scheduler tick forever.
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{
			{
				Name: "clone",
				Run:  "git clone $SOURCE_URL",
				Env:  map[string]string{"SOURCE_URL": "git://example.com/org/repo.git"},
			},
		},
	})
	assert.False(t, gittemplate.HasGitURIs(specJSON))
}

// ---- ResolveSpec: basic expansion ----

func noCred(ctx context.Context, host string) (gittemplate.Credential, error) {
	return gittemplate.Credential{}, nil
}

func TestResolveSpec_ExpandsUsesStep(t *testing.T) {
	const fetchedYAML = `apiVersion: unified-cd/v1
kind: JobTemplate
metadata:
  name: fetched
spec:
  steps:
    - name: run
      run: echo from git
`
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{
			{Name: "useTemplate", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/build.yaml@v1.0.0"}},
			{Name: "local", Run: "echo local"},
		},
	})

	fetcher := &stubFetcher{data: []byte(fetchedYAML)}
	resolver := gittemplate.NewResolver(fetcher, nil)

	resolved, err := resolver.ResolveSpec(context.Background(), specJSON, noCred)
	require.NoError(t, err)

	var spec dsl.Spec
	require.NoError(t, json.Unmarshal(resolved, &spec))

	require.Len(t, spec.Steps, 4) // useTemplate__inputs, useTemplate__run, useTemplate, local
	assert.Equal(t, "useTemplate__inputs", spec.Steps[0].Name)
	assert.Equal(t, "useTemplate__run", spec.Steps[1].Name)
	assert.Equal(t, "echo from git", spec.Steps[1].Run)
	assert.Equal(t, "useTemplate", spec.Steps[2].Name)
	assert.Equal(t, "local", spec.Steps[3].Name)
	assert.Equal(t, "echo local", spec.Steps[3].Run)
}

func TestResolveSpec_NoOp_WhenNoGitURIs(t *testing.T) {
	specJSON := mustMarshalSpec(dsl.Spec{Steps: []dsl.StepEntry{{Name: "b", Run: "echo hi"}}})
	fetcher := &stubFetcher{}
	resolver := gittemplate.NewResolver(fetcher, nil)

	resolved, err := resolver.ResolveSpec(context.Background(), specJSON, noCred)
	require.NoError(t, err)
	assert.JSONEq(t, string(specJSON), string(resolved))
	assert.Equal(t, 0, fetcher.calls, "expected 0 fetch calls")
}

func TestResolveSpec_WithCache_FixedRef(t *testing.T) {
	const fetchedYAML = `apiVersion: unified-cd/v1
kind: JobTemplate
metadata:
  name: cached-job
spec:
  steps:
    - name: run
      run: echo cached
`
	rawURI := "git://github.com/org/repo/cached.yaml@v2.0.0"
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{
			{Name: "useTemplate", Uses: &dsl.UsesStep{Job: rawURI}},
		},
	})

	objStore := objectstore.NewLocalObjectStore(t.TempDir())
	cache := gittemplate.NewCache(objStore)
	fetcher := &stubFetcher{data: []byte(fetchedYAML)}
	resolver := gittemplate.NewResolver(fetcher, cache)

	_, err := resolver.ResolveSpec(context.Background(), specJSON, noCred)
	require.NoError(t, err)
	assert.Equal(t, 1, fetcher.calls)

	_, err = resolver.ResolveSpec(context.Background(), specJSON, noCred)
	require.NoError(t, err)
	assert.Equal(t, 1, fetcher.calls, "second call should use cache")
}

// ---- ResolveSpec: with / outputs round-trip ----

func TestResolveSpec_WithAndOutputs(t *testing.T) {
	const fetchedYAML = `apiVersion: unified-cd/v1
kind: JobTemplate
metadata:
  name: fetched
spec:
  params:
    outputs:
      - name: image_ref
        type: string
  steps:
    - name: build
      run: docker build -t {{ .Params.tag }} .
      outputs:
        image_ref: "{{ .Params.tag }}"
`
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{
			{Name: "buildIt", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/build.yaml@v1", With: map[string]any{"tag": "myapp:latest"}}},
		},
	})
	fetcher := &stubFetcher{data: []byte(fetchedYAML)}
	resolver := gittemplate.NewResolver(fetcher, nil)

	resolved, err := resolver.ResolveSpec(context.Background(), specJSON, noCred)
	require.NoError(t, err)

	var spec dsl.Spec
	require.NoError(t, json.Unmarshal(resolved, &spec))
	require.Len(t, spec.Steps, 3)
	assert.Equal(t, "myapp:latest", spec.Steps[0].Outputs["tag"])
	assert.Equal(t, "{{ .Steps.buildIt__build.Outputs.image_ref }}", spec.Steps[2].Outputs["image_ref"])
}

// ---- ResolveSpec: recursion, cycles, depth, collisions ----

func TestResolveSpec_RecursiveUses(t *testing.T) {
	const outerYAML = `apiVersion: unified-cd/v1
kind: JobTemplate
metadata:
  name: outer
spec:
  steps:
    - name: inner
      uses:
        job: git://github.com/org/repo/inner.yaml@v1
`
	const innerYAML = `apiVersion: unified-cd/v1
kind: JobTemplate
metadata:
  name: inner
spec:
  steps:
    - name: leaf
      run: echo leaf
`
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{
			{Name: "outer", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/outer.yaml@v1"}},
		},
	})
	fetcher := &mapFetcher{byURI: map[string][]byte{
		"git://github.com/org/repo/outer.yaml@v1": []byte(outerYAML),
		"git://github.com/org/repo/inner.yaml@v1": []byte(innerYAML),
	}}
	resolver := gittemplate.NewResolver(fetcher, nil)

	resolved, err := resolver.ResolveSpec(context.Background(), specJSON, noCred)
	require.NoError(t, err)

	var spec dsl.Spec
	require.NoError(t, json.Unmarshal(resolved, &spec))

	var names []string
	for _, s := range spec.Steps {
		names = append(names, s.Name)
	}
	assert.Equal(t, []string{
		"outer__inputs",
		"outer__inner__inputs",
		"outer__inner__leaf",
		"outer__inner",
		"outer",
	}, names)
}

func TestResolveSpec_CycleDetected(t *testing.T) {
	const aYAML = `apiVersion: unified-cd/v1
kind: JobTemplate
metadata:
  name: a
spec:
  steps:
    - name: toB
      uses:
        job: git://github.com/org/repo/b.yaml@v1
`
	const bYAML = `apiVersion: unified-cd/v1
kind: JobTemplate
metadata:
  name: b
spec:
  steps:
    - name: toA
      uses:
        job: git://github.com/org/repo/a.yaml@v1
`
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{
			{Name: "start", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/a.yaml@v1"}},
		},
	})
	fetcher := &mapFetcher{byURI: map[string][]byte{
		"git://github.com/org/repo/a.yaml@v1": []byte(aYAML),
		"git://github.com/org/repo/b.yaml@v1": []byte(bYAML),
	}}
	resolver := gittemplate.NewResolver(fetcher, nil)

	_, err := resolver.ResolveSpec(context.Background(), specJSON, noCred)
	require.Error(t, err)
	assert.True(t, gittemplate.IsResolveError(err))
	assert.Contains(t, err.Error(), "circular")
}

func TestResolveSpec_MaxDepthExceeded(t *testing.T) {
	// 12 levels of uses chained, each referencing the next by index — exceeds the depth limit of 10.
	const levels = 12
	byURI := map[string][]byte{}
	for i := 0; i < levels; i++ {
		uri := fmt.Sprintf("git://github.com/org/repo/level%d.yaml@v1", i)
		var stepYAML string
		if i == levels-1 {
			stepYAML = "    - name: leaf\n      run: echo done\n"
		} else {
			next := fmt.Sprintf("git://github.com/org/repo/level%d.yaml@v1", i+1)
			stepYAML = fmt.Sprintf("    - name: next\n      uses:\n        job: %s\n", next)
		}
		byURI[uri] = []byte(fmt.Sprintf("apiVersion: unified-cd/v1\nkind: JobTemplate\nmetadata:\n  name: level%d\nspec:\n  steps:\n%s", i, stepYAML))
	}
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{
			{Name: "start", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/level0.yaml@v1"}},
		},
	})
	fetcher := &mapFetcher{byURI: byURI}
	resolver := gittemplate.NewResolver(fetcher, nil)

	_, err := resolver.ResolveSpec(context.Background(), specJSON, noCred)
	require.Error(t, err)
	assert.True(t, gittemplate.IsResolveError(err))
	assert.Contains(t, err.Error(), "depth")
}

func TestResolveSpec_NameCollisionDetected(t *testing.T) {
	const fetchedYAML = `apiVersion: unified-cd/v1
kind: JobTemplate
metadata:
  name: fetched
spec:
  steps:
    - name: inputs
      run: echo hi
`
	// The parent already has a step literally named "useTemplate__inputs", which
	// collides with the synthetic inputs-capture step name expandUsesStep would generate.
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{
			{Name: "useTemplate", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/build.yaml@v1"}},
			{Name: "useTemplate__inputs", Run: "echo collide"},
		},
	})
	fetcher := &stubFetcher{data: []byte(fetchedYAML)}
	resolver := gittemplate.NewResolver(fetcher, nil)

	_, err := resolver.ResolveSpec(context.Background(), specJSON, noCred)
	require.Error(t, err)
	assert.True(t, gittemplate.IsResolveError(err))
	assert.Contains(t, err.Error(), "collid")
}

func TestResolveSpec_MalformedYAML_IsDeterministicError(t *testing.T) {
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{
			{Name: "useTemplate", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/build.yaml@v1"}},
		},
	})
	fetcher := &stubFetcher{data: []byte("not a job manifest")}
	resolver := gittemplate.NewResolver(fetcher, nil)

	_, err := resolver.ResolveSpec(context.Background(), specJSON, noCred)
	require.Error(t, err)
	assert.True(t, gittemplate.IsResolveError(err))
}

func TestResolveSpec_FetchFailure_IsTransientError(t *testing.T) {
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{
			{Name: "useTemplate", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/build.yaml@v1"}},
		},
	})
	fetcher := &erroringFetcher{}
	resolver := gittemplate.NewResolver(fetcher, nil)

	_, err := resolver.ResolveSpec(context.Background(), specJSON, noCred)
	require.Error(t, err)
	assert.False(t, gittemplate.IsResolveError(err), "fetch failures must be treated as transient, not deterministic")
}

// ---- test doubles ----

type stubFetcher struct {
	data  []byte
	calls int
}

func (s *stubFetcher) Fetch(ctx context.Context, uri gittemplate.URI, token, sshKey string) ([]byte, error) {
	s.calls++
	return s.data, nil
}

// mapFetcher returns fetchedYAML keyed by the exact "git://..." URI string requested.
type mapFetcher struct {
	byURI map[string][]byte
}

func (m *mapFetcher) Fetch(ctx context.Context, uri gittemplate.URI, token, sshKey string) ([]byte, error) {
	data, ok := m.byURI[uri.Raw]
	if !ok {
		return nil, fmt.Errorf("mapFetcher: no fixture for %q", uri.Raw)
	}
	return data, nil
}

type erroringFetcher struct{}

func (erroringFetcher) Fetch(ctx context.Context, uri gittemplate.URI, token, sshKey string) ([]byte, error) {
	return nil, fmt.Errorf("network unreachable")
}

func mustMarshalSpec(spec dsl.Spec) []byte {
	b, err := json.Marshal(spec)
	if err != nil {
		panic(err)
	}
	return b
}
