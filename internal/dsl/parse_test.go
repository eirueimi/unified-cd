package dsl

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse_MinimalJob(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: hello
spec:
  steps:
    - name: greet
      run: echo hello
`
	job, err := Parse(strings.NewReader(input))
	require.NoError(t, err)
	assert.Equal(t, "unified-cd/v1", job.APIVersion)
	assert.Equal(t, "Job", job.Kind)
	assert.Equal(t, "hello", job.Metadata.Name)
	require.Len(t, job.Spec.Steps, 1)
	assert.Equal(t, "greet", job.Spec.Steps[0].Name)
	assert.Equal(t, "echo hello", job.Spec.Steps[0].Run)
}

func TestParse_RejectsWrongAPIVersion(t *testing.T) {
	input := `
apiVersion: wrong/v1
kind: Job
metadata:
  name: x
spec:
  steps:
    - name: s
      run: echo x
`
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "apiVersion")
}

func TestParse_RejectsMissingName(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
spec:
  steps:
    - name: s
      run: echo x
`
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "metadata.name")
}

func TestParse_RejectsInvalidNameFormat(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: My_Job
spec:
  steps:
    - name: s
      run: echo x
`
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "metadata.name is invalid")
}

func TestParse_RejectsEmptySteps(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: x
spec:
  steps: []
`
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "steps")
}

func TestParse_ParsesParams(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: with-params
spec:
  params:
    inputs:
      - name: target
        type: string
        required: true
      - name: dry_run
        type: bool
        default: false
  steps:
    - name: s
      run: echo hello
`
	job, err := Parse(strings.NewReader(input))
	require.NoError(t, err)
	require.Len(t, job.Spec.Params.Inputs, 2)
	assert.Equal(t, "target", job.Spec.Params.Inputs[0].Name)
	assert.Equal(t, "string", job.Spec.Params.Inputs[0].Type)
	assert.True(t, job.Spec.Params.Inputs[0].Required)
}

func TestParse_StepOutputs(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: build
spec:
  params:
    outputs:
      - name: artifact_url
        type: string
  steps:
    - name: build
      run: make build && echo "ARTIFACT=s3://bucket/a.tar.gz"
      outputs:
        artifact_url: '{{ .Stdout | grep "ARTIFACT=" | cut "=" 2 | trim }}'
`
	job, err := Parse(strings.NewReader(input))
	require.NoError(t, err)
	require.Len(t, job.Spec.Params.Outputs, 1)
	assert.Equal(t, "artifact_url", job.Spec.Params.Outputs[0].Name)
	assert.Equal(t, "string", job.Spec.Params.Outputs[0].Type)
	require.Len(t, job.Spec.Steps, 1)
	assert.Equal(t, `{{ .Stdout | grep "ARTIFACT=" | cut "=" 2 | trim }}`, job.Spec.Steps[0].Outputs["artifact_url"])
}

func TestParse_ConcurrencyMutex(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: deploy
spec:
  concurrency:
    mutex: deploy-prod
  steps:
    - name: deploy
      run: ./deploy.sh
`
	job, err := Parse(strings.NewReader(input))
	require.NoError(t, err)
	require.NotNil(t, job.Spec.Concurrency)
	assert.Equal(t, "deploy-prod", job.Spec.Concurrency.Mutex)
}

func TestParse_ConcurrencySemaphores(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: test
spec:
  concurrency:
    semaphores:
      - pool: deploy-tokens
        capacity: 3
  steps:
    - name: run
      run: ./test.sh
`
	job, err := Parse(strings.NewReader(input))
	require.NoError(t, err)
	require.NotNil(t, job.Spec.Concurrency)
	require.Len(t, job.Spec.Concurrency.Semaphores, 1)
	assert.Equal(t, "deploy-tokens", job.Spec.Concurrency.Semaphores[0].Pool)
	assert.Equal(t, 3, job.Spec.Concurrency.Semaphores[0].Capacity)
}

