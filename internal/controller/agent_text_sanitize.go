package controller

import (
	"strings"
	"unicode/utf8"
)

// sanitizeAgentText makes s safe to store in a PostgreSQL text column.
//
// PostgreSQL's text type rejects two things outright, both with SQLSTATE
// 22021 ("invalid byte sequence for encoding UTF8"):
//
//   - A NUL byte (0x00). NUL is a legitimate Unicode code point (U+0000) and
//     so passes utf8.ValidString, but PostgreSQL still refuses it because it
//     collides with NUL-terminated C strings internally.
//   - Any byte sequence that is not valid UTF-8 at all -- a stray high byte
//     from a tool that isn't UTF-8-clean, a truncated multi-byte sequence,
//     an overlong encoding, and so on.
//
// A single such byte anywhere in an agent-submitted line or output value
// fails the whole INSERT. On the log path that is not just one dropped
// line: LogPusher.flushPendingLocked (internal/agent/runner.go) retries a
// failed batch oldest-first and stops at the first failure, so the bad byte
// wedges that run's entire log delivery until drop-oldest eviction discards
// the batch. Handling only NUL and not general invalid UTF-8 (or vice
// versa) would still leave that wedge reachable, just via a different input
// byte -- so both are handled here, by the same pass.
//
// This is the controller's ingestion boundary, not the store: untrusted
// step output (stdout/stderr lines, captured `outputs:` values) enters the
// system exactly here, on the agent-authenticated write paths in
// api_agent.go. The store's other AppendLog/SetStepOutput/SetRunOutput
// callers (the reaper, the scheduler, the claim-build-failure path, etc.)
// write controller-generated strings -- Go error messages and static text
// -- that are never agent-supplied and so can never carry a raw NUL or
// arbitrary invalid-UTF-8 byte. Sanitizing in the store as well would
// duplicate this cost on every one of those calls for no benefit, and would
// still need this same controller-side fix for the endpoints that decode
// straight from the agent's JSON body before ever reaching the store.
//
// Each offending byte is replaced with U+FFFD (the Unicode replacement
// character), in place. That makes the alteration visible exactly where it
// happened -- a line already containing "�" reads unambiguously as
// "something here was not valid text" -- without prefixing or annotating
// the line with any separate marker. A marker on every altered line would
// be the dominant content of a binary blob accidentally piped to stdout;
// the replacement character alone carries the same signal with none of that
// noise. Bytes are dropped silently only in the sense that they are not
// reproduced verbatim -- they cannot be, since they are exactly the bytes
// PostgreSQL refuses to store -- not in the sense that the line disappears
// or is unmarked.
//
// This runs on every line submitted by every agent, including through the
// bulk log endpoint that exists specifically to keep that path cheap (see
// Postgres.AppendLogs), so the overwhelmingly common case -- a line that is
// already clean -- must cost next to nothing. The fast path is two linear,
// allocation-free scans: an IndexByte for a NUL and utf8.ValidString for
// general validity. Only a line that actually needs rewriting pays for the
// rune-by-rune pass below.
func sanitizeAgentText(s string) (out string, changed bool) {
	if strings.IndexByte(s, 0) < 0 && utf8.ValidString(s) {
		return s, false
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == 0 {
			b.WriteRune(utf8.RuneError)
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size <= 1 {
			// A genuinely invalid encoding decodes as (RuneError, 1) or
			// (RuneError, 0); DecodeRuneInString never returns size 0 for a
			// non-empty input, but guard <= 1 rather than == 1 to stay
			// correct if that ever changes. A line that legitimately
			// contains the replacement character itself decodes as
			// (RuneError, 3) -- size 3, the correct UTF-8 encoding of
			// U+FFFD -- and is copied through unchanged, not misdetected
			// here.
			b.WriteRune(utf8.RuneError)
			i++
			continue
		}
		b.WriteString(s[i : i+size])
		i += size
	}
	return b.String(), true
}
