package k8sagent

import (
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
)

func TestScopePodKeyDistinctPerMatrix(t *testing.T) {
	a := scopeKey(api.ClaimStep{ScopeID: "s", MatrixKey: "linux"})
	b := scopeKey(api.ClaimStep{ScopeID: "s", MatrixKey: "windows"})
	if a == b || a == "" {
		t.Fatalf("bad scope keys: %q %q", a, b)
	}
}
