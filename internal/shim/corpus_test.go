package shim

import (
	"context"
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
	"mvdan.cc/sh/v3/interp"
)

// -----------------------------------------------------------------------
// Part 1: compatibility corpus gate.
//
// TestCorpus walks every *.yaml file under examples/ and templates/,
// extracts every `run:` script reachable from a Job's spec.steps (including
// parallel: sub-steps), spec.finally, and post: hooks, and executes each one
// through the exact same parse -> SanitizeTraps -> interp pipeline shim.Run
// uses in production, via shim.RunWithHandlers (see run.go) plus one extra
// exec handler: stubExecHandler, which treats every external (non-builtin)
// command as an immediate success with empty output instead of actually
// exec-ing it. That's the whole point of this gate: execute shipped scripts
// WITHOUT depending on the external programs they invoke (git, docker, aws,
// helm, ...) actually being installed on whatever machine happens to run
// `go test`. "Builtins stay real" because builtins (cd, echo, printf, true,
// false, test/[, set, export, ...) are implemented inside the interp
// package itself and never reach the ExecHandler chain at all — only a
// genuine external command name does.
//
// This used to be a hand-rolled reimplementation of shim.Run's pipeline
// (parse/sanitize/interp.New wired up a second time in this file) because
// shim.Run had no way to install a custom exec handler. RunWithHandlers
// closes that gap so there is exactly one pipeline, exercised both in
// production and here.
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
//
// Caveat for future editors: this per-action substitution has no concept of
// template CONTROL FLOW. A `{{ range .Foreach.items }}...{{ end }}` loop
// body is not aware it's inside a loop — `{{ range ... }}` and `{{ end }}`
// each collapse to their own standalone "X" the same as any other action,
// so the body between them survives verbatim and appears exactly ONCE in
// the neutralized script, never repeated. If a shipped script's loop body
// contains shell syntax that's only valid when concatenated with itself
// (e.g. an unbalanced quote or heredoc meant to be closed by a later
// iteration), this test would not catch it. Nothing in the corpus uses
// {{ range }} today.
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

// jobDoc pairs a parsed Job document with its own raw (post-split) text, so
// callers can independently cross-check the parsed form against the raw
// source (see the run:-count self-check in TestCorpus).
type jobDoc struct {
	job  *dsl.Job
	text string // this document's own text, CRLF already normalized to LF
	idx  int    // 0-based document index within the file, for error messages
}

// jobDocsInFile splits path's contents on the "\n---\n" document separator
// (same convention as dsl/examples_parse_test.go) and parses every document
// whose `kind:` is Job. Non-Job documents (WebhookReceiver, AppSource,
// Schedule, GitCredential, or files with no `kind:` at all, e.g.
// examples/config/*.yaml) are silently skipped: they carry no steps: and so
// contribute no run: scripts.
//
// CRLF handling: every file in this corpus is checked out CRLF on Windows.
// splitting on the literal "\n---\n" never matches "\r\n---\r\n", so an
// un-normalized split silently collapses every multi-doc file down to its
// first document — the rest of the docs (and every run: script inside them)
// vanish with no error. We normalize \r\n -> \n on the whole file up front,
// before splitting, rather than switching to a CRLF-tolerant split regexp
// (`\r?\n---\r?\n`), because normalizing first also hardens everything
// downstream of the split: dsl.Parse, the raw-text run:-count regexp below,
// and any future consumer of a document's text all then operate on
// consistently-LF text instead of each having to independently worry about
// stray \r bytes (e.g. in a heredoc or a regexp anchor).
func jobDocsInFile(t *testing.T, path string) []jobDoc {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	normalized := strings.ReplaceAll(string(data), "\r\n", "\n")

	var jobs []jobDoc
	docs := strings.Split(normalized, "\n---\n")
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
		jobs = append(jobs, jobDoc{job: job, text: doc, idx: i})
	}
	return jobs
}

// runFieldRe matches a top-level (possibly indented) `run:` field key on its
// own line. Used only as an independent, dumb cross-check against the
// structured extractScripts walk below — see the self-check in TestCorpus.
var runFieldRe = regexp.MustCompile(`(?m)^\s*run:`)

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

// stubExecHandler makes every external (non-builtin) command in a script
// succeed instantly with no output — see the file-level doc comment above
// for why. It never delegates to a "next" handler (RunWithHandlers wires it
// as the sole, terminal handler), so DefaultExecHandler (which would
// os/exec.LookPath + actually spawn a process) never runs.
var stubExecHandler interp.ExecHandlerFunc = func(ctx context.Context, args []string) error {
	return nil
}

