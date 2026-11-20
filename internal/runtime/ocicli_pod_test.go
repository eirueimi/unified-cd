package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateArgs_Labels(t *testing.T) {
	r := &ociCLI{bin: "docker"}
	args, err := r.createArgs(CreateSpec{
		Image:  "img:1",
		Labels: map[string]string{"unified-cd/scope": "true", "unified-cd/agent-id": "a1"},
	})
	require.NoError(t, err)
	joined := strings.Join(args, " ")
	assert.Contains(t, joined, "--label unified-cd/agent-id=a1 --label unified-cd/scope=true")
	assert.Less(t, strings.Index(joined, "--label"), strings.Index(joined, "img:1"), "labels precede the image")
}

func TestCreateArgs_NoLabelsByDefault(t *testing.T) {
	args, err := (&ociCLI{bin: "docker"}).createArgs(CreateSpec{Image: "img:1"})
	require.NoError(t, err)
	assert.NotContains(t, args, "--label")
}

// helperEnv selects the helper process behaviour: "<exit>|<stdout>" keyed by
// the first argv element after the binary name that identifies the call.
const helperEnv = "OCI_POD_TEST_HELPER"

func TestHelperProcess(t *testing.T) {
	spec := os.Getenv(helperEnv)
	if spec == "" {
		return
	}
	parts := strings.SplitN(spec, "|", 2)
	fmt.Print(strings.ReplaceAll(parts[1], `\n`, "\n"))
	if parts[0] != "0" {
		os.Exit(1)
	}
	os.Exit(0)
}

// withHelperExec records argv and re-executes the test binary as the fake
// container CLI; reply picks "<exit>|<stdout>" per call.
func withHelperExec(t *testing.T, record *[][]string, reply func(args []string) string) {
	t.Helper()
	orig := execCommand
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		*record = append(*record, append([]string{name}, args...))
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperProcess")
		cmd.Env = append(os.Environ(), helperEnv+"="+reply(args))
		return cmd
	}
	t.Cleanup(func() { execCommand = orig })
}

