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

	return resolved, nil
}

// validateWebhookPayloadMappedParams enforces that every webhook paramsMapping
// entry whose template actually reads from the request payload (contains
// ".Payload") is declared by the TARGET JOB's params.inputs with either a
// pattern: or an explicit unvalidated: true opt-out.
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
// reads the payload is author-controlled, not attacker-controlled, and is not
// subject to this check.
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
		if !strings.Contains(mapping[param], ".Payload") {
			continue
		}
		in, declared := byName[param]
		if declared && (in.Pattern != "" || in.Unvalidated) {
			continue
		}
		return fmt.Errorf(
			"webhook receiver %q: param %q is mapped from the request payload but job %q declares no pattern for it (add pattern: to the input, or unvalidated: true to accept it explicitly)",
			receiverName, param, jobName)
	}
	return nil
}
