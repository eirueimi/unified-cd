package k8sagent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/config"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

var pvcResource = corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims")

// installPVCCompareAndSwap makes the fake clientset enforce what a real API
// server does and the fake does not: an Update must carry the resourceVersion of
// the object it replaces, or it is a Conflict. Without it the lock's optimistic
// update could never fail in a test. The fake holds its lock across a whole
// reaction, so the check and the write are atomic.
func installPVCCompareAndSwap(cs *fake.Clientset) {
	cs.PrependReactor("update", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		obj := a.(k8stesting.UpdateAction).GetObject().(*corev1.PersistentVolumeClaim)
		cur, err := cs.Tracker().Get(pvcResource, obj.Namespace, obj.Name)
		if err != nil {
			return true, nil, err
		}
		stored := cur.(*corev1.PersistentVolumeClaim)
		if stored.ResourceVersion != obj.ResourceVersion {
			return true, nil, apierrors.NewConflict(pvcResource.GroupResource(), obj.Name, errors.New("the object has been modified"))
		}
		n, _ := strconv.Atoi(stored.ResourceVersion)
		obj.ResourceVersion = strconv.Itoa(n + 1)
		return false, nil, nil
	})
}

func pvcAnnotation(t *testing.T, cs *fake.Clientset, claim, key string) string {
	t.Helper()
	p, err := cs.CoreV1().PersistentVolumeClaims(testVolNS).Get(context.Background(), claim, metav1.GetOptions{})
	require.NoError(t, err)
	return p.Annotations[key]
}

func setLock(t *testing.T, cs *fake.Clientset, claim, holder string, at time.Time) {
	t.Helper()
	p, err := cs.CoreV1().PersistentVolumeClaims(testVolNS).Get(context.Background(), claim, metav1.GetOptions{})
	require.NoError(t, err)
	p.Annotations = map[string]string{annoVolumeHeldBy: holder, annoVolumeHeldAt: at.UTC().Format(time.RFC3339)}
	_, err = cs.CoreV1().PersistentVolumeClaims(testVolNS).Update(context.Background(), p, metav1.UpdateOptions{})
	require.NoError(t, err)
}

// controllerStub answers what a waiting or failing claim talks to: the run is
// running, log lines are kept, finish is recorded.
type controllerStub struct {
	srv *httptest.Server
	mu  sync.Mutex
	// logs holds every log request body per run.
	logs     map[string][]string
	finished map[string]string
}

func newControllerStub(t *testing.T) *controllerStub {
	t.Helper()
	s := &controllerStub{logs: map[string][]string{}, finished: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		orchestrateWriteJSON(w, api.Run{ID: r.PathValue("id"), Status: api.RunRunning})
	})
	mux.HandleFunc("POST /api/v1/agents/{agent}/runs/{run}/steps/{idx}/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.logs[r.PathValue("run")] = append(s.logs[r.PathValue("run")], string(body))
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/{agent}/runs/{run}/finish", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.finished[r.PathValue("run")] = body["status"]
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *controllerStub) logText(run string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.logs[run], "\n")
}

func (s *controllerStub) status(run string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finished[run]
}

// barrier lets n callers arrive, then releases all of them together.
type barrier struct {
	n     int32
	count atomic.Int32
	open  chan struct{}
}

func newBarrier(n int32) *barrier { return &barrier{n: n, open: make(chan struct{})} }

func (b *barrier) await() {
	if b.count.Add(1) == b.n {
		close(b.open)
	}
	select {
	case <-b.open:
	case <-time.After(5 * time.Second):
	}
}

// barrierPM holds a replica's FIRST read of the cluster's pods until every
// replica has made theirs, so both see the volume free before either acts on it.
// That is the read-modify-write interleaving the PVC lock exists to defeat.
type barrierPM struct {
	podManager
	once sync.Once
	b    *barrier
}

