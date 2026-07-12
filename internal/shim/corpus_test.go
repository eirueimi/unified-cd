package shim

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"gopkg.in/yaml.v3"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// -----------------------------------------------------------------------
// Part 1: compatibility corpus gate.
//
// TestCorpus walks every *.yaml file under examples/ and templates/,
// extracts every `run:` script reachable from a Job's spec.steps (including
// parallel: sub-steps), spec.finally, and post: hooks, and executes each one
// through the same parse -> SanitizeTraps -> interp pipeline shim.Run uses.
//
// It does NOT call shim.Run itself: shim.Run's RunnerOption set is fixed
// (StdIO/Dir/Env only — see run.go) and does not expose a way to install a
// custom interp.ExecHandlers stub, but the whole point of this gate is to
// execute shipped scripts WITHOUT depending on the external programs they
// invoke (git, docker, aws, helm, ...) actually being installed on whatever
// machine happens to run `go test`. So runCorpusScript below reimplements
// shim.Run's pipeline verbatim (parse with syntax.NewParser, sanitize with
// the exported SanitizeTraps, run with interp.New) plus one extra
// RunnerOption: interp.ExecHandlers with a middleware that treats every
// external (non-builtin) command as an immediate success with empty output.
// "Builtins stay real" because builtins (cd, echo, printf, true, false,
// test/[, set, export, ...) are implemented inside the interp package
// itself and never reach the ExecHandler chain at all — only a genuine
// external command name does.
// -----------------------------------------------------------------------

// corpusScript is one extracted `run:` script plus enough provenance to
// name it in a t.Run subtest and in failure messages.
type corpusScript struct {
	file    string // path relative to repo root, forward-slash normalized
	locator string // e.g. "spec.steps[2](build).parallel[1](lint).post"
	script  string
}

// templateExprRe matches a Go-template action ({{ ... }}) so it can be
// replaced with a neutral literal before the script is handed to the shell
// parser/interpreter. We are testing shell-CONSTRUCT compatibility here —
// does mvdan.cc/sh accept and run the shell code our examples/templates
// ship — not template-RENDERING semantics (that's dsl/template's job), so
// {{ .Params.x }}, {{ .Secrets.y }}, {{ eq .Foreach.z "a" }}, etc. all
// collapse to the same inert bareword "X" rather than being rendered.
var templateExprRe = regexp.MustCompile(`\{\{[^}]*\}\}`)

func neutralizeTemplates(script string) string {
	return templateExprRe.ReplaceAllString(script, "X")
}

// walkYAMLFiles returns every *.yaml file under root, recursively, sorted
// for deterministic test output. Per the brief, we skip nothing at the file
// level (including examples/config/*.yaml, which have no `kind:` and are
// filtered out later once probed).
func walkYAMLFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, ".yaml") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(files)
	return files
}

// jobDocsInFile splits path's contents on the "\n---\n" document separator
// (same convention as dsl/examples_parse_test.go) and parses every document
// whose `kind:` is Job. Non-Job documents (WebhookReceiver, AppSource,
// Schedule, GitCredential, or files with no `kind:` at all, e.g.
// examples/config/*.yaml) are silently skipped: they carry no steps: and so
// contribute no run: scripts.
func jobDocsInFile(t *testing.T, path string) []*dsl.Job {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var jobs []*dsl.Job
	docs := strings.Split(string(data), "\n---\n")
	for i, doc := range docs {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}
		var probe struct {
			Kind string `yaml:"kind"`
		}
		if err := yaml.Unmarshal([]byte(doc), &probe); err != nil {
			t.Fatalf("%s: doc %d: probe kind: %v", path, i, err)
		}
		if probe.Kind != "Job" {
			continue
		}
		job, err := dsl.Parse(strings.NewReader(doc))
		if err != nil {
			t.Fatalf("%s: doc %d: dsl.Parse: %v", path, i, err)
		}
		jobs = append(jobs, job)
	}
	return jobs
}