func TestOCI_PodRuntimeArgs(t *testing.T) {
	ctx := context.Background()
	r := &ociCLI{bin: "docker"}

	t.Run("ImageID present", func(t *testing.T) {
		var rec [][]string
		withHelperExec(t, &rec, func([]string) string { return "0|sha256:abc\n" })
		id, ok, err := r.ImageID(ctx, "img:1")
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, "sha256:abc", id)
		assert.Equal(t, []string{"docker", "image", "inspect", "--format", "{{.Id}}", "img:1"}, rec[0])
	})

	t.Run("ImageID missing is not an error", func(t *testing.T) {
		var rec [][]string
		withHelperExec(t, &rec, func([]string) string { return "1|" })
		id, ok, err := r.ImageID(ctx, "img:1")
		require.NoError(t, err)
		assert.False(t, ok)
		assert.Empty(t, id)
	})

	t.Run("ContainerRunning", func(t *testing.T) {
		var rec [][]string
		withHelperExec(t, &rec, func([]string) string { return "0|true\n" })
		running, err := r.ContainerRunning(ctx, ContainerHandle{ID: "c1"})
		require.NoError(t, err)
		assert.True(t, running)
		assert.Equal(t, []string{"docker", "inspect", "-f", "{{.State.Running}}", "c1"}, rec[0])
	})

	t.Run("ListContainers", func(t *testing.T) {
		var rec [][]string
		withHelperExec(t, &rec, func(args []string) string {
			if args[0] == "ps" {
				return "0|id1\nid2\n"
			}
			return `0|{"unified-cd/agent-id":"a1"}` + `\n`
		})
		infos, err := r.ListContainers(ctx, map[string]string{"unified-cd/scope": "true", "unified-cd/agent-id": "a1"})
		require.NoError(t, err)
		require.Len(t, infos, 2)
		assert.Equal(t, "id1", infos[0].Handle.ID)
		assert.Equal(t, map[string]string{"unified-cd/agent-id": "a1"}, infos[0].Labels)
		assert.Equal(t, []string{"docker", "ps", "-a", "-q", "--no-trunc",
			"--filter", "label=unified-cd/agent-id=a1", "--filter", "label=unified-cd/scope=true"}, rec[0])
		assert.Equal(t, []string{"docker", "inspect", "-f", "{{json .Config.Labels}}", "id1"}, rec[1])
	})

	t.Run("LogsSince", func(t *testing.T) {
		var rec [][]string
		withHelperExec(t, &rec, func([]string) string { return "0|" })
		since := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
		require.NoError(t, r.LogsSince(ctx, ContainerHandle{ID: "c1"}, since, &strings.Builder{}, &strings.Builder{}))
		assert.Equal(t, []string{"docker", "logs", "-f", "--since", "2026-01-02T03:04:05.000000006Z", "c1"}, rec[0])
	})

	t.Run("LastLogTime", func(t *testing.T) {
		var rec [][]string
		withHelperExec(t, &rec, func([]string) string { return "0|2026-01-02T03:04:05.123456789Z hello world\n" })
		got, err := r.LastLogTime(ctx, ContainerHandle{ID: "c1"})
		require.NoError(t, err)
		assert.True(t, got.Equal(time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)))
		assert.Equal(t, []string{"docker", "logs", "--timestamps", "--tail", "1", "c1"}, rec[0])
	})

	t.Run("LastLogTime no output", func(t *testing.T) {
		var rec [][]string
		withHelperExec(t, &rec, func([]string) string { return "0|" })
		got, err := r.LastLogTime(ctx, ContainerHandle{ID: "c1"})
		require.NoError(t, err)
		assert.True(t, got.IsZero())
	})

	t.Run("LastLogTime ignores CLI warnings and accepts podman offsets and microseconds", func(t *testing.T) {
		var rec [][]string
		withHelperExec(t, &rec, func([]string) string {
			return "0|WARN[0000] The cgroupv2 manager is set to systemd\n2026-01-02T03:04:05.123456+00:00 last line\nWARNING: Error loading config file: x\n"
		})
		got, err := r.LastLogTime(ctx, ContainerHandle{ID: "c1"})
		require.NoError(t, err)
		assert.True(t, got.Equal(time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)), got.String())
	})

	t.Run("LastLogTime warnings only means no output", func(t *testing.T) {
		var rec [][]string
		withHelperExec(t, &rec, func([]string) string { return "0|WARN[0000] something\n" })
		got, err := r.LastLogTime(ctx, ContainerHandle{ID: "c1"})
		require.NoError(t, err)
		assert.True(t, got.IsZero())
	})

	t.Run("LastLogTime failure carries the CLI output", func(t *testing.T) {
		var rec [][]string
		withHelperExec(t, &rec, func([]string) string { return "1|Error: no such container\n" })
		_, err := r.LastLogTime(ctx, ContainerHandle{ID: "c1"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no such container")
	})
}

func TestOCI_ListContainers_SkipsBadInspect(t *testing.T) {
	ctx := context.Background()
	r := &ociCLI{bin: "docker"}

	t.Run("vanished container is skipped", func(t *testing.T) {
		var rec [][]string
		withHelperExec(t, &rec, func(args []string) string {
			switch {
			case args[0] == "ps":
				return "0|gone\nok\n"
			case args[len(args)-1] == "gone":
				return "1|"
			}
			return `0|{"k":"v"}` + `
`
		})
		infos, err := r.ListContainers(ctx, map[string]string{"k": "v"})
		require.NoError(t, err)
		require.Len(t, infos, 1)
		assert.Equal(t, "ok", infos[0].Handle.ID)
	})

	t.Run("malformed labels JSON is skipped and listing continues", func(t *testing.T) {
		var rec [][]string
		withHelperExec(t, &rec, func(args []string) string {
			switch {
			case args[0] == "ps":
				return "0|bad\nok\n"
			case args[len(args)-1] == "bad":
				return "0|not-json\n"
			}
			return `0|{"k":"v"}` + `
`
		})
		infos, err := r.ListContainers(ctx, map[string]string{"k": "v"})
		require.NoError(t, err)
		require.Len(t, infos, 1)
		assert.Equal(t, "ok", infos[0].Handle.ID)
	})
}