func (p *barrierPM) ListPods(ctx context.Context, sel string) (*corev1.PodList, error) {
	if sel == agentPodSelector {
		p.once.Do(p.b.await)
	}
	return p.podManager.ListPods(ctx, sel)
}

// lockReplica is one k8s-agent process of a Deployment: same config, same
// cluster, its own memory.
func lockReplica(cs *fake.Clientset, ctlURL string, pm podManager, vols []config.VolumeConfig) *K8sAgent {
	return &K8sAgent{
		cfg:    Config{AgentID: "k8s-replica", AgentName: "agent1", Namespace: testVolNS, PodImage: "ubuntu:22.04", Volumes: vols},
		client: agentlib.NewClient(ctlURL, "tok"),
		pm:     pm,
		kube:   cs,
	}
}

func lockCluster(t *testing.T, vols ...config.VolumeConfig) *fake.Clientset {
	t.Helper()
	cs := fake.NewSimpleClientset()
	installPVCCompareAndSwap(cs)
	require.NoError(t, EnsurePVCs(context.Background(), cs, testVolNS, "agent1", vols))
	old := agentlib.CancelPollInterval
	agentlib.CancelPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { agentlib.CancelPollInterval = old })
	return cs
}

func claimFor(run string, vols ...dsl.VolumeMount) api.ClaimResponse {
	return api.ClaimResponse{RunID: run, JobName: "j", Volumes: vols}
}

// TestVolumeLock_TwoReplicasCannotBothTakeAFreeVolume is the exclusion property
// itself. Both replicas read the pods while the volume is free (the barrier
// forces that interleaving), then both try to take it. Reading pod labels alone
// would let both create a pod; the PVC lock lets exactly one.
func TestVolumeLock_TwoReplicasCannotBothTakeAFreeVolume(t *testing.T) {
	cs := lockCluster(t, vcfg("unity-main", "exclusive", "1Gi"))
	ctl := newControllerStub(t)
	bar := newBarrier(2)
	vols := []config.VolumeConfig{vcfg("unity-main", "exclusive", "1Gi")}
	mk := func() *K8sAgent {
		return lockReplica(cs, ctl.srv.URL, &barrierPM{podManager: volPM{NewPodManager(cs, testVolNS, "img")}, b: bar}, vols)
	}
	a, b := mk(), mk()

	type result struct {
		run string
		res claimPodResult
		err error
	}
	results := make(chan result, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, run := range []string{"run-a", "run-b"} {
		ag := a
		if run == "run-b" {
			ag = b
		}
		go func() {
			res, err := ag.createClaimPod(ctx, claimFor(run, mount("unity-main", "", ".")))
			results <- result{run, res, err}
		}()
	}

	var winner result
	select {
	case winner = <-results:
		require.NoError(t, winner.err)
	case <-time.After(10 * time.Second):
		t.Fatal("neither replica got the volume")
	}

	// The loser keeps waiting, polling many times over, and never creates a pod.
	select {
	case r := <-results:
		t.Fatalf("both replicas got the exclusive volume: %s and %s", winner.run, r.run)
	case <-time.After(400 * time.Millisecond):
	}
	pods, err := cs.CoreV1().Pods(testVolNS).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pods.Items, 1, "exactly one run pod may exist for an exclusive volume")
	assert.Equal(t, winner.run, pvcAnnotation(t, cs, "agent1-unity-main", annoVolumeHeldBy))

	// When the winner ends (pod gone, lock released) the loser gets its turn.
	require.NoError(t, cs.CoreV1().Pods(testVolNS).Delete(context.Background(), winner.res.pod.Name, metav1.DeleteOptions{}))
	winner.res.release(ctx)
	select {
	case r := <-results:
		require.NoError(t, r.err)
		assert.NotEqual(t, winner.run, r.run)
		assert.Equal(t, r.run, pvcAnnotation(t, cs, "agent1-unity-main", annoVolumeHeldBy))
	case <-time.After(10 * time.Second):
		t.Fatal("the waiting replica never got the volume after the holder released it")
	}
}

