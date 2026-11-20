package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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
	onBlock  func() // called when a blocking Pull/Create is entered
}

func (f *pullRT) Pull(ctx context.Context, image string) error {
	f.mu.Lock()
	f.order = append(f.order, "pull:"+image)
	f.pulled = append(f.pulled, image)
	err := f.pullErr[image]
	block := f.blockPul
	f.mu.Unlock()
	if block {
		if f.onBlock != nil {
			f.onBlock()
		}
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
		if f.onBlock != nil {
			f.onBlock()
		}
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
	return runIsolatedWithHarness(t, newIsolatedHarness(), f, a)
}

func runIsolatedWithHarness(t *testing.T, h *isolatedHarness, f crt.ContainerRuntime, a *Agent) (*isolatedHarness, string) {
	t.Helper()
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

// cancelOnBlock makes the harness report Cancelled once the fake's blocking
// Pull or Create is entered, and speeds up the cancel poll.
func cancelOnBlock(t *testing.T, h *isolatedHarness) func() {
	t.Helper()
	prev := CancelPollInterval
	CancelPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { CancelPollInterval = prev })
	return func() {
		h.mu.Lock()
		h.runStatus = api.RunCancelled
		h.mu.Unlock()
	}
}

func TestExecuteRun_CancelDuringPull_AbandonsWithoutFinishRun(t *testing.T) {
	h := newIsolatedHarness()
	f := &pullRT{blockPul: true, onBlock: cancelOnBlock(t, h)}
	a := &Agent{} // default (30m) pull timeout: only the cancel can end the pull
	_, logs := runIsolatedWithHarness(t, h, f, a)
	h.mu.Lock()
	defer h.mu.Unlock()
	assert.Zero(t, h.finishCalls, "an abandoned claim sends no FinishRun")
	assert.Zero(t, f.creates, "no container is created")
	assert.Empty(t, h.logsByStep[-1], "no step -1 failure line")
	assert.NotContains(t, logs, "podPullTimeout", "a cancel is not reported as a pull timeout")
}

func TestExecuteRun_CancelDuringStart_RemovesPartialsWithoutFinishRun(t *testing.T) {
	h := newIsolatedHarness()
	f := &pullRT{blockAt: 2, onBlock: cancelOnBlock(t, h), podFakeRT: podFakeRT{imageIDs: map[string]string{
		"pause:img": "p", "mysql:8": "m", "runner:img": "r",
	}}}
	a := &Agent{}
	runIsolatedWithHarness(t, h, f, a)
	h.mu.Lock()
	defer h.mu.Unlock()
	assert.Zero(t, h.finishCalls)
	assert.Empty(t, h.logsByStep[-1])
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(t, f.created, "the pause container was created before the cancel")
	assert.Len(t, f.removed, len(f.created), "every created container (pause included) is removed")
}

func TestStartClaimCancelWatch_StopJoins(t *testing.T) {
	h := newIsolatedHarness()
	srv := newIsolatedServer(t, "w-agent", h)
	client := NewClient(srv.URL, "tok")
	const iv = 10 * time.Millisecond
	wctx, terminal, stop := startClaimCancelWatch(context.Background(), client, "run-w", iv)
	time.Sleep(5 * iv)
	assert.False(t, terminal())
	assert.NoError(t, wctx.Err())
	stop()
	h.mu.Lock()
	n := h.getRunCalls
	h.mu.Unlock()
	require.Positive(t, n, "the watcher polled while running")
	time.Sleep(3 * iv)
	h.mu.Lock()
	assert.Equal(t, n, h.getRunCalls, "no GetRun after stop")
	h.mu.Unlock()
}

func TestStartClaimCancelWatch_LogsFirstFailureOnly(t *testing.T) {
	var calls atomic.Int32
	const failures = 5
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if int(calls.Add(1)) <= failures {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.Run{ID: "run-w", Status: api.RunRunning})
	}))
	defer srv.Close()
	var logs bytes.Buffer
	defer captureSlog(&logs)()

	const iv = 10 * time.Millisecond
	_, _, stop := startClaimCancelWatch(context.Background(), NewClient(srv.URL, "tok"), "run-w", iv)
	require.Eventually(t, func() bool { return calls.Load() >= failures+2 }, 5*time.Second, iv)
	stop()

	out := logs.String()
	assert.Equal(t, 1, strings.Count(out, "get run failed"), "only the first failure of an outage is logged")
	assert.Equal(t, 1, strings.Count(out, "polling recovered"))
}
