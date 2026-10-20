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
//
// after, if set, runs once the first CreatePod has committed and before it
// returns to the caller: the moment between "the pod exists" and "the agent
// finds out".
type onCreatePM struct {
	podManager
	once, afterOnce sync.Once
	hook            func()
	after           func()
}

func (p *onCreatePM) CreatePod(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	if p.hook != nil {
		p.once.Do(p.hook)
	}
	made, err := p.podManager.CreatePod(ctx, pod)
	if p.after != nil {
		p.afterOnce.Do(p.after)
	}
	return made, err
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

// TestVolumeLock_ConfirmationIsACompareAndSwapThatInvalidatesARivalsPendingWrite
// is the clock-skew case a read cannot close. Replica B read the claim while A
// held the lock, judged A's run to have no pod (A's create had not committed) and
// is about to write the takeover with the resourceVersion it read. A bare Get by
// A after its create would leave that resourceVersion alone and B's write would
// still succeed; A's confirmation is an Update, which bumps it, so B's pending
// write is a Conflict.
func TestVolumeLock_ConfirmationIsACompareAndSwapThatInvalidatesARivalsPendingWrite(t *testing.T) {
	cs := lockCluster(t, vcfg("unity-main", "exclusive", "1Gi"))
	ctl := newControllerStub(t)
	pm := &onCreatePM{podManager: volPM{NewPodManager(cs, testVolNS, "img")}}
	var bsView *corev1.PersistentVolumeClaim
	// After A's pod commits and before A hears about it, B reads the claim and
	// is thereby holding the resourceVersion A's lock write produced.
	pm.after = func() {
		p, err := cs.CoreV1().PersistentVolumeClaims(testVolNS).Get(context.Background(), "agent1-unity-main", metav1.GetOptions{})
		require.NoError(t, err)
		bsView = p
	}
	a := lockReplica(cs, ctl.srv.URL, pm, []config.VolumeConfig{vcfg("unity-main", "exclusive", "1Gi")})

	res, err := a.createClaimPod(context.Background(), claimFor("run-a", mount("unity-main", "", ".")))
	require.NoError(t, err)
	require.NotNil(t, res.pod, "run-a confirmed its lock and keeps its pod")
	require.NotNil(t, bsView)

	// B's write, issued only now, against the resourceVersion it read earlier.
	bsView.Annotations[annoVolumeHeldBy] = "run-b"
	_, err = cs.CoreV1().PersistentVolumeClaims(testVolNS).Update(context.Background(), bsView, metav1.UpdateOptions{})
	require.Error(t, err, "B's takeover must not succeed after A confirmed")
	assert.True(t, apierrors.IsConflict(err), "got %v", err)
	assert.Equal(t, "run-a", pvcAnnotation(t, cs, "agent1-unity-main", annoVolumeHeldBy))
}

// If the rival's write lands between A's read and A's confirming write, A's
// Update conflicts, A reads again, sees the rival and gives its pod up.
func TestVolumeLock_ConfirmationConflictReReadsAndLosesToTheWinner(t *testing.T) {
	cs := lockCluster(t, vcfg("unity-main", "exclusive", "1Gi"))
	var updates atomic.Int32
	cs.PrependReactor("update", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		// Update 1 is A taking the lock; update 2 is A confirming it.
		if updates.Add(1) != 2 {
			return false, nil, nil
		}
		obj := a.(k8stesting.UpdateAction).GetObject().(*corev1.PersistentVolumeClaim)
		cur, err := cs.Tracker().Get(pvcResource, obj.Namespace, obj.Name)
		require.NoError(t, err)
		won := cur.(*corev1.PersistentVolumeClaim).DeepCopy()
		won.Annotations[annoVolumeHeldBy] = "run-b"
		n, _ := strconv.Atoi(won.ResourceVersion)
		won.ResourceVersion = strconv.Itoa(n + 1)
		require.NoError(t, cs.Tracker().Update(pvcResource, won, obj.Namespace))
		return true, nil, apierrors.NewConflict(pvcResource.GroupResource(), obj.Name, errors.New("modified"))
	})
	ctl := newControllerStub(t)
	a := lockReplica(cs, ctl.srv.URL, volPM{NewPodManager(cs, testVolNS, "img")}, []config.VolumeConfig{vcfg("unity-main", "exclusive", "1Gi")})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res, err := a.createClaimPod(ctx, claimFor("run-a", mount("unity-main", "", ".")))
	require.Error(t, err, "run-a keeps waiting for a volume run-b holds, until its context ends")
	assert.Nil(t, res.pod)
	pods, perr := cs.CoreV1().Pods(testVolNS).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, perr)
	assert.Empty(t, pods.Items, "the loser's pod is deleted")
	assert.Equal(t, "run-b", pvcAnnotation(t, cs, "agent1-unity-main", annoVolumeHeldBy))
}

