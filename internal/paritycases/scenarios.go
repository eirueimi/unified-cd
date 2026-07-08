package paritycases

import (
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// Cases returns the shared DSL-conformance scenarios. Each Case's Claim
// func must be called independently by each driver (host, k8s) so the two
// runs never share mutable ClaimResponse state.
func Cases() []Case {
	return []Case{
		ifSkipsStep(),
		envReachesScript(),
		continueOnError(),
		finallyRunsOnFailure(),
		postHooksLIFO(),
		matrixVariants(),
		secretsResolveAndMask(),
		stepTimeoutFails(),
		stdoutOutputs(),
		callSucceedsWithLink(),
		cacheEmptyKeySkips(),
		matrixStructuralErrorAborts(),
		isolatedDispatch(),
	}
}

// 1. if-skips-step: a step with `if: "false"` must be reported Skipped (never
// run), while a prior unconditional step succeeds normally, and the overall
// run still finishes Succeeded (a Skipped step is not a failure).
func ifSkipsStep() Case {
	return Case{
		Name: "if-skips-step",
		Claim: func() api.ClaimResponse {
			return api.ClaimResponse{
				RunID:   "run-if-skips-step",
				JobName: "if-skips-step",
				Native:  true,
				Stages: []api.ClaimStage{
					{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "first", Run: "echo first-ran"}},
					{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "second", If: "false", Run: "echo should-not-run"}},
				},
			}
		},
		Expect: Expectation{
			StepStatus: map[string]string{
				"first":  "Succeeded",
				"second": "Skipped",
			},
			RunFinished: "Succeeded",
		},
	}
}

// 2. env-reaches-script: a step-level `env:` entry must be visible to the
// script's environment, and UNIFIED_AGENT_OS must be injected. This is now
// done once by the shared orchestration loop (agentlib.RunClaim,
// internal/agent/orchestrator.go) via agentOSForStep(step, b.DefaultAgentOS())
// for every non-scoped/non-image step, regardless of which agent's
// ExecBackend is plugged in. The OS value legitimately differs between
// drivers (host: runtime.GOOS: "windows" here; k8s: "linux", hardcoded via
// k8sBackend.DefaultAgentOS for pod-exec steps) so it is asserted with a
// permissive regex (any non-whitespace value) rather than pinned to one OS
// string.
func envReachesScript() Case {
	return Case{
		Name: "env-reaches-script",
		Claim: func() api.ClaimResponse {
			return api.ClaimResponse{
				RunID:   "run-env-reaches-script",
				JobName: "env-reaches-script",
				Native:  true,
				Stages: []api.ClaimStage{
					{Step: &api.ClaimStep{
						Index: 0, StageIndex: 0, Name: "envstep",
						Env: map[string]string{"FOO": "bar"},
						Run: "echo got=$FOO && echo os=$UNIFIED_AGENT_OS",
					}},
				},
			}
		},
		Expect: Expectation{
			StepStatus:  map[string]string{"envstep": "Succeeded"},
			RunFinished: "Succeeded",
			LogMustContain: []LogLine{
				{Step: "envstep", Stream: "stdout", Substring: "got=bar"},
			},
			// LogMustMatch: Substring here is a REGEX pattern (not a literal
			// substring) matched against the full captured line via
			// regexp.MatchString. Used because the concrete OS value
			// legitimately differs between the host and k8s drivers.
			LogMustMatch: []LogLine{
				{Step: "envstep", Stream: "stdout", Substring: `os=\S+`},
			},
		},
	}
}

// 3. continue-on-error: a step that fails with `continueOnError: true` must
// not block later steps, and — per docs/jobs.md ("Continue on Error": "run
// will continue even if this step fails") and both agents' recordFailure
// (early-returns on step.ContinueOnError before ever setting the
// run-failed flag) — the run finishes Succeeded overall, since no
// non-continueOnError step failed.
func continueOnError() Case {
	return Case{
		Name: "continue-on-error",
		Claim: func() api.ClaimResponse {
			return api.ClaimResponse{
				RunID:   "run-continue-on-error",
				JobName: "continue-on-error",
				Native:  true,
				Stages: []api.ClaimStage{
					{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "flaky", Run: "exit 1", ContinueOnError: true}},
					{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "after", Run: "echo after-ran"}},
				},
			}
		},
		Expect: Expectation{
			StepStatus: map[string]string{
				"flaky": "Failed",
				"after": "Succeeded",
			},
			RunFinished: "Succeeded",
		},
	}
}