func TestVolumeLock_ALostRaceFallsThroughToTheNextCandidate(t *testing.T) {
	cs := lockCluster(t, vcfg("cache-1", "exclusive", "1Gi"), vcfg("cache-2", "exclusive", "1Gi"))
	ctl := newControllerStub(t)
	// Another replica holds cache-1 and has not created its pod yet, so the pod
	// labels call cache-1 free.
	setLock(t, cs, "agent1-cache-1", "other-run", time.Now())
	a := lockReplica(cs, ctl.srv.URL, volPM{NewPodManager(cs, testVolNS, "img")},
		[]config.VolumeConfig{vcfg("cache-1", "exclusive", "1Gi"), vcfg("cache-2", "exclusive", "1Gi")})

	res, err := a.createClaimPod(context.Background(), claimFor("run-a",
		dsl.VolumeMount{Name: dsl.StringOrSlice{"cache-1", "cache-2"}, As: "cache", MountPath: ".cache"}))
	require.NoError(t, err)
	require.NotNil(t, res.pod)

	var claims []string
	for _, v := range res.pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			claims = append(claims, v.PersistentVolumeClaim.ClaimName)
		}
	}
	assert.Equal(t, []string{"agent1-cache-2"}, claims)
	assert.Equal(t, "other-run", pvcAnnotation(t, cs, "agent1-cache-1", annoVolumeHeldBy), "the other run's lock is untouched")
	assert.Equal(t, "run-a", pvcAnnotation(t, cs, "agent1-cache-2", annoVolumeHeldBy))
}

func TestVolumeLock_ConflictingUpdateIsRetriedNotLost(t *testing.T) {
	cs := lockCluster(t, vcfg("unity-main", "exclusive", "1Gi"))
	var injected atomic.Int32
	cs.PrependReactor("update", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if injected.Add(1) == 1 {
			// An unrelated writer (the provisioner binding the claim) got in first.
			return true, nil, apierrors.NewConflict(pvcResource.GroupResource(), "agent1-unity-main", errors.New("modified"))
		}
		return false, nil, nil
	})
	ctl := newControllerStub(t)
	a := lockReplica(cs, ctl.srv.URL, volPM{NewPodManager(cs, testVolNS, "img")}, []config.VolumeConfig{vcfg("unity-main", "exclusive", "1Gi")})

	ok, err := a.lockVolume(context.Background(), "unity-main", "run-a")
	require.NoError(t, err)
	assert.True(t, ok, "a conflict with nobody holding the lock is re-read and retried")
	assert.Equal(t, "run-a", pvcAnnotation(t, cs, "agent1-unity-main", annoVolumeHeldBy))
}

func TestVolumeLock_StaleHolders(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	podOf := func(run string, phase corev1.PodPhase) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "p-" + run, Namespace: testVolNS, Labels: map[string]string{"app": "unified-cd-agent", labelRunID: run}},
			Status:     corev1.PodStatus{Phase: phase},
		}
	}
	cases := []struct {
		name   string
		at     time.Time
		pod    *corev1.Pod
		expect bool
	}{
		{"holder has no pod and the lock is old", old, nil, true},
		{"holder's pod finished and the lock is old", old, podOf("dead", corev1.PodSucceeded), true},
		{"holder has no pod but the lock is fresh (its pod is about to be created)", time.Now(), nil, false},
		{"holder's pod is alive", old, podOf("dead", corev1.PodRunning), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := lockCluster(t, vcfg("unity-main", "exclusive", "1Gi"))
			if tc.pod != nil {
				_, err := cs.CoreV1().Pods(testVolNS).Create(context.Background(), tc.pod, metav1.CreateOptions{})
				require.NoError(t, err)
			}
			setLock(t, cs, "agent1-unity-main", "dead", tc.at)
			ctl := newControllerStub(t)
			a := lockReplica(cs, ctl.srv.URL, volPM{NewPodManager(cs, testVolNS, "img")}, []config.VolumeConfig{vcfg("unity-main", "exclusive", "1Gi")})

			ok, err := a.lockVolume(context.Background(), "unity-main", "run-a")
			require.NoError(t, err)
			assert.Equal(t, tc.expect, ok)
			if tc.expect {
				assert.Equal(t, "run-a", pvcAnnotation(t, cs, "agent1-unity-main", annoVolumeHeldBy))
			} else {
				assert.Equal(t, "dead", pvcAnnotation(t, cs, "agent1-unity-main", annoVolumeHeldBy))
			}
		})
	}
}

