package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	crt "github.com/eirueimi/unified-cd/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// probeFakeRT is a scripted runtime for the securityContext probe. Embedding
// the interface leaves every method the probe must not call as a nil-pointer
// panic.
type probeFakeRT struct {
	crt.ContainerRuntime

	rootProbe     string // output of `secctx-probe` in the root helper
	userProbe     string // output of `secctx-probe` in the --user container
	fsgroupExit   int
	createErr     error // returned for the user container when set
	rootCreateErr error

	created []crt.CreateSpec
	removed []string
	execs   [][]string
}

func (f *probeFakeRT) Create(_ context.Context, s crt.CreateSpec) (crt.ContainerHandle, error) {
	isUser := s.User != ""
	if isUser && f.createErr != nil {
		return crt.ContainerHandle{}, f.createErr
	}
	if !isUser && f.rootCreateErr != nil {
		return crt.ContainerHandle{}, f.rootCreateErr
	}
	f.created = append(f.created, s)
	id := "root"
	if isUser {
		id = "user"
	}
	return crt.ContainerHandle{ID: id}, nil
}

func (f *probeFakeRT) Exec(_ context.Context, h crt.ContainerHandle, s crt.ExecSpec, out, _ io.Writer) (int, error) {
	argv := append(append([]string{}, s.Shell...), s.Script)
	f.execs = append(f.execs, append([]string{h.ID}, argv...))
	if len(argv) >= 2 {
		switch argv[1] {
		case "fsgroup":
			return f.fsgroupExit, nil
		case "secctx-probe":
			if h.ID == "user" {
				_, _ = io.WriteString(out, f.userProbe)
			} else {
				_, _ = io.WriteString(out, f.rootProbe)
			}
			return 0, nil
		}
	}
	return 0, nil
}

func (f *probeFakeRT) Remove(_ context.Context, h crt.ContainerHandle) error {
	f.removed = append(f.removed, h.ID)
	return nil
}

const (
	goodRootProbe = "uid=0 gid=0 groups=0\nfile_uid=0 file_gid=54321 dir_mode=2770\nuid_map=0 0 4294967295\ngid_map=0 0 4294967295\n"
	goodUserProbe = "uid=54322 gid=54323 groups=54321,54323\nfile_uid=54322 file_gid=54321 dir_mode=2770\n"
)

func runProbe(t *testing.T, f *probeFakeRT) probeResult {
	t.Helper()
	ws := t.TempDir()
	res, err := probeSecurityContext(context.Background(), f, ws, "/host/tools", "pause:img")
	if res.OK {
		require.NoError(t, err)
	}
	// Cleanup on every path: no probe dir left behind.
	entries, rerr := os.ReadDir(ws)
	require.NoError(t, rerr)
	assert.Empty(t, entries, "the probe must remove its temp dir")
	for _, id := range []string{"root", "user"} {
		created := false
		for _, c := range f.created {
			if (c.User != "") == (id == "user") {
				created = true
			}
		}
		if created {
			assert.Contains(t, f.removed, id, "container %s must be removed", id)
		}
	}
	return res
}

func TestProbe_AllChecksPass(t *testing.T) {
	f := &probeFakeRT{rootProbe: goodRootProbe, userProbe: goodUserProbe}
	res := runProbe(t, f)
	assert.True(t, res.OK, res.Reason)
	assert.Empty(t, res.Reason)
	assert.False(t, res.Rootless)
	assert.Equal(t, int64(0), res.MaxMappedID)

	require.Len(t, f.created, 2)
	assert.Empty(t, f.created[0].User, "the root helper is created without --user")
	assert.Equal(t, "54322:54323", f.created[1].User)
	assert.Equal(t, []string{"54321"}, f.created[1].GroupAdd)
	for _, c := range f.created {
		assert.Equal(t, "pause:img", c.Image)
		assert.Equal(t, ucdShPause, c.Entrypoint)
		var hasTools bool
		for _, m := range c.Mounts {
			if m.ContainerPath == "/.ucd" {
				hasTools = true
				assert.True(t, m.ReadOnly)
				assert.Equal(t, "/host/tools", m.HostPath)
			}
		}
		assert.True(t, hasTools)
	}
	// fsgroup ran in the root helper before the user probe.
	require.NotEmpty(t, f.execs)
	assert.Equal(t, []string{"root", "/.ucd/ucd-sh", "fsgroup", "54321", "/p"}, f.execs[0])
}