// 4. finally-runs-on-failure: a main-stage step fails; a `finally:` step must
// still run (and its marker must be shipped), and the run finishes Failed.
func finallyRunsOnFailure() Case {
	return Case{
		Name: "finally-runs-on-failure",
		Claim: func() api.ClaimResponse {
			return api.ClaimResponse{
				RunID:   "run-finally-runs-on-failure",
				JobName: "finally-runs-on-failure",
				Native:  true,
				Stages: []api.ClaimStage{
					{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "boom", Run: "exit 1"}},
				},
				Finally: []api.ClaimStage{
					{Step: &api.ClaimStep{Index: 1, StageIndex: 0, Name: "cleanup", Run: "echo FINALLY_MARKER"}},
				},
			}
		},
		Expect: Expectation{
			StepStatus: map[string]string{
				"boom":    "Failed",
				"cleanup": "Succeeded",
			},
			RunFinished: "Failed",
			LogMustContain: []LogLine{
				{Step: "cleanup", Stream: "stdout", Substring: "FINALLY_MARKER"},
			},
		},
	}
}

// 5. post-hooks-lifo: two succeeding steps each declare a `post:` hook; the
// hooks must drain in LIFO order (step2's post fires before step1's post)
// after the main DAG completes.
//
// IMPORTANT: neither agent ships a post: hook's stdout/stderr through the log
// pipeline — the host's drain calls RunStepCapture(hookCtx, cmd, nil, ...)
// (internal/agent/agent.go: stderr writer is nil, and the returned stdout is
// never forwarded to a LogPusher), and the k8s drain's postExec calls
// a.exec.ExecStep(..., io.Discard, io.Discard) (internal/k8sagent/agent.go).
// So this case cannot be verified via Expectation.LogMustContain/Logs at all;
// LogMustContain is intentionally left empty here. Instead each driver's test
// must independently capture post-hook invocation order via its own fake
// (host: wrap RunStepCapture's call site is not feasible without touching
// production code, so the host driver instead gives each post script a
// side-effect the test can observe out-of-band — e.g. writing a marker file
// into the run's workDir — and asserts the file-write order; k8s: the fake
// postExec function the driver already supplies records (script, order)
// directly). See parity_host_test.go / parity_k8s_test.go for exactly how
// each driver observes and asserts the LIFO order for this case.
func postHooksLIFO() Case {
	return Case{
		Name: "post-hooks-lifo",
		Claim: func() api.ClaimResponse {
			return api.ClaimResponse{
				RunID:   "run-post-hooks-lifo",
				JobName: "post-hooks-lifo",
				Native:  true,
				Stages: []api.ClaimStage{
					{Step: &api.ClaimStep{
						Index: 0, StageIndex: 0, Name: "step1", Run: "echo step1-ran",
						Post: &api.PostStep{Run: "echo post-1 >> \"$POSTHOOK_MARKER_FILE\""},
					}},
					{Step: &api.ClaimStep{
						Index: 1, StageIndex: 1, Name: "step2", Run: "echo step2-ran",
						Post: &api.PostStep{Run: "echo post-2 >> \"$POSTHOOK_MARKER_FILE\""},
					}},
				},
			}
		},
		Expect: Expectation{
			StepStatus: map[string]string{
				"step1": "Succeeded",
				"step2": "Succeeded",
			},
			RunFinished: "Succeeded",
		},
	}
}

// 6. matrix-variants: a single step with a 2-value matrix dimension expands
// into 2 variants, each succeeding independently. Asserted set-wise (map
// comparison) rather than by sequence, since the host may run matrix
// variants concurrently (runParallel) while the k8s agent runs them
// sequentially — both are valid expansions of the same DAG, order is not
// part of the contract here.
func matrixVariants() Case {
	return Case{
		Name: "matrix-variants",
		Claim: func() api.ClaimResponse {
			return api.ClaimResponse{
				RunID:   "run-matrix-variants",
				JobName: "matrix-variants",
				Native:  true,
				Stages: []api.ClaimStage{
					{Step: &api.ClaimStep{
						Index: 0, StageIndex: 0, Name: "build", Run: "echo building-{{ .Matrix.version }}",
						Matrix: &api.ClaimMatrixDef{Dimensions: []api.ClaimMatrixDimension{
							{Name: "version", Source: api.ClaimForeachSource{Literal: []string{"a", "b"}}},
						}},
					}},
				},
			}
		},
		Expect: Expectation{
			StepStatus: map[string]string{
				"build@a": "Succeeded",
				"build@b": "Succeeded",
			},
			RunFinished: "Succeeded",
		},
	}
}

