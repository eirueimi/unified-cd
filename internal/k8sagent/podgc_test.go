package k8sagent

import (
	"context"
	"errors"
	"net/http"
	"testing"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPodGCDecision(t *testing.T) {
	cases := []struct {
		status      api.RunStatus
		found       bool
		poolManaged bool
		wantDelete  bool
	}{
		{api.RunSucceeded, true, false, true},
		{api.RunFailed, true, false, true},
		{api.RunCancelled, true, false, true},
		{api.RunRunning, true, false, false},
		{api.RunRunning, false, false, true},
		// Regression: a pool-managed pod (poolManaged=true — carries the
		// pool-status annotation, whether idle or in-use) must never be
		// deleted by the orphan GC even when its creating run is terminal;
		// the pool's own idle-timeout/Restore logic owns its lifecycle. This
		// is the exact live-bug scenario: an idle pooled pod from an inline
		// (unnamed) podTemplate has its creating run go terminal, and must
		// survive the sweep so the next run can reuse it.
		{api.RunSucceeded, true, true, false}, // pool-managed, terminal run -> keep
	}
	for _, c := range cases {
		if got := podGCDecision(c.status, c.found, c.poolManaged); got != c.wantDelete {
			t.Fatalf("podGCDecision(%v,found=%v,poolManaged=%v)=%v want %v", c.status, c.found, c.poolManaged, got, c.wantDelete)
		}
	}
}

func TestPodGCOnce(t *testing.T) {
	pods := []gcPod{
		{podName: "ucd-run-terminal", runID: "run-terminal"},
		{podName: "ucd-run-active", runID: "run-active"},
		{podName: "ucd-run-pooled", runID: "run-pooled", poolManaged: true},
		// Regression: an idle pooled pod created from an unnamed/inline
		// podTemplate (empty pool-template annotation, but pool-status
		// annotation set to idle by pool.go's ReleasePod). Its creating run
		// is terminal. Before the fix this was indistinguishable from an
		// ordinary orphan and got deleted; it must survive the sweep.
		{podName: "ucd-run-pooled-idle-unnamed", runID: "run-pooled-idle-unnamed", poolManaged: true},
		{podName: "ucd-run-gone", runID: "run-gone"},
		{podName: "ucd-run-transient", runID: "run-transient"},
	}

	lister := func(ctx context.Context) ([]gcPod, error) {
		return pods, nil
	}

	runStatus := map[string]api.RunStatus{
		"run-terminal":            api.RunSucceeded,
		"run-active":              api.RunRunning,
		"run-pooled":              api.RunSucceeded, // terminal, but pooled in-use -> must be kept
		"run-pooled-idle-unnamed": api.RunSucceeded, // terminal, idle pooled (unnamed template) -> must be kept
	}

	getRun := func(ctx context.Context, runID string) (api.Run, error) {
		switch runID {
		case "run-gone":
			// definitive 404 -> orphan, delete
			return api.Run{}, &agentlib.HTTPError{StatusCode: http.StatusNotFound, Body: "not found"}
		case "run-transient":
			// transient/unknown error -> must be SKIPPED, not deleted
			return api.Run{}, errors.New("connection refused")
		}
		status, ok := runStatus[runID]
		if !ok {
			return api.Run{}, errors.New("unexpected runID")
		}
		return api.Run{ID: runID, Status: status}, nil
	}

	var deleted []string
	deletePod := func(ctx context.Context, podName string) error {
		deleted = append(deleted, podName)
		return nil
	}

	if err := runPodGCOnce(context.Background(), lister, getRun, deletePod); err != nil {
		t.Fatalf("runPodGCOnce error: %v", err)
	}

	want := map[string]bool{"ucd-run-terminal": true, "ucd-run-gone": true}
	if len(deleted) != len(want) {
		t.Fatalf("deleted = %v, want keys of %v", deleted, want)
	}
	for _, name := range deleted {
		if !want[name] {
			t.Fatalf("unexpected delete of pod %q", name)
		}
	}
}

// listRunPodsPM is a minimal podManager fake for TestListRunPods_PoolManaged
// that returns a fixed PodList regardless of the label selector passed in.
type listRunPodsPM struct {
	pods *corev1.PodList
}

func (f *listRunPodsPM) CreatePod(_ context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	return pod, nil
}
func (f *listRunPodsPM) WaitForPodRunning(_ context.Context, _ string) error { return nil }
func (f *listRunPodsPM) DeletePod(_ context.Context, _ string) error         { return nil }
func (f *listRunPodsPM) ListPods(_ context.Context, _ string) (*corev1.PodList, error) {
	return f.pods, nil
}

// TestListRunPods_PoolManaged is a regression test for the live bug: it
// exercises listRunPods against real Pod annotations rather than a
// pre-computed bool, so it would have caught the wrong-annotation read
// directly. A pooled pod created from an unnamed/inline podTemplate carries
// no pool-template annotation (podbuilder.go only sets it for named
// templates) but always carries the pool-status annotation (idle or
// in-use, set unconditionally by podbuilder.go/pool.go). listRunPods must
// derive poolManaged from pool-status, not pool-template, or such a pod is
// misclassified as an ordinary (non-pooled) run Pod.
func TestListRunPods_PoolManaged(t *testing.T) {
	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				// Idle pooled pod from an UNNAMED (inline) podTemplate: no
				// pool-template annotation, but pool-status is set. This is
				// the exact shape of the pod from the live bug report.
				ObjectMeta: metav1.ObjectMeta{
					Name:        "ucd-run-idle-unnamed",
					Labels:      map[string]string{"unified-cd/runId": "run-idle-unnamed"},
					Annotations: map[string]string{annoPoolStatus: poolStatusIdle},
				},
			},
			{
				// In-use pooled pod from a NAMED podTemplate: both
				// annotations set.
				ObjectMeta: metav1.ObjectMeta{
					Name:   "ucd-run-inuse-named",
					Labels: map[string]string{"unified-cd/runId": "run-inuse-named"},
					Annotations: map[string]string{
						annoPoolTemplate: "golang",
						annoPoolStatus:   poolStatusInUse,
					},
				},
			},
			{
				// Ordinary (non-pooled) run Pod: no pool annotations at all.
				ObjectMeta: metav1.ObjectMeta{
					Name:   "ucd-run-plain",
					Labels: map[string]string{"unified-cd/runId": "run-plain"},
				},
			},
		},
	}

	a := &K8sAgent{pm: &listRunPodsPM{pods: pods}}
	got, err := a.listRunPods(context.Background())
	if err != nil {
		t.Fatalf("listRunPods error: %v", err)
	}

	want := map[string]bool{
		"ucd-run-idle-unnamed": true,
		"ucd-run-inuse-named":  true,
		"ucd-run-plain":        false,
	}
	if len(got) != len(want) {
		t.Fatalf("listRunPods returned %d pods, want %d: %+v", len(got), len(want), got)
	}
	for _, p := range got {
		wantPoolManaged, ok := want[p.podName]
		if !ok {
			t.Fatalf("unexpected pod %q in listRunPods result", p.podName)
		}
		if p.poolManaged != wantPoolManaged {
			t.Errorf("listRunPods(%q).poolManaged = %v, want %v", p.podName, p.poolManaged, wantPoolManaged)
		}
	}
}
