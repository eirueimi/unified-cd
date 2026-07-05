package agent

import (
	"context"
	"io"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
)

type fakeRT struct {
	created  int
	removed  int
	lastExec string
}

func (f *fakeRT) Name() string                                                      { return "fake" }
func (f *fakeRT) Available() bool                                                   { return true }
func (f *fakeRT) Pull(context.Context, string) error                                { return nil }
func (f *fakeRT) Run(context.Context, crt.RunSpec, io.Writer, io.Writer) (int, error) { return 0, nil }
func (f *fakeRT) Create(context.Context, crt.CreateSpec) (crt.ContainerHandle, error) {
	f.created++
	return crt.ContainerHandle{ID: "c1"}, nil
}
func (f *fakeRT) Exec(_ context.Context, _ crt.ContainerHandle, spec crt.ExecSpec, _, _ io.Writer) (int, error) {
	f.lastExec = spec.Script
	return 0, nil
}
func (f *fakeRT) CopyIn(context.Context, crt.ContainerHandle, string, string) error  { return nil }
func (f *fakeRT) CopyOut(context.Context, crt.ContainerHandle, string, string) error { return nil }
func (f *fakeRT) Remove(context.Context, crt.ContainerHandle) error                  { f.removed++; return nil }

func TestScopeManagerReusesEnvPerKey(t *testing.T) {
	f := &fakeRT{}
	m := newScopeManager(f)
	ctx := context.Background()
	s := api.ClaimStep{ScopeID: "scope:build", ScopeImage: "img", MatrixKey: ""}

	if _, err := m.ensure(ctx, s, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ensure(ctx, s, nil); err != nil {
		t.Fatal(err)
	}
	if f.created != 1 {
		t.Fatalf("expected 1 Create for same key, got %d", f.created)
	}
	m.closeAll(ctx)
	if f.removed != 1 {
		t.Fatalf("expected 1 Remove, got %d", f.removed)
	}
}

func TestScopeManagerKeyIncludesMatrix(t *testing.T) {
	m := newScopeManager(&fakeRT{})
	a := m.key(api.ClaimStep{ScopeID: "s", MatrixKey: "linux"})
	b := m.key(api.ClaimStep{ScopeID: "s", MatrixKey: "windows"})
	if a == b {
		t.Fatal("matrix variants must have distinct scope keys")
	}
}
