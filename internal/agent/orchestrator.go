package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/secrets"
)

// isTerminalRunStatus reports whether a run has finished at the controller.
// (Local copy: the k8s agent has its own in package k8sagent; controller/sse.go
// and cli/wait.go likewise keep their own — there is no shared exported form.)
func isTerminalRunStatus(s api.RunStatus) bool {
	switch s {
	case api.RunSucceeded, api.RunFailed, api.RunCancelled:
		return true
	default:
		return false
	}
}

// CancelPollInterval is how often RunClaim's cancellation poller asks the
// controller whether the run was cancelled mid-flight. It is an exported var
// (not a const) so tests — in this package and in the k8s agent, which used
// to own its own unexported cancelPollInterval before this loop was shared —
// can shorten it instead of waiting through a real 5s tick.
var CancelPollInterval = 5 * time.Second

// DefaultFinallyBudget is the fallback ceiling on each of RunClaim's
// post-DAG cleanup phases: the `finally:` pipeline itself, each of the two
// `post:`/`cache:` hook drains, and the deferred scope/pod teardown
// (CloseScopes). The rule is "every post-DAG phase that EXECUTES something
// carries this budget"; the single exception is the controller report
// (FinishRun/SetRunOutputs), which executes nothing of the job's and is
// deliberately unbounded — see the finishCtx note in RunClaim for why a
// ceiling there has nothing to hand off to.
//
// Four windows, therefore, in the worst case, each of phaseBudget: drain,
// `finally:`, drain, teardown — 40 minutes at the default. They are separate
// windows, not a shared total, because they are separated by the main DAG,
// which may legitimately run for hours; one window opened before the DAG
// would already have expired by the time cleanup began.
//
// All four are enforced on BOTH backends, which took work on the k8s side:
// hostBackend.CloseScopes threads its context through rt.Remove already, but
// k8sBackend.CloseScopes used to re-strip it (see that method) and so kept
// none of this promise. Do not add a post-DAG phase, on either backend, that
// executes something without a window — the numbers below are published to
// operators who size rollout grace periods against them.
//
// The Kubernetes agent adds a FIFTH window of the same size: its claim Pod is
// deleted or returned to the idle pool from runClaim's own defers, after this
// loop has returned — see k8sagent.K8sAgent.claimPodTeardownContext. The host
// agent has no equivalent, because hostBackend.CloseScopes tears its claim pod
// down inside window four.
//
// Why a ceiling exists at all. Those phases deliberately run on
// context.WithoutCancel(ctx) so that cleanup still happens when the run was
// cancelled or the job-level timeout fired — that is the entire point of
// `finally:`. But WithoutCancel strips the job-level deadline
// (spec.timeoutMinutes, applied to ctx at the top of RunClaim) along with the
// cancellation, and the resulting context's Done() channel is nil: a `select`
// on it blocks forever. A `call:` step in `finally:` with no per-step
// timeoutMinutes therefore polled its child run forever (ExecuteCallStep's
// only wait bound is ctx), and a plain `run:` was unbounded the same way.
// Nothing else caught it: the parent's cancelDescendantRuns only fires on
// FinishRun, which cannot be reached until `finally` returns, so a cancelled
// parent and a never-claimed child deadlocked each other; and the controller's
// stuck-run reaper keys on AGENT liveness, which stays healthy because the
// agent keeps heartbeating. The hook drains inherited the same
// unboundedness — and became reachable for `finally:`-registered hooks only
// with the two-drain fix, since RunPostHook takes no timeout of its own.
//
// The budget is deliberately NOT spec.timeoutMinutes: a six-hour job would get
// a six-hour finally, which bounds nothing. It is also not a DSL field —
// per-step `timeoutMinutes:` already works inside `finally:` and remains the
// precise tool. This is only the backstop for authors who set nothing.
//
// 10 minutes: cleanup work (a cache save, an artifact upload, a notification
// webhook, tearing down test infrastructure) is a seconds-to-minutes job. The
// closest existing ceiling in this repository, podStartTimeout, is 5m, but
// that bounds ONE infrastructure wait, whereas this bounds a whole
// author-written pipeline that may hold several steps and a `call:` into a
// teardown job — so it is set to twice that. Generous enough that no realistic
// cleanup trips it; short enough that a wedged phase surfaces within a single
// on-call attention span instead of pinning the agent's concurrency slot
// indefinitely.
const DefaultFinallyBudget = 10 * time.Minute

// FinallyBudget is the effective per-phase ceiling described on
// DefaultFinallyBudget. It is an exported var (like CancelPollInterval) rather
// than a parameter because RunClaim is the ONE orchestration loop shared by
// both agents, and each agent binary owns its own configuration type: the host
// agent sets it from `finallyTimeout` / UNIFIED_AGENT_FINALLY_TIMEOUT, the k8s
// agent from `finallyTimeout` / UNIFIED_K8S_FINALLY_TIMEOUT. Tests shrink it
// so a budget test asserts on the OUTCOME (the phase ended, the run reached a
// terminal status) without waiting out a real ten-minute deadline.
//
// A non-positive value falls back to DefaultFinallyBudget: "no ceiling" is the
// bug this exists to fix and is deliberately not expressible.
//
// The budget is PER PHASE, not a single shared total. Each drain, the
// `finally` pipeline, and the scope/pod teardown start their own window —
// four in all, so the worst-case post-DAG time is 4 × this value (40m at the
// default). They are separate because the phases are separated by the main
// DAG (which may legitimately run for hours), so one window opened before the
// DAG would already have expired by the time cleanup began. See
// DefaultFinallyBudget for the full accounting, including the k8s agent's
// fifth window.
var FinallyBudget = DefaultFinallyBudget

// finallyBudget reads FinallyBudget once, on the caller's own goroutine,
// applying the non-positive fallback. RunClaim calls it exactly once per claim
// for the same reason it snapshots CancelPollInterval: nothing downstream may
// touch package-level state from a goroutine that could outlive the claim.
func finallyBudget() time.Duration {
	if d := FinallyBudget; d > 0 {
		return d
	}
	return DefaultFinallyBudget
}

// maxTruncationAttributionNames caps how many SKIPPED items a truncation
// record enumerates by name before it summarises the rest as a count.
//
// The record goes into the run's own log, where it is read by a human, and a
// job may legitimately register hundreds of hooks (a large matrix, each
// combination with a `post:`); enumerating all of them would bury the one
// line that matters — the interrupted item, which is always named — under a
// wall of text and turn one useful record into a paging problem. Five names
// plus "(+N more)" keeps the common case (a handful of hooks) fully
// enumerated and the pathological case one line long. The interrupted item is
// deliberately outside this cap: there is at most one, and it is the single
// most important fact in the record.
const maxTruncationAttributionNames = 5

// truncationAttribution renders what a truncated hook drain cut off and what
// it never got to. Returns "" when there is nothing to attribute, which is the
// signal to recordPhaseTruncated to emit its generic sentence unchanged — that
// happens when the deadline lands between items with none left to run.
func truncationAttribution(interrupted string, skipped []string) string {
	var parts []string
	if interrupted != "" {
		parts = append(parts, "Interrupted: "+interrupted+".")
	}
	if len(skipped) > 0 {
		named := skipped
		extra := 0
		if len(named) > maxTruncationAttributionNames {
			extra = len(named) - maxTruncationAttributionNames
			named = named[:maxTruncationAttributionNames]
		}
		s := "Never started: " + strings.Join(named, ", ")
		if extra > 0 {
			s += fmt.Sprintf(" (+%d more)", extra)
		}
		parts = append(parts, s+".")
	}
	return strings.Join(parts, " ")
}

