package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTimeoutMessages(t *testing.T) {
	assert.Equal(t, "claim pod image pull exceeded podPullTimeout (30m0s)", pullTimeoutMsg(30*time.Minute))
	assert.Equal(t, "claim pod start exceeded podStartTimeout (5m0s)", startTimeoutMsg(5*time.Minute))
}

// pullRT records Pull and Create calls in order and lets a test block or fail
// them.
type pullRT struct {
	podFakeRT
	mu       sync.Mutex
	order    []string
	pulled   []string
	pullErr  map[string]error
	blockPul bool
	blockAt  int // block the Nth Create (1-indexed) until ctx is done; 0 = never
	creates  int
}

func (f *pullRT) Pull(ctx context.Context, image string) error {
	f.mu.Lock()
	f.order = append(f.order, "pull:"+image)
	f.pulled = append(f.pulled, image)
	err := f.pullErr[image]
	block := f.blockPul
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	if err == nil {
		// A successful pull makes the image present (re-pulls refresh its ID).
		if f.imageIDs == nil {
			f.imageIDs = map[string]string{}
		}
		f.imageIDs[image] = "pulled-" + image
	}
	return err
}

func (f *pullRT) Create(ctx context.Context, s crt.CreateSpec) (crt.ContainerHandle, error) {
	f.mu.Lock()
	f.order = append(f.order, "create:"+s.Image)
	f.creates++
	n := f.creates
	block := f.blockAt == n
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return crt.ContainerHandle{}, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.podFakeRT.Create(ctx, s)
}

func TestPullClaimImages_LatestAlwaysPulledOthersOnlyWhenMissing(t *testing.T) {
	f := &pullRT{podFakeRT: podFakeRT{imageIDs: map[string]string{
		"mysql:8":          "sha256:aaa",
		"pause@sha256:def": "sha256:bbb",
		"runner:latest":    "sha256:ccc",
		"missing:1.0":      "",
	}}}
	delete(f.imageIDs, "missing:1.0")
	images := []string{"pause@sha256:def", "mysql:8", "runner:latest", "runner:latest", "missing:1.0"}
	ids, err := pullClaimImages(context.Background(), f, images)
	require.NoError(t, err)
	assert.Equal(t, []string{"runner:latest", "missing:1.0"}, f.pulled, "mysql:8 and the pinned pause image are present and not pulled; runner:latest is pulled once")
	assert.Equal(t, "sha256:aaa", ids["mysql:8"])
	assert.Equal(t, "sha256:bbb", ids["pause@sha256:def"])
}

func TestPullClaimImages_PullFailureFailsNoStaleFallback(t *testing.T) {
	f := &pullRT{
		podFakeRT: podFakeRT{imageIDs: map[string]string{"app:latest": "sha256:old"}},
		pullErr:   map[string]error{"app:latest": errors.New("registry down")},
	}
	_, err := pullClaimImages(context.Background(), f, []string{"app:latest"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "registry down")
}

func TestClaimImages_PauseFirstDedup(t *testing.T) {
	defs := []containerDef{{Image: "a"}, {Image: "pause"}, {Image: "a"}, {Image: "b"}}
	assert.Equal(t, []string{"pause", "a", "b"}, claimImages(defs, "pause"))
}

func runIsolatedWith(t *testing.T, f crt.ContainerRuntime, a *Agent) (*isolatedHarness, string) {
	t.Helper()
	h := newIsolatedHarness()
	srv := newIsolatedServer(t, "pull-agent", h)
	a.ID = "pull-agent"
	a.Client = NewClient(srv.URL, "tok")
	a.PauseImage = "pause:img"
	a.RunnerImage = "runner:img"
	a.runtimeOnce.Do(func() {})
	a.resolvedRuntime = f
	claim := api.ClaimResponse{
		RunID:       "run-pull",
		JobName:     "pull-job",
		PodTemplate: mysqlTemplate(),
		Stages:      []api.ClaimStage{{Step: &api.ClaimStep{Index: 0, Name: "s1", Run: "echo hi"}}},
	}
	a.executeRun(context.Background(), claim, t.TempDir())
	h.mu.Lock()
	defer h.mu.Unlock()
	var msgs []string
	for _, l := range h.logsByStep[-1] {
		msgs = append(msgs, l.Line)
	}
	return h, strings.Join(msgs, "\n")
}

func TestExecuteRun_PullTimeout_FailsWithExactMessage(t *testing.T) {
	f := &pullRT{blockPul: true}
	a := &Agent{PodPullTimeout: 50 * time.Millisecond}
	h, logs := runIsolatedWith(t, f, a)
	assert.Equal(t, "Failed", h.finishStatus)
	assert.Contains(t, logs, "claim pod image pull exceeded podPullTimeout (50ms)")
	assert.Empty(t, f.created, "no container is created after a pull timeout")
	assert.Zero(t, f.creates)
}

func TestExecuteRun_StartTimeout_FailsWithExactMessage(t *testing.T) {
	f := &pullRT{blockAt: 2, podFakeRT: podFakeRT{imageIDs: map[string]string{
		"pause:img": "p", "mysql:8": "m", "runner:img": "r",
	}}}
	a := &Agent{PodStartTimeout: 50 * time.Millisecond}
	h, logs := runIsolatedWith(t, f, a)
	assert.Equal(t, "Failed", h.finishStatus)
	assert.Contains(t, logs, "claim pod start exceeded podStartTimeout (50ms)")
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(t, f.created)
	assert.Len(t, f.removed, len(f.created), "every created container is removed")
}

func TestExecuteRun_PullPrecedesCreate(t *testing.T) {
	f := &pullRT{} // no image IDs: everything is missing and pulled
	a := &Agent{}
	runIsolatedWith(t, f, a) // final status is irrelevant here
	f.mu.Lock()
	defer f.mu.Unlock()
	firstCreate := -1
	lastPull := -1
	for i, o := range f.order {
		if strings.HasPrefix(o, "create:") && firstCreate < 0 {
			firstCreate = i
		}
		if strings.HasPrefix(o, "pull:") {
			lastPull = i
		}
	}
	require.GreaterOrEqual(t, firstCreate, 0)
	require.GreaterOrEqual(t, lastPull, 0)
	assert.Less(t, lastPull, firstCreate)
}
