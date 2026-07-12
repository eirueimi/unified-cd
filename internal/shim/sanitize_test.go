package shim

import (
	"strings"
	"testing"

	"mvdan.cc/sh/v3/syntax"
)

// sanitize parses src, runs SanitizeTraps, and returns the rewritten source
// (via the printer) plus every warning message collected.
func sanitize(t *testing.T, src string) (rewritten string, warnings []string) {
	t.Helper()
	f, err := syntax.NewParser().Parse(strings.NewReader(src), "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	SanitizeTraps(f, func(msg string) {
		warnings = append(warnings, msg)
	})
	var buf strings.Builder
	if err := syntax.NewPrinter().Print(&buf, f); err != nil {
		t.Fatalf("print: %v", err)
	}
	return buf.String(), warnings
}

func TestSanitizeTraps_ExitAloneUntouched(t *testing.T) {
	out, warnings := sanitize(t, "trap c EXIT\n")
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
	if !strings.Contains(out, "trap c EXIT") {
		t.Fatalf("rewritten script = %q, want trap c EXIT left untouched", out)
	}
}

func TestSanitizeTraps_ErrAloneUntouched(t *testing.T) {
	out, warnings := sanitize(t, "trap c ERR\n")
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
	if !strings.Contains(out, "trap c ERR") {
		t.Fatalf("rewritten script = %q, want trap c ERR left untouched", out)
	}
}

func TestSanitizeTraps_TermRemovedExitKept(t *testing.T) {
	out, warnings := sanitize(t, "trap c TERM EXIT\n")
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly 1", warnings)
	}
	if !strings.Contains(warnings[0], "TERM") {
		t.Fatalf("warning = %q, want it to name TERM", warnings[0])
	}
	if strings.Contains(out, "TERM") {
		t.Fatalf("rewritten script = %q, want TERM removed", out)
	}
	if !strings.Contains(out, "EXIT") {
		t.Fatalf("rewritten script = %q, want EXIT kept", out)
	}
}

func TestSanitizeTraps_IntAloneBecomesNoOp(t *testing.T) {
	out, warnings := sanitize(t, "trap c INT\n")
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly 1", warnings)
	}
	if strings.Contains(out, "trap") {
		t.Fatalf("rewritten script = %q, want the trap call replaced with a no-op", out)
	}
	if !strings.Contains(out, "true") {
		t.Fatalf("rewritten script = %q, want a `true` no-op in its place", out)
	}
}

func TestSanitizeTraps_MultipleUnsupportedEachWarnOnce(t *testing.T) {
	out, warnings := sanitize(t, "trap c INT TERM HUP QUIT\n")
	if len(warnings) != 4 {
		t.Fatalf("warnings = %v, want exactly 4 (one per removed signal)", warnings)
	}
	if !strings.Contains(out, "true") || strings.Contains(out, "trap") {
		t.Fatalf("rewritten script = %q, want a `true` no-op (all conditions removed)", out)
	}
}

func TestSanitizeTraps_SigPrefixedAndNumericRemoved(t *testing.T) {
	out, warnings := sanitize(t, "trap c SIGTERM 2 EXIT\n")
	if len(warnings) != 2 {
		t.Fatalf("warnings = %v, want exactly 2 (SIGTERM and 2)", warnings)
	}
	trapLine := strings.TrimSpace(out)
	if trapLine != "trap c EXIT" {
		t.Fatalf("rewritten script = %q, want exactly %q", trapLine, "trap c EXIT")
	}
}

func TestSanitizeTraps_UnrecognizedWordLeftAlone(t *testing.T) {
	// Not a signal name we recognize; leave it in place so the
	// interpreter's own error fires (conservative: we don't guess).
	out, warnings := sanitize(t, "trap c BOGUS\n")
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none for an unrecognized word", warnings)
	}
	if !strings.Contains(out, "BOGUS") {
		t.Fatalf("rewritten script = %q, want BOGUS left untouched", out)
	}
}

func TestSanitizeTraps_NonTrapCallsUntouched(t *testing.T) {
	out, warnings := sanitize(t, "echo TERM INT EXIT\n")
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
	if !strings.Contains(out, "echo TERM INT EXIT") {
		t.Fatalf("rewritten script = %q, want echo call left untouched", out)
	}
}

func TestSanitizeTraps_TrapWithNoConditionsUntouched(t *testing.T) {
	// `trap` with zero args (list form) or one arg has no condition list to
	// sanitize at all.
	out, warnings := sanitize(t, "trap\ntrap c\n")
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
	if !strings.Contains(out, "trap\ntrap c") {
		t.Fatalf("rewritten script = %q, want both trap calls left untouched", out)
	}
}

func TestSanitizeTraps_RunningTermEmitsCleanedOnceNoError(t *testing.T) {
	// End-to-end: the sanitized script must not error, and EXIT must still
	// fire exactly once.
	stdout, stderr, code, err := runScript(t, `trap 'echo cleaned' TERM EXIT`)
	if err != nil {
		t.Fatalf("unexpected error: %v (stderr: %s)", err, stderr)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if strings.Count(stdout, "cleaned") != 1 {
		t.Fatalf("stdout = %q, want exactly one %q", stdout, "cleaned")
	}
}

// Regression for the bare two-word reset form: `trap TERM` has no handler
// word — the single operand is a condition, and an unsupported one must be
// stripped or it kills set -e scripts with status 2.
func TestSanitizeTraps_BareSignalResetRemoved(t *testing.T) {
	out, warnings := sanitize(t, "trap TERM\n")
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly 1", warnings)
	}
	if !strings.Contains(warnings[0], "TERM") {
		t.Fatalf("warning = %q, want it to name TERM", warnings[0])
	}
	if strings.Contains(out, "trap") {
		t.Fatalf("rewritten script = %q, want the trap call replaced by a no-op", out)
	}
	if !strings.Contains(out, "true") {
		t.Fatalf("rewritten script = %q, want the no-op true", out)
	}
}

// `trap EXIT` (bare reset of a SUPPORTED condition) must pass through untouched.
func TestSanitizeTraps_BareExitResetKept(t *testing.T) {
	out, warnings := sanitize(t, "trap EXIT\n")
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
	if !strings.Contains(out, "trap EXIT") {
		t.Fatalf("rewritten script = %q, want trap EXIT left untouched", out)
	}
}

// The handler STRING is never scanned for signal names: `trap 'echo INT' EXIT`
// keeps its handler verbatim (the highest-risk positional-corruption case).
func TestSanitizeTraps_HandlerStringWithSignalNameUntouched(t *testing.T) {
	out, warnings := sanitize(t, "trap 'echo INT' EXIT\n")
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
	if !strings.Contains(out, "echo INT") {
		t.Fatalf("rewritten script = %q, want the handler string kept verbatim", out)
	}
	if !strings.Contains(out, "EXIT") {
		t.Fatalf("rewritten script = %q, want EXIT kept", out)
	}
}
