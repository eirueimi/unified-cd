package agent

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	crt "github.com/eirueimi/unified-cd/internal/runtime"
)

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

func (r *recordingRT) Name() string                       { return "recording" }
func (r *recordingRT) Available() bool                    { return true }
func (r *recordingRT) Pull(context.Context, string) error { return nil }
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
func (r *recordingRT) Logs(context.Context, crt.ContainerHandle, io.Writer, io.Writer) error {
	return nil
}
func (r *recordingRT) ExitCode(context.Context, crt.ContainerHandle) (int, error) { return 0, nil }