// 7. secrets-resolve-and-mask: a step references {{ .Secrets.TOKEN }}; the
// raw secret value must reach the script (run succeeds) but must NEVER
// appear in any shipped log line — the masker (internal/secrets.Masker,
// replacement "***", see masker.go) must replace it before the line reaches
// the controller.
func secretsResolveAndMask() Case {
	return Case{
		Name: "secrets-resolve-and-mask",
		Claim: func() api.ClaimResponse {
			return api.ClaimResponse{
				RunID:         "run-secrets-resolve-and-mask",
				JobName:       "secrets-resolve-and-mask",
				Native:        true,
				SecretsNeeded: []string{"TOKEN"},
				Stages: []api.ClaimStage{
					{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "leaky", Run: "echo tok={{ .Secrets.TOKEN }}"}},
				},
			}
		},
		Secrets: map[string]string{"TOKEN": "s3cr3t-value"},
		Expect: Expectation{
			StepStatus:        map[string]string{"leaky": "Succeeded"},
			RunFinished:       "Succeeded",
			LogMustNotContain: []string{"s3cr3t-value"},
			LogMustContain: []LogLine{
				// The masker replaces the entire matched secret token with the
				// literal "***" (see internal/secrets/masker.go: Mask
				// replaces each registered pattern with "***"), so the shipped
				// line reads "tok=***".
				{Step: "leaky", Stream: "stdout", Substring: "tok=***"},
			},
		},
	}
}

// 8. step-timeout-fails: a step with a ~1.2s timeout runs `sleep 10`, which
// must be interrupted and reported Failed well before the sleep would
// naturally complete; the run finishes Failed. The driver (not this data
// file) additionally asserts wall-clock duration < 8s around the
// executeRun/orchestrate call.
func stepTimeoutFails() Case {
	return Case{
		Name: "step-timeout-fails",
		Claim: func() api.ClaimResponse {
			return api.ClaimResponse{
				RunID:   "run-step-timeout-fails",
				JobName: "step-timeout-fails",
				Native:  true,
				Stages: []api.ClaimStage{
					{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "slow", Run: "sleep 10", TimeoutMinutes: 0.02}},
				},
			}
		},
		Expect: Expectation{
			StepStatus:  map[string]string{"slow": "Failed"},
			RunFinished: "Failed",
		},
	}
}

// 9. stdout-outputs: a step captures `{{ .Stdout }}` into an output key; the
// recorded SetStepOutputs value must contain the printed text ("hello").
// Substring (not exact) match, since captured stdout may carry trailing
// whitespace/newlines the template preserves verbatim.
func stdoutOutputs() Case {
	return Case{
		Name: "stdout-outputs",
		Claim: func() api.ClaimResponse {
			return api.ClaimResponse{
				RunID:   "run-stdout-outputs",
				JobName: "stdout-outputs",
				Native:  true,
				Stages: []api.ClaimStage{
					{Step: &api.ClaimStep{
						Index: 0, StageIndex: 0, Name: "printer", Run: "printf hello",
						Outputs: map[string]string{"val": "{{ .Stdout }}"},
					}},
				},
			}
		},
		Expect: Expectation{
			StepStatus:  map[string]string{"printer": "Succeeded"},
			RunFinished: "Succeeded",
			Outputs: map[string]map[string]string{
				"printer": {"val": "hello"},
			},
		},
	}
}

// 10. call-succeeds-with-link: a `call:` step launches a child run; the fake
// controller's CreateRun returns a fixed child id, GetRun reports it
// Succeeded, GetRunOutputs returns empty. The step must succeed and its
// terminal StepReport must carry ChildRunID == the fixed id (both agents
// share ExecuteCallStep/callstep.go for this, so they cannot diverge on the
// wire contract — this case pins that shared behavior from both call sites).
func callSucceedsWithLink() Case {
	const childRunID = "child-run-123"
	return Case{
		Name: "call-succeeds-with-link",
		Claim: func() api.ClaimResponse {
			return api.ClaimResponse{
				RunID:   "run-call-succeeds-with-link",
				JobName: "call-succeeds-with-link",
				Native:  true,
				Stages: []api.ClaimStage{
					{Step: &api.ClaimStep{
						Index: 0, StageIndex: 0, Name: "callChild",
						Call: &api.ClaimCallStep{Job: "child-job"},
					}},
				},
			}
		},
		Expect: Expectation{
			StepStatus:  map[string]string{"callChild": "Succeeded"},
			RunFinished: "Succeeded",
		},
	}
}

// ChildRunIDFixture is the fixed child run id both drivers' fake CreateRun
// must return for the call-succeeds-with-link case (exported so both driver
// test files can wire their fake CreateRun/GetRun endpoints to the same
// constant without duplicating a magic string).
const ChildRunIDFixture = "child-run-123"