func TestParse_CallStep(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: parent
spec:
  steps:
    - name: deploy
      call:
        job: deploy-runner
        with:
          target: "{{ .Params.target_env }}"
`
	job, err := Parse(strings.NewReader(input))
	require.NoError(t, err)
	require.Len(t, job.Spec.Steps, 1)
	step := job.Spec.Steps[0]
	assert.Empty(t, step.Run)
	require.NotNil(t, step.Call)
	assert.Equal(t, "deploy-runner", step.Call.Job)
	assert.Equal(t, `{{ .Params.target_env }}`, step.Call.With["target"])
}

func TestParse_RejectsBothRunAndCall(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: x
spec:
  steps:
    - name: s
      run: echo x
      call:
        job: other
`
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "run")
}

func TestParse_RejectsNeitherRunNorCall(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: x
spec:
  steps:
    - name: s
`
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "run")
}

func TestParse_RejectsSemaphoreZeroCapacity(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: x
spec:
  concurrency:
    semaphores:
      - pool: tokens
        capacity: 0
  steps:
    - name: s
      run: echo x
`
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "capacity")
}

func TestParse_ConcurrencyOrLocks(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: test
spec:
  concurrency:
    orLocks:
      - name: env
        in: [env-a, env-b]
  steps:
    - name: run
      run: ./test.sh
`
	job, err := Parse(strings.NewReader(input))
	require.NoError(t, err)
	require.NotNil(t, job.Spec.Concurrency)
	require.Len(t, job.Spec.Concurrency.OrLocks, 1)
	assert.Equal(t, "env", job.Spec.Concurrency.OrLocks[0].Name)
	assert.Equal(t, []string{"env-a", "env-b"}, job.Spec.Concurrency.OrLocks[0].In.Literal)
}

func TestParse_OrLockInExpr(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: test
spec:
  concurrency:
    orLocks:
      - name: env
        in: $envs
  steps:
    - name: run
      run: ./test.sh
`
	job, err := Parse(strings.NewReader(input))
	require.NoError(t, err)
	assert.Equal(t, "$envs", job.Spec.Concurrency.OrLocks[0].In.Expr)
}

func TestParse_RejectsOrLockEmptyName(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: x
spec:
  concurrency:
    orLocks:
      - name: ""
        in: [a]
  steps:
    - name: s
      run: echo x
`
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "orLocks[0].name is required")
}

func TestParse_RejectsOrLockInvalidNameChars(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: x
spec:
  concurrency:
    orLocks:
      - name: "env-1"
        in: [a]
  steps:
    - name: s
      run: echo x
`
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `orLocks[0].name "env-1"`)
}

func TestParse_RejectsOrLockDuplicateName(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: x
spec:
  concurrency:
    orLocks:
      - name: env
        in: [a]
      - name: env
        in: [b]
  steps:
    - name: s
      run: echo x
`
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `orLocks[1].name "env" is duplicated`)
}

func TestParse_RejectsOrLockEmptyIn(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: x
spec:
  concurrency:
    orLocks:
      - name: env
  steps:
    - name: s
      run: echo x
`
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "orLocks[0].in is required")
}

func TestParse_AgentSelector(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: k8s-job
spec:
  agentSelector:
    - kind:kubernetes
    - pool:build
  steps:
    - name: build
      run: make build
`
	job, err := Parse(strings.NewReader(input))
	require.NoError(t, err)
	assert.Equal(t, []string{"kind:kubernetes", "pool:build"}, job.Spec.AgentSelector)
}

func TestParse_AgentSelectorEmpty(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: any-agent
spec:
  steps:
    - name: build
      run: make build
`
	job, err := Parse(strings.NewReader(input))
	require.NoError(t, err)
	assert.Empty(t, job.Spec.AgentSelector)
}

