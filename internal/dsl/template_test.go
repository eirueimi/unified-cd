package dsl

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExpandTemplate_Params(t *testing.T) {
	data := TemplateData{
		Params: map[string]string{"target_env": "prod", "dry_run": "false"},
	}
	result, err := ExpandTemplate("deploy to {{ .Params.target_env }}", data)
	require.NoError(t, err)
	assert.Equal(t, "deploy to prod", result)
}

func TestExpandAgentSelector_ExpandsParams(t *testing.T) {
	result, err := ExpandAgentSelector(
		[]string{"kind:linux", "pool:{{ .Params.pool }}"},
		map[string]string{"pool": "build"},
	)
	require.NoError(t, err)
	assert.Equal(t, []string{"kind:linux", "pool:build"}, result)
}

func TestExpandAgentSelector_EmptyReturnsEmpty(t *testing.T) {
	result, err := ExpandAgentSelector(nil, map[string]string{})
	require.NoError(t, err)
	assert.Empty(t, result)
}

func TestExpandAgentSelector_PropagatesTemplateError(t *testing.T) {
	_, err := ExpandAgentSelector([]string{"pool:{{ .Params.pool"}, map[string]string{})
	assert.Error(t, err)
}

func TestExpandConcurrency_NilReturnsNil(t *testing.T) {
	result, err := ExpandConcurrency(nil, map[string]string{"env": "prod"})
	require.NoError(t, err)
	assert.Nil(t, result)
}

func TestExpandConcurrency_ExpandsMutexAndPool(t *testing.T) {
	c := &Concurrency{
		Mutex: "deploy-{{ .Params.env }}",
		Semaphores: []Semaphore{
			{Pool: "{{ .Params.env }}-tokens", Capacity: 3},
		},
	}
	result, err := ExpandConcurrency(c, map[string]string{"env": "prod"})
	require.NoError(t, err)
	assert.Equal(t, "deploy-prod", result.Mutex)
	require.Len(t, result.Semaphores, 1)
	assert.Equal(t, "prod-tokens", result.Semaphores[0].Pool)
	assert.Equal(t, 3, result.Semaphores[0].Capacity)
}

func TestExpandConcurrency_DoesNotMutateInput(t *testing.T) {
	c := &Concurrency{
		Mutex:      "deploy-{{ .Params.env }}",
		Semaphores: []Semaphore{{Pool: "{{ .Params.env }}-tokens", Capacity: 1}},
	}
	_, err := ExpandConcurrency(c, map[string]string{"env": "prod"})
	require.NoError(t, err)
	assert.Equal(t, "deploy-{{ .Params.env }}", c.Mutex, "input Mutex must not be mutated")
	assert.Equal(t, "{{ .Params.env }}-tokens", c.Semaphores[0].Pool, "input Pool must not be mutated")
}

func TestExpandConcurrency_NoTemplateLiteralPassthrough(t *testing.T) {
	c := &Concurrency{
		Mutex:      "deploy-prod",
		Semaphores: []Semaphore{{Pool: "tokens", Capacity: 2}},
	}
	result, err := ExpandConcurrency(c, map[string]string{})
	require.NoError(t, err)
	assert.Equal(t, "deploy-prod", result.Mutex)
	assert.Equal(t, "tokens", result.Semaphores[0].Pool)
}

func TestExpandConcurrency_MissingParamKeyExpandsToEmpty(t *testing.T) {
	c := &Concurrency{Mutex: "deploy-{{ .Params.missing }}"}
	result, err := ExpandConcurrency(c, map[string]string{})
	require.NoError(t, err)
	assert.Equal(t, "deploy-", result.Mutex)
}

func TestExpandConcurrency_PropagatesMutexTemplateError(t *testing.T) {
	c := &Concurrency{Mutex: "deploy-{{ .Params.env"}
	_, err := ExpandConcurrency(c, map[string]string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "concurrency.mutex")
}

