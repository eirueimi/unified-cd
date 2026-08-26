package dsl

import (
	"sort"
	"strings"

	"github.com/google/cel-go/cel"
	celast "github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/operators"
)

// DeclaredParamKeys returns the param names a Job declares — the set an `if:`
// expression may reference by literal name.
//
// It is the union of two sources, both statically visible in the manifest:
//
//	spec.params.inputs[].name              — the job's declared interface
//	spec.concurrency.orLocks[].name        — SYNTHESIZED as
//	                                         strings.ToUpper(name)+"_LOCK_VALUE"
//
// The or-lock keys matter: the store injects the acquired candidate into the
// run's params under that name (see orLockValues in internal/store/postgres.go),
// so `if: params.ENV_LOCK_VALUE == "blue"` is legitimate and is declared
// nowhere under spec.params.inputs.
//
// This set is NOT the complete set of keys a run can carry. resolveParams in
// internal/controller passes UNDECLARED caller-supplied params through
// unchanged, so a CLI --param, a literal webhook paramsMapping entry, a
// schedule's params, or a `call:` step's with: can each introduce a key this
// job never declared. That is why ValidateConditionParamRefs only enforces the
// set when the job declares one at all — see the note there.
func DeclaredParamKeys(spec Spec) map[string]bool {
	declared := make(map[string]bool, len(spec.Params.Inputs))
	for _, in := range spec.Params.Inputs {
		if in.Name != "" {
			declared[in.Name] = true
		}
	}
	if spec.Concurrency != nil {
		for _, ol := range spec.Concurrency.OrLocks {
			if ol.Name != "" {
				declared[strings.ToUpper(ol.Name)+"_LOCK_VALUE"] = true
			}
		}
	}
	return declared
}

// literalParamRefs returns the param keys the expression selects by LITERAL
// name, i.e. the ones whose spelling is knowable without running the job:
//
//	params.FOO       → FOO
//	params["FOO"]    → FOO
//	params["A" + x]  → nothing (computed; the key is not knowable here)
//	params[k]        → nothing (dynamic)
//	has(params.FOO)  → nothing (a presence test is how you ASK about a key
//	                   that may legitimately be absent; rejecting it would
//	                   break the only safe way to reference an optional param)
//
// The walk is over the CHECKED AST rather than a regexp over the source, so
// `params` inside a string literal, a comment-free but oddly spaced
// `params . FOO`, and a same-named field on some other value are all handled
// by the parser rather than by pattern-guessing.
func literalParamRefs(ast *cel.Ast) []string {
	seen := map[string]bool{}
	nav := celast.NavigateAST(ast.NativeRep())
	for _, node := range celast.MatchDescendants(nav, celast.AllMatcher()) {
		switch node.Kind() {
		case celast.SelectKind:
			sel := node.AsSelect()
			if sel.IsTestOnly() || !isParamsIdent(sel.Operand()) {
				continue
			}
			seen[sel.FieldName()] = true
		case celast.CallKind:
			call := node.AsCall()
			if call.FunctionName() != operators.Index || len(call.Args()) != 2 {
				continue
			}
			if !isParamsIdent(call.Args()[0]) {
				continue
			}
			if call.Args()[1].Kind() != celast.LiteralKind {
				continue
			}
			key, ok := call.Args()[1].AsLiteral().Value().(string)
			if !ok {
				continue
			}
			seen[key] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func isParamsIdent(e celast.Expr) bool {
	return e.Kind() == celast.IdentKind && e.AsIdent() == "params"
}