func TestParse_StepEnv(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: deploy
spec:
  steps:
    - name: deploy
      env:
        AWS_ACCESS_KEY_ID: "{{ secrets.AWS_KEY }}"
        REGION: us-east-1
      run: ./deploy.sh
`
	job, err := Parse(strings.NewReader(input))
	require.NoError(t, err)
	require.Len(t, job.Spec.Steps, 1)
	step := job.Spec.Steps[0]
	assert.Equal(t, `{{ secrets.AWS_KEY }}`, step.Env["AWS_ACCESS_KEY_ID"])
	assert.Equal(t, "us-east-1", step.Env["REGION"])
}

func TestParse_DuplicateStepName(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: x
spec:
  steps:
    - name: a
      run: echo a
    - name: a
      run: echo a again
`
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate")
}

func TestParse_CacheStep_Valid(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: test
spec:
  agentSelector: [kind:docker]
  steps:
    - name: restore
      cache:
        path: node_modules
        key: npm-abc
        restoreKeys: [npm-]
        ttlDays: 14
`
	job, err := Parse(strings.NewReader(input))
	require.NoError(t, err)
	// assert the cache step was parsed correctly
	require.Len(t, job.Spec.Steps, 1)
	step := job.Spec.Steps[0]
	require.NotNil(t, step.Cache)
	assert.Equal(t, "node_modules", step.Cache.Path)
	assert.Equal(t, "npm-abc", step.Cache.Key)
	assert.Equal(t, []string{"npm-"}, step.Cache.RestoreKeys)
	assert.Equal(t, 14, step.Cache.TTLDays)
}

func TestParse_CacheStep_MissingPath(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: test
spec:
  agentSelector: [kind:docker]
  steps:
    - name: bad
      cache:
        key: npm-abc
`
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cache.path")
}

func TestParse_CacheStep_MissingKey(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: test
spec:
  agentSelector: [kind:docker]
  steps:
    - name: bad
      cache:
        path: node_modules
`
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cache.key")
}

func TestParse_CacheStep_MultipleActions(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: test
spec:
  agentSelector: [kind:docker]
  steps:
    - name: bad
      run: echo hi
      cache:
        path: node_modules
        key: npm-abc
`
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only one of")
}

func TestParse_ContinueOnError(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: x
spec:
  steps:
    - name: a
      run: echo a
      continueOnError: true
`
	job, err := Parse(strings.NewReader(input))
	require.NoError(t, err)
	assert.True(t, job.Spec.Steps[0].ContinueOnError)
}

func TestParse_PostStep_Valid(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: test-post
spec:
  steps:
    - name: build
      run: echo building
      post:
        run: echo cleanup
    - name: deploy
      run: echo deploying
`
	job, err := Parse(strings.NewReader(input))
	require.NoError(t, err)
	require.NotNil(t, job.Spec.Steps[0].Post)
	assert.Equal(t, "echo cleanup", job.Spec.Steps[0].Post.Run)
	assert.Nil(t, job.Spec.Steps[1].Post)
}

func TestParse_PostStep_MissingRun(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: test-bad-post
spec:
  steps:
    - name: build
      run: echo building
      post:
        env:
          KEY: value
`
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "post.run")
}

func TestParse_PostStep_WithEnv(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: test-post-env
spec:
  steps:
    - name: build
      run: echo building
      post:
        run: echo cleanup
        env:
          CLEANUP_KEY: cleanup_val
`
	job, err := Parse(strings.NewReader(input))
	require.NoError(t, err)
	post := job.Spec.Steps[0].Post
	require.NotNil(t, post)
	assert.Equal(t, "cleanup_val", post.Env["CLEANUP_KEY"])
}

func TestParse_UsesGitURI_MissingRef(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: test-git-noref
spec:
  steps:
    - name: fetch
      uses:
        job: "git://github.com/org/repo/build.yaml"
`
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "@ref")
}

func TestParse_UsesGitURI_PathTraversal(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: test-git-traversal
spec:
  steps:
    - name: fetch
      uses:
        job: "git://github.com/org/repo/../../../etc/passwd@v1"
`
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "..")
}

func TestParse_UsesStep_RequiresGitURI(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: test-uses-plain-name
spec:
  steps:
    - name: fetch
      uses:
        job: some-registered-job-name
`
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "uses.job must be a git:// URI")
}

func TestParse_UsesStep_JobRequired(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: test-uses-empty-job
spec:
  steps:
    - name: fetch
      uses:
        job: ""
`
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "uses.job is required")
}

