// internal/runtime/apple_lifecycle_test.go
package runtime

import (
	"context"
	"strings"
	"testing"
)

func TestAppleSatisfiesInterface(t *testing.T) {
	var _ ContainerRuntime = &appleContainer{}
}

// TestAppleCreateArgv_WorkDir mirrors TestOCICLICreateArgv_WorkDir: Apple's
// `container` CLI is docker-compatible for run/exec/cp/rm (see the comment
// above appleContainer.Create), so it must also emit -w for spec.WorkDir.
func TestAppleCreateArgv_WorkDir(t *testing.T) {
	a := &appleContainer{}
	got := a.createArgs(CreateSpec{Image: "golang:1.22", WorkDir: "/workspace"})
	foundFlag := false
	for i, arg := range got {
		if arg == "-w" {
			foundFlag = true
			if i+1 >= len(got) || got[i+1] != "/workspace" {
				t.Fatalf("expected -w to be followed by /workspace, argv = %v", got)
			}
		}
	}
	if !foundFlag {
		t.Fatalf("expected -w /workspace in argv, got %v", got)
	}
}

// TestAppleCreateArgv_NoWorkDirWhenEmpty confirms empty WorkDir omits -w.
func TestAppleCreateArgv_NoWorkDirWhenEmpty(t *testing.T) {
	a := &appleContainer{}
	got := a.createArgs(CreateSpec{Image: "golang:1.22"})
	for _, arg := range got {
		if arg == "-w" {
			t.Fatalf("expected no -w flag when WorkDir is empty, argv = %v", got)
		}
	}
}

func TestAppleCreateArgv_Mounts(t *testing.T) {
	a := &appleContainer{}
	got := a.createArgs(CreateSpec{
		Image:   "alpine",
		WorkDir: "/workspace",
		Mounts:  []Mount{{HostPath: "/host/ws", ContainerPath: "/workspace"}},
	})
	found := false
	for i, s := range got {
		if s == "-v" && i+1 < len(got) && got[i+1] == "/host/ws:/workspace" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected -v /host/ws:/workspace, argv = %v", got)
	}
}

// TestAppleCreate_RejectsNetworkContainer confirms Apple's `container` CLI
// cannot join another container's netns (no --network container:<id>
// equivalent), so the claim pod's per-job containers require
// docker/podman/nerdctl. Create must fail fast with an explanatory error
// instead of silently ignoring NetworkContainer.
func TestAppleCreate_RejectsNetworkContainer(t *testing.T) {
	a := &appleContainer{}
	_, err := a.Create(context.Background(), CreateSpec{Image: "alpine", NetworkContainer: "abc123"})
	if err == nil {
		t.Fatalf("expected error when NetworkContainer is set, got nil")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("expected error to mention 'not supported', got %q", err.Error())
	}
}
