package controller

import (
	"strings"
	"unicode/utf8"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

// maxDisplayNameLength caps the number of runes kept in a run's display
// name after template interpolation. The value is derived from an
// attacker-influenceable source (params can be mapped from webhook
// payloads -- see internal/dsl/types.go's DisplayName doc comment), so it
// has no natural upper bound; 200 is comfortably larger than any label
// that stays readable in a WebUI table cell or a run-detail page header,
// while still bounding storage and layout cost. Exceeding it truncates
// rather than rejects the run: unlike an author-declared identifier
// (see dsl.ValidateName), a display name is cosmetic, so failing run
// creation over a cosmetic overflow would be a worse failure mode than a
// truncated label.
const maxDisplayNameLength = 200

// expandRunDisplayName interpolates a job's spec.displayName template
// with the run's own resolved params, then makes the result safe to
// store and display.
//
// It reuses dsl.ExpandTemplate -- the same Go-template path
// ExpandAgentSelector already uses at this same run-creation moment --
// rather than a second template implementation. That path's
// missingkey=zero option means a reference to an undeclared param (e.g.
// {{ .Params.typo }}) expands to "" rather than erroring; that is not a
// bug to guard against here, it is the same behavior PR #162
// deliberately made if: conditions consistent with, and this function
// stays consistent with both by doing nothing extra for it. A malformed
// template (bad syntax, an unknown function call) is a genuine error and
// is returned to the caller, which aborts run creation exactly like
// ExpandAgentSelector's own template errors do -- a broken displayName:
// template is an authoring mistake in the job, not a runtime input to
// silently swallow.
//
// The expanded string is then run through sanitizeAgentText: params can
// be mapped from webhook payloads (see internal/controller/params.go's
// resolveParams), so the interpolated text is exactly as untrusted as
// agent-submitted log lines, which is what sanitizeAgentText already
// exists to make Postgres-storable. See that function's doc comment for
// why fixing bytes up front beats failing the request over one bad byte.
//
// Finally the result is capped at maxDisplayNameLength runes. An empty
// template (the common case: no spec.displayName declared) short-circuits
// before any of this, so a run with no displayName: costs one string
// comparison, not a template parse.
func expandRunDisplayName(tmpl string, params map[string]string) (string, error) {
	if tmpl == "" {
		return "", nil
	}
	expanded, err := dsl.ExpandTemplate(tmpl, dsl.TemplateData{Params: params})
	if err != nil {
		return "", err
	}
	expanded, _ = sanitizeAgentText(expanded)
	// A template made only of references to undeclared/absent params (e.g.
	// "{{ .Params.a }} {{ .Params.b }}") expands each reference to "" (see
	// the missingkey=zero note above) but leaves any literal whitespace
	// between them intact, producing a non-empty string that is entirely
	// whitespace. That is the same "no display name" outcome as an empty
	// template, not a real one-space label -- a run whose entire link text
	// is a single space is effectively unreachable in the WebUI run list.
	// Treat an all-whitespace expansion the same as the tmpl == "" fast
	// path above. A legitimately non-empty name (any real, non-whitespace
	// content) is returned exactly as expanded -- this only collapses the
	// degenerate all-whitespace case, it does not trim real names.
	if strings.TrimSpace(expanded) == "" {
		return "", nil
	}
	return truncateDisplayName(expanded, maxDisplayNameLength), nil
}

// truncateDisplayName returns s unchanged if it has at most limit runes,
// otherwise the first limit runes followed by a single "…" so a truncated
// name is visually distinguishable from one that happens to be exactly
// limit runes long. Operates on runes, not bytes, so a cap never lands
// mid-multi-byte-character -- sanitizeAgentText has already guaranteed s
// is valid UTF-8 by the time this runs, so a rune-by-rune walk here is
// safe and cheap.
func truncateDisplayName(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	runes := []rune(s)
	return string(runes[:limit]) + "…"
}