func TestParse_CallStep_RejectsGitURI(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: test-call-git-rejected
spec:
  steps:
    - name: fetch
      call:
        job: "git://github.com/org/repo/build.yaml@v1"
`
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `call.job no longer supports git:// URIs`)
}

func TestParse_Step_RunCallCacheUses_MutuallyExclusive(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: test-exclusive
spec:
  steps:
    - name: both
      run: echo hi
      uses:
        job: "git://github.com/org/repo/build.yaml@v1"
`
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only one of run, call, cache, uses")
}

func TestParse_CallStep_ArrayWith(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: parent
spec:
  steps:
    - name: checkout
      call:
        job: git-checkout
        with:
          url: "https://github.com/org/repo"
          sparse_paths:
            - src/
            - docs/
`
	job, err := Parse(strings.NewReader(input))
	require.NoError(t, err)
	step := job.Spec.Steps[0]
	require.NotNil(t, step.Call)
	assert.Equal(t, "https://github.com/org/repo", step.Call.With["url"])
	arr, ok := step.Call.With["sparse_paths"].([]any)
	require.True(t, ok)
	assert.Equal(t, []any{"src/", "docs/"}, arr)

	// WithAsStrings joins array values with newlines
	params := step.Call.WithAsStrings()
	assert.Equal(t, "https://github.com/org/repo", params["url"])
	assert.Equal(t, "src/\ndocs/", params["sparse_paths"])
}

func TestParse_ParallelBlock(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: x
spec:
  steps:
    - parallel:
      - name: a
        run: echo a
      - name: b
        run: echo b
    - name: c
      run: echo c
`
	job, err := Parse(strings.NewReader(input))
	require.NoError(t, err)
	require.Len(t, job.Spec.Steps, 2)
	assert.Len(t, job.Spec.Steps[0].Parallel, 2)
	assert.Equal(t, "a", job.Spec.Steps[0].Parallel[0].Name)
	assert.Equal(t, "c", job.Spec.Steps[1].Name)
}

func TestParse_NeedsFieldRejected(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: x
spec:
  steps:
    - name: a
      run: echo a
    - name: b
      run: echo b
      needs: [a]
`
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "needs")
}

func TestParse_FailFastFieldRejected(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: x
spec:
  failFast: true
  steps:
    - name: a
      run: echo a
`
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failFast")
}

func TestParse_ForeachLiteral(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: x
spec:
  steps:
    - name: deploy
      foreach:
        key: env
        in: [prod, staging]
      run: echo {{ .Foreach.env }}
`
	job, err := Parse(strings.NewReader(input))
	require.NoError(t, err)
	require.NotNil(t, job.Spec.Steps[0].Foreach)
	assert.Equal(t, "env", job.Spec.Steps[0].Foreach.Key)
	assert.Equal(t, []string{"prod", "staging"}, job.Spec.Steps[0].Foreach.Source.Literal)
}

func TestParse_ForeachExpr(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: x
spec:
  params:
    inputs:
      - name: envs
        type: array
  steps:
    - name: deploy
      foreach:
        key: env
        in: $envs
      run: echo {{ .Foreach.env }}
`
	job, err := Parse(strings.NewReader(input))
	require.NoError(t, err)
	assert.Equal(t, "$envs", job.Spec.Steps[0].Foreach.Source.Expr)
}

func TestParse_ParallelAndNameMutuallyExclusive(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: x
spec:
  steps:
    - name: bad
      parallel:
        - name: inner
          run: echo x
`
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parallel")
}

func TestParse_ForeachRequiresKey(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: x
spec:
  steps:
    - name: deploy
      foreach:
        in: [a, b]
      run: echo x
`
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "key")
}

func TestParse_InputTypeArray(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: x
spec:
  params:
    inputs:
      - name: envs
        type: array
  steps:
    - name: a
      run: echo a
`
	job, err := Parse(strings.NewReader(input))
	require.NoError(t, err)
	assert.Equal(t, "array", job.Spec.Params.Inputs[0].Type)
}

