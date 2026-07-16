// internal/runtime/ocicli_mount_test.go
package runtime

import (
	"strings"
	"testing"
)

// containsPair reports whether args contains flag immediately followed by
// value as consecutive elements.
func containsPair(args []string, flag, value string) bool {
	for i, a := range args {
		if a == flag && i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}

// TestCreateArgs_MountForm is the regression test for G6: OCI CLI bind
// mounts must use the `--mount type=bind,source=...,target=...[,readonly]`
// form instead of `-v host:ctr[:ro]`. The `-v` colon-joined form is
// ambiguous on Windows, where a host path itself contains a colon after the
// drive letter (`C:\ws`), so it cannot safely be told apart from the
// host:container separator.
func TestCreateArgs_MountForm(t *testing.T) {
	cases := []struct {
		host, ctr string
		ro        bool
		want      string
	}{
		{"/host/ws", "/workspace", false, "type=bind,source=/host/ws,target=/workspace"},
		{"/host/ws", "/workspace", true, "type=bind,source=/host/ws,target=/workspace,readonly"},
		{`C:\ws`, "/workspace", false, `type=bind,source=C:\ws,target=/workspace`},
	}
	for _, c := range cases {
		spec := CreateSpec{Image: "img", Mounts: []Mount{{HostPath: c.host, ContainerPath: c.ctr, ReadOnly: c.ro}}}
		args, err := (&ociCLI{bin: "docker"}).createArgs(spec)
		if err != nil {
			t.Fatalf("createArgs(%+v): unexpected error: %v", c, err)
		}
		joined := strings.Join(args, " ")
		if !containsPair(args, "--mount", c.want) {
			t.Errorf("mount %v: argv %v missing --mount %s", c, args, c.want)
		}
		if strings.Contains(joined, " -v ") {
			t.Errorf("old -v form still present: %v", args)
		}
	}
}

// TestCreateArgs_MountRejectsSeparators asserts a mount path containing ','
// or '=' is rejected up front: both characters are significant in `--mount`'s
// comma-separated key=value grammar, and a path containing either can't be
// expressed unambiguously in that syntax.
func TestCreateArgs_MountRejectsSeparators(t *testing.T) {
	spec := CreateSpec{Image: "img", Mounts: []Mount{{HostPath: "/a,b", ContainerPath: "/w"}}}
	_, err := (&ociCLI{bin: "docker"}).createArgs(spec)
	if err == nil {
		t.Fatal("a mount path containing ',' must be rejected")
	}
}

// TestCreateArgs_MountRejectsEquals mirrors TestCreateArgs_MountRejectsSeparators
// for '='.
func TestCreateArgs_MountRejectsEquals(t *testing.T) {
	spec := CreateSpec{Image: "img", Mounts: []Mount{{HostPath: "/a", ContainerPath: "/w=x"}}}
	_, err := (&ociCLI{bin: "docker"}).createArgs(spec)
	if err == nil {
		t.Fatal("a mount path containing '=' must be rejected")
	}
}