// corpusEnv is a minimal, deterministic fake environment for corpus
// scripts: just enough that $PATH/$HOME-consulting builtins (command -v,
// type, hash) see defined values. No real program on this PATH is ever
// invoked — stubExecHandler intercepts every external command before any
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

// runCorpusScript runs script through shim.RunWithHandlers — the exact same
// parse -> SanitizeTraps -> interp.New -> Run pipeline production code
// (shim.Run) uses, plus stubExecHandler so external commands never actually
// execute. There is no longer a second, hand-rolled copy of that pipeline
// here (see run.go's RunWithHandlers doc comment for why one previously
// existed and why it was removed).
//
// It returns the interpreter's own error un-mapped by exit-status (unlike
// the raw exit code alone) so the caller can distinguish a parse error /
// context-cancellation / non-ExitStatus interpreter error (a real
// compatibility break) from a plain nonzero exit status (a script
// legitimately exiting nonzero along a stubbed-condition branch, e.g.
// `some-tool --check || exit 1` where some-tool is stubbed to "succeed" —
// not a compatibility problem): RunWithHandlers (via Run's contract) returns
// a nil error precisely when the script parsed and ran to completion with
// nothing worse than a plain exit status, and a non-nil error for a parse
// failure, a context cancellation/timeout (mapped to 124/130, same as
// production), or any other interpreter-internal error.
func runCorpusScript(t *testing.T, script string, dir string) (exitCode int, err error) {
	t.Helper()

	// syncBuffer (defined in run_test.go), not a bare bytes.Buffer: corpus
	// scripts are shipped shell scripts, and nothing here statically
	// guarantees none of them ever backgrounds a command (`&`) — the interp
	// package documents that Stdout/Stderr writes may then happen
	// concurrently from background-command goroutines. See syncBuffer's doc
	// comment for the full rationale.
	var stdout, stderr syncBuffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	code, runErr := RunWithHandlers(ctx, script, strings.NewReader(""), &stdout, &stderr,
		corpusEnv(dir), dir, stubExecHandler)
	if stderr.Len() > 0 {
		// Sanitizer warnings (and nothing else — scripts don't otherwise
		// write to stderr under a successful/plain-exit-status run without
		// it being their own business) land here via shim's "[ucd-sh] "
		// prefix plumbing; surface them the same way the old inline
		// SanitizeTraps callback used to.
		t.Logf("stderr:\n%s", stderr.String())
	}
	return code, runErr
}