func TestParse_FinallyValid(t *testing.T) {
	y := `apiVersion: unified-cd/v1
kind: Job
metadata:
  name: with-finally
spec:
  steps:
    - name: build
      run: make build
  finally:
    - name: notify
      run: ./notify.sh
    - name: rollback
      if: failure()
      run: ./rollback.sh`
	job, err := Parse(strings.NewReader(y))
	require.NoError(t, err)
	require.Len(t, job.Spec.Finally, 2)
	assert.Equal(t, "notify", job.Spec.Finally[0].Name)
	assert.Equal(t, "failure()", job.Spec.Finally[1].If)
}

func TestParse_FinallyDuplicateNameAcrossStepsAndFinally(t *testing.T) {
	y := `apiVersion: unified-cd/v1
kind: Job
metadata:
  name: dup
spec:
  steps:
    - name: build
      run: make build
  finally:
    - name: build
      run: ./cleanup.sh`
	_, err := Parse(strings.NewReader(y))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate step name")
}

func TestParse_FinallyStepMissingAction(t *testing.T) {
	y := `apiVersion: unified-cd/v1
kind: Job
metadata:
  name: bad
spec:
  steps:
    - name: build
      run: make build
  finally:
    - name: cleanup`
	_, err := Parse(strings.NewReader(y))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "one of run, call, or uses is required")
}

func TestParse_FinallyForbidsNeeds(t *testing.T) {
	t.Run("top-level needs in finally", func(t *testing.T) {
		input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: x
spec:
  steps:
    - name: build
      run: make build
  finally:
    - name: notify
      run: ./notify.sh
      needs: [build]
`
		_, err := Parse(strings.NewReader(input))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "spec.finally")
		assert.Contains(t, err.Error(), "needs")
	})

	t.Run("needs inside finally parallel block", func(t *testing.T) {
		input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: x
spec:
  steps:
    - name: build
      run: make build
  finally:
    - parallel:
      - name: cleanup-a
        run: ./cleanup-a.sh
        needs: [build]
      - name: cleanup-b
        run: ./cleanup-b.sh
`
		_, err := Parse(strings.NewReader(input))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "spec.finally")
		assert.Contains(t, err.Error(), "needs")
	})
}

func TestParse_FinallyParallelBlock(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: x
spec:
  steps:
    - name: build
      run: make build
  finally:
    - parallel:
      - name: notify-slack
        run: ./notify-slack.sh
      - name: notify-email
        run: ./notify-email.sh
    - name: teardown
      run: ./teardown.sh
`
	job, err := Parse(strings.NewReader(input))
	require.NoError(t, err)
	require.Len(t, job.Spec.Finally, 2)
	assert.Len(t, job.Spec.Finally[0].Parallel, 2)
	assert.Equal(t, "notify-slack", job.Spec.Finally[0].Parallel[0].Name)
	assert.Equal(t, "notify-email", job.Spec.Finally[0].Parallel[1].Name)
	assert.Equal(t, "teardown", job.Spec.Finally[1].Name)
}

func TestParse_UsesStep_ArrayWith(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: parent
spec:
  steps:
    - name: buildWithTemplate
      uses:
        job: git://github.com/org/repo/build.yaml@v1.0.0
        with:
          url: "https://github.com/org/repo"
          sparse_paths:
            - src/
            - docs/
`
	job, err := Parse(strings.NewReader(input))
	require.NoError(t, err)
	step := job.Spec.Steps[0]
	require.NotNil(t, step.Uses)
	assert.Equal(t, "git://github.com/org/repo/build.yaml@v1.0.0", step.Uses.Job)
	assert.Equal(t, "https://github.com/org/repo", step.Uses.With["url"])

	params := step.Uses.WithAsStrings()
	assert.Equal(t, "https://github.com/org/repo", params["url"])
	assert.Equal(t, "src/\ndocs/", params["sparse_paths"])
}
