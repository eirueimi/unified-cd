// Package shim implements the ucd-sh runtime: a mvdan.cc/sh-backed script
// interpreter used as the default `run:` step shell, plus the "pause"
// keep-alive and "--install" self-copy modes.
package shim

import (
	"fmt"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// sanitizedSignals is the conservative set of signal condition words (after
// upper-casing and stripping a leading "SIG") that mvdan.cc/sh's trap
// builtin does not support and that SanitizeTraps therefore strips. The set
// intentionally excludes anything not on it: unrecognized words are left in
// place so the interpreter's own "invalid signal specification" error fires
// naturally, which is the correct behavior for typos or exotic conditions we
// don't understand.
var sanitizedSignals = map[string]bool{
	"INT":   true,
	"TERM":  true,
	"HUP":   true,
	"QUIT":  true,
	"KILL":  true,
	"USR1":  true,
	"USR2":  true,
	"PIPE":  true,
	"ALRM":  true,
	"CHLD":  true,
	"WINCH": true,
}

// isRemovedCondition reports whether word is a trap condition that
// mvdan.cc/sh's interpreter does not support (i.e. anything other than the
// literal EXIT/ERR it implements). Matching is case-insensitive and
// recognizes a leading "SIG" prefix and bare signal numbers.
func isRemovedCondition(word string) bool {
	upper := strings.ToUpper(word)
	if upper == "EXIT" || upper == "ERR" {
		return false
	}
	upper = strings.TrimPrefix(upper, "SIG")
	if sanitizedSignals[upper] {
		return true
	}
	if _, err := strconv.Atoi(word); err == nil {
		return true
	}
	return false
}

// SanitizeTraps rewrites every `trap` call in f in place, removing
// condition words that mvdan.cc/sh's interpreter does not support (anything
// but EXIT/ERR). mvdan/sh's trap builtin errors with exit status 2 on any
// other condition, which under `set -e` kills the script before it does
// anything; stripping the unsupported conditions degrades gracefully
// instead.
//
// warn is invoked once per removed condition word with a human-readable
// message naming the signal and recommending `shell: [bash, -lc]` for
// scripts that need real signal traps. If every condition word on a call is
// removed, the call is replaced with the `true` no-op builtin (there being
// nothing left for `trap` to attach the handler to).
func SanitizeTraps(f *syntax.File, warn func(msg string)) {
	syntax.Walk(f, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok {
			return true
		}
		// A trap call's first word is the literal command name "trap"; the
		// second (if present) is the handler/action word, and the rest are
		// condition words (signal names).
		if len(call.Args) < 2 || call.Args[0].Lit() != "trap" {
			return true
		}

		// Bare two-word form: `trap SIGNAL` has NO handler word — POSIX
		// treats the single operand as a condition and resets it to its
		// default disposition (equivalent to `trap - SIGNAL`). mvdan's trap
		// builtin still only accepts EXIT/ERR there, so an unsupported
		// signal must be stripped like any other condition — otherwise the
		// call errors with status 2 and kills `set -e` scripts, the exact
		// failure this sanitizer exists to prevent. EXIT/ERR resets and
		// unclassifiable words (flags like -l, expansions) pass through.
		if len(call.Args) == 2 {
			word := call.Args[1].Lit()
			if word != "" && isRemovedCondition(word) {
				if warn != nil {
					warn(fmt.Sprintf(
						"trap: removed unsupported signal %q (mvdan.cc/sh traps only support EXIT/ERR); use shell: [bash, -lc] if this step needs real signal traps",
						word,
					))
				}
				call.Args = []*syntax.Word{literalWord("true")}
				call.Assigns = nil
			}
			return true
		}

		kept := make([]*syntax.Word, 0, len(call.Args))
		kept = append(kept, call.Args[0], call.Args[1])
		anyConditionKept := false
		for _, w := range call.Args[2:] {
			word := w.Lit()
			if word == "" {
				// Not a plain literal (e.g. a parameter expansion) - we
				// can't classify it statically, so leave it alone.
				kept = append(kept, w)
				anyConditionKept = true
				continue
			}
			if isRemovedCondition(word) {
				if warn != nil {
					warn(fmt.Sprintf(
						"trap: removed unsupported signal %q (mvdan.cc/sh traps only support EXIT/ERR); use shell: [bash, -lc] if this step needs real signal traps",
						word,
					))
				}
				continue
			}
			kept = append(kept, w)
			anyConditionKept = true
		}

		if !anyConditionKept {
			// Every condition was stripped: there is nothing left for this
			// trap call to attach to, so replace it with a no-op.
			call.Args = []*syntax.Word{literalWord("true")}
			call.Assigns = nil
			return true
		}

		call.Args = kept
		return true
	})
}

func literalWord(s string) *syntax.Word {
	return &syntax.Word{Parts: []syntax.WordPart{&syntax.Lit{Value: s}}}
}