func TestVolumeLock_UnlockLeavesAnotherRunsLockAlone(t *testing.T) {
	cs := lockCluster(t, vcfg("unity-main", "exclusive", "1Gi"))
	ctl := newControllerStub(t)
	a := lockReplica(cs, ctl.srv.URL, volPM{NewPodManager(cs, testVolNS, "img")}, []config.VolumeConfig{vcfg("unity-main", "exclusive", "1Gi")})
	setLock(t, cs, "agent1-unity-main", "someone-else", time.Now())

	require.NoError(t, a.unlockVolume(context.Background(), "unity-main", "run-a"))
	assert.Equal(t, "someone-else", pvcAnnotation(t, cs, "agent1-unity-main", annoVolumeHeldBy))
}

func TestVolumeLock_SharedVolumesAreNeverLocked(t *testing.T) {
	cs := lockCluster(t, vcfg("go-cache", "shared", "1Gi"))
	ctl := newControllerStub(t)
	a := lockReplica(cs, ctl.srv.URL, volPM{NewPodManager(cs, testVolNS, "img")}, []config.VolumeConfig{vcfg("go-cache", "shared", "1Gi")})
	res, err := a.createClaimPod(context.Background(), claimFor("run-a", mount("go-cache", "", ".cache")))
	require.NoError(t, err)
	assert.Empty(t, pvcAnnotation(t, cs, "agent1-go-cache", annoVolumeHeldBy))
	res.release(context.Background())
}

// TestExecuteRun_HoldsTheVolumeLockForTheRunAndReleasesItAfter: the lock is
// taken before the pod, is in place while a step runs, and is gone when the run
// ends.
func TestExecuteRun_HoldsTheVolumeLockForTheRunAndReleasesItAfter(t *testing.T) {
	f := newVolFixture(t, []config.VolumeConfig{vcfg("unity-main", "exclusive", "1Gi")})
	var during string
	f.a.exec = &podCapturingExec{inner: f.ex, onExec: func() {
		during = pvcAnnotation(t, f.cs, "agent1-unity-main", annoVolumeHeldBy)
	}}

	f.a.executeRun(context.Background(), f.claim([]dsl.VolumeMount{mount("unity-main", "", ".")}))
	require.Equal(t, api.RunSucceeded, f.waitFinish(t))

	assert.Equal(t, f.runID, during)
	assert.Empty(t, pvcAnnotation(t, f.cs, "agent1-unity-main", annoVolumeHeldBy), "the lock is released when the run ends")
	assert.Empty(t, pvcAnnotation(t, f.cs, "agent1-unity-main", annoVolumeHeldAt))
}

// onCreatePM runs hook once, just before the first CreatePod: the moment a
// stalled create lets the world move on.
type onCreatePM struct {
	podManager
	once sync.Once
	hook func()
}

func (p *onCreatePM) CreatePod(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	p.once.Do(p.hook)
	return p.podManager.CreatePod(ctx, pod)
}