func TestExpandConcurrency_PropagatesPoolTemplateError(t *testing.T) {
	c := &Concurrency{Semaphores: []Semaphore{{Pool: "{{ .Params.env", Capacity: 1}}}
	_, err := ExpandConcurrency(c, map[string]string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "concurrency.semaphores[0].pool")
}

func TestExpandTemplate_StepOutputs(t *testing.T) {
	data := TemplateData{
		Steps: map[string]StepData{
			"build": {Outputs: map[string]string{"artifact_url": "s3://bucket/a.tar.gz"}},
		},
	}
	result, err := ExpandTemplate("artifact: {{ .Steps.build.Outputs.artifact_url }}", data)
	require.NoError(t, err)
	assert.Equal(t, "artifact: s3://bucket/a.tar.gz", result)
}

func TestExpandTemplate_GrepCut(t *testing.T) {
	data := TemplateData{
		Stdout: "INFO: starting\nARTIFACT=s3://bucket/a.tar.gz\nINFO: done",
	}
	result, err := ExpandTemplate(`{{ .Stdout | grep "ARTIFACT=" | cut "=" 2 | trim }}`, data)
	require.NoError(t, err)
	assert.Equal(t, "s3://bucket/a.tar.gz", result)
}

func TestExpandTemplate_GrepNoMatch(t *testing.T) {
	data := TemplateData{Stdout: "no match here"}
	result, err := ExpandTemplate(`{{ .Stdout | grep "ARTIFACT=" | trim }}`, data)
	require.NoError(t, err)
	assert.Equal(t, "", result)
}

func TestExpandTemplate_Trim(t *testing.T) {
	data := TemplateData{Stdout: "  hello world  "}
	result, err := ExpandTemplate(`{{ .Stdout | trim }}`, data)
	require.NoError(t, err)
	assert.Equal(t, "hello world", result)
}

func TestExpandTemplate_MissingKeyReturnsEmpty(t *testing.T) {
	data := TemplateData{Params: map[string]string{}}
	result, err := ExpandTemplate("{{ .Params.missing }}", data)
	require.NoError(t, err)
	assert.Equal(t, "", result)
}

func TestExpandTemplate_MultilineGrep(t *testing.T) {
	data := TemplateData{
		Stdout: "building...\nVERSION=1.2.3\ndone",
	}
	result, err := ExpandTemplate(`{{ .Stdout | grep "VERSION=" | cut "=" 2 | trim }}`, data)
	require.NoError(t, err)
	assert.Equal(t, "1.2.3", result)
}

func TestHashFile_ReturnsHexHash(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "lock.json"), []byte(`{"version":1}`), 0o644))
	t.Chdir(dir)

	result, err := ExpandTemplate(`{{ hashFile "lock.json" }}`, TemplateData{})
	require.NoError(t, err)
	assert.Len(t, result, 64) // SHA-256 hex = 64 chars
}

func TestHashFile_ChangesOnFileModify(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "lock.json")
	require.NoError(t, os.WriteFile(f, []byte("v1"), 0o644))
	t.Chdir(dir)

	h1, err := ExpandTemplate(`{{ hashFile "lock.json" }}`, TemplateData{})
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(f, []byte("v2"), 0o644))
	h2, err := ExpandTemplate(`{{ hashFile "lock.json" }}`, TemplateData{})
	require.NoError(t, err)

	assert.NotEqual(t, h1, h2)
}

func TestHashFile_NoMatchReturnsEmpty(t *testing.T) {
	t.Chdir(t.TempDir())
	result, err := ExpandTemplate(`{{ hashFile "nonexistent.json" }}`, TemplateData{})
	require.NoError(t, err)
	assert.Equal(t, "", result)
}

func TestExpandConcurrency_ExpandsOrLockInButNotName(t *testing.T) {
	c := &Concurrency{
		OrLocks: []OrLock{
			// Name looks like a template to prove it is left untouched while In is evaluated.
			{Name: "{{ .Params.notexpanded }}", In: ForeachSource{Expr: "$envs"}},
		},
	}
	result, err := ExpandConcurrency(c, map[string]string{"envs": `["env-a","env-b"]`, "notexpanded": "should-not-appear"})
	require.NoError(t, err)
	require.Len(t, result.OrLocks, 1)
	assert.Equal(t, "{{ .Params.notexpanded }}", result.OrLocks[0].Name, "OrLock.Name must never be template-expanded")
	assert.Equal(t, []string{"env-a", "env-b"}, result.OrLocks[0].In.Literal)
}

