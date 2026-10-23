package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOCICLICreateArgs_Security_AllFields_BeforeImage(t *testing.T) {
	r := &ociCLI{bin: "docker"}
	got, err := r.createArgs(CreateSpec{
		Image:           "img",
		User:            "1000:2000",
		GroupAdd:        []string{"3000", "4000"},
		CapDrop:         []string{"ALL"},
		CapAdd:          []string{"NET_BIND_SERVICE"},
		NoNewPrivileges: true,
		ReadOnlyRootfs:  true,
		Entrypoint:      []string{"sh"},
		Args:            []string{"-c", "true"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{
		"run", "-d",
		"--user", "1000:2000",
		"--group-add", "3000",
		"--group-add", "4000",
		"--cap-drop", "ALL",
		"--cap-add", "NET_BIND_SERVICE",
		"--security-opt", "no-new-privileges",
		"--read-only",
		"--entrypoint", "",
		"img", "sh", "-c", "true",
	}, got)
}

func TestOCICLICreateArgs_Security_Unset_NoFlags(t *testing.T) {
	r := &ociCLI{bin: "docker"}
	got, err := r.createArgs(CreateSpec{Image: "img"})
	require.NoError(t, err)
	assert.Equal(t, []string{"run", "-d", "img"}, got)
}

func TestOCICLICreateArgs_Security_EachFieldAlone(t *testing.T) {
	cases := map[string]struct {
		spec CreateSpec
		want []string
	}{
		"user":     {CreateSpec{User: "1000"}, []string{"--user", "1000"}},
		"group":    {CreateSpec{GroupAdd: []string{"7"}}, []string{"--group-add", "7"}},
		"capdrop":  {CreateSpec{CapDrop: []string{"A", "B"}}, []string{"--cap-drop", "A", "--cap-drop", "B"}},
		"capadd":   {CreateSpec{CapAdd: []string{"A"}}, []string{"--cap-add", "A"}},
		"nonewpri": {CreateSpec{NoNewPrivileges: true}, []string{"--security-opt", "no-new-privileges"}},
		"readonly": {CreateSpec{ReadOnlyRootfs: true}, []string{"--read-only"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			c.spec.Image = "img"
			got, err := (&ociCLI{bin: "docker"}).createArgs(c.spec)
			require.NoError(t, err)
			assert.Equal(t, append(append([]string{"run", "-d"}, c.want...), "img"), got)
		})
	}
}

func TestOCICLICreateArgs_ReadOnly_PodmanOnlyTmpfsFlag(t *testing.T) {
	spec := CreateSpec{Image: "img", ReadOnlyRootfs: true}
	pod, err := (&ociCLI{bin: "podman"}).createArgs(spec)
	require.NoError(t, err)
	assert.Equal(t, []string{"run", "-d", "--read-only", "--read-only-tmpfs=false", "img"}, pod)

	for _, bin := range []string{"docker", "nerdctl"} {
		got, err := (&ociCLI{bin: bin}).createArgs(spec)
		require.NoError(t, err)
		assert.NotContains(t, got, "--read-only-tmpfs=false", bin)
	}

	// Without ReadOnlyRootfs podman gets no tmpfs flag.
	got, err := (&ociCLI{bin: "podman"}).createArgs(CreateSpec{Image: "img"})
	require.NoError(t, err)
	assert.NotContains(t, got, "--read-only-tmpfs=false")
}

// TestHelperInspect is re-executed as the fake runtime binary. It is driven by
// HELPER_* env vars and is a no-op in a normal test run.
func TestHelperInspect(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_INSPECT") != "1" {
		return
	}
	if msg := os.Getenv("HELPER_STDERR"); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, os.Getenv("HELPER_STDOUT"))
	os.Exit(0)
}

func helperCmd(ctx context.Context, stdout, stderr string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHelperInspect$")
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_INSPECT=1", "HELPER_STDOUT="+stdout, "HELPER_STDERR="+stderr)
	return cmd
}

func TestOCICLIImageUser_Argv(t *testing.T) {
	var rec [][]string
	orig := execCommand
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		rec = append(rec, append([]string{name}, args...))
		return helperCmd(ctx, "app:staff", "")
	}
	t.Cleanup(func() { execCommand = orig })

	u, err := (&ociCLI{bin: "docker"}).ImageUser(context.Background(), "img")
	require.NoError(t, err)
	assert.Equal(t, "app:staff", u)
	require.Len(t, rec, 1)
	assert.Equal(t, []string{"docker", "image", "inspect", "--format", "{{.Config.User}}", "img"}, rec[0])
}

func TestOCICLIImageUser_EmptyUser(t *testing.T) {
	orig := execCommand
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return helperCmd(ctx, "", "")
	}
	t.Cleanup(func() { execCommand = orig })
	u, err := (&ociCLI{bin: "docker"}).ImageUser(context.Background(), "img")
	require.NoError(t, err)
	assert.Equal(t, "", u)
}

func TestOCICLIImageUser_PullsThenRetriesOnNoSuchImage(t *testing.T) {
	var rec [][]string
	inspects := 0
	orig := execCommand
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		rec = append(rec, append([]string{name}, args...))
		if len(args) > 0 && args[0] == "pull" {
			return helperCmd(ctx, "", "")
		}
		inspects++
		if inspects == 1 {
			return helperCmd(ctx, "", "Error: No such image: img")
		}
		return helperCmd(ctx, "1000", "")
	}
	t.Cleanup(func() { execCommand = orig })

	u, err := (&ociCLI{bin: "docker"}).ImageUser(context.Background(), "img")
	require.NoError(t, err)
	assert.Equal(t, "1000", u)
	require.Len(t, rec, 3)
	assert.Equal(t, []string{"docker", "pull", "img"}, rec[1])
	assert.Equal(t, rec[0], rec[2])
}

func TestOCICLIImageUser_OtherErrorDoesNotPull(t *testing.T) {
	var rec [][]string
	orig := execCommand
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		rec = append(rec, append([]string{name}, args...))
		return helperCmd(ctx, "", "Cannot connect to the daemon")
	}
	t.Cleanup(func() { execCommand = orig })

	_, err := (&ociCLI{bin: "docker"}).ImageUser(context.Background(), "img")
	require.Error(t, err)
	assert.Len(t, rec, 1)
	assert.True(t, strings.Contains(err.Error(), "daemon"))
}

func TestOCICLIImageUser_RetriesOnlyOnce(t *testing.T) {
	var rec [][]string
	orig := execCommand
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		rec = append(rec, append([]string{name}, args...))
		if len(args) > 0 && args[0] == "pull" {
			return helperCmd(ctx, "", "")
		}
		return helperCmd(ctx, "", "Error: No such image: img")
	}
	t.Cleanup(func() { execCommand = orig })
	_, err := (&ociCLI{bin: "docker"}).ImageUser(context.Background(), "img")
	require.Error(t, err)
	assert.Len(t, rec, 3)
}

func TestOCICLI_ImplementsImageInspector(t *testing.T) {
	var _ ImageInspector = (*ociCLI)(nil)
}
