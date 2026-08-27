package controller

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

// resolveParams validates the caller-supplied params against the Job's declared
// inputs and fills in defaults for any that were omitted.
//
//   - A param omitted by the caller (or supplied as an explicit empty string)
//     but carrying a `default` is injected with that default (formatted via
//     fmt.Sprintf("%v", ...)). An explicit empty value is treated as "unset" so
//     that `working_dir: ""` falls back to the declared default rather than
//     silently overriding it with "".
//   - A param declared `required: true` with no caller-supplied (non-empty) value
//     and no default causes an error naming the missing param.
//   - Params not declared as inputs are passed through unchanged.
//   - A param declared with `pattern:` must have its resolved value (whether
//     caller-supplied or filled in from `default:`) match the pattern, or the
//     call fails. This is the shared choke point every param source flows
//     through (webhook mapping, CLI --param, call:/uses: with:, schedule
//     params) — and param values are interpolated into step shell text, so an
//     externally-sourced value is a command-injection vector unless
//     constrained. The rejected value is never echoed in the error, since it
//     may itself carry an injection payload into operator-read logs. A
//     malformed pattern is itself an error rather than silently matching
//     everything.
//
// The input map is not mutated; a new map is returned.
func resolveParams(inputs []dsl.Input, supplied map[string]string) (map[string]string, error) {
	resolved := make(map[string]string, len(supplied)+len(inputs))
	for k, v := range supplied {
		resolved[k] = v
	}

	var missing []string
	for _, in := range inputs {
		// Presence with a non-empty value keeps the caller's value. An explicitly
		// empty value ("") is treated as unset so `default:` still applies — this
		// matches documented behavior and avoids an empty string silently bypassing
		// a declared default. (Params without a declared default keep the caller's
		// empty string, since there is nothing to fall back to.)
		if v, ok := resolved[in.Name]; ok && v != "" {
			continue
		}
		if in.Default != nil {
			resolved[in.Name] = fmt.Sprintf("%v", in.Default)
			continue
		}
		if in.Required {
			missing = append(missing, in.Name)
		}
	}

	if len(missing) == 1 {
		return nil, fmt.Errorf("missing required param: %s", missing[0])
	}
	if len(missing) > 1 {
		return nil, fmt.Errorf("missing required params: %v", missing)
	}

	for _, in := range inputs {
		if in.Pattern == "" {
			continue
		}
		value, ok := resolved[in.Name]
		if !ok {
			continue
		}
		re, err := regexp.Compile(in.Pattern)
		if err != nil {
			return nil, fmt.Errorf("param %q: invalid pattern %q: %w", in.Name, in.Pattern, err)
		}
		if !re.MatchString(value) {
			// Do not echo the rejected value: it may carry an injection payload
			// into logs read by an operator.
			return nil, fmt.Errorf("param %q does not match required pattern %q", in.Name, in.Pattern)
		}
	}

	// A param declared with `choices:` must have its resolved value be one of
	// the listed choices. dsl.parse already guarantees (by the time an Input
	// reaches here) that Choices, if non-empty, only appears on a string/int
	// input, is never combined with Pattern, has no duplicates, and that
	// Default (if set) is itself a member — so this loop only ever has to
	// reject a value the CALLER supplied.
	//
	// Unlike the Pattern loop above, an explicitly empty resolved value ("")
	// is skipped here even though it IS present in the map (ok == true). This
	// is deliberate and choices-specific: the Web UI's <select> naturally
	// represents "no selection" as an empty string, and this package already
	// treats an empty string as "unset" for defaulting purposes (see this
	// function's doc comment, and TestResolveParams_ExplicitEmptyValue_NoDefault_KeptEmpty
	// in params_test.go — an optional param with no default and an explicit
	// "" is kept as "" without error). If Choices behaved like Pattern here,
	// that same optional/no-default/unselected case would be rejected as "not
	// an allowed value", which is wrong: the param is genuinely unset, not
	// invalid. (A `required: true` choices param with no value never reaches
	// this loop with an empty string in the first place — the missing-required
	// check above already turned it into an error, or a default already filled
	// it in.) The Pattern loop's own behavior is intentionally left alone: a
	// pattern is a syntax constraint an empty string can still violate, and
	// changing that here would be an unrelated, wider behavior change.
	for _, in := range inputs {
		if len(in.Choices) == 0 {
			continue
		}
		value, ok := resolved[in.Name]
		if !ok || value == "" {
			continue
		}
		allowed := false
		for _, c := range in.Choices {
			if value == c {
				allowed = true
				break
			}
		}
		if !allowed {
			// Do not echo the rejected value, for the same reason as the
			// Pattern check above: it may carry an injection payload into
			// operator-read logs. Naming the ALLOWED values (not the rejected
			// one) is the whole point of choices: it tells the caller what IS
			// valid without repeating back anything attacker-controlled.
			return nil, fmt.Errorf("param %q must be one of: %s", in.Name, strings.Join(in.Choices, ", "))
		}
	}

	return resolved, nil
}