func TestExpandConcurrency_OrLocksDoesNotMutateInput(t *testing.T) {
	c := &Concurrency{
		OrLocks: []OrLock{{Name: "env", In: ForeachSource{Expr: "$envs"}}},
	}
	_, err := ExpandConcurrency(c, map[string]string{"envs": `["prod"]`})
	require.NoError(t, err)
	assert.Equal(t, "$envs", c.OrLocks[0].In.Expr, "input In must not be mutated")
}

func TestExpandConcurrency_PropagatesOrLockInError(t *testing.T) {
	c := &Concurrency{
		OrLocks: []OrLock{{Name: "env", In: ForeachSource{Expr: "$missing"}}},
	}
	_, err := ExpandConcurrency(c, map[string]string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "concurrency.orLocks[0].in")
}

func TestExpandConcurrency_OrLockInParamExpr(t *testing.T) {
	c := &Concurrency{
		OrLocks: []OrLock{{Name: "env", In: ForeachSource{Expr: "$envs"}}},
	}
	result, err := ExpandConcurrency(c, map[string]string{"envs": `["prod","staging"]`})
	require.NoError(t, err)
	assert.Equal(t, []string{"prod", "staging"}, result.OrLocks[0].In.Literal)
}

func TestFuncMap_Concat(t *testing.T) {
	data2 := TemplateData{Params: map[string]string{"envs": "prod,staging"}}
	items, err := EvalForeachSource(ForeachSource{Expr: `{{ concat (.Params.envs | split ",") (list "fallback") }}`}, data2)
	require.NoError(t, err)
	assert.Equal(t, []string{"prod", "staging", "fallback"}, items)
}

func TestExpandTemplate_SecretsNoDot(t *testing.T) {
	data := TemplateData{Secrets: map[string]string{"gitlab-token": "glpat-secret"}}
	result, err := ExpandTemplate(`{{ secrets.gitlab-token }}`, data)
	require.NoError(t, err)
	assert.Equal(t, "glpat-secret", result)
}

func TestExpandTemplate_SecretsDotForm(t *testing.T) {
	data := TemplateData{Secrets: map[string]string{"gitlab-token": "glpat-secret"}}
	result, err := ExpandTemplate(`{{ .Secrets.gitlab-token }}`, data)
	require.NoError(t, err)
	assert.Equal(t, "glpat-secret", result)
}

func TestExpandTemplate_SecretsUnderscoreName(t *testing.T) {
	data := TemplateData{Secrets: map[string]string{"AWS_KEY": "AKIAIOSFODNN7"}}
	result, err := ExpandTemplate(`{{ secrets.AWS_KEY }}`, data)
	require.NoError(t, err)
	assert.Equal(t, "AKIAIOSFODNN7", result)
}

func TestExpandTemplate_SecretsMissing(t *testing.T) {
	data := TemplateData{Secrets: map[string]string{}}
	result, err := ExpandTemplate(`{{ secrets.missing-key }}`, data)
	require.NoError(t, err)
	assert.Equal(t, "", result)
}

func TestExpandTemplate_Split(t *testing.T) {
	data := TemplateData{Params: map[string]string{"envs": "prod,staging,dev"}}
	result, err := ExpandTemplate(`{{ index (.Params.envs | split ",") 1 }}`, data)
	require.NoError(t, err)
	assert.Equal(t, "staging", result)
}

func TestExpandTemplate_ForeachContext(t *testing.T) {
	data := TemplateData{
		Foreach: map[string]string{"env": "prod"},
	}
	result, err := ExpandTemplate("deploy to {{ .Foreach.env }}", data)
	require.NoError(t, err)
	assert.Equal(t, "deploy to prod", result)
}