// retrySleep waits d honoring ctx (a var so tests run instantly). Returns
// ctx.Err() if the wait is cancelled.
var retrySleep = func(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// RunClaim is the single shared step-orchestration loop driving both the host
// and k8s agents. It owns: secrets fetch -> masker construction -> installing
// the masker on b (SetMasker) -> the cancellation poller -> per-step context
// (timeouts, if:, approval, cache/artifact/call/run dispatch via b) ->
// RunPipeline for the main DAG (concurrency mode decided by b) -> hook drain
// (deferred cache saves, then post: hooks LIFO) -> `finally` -> a second hook
// drain for whatever the finally steps registered -> job-output promotion ->
// FinishRun.
//
// b is the ONLY seam between this orchestration logic and a concrete
// execution environment (host process vs k8s pod); the loop itself never
// branches on which backend it is driving. This is what makes drift between
// the two agents' orchestration logic structurally impossible: there is only
// one copy of this logic.
//
// client/agentID identify who reports progress; c is the claimed run. Callers
// (the host and k8s executeRun wrappers) are responsible for everything
// backend-specific that must happen BEFORE this call: acquiring the execution
// environment (workDir / pod), constructing b, and any host/k8s-only handling
// (e.g. the host agent branches on c.Native: an isolated claim eagerly builds
// the claim pod that backs its default and container: steps, while a native
// claim runs default steps as host processes).
func RunClaim(ctx context.Context, client *Client, agentID string, c api.ClaimResponse, b ExecBackend) {
	slog.Info("running", "runId", c.RunID, "job", c.JobName)

	// Apply job-level timeout to the context if one is configured
	if c.TimeoutMinutes > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(c.TimeoutMinutes*float64(time.Minute)))
		defer cancel()
	}

	var cancelledByMaster atomic.Bool
	var reapedByMaster atomic.Bool // controller marked the run terminal (Failed/other) out-of-band
	// anyStepFailed: a non-continueOnError step failed (used for if: status).
	// Benign race: a step failing at the exact instant cancellation arrives may be
	// reported as Failed vs Cancelled, but both are terminal non-success — no corruption.
	var anyStepFailed atomic.Bool

	statusView := func() dsl.RunStatusView {
		cancelled := cancelledByMaster.Load()
		return dsl.RunStatusView{
			Failed:    anyStepFailed.Load() && !cancelled,
			Cancelled: cancelled,
		}
	}

	runCtx, cancelRun := context.WithCancel(ctx)

	// The cancel poller lives for the duration of the claim. Two things matter
	// for its lifecycle — the second is what keeps `go test -race` honest:
	//
	//  1. Read CancelPollInterval HERE, on RunClaim's own goroutine, not inside
	//     the poller. The poller then never touches package-level state.
	//  2. JOIN the poller before returning. In a fast run the whole DAG can
	//     finish and RunClaim can return before the scheduler has even run the
	//     poller's first line; without a join the goroutine would start late and
	//     read package state after RunClaim returned, racing a caller that
	//     mutates CancelPollInterval between runs (e.g. a test restoring it in
	//     Cleanup). It would also outlive the httptest server a test closes on
	//     teardown. Cancel first, then wait.
	pollInterval := CancelPollInterval
	// Snapshotted here for the same reason as pollInterval (see above): read
	// package state once, on RunClaim's own goroutine.
	phaseBudget := finallyBudget()
	var pollerWG sync.WaitGroup
	defer func() {
		cancelRun()
		pollerWG.Wait()
	}()

	pollerWG.Add(1)
	go func() {
		defer pollerWG.Done()
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				run, err := client.GetRun(runCtx, c.RunID)
				if err != nil {
					slog.Warn("cancel poller: get run failed", "runID", c.RunID, "error", err)
					continue
				}
				if isTerminalRunStatus(run.Status) {
					if run.Status == api.RunCancelled {
						slog.Info("received cancellation signal from master; interrupting run", "runID", c.RunID)
						cancelledByMaster.Store(true)
					} else {
						slog.Info("master reported run terminal; interrupting run", "runID", c.RunID, "status", run.Status)
						reapedByMaster.Store(true)
					}
					cancelRun()
					return
				}
			}
		}
	}()

	sctx := &safeStepCtx{
		data: dsl.TemplateData{
			Params: c.Params,
			Vars:   c.Vars,
			Steps:  map[string]dsl.StepData{},
		},
	}

	// Fetch the secrets needed for this Run and build the masker
	var masker *secrets.Masker
	if len(c.SecretsNeeded) > 0 {
		secretValues, err := client.FetchSecrets(ctx, agentID, c.RunID, c.SecretsNeeded)
		if err != nil {
			// Continuing with an empty map would run every step as though its
			// secrets were configured but empty — e.g. `curl -H "Authorization:
			// Bearer $TOKEN"` with no token — which either fails confusingly or
			// does the wrong thing silently. RunClaim has no *Agent/*K8sAgent to
			// call their failRun on (see agent.go's Agent.failRun and
			// k8sagent/agent.go's K8sAgent.failRun, both used for fatal
			// step-setup failures that happen before RunClaim starts), so this
			// inlines the same pattern: log the reason into the run's own logs
			// (stepIndex -1, "System" in the UI), then FinishRun(Failed), and
			// return without running the DAG. The cancel poller started above is
			// still torn down correctly by the deferred cancelRun/pollerWG.Wait().
			reason := fmt.Sprintf("fetch secrets for run %s: %v", c.RunID, err)
			slog.Error(reason, "runId", c.RunID)
			failCtx := context.WithoutCancel(ctx)
			_ = client.AppendLogBulk(failCtx, agentID, c.RunID, -1, []api.LogAppendRequest{{
				RunID:     c.RunID,
				StepIndex: -1,
				Stream:    "stderr",
				Timestamp: time.Now().UTC(),
				Line:      reason,
			}})
			retryUntilSuccess(failCtx, func(callCtx context.Context) error {
				return client.FinishRun(callCtx, agentID, c.RunID, api.RunFailed)
			})
			return
		}
		sctx.mu.Lock()
		sctx.data.Secrets = secretValues
		sctx.mu.Unlock()
		vals := make([]string, 0, len(secretValues))
		for _, v := range secretValues {
			vals = append(vals, v)
		}
		masker = secrets.NewMasker(vals)
	} else {
		masker = secrets.NoOpMasker
	}

	// recordPhaseTruncated reports a post-DAG cleanup phase that hit the
	// phaseBudget ceiling into the RUN's own logs (stepIndex -1, rendered as
	// "System" in the UI) — the same mechanism warnSkippedOutput below and the
	// secret-fetch failure above use.
	//
	// Why the run's log, and not only slog. A truncated phase is otherwise
	// invisible in the one place an operator actually looks. The hook drains
	// are the sharp case: a `post:`/`cache:` hook has never changed the run's
	// status whatever it fails on, so a multi-gigabyte cache save cut off at
	// the budget leaves the run reported Succeeded with the cache silently not
	// saved. The agent's slog is the wrong place to find that — it is on
	// another host, interleaved with every other concurrent run, and is not
	// what the person debugging THIS run has open. Trading "hangs forever" for
	// "quietly loses the cache" would not have been a fix.
	//
	// The context is deliberately neither ctx (which may be cancelled — that
	// is why these phases exist) nor the phase's own budget context, which is
	// expired by definition at every call site: the message about a deadline
	// must not itself be dropped by that deadline. It is unbounded for the
	// same reason finishCtx is (see the note there); AppendLogBulk is a single
	// non-retried call, so the exposure is one HTTP request, not a loop.
	//
	// attribution, when non-empty, names WHAT was cut off and what never
	// started (built by drainHooks; see truncationAttribution). Detecting the
	// truncation is only half the job: "the post:/cache: hook drain that
	// follows the main steps did not finish" leaves an operator with three
	// `cache:` steps no way to tell which cache is now stale, which is the one
	// question this record exists to answer.
	//
	// BOTH sinks get the masked text, the run's log and the agent's own slog.
	// A `cache: key:` may interpolate {{ .Secrets.X }}, so the attribution can
	// carry a secret; this call site does not otherwise pass through the masker
	// the way a step's log writers do. Masking one sink and not the other would
	// put in the process log exactly what was scrubbed from the run log — an
	// asymmetry nobody reading this would expect, and one that survives into
	// whatever ships the agent's stdout off the host. (executeCacheStep's save
	// closure does still slog the raw key; that is a pre-existing sink of the
	// same class, not a reason to add another.)
	recordPhaseTruncated := func(phase, attribution string) {
		line := fmt.Sprintf("unified-cd: %s did not finish: it hit the %s cleanup budget (finallyTimeout) and was stopped. "+
			"Work still in flight was interrupted and anything not yet started was skipped.", phase, phaseBudget)
		if attribution != "" {
			line += " " + attribution
		}
		line = masker.Mask(line)
		slog.Warn("cleanup phase truncated by its finallyTimeout budget",
			"runId", c.RunID, "phase", phase, "budget", phaseBudget, "attribution", masker.Mask(attribution))
		_ = client.AppendLogBulk(context.WithoutCancel(ctx), agentID, c.RunID, -1, []api.LogAppendRequest{{
			RunID:     c.RunID,
			StepIndex: -1,
			Stream:    "stderr",
			Timestamp: time.Now().UTC(),
			Line:      line,
		}})
	}

	// Register scope/pod/pump teardown BEFORE SetMasker starts the host
	// sidecar pump (SetMasker spawns `docker logs -f` goroutines when
	// b.pod != nil — see hostBackend.SetMasker). CloseScopes is nil-safe (it
	// guards sidecarPump/scopes/pod independently), so deferring it this
	// early is free today; the point is that ANY future early-return between
	// here and the pump's start can no longer skip teardown and leak the
	// pump's subprocesses/goroutines.
	defer func() {
		// Bounded, like every other post-DAG phase — see DefaultFinallyBudget
		// for the one-sentence rule (every phase that EXECUTES something has a
		// ceiling; only the controller report does not, see finishCtx below).
		// Scope/pod teardown executes container-runtime and Kubernetes API
		// calls and can wedge exactly as a cleanup hook can, and it runs on
		// context.WithoutCancel for the same reason, so it inherited the same
		// unboundedness.
		//
		// Unlike the other phases this one is NOT recorded into the run's log:
		// it runs after FinishRun has already made the run terminal, and what
		// leaks when it is cut short (a scope container, a claim pod) is a
		// property of the agent host, not of the job's result — the operator
		// who can act on it is reading the agent's log, not the run's.
		scopeCtx, cancelScopes := context.WithTimeout(context.WithoutCancel(ctx), phaseBudget)
		defer cancelScopes()
		b.CloseScopes(scopeCtx)
		if errors.Is(scopeCtx.Err(), context.DeadlineExceeded) {
			slog.Warn("scope/pod teardown truncated by its finallyTimeout budget; containers or a claim pod may have leaked",
				"runId", c.RunID, "budget", phaseBudget)
		}
	}()

	// The masker is born here (after secrets are fetched), so it is installed
	// via SetMasker rather than passed to the backend's constructor.
	b.SetMasker(masker)

	// warnSkippedOutput surfaces a dropped output both to the agent log and
	// into the run's own logs (stepIndex -1 renders as "System" in the UI).
	warnSkippedOutput := func(ctx context.Context, stepIndex int, key string) {
		slog.Warn("output skipped: value may contain a secret",
			"runId", c.RunID, "stepIndex", stepIndex, "key", key)
		_ = client.AppendLogBulk(ctx, agentID, c.RunID, stepIndex, []api.LogAppendRequest{{
			RunID:     c.RunID,
			StepIndex: stepIndex,
			Stream:    "stderr",
			Timestamp: time.Now().UTC(),
			Line:      fmt.Sprintf("output %q skipped: value may contain a secret", key),
		}})
	}

	// recordIfDiagnostic puts an if:-condition problem into the RUN's own log
	// (stepIndex -1, "System" in the UI) — the same mechanism the secret-fetch
	// failure, recordPhaseTruncated and warnSkippedOutput use.
	//
	// Why the run's log and not only slog. The two things that can go wrong
	// with an if: are both invisible otherwise AND both invert the author's
	// intent: a condition that fails to compile or evaluate is fail-safe (the
	// step RUNS), and a condition that reads an undefined params, vars, or
	// secrets key gates on an empty string. Either way the person looking at
	// the run sees a step that ran when it should not have (or a gate that
	// never matches) with nothing in the run to explain it — the agent's slog
	// is on another host, interleaved with every other concurrent run, and is
	// not what they have open. slog still gets the same message for the
	// operator's benefit.
	//
	// Deduplicated on the step's BASE name plus the message, so a 20-copy
	// matrix step with a broken condition contributes one line and not twenty
	// (every copy shares step.Name; only DisplayName carries the variant),
	// while two genuinely different steps that share the same broken
	// expression each get their own line. The map is written from the
	// concurrently-invoked step runner (parallel: groups and matrix copies run
	// as goroutines), hence the mutex.
	//
	// Masked for the same reason recordPhaseTruncated is: an if: may reference
	// secrets.X and the diagnostic quotes the expression back.
	//
	// The context is deliberately WithoutCancel: a cancelled run still needs
	// the record of why its steps did or did not run.
	var ifDiagMu sync.Mutex
	seenIfDiag := map[string]bool{}
	recordIfDiagnostic := func(step api.ClaimStep, msg string) {
		key := step.Name + "\x00" + msg
		ifDiagMu.Lock()
		dup := seenIfDiag[key]
		seenIfDiag[key] = true
		ifDiagMu.Unlock()
		if dup {
			return
		}
		// A CEL compile error is multi-line (it draws a caret under the
		// offending column); flattened so the run's log gets one line.
		line := masker.Mask(strings.Join(strings.Fields(
			fmt.Sprintf("unified-cd: step %q: %s", step.DisplayName(), msg)), " "))
		_ = client.AppendLogBulk(context.WithoutCancel(ctx), agentID, c.RunID, -1, []api.LogAppendRequest{{
			RunID:     c.RunID,
			StepIndex: -1,
			Stream:    "stderr",
			Timestamp: time.Now().UTC(),
			Line:      line,
		}})
	}

	// deferred hooks: run after RunPipeline completes (cache save, etc.)
	//
	// parallel: steps in the same claim run concurrently as goroutines under
	// Concurrent mode (see runParallel in pipeline.go), and both postHooks
	// (cache save, appended from executeCacheStep) and hookStack (post: hooks,
	// appended below in makeStepRunner) are appended from inside that
	// concurrently-invoked step runner. postHooksMu guards every append to
	// either slice so concurrent parallel-group members with a post:/cache:
	// don't race on the shared backing array. Both production backends now run
	// Concurrent (the k8s agent since 07b9d0d), and `finally:` steps go
	// through the very same runner, so finally-registered hooks are appended
	// concurrently too.
	//
	// drainHooks (below) is called once after the main DAG and again after
	// `finally`. It detaches both slices under postHooksMu and resets them to
	// nil in the same critical section, so it is safe to call repeatedly:
	// each hook is handed to exactly one drain, and the second call sees only
	// what `finally` added.
	var postHooksMu sync.Mutex
	var postHooks []deferredHook
	var hookStack []postHookEntry

	// scopes: one scope-tracking structure for the whole claim, created lazily
	// on first use by a uses-scope step (owned by b). Torn down at claim end
	// regardless of how the claim finished (success, failure, or
	// cancellation) — see the defer registered above, before SetMasker.

	getData := func() dsl.TemplateData { return sctx.snapshot() }

	// makeStepRunner builds the per-step execution function. It is reused for the
	// main DAG and for the finally block, parametrized by:
	//   statusFn        — supplies the RunStatusView used to evaluate if:
	//                     (live status for the main DAG, frozen status for finally)
	//   implicitSuccess — true for the main DAG (auto-skip after a failure),
	//                     false for finally (no-if steps always run)
	//   failedFlag      — set when a non-continueOnError step fails
	//   suppressOnCancel — true for the main DAG (cancellation does not count as a
	//                      failure), false for finally (a genuine finally failure
	//                      counts even when the run was cancelled)
	//
	// suppressOnCancel is the PHASE DISCRIMINATOR, and every place that asks
	// "does a master cancellation mask this step's failure?" must consult it
	// rather than cancelledByMaster alone. Three places do, and all three used
	// to consult cancelledByMaster globally — which silently defeated
	// suppressOnCancel=false for `run:`/`call:` finally steps:
	//
	//   1. recordFailure          — the flag that drives the run's status.
	//   2. the reported step status — a "Failed" rewritten to "Cancelled"
	//      showed an operator a clean cancellation where teardown had actually
	//      broken, and (because the failure is only recorded under
	//      `status == "Failed"`) skipped recordFailure entirely. The `cache:`
	//      and artifact branches call markFailed directly and were never
	//      affected, which is why the behaviour looked inconsistent from
	//      outside.
	//   3. the retry attempt loop  — `break` on cancellation degraded a
	//      `retry:` on a finally step to a single attempt, and suppressed the
	//      "step failed to execute" diagnostic, leaving a genuinely broken
	//      finally step with an empty log.
	makeStepRunner := func(statusFn func() dsl.RunStatusView, implicitSuccess bool, failedFlag *atomic.Bool, suppressOnCancel bool) func(context.Context, api.ClaimStep) error {
		return func(stepCtx context.Context, step api.ClaimStep) error {
			// cancelMasksFailure reports whether a master/user cancellation
			// should be treated as "this step did not really fail" for THIS
			// phase. See the phase-discriminator note on makeStepRunner: true
			// for the main DAG, false for `finally:`, where a non-zero exit is
			// a genuine cleanup failure regardless of why the run is ending.
			cancelMasksFailure := func() bool {
				return suppressOnCancel && cancelledByMaster.Load()
			}

			// recordFailure records a non-continueOnError failure into failedFlag,
			// honouring suppressOnCancel (cancellation does not mask finally failures).
			recordFailure := func() {
				if step.ContinueOnError {
					return
				}
				if cancelMasksFailure() {
					return
				}
				failedFlag.Store(true)
			}

			// markFailed records a failure (via recordFailure) and reports the step as
			// Failed. Used by the cache/artifact branches, which otherwise would not
			// report a Failed status when their internal helper returns an error.
			markFailed := func(reportCtx context.Context) {
				recordFailure()
				_ = client.ReportStep(reportCtx, agentID, api.StepReportRequest{
					RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex,
					StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Failed", EndedAt: time.Now().UTC(),
				})
			}

			// Panic guard (PRIMARY path). A panic in this step's body — backend
			// exec (b.RunDefault / RunNamedContainer / RunInScope), template
			// expansion, output evaluation, etc. — is handled here exactly like a
			// normal step failure: ship the panic to the STEP's own log, send a
			// terminal Failed ReportStep, record the failure (so overallStatus
			// becomes Failed and subsequent steps auto-skip on the failed status),
			// and return nil so RunPipeline continues normally rather than bailing
			// on a returned error. Without this, runOne's own recover (pipeline.go)
			// still turns the panic into a returned error that marks the run
			// Failed — but only AFTER this closure has already sent the "Running"
			// ReportStep (line ~363) and panicked before its terminal report, so
			// the STEP is left stuck "Running" forever with an empty log and no
			// author-visible cause. runOne's recover remains a BACKSTOP for panics
			// OUTSIDE this closure (e.g. in runParallel/runSequential machinery),
			// but this is the seam that keeps the step's terminal report + log
			// intact. recordFailure inside markFailed still honours ContinueOnError
			// (a panic on a continueOnError step is reported Failed but does not
			// fail the run), matching a normal error on such a step.
			defer func() {
				if r := recover(); r != nil {
					stack := debug.Stack()
					slog.Error("step panicked", "runId", c.RunID, "step", step.Name, "index", step.Index, "panic", r, "stack", string(stack))
					reportCtx := context.WithoutCancel(stepCtx)
					_ = client.AppendLogBulk(reportCtx, agentID, c.RunID, step.Index, []api.LogAppendRequest{{
						RunID:     c.RunID,
						StepIndex: step.Index,
						Stream:    "stderr",
						Timestamp: time.Now().UTC(),
						Line:      fmt.Sprintf("step panicked: %v", r),
					}})
					markFailed(reportCtx)
				}
			}()

			// if: evaluate condition against the supplied run status. For the main DAG
			// every step is evaluated — including steps with an empty if: — so that a
			// normal step auto-skips once a prior step has failed (implicitSuccess). For
			// finally the status is frozen and implicitSuccess is false. If false, skip.
			//
			// sctx.snapshot() carries Params, Vars, Steps and Secrets — the
			// four variables dsl.conditionVars declares — so a vars-gated
			// condition sees the same values the step's env and templates do.
			ifData := sctx.snapshot()
			ok, ifWarnings, err := dsl.EvalCondition(step.If, ifData, statusFn(), implicitSuccess)
			if err != nil {
				slog.Warn("if: condition eval failed, running step", "step", step.Name, "error", err)
				recordIfDiagnostic(step,
					fmt.Sprintf("%v — the condition could not be evaluated, so the step RAN (fail-safe)", err))
			}
			for _, w := range ifWarnings {
				slog.Warn("if: condition warning", "step", step.Name, "warning", w)
				recordIfDiagnostic(step, w)
			}
			if !ok {
				retryUntilSuccess(ctx, func(callCtx context.Context) error {
					return client.ReportStep(callCtx, agentID, api.StepReportRequest{
						RunID:      c.RunID,
						StepIndex:  step.Index,
						StageIndex: step.StageIndex,
						StepName:   step.DisplayName(),
						Variant:    step.MatrixKey,
						Status:     "Skipped",
					})
				})
				return nil
			}
			// Apply step-level timeout to the context if one is configured.
			// A retry step applies its timeout per ATTEMPT inside the run loop below,
			// so the whole-step timeout is skipped here (it would otherwise cap the
			// entire retry budget). Non-retry steps keep the single per-step timeout.
			if step.TimeoutMinutes > 0 && step.Retry == nil {
				var stepCancel context.CancelFunc
				stepCtx, stepCancel = context.WithTimeout(stepCtx, time.Duration(step.TimeoutMinutes*float64(time.Minute)))
				defer stepCancel()
			}

			// approval gate: report WaitingApproval, poll for the human decision.
			// Placed after the if: gate so an approval step can itself be if:-gated.
			if step.Approval != nil {
				approved := WaitForApproval(stepCtx, client, agentID, c.RunID, step, ApprovalPollInterval)
				if approved {
					_ = client.ReportStep(stepCtx, agentID, api.StepReportRequest{
						RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex,
						StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Succeeded", EndedAt: time.Now().UTC(),
					})
				} else {
					_ = client.ReportStep(stepCtx, agentID, api.StepReportRequest{
						RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex,
						StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Failed", EndedAt: time.Now().UTC(),
					})
					recordFailure()
				}
				return nil
			}

			// cache steps: restore immediately, defer save to postHooks
			if step.Cache != nil {
				scope, serr := resolveScope(stepCtx, step, b)
				if serr != nil {
					// Cache stays warn+skip (lenient policy), matching the
					// k8s agent: a scope pod/container that never becomes
					// available must not fail the step, just skip the cache
					// operation (no restore, no deferred save). Unlike
					// artifact upload/download, which remain fail-loud.
					slog.Warn("cache scope unavailable; skipping cache for step", "step", step.Name, "error", serr)
					_ = client.ReportStep(context.WithoutCancel(stepCtx), agentID, api.StepReportRequest{
						RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex,
						StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Succeeded", EndedAt: time.Now().UTC(),
					})
					return nil
				}
				if err := executeCacheStep(stepCtx, client, agentID, step, c.RunID, sctx, &postHooksMu, &postHooks, b, scope); err != nil {
					slog.Error("cache step failed", "step", step.Name, "error", err)
					markFailed(context.WithoutCancel(stepCtx))
				}
				return nil
			}
			if step.UploadArtifact != nil {
				scope, serr := resolveScope(stepCtx, step, b)
				if serr != nil {
					slog.Error("upload artifact failed", "step", step.Name, "error", serr)
					markFailed(context.WithoutCancel(stepCtx))
					return nil
				}
				if err := executeUploadArtifact(stepCtx, client, agentID, step, c.RunID, b, scope); err != nil {
					slog.Error("upload artifact failed", "step", step.Name, "error", err)
					markFailed(context.WithoutCancel(stepCtx))
				}
				return nil
			}
			if step.DownloadArtifact != nil {
				scope, serr := resolveScope(stepCtx, step, b)
				if serr != nil {
					slog.Error("download artifact failed", "step", step.Name, "error", serr)
					markFailed(context.WithoutCancel(stepCtx))
					return nil
				}
				dlData := sctx.snapshot()
				if step.MatrixValues != nil {
					dlData.Matrix = step.MatrixValues
					dlData.Foreach = step.MatrixValues // foreach sugar compatibility: {{ .Foreach.key }}
				}
				if err := executeDownloadArtifact(stepCtx, client, agentID, step, c.RunID, b, scope, dlData); err != nil {
					slog.Error("download artifact failed", "step", step.Name, "error", err)
					markFailed(context.WithoutCancel(stepCtx))
				}
				return nil
			}

			started := time.Now().UTC()
			_ = client.ReportStep(stepCtx, agentID, api.StepReportRequest{
				RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Running", StartedAt: started,
			})

			status := "Succeeded"
			exitCode := 0
			tplData := sctx.snapshot()
			if step.MatrixValues != nil {
				tplData.Matrix = step.MatrixValues
				tplData.Foreach = step.MatrixValues // foreach sugar compatibility: {{ .Foreach.key }}
			}

			var callChildRunID, callJobName string
			// stepScope captures this step's scope handle (if any), set inside
			// the isScopedStep run-branch case below. A step's post: hook must
			// execute inside the same scope container the step body ran in
			// (not the host workspace), so postHookEntry carries this through
			// to drainHooks.
			var stepScope ScopeHandle

			if step.Call != nil {
				childOutputs, childRunID, callErr := ExecuteCallStep(stepCtx, client, agentID, c.RunID, step, tplData)
				callChildRunID = childRunID
				callJobName = step.Call.Job
				if callErr != nil {
					slog.Error("call step failed", "step", step.Name, "error", callErr)
					status = "Failed"
				} else {
					sctx.setCallStepResult(step.Name, step.MatrixKey, childOutputs, childRunID)
					if len(childOutputs) > 0 {
						safe := FilterSecretOutputs(childOutputs, masker, func(k string) {
							warnSkippedOutput(stepCtx, step.Index, k)
						})
						if len(safe) > 0 {
							_ = client.SetStepOutputs(stepCtx, agentID, c.RunID, step.Index, step.MatrixKey, safe)
						}
					}
				}
			} else {
				expandedRun, tplErr := dsl.ExpandTemplate(step.Run, tplData)
				if tplErr != nil {
					slog.Error("template expansion failed", "step", step.Name, "error", tplErr)
					expandedRun = step.Run
				}

				// UNIFIED_AGENT_OS lets job authors determine the running OS from within a step.
				// Scoped / runsIn.image steps run in a Linux container regardless of
				// backend; every other step reports b.DefaultAgentOS() — see agentOSForStep.
				//
				// UNIFIED_WORKSPACE lets job authors build workspace-relative paths
				// portably. b.WorkspacePath only inspects whether its scope argument
				// is zero (both backends return a fixed constant for any scoped
				// step; see hostBackend.WorkspacePath / k8sBackend.WorkspacePath), so
				// a placeholder non-zero handle stands in for isScopedStep(step) here
				// rather than the real scope handle: EnsureScope (called below, only
				// for the isScopedStep case) provisions the actual scope container
				// using THIS extraEnv slice, so the slice must already be complete
				// before that call — calling EnsureScope here to obtain a real handle
				// would provision the container too early, with an incomplete env.
				workspaceScope := ScopeHandle{}
				if isScopedStep(step) {
					workspaceScope = NewScopeHandle(step.ScopeID)
				}
				extraEnv := []string{
					EnvAgentOS + "=" + agentOSForStep(step, b.DefaultAgentOS()),
					EnvWorkspace + "=" + b.WorkspacePath(workspaceScope),
				}
				// Vars first, step env second: precedence is expressed as
				// ordering, because a later duplicate key wins. varsExtraEnv
				// also drops any var named after an agent credential — see
				// its doc comment; StepEnv does not filter extraEnv itself.
				extraEnv = append(extraEnv, varsExtraEnv(c.Vars)...)
				for k, v := range step.Env {
					expanded, _ := dsl.ExpandTemplate(v, tplData)
					extraEnv = append(extraEnv, k+"="+expanded)
				}

				// attempts/backoff: a plain step (no retry:) runs its body exactly
				// once via this same loop (attempts defaults to 1, backoff 0).
				attempts := 1
				var backoff time.Duration
				if step.Retry != nil {
					attempts = step.Retry.Attempts
					backoff, _ = time.ParseDuration(step.Retry.Backoff) // validated at apply time; "" -> 0
				}

				var ec int
				var runErr error
				var capturedStdout string
				for try := 1; try <= attempts; try++ {
					// Per-attempt timeout (retry steps only; non-retry steps use
					// stepCtx as-is — its own whole-step timeout, if any, was
					// already applied above).
					attemptCtx := stepCtx
					var attemptCancel context.CancelFunc
					if step.Retry != nil && step.TimeoutMinutes > 0 {
						attemptCtx, attemptCancel = context.WithTimeout(stepCtx, time.Duration(step.TimeoutMinutes*float64(time.Minute)))
					}

					shippedStdout, shippedStderr, finishLogs := b.StepLogWriters(attemptCtx, step.Index)
					// stdout is teed: streamed to the server while the step runs
					// (mirroring the k8s agent's io.MultiWriter approach) AND kept in
					// stdoutBuf for {{ .Stdout }} output-template evaluation below.
					var stdoutBuf bytes.Buffer
					stdoutTee := io.MultiWriter(&stdoutBuf, shippedStdout)
					switch {
					case isScopedStep(step):
						// A scope-tagged step's Container is not guaranteed empty: a
						// template step already scope-tagged by an inner uses:
						// runsIn.image can be inlined again by an outer, non-scope
						// uses: whose container-defaulting (gittemplate/inline.go's
						// `else if ns.Container == ""` branch) stamps its own
						// container: onto the step regardless of ScopeID, so both
						// end up set. That is harmless, not a state to guard against:
						// this case is checked first, so a scope-tagged step always
						// runs in its scope environment no matter what Container
						// holds, and dsl.ValidateContainerReferences short-circuits
						// on a non-empty ScopeID without ever looking at Container.
						//
						// extraEnv here, nil from resolveScope's cache/artifact
						// path: the scope is created once per (ScopeID,
						// MatrixKey) and keeps the FIRST caller's env, so
						// concurrent members sharing a ScopeID resolve it by
						// scheduling. See resolveScope (agent.go) and
						// k8sBackend.ensureScopePod for why that is tolerated.
						h, herr := b.EnsureScope(attemptCtx, step, extraEnv)
						if herr != nil {
							runErr = herr
							ec = -1
						} else {
							stepScope = h
							ec, runErr = b.RunInScope(attemptCtx, h, expandedRun, step.Shell, extraEnv, stdoutTee, shippedStderr)
						}
					case step.Container != "":
						ec, runErr = b.RunNamedContainer(attemptCtx, step, step.Container, expandedRun, extraEnv, stdoutTee, shippedStderr)
					default:
						ec, runErr = b.RunDefault(attemptCtx, step, expandedRun, extraEnv, stdoutTee, shippedStderr)
					}
					capturedStdout = stdoutBuf.String()

					// A non-nil runErr means the step's process never ran to
					// completion — the exec itself failed (target container has no
					// shell, the pod/container is not running, the exec stream
					// broke, etc.) rather than the command exiting non-zero. The
					// command produced no output in that case, so the step log
					// would otherwise be empty and the run would show an opaque
					// failure with nothing to debug. Surface the reason on the
					// step's own stderr stream before flushing. A master
					// cancellation is expected shutdown, not a diagnosable fault,
					// so it is excluded — but only for the phase the cancellation
					// actually terminates. In `finally:` (cancelMasksFailure ==
					// false) the pipeline runs on a non-cancelling context, so an
					// exec error there is a genuine, diagnosable fault even though
					// the run is being cancelled; suppressing it left a broken
					// cleanup step with a completely empty log.
					if runErr != nil && !cancelMasksFailure() {
						fmt.Fprintf(shippedStderr, "unified-cd: step %q failed to execute: %v\n", step.Name, runErr)
						slog.Warn("step exec error",
							"run", c.RunID, "step", step.Name, "container", step.Container, "error", runErr)
					}
					finishLogs(attemptCtx)
					if attemptCancel != nil {
						attemptCancel()
					}

					if runErr == nil && ec == 0 {
						break // success
					}
					if cancelMasksFailure() {
						// Never retry a master/user cancellation — in the phase
						// that cancellation ends. A `finally:` step keeps its full
						// retry budget on a cancelled run: its context is not
						// cancelled, its failures are genuine, and degrading
						// `retry:` to one attempt exactly when cleanup matters most
						// is the opposite of what the author asked for.
						break
					}
					if try < attempts {
						// Separator on the NEXT attempt's stderr writer so it lands in the log.
						_, nextStderr, nextFinish := b.StepLogWriters(stepCtx, step.Index)
						fmt.Fprintf(nextStderr, "── retry %d/%d after %s (previous: exit %d) ──\n", try+1, attempts, backoff, ec)
						nextFinish(stepCtx)
						if serr := retrySleep(stepCtx, backoff); serr != nil {
							break // cancelled during backoff
						}
					}
				}
				exitCode = ec

				if runErr != nil || ec != 0 {
					status = "Failed"
					// A master/user cancellation (during exec OR during a retry backoff) is a
					// cancel, not a fault — cancelledByMaster is the authority, not runErr
					// (with retry, a cancel can land after a non-zero exit with runErr == nil).
					//
					// PHASE-AWARE (see makeStepRunner's discriminator note): this
					// rewrite applies only to the phase the cancellation actually
					// terminates. A `finally:` step that exits non-zero on a
					// cancelled run failed for its own reasons — its context was
					// never cancelled — so it stays "Failed", which is also what
					// makes recordFailure below fire and flip the run to Failed,
					// as dsl.Spec.Finally documents.
					if cancelMasksFailure() {
						status = "Cancelled"
					}
				} else {
					capturedOutputs := map[string]string{}
					outputCtx := dsl.TemplateData{
						Params:  tplData.Params,
						Vars:    tplData.Vars,
						Steps:   tplData.Steps,
						Stdout:  capturedStdout,
						Secrets: tplData.Secrets,
						Matrix:  tplData.Matrix,
						Foreach: tplData.Foreach,
					}
					for outKey, outTpl := range step.Outputs {
						val, err := dsl.ExpandTemplate(outTpl, outputCtx)
						if err != nil {
							slog.Warn("output template evaluation failed", "step", step.Name, "key", outKey, "error", err)
							continue
						}
						capturedOutputs[outKey] = val
					}
					if step.MatrixKey != "" {
						sctx.setStepMatrixOutputs(step.Name, step.MatrixKey, capturedOutputs)
					} else {
						sctx.setStep(step.Name, dsl.StepData{Outputs: dsl.StringOutputs(capturedOutputs)})
					}
					if len(capturedOutputs) > 0 {
						safe := FilterSecretOutputs(capturedOutputs, masker, func(k string) {
							warnSkippedOutput(stepCtx, step.Index, k)
						})
						if len(safe) > 0 {
							_ = client.SetStepOutputs(stepCtx, agentID, c.RunID, step.Index, step.MatrixKey, safe)
						}
					}
				}
			}

			if status == "Succeeded" && step.Post != nil {
				container := step.Container
				// The hook's effective shell: its own declared shell: if set,
				// else the owning step's effective ClaimStep.Shell (inherit).
				// Resolved once here so the hookStack drain (which runs after
				// the step's own ClaimStep is out of scope) doesn't need to.
				hookShell := step.Post.Shell
				if len(hookShell) == 0 {
					hookShell = step.Shell
				}
				postHooksMu.Lock()
				hookStack = append(hookStack, postHookEntry{
					stepName:  step.Name,
					post:      *step.Post,
					scope:     stepScope,
					container: container,
					stepIndex: step.Index,
					shell:     hookShell,
				})
				postHooksMu.Unlock()
			}

			ended := time.Now().UTC()
			// Use a non-cancelling context for the retry so that ReportStep is reliably called
			// even when stepCtx has been cancelled due to timeout or other reasons.
			reportCtx := context.WithoutCancel(stepCtx)
			reportReq := api.StepReportRequest{
				RunID:       c.RunID,
				StepIndex:   step.Index,
				StageIndex:  step.StageIndex,
				StepName:    step.DisplayName(),
				Variant:     step.MatrixKey,
				Status:      status,
				ExitCode:    exitCode,
				StartedAt:   started,
				EndedAt:     ended,
				ChildRunID:  callChildRunID,
				CallJobName: callJobName,
			}
			retryUntilSuccess(reportCtx, func(callCtx context.Context) error {
				return client.ReportStep(callCtx, agentID, reportReq)
			})
			if status == "Failed" {
				recordFailure()
				return nil
			}
			return nil
		}
	}

	// drainHooks runs everything registered on the two hook slices SINCE THE
	// LAST DRAIN and empties them, so it can be called more than once per
	// claim without ever re-running a hook.
	//
	// Two drains, not one: the main DAG's hooks drain immediately after the
	// main pipeline (below), and the `finally` pipeline's hooks drain
	// immediately after finally (further below). A `finally:` step is a
	// perfectly ordinary step — it may carry `post:` or `cache:` — and before
	// this second call the hooks it registered were appended to a stack that
	// nothing ever popped: they were silently dropped, with no step-status,
	// run-status, or log signal that any cleanup had been skipped.
	//
	// Why two drains rather than moving the single drain past finally: a
	// normal step's `post:` hook is that STEP's cleanup, and must not be made
	// to wait on the job-level `finally:` block (which may be long-running,
	// and whose steps may legitimately want to observe the state a main
	// step's post hook left behind). Keeping drain #1 where it always was
	// preserves the existing, observable ordering exactly; drain #2 is purely
	// additive.
	//
	// Ordering contract (unchanged for hooks that already ran, see the
	// `post-hooks-lifo` parity case): within one drain, cache saves run in
	// registration order and `post:` hooks run LIFO — last registered, first
	// run. LIFO is a WITHIN-BATCH guarantee: main-DAG hooks all run before
	// any finally hook, because the finally hooks do not exist yet when the
	// first batch drains.
	//
	// Concurrency: steps register hooks from concurrent goroutines
	// (runParallel), so the slices are detached under postHooksMu and reset
	// to nil in the same critical section. Every append is already guarded by
	// the same mutex, so a hook is handed to exactly one drain. The drains
	// themselves run on RunClaim's own goroutine, after the pipeline whose
	// steps registered them has returned.
	//
	// Cancellation/failure: hookCtx is built from context.WithoutCancel(ctx),
	// so both drains run when the run was cancelled, timed out, or already
	// failed — which is exactly when a `finally:` step's cleanup matters most
	// — and run to completion unless they hit the budget below. A hook that
	// itself fails is logged and does not change the run's status, identically
	// for main and finally hooks.
	//
	// Bounded: WithoutCancel drops the job-level deadline along with the
	// cancellation, leaving a context whose Done() is nil, and RunPostHook
	// takes no timeout of its own — so a hanging cleanup hook pinned RunClaim
	// forever. Each drain therefore opens its OWN phaseBudget window (see
	// DefaultFinallyBudget): the two drains are separated by the whole
	// `finally` pipeline, and a single shared window would start counting
	// before the main DAG had even run.
	//
	// A drain cut short by that budget does NOT change the run's status — a
	// hook failure never has, whatever it fails on, and making "failed because
	// the budget expired" fatal while "failed because the object store was
	// down" stays benign would be an incoherent rule. It is instead recorded
	// into the run's own logs by recordPhaseTruncated, which is the whole
	// point: a truncated cache save must not be silent. phase names the drain
	// in that record, and truncationAttribution names the individual save or
	// hook that was cut off plus the ones that never started — naming the
	// phase alone still leaves "which of my three caches is stale?"
	// unanswered.
	drainHooks := func(phase string) {
		hookCtx, cancelHooks := context.WithTimeout(context.WithoutCancel(ctx), phaseBudget)
		defer cancelHooks()
		// interrupted/skipped are what the truncation record is attributed
		// to. They are written only from this goroutine — the drain runs on
		// RunClaim's own goroutine, after the pipeline whose steps registered
		// the hooks has returned (see the Concurrency note above) — so no
		// lock is needed here even though the slices they describe were
		// appended concurrently.
		var interrupted string
		var skipped []string
		defer func() {
			// Checked before the deferred cancelHooks above (defers run LIFO),
			// so ctx.Err() can only be DeadlineExceeded here — the parent is
			// WithoutCancel and is never cancelled from anywhere else.
			if errors.Is(hookCtx.Err(), context.DeadlineExceeded) {
				recordPhaseTruncated(phase, truncationAttribution(interrupted, skipped))
			}
		}()

		postHooksMu.Lock()
		saves := postHooks
		hooks := hookStack
		postHooks = nil
		hookStack = nil
		postHooksMu.Unlock()

		// runHook executes one queued item and classifies it for the
		// truncation record: an item the budget had already expired before we
		// reached is SKIPPED (it never ran at all), and the one item whose
		// execution the expiry landed inside is INTERRUPTED (it started and
		// did not get to finish).
		//
		// The classification reads hookCtx.Err() around the call rather than
		// inspecting the item's own error, because neither a cache save nor a
		// post hook reports one in a form this loop sees: CacheSave's failure
		// is swallowed into slog inside the closure, and RunPostHook's is
		// deliberately non-fatal.
		//
		// IMPRECISION, both directions: an item at the deadline BOUNDARY may be
		// labelled interrupted whether it completed or never got going. The
		// second is the likelier of the two — the Err() check passes, and the
		// deadline lands in the gap before CacheSave/RunPostHook touches the
		// context, so the item does nothing at all yet is reported interrupted;
		// that gap is nanoseconds wide but is entered on every item, whereas
		// "finished in the last instant" needs the deadline to land inside the
		// call itself. Either way the item IS named, nothing is dropped, and at
		// most one item per drain can be mislabelled — only the heading it lands
		// under is wrong, never the fact that it needs looking at.
		//
		// Distinguishing them would mean threading a result out of every hook.
		// Not worth it: a mislabelled item costs an operator a second look at
		// one save, where the alternative — today's behaviour, no attribution at
		// all — costs them the ability to find the truncated save.
		runHook := func(label string, fn func()) {
			if hookCtx.Err() != nil {
				skipped = append(skipped, label)
				return
			}
			fn()
			if interrupted == "" && hookCtx.Err() != nil {
				interrupted = label
			}
		}

		for _, save := range saves {
			runHook(save.label, func() { save.fn(hookCtx) })
		}
		for i := len(hooks) - 1; i >= 0; i-- {
			entry := hooks[i]
			cmd := entry.post.Run
			var extraEnv []string
			for k, v := range entry.post.Env {
				extraEnv = append(extraEnv, k+"="+v)
			}
			// The owning step's scope (if any) is still alive here — both
			// drains run before the deferred b.CloseScopes (see the `defer`
			// registered alongside masker installation above).
			//
			// Post-hook output is shipped into the OWNING step's log (entry.stepIndex),
			// the same way a main step's output is: open writers via StepLogWriters
			// (which applies the masker, same as every other step's writers) and call
			// finish to flush/stop auto-flush once the hook has run. This is opened
			// fresh per hook (not reused from the step's own StepLogWriters call,
			// which already finished when the step itself completed) so post output
			// gets its own flush lifecycle independent of the step body's.
			runHook(fmt.Sprintf("the post: hook of step %q", entry.stepName), func() {
				postStdout, postStderr, finishPostLogs := b.StepLogWriters(hookCtx, entry.stepIndex)
				runErr := b.RunPostHook(hookCtx, entry.scope, entry.container, cmd, entry.shell, extraEnv, postStdout, postStderr)
				finishPostLogs(hookCtx)
				// Intentionally best-effort: a post: hook's exit code is not
				// even threaded back by ExecBackend.RunPostHook (see its doc
				// comment), and runErr here — reserved for exec-level
				// failures like "couldn't spawn" — only gets logged, never
				// fails the step or run, on either backend. Do not change
				// this to markFailed/recordFailure; see
				// docs/user-guide/writing-jobs/steps.md's "Post-step hooks"
				// section for the user-facing contract this preserves.
				if runErr != nil {
					slog.Warn("post step failed", "step", entry.stepName, "error", runErr)
				}
			})
		}
	}

	mainRunner := makeStepRunner(statusView, true, &anyStepFailed, true)
	dagErr := RunPipeline(runCtx, c.Stages, getData, c.MatrixMaxCombinations, b.ConcurrencyMode(), mainRunner)

	// post-hooks run regardless of DAG success/failure (cache save should always attempt).
	drainHooks("the post:/cache: hook drain that follows the main steps")

	// Freeze the main-DAG status for finally if: evaluation.
	cancelled := cancelledByMaster.Load()
	mainFailed := anyStepFailed.Load() || (dagErr != nil && !cancelled)

	// finally runs after the main DAG on success, failure, AND cancellation. Its if:
	// conditions are evaluated against a frozen main status (so finally steps never
	// auto-skip one another) with implicitSuccess=false (a no-if finally step always
	// runs). A finally step failure flips the run to Failed even on cancellation.
	var finallyFailed atomic.Bool
	if len(c.Finally) > 0 {
		frozen := dsl.RunStatusView{Failed: mainFailed, Cancelled: cancelled}
		finallyStatus := func() dsl.RunStatusView { return frozen }
		finallyRunner := makeStepRunner(finallyStatus, false, &finallyFailed, false)
		// Non-cancelling, but NOT unbounded. Cancellation is deliberately not
		// inherited — that is why `finally:` exists — while the phase still
		// carries a deadline of its own, because WithoutCancel also strips the
		// job-level spec.timeoutMinutes deadline and yields a context whose
		// Done() is nil. See DefaultFinallyBudget for the full failure mode:
		// without this, a `call:` (or plain `run:`) finally step with no
		// per-step timeoutMinutes waited forever, and nothing else could break
		// the deadlock. Per-step `timeoutMinutes:` still works inside
		// `finally:` and remains the precise tool; this is the ceiling.
		finallyCtx, cancelFinally := context.WithTimeout(context.WithoutCancel(ctx), phaseBudget)
		defer cancelFinally()
		if err := RunPipeline(finallyCtx, c.Finally, getData, c.MatrixMaxCombinations, b.ConcurrencyMode(), finallyRunner); err != nil {
			slog.Warn("finally: structural error", "runId", c.RunID, "error", err)
			finallyFailed.Store(true)
		}
		// A truncated `finally:` phase is a PHASE-level fact, recorded and
		// status-affecting independently of any individual step.
		//
		// Independent of the steps because `continueOnError: true` makes
		// recordFailure return early, so a `finally:` block whose steps all
		// carry it could be cut off mid-cleanup and still report Succeeded —
		// the exact silence this whole change exists to remove. Whether a
		// PARTICULAR step's failure matters is what continueOnError answers;
		// whether the PHASE finished is not that step's to say.
		//
		// Status-affecting, unlike a truncated hook drain above, because this
		// phase is author-written pipeline work whose failures already flip
		// the run to Failed (dsl.Spec.Finally's documented contract, and F3's
		// fix). A pipeline that did not finish did not succeed. The main DAG's
		// own result is untouched: what changes is only the run's verdict,
		// which is the honest one — the job's work may have succeeded, but its
		// cleanup demonstrably did not complete.
		if errors.Is(finallyCtx.Err(), context.DeadlineExceeded) {
			// No attribution argument: unlike a hook drain, a truncated
			// `finally:` pipeline already attributes itself. Its steps are
			// real steps — each one was reported to the controller, and the
			// one running at the deadline is reported Failed with its own
			// log — so the operator can already see which step was cut off.
			recordPhaseTruncated("the finally: phase", "")
			finallyFailed.Store(true)
		}
		// Second drain: the `post:` hooks and `cache:` saves the finally
		// steps just registered. Deliberately OUTSIDE the RunPipeline error
		// check — a structural error in the finally pipeline does not
		// un-register the hooks its steps already put on the stack, and
		// dropping them would reintroduce exactly the silent-skip bug this
		// call fixes. Still before the deferred CloseScopes, so a scoped
		// finally step's hook still finds its scope container alive.
		drainHooks("the post:/cache: hook drain that follows finally:")
	}

	var overallStatus api.RunStatus
	switch {
	case mainFailed || finallyFailed.Load():
		overallStatus = api.RunFailed
	case cancelled:
		overallStatus = api.RunCancelled
	default:
		overallStatus = api.RunSucceeded
	}

	// Use a non-cancelling context so that FinishRun and SetRunOutputs are reliably called
	// even when ctx has been cancelled due to timeout or other reasons.
	//
	// DELIBERATELY UNBOUNDED, and the only post-DAG phase that is. The rule
	// for the rest is "every phase that EXECUTES something carries
	// phaseBudget": the `finally:` pipeline, both hook drains, and the
	// deferred CloseScopes teardown. This phase executes nothing of the job's;
	// it only makes the controller's view of the run match what already
	// happened here, through retryUntilSuccess.
	//
	// A ceiling here has nothing to hand off to. retryUntilSuccess already
	// stops on any sub-500 HTTPError, so a controller that has moved on
	// (4xx — the run reaped, deleted, or already terminal) ends the loop at
	// once; the only case that spins is a persistent 5xx, e.g. a load balancer
	// in front of the controller returning 502 through an outage. If the agent
	// gave up there, the run would stay Running at the controller forever with
	// nothing to recover it: the stuck-run reaper's predicate is "the agent
	// row is gone OR its last_seen_at is stale" (store.ListStuckRuns), and an
	// agent that gave up on FinishRun is still claiming and still
	// heartbeating, so it is neither. Giving up strands the run permanently;
	// spinning recovers it the moment the endpoint answers.
	//
	// In the outage that would appear to justify a ceiling — the controller
	// unreachable altogether — the spin costs nothing that is worth having.
	// The agent's own heartbeats are failing too, so its row DOES go stale and
	// the reaper does take over; and the concurrency slot the spin pins is
	// worthless meanwhile, because new claims go through the same controller.
	//
	// If this ever does need a ceiling, the missing piece is on the controller
	// (a reaper predicate that can see a run whose agent is alive but whose
	// finish never landed), not here.
	finishCtx := context.WithoutCancel(ctx)

	// If the controller already marked this run terminal out-of-band (e.g. the
	// stuck-run reaper tripped during a partition), it holds the authoritative
	// status. Do not promote outputs or send our own FinishRun — that would race
	// or overwrite the controller's decision. The pipeline was already stopped by
	// the poller's cancelRun(). (Cancelled is handled by the normal path so the
	// run is still reported Cancelled.)
	if reapedByMaster.Load() {
		slog.Info("run already terminal at master; skipping local outputs/finish", "runId", c.RunID)
		return
	}

	// Promote declared job outputs (only from steps that actually executed).
	//
	// Both phases are scanned. A `finally:` step is an ordinary step — it can
	// set a declared spec.params.outputs value (SetStepOutputs already lands
	// for it) — but only c.Stages used to be walked here, so a report URL
	// published during teardown reached the step's own outputs and then
	// vanished: SetRunOutputs never carried it and a parent `call:` step read
	// nothing. The ORDERING was already right (promotion happens after the
	// finally pipeline); only the scan set was wrong.
	//
	// Name collisions (a main step and a finally step both declaring the same
	// output name) resolve LAST IN DECLARATION ORDER WINS, and c.Finally is
	// scanned after c.Stages, so the finally value wins. Declaration order,
	// not execution order: this loop walks the stages and the steps within
	// each stage exactly as the job wrote them, so for two members of one
	// PARALLEL group — which race, and whose finishing order is not
	// reproducible — the winner is still the one written last. Sequential
	// stages are the case where the two orders coincide.
	//
	// That is not a special case invented here: the loop already resolves
	// collisions between two main-DAG steps that way (a later stage overwrites
	// an earlier one), and `finally:` is written last, so extending the same
	// rule keeps one predictable statement — "the value promoted is the one
	// set by the last step that declares it" — instead of two rules that
	// disagree at the phase boundary. It is also the useful
	// direction: a teardown step overriding a provisional value (a rollback
	// recording the URL actually left live) is a real pattern, whereas a main
	// step reaching back to overwrite a later cleanup step's value is not.
	runOutputs := map[string]string{}
	finalData := sctx.snapshot()
	promote := func(stages []api.ClaimStage) {
		for _, outName := range c.JobOutputs {
			for _, stage := range stages {
				for _, step := range api.StageSteps(stage) {
					if sd, ok := finalData.Steps[step.Name]; ok {
						if val, ok := sd.Outputs[outName]; ok {
							runOutputs[outName] = dsl.OutputValueString(val)
						}
					}
				}
			}
		}
	}
	promote(c.Stages)
	promote(c.Finally)
	if len(runOutputs) > 0 {
		safeRunOutputs := FilterSecretOutputs(runOutputs, masker, func(k string) {
			warnSkippedOutput(finishCtx, -1, k)
		})
		if len(safeRunOutputs) > 0 {
			// Retried until success (unlike the pre-refactor host single-shot
			// call): a transient failure here must not silently drop job outputs,
			// matching the pre-refactor k8s agent's RetryUntilSuccess wrapping.
			// Deliberate reconciliation pick — see Task 8 report.
			retryUntilSuccess(finishCtx, func(callCtx context.Context) error {
				return client.SetRunOutputs(callCtx, agentID, c.RunID, safeRunOutputs)
			})
		}
	}

	retryUntilSuccess(finishCtx, func(callCtx context.Context) error {
		return client.FinishRun(callCtx, agentID, c.RunID, overallStatus)
	})
}