// wantFilesWithJobDocs and wantTotalScripts are the corpus's known-good
// totals as of this writing (recounted independently of extractScripts —
// via `grep -c '^\s*run:'` per Job doc — while fixing the CRLF bug that
// used to silently collapse every multi-doc *.yaml file down to its first
// document, undercounting both). TestCorpus asserts the walker reproduces
// these exact numbers as a drift trip-wire: a silent regression in the
// walker (e.g. this CRLF bug's shape recurring, or a future refactor that
// breaks doc-splitting again) changes these numbers instead of failing
// silently.
//
// wantTotalScripts counts scripts the walker EXECUTES under interp; scripts
// skipped for a non-default shell: are counted by wantSkipped instead, not
// here.
//
// ** Adding a new example or template with a Job doc? Update these
// constants to match (and re-derive them independently — don't just copy
// whatever the test prints) as part of that change. **
const (
	wantFilesWithJobDocs = 50
	wantTotalScripts     = 87

	// wantSkipped is the number of run: scripts the gate legitimately does
	// NOT execute under interp because their step declares a non-default
	// shell:. Today that is exactly the two shell: steps in
	// examples/jobs/shell-override.yaml (real-bash → [bash, -lc];
	// run-python → [python3, -c]) — those opt into a specific interpreter,
	// so interp compatibility is out of scope for them by design. Adding or
	// removing a shell:-declared step in the corpus must update this count.
	wantSkipped = 2
)

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
//
// It also self-checks its own extraction, per Job doc and corpus-wide, so a
// class of bug like the CRLF doc-splitting bug this test previously had
// (which silently dropped 12 of 83 scripts down to 71, misclassifying 2
// files in the process) cannot hide again: see the raw-text run:-count
// cross-check inside the loop below, and the wantFilesWithJobDocs /
// wantTotalScripts trip-wire after it.
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
		totalPostScripts int
	)

	for _, path := range allFiles {
		norm := filepath.ToSlash(path)
		jobs := jobDocsInFile(t, path)
		if len(jobs) == 0 {
			continue
		}
		filesWithJobDocs++

		for _, jd := range jobs {
			scripts, skipped := extractScripts(jd.job, norm)
			totalSkipped += skipped

			// Self-check: an independent, dumb raw-text regexp count of
			// `run:` fields in this doc's own text must equal the number
			// of run: fields the structured walk (extractScripts) found
			// in it (scripts it will execute, plus any it skipped for a
			// non-nil shell: override — both are still "a run: field the
			// walker saw"). A mismatch means extraction and the raw
			// source have diverged — e.g. a doc-splitting bug silently
			// dropping part of this document, or extractScripts failing
			// to walk some path that has a run: field (a new step shape,
			// a new post: location, etc.) — and must fail loudly, naming
			// exactly which file and doc, rather than just quietly
			// undercounting the corpus-wide totals below.
			rawCount := len(runFieldRe.FindAllString(jd.text, -1))
			extractedCount := len(scripts) + skipped
			if rawCount != extractedCount {
				t.Fatalf("%s: doc %d (Job %q): raw-text `run:` field count = %d, but extraction found %d "+
					"(%d executable + %d skipped) — extraction disagrees with the document's own source; "+
					"a run: script may have been silently dropped or double-counted",
					norm, jd.idx, jd.job.Metadata.Name, rawCount, extractedCount, len(scripts), skipped)
			}

			for _, sc := range scripts {
				sc := sc
				totalScripts++
				if strings.HasSuffix(sc.locator, ".post") {
					totalPostScripts++
				}
				t.Run(norm+"/"+sc.locator, func(t *testing.T) {
					dir := t.TempDir()
					neutralized := neutralizeTemplates(sc.script)
					code, err := runCorpusScript(t, neutralized, dir)
					if err != nil {
						t.Fatalf("non-exit-status error (parse failure, timeout, or interp-internal error): %v\nscript:\n%s", err, neutralized)
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
	if totalSkipped != wantSkipped {
		t.Fatalf("extractScripts skipped %d script(s) due to a non-nil shell: override, want %d; "+
			"a shell:-declared step was added or removed in the corpus (or a script is being "+
			"misclassified) — update wantSkipped and confirm which steps it accounts for "+
			"(see examples/jobs/shell-override.yaml)",
			totalSkipped, wantSkipped)
	}

	if totalScripts == 0 {
		t.Fatal("extracted zero run: scripts from examples/ and templates/ — walker is broken")
	}

	// post: hooks are a distinct extraction path (see extractScripts'
	// walkEntries: e.Post / s.Post, separate from the plain run: walk) that
	// the CRLF bug this test previously had completely blinded: every post:
	// hook in the corpus lived in git-template.yaml's second document,
	// which the un-normalized split silently dropped, so this path had zero
	// coverage despite the corpus containing post: examples all along.
	// Assert at least one is extracted so that class of gap can't recur
	// unnoticed.
	if totalPostScripts == 0 {
		t.Fatal("zero post: hook scripts were extracted corpus-wide — the post: extraction path " +
			"has silently lost coverage (examples/jobs/git-template.yaml's post-step-demo doc ships " +
			"3 post: hooks today; if it or an equivalent example was removed, either restore post: " +
			"coverage in the corpus or explain why this path is legitimately untested)")
	}

	if filesWithJobDocs != wantFilesWithJobDocs {
		t.Fatalf("files containing >=1 Job doc = %d, want %d (hardcoded trip-wire — see the "+
			"wantFilesWithJobDocs doc comment; update it if this change is an intentional corpus edit)",
			filesWithJobDocs, wantFilesWithJobDocs)
	}
	if totalScripts != wantTotalScripts {
		t.Fatalf("total run: scripts extracted = %d, want %d (hardcoded trip-wire — see the "+
			"wantTotalScripts doc comment; update it if this change is an intentional corpus edit)",
			totalScripts, wantTotalScripts)
	}

	t.Logf("corpus stats: %d yaml files walked, %d contained at least one Job doc, %d run: scripts executed (%d from post: hooks), %d skipped (shell: override)",
		len(allFiles), filesWithJobDocs, totalScripts, totalPostScripts, totalSkipped)
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

	// syncBuffer, not bytes.Buffer: this script backgrounds a job it never
	// waits for, so the orphaned goroutine may still be writing to
	// stdout/stderr (e.g. its own late writes racing this test's later
	// stdout.String() reads) after Run returns — see syncBuffer's doc
	// comment in run_test.go.
	var stdout, stderr syncBuffer
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