// extractScripts walks job's spec.steps (including parallel: sub-steps),
// spec.finally, and each step's post: hook, collecting every non-empty
// run: string. A step (or post hook) that declares its own non-nil shell:
// is not a shell script we can safely feed to the interp package as-is (it
// might be `shell: [python3, -c]`) — extractScripts skips those and reports
// how many it skipped, so the caller can assert that count is zero today.
// See TestCorpus for why that assertion matters.
func extractScripts(job *dsl.Job, file string) (scripts []corpusScript, skipped int) {
	walkStep := func(run string, shell []string, locator string) {
		if strings.TrimSpace(run) == "" {
			return
		}
		if shell != nil {
			skipped++
			return
		}
		scripts = append(scripts, corpusScript{file: file, locator: locator, script: run})
	}

	walkEntries := func(entries []dsl.StepEntry, section string) {
		for i, e := range entries {
			base := fmt.Sprintf("%s[%d](%s)", section, i, e.Name)
			if e.Parallel != nil {
				for j, s := range e.Parallel {
					sub := fmt.Sprintf("%s.parallel[%d](%s)", base, j, s.Name)
					walkStep(s.Run, s.Shell, sub)
					if s.Post != nil {
						walkStep(s.Post.Run, s.Post.Shell, sub+".post")
					}
				}
				continue
			}
			walkStep(e.Run, e.Shell, base)
			if e.Post != nil {
				walkStep(e.Post.Run, e.Post.Shell, base+".post")
			}
		}
	}

	walkEntries(job.Spec.Steps, "spec.steps")
	walkEntries(job.Spec.Finally, "spec.finally")
	return scripts, skipped
}

// stubExecHandlers makes every external (non-builtin) command in a script
// succeed instantly with no output — see the file-level doc comment above
// for why. It never calls "next", so DefaultExecHandler (which would
// os/exec.LookPath + actually spawn a process) never runs.
func stubExecHandlers(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		return nil
	}
}

// corpusEnv is a minimal, deterministic fake environment for corpus
// scripts: just enough that $PATH/$HOME-consulting builtins (command -v,
// type, hash) see defined values. No real program on this PATH is ever
// invoked — stubExecHandlers intercepts every external command before any
// exec would happen.
func corpusEnv(homeDir string) []string {
	return []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + homeDir,
		"USER=ucd-corpus",
		"SHELL=/.ucd/ucd-sh",
		"LANG=C",
	}
}