// executeUploadArtifact runs an upload-artifact step: b.ResolveArtifactPath
// resolves ua.Path against the right root for scope (host workDir / k8s pod
// mount path when non-scoped, the scope container's fixed working directory
// when scoped — see ExecBackend.ResolveArtifactPath), and b.UploadArtifact
// routes the actual transfer (host file vs scope-container copyOutToTemp vs
// k8s sidecar exec). Fail-loud: artifact operations do not silently skip on
// error.
func executeUploadArtifact(ctx context.Context, client *Client, agentID string, step api.ClaimStep, runID string, b ExecBackend, scope ScopeHandle) error {
	started := time.Now().UTC()
	_ = client.ReportStep(ctx, agentID, api.StepReportRequest{
		RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Running", StartedAt: started,
	})

	ua := step.UploadArtifact
	artifactPath, err := b.ResolveArtifactPath(scope, ua.Path)
	if err != nil {
		slog.Error("upload-artifact path rejected", "step", step.Name, "error", err)
		_ = client.ReportStep(ctx, agentID, api.StepReportRequest{
			RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Failed",
			StartedAt: started, EndedAt: time.Now().UTC(),
		})
		return fmt.Errorf("upload-artifact %q: %w", ua.Name, err)
	}
	if err := b.UploadArtifact(ctx, scope, runID, ua.Name, artifactPath); err != nil {
		slog.Error("upload-artifact failed", "step", step.Name, "error", err)
		_ = client.ReportStep(ctx, agentID, api.StepReportRequest{
			RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Failed",
			StartedAt: started, EndedAt: time.Now().UTC(),
		})
		return fmt.Errorf("upload-artifact %q: %w", ua.Name, err)
	}
	_ = client.ReportStep(ctx, agentID, api.StepReportRequest{
		RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Succeeded",
		StartedAt: started, EndedAt: time.Now().UTC(),
	})
	return nil
}