type stallOnceCreatePM struct {
	podManager
	once sync.Once
	n    atomic.Int32
}

func (p *stallOnceCreatePM) CreatePod(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	p.n.Add(1)
	stall := false
	p.once.Do(func() { stall = true })
	if stall {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return p.podManager.CreatePod(ctx, pod)
}

// TestVolumeLock_ACreateThatTimesOutIsRetriedNotFatal: the create is cut off well
// under the grace, but a slow cluster must cost the run time and not the run.
func TestVolumeLock_ACreateThatTimesOutIsRetriedNotFatal(t *testing.T) {
	cs := lockCluster(t, vcfg("unity-main", "exclusive", "1Gi"))
	ctl := newControllerStub(t)
	pm := &stallOnceCreatePM{podManager: volPM{NewPodManager(cs, testVolNS, "img")}}
	a := lockReplica(cs, ctl.srv.URL, pm, []config.VolumeConfig{vcfg("unity-main", "exclusive", "1Gi")})
	a.lockGraceOverride = 400 * time.Millisecond // so the create deadline is 100ms

	start := time.Now()
	res, err := a.createClaimPod(context.Background(), claimFor("run-a", mount("unity-main", "", ".")))
	require.NoError(t, err, "a timed-out create is retried")
	require.NotNil(t, res.pod)
	assert.Equal(t, int32(2), pm.n.Load())
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Equal(t, "run-a", pvcAnnotation(t, cs, "agent1-unity-main", annoVolumeHeldBy), "the retry holds the lock again")

	// The run log says what happened, not that another run holds the volume.
	log := ctl.logText("run-a")
	assert.Contains(t, log, "creating the run's pod did not complete")
	assert.Contains(t, log, "timed out")
	assert.NotContains(t, log, "another run is using")
}

type alwaysAlreadyExistsPM struct {
	podManager
	n atomic.Int32
}

func (p *alwaysAlreadyExistsPM) CreatePod(_ context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	p.n.Add(1)
	return nil, apierrors.NewAlreadyExists(corev1.Resource("pods"), pod.Name)
}

// TestVolumeLock_APodCreateThatNeverLandsFailsTheRunWithTheTrueReason: a cause
// that does not clear (here a name taken by a pod this agent will not delete,
// because it is not this run's) must end the run after maxCreateAttempts with an
// honest message, not hold it under "waiting for volumes" until the job's own
// timeout, if it has one.
func TestVolumeLock_APodCreateThatNeverLandsFailsTheRunWithTheTrueReason(t *testing.T) {
	cs := lockCluster(t, vcfg("unity-main", "exclusive", "1Gi"))
	ctl := newControllerStub(t)
	pm := &alwaysAlreadyExistsPM{podManager: volPM{NewPodManager(cs, testVolNS, "img")}}
	a := lockReplica(cs, ctl.srv.URL, pm, []config.VolumeConfig{vcfg("unity-main", "exclusive", "1Gi")})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := a.createClaimPod(ctx, claimFor("run-a", mount("unity-main", "", ".")))
	require.Error(t, err)
	assert.Nil(t, res.pod)
	assert.Contains(t, err.Error(), "did not complete in 5 consecutive attempts")
	assert.Contains(t, err.Error(), "already exists", "the last cause is carried")
	assert.Equal(t, int32(maxCreateAttempts), pm.n.Load(), "exactly maxCreateAttempts creates were tried")
	assert.Empty(t, pvcAnnotation(t, cs, "agent1-unity-main", annoVolumeHeldBy), "a failed run holds no lock")

	log := ctl.logText("run-a")
	assert.Contains(t, log, "attempt 1 of 5")
	assert.Contains(t, log, "attempt 4 of 5")
	assert.Contains(t, log, "still exists from an earlier attempt")
	assert.NotContains(t, log, "another run is using", "the volume was never busy")
}

// scriptedCreatePM fails the creates whose 1-based number fail accepts, with
// AlreadyExists, and runs after(n) once the n-th has failed.
type scriptedCreatePM struct {
	podManager
	n     atomic.Int32
	fail  func(n int32) bool
	after func(n int32)
}

func (p *scriptedCreatePM) CreatePod(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	n := p.n.Add(1)
	if p.fail(n) {
		if p.after != nil {
			p.after(n)
		}
		return nil, apierrors.NewAlreadyExists(corev1.Resource("pods"), pod.Name)
	}
	return p.podManager.CreatePod(ctx, pod)
}

// TestVolumeLock_FailedCreatesAreCountedConsecutivelyNotForTheLifetimeOfTheRun: a
// run that waits hours for a busy volume and meets scattered transient create
// failures must not be failed with "N times in a row" for failures that were not
// in a row. Here four creates fail, a wait for a busy volume follows, and four
// more fail: eight in all, never five together.
func TestVolumeLock_FailedCreatesAreCountedConsecutivelyNotForTheLifetimeOfTheRun(t *testing.T) {
	cs := lockCluster(t, vcfg("unity-main", "exclusive", "1Gi"))
	ctl := newControllerStub(t)
	pm := &scriptedCreatePM{podManager: volPM{NewPodManager(cs, testVolNS, "img")}}
	pm.fail = func(n int32) bool { return n <= 8 }
	pm.after = func(n int32) {
		if n != 4 {
			return
		}
		// Another run takes the volume, and gives it up a moment later.
		holder := claimPod("holder", "agent1", corev1.PodRunning, "unity-main")
		_, err := cs.CoreV1().Pods(testVolNS).Create(context.Background(), holder, metav1.CreateOptions{})
		require.NoError(t, err)
		time.AfterFunc(200*time.Millisecond, func() {
			_ = cs.CoreV1().Pods(testVolNS).Delete(context.Background(), "holder", metav1.DeleteOptions{})
		})
	}
	a := lockReplica(cs, ctl.srv.URL, pm, []config.VolumeConfig{vcfg("unity-main", "exclusive", "1Gi")})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := a.createClaimPod(ctx, claimFor("run-a", mount("unity-main", "", ".")))
	require.NoError(t, err, "eight failures with a wait in the middle are not five in a row")
	require.NotNil(t, res.pod)
	assert.Equal(t, int32(9), pm.n.Load())
	assert.Contains(t, ctl.logText("run-a"), "waiting for volumes", "the wait in the middle really happened")
}

type alreadyExistsOncePM struct {
	podManager
	once sync.Once
}

func (p *alreadyExistsOncePM) CreatePod(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	fail := false
	p.once.Do(func() { fail = true })
	if fail {
		return nil, apierrors.NewAlreadyExists(corev1.Resource("pods"), pod.Name)
	}
	return p.podManager.CreatePod(ctx, pod)
}

// The pod name is derived from the run ID, so a retry after an earlier attempt
// whose pod landed finds its own pod there. That pod is removed and the create is
// tried again.
func TestVolumeLock_AlreadyExistsFromAnEarlierAttemptIsRetried(t *testing.T) {
	cs := lockCluster(t, vcfg("unity-main", "exclusive", "1Gi"))
	ctl := newControllerStub(t)
	leftover, err := BuildPod("run-a", testVolNS, nil, nil, "img", SidecarSpec{}, testShimImage, nil)
	require.NoError(t, err)
	_, err = cs.CoreV1().Pods(testVolNS).Create(context.Background(), leftover, metav1.CreateOptions{})
	require.NoError(t, err)

	a := lockReplica(cs, ctl.srv.URL, &alreadyExistsOncePM{podManager: volPM{NewPodManager(cs, testVolNS, "img")}},
		[]config.VolumeConfig{vcfg("unity-main", "exclusive", "1Gi")})
	res, err := a.createClaimPod(context.Background(), claimFor("run-a", mount("unity-main", "", ".")))
	require.NoError(t, err)
	require.NotNil(t, res.pod)
	pods, err := cs.CoreV1().Pods(testVolNS).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pods.Items, 1)
	assert.NotEmpty(t, pods.Items[0].Spec.Volumes, "the surviving pod is the new one, with the volume mounted")
}

