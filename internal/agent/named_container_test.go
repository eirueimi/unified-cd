package agent

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
)

func podTmpl(containers ...map[string]any) *dsl.PodTemplate {
	cs := make([]any, len(containers))
	for i, c := range containers {
		cs[i] = c
	}
	return &dsl.PodTemplate{Spec: map[string]any{"containers": cs}}
}

func TestNamedContainerDef_Found(t *testing.T) {
	pt := podTmpl(
		map[string]any{"name": "other", "image": "busybox"},
		map[string]any{
			"name":  "tools",
			"image": "node:20",
			"env":   []any{map[string]any{"name": "FOO", "value": "bar"}},
			"resources": map[string]any{
				"limits": map[string]any{"cpu": "500m", "memory": "256Mi"},
			},
		},
	)
	def, err := namedContainerDef(pt, "tools")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if def.Name != "tools" || def.Image != "node:20" {
		t.Fatalf("unexpected def: %+v", def)
	}
	if len(def.Env) != 1 || def.Env[0] != "FOO=bar" {
		t.Fatalf("env = %v, want [FOO=bar]", def.Env)
	}
	if def.CPULimit != "0.5" {
		t.Fatalf("CPULimit = %q, want 0.5", def.CPULimit)
	}
	if def.MemLimit != "268435456" {
		t.Fatalf("MemLimit = %q, want 268435456", def.MemLimit)
	}
}

func TestNamedContainerDef_NoPodTemplate(t *testing.T) {
	if _, err := namedContainerDef(nil, "tools"); err == nil {
		t.Fatal("expected error when podTemplate is nil")
	}
}

func TestNamedContainerDef_UnknownName(t *testing.T) {
	pt := podTmpl(map[string]any{"name": "tools", "image": "node:20"})
	if _, err := namedContainerDef(pt, "missing"); err == nil {
		t.Fatal("expected error when container name is absent")
	}
}

func TestLimitStrings(t *testing.T) {
	cpu, mem := limitStrings("500m", "256Mi")
	if cpu != "0.5" {
		t.Fatalf("cpu = %q, want 0.5", cpu)
	}
	if mem != "268435456" {
		t.Fatalf("mem = %q, want 268435456", mem)
	}
	if c, m := limitStrings("", ""); c != "" || m != "" {
		t.Fatalf("empty in must yield empty out, got %q %q", c, m)
	}
}

// recordingRT records every CreateSpec it is asked to Create, and is safe for
// concurrent use.
type recordingRT struct {
	mu      sync.Mutex
	specs   []crt.CreateSpec
	creates atomic.Int64
	removes atomic.Int64
}

func (r *recordingRT) Name() string                                   { return "recording" }
func (r *recordingRT) Available() bool                                { return true }
func (r *recordingRT) Pull(context.Context, string) error            { return nil }
func (r *recordingRT) Run(context.Context, crt.RunSpec, io.Writer, io.Writer) (int, error) {
	return 0, nil
}
func (r *recordingRT) Create(_ context.Context, spec crt.CreateSpec) (crt.ContainerHandle, error) {
	r.creates.Add(1)
	r.mu.Lock()
	r.specs = append(r.specs, spec)
	n := len(r.specs)
	r.mu.Unlock()
	return crt.ContainerHandle{ID: fmt.Sprintf("c%d", n)}, nil
}
func (r *recordingRT) Exec(context.Context, crt.ContainerHandle, crt.ExecSpec, io.Writer, io.Writer) (int, error) {
	return 0, nil
}
func (r *recordingRT) CopyIn(context.Context, crt.ContainerHandle, string, string) error  { return nil }
func (r *recordingRT) CopyOut(context.Context, crt.ContainerHandle, string, string) error { return nil }
func (r *recordingRT) Remove(context.Context, crt.ContainerHandle) error {
	r.removes.Add(1)
	return nil
}

func TestNamedContainerManager_ReusesPerNameAndBindMounts(t *testing.T) {
	rt := &recordingRT{}
	m := newNamedContainerManager(rt, "/host/ws", "/workspace")
	ctx := context.Background()
	def := containerDef{Name: "tools", Image: "node:20"}

	h1, err := m.ensure(ctx, def)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := m.ensure(ctx, def)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("expected same handle for same name, got %v and %v", h1, h2)
	}
	if got := rt.creates.Load(); got != 1 {
		t.Fatalf("expected 1 Create for a reused name, got %d", got)
	}
	spec := rt.specs[0]
	if spec.WorkDir != "/workspace" {
		t.Fatalf("WorkDir = %q, want /workspace", spec.WorkDir)
	}
	if len(spec.Mounts) != 1 || spec.Mounts[0].HostPath != "/host/ws" || spec.Mounts[0].ContainerPath != "/workspace" {
		t.Fatalf("Mounts = %+v, want one /host/ws:/workspace", spec.Mounts)
	}

	m.closeAll(ctx)
	if got := rt.removes.Load(); got != 1 {
		t.Fatalf("expected 1 Remove, got %d", got)
	}
}

// TestNamedContainerManager_ConcurrentSameName must be run with -race: many
// goroutines racing to ensure() the same name (parallel: steps sharing a
// container) must produce exactly one Create.
func TestNamedContainerManager_ConcurrentSameName(t *testing.T) {
	rt := &recordingRT{}
	m := newNamedContainerManager(rt, "/host/ws", "/workspace")
	def := containerDef{Name: "tools", Image: "node:20"}

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.ensure(context.Background(), def); err != nil {
				t.Errorf("ensure: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := rt.creates.Load(); got != 1 {
		t.Fatalf("expected exactly 1 Create under concurrency, got %d", got)
	}
}