// 11. cache-empty-key-skips: a cache step whose `path` template expands
// SUCCESSFULLY to an empty string (Params.novalue == "") must not fail the
// step or the run — per the approved spec, template expansion succeeding but
// yielding an empty key/path is warn+skip (cache operation skipped), not a
// hard failure, on BOTH agents. k8s already implements this (see
// internal/k8sagent/agent.go's empty-key/empty-path branches); this case
// pins the host agent to the same behavior. Pre-fix, the host agent's
// executeCacheStep hard-failed the step when the expanded path was empty
// (internal/agent/agent.go: `if cachePath == "" { return fmt.Errorf(...) }`),
// which is the actual pre-fix drift this case targets — a fixed non-empty
// literal path would not exercise that branch. A second step then runs to
// confirm the pipeline continues normally past the cache step.
func cacheEmptyKeySkips() Case {
	return Case{
		Name: "cache-empty-key-skips",
		Claim: func() api.ClaimResponse {
			return api.ClaimResponse{
				RunID:   "run-cache-empty-key-skips",
				JobName: "cache-empty-key-skips",
				Native:  true,
				Params:  map[string]string{"novalue": ""},
				Stages: []api.ClaimStage{
					{Step: &api.ClaimStep{
						Index: 0, StageIndex: 0, Name: "cacheit",
						Cache: &dsl.CacheStep{Key: "some-key", Path: "{{ .Params.novalue }}"},
					}},
					{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "after", Run: "echo after-cache"}},
				},
			}
		},
		Expect: Expectation{
			StepStatus: map[string]string{
				"cacheit": "Succeeded",
				"after":   "Succeeded",
			},
			RunFinished: "Succeeded",
			LogMustContain: []LogLine{
				{Step: "after", Stream: "stdout", Substring: "after-cache"},
			},
		},
	}
}

// 12. matrix-structural-error-aborts: a step whose matrix dimension carries a
// malformed template expression fails EXPANSION (not execution). The shared
// RunPipeline contract — established on the host and adopted by the k8s agent
// in the shared-orchestrator refactor (TODO #44 stage 2, task 5) — is that a
// structural expansion error aborts ALL remaining steps/stages of that
// pipeline immediately (the pre-refactor k8s loop instead skipped the broken
// step and kept going). The later step's marker must therefore never be
// executed or shipped, and the run must finish Failed.
func matrixStructuralErrorAborts() Case {
	return Case{
		Name: "matrix-structural-error-aborts",
		Claim: func() api.ClaimResponse {
			return api.ClaimResponse{
				RunID:   "run-matrix-structural-error",
				JobName: "matrix-structural-error",
				Native:  true,
				Stages: []api.ClaimStage{
					{Step: &api.ClaimStep{
						Index: 0, StageIndex: 0, Name: "broken",
						Run: "echo broken-ran",
						Matrix: &api.ClaimMatrixDef{Dimensions: []api.ClaimMatrixDimension{
							// Unclosed template action: EvalForeachSource fails to
							// parse it, so ExpandMatrixStep returns an error before
							// any variant exists.
							{Name: "v", Source: api.ClaimForeachSource{Expr: "{{ .Params.envs"}},
						}},
					}},
					{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "later", Run: "echo never-ran-marker"}},
				},
			}
		},
		Expect: Expectation{
			RunFinished: "Failed",
			// Neither the broken step's body nor anything after it may run.
			LogMustNotContain: []string{"broken-ran", "never-ran-marker"},
		},
	}
}

// 13. isolated dispatch: this is the one case that deliberately leaves
// Native unset (Native == false), so BOTH backends run it through the
// isolated/job-container path rather than the native host-process path. It
// proves the two backends dispatch identically: a default step (no
// container:) lands in the primary "job" container, while a `container: X`
// step lands in the named container X, and each step's stdout still flows
// through the log pipeline. The observable log/status contract lives here in
// the shared Expectation; each driver additionally asserts its own dispatch
// TARGET (host: the exec'd claim-pod container handle; k8s: the exec'd
// container name) in its test file, since the concrete dispatch surface
// differs per backend even though the semantics are identical.
func isolatedDispatch() Case {
	return Case{
		Name: "isolated dispatch: default step hits the job container, container: hits the named one",
		Claim: func() api.ClaimResponse {
			return api.ClaimResponse{
				RunID: "run-isolated", JobName: "isolated-job",
				PodTemplate: &dsl.PodTemplate{Spec: map[string]any{
					"containers": []any{
						map[string]any{"name": "tools", "image": "busybox"},
					},
				}},
				Stages: []api.ClaimStage{
					{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "main",
						Run: "echo from-primary"}},
					{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "side",
						Container: "tools", Run: "echo from-tools"}},
				},
			}
		},
		Expect: Expectation{
			StepStatus:  map[string]string{"main": "Succeeded", "side": "Succeeded"},
			RunFinished: "Succeeded",
			LogMustContain: []LogLine{
				{Step: "main", Stream: "stdout", Substring: "from-primary"},
				{Step: "side", Stream: "stdout", Substring: "from-tools"},
			},
		},
	}
}