// removeOwnPod matches on the run label as well as the name: the name comes from
// a truncated run ID and must never be the only evidence a pod is ours.
func TestRemoveOwnPod_NeverDeletesAnotherRunsPod(t *testing.T) {
	cs := lockCluster(t)
	ctl := newControllerStub(t)
	other := claimPod("ucd-run-shared-prefix", "agent1", corev1.PodRunning)
	other.Labels[labelRunID] = "some-other-run"
	_, err := cs.CoreV1().Pods(testVolNS).Create(context.Background(), other, metav1.CreateOptions{})
	require.NoError(t, err)
	a := lockReplica(cs, ctl.srv.URL, volPM{NewPodManager(cs, testVolNS, "img")}, nil)

	a.removeOwnPod(context.Background(), "run-a", "ucd-run-shared-prefix")
	_, err = cs.CoreV1().Pods(testVolNS).Get(context.Background(), "ucd-run-shared-prefix", metav1.GetOptions{})
	require.NoError(t, err, "a pod labelled for another run survives")

	a.removeOwnPod(context.Background(), "some-other-run", "ucd-run-shared-prefix")
	_, err = cs.CoreV1().Pods(testVolNS).Get(context.Background(), "ucd-run-shared-prefix", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "its own run's pod is removed")
}

func TestExecuteRun_RejectsTwoVolumesAtOneMountPath(t *testing.T) {
	f := newVolFixture(t, []config.VolumeConfig{vcfg("a-cache", "exclusive", "1Gi"), vcfg("b-cache", "exclusive", "1Gi")})
	f.a.executeRun(context.Background(), f.claim([]dsl.VolumeMount{
		mount("a-cache", "", ".cache"), mount("b-cache", "", "./.cache/"),
	}))
	assert.Equal(t, api.RunFailed, f.waitFinish(t))
	assert.Contains(t, f.logText(), "also used by spec.volumes[0]")
	assert.Empty(t, f.createdPods(t))
}

