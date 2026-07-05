// internal/runtime/apple_lifecycle_test.go
package runtime

import "testing"

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
