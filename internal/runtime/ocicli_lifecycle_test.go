// internal/runtime/ocicli_lifecycle_test.go
package runtime

import (
	"context"
	"os/exec"
	"testing"
)

func withFakeExec(t *testing.T, record *[][]string) {
	t.Helper()
	orig := execCommand
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		*record = append(*record, append([]string{name}, args...))
		// `true` exists on the test host (Linux/macOS/Git-Bash) and exits 0.
		return orig(ctx, "true")
	}
	t.Cleanup(func() { execCommand = orig })
}

func TestOCICLICopyOutArgv(t *testing.T) {
	var rec [][]string
	withFakeExec(t, &rec)
	r := &ociCLI{bin: "docker"}
	if err := r.CopyOut(context.Background(), ContainerHandle{ID: "abc"}, "/out/app", "/tmp/app"); err != nil {
		t.Fatalf("CopyOut: %v", err)
	}
	got := rec[0]
	want := []string{"docker", "cp", "abc:/out/app", "/tmp/app"}
	if len(got) != len(want) {
		t.Fatalf("argv = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv = %v, want %v", got, want)
		}
	}
}

func TestOCICLICopyInArgv(t *testing.T) {
	var rec [][]string
	withFakeExec(t, &rec)
	r := &ociCLI{bin: "podman"}
	if err := r.CopyIn(context.Background(), ContainerHandle{ID: "xyz"}, "/tmp/deps", "/work/deps"); err != nil {
		t.Fatalf("CopyIn: %v", err)
	}
	want := []string{"podman", "cp", "/tmp/deps", "xyz:/work/deps"}
	for i := range want {
		if rec[0][i] != want[i] {
			t.Fatalf("argv = %v, want %v", rec[0], want)
		}
	}
}