// Two mount paths that are different strings can still be one container path: a
// relative path lands under the workspace, and an absolute one may name that same
// directory. Only the agent knows where the workspace is, so only it can see this.
func TestUniqueMountPath_ComparesTheContainerPathNotTheSpelling(t *testing.T) {
	cases := []struct {
		name      string
		workspace string
		a, b      string
		dup       bool
	}{
		{"relative and the absolute path it resolves to", "/workspace", ".cache", "/workspace/.cache", true},
		{"in the other order", "/workspace", "/workspace/.cache", ".cache", true},
		{"under a workspace an agent template moved", "/src", ".cache", "/src/.cache", true},
		{"relative path that only looks like the absolute one", "/src", ".cache", "/workspace/.cache", false},
		{"different directories", "/workspace", ".cache", ".other", false},
		{"unnormalised spellings", "/workspace", "a/../.cache", "./.cache/", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seen := map[string]int{}
			_, err := uniqueMountPath(seen, 0, mount("x", "", tc.a), tc.workspace)
			require.NoError(t, err)
			_, err = uniqueMountPath(seen, 1, mount("y", "", tc.b), tc.workspace)
			if tc.dup {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "also used by spec.volumes[0]")
				assert.Contains(t, err.Error(), "in the container")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestVolumeLock_ConfirmLockWithoutAClient(t *testing.T) {
	a := &K8sAgent{cfg: Config{AgentName: "agent1", Namespace: testVolNS,
		Volumes: []config.VolumeConfig{vcfg("go-cache", "shared", "1Gi"), vcfg("unity-main", "exclusive", "1Gi")}}}

	lost, err := a.confirmLock(context.Background(), []string{"go-cache"}, "run-a")
	require.NoError(t, err, "an all-shared claim locked nothing, so there is nothing to confirm and no client is needed")
	assert.Empty(t, lost)

	_, err = a.confirmLock(context.Background(), []string{"unity-main"}, "run-a")
	require.ErrorIs(t, err, errNoKubeClient, "an exclusive volume with no client is an error, not a nil dereference")
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