func TestProbe_FailedExpectations(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(f *probeFakeRT)
		reason string
	}{
		{"uid", func(f *probeFakeRT) {
			f.userProbe = strings.Replace(goodUserProbe, "uid=54322", "uid=0", 1)
		}, "uid"},
		{"gid", func(f *probeFakeRT) {
			f.userProbe = strings.Replace(goodUserProbe, "gid=54323 groups", "gid=0 groups", 1)
		}, "gid"},
		{"missing supplementary group", func(f *probeFakeRT) {
			f.userProbe = strings.Replace(goodUserProbe, "groups=54321,54323", "groups=54323", 1)
		}, "supplementary group"},
		{"file group not inherited", func(f *probeFakeRT) {
			f.userProbe = strings.Replace(goodUserProbe, "file_gid=54321", "file_gid=54323", 1)
		}, "setgid inheritance"},
		{"file owner", func(f *probeFakeRT) {
			f.userProbe = strings.Replace(goodUserProbe, "file_uid=54322", "file_uid=0", 1)
		}, "file owner"},
		{"setgid bit missing", func(f *probeFakeRT) {
			f.userProbe = strings.Replace(goodUserProbe, "dir_mode=2770", "dir_mode=0770", 1)
		}, "setgid bit"},
		{"group not rwx", func(f *probeFakeRT) {
			f.userProbe = strings.Replace(goodUserProbe, "dir_mode=2770", "dir_mode=2750", 1)
		}, "group rwx"},
		{"root helper not uid 0", func(f *probeFakeRT) {
			f.rootProbe = strings.Replace(goodRootProbe, "uid=0 gid=0", "uid=1000 gid=1000", 1)
		}, "root helper"},
		{"fsgroup fails", func(f *probeFakeRT) { f.fsgroupExit = 2 }, "fsgroup"},
		{"unparseable output", func(f *probeFakeRT) { f.userProbe = "garbage\n" }, "uid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &probeFakeRT{rootProbe: goodRootProbe, userProbe: goodUserProbe}
			tc.mutate(f)
			res := runProbe(t, f)
			assert.False(t, res.OK)
			assert.Contains(t, res.Reason, tc.reason)
		})
	}
}

func TestProbe_CreateFailureIsAReasonAndCleansUp(t *testing.T) {
	f := &probeFakeRT{rootProbe: goodRootProbe, userProbe: goodUserProbe, createErr: errors.New("boom: --group-add unsupported")}
	res := runProbe(t, f)
	assert.False(t, res.OK)
	assert.Contains(t, res.Reason, "boom")
	assert.Contains(t, f.removed, "root", "the helper started before the failure is removed")
}

func TestProbe_RootHelperCreateFailure(t *testing.T) {
	f := &probeFakeRT{rootCreateErr: errors.New("no such image")}
	res := runProbe(t, f)
	assert.False(t, res.OK)
	assert.Contains(t, res.Reason, "no such image")
}

func TestProbe_RootlessMapGivesMaxMappedID(t *testing.T) {
	f := &probeFakeRT{
		rootProbe: "uid=0 gid=0 groups=0\nfile_uid=0 file_gid=0 dir_mode=2770\nuid_map=0 1000 1;1 100000 65536\ngid_map=0 1000 1;1 100000 65536\n",
		userProbe: goodUserProbe,
	}
	res := runProbe(t, f)
	assert.True(t, res.OK, res.Reason)
	assert.True(t, res.Rootless)
	assert.Equal(t, int64(65536), res.MaxMappedID)
}

func TestProbe_NarrowerOfUIDAndGIDMapWins(t *testing.T) {
	f := &probeFakeRT{
		rootProbe: "uid=0 gid=0 groups=0\nfile_uid=0 file_gid=0 dir_mode=2770\nuid_map=0 1000 1;1 100000 65536\ngid_map=0 1000 1;1 100000 1000\n",
		userProbe: goodUserProbe,
	}
	res := runProbe(t, f)
	assert.Equal(t, int64(1000), res.MaxMappedID)
}

func TestProbe_RootfulMapIsUnlimited(t *testing.T) {
	f := &probeFakeRT{rootProbe: goodRootProbe, userProbe: goodUserProbe}
	res := runProbe(t, f)
	assert.False(t, res.Rootless)
	assert.Equal(t, int64(0), res.MaxMappedID)
}

func TestProbe_NoMapReadableMeansUnlimitedNotRootless(t *testing.T) {
	f := &probeFakeRT{rootProbe: "uid=0 gid=0 groups=0\nfile_uid=0 file_gid=0 dir_mode=2770\n", userProbe: goodUserProbe}
	res := runProbe(t, f)
	assert.True(t, res.OK, res.Reason)
	assert.False(t, res.Rootless)
	assert.Equal(t, int64(0), res.MaxMappedID)
}

func TestProbe_CancelledContextStillCleansUp(t *testing.T) {
	f := &probeFakeRT{rootProbe: goodRootProbe, userProbe: goodUserProbe}
	ws := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, _ := probeSecurityContext(ctx, f, ws, "/host/tools", "pause:img")
	assert.False(t, res.OK)
	entries, err := os.ReadDir(ws)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

// TestProbe_RealDockerOutputParses feeds the probe the exact text a rootful
// docker on a Linux filesystem printed for the shim (captured by hand).
func TestProbe_RealDockerOutputParses(t *testing.T) {
	f := &probeFakeRT{
		rootProbe: "uid=0 gid=0 groups=0,10\nfile_uid=0 file_gid=54321 dir_mode=2770\nuid_map=0 0 4294967295\ngid_map=0 0 4294967295\n",
		userProbe: "uid=54322 gid=54323 groups=54321,54323\nfile_uid=54322 file_gid=54321 dir_mode=2770\nuid_map=0 0 4294967295\ngid_map=0 0 4294967295\n",
	}
	res := runProbe(t, f)
	assert.True(t, res.OK, res.Reason)
	assert.False(t, res.Rootless)
}
