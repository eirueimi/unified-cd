package agent

import (
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
)

// TestScopeRoutingParity_HostVsK8s guards backend divergence in uses-scope
// routing. Both the host agent (isScopedStep, this package) and the k8s
// agent (internal/k8sagent.scopeKey / the "step.ScopeID != \"\"" check in
// internal/k8sagent/agent.go) must agree on:
//
//  1. a step is routed to a scope environment iff step.ScopeID != "" — see
//     isScopedStep (scope.go) and its k8sagent counterpart in
//     internal/k8sagent/scope_parity_test.go.
//  2. the scope environment key incorporates MatrixKey, so distinct matrix
//     variants of the same scoped uses never share an environment — see
//     scopeManager.key (scope.go) and its k8sagent counterpart scopeKey
//     (internal/k8sagent/scopepod.go).
//
// The two predicates live in sibling packages that both import internal/dsl
// (via internal/api), so internal/dsl cannot import either back — a single
// cross-package test would require an import cycle. Instead this test and
// internal/k8sagent/scope_parity_test.go each assert the shared invariant on
// their own backend's predicate; read together they guard the two backends
// from silently diverging. This cross-links Task 7 (host) and Task 10 (k8s)
// coverage under one explicit parity check, per the Task 11 brief.
func TestScopeRoutingParity_HostVsK8s(t *testing.T) {
	m := newScopeManager(&fakeRT{}, "")

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
			if got := isScopedStep(tc.step); got != tc.wantScoped {
				t.Fatalf("isScopedStep(%+v) = %v, want %v", tc.step, got, tc.wantScoped)
			}
		})
	}

	// The scope key must incorporate MatrixKey: two matrix variants of the
	// same ScopeID must never collide on one environment. k8sagent.scopeKey
	// must hold the same property (see the sibling test).
	a := m.key(api.ClaimStep{ScopeID: "s", MatrixKey: "linux"})
	b := m.key(api.ClaimStep{ScopeID: "s", MatrixKey: "windows"})
	if a == b {
		t.Fatal("scopeManager.key must incorporate MatrixKey: distinct variants collided")
	}
}