// artifactRunIDRe constrains the expanded downloadArtifact.runId value. The
// value is spliced into a URL path (host backend) and a sidecar --run
// argument (k8s backend), so it must not contain path separators, dots, or
// any URL-structure characters.
var artifactRunIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// executeDownloadArtifact runs a download-artifact step, mirroring
// executeUploadArtifact's path resolution (see ExecBackend.ResolveArtifactPath).
// tplData is used to expand DownloadArtifact.RunID; Secrets and Stdout are
// deliberately excluded from the expansion context because the expanded
// value is embedded in a URL path and appears in logs (same precedent as
// call: param expansion in ExecuteCallStep).
func executeDownloadArtifact(ctx context.Context, client *Client, agentID string, step api.ClaimStep, runID string, b ExecBackend, scope ScopeHandle, tplData dsl.TemplateData) error {
	started := time.Now().UTC()
	_ = client.ReportStep(ctx, agentID, api.StepReportRequest{
		RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Running", StartedAt: started,
	})

	da := step.DownloadArtifact
	failStep := func(err error) error {
		slog.Error("download-artifact failed", "step", step.Name, "error", err)
		_ = client.ReportStep(ctx, agentID, api.StepReportRequest{
			RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Failed",
			StartedAt: started, EndedAt: time.Now().UTC(),
		})
		return fmt.Errorf("download-artifact %q: %w", da.Name, err)
	}

	targetRunID := runID
	if da.RunID != "" {
		restricted := dsl.TemplateData{Params: tplData.Params, Vars: tplData.Vars, Steps: tplData.Steps, Matrix: tplData.Matrix, Foreach: tplData.Foreach}
		expanded, err := dsl.ExpandTemplate(da.RunID, restricted)
		if err != nil {
			return failStep(fmt.Errorf("runId template: %w", err))
		}
		if !artifactRunIDRe.MatchString(expanded) {
			// Do not echo the expanded value: it is attacker-influenced on
			// the failure path and would land in operator-read logs.
			return failStep(fmt.Errorf("runId expanded to a value not matching %s", artifactRunIDRe.String()))
		}
		targetRunID = expanded
	}

	destDir := da.DestDir
	if destDir == "" {
		destDir = "."
	}
	resolvedDestDir, err := b.ResolveArtifactPath(scope, destDir)
	if err != nil {
		return failStep(err)
	}

	if err := b.DownloadArtifact(ctx, scope, targetRunID, da.Name, resolvedDestDir); err != nil {
		return failStep(err)
	}

	_ = client.ReportStep(ctx, agentID, api.StepReportRequest{
		RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Succeeded",
		StartedAt: started, EndedAt: time.Now().UTC(),
	})
	return nil
}

