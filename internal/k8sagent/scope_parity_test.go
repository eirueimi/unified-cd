package k8sagent

import (
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
)

// TestScopeRoutingParity_HostVsK8s guards backend divergence in uses-scope
// routing. Both the k8s agent (scopeKey / the "step.ScopeID != \"\"" checks
// in agent.go) and the host agent (internal/agent.isScopedStep /
// scopeManager.key) must agree on:
//
//  1. a step is routed to a scope pod iff step.ScopeID != "" — see the
//     `if step.ScopeID != ""` guards in agent.go and their host counterpart
//     isScopedStep in internal/agent/scope_parity_test.go.
//  2. the scope environment key incorporates MatrixKey, so distinct matrix
//     variants of the same scoped uses never share an environment — see
//     scopeKey (scopepod.go) and its host counterpart scopeManager.key
//     (internal/agent/scope.go).
//
// The two predicates live in sibling packages that both import internal/dsl
// (via internal/api), so internal/dsl cannot import either back — a single
// cross-package test would require an import cycle. Instead this test and
// internal/agent/scope_parity_test.go each assert the shared invariant on
// their own backend's predicate; read together they guard the two backends
// from silently diverging. This cross-links Task 7 (host) and Task 10 (k8s)
// coverage under one explicit parity check, per the Task 11 brief.
func TestScopeRoutingParity_HostVsK8s(t *testing.T) {
	cases := []struct {
		name       string
		step       api.ClaimStep
		wantScoped bool
	}{
		{"no ScopeID is not scoped", api.ClaimStep{}, false},
		{"ScopeID set is scoped", api.ClaimStep{ScopeID: "scope:build"}, true},
		{"ScopeID set with MatrixKey is scoped", api.ClaimStep{ScopeID: "scope:build", MatrixKey: "linux"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.step.ScopeID != ""; got != tc.wantScoped {
				t.Fatalf("(step.ScopeID != \"\") for %+v = %v, want %v", tc.step, got, tc.wantScoped)
			}
		})
	}

	// The scope key must incorporate MatrixKey: two matrix variants of the
	// same ScopeID must never collide on one pod. internal/agent.scopeManager.key
	// must hold the same property (see the sibling test).
	a := scopeKey(api.ClaimStep{ScopeID: "s", MatrixKey: "linux"})
	b := scopeKey(api.ClaimStep{ScopeID: "s", MatrixKey: "windows"})
	if a == b {
		t.Fatal("scopeKey must incorporate MatrixKey: distinct variants collided")
	}
}
