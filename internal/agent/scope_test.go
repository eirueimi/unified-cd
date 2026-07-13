package agent

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeRT struct {
	created    int
	removed    int
	lastExec   string
	lastCreate crt.CreateSpec
	// lastExecSpec keeps the full ExecSpec (notably Shell) from the most
	// recent Exec call, for tests asserting on shell-argv threading
	// (shell_threading_test.go).
	lastExecSpec crt.ExecSpec
}

func (f *fakeRT) Name() string                                                      { return "fake" }
func (f *fakeRT) Available() bool                                                   { return true }
func (f *fakeRT) Pull(context.Context, string) error                                { return nil }
func (f *fakeRT) Run(context.Context, crt.RunSpec, io.Writer, io.Writer) (int, error) { return 0, nil }
func (f *fakeRT) Create(_ context.Context, spec crt.CreateSpec) (crt.ContainerHandle, error) {
	f.created++
	f.lastCreate = spec
	return crt.ContainerHandle{ID: "c1"}, nil
}
func (f *fakeRT) Exec(_ context.Context, _ crt.ContainerHandle, spec crt.ExecSpec, _, _ io.Writer) (int, error) {
	f.lastExec = spec.Script
	f.lastExecSpec = spec
	return 0, nil
}
func (f *fakeRT) CopyIn(context.Context, crt.ContainerHandle, string, string) error  { return nil }
func (f *fakeRT) CopyOut(context.Context, crt.ContainerHandle, string, string) error { return nil }
func (f *fakeRT) Remove(context.Context, crt.ContainerHandle) error                  { f.removed++; return nil }
func (f *fakeRT) Logs(context.Context, crt.ContainerHandle, io.Writer, io.Writer) error {
	return nil
}
func (f *fakeRT) ExitCode(context.Context, crt.ContainerHandle) (int, error) { return 0, nil }