// executeCacheStep runs a cache step: restore immediately (best-effort,
// lenient — a miss/error never fails the step), deferring the matching save
// into postHooks so it captures the final workspace state at claim end.
// cachePath resolution mirrors executeUploadArtifact (see
// ExecBackend.ResolveCachePath) for both the scoped and non-scoped case: a
// path that is absolute or escapes the resolved root is a hard error (unlike
// restore/save's own lenient miss/error handling below) — a path-escape is
// a containment violation, not a cache miss.
func executeCacheStep(
	ctx context.Context,
	client *Client,
	agentID string,
	step api.ClaimStep,
	runID string,
	sctx *safeStepCtx,
	postHooksMu *sync.Mutex,
	postHooks *[]deferredHook,
	b ExecBackend,
	scope ScopeHandle,
) error {
	started := time.Now().UTC()
	_ = client.ReportStep(ctx, agentID, api.StepReportRequest{
		RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Running", StartedAt: started,
	})

	cs := step.Cache
	tplData := sctx.snapshot()

	key, err := dsl.ExpandTemplate(cs.Key, tplData)
	if err != nil {
		return fmt.Errorf("cache key template: %w", err)
	}
	cachePath, err := dsl.ExpandTemplate(cs.Path, tplData)
	if err != nil {
		return fmt.Errorf("cache path template: %w", err)
	}
	// A key/path template that expands SUCCESSFULLY to an empty string is
	// warn+skip (cache operation skipped, step still Succeeded), matching the
	// k8s agent's empty-key/empty-path branches.
	// A valid-but-empty key must not silently collide caches across runs, and
	// a valid-but-empty path would target the wrong directory (workspace
	// root, mount root, etc.) — either way the safe behavior is to skip, not
	// hard-fail: only a template EXPANSION ERROR (above) is a hard failure.
	if key == "" {
		slog.Warn("cache key expanded to empty; skipping cache for step", "step", step.Name, "keyTemplate", cs.Key)
		_ = client.ReportStep(ctx, agentID, api.StepReportRequest{
			RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Succeeded", StartedAt: started, EndedAt: time.Now().UTC(),
		})
		return nil
	}
	if cachePath == "" {
		slog.Warn("cache path expanded to empty; skipping cache for step", "step", step.Name, "pathTemplate", cs.Path)
		_ = client.ReportStep(ctx, agentID, api.StepReportRequest{
			RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Succeeded", StartedAt: started, EndedAt: time.Now().UTC(),
		})
		return nil
	}
	// b.ResolveCachePath resolves cachePath for scope on both backends: a
	// scoped path resolves against the scope container's working directory
	// (so it is always absolute before copyIn/copyOut); a non-scoped path
	// resolves against the claim workspace / pod mount path (mirrors the
	// pre-refactor path.Join(mount, expandedPath)) — see
	// ExecBackend.ResolveCachePath's doc comment. An escaping/absolute
	// cachePath is a hard error here, not a lenient miss.
	scopedCachePath, err := b.ResolveCachePath(scope, cachePath)
	if err != nil {
		slog.Error("cache path rejected", "step", step.Name, "error", err)
		return fmt.Errorf("cache path %q: %w", cachePath, err)
	}
	var restoreKeys []string
	for _, rk := range cs.RestoreKeys {
		expanded, _ := dsl.ExpandTemplate(rk, tplData)
		if expanded != "" {
			restoreKeys = append(restoreKeys, expanded)
		}
	}

	// Cache stays warn+skip on error (lenient policy): a restore/save problem
	// should not fail the step, unlike artifact upload/download.
	// A restore that FAILED is not a hit and not a miss: the backend reports it
	// as an error and it is logged as "not restored", never as "cache hit". The
	// step still succeeds (lenient policy above), but the log now says what
	// actually happened — a cache the step never contacted must not report a hit.
	if hit, err := b.CacheRestore(ctx, scope, key, restoreKeys, scopedCachePath); err != nil {
		slog.Warn("cache not restored; continuing without a cache (best-effort)", "step", step.Name, "key", key, "error", err)
	} else if hit {
		slog.Info("cache hit", "step", step.Name, "key", key)
	} else {
		slog.Info("cache miss", "step", step.Name, "key", key)
	}

	ttlDays := cs.TTLDays
	if ttlDays == 0 {
		ttlDays = 30
	}
	capturedPath := scopedCachePath
	capturedKey := key
	postHooksMu.Lock()
	*postHooks = append(*postHooks, deferredHook{
		label: fmt.Sprintf("the cache: save for step %q (key %q)", step.DisplayName(), capturedKey),
		fn: func(hookCtx context.Context) {
			// NOTE: on the host backend with a nil CacheStore (cache disabled),
			// b.CacheSave is a silent no-op that returns nil, so this still logs
			// "cache saved" even though nothing was saved (hostBackend.CacheSave
			// logs its own DEBUG-level "cache disabled; save skipped" instead).
			// Fixing this precisely would require an ExecBackend interface change
			// (e.g. a bool "did it actually save" return) or a type assertion
			// across the host/k8s seam — too big a change for what is an
			// imprecise log line with no functional impact, so it is left as-is.
			if err := b.CacheSave(hookCtx, scope, capturedKey, capturedPath, ttlDays); err != nil {
				slog.Warn("cache not saved (best-effort)", "key", capturedKey, "error", err)
			} else {
				slog.Info("cache saved", "key", capturedKey)
			}
		},
	})
	postHooksMu.Unlock()

	_ = client.ReportStep(ctx, agentID, api.StepReportRequest{
		RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Succeeded", StartedAt: started, EndedAt: time.Now().UTC(),
	})
	return nil
}