// TestVolumeLock_LockStolenDuringACreateIsNotKeptByThePod is the lock-to-pod
// window. Nothing renews the lock while CreatePod is in flight, so a create that
// stalls (client-side throttling) or a replica whose clock runs ahead can see the
// lock judged stale and taken over by another replica. When the stalled create
// finally succeeds the run no longer holds the volume, and its pod must not stay:
// two pods would mount one exclusive volume.
func TestVolumeLock_LockStolenDuringACreateIsNotKeptByThePod(t *testing.T) {
	cs := lockCluster(t, vcfg("unity-main", "exclusive", "1Gi"))
	ctl := newControllerStub(t)
	vols := []config.VolumeConfig{vcfg("unity-main", "exclusive", "1Gi")}
	pm := &onCreatePM{podManager: volPM{NewPodManager(cs, testVolNS, "img")}}
	// Replica B judges A's lock stale and takes it over while A's create is in flight.
	pm.hook = func() { setLock(t, cs, "agent1-unity-main", "run-b", time.Now()) }
	a := lockReplica(cs, ctl.srv.URL, pm, vols)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	type result struct {
		res claimPodResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := a.createClaimPod(ctx, claimFor("run-a", mount("unity-main", "", ".")))
		done <- result{res, err}
	}()

	select {
	case r := <-done:
		t.Fatalf("run-a kept a pod for a volume run-b holds (err=%v)", r.err)
	case <-time.After(400 * time.Millisecond):
	}
	pods, err := cs.CoreV1().Pods(testVolNS).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, pods.Items, "the pod made under a lock that was lost is deleted")
	assert.Equal(t, "run-b", pvcAnnotation(t, cs, "agent1-unity-main", annoVolumeHeldBy), "the other run's lock is untouched")

	// run-b finishes and releases; run-a, which has been waiting, gets its turn.
	require.NoError(t, (&K8sAgent{cfg: a.cfg, kube: cs, pm: pm}).unlockVolume(context.Background(), "unity-main", "run-b"))
	select {
	case r := <-done:
		require.NoError(t, r.err)
		require.NotNil(t, r.res.pod)
		assert.Equal(t, "run-a", pvcAnnotation(t, cs, "agent1-unity-main", annoVolumeHeldBy))
	case <-time.After(10 * time.Second):
		t.Fatal("run-a never got the volume once run-b released it")
	}
}

type stallingCreatePM struct{ podManager }