// validateWebhookPayloadMappedParams enforces that every webhook paramsMapping
// entry that evaluates as a Go template — i.e. contains a "{{" action — is
// declared by the TARGET JOB's params.inputs with a pattern:, a choices:
// allow-list, or an explicit unvalidated: true opt-out.
//
// choices: satisfies this gate on its own, with no pattern: required,
// because it is strictly stronger than any pattern: regex: a regex only
// shapes the SYNTAX of a value (which characters may appear, in which
// order), while choices enumerates the value's exact allowed MEMBERS. A
// pattern can be satisfied by infinitely many strings — including, if the
// author is not careful, ones that still carry shell metacharacters a
// slightly-too-permissive regex let through. A choices: value, by contrast,
// can only ever be one of a small, fixed, author-chosen set of literal
// strings; there is no way for an attacker-controlled webhook payload to
// smuggle an injection payload through it, because whichever member it
// picks IS the entire value — there is no room for it to also carry
// anything else. So choices: gives the exact same guarantee this gate
// exists to enforce ("the resolved value can't carry an injection payload
// into a step's shell text"), by a strictly tighter mechanism than pattern:
// already provides.
//
// Why here, and why not at receiver-parse time: a valid HMAC/token signature
// on a webhook only proves who sent the request, not that its content is
// benign. A GitHub push or pull_request payload has fields fully controlled by
// whoever can open a PR or push a branch (e.g. .Payload.pull_request.head.ref)
// — the same class of vulnerability as GitHub Actions script injection — so an
// unconstrained payload-mapped param is a command-injection vector once
// dsl.ExpandTemplate interpolates it into a step's `sh -lc` text.
//
// The check cannot live in dsl.ParseWebhookReceiver: a WebhookReceiver is
// parsed in isolation and has no access to the Job it targets (which may not
// exist yet, or may be edited independently after the receiver is applied).
// It runs here instead, at the point a live webhook delivery resolves params
// against the job's current spec, immediately before the Run is created —
// the same "validate the resolved, cross-resource picture right before it is
// acted on" pattern dsl.ValidateContainerReferences follows for a run's
// container references. A literal mapping (e.g. `tag: "myapp"`) that never
// evaluates a template is author-controlled, not attacker-controlled, and is
// not subject to this check.
//
// Why "contains {{" rather than enumerating reference forms (".Payload",
// ".Headers", ...): WebhookTemplateData's only field is Payload, so `{{ . }}`
// and `{{ $ }}` render the ENTIRE attacker-controlled payload while
// containing neither substring — an empirically confirmed fail-open bypass
// (`tpl="{{ . }}"` rendered the full payload map, including injected shell
// metacharacters). Enumerating substrings is inherently a denylist against an
// open-ended template language: any field access, range, or future template
// variable that doesn't happen to spell ".Payload" or ".Headers" slips
// through. The only sound question is "does this template evaluate at all,
// reading from data the guard cannot see the shape of" — so any "{{" is
// treated as untrusted output unless the job explicitly opts in. This is
// strictly stronger than the substring list it replaces: every string the old
// check caught still contains "{{" (a Go template action needs the
// delimiter), plus it now also catches the whole-payload dot forms.
func validateWebhookPayloadMappedParams(receiverName string, mapping map[string]string, inputs []dsl.Input, jobName string) error {
	byName := make(map[string]dsl.Input, len(inputs))
	for _, in := range inputs {
		byName[in.Name] = in
	}

	params := make([]string, 0, len(mapping))
	for k := range mapping {
		params = append(params, k)
	}
	sort.Strings(params)

	for _, param := range params {
		tmpl := mapping[param]
		// Any template action is untrusted output until proven otherwise: the
		// mapping is evaluated against WebhookTemplateData, which exposes the
		// full (attacker-controlled) request payload, so a template that
		// evaluates at all — regardless of which field(s) it happens to
		// reference — requires pattern: or unvalidated: true. A literal value
		// with no "{{" can never read the payload and is author-controlled.
		if !strings.Contains(tmpl, "{{") {
			continue
		}
		in, declared := byName[param]
		if declared && (in.Pattern != "" || in.Unvalidated || len(in.Choices) > 0) {
			continue
		}
		return fmt.Errorf(
			"webhook receiver %q: param %q is mapped from the request payload but job %q declares no pattern for it (add pattern: to the input, choices: to restrict it to a fixed set of values, or unvalidated: true to accept it explicitly)",
			receiverName, param, jobName)
	}
	return nil
}
