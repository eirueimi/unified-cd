// Package paritycases defines a shared set of DSL-conformance scenarios used
// to prove the host agent (internal/agent) and the k8s agent
// (internal/k8sagent) execute the step DSL identically. It is intentionally a
// plain (non-test) package: both agent packages' *_test.go drivers import it,
// but the data/assertion contract itself carries no *testing.T-specific
// scaffolding beyond the Assert helper below, which is the one place the
// `testing` package is used.
//
// Each Case builds a fresh api.ClaimResponse (via the Claim func field) so the
// host driver and the k8s driver never share mutable claim state.
package paritycases

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
)

// LogLine describes an expected shipped log line: a step name, a stream
// ("stdout" or "stderr" — the exact stream names both agents pass to
// LogAppendRequest.Stream / NewLogPusher), and a substring that must appear in
// at least one shipped line for that step+stream.
type LogLine struct {
	Step      string
	Stream    string // "stdout" or "stderr"
	Substring string
}

// Expectation is the driver-agnostic assertion contract for one Case.
type Expectation struct {
	// StepStatus maps "name" (or "name@variant" for a matrix/foreach
	// combination) to the expected terminal status string, e.g. "Succeeded",
	// "Failed", "Skipped", "Cancelled".
	StepStatus map[string]string

	// RunFinished is the expected terminal run status: "Succeeded"|"Failed"|"Cancelled".
	RunFinished string

	// LogMustContain lists (step, stream, substring) triples that must each
	// match at least one captured log line for that step+stream.
	LogMustContain []LogLine

	// LogMustNotContain lists substrings that must not appear in ANY shipped
	// log line, across every step/stream (e.g. a raw unmasked secret value).
	LogMustNotContain []string

	// LogMustMatch is like LogMustContain, but LogLine.Substring is instead a
	// regular expression (regexp.MatchString) evaluated against the full line
	// text. Used sparingly (e.g. asserting `os=<something>` without pinning
	// the exact OS string). Kept as a separate slice rather than overloading
	// LogMustContain so the two matching modes (plain substring vs regex)
	// stay unambiguous in Assert.
	LogMustMatch []LogLine

	// Outputs maps stepName -> output key -> an expected SUBSTRING of the
	// recorded output value (substring, not exact match, since e.g. captured
	// stdout may carry a trailing newline the template preserves).
	Outputs map[string]map[string]string
}

// Case is one parity conformance scenario.
type Case struct {
	Name string
	// Claim builds a fresh api.ClaimResponse for this scenario. It is a func
	// (not a struct literal) so that host and k8s drivers each get their own
	// independent copy — ClaimResponse contains slices/maps/pointers that must
	// never be shared/mutated across the two driver runs of the same case.
	Claim func() api.ClaimResponse
	// Secrets is served by both drivers' fake FetchSecrets endpoint.
	Secrets map[string]string
	Expect  Expectation
}

// Observation is what a driver (host or k8s) records while running a Case's
// claim through the real orchestration entrypoint, converted into the shared
// shape Assert checks against an Expectation.
type Observation struct {
	// StepStatus is populated from the LAST (terminal) StepReportRequest
	// received per step+variant key ("name" or "name@variant").
	StepStatus map[string]string
	// RunFinished is the status string passed to FinishRun.
	RunFinished string
	// Logs holds every captured log line, in the order the fake controller
	// received them. NOTE: here (unlike Expectation.LogMustContain) the
	// Substring field holds the FULL captured line text, not a pattern —
	// Assert does substring/regex search over these lines within a given
	// step+stream. Step/Stream are populated from the request that carried
	// the line (LogAppendRequest.StepIndex resolved back to a step name by
	// the driver, and LogAppendRequest.Stream).
	Logs []LogLine
	// Outputs maps stepName -> key -> recorded output value, from
	// SetStepOutputs request bodies.
	Outputs map[string]map[string]string
	// ChildRunID maps stepName -> the ChildRunID recorded on that step's
	// terminal StepReport (only populated for `call:` steps).
	ChildRunID map[string]string
}