// runCorpusScript parses, sanitizes, and runs script the same way shim.Run
// does, except with stubExecHandlers installed. It returns the
// interpreter's own error un-mapped (unlike shim.Run, which maps everything
// down to an int exit code) so the caller can distinguish a parse error /
// non-ExitStatus interpreter error (a real compatibility break) from a
// plain nonzero exit status (a script legitimately exiting nonzero along a
// stubbed-condition branch, e.g. `some-tool --check || exit 1` where
// some-tool is stubbed to "succeed" — not a compatibility problem).
func runCorpusScript(t *testing.T, script string, dir string) (exitCode int, runErr error, parseErr error) {
	t.Helper()

	parser := syntax.NewParser()
	file, perr := parser.Parse(strings.NewReader(script), "")
	if perr != nil {
		return 2, nil, perr
	}

	SanitizeTraps(file, func(msg string) {
		t.Logf("sanitizer warning: %s", msg)
	})

	var stdout, stderr bytes.Buffer
	runner, err := interp.New(
		interp.StdIO(strings.NewReader(""), &stdout, &stderr),
		interp.Dir(dir),
		interp.Env(expand.ListEnviron(corpusEnv(dir)...)),
		interp.ExecHandlers(stubExecHandlers),
	)
	if err != nil {
		t.Fatalf("interp.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	runErr = runner.Run(ctx, file)
	if runErr == nil {
		return 0, nil, nil
	}
	var status interp.ExitStatus
	if errors.As(runErr, &status) {
		return int(status), nil, nil
	}
	return 1, runErr, nil
}

// TestCorpus is the compatibility corpus gate: every run: script shipped in
// examples/ and templates/ must parse and execute under the interp package
// using only supported constructs. PASS policy per script (documented here
// because "the script exits nonzero" is explicitly NOT a failure condition
// on its own — stubbed external commands can legitimately steer a script
// down an `exit 1` branch, and that's fine):
//
//   - no parse error: a script we ship must be valid shell syntax the
//     interp package's parser accepts;
//   - no interp-internal error other than a plain exit status: anything
//     else ("not implemented", an internal panic recovered by the runner,
//     a nil dereference, an unsupported-construct error) is a real
//     compatibility break this gate exists to catch;
//   - the resulting exit code is recorded (t.Log) but never asserted to be
//     any particular value — deliberately, since a stub-influenced branch
//     can legitimately end in `exit 1`.
func TestCorpus(t *testing.T) {
	var allFiles []string
	for _, root := range []string{"../../examples", "../../templates"} {
		allFiles = append(allFiles, walkYAMLFiles(t, root)...)
	}
	if len(allFiles) == 0 {
		t.Fatal("expected to find yaml files under examples/ and templates/")
	}

	var (
		filesWithJobDocs int
		totalScripts     int
		totalSkipped     int
	)

	for _, path := range allFiles {
		norm := filepath.ToSlash(path)
		jobs := jobDocsInFile(t, path)
		if len(jobs) == 0 {
			continue
		}
		filesWithJobDocs++

		for _, job := range jobs {
			scripts, skipped := extractScripts(job, norm)
			totalSkipped += skipped
			for _, sc := range scripts {
				sc := sc
				totalScripts++
				t.Run(norm+"/"+sc.locator, func(t *testing.T) {
					dir := t.TempDir()
					neutralized := neutralizeTemplates(sc.script)
					code, runErr, parseErr := runCorpusScript(t, neutralized, dir)
					if parseErr != nil {
						t.Fatalf("parse error: %v\nscript:\n%s", parseErr, neutralized)
					}
					if runErr != nil {
						t.Fatalf("non-exit-status interp error: %v\nscript:\n%s", runErr, neutralized)
					}
					t.Logf("exit code %d", code)
				})
			}
		}
	}

	// Static audit claim from the design spec ("zero hits for trap-signals,
	// wait -n, jobs, $!, /dev/tcp, [[ in the current corpus") extends to
	// shell: overrides too: today nothing in examples/ or templates/
	// declares a non-default shell:, so extractScripts should never have
	// had to skip a script. Assert that so the day someone adds a
	// `shell: [bash, -lc]` step to a shipped example, this gate is forced
	// to either grow real handling for exercising it (as that shell, not
	// interp) or an explicit, reviewed exemption here — instead of
	// silently skipping it forever.
	if totalSkipped != 0 {
		t.Fatalf("extractScripts skipped %d script(s) due to a non-nil shell: override; "+
			"the corpus is assumed to contain none today — either that assumption is now false "+
			"(teach this test to handle non-default shells) or something is misclassifying a script",
			totalSkipped)
	}

	if totalScripts == 0 {
		t.Fatal("extracted zero run: scripts from examples/ and templates/ — walker is broken")
	}

	t.Logf("corpus stats: %d yaml files walked, %d contained at least one Job doc, %d run: scripts executed, %d skipped (shell: override)",
		len(allFiles), filesWithJobDocs, totalScripts, totalSkipped)
}

// -----------------------------------------------------------------------
// Part 2: construct pins.
//
// Each of these calls shim.Run directly (via the runScript helper defined
// in run_test.go, same package) — real interp, no exec stubbing, because
// none of these scripts invoke an external command. They pin specific
// interp-package behaviors the design doc calls out as either "supported,
// verify it" or "explicitly unsupported, verify the failure mode is clean."
// -----------------------------------------------------------------------

// TestPin_SetDashDashArgvManipulation pins `set --` positional-parameter
// manipulation, including "$@" re-expansion into a new set --.
func TestPin_SetDashDashArgvManipulation(t *testing.T) {
	stdout, stderr, code, err := runScript(t, `set -- a b; set -- "$@" c; echo $#`)
	if err != nil {
		t.Fatalf("unexpected error: %v (stderr: %s)", err, stderr)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	if got := strings.TrimRight(stdout, "\n"); got != "3" {
		t.Fatalf("stdout = %q, want %q", got, "3")
	}
}

// TestPin_IFSNewlineSplittingLoop pins word-splitting an unquoted expansion
// on a custom IFS (a single newline), the standard "for line in $multiline"
// idiom for iterating a newline-delimited variable.
func TestPin_IFSNewlineSplittingLoop(t *testing.T) {
	script := `items="one
two
three"
IFS='
'
count=0
collected=""
for i in $items; do
  count=$((count + 1))
  collected="$collected|$i"
done
echo "$count$collected"
`
	stdout, stderr, code, err := runScript(t, script)
	if err != nil {
		t.Fatalf("unexpected error: %v (stderr: %s)", err, stderr)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	got := strings.TrimRight(stdout, "\n")
	want := "3|one|two|three"
	if got != want {
		t.Fatalf("stdout = %q, want %q (IFS-newline splitting did not collect all items)", got, want)
	}
}

// TestPin_UntilLoopWithArithmetic pins `until` combined with `$((...))`
// arithmetic expansion terminating correctly.
func TestPin_UntilLoopWithArithmetic(t *testing.T) {
	script := `i=0
until [ "$i" -ge 5 ]; do
  i=$((i + 1))
done
echo "$i"
`
	stdout, stderr, code, err := runScript(t, script)
	if err != nil {
		t.Fatalf("unexpected error: %v (stderr: %s)", err, stderr)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	if got := strings.TrimRight(stdout, "\n"); got != "5" {
		t.Fatalf("stdout = %q, want %q", got, "5")
	}
}

// TestPin_FanOutJoin pins `cmd & cmd & wait` fan-out/join: both
// backgrounded subshells must complete before the script continues past
// `wait`.
func TestPin_FanOutJoin(t *testing.T) {
	script := `(echo a) & (echo b) & wait
echo done
`
	stdout, stderr, code, err := runScript(t, script)
	if err != nil {
		t.Fatalf("unexpected error: %v (stderr: %s)", err, stderr)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "a") || !strings.Contains(stdout, "b") || !strings.Contains(stdout, "done") {
		t.Fatalf("stdout = %q, want it to contain a, b, and done (fan-out/join incomplete)", stdout)
	}
	if !strings.HasSuffix(strings.TrimRight(stdout, "\n"), "done") {
		t.Fatalf("stdout = %q, want \"done\" last (wait must block until both backgrounded jobs finish)", stdout)
	}
}

// TestPin_WaitOnBackgroundPIDReturnsChildStatus pins `p=$!; wait $p`: the
// virtual job handle mvdan.cc/sh assigns to `$!` after a backgrounded
// command must be usable with `wait` to retrieve that specific command's
// exit status.
func TestPin_WaitOnBackgroundPIDReturnsChildStatus(t *testing.T) {
	script := `false &
p=$!
wait "$p"
echo "$?"
`
	stdout, stderr, code, err := runScript(t, script)
	if err != nil {
		t.Fatalf("unexpected error: %v (stderr: %s)", err, stderr)
	}
	// The script's own exit code is that of its last command (`echo`,
	// which always succeeds) — the pinned behavior is what `wait "$p"`
	// leaves in $?, captured in stdout below.
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	got := strings.TrimRight(stdout, "\n")
	if got != "1" {
		t.Fatalf(`stdout = %q, want %q — wait "$p" should surface the backgrounded false's exit status`, got, "1")
	}
}

// TestPin_TrapExitFiresExactlyOnce pins the supported-condition baseline:
// `trap ... EXIT` alone (nothing for SanitizeTraps to strip) fires exactly
// once, with no sanitizer warning.
func TestPin_TrapExitFiresExactlyOnce(t *testing.T) {
	stdout, stderr, code, err := runScript(t, `trap 'echo x' EXIT`)
	if err != nil {
		t.Fatalf("unexpected error: %v (stderr: %s)", err, stderr)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	if got := strings.Count(stdout, "x"); got != 1 {
		t.Fatalf("stdout = %q, want exactly one %q", stdout, "x")
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty (EXIT is supported, no sanitizer warning expected)", stderr)
	}
}

// TestPin_TrapUnsupportedSignalsSanitizedWarnPlumbing pins the
// `trap ACTION TERM EXIT`-shaped case Task 7's brief calls out. The same
// underlying sanitizer behavior is already unit-tested directly against
// SanitizeTraps in sanitize_test.go, and end-to-end for a single stripped
// signal in run_test.go's TestRun_TrapExitStillFiresAfterSanitizing / here
// again for the record; this pin additionally documents *how a caller
// observes the warning at all*: shim.Run (see run.go) has no warn-callback
// parameter — its only signature is (ctx, script, stdin, stdout, stderr,
// env, dir) — so stderr, prefixed "[ucd-sh] ", is the sole plumbing for
// sanitizer warnings. Two signals (HUP, TERM) are stripped here to also
// pin one-warning-line-per-removed-condition.
func TestPin_TrapUnsupportedSignalsSanitizedWarnPlumbing(t *testing.T) {
	stdout, stderr, code, err := runScript(t, `trap 'echo cleaned' HUP TERM EXIT`)
	if err != nil {
		t.Fatalf("unexpected error: %v (stderr: %s)", err, stderr)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 — a sanitized trap must not error under set -e", code)
	}
	if strings.Count(stdout, "cleaned") != 1 {
		t.Fatalf("stdout = %q, want exactly one %q (EXIT must still fire)", stdout, "cleaned")
	}
	if !strings.HasPrefix(stderr, "[ucd-sh] ") {
		t.Fatalf("stderr = %q, want it to start with the shim's warning prefix", stderr)
	}
	if got := strings.Count(stderr, "[ucd-sh] "); got != 2 {
		t.Fatalf("stderr = %q, want exactly 2 warning lines (one per stripped signal: HUP, TERM)", stderr)
	}
	if !strings.Contains(stderr, "HUP") || !strings.Contains(stderr, "TERM") {
		t.Fatalf("stderr = %q, want it to name both stripped signals", stderr)
	}
}

// TestPin_WaitDashNIsRejected pins `wait -n`'s exact failure mode. Per the
// design doc's interp constraints table, `wait -n`/`-p` are not supported
// upstream (mvdan.cc/sh v3.13.1); a script needing them must declare
// `shell: [bash, -lc]`. Pinned here: it is a clean, immediate error (not a
// panic, not a hang) — status 2 (the interp package's convention for a
// builtin usage error) with a message naming both "wait" and the rejected
// flag.
func TestPin_WaitDashNIsRejected(t *testing.T) {
	stdout, stderr, code, err := runScript(t, `wait -n`)
	if err != nil {
		t.Fatalf("unexpected error: %v (stderr: %s)", err, stderr)
	}
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (pinned builtin-usage-error status for `wait -n`)", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "wait") {
		t.Fatalf("stderr = %q, want it to mention \"wait\"", stderr)
	}
	if !strings.Contains(stderr, "-n") {
		t.Fatalf("stderr = %q, want it to name the rejected \"-n\" flag", stderr)
	}
}

// TestPin_BackgroundJobStillRunningAtScriptEnd answers the design doc's
// explicitly open question ("does a lingering `cmd &` hang the run?"):
// backgrounds a `(while true; do :; done) &` — a subshell that never exits
// on its own — and does NOT `wait` for it. If Run blocked until every
// backgrounded job finished, this would hang until the context deadline
// (or forever, without one). If Run returns as soon as the main script
// body finishes, the background job is orphaned as a goroutine bounded
// only by ctx's lifetime.
//
// PINNED FINDING (see the t.Log below, and reproduced in the task-7
// report): Run returns PROMPTLY — it does not wait for backgrounded jobs
// still running when the script body ends. mvdan.cc/sh subshells are
// goroutines, not OS processes; an unreaped background job is not killed
// by Run returning, it keeps running until the ctx passed to Run is
// canceled (in production, that's the step's own context — a step timeout
// or run cancellation will eventually stop it, but a step with no
// wait/timeout that backgrounds a long-running or infinite-looping job
// will report success and move on while that job is still executing
// in-process). This is a real constraint worth documenting outside this
// test (see docs follow-up in the task-7 report) — it is NOT something
// this test can "fix": it is pinning shim.Run's actual, current behavior.
func TestPin_BackgroundJobStillRunningAtScriptEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var stdout, stderr bytes.Buffer
	script := "(while true; do :; done) &\necho done\n"

	done := make(chan struct{})
	var code int
	var runErr error
	start := time.Now()
	go func() {
		code, runErr = Run(ctx, script, strings.NewReader(""), &stdout, &stderr, nil, "")
		close(done)
	}()

	const promptBound = 2 * time.Second
	select {
	case <-done:
		elapsed := time.Since(start)
		t.Logf("FINDING: Run returned after %v (well under the %v bound), orphaning the "+
			"backgrounded infinite loop rather than waiting for it. exit code=%d err=%v stdout=%q",
			elapsed, promptBound, code, runErr, stdout.String())
		if elapsed >= promptBound {
			t.Fatalf("Run took %v to return, want well under %v for a script whose main body "+
				"has nothing left to do but exit — this would mean the pinned prompt-return "+
				"behavior regressed", elapsed, promptBound)
		}
		if runErr != nil {
			t.Fatalf("unexpected error: %v (stderr: %s)", runErr, stderr.String())
		}
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
		}
		if got := strings.TrimRight(stdout.String(), "\n"); got != "done" {
			t.Fatalf("stdout = %q, want %q", got, "done")
		}
	case <-time.After(promptBound):
		// The opposite finding: Run blocks on the backgrounded job. Fail
		// loudly rather than hang the test suite past this bound, and
		// unblock the goroutine via cancellation so the test process can
		// still exit cleanly.
		cancel()
		<-done
		t.Fatalf("FINDING: Run did NOT return within %v — it appears to block waiting for "+
			"backgrounded jobs to finish rather than orphaning them at script end. This "+
			"contradicts the pinned finding documented in this test and the task-7 report; "+
			"if this is now the real behavior, update both.", promptBound)
	}
}