func (stallingCreatePM) CreatePod(ctx context.Context, _ *corev1.Pod) (*corev1.Pod, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestVolumeLock_CreatePodIsBoundedWellUnderTheGrace: a create that never
// returns must be cut off long before the lock could be judged stale, and must
// give the lock back.
func TestVolumeLock_CreatePodIsBoundedWellUnderTheGrace(t *testing.T) {
	prev := volumeLockGrace
	volumeLockGrace = 400 * time.Millisecond
	t.Cleanup(func() { volumeLockGrace = prev })

	cs := lockCluster(t, vcfg("unity-main", "exclusive", "1Gi"))
	ctl := newControllerStub(t)
	a := lockReplica(cs, ctl.srv.URL, stallingCreatePM{volPM{NewPodManager(cs, testVolNS, "img")}},
		[]config.VolumeConfig{vcfg("unity-main", "exclusive", "1Gi")})

	start := time.Now()
	_, err := a.createClaimPod(context.Background(), claimFor("run-a", mount("unity-main", "", ".")))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create Pod")
	assert.Less(t, time.Since(start), volumeLockGrace, "the create is abandoned before the lock could be taken over")
	assert.Empty(t, pvcAnnotation(t, cs, "agent1-unity-main", annoVolumeHeldBy), "a timed-out create releases the lock")
}

type failCreatePM struct{ podManager }

func (failCreatePM) CreatePod(context.Context, *corev1.Pod) (*corev1.Pod, error) {
	return nil, errors.New("the API server said no")
}

func TestExecuteRun_ReleasesTheLockWhenThePodCannotBeCreated(t *testing.T) {
	f := newVolFixture(t, []config.VolumeConfig{vcfg("unity-main", "exclusive", "1Gi")})
	f.a.pm = failCreatePM{f.a.pm}

	f.a.executeRun(context.Background(), f.claim([]dsl.VolumeMount{mount("unity-main", "", ".")}))
	require.Equal(t, api.RunFailed, f.waitFinish(t))
	assert.Empty(t, pvcAnnotation(t, f.cs, "agent1-unity-main", annoVolumeHeldBy), "a lock must not outlive a pod that was never created")
}

// TestRun_VolumeWaitIsAbandonedOnDrain is the Kubernetes twin of the host
// agent's TestRunLoop_VolumeWaitIsAbandonedOnDrain. A run claimed while another
// pod holds its exclusive volume waits; when the agent starts draining, the wait
// ends and the run fails with the reason, without waiting for the holder. An
// agent that cannot be drained cannot be rolled.
func TestRun_VolumeWaitIsAbandonedOnDrain(t *testing.T) {
	const agentID = "k8s-1"
	cs := fake.NewSimpleClientset()
	installPVCCompareAndSwap(cs)
	old := agentlib.CancelPollInterval
	agentlib.CancelPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { agentlib.CancelPollInterval = old })

	// The holder: a live pod, as another replica's run pod would be.
	_, err := cs.CoreV1().Pods(testVolNS).Create(context.Background(), claimPod("holder", "agent1", corev1.PodRunning, "unity-main"), metav1.CreateOptions{})
	require.NoError(t, err)

	ctl := newControllerStub(t)
	var served atomic.Int32
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, r *http.Request) { drainRequestBody(r); w.WriteHeader(http.StatusOK) }
	mux.HandleFunc("POST /api/v1/agents/register", ok)
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/heartbeat", ok)
	mux.HandleFunc("DELETE /api/v1/agents/"+agentID, ok)
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/reconcile", func(w http.ResponseWriter, r *http.Request) {
		drainRequestBody(r)
		orchestrateWriteJSON(w, map[string]int{"failedRuns": 0})
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/claim", func(w http.ResponseWriter, r *http.Request) {
		drainRequestBody(r)
		if r.URL.Query().Get("kind") != "detached" && served.Add(1) == 1 {
			orchestrateWriteJSON(w, claimFor("run-2", mount("unity-main", "", ".")))
			return
		}
		time.Sleep(50 * time.Millisecond)
		orchestrateWriteJSON(w, api.ClaimResponse{})
	})
	// Everything else is the controller stub's.
	mux.Handle("/", ctl.srv.Config.Handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := Config{AgentID: agentID, AgentName: "agent1", Namespace: testVolNS, MaxConcurrent: 2,
		Volumes: []config.VolumeConfig{vcfg("unity-main", "exclusive", "1Gi")}}
	pm := NewPodManager(cs, testVolNS, "img")
	a := NewK8sAgent(cfg, agentlib.NewClient(srv.URL, "tok"), pm, nil, NewPodPool(cs, testVolNS, pm))
	// No DrainTimeout: the run context is not cancelled by the drain, so only the
	// claim context (the drain itself) can break the wait.

	claimCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(claimCtx) }()

	require.Eventually(t, func() bool { return strings.Contains(ctl.logText("run-2"), "waiting for volumes") },
		10*time.Second, 10*time.Millisecond, "run-2 was claimed and is blocked on the volume")
	assert.Empty(t, ctl.status("run-2"))

	cancel() // begin the drain; the holder is still running

	require.Eventually(t, func() bool { return ctl.status("run-2") == string(api.RunFailed) },
		10*time.Second, 10*time.Millisecond, "the abandoned run reports why it did not run")
	assert.Contains(t, ctl.logText("run-2"), "draining")
	_, err = cs.CoreV1().Pods(testVolNS).Get(context.Background(), "holder", metav1.GetOptions{})
	require.NoError(t, err, "the holder is untouched: the wait ended without waiting for it")
	pods, err := cs.CoreV1().Pods(testVolNS).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, pods.Items, 1, "an abandoned run must not create a pod")
	assert.Empty(t, pvcAnnotation(t, cs, "agent1-unity-main", annoVolumeHeldBy), "a waiting run holds no lock")

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the drain")
	}
}