// Assert checks got against want, reporting every mismatch via t.Errorf so a
// single test run surfaces all failures at once rather than stopping at the
// first one.
func Assert(t *testing.T, want Expectation, got Observation) {
	t.Helper()

	// --- Step statuses ---
	for step, wantStatus := range want.StepStatus {
		gotStatus, ok := got.StepStatus[step]
		if !ok {
			t.Errorf("step %q: expected terminal status %q, but no terminal status was observed (observed steps: %v)",
				step, wantStatus, sortedKeys(got.StepStatus))
			continue
		}
		if gotStatus != wantStatus {
			t.Errorf("step %q: status mismatch: want %q, got %q", step, wantStatus, gotStatus)
		}
	}
	for step := range got.StepStatus {
		if _, expected := want.StepStatus[step]; !expected {
			// Not necessarily an error (a case may not care about a given
			// step), but surfaced for visibility when debugging drift.
			t.Logf("note: observed step %q (status %q) was not asserted by this case's Expectation.StepStatus", step, got.StepStatus[step])
		}
	}

	// --- Run status ---
	if want.RunFinished != "" && got.RunFinished != want.RunFinished {
		t.Errorf("run finished status mismatch: want %q, got %q", want.RunFinished, got.RunFinished)
	}

	// --- LogMustContain ---
	for _, want := range want.LogMustContain {
		if !anyLogMatches(got.Logs, want, func(line, pat string) bool { return strings.Contains(line, pat) }) {
			t.Errorf("expected a %s/%s log line containing %q; observed %s/%s lines: %v",
				want.Step, want.Stream, want.Substring, want.Step, want.Stream, matchingStepStreamLines(got.Logs, want.Step, want.Stream))
		}
	}

	// --- LogMustMatch (regex) ---
	for _, want := range want.LogMustMatch {
		re, err := regexp.Compile(want.Substring)
		if err != nil {
			t.Errorf("LogMustMatch: invalid regexp %q: %v", want.Substring, err)
			continue
		}
		if !anyLogMatches(got.Logs, want, func(line, _ string) bool { return re.MatchString(line) }) {
			t.Errorf("expected a %s/%s log line matching regexp %q; observed %s/%s lines: %v",
				want.Step, want.Stream, want.Substring, want.Step, want.Stream, matchingStepStreamLines(got.Logs, want.Step, want.Stream))
		}
	}

	// --- LogMustNotContain ---
	for _, forbidden := range want.LogMustNotContain {
		for _, l := range got.Logs {
			if strings.Contains(l.Substring, forbidden) {
				t.Errorf("forbidden substring %q found in shipped log line (step=%s stream=%s): %q",
					forbidden, l.Step, l.Stream, l.Substring)
			}
		}
	}

	// --- Outputs ---
	for step, wantKV := range want.Outputs {
		gotKV, ok := got.Outputs[step]
		if !ok {
			t.Errorf("step %q: expected outputs %v, but no outputs were recorded for this step", step, wantKV)
			continue
		}
		for k, wantSub := range wantKV {
			gotVal, ok := gotKV[k]
			if !ok {
				t.Errorf("step %q: expected output %q to contain %q, but key was never set (recorded outputs: %v)", step, k, wantSub, gotKV)
				continue
			}
			if !strings.Contains(gotVal, wantSub) {
				t.Errorf("step %q output %q: want substring %q, got %q", step, k, wantSub, gotVal)
			}
		}
	}
}

// anyLogMatches reports whether any captured line for want.Step/want.Stream
// satisfies match(line, want.Substring). An empty want.Step or want.Stream
// matches any step/stream respectively (not used by the current 10 cases, but
// keeps the helper generally useful).
func anyLogMatches(logs []LogLine, want LogLine, match func(line, pattern string) bool) bool {
	for _, l := range logs {
		if want.Step != "" && l.Step != want.Step {
			continue
		}
		if want.Stream != "" && l.Stream != want.Stream {
			continue
		}
		if match(l.Substring, want.Substring) {
			return true
		}
	}
	return false
}

// matchingStepStreamLines returns the captured line text for every log entry
// matching step/stream, for error messages.
func matchingStepStreamLines(logs []LogLine, step, stream string) []string {
	var out []string
	for _, l := range logs {
		if (step == "" || l.Step == step) && (stream == "" || l.Stream == stream) {
			out = append(out, l.Substring)
		}
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// VariantKey builds the "name@variant" step-status key used by Expectation/
// Observation.StepStatus for a matrix/foreach combination. Non-matrix steps
// use the plain name (variant == "").
func VariantKey(name, variant string) string {
	if variant == "" {
		return name
	}
	return fmt.Sprintf("%s@%s", name, variant)
}