func TestScopeManagerReusesEnvPerKey(t *testing.T) {
	f := &fakeRT{}
	m := newScopeManager(f, "")
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

// TestScopeManagerEnsure_KeepAliveCommand is the regression test for the
// sidecar-sleep-infinity fix: since CreateSpec.Entrypoint now defaults to nil
// (image entrypoint) instead of the runtime hardcoding a keep-alive, the
// uses-scope container — a step exec target, not a service sidecar — must
// set Entrypoint explicitly so it doesn't regress into running its image's
// default entrypoint and exiting immediately. The keep-alive itself is now
// the ucd-sh shim's pause mode (Component 4 of the step-shell-shim design),
// not sleep infinity.
func TestScopeManagerEnsure_KeepAliveCommand(t *testing.T) {
	f := &fakeRT{}
	m := newScopeManager(f, "")
	s := api.ClaimStep{ScopeID: "scope:build", ScopeImage: "img", MatrixKey: ""}

	if _, err := m.ensure(context.Background(), s, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.lastCreate.Entrypoint; len(got) != 2 || got[0] != "/.ucd/ucd-sh" || got[1] != "pause" {
		t.Fatalf("expected scope container Entrypoint = [/.ucd/ucd-sh pause], got %v", got)
	}
}

// TestScopeManagerEnsure_MountsToolsDirReadOnly verifies the scope container
// carries the read-only /.ucd shim mount when toolsDir is set (mirroring the
// claim pod's ucdToolsMount — see claim_pod.go).
func TestScopeManagerEnsure_MountsToolsDirReadOnly(t *testing.T) {
	f := &fakeRT{}
	m := newScopeManager(f, "/host/tools")
	s := api.ClaimStep{ScopeID: "scope:build", ScopeImage: "img"}

	if _, err := m.ensure(context.Background(), s, nil); err != nil {
		t.Fatal(err)
	}
	require.Len(t, f.lastCreate.Mounts, 1)
	assert.Equal(t, crt.Mount{HostPath: "/host/tools", ContainerPath: "/.ucd", ReadOnly: true}, f.lastCreate.Mounts[0])
}

func TestScopeManagerKeyIncludesMatrix(t *testing.T) {
	m := newScopeManager(&fakeRT{}, "")
	a := m.key(api.ClaimStep{ScopeID: "s", MatrixKey: "linux"})
	b := m.key(api.ClaimStep{ScopeID: "s", MatrixKey: "windows"})
	if a == b {
		t.Fatal("matrix variants must have distinct scope keys")
	}
}

// counterRT is a concurrency-safe fake ContainerRuntime: every method uses
// atomics/mutex-free counters that are safe to increment from many
// goroutines at once (unlike the plain-int fakeRT above, which is only used
// from single-threaded tests). Create sleeps briefly to widen the window in
// which a racy ensure/getScopes implementation could double-create.
type counterRT struct {
	createCalls atomic.Int64
	removeCalls atomic.Int64
	execCalls   atomic.Int64

	mu      sync.Mutex
	created map[string]int // keyed by Image, counts Create calls per distinct image
}

func (c *counterRT) Name() string      { return "counter" }
func (c *counterRT) Available() bool   { return true }
func (c *counterRT) Pull(context.Context, string) error { return nil }
func (c *counterRT) Run(context.Context, crt.RunSpec, io.Writer, io.Writer) (int, error) {
	return 0, nil
}
func (c *counterRT) Create(_ context.Context, spec crt.CreateSpec) (crt.ContainerHandle, error) {
	c.createCalls.Add(1)
	c.mu.Lock()
	if c.created == nil {
		c.created = map[string]int{}
	}
	c.created[spec.Image]++
	n := c.created[spec.Image]
	c.mu.Unlock()
	return crt.ContainerHandle{ID: fmt.Sprintf("%s-%d", spec.Image, n)}, nil
}
func (c *counterRT) Exec(context.Context, crt.ContainerHandle, crt.ExecSpec, io.Writer, io.Writer) (int, error) {
	c.execCalls.Add(1)
	return 0, nil
}
func (c *counterRT) CopyIn(context.Context, crt.ContainerHandle, string, string) error  { return nil }
func (c *counterRT) CopyOut(context.Context, crt.ContainerHandle, string, string) error { return nil }
func (c *counterRT) Remove(context.Context, crt.ContainerHandle) error {
	c.removeCalls.Add(1)
	return nil
}
func (c *counterRT) Logs(context.Context, crt.ContainerHandle, io.Writer, io.Writer) error {
	return nil
}
func (c *counterRT) ExitCode(context.Context, crt.ContainerHandle) (int, error) { return 0, nil }

// TestScopeManagerEnsure_ConcurrentSameKey exercises many goroutines racing to
// ensure() the *same* scope key at once, as happens when several members of a
// parallel: group share a ScopeID/MatrixKey. Must be run with -race. Also
// asserts the "one Create per key" semantics survive concurrency.
func TestScopeManagerEnsure_ConcurrentSameKey(t *testing.T) {
	rt := &counterRT{}
	m := newScopeManager(rt, "")
	step := api.ClaimStep{ScopeID: "scope:shared", ScopeImage: "img:shared"}

	const n = 50
	var wg sync.WaitGroup
	handles := make([]crt.ContainerHandle, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			h, err := m.ensure(context.Background(), step, nil)
			handles[idx] = h
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: unexpected error: %v", i, err)
		}
	}
	for i := 1; i < n; i++ {
		if handles[i] != handles[0] {
			t.Fatalf("expected all goroutines to observe the same container handle, got %+v and %+v", handles[0], handles[i])
		}
	}
	if got := rt.createCalls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 Create for a shared key under concurrency, got %d", got)
	}
}

// TestScopeManagerEnsure_ConcurrentDistinctKeys races goroutines that each
// provision a distinct (ScopeID, MatrixKey) pair, plus concurrent closeAll
// calls, to catch data races / "concurrent map writes" panics in the open
// map. Must be run with -race.
func TestScopeManagerEnsure_ConcurrentDistinctKeys(t *testing.T) {
	rt := &counterRT{}
	m := newScopeManager(rt, "")

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			step := api.ClaimStep{
				ScopeID:    "scope:build",
				ScopeImage: "img",
				MatrixKey:  fmt.Sprintf("variant-%d", idx),
			}
			if _, err := m.ensure(context.Background(), step, nil); err != nil {
				t.Errorf("ensure goroutine %d: %v", idx, err)
			}
		}(i)
	}
	wg.Wait()

	if got := rt.createCalls.Load(); got != n {
		t.Fatalf("expected %d distinct Create calls for %d distinct keys, got %d", n, n, got)
	}

	// Concurrent teardown must not race on the open map either.
	var wg2 sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			m.closeAll(context.Background())
		}()
	}
	wg2.Wait()

	if got := rt.removeCalls.Load(); got != n {
		t.Fatalf("expected %d Remove calls after concurrent closeAll, got %d", n, got)
	}
}
