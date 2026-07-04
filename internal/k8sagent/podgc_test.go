package k8sagent

import (
	"context"
	"errors"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
)

func TestPodGCDecision(t *testing.T) {
	cases := []struct {
		status      api.RunStatus
		found       bool
		pooledInUse bool
		wantDelete  bool
	}{
		{api.RunSucceeded, true, false, true},
		{api.RunFailed, true, false, true},
		{api.RunCancelled, true, false, true},
		{api.RunRunning, true, false, false},
		{api.RunRunning, false, false, true},  // run gone -> orphan
		{api.RunSucceeded, true, true, false}, // pooled in-use -> keep
	}
	for _, c := range cases {
		if got := podGCDecision(c.status, c.found, c.pooledInUse); got != c.wantDelete {
			t.Fatalf("podGCDecision(%v,found=%v,pooled=%v)=%v want %v", c.status, c.found, c.pooledInUse, got, c.wantDelete)
		}
	}
}

func TestPodGCOnce(t *testing.T) {
	pods := []gcPod{
		{podName: "ucd-run-terminal", runID: "run-terminal"},
		{podName: "ucd-run-active", runID: "run-active"},
		{podName: "ucd-run-pooled", runID: "run-pooled", pooledInUse: true},
		{podName: "ucd-run-gone", runID: "run-gone"},
	}

	lister := func(ctx context.Context) ([]gcPod, error) {
		return pods, nil
	}

	runStatus := map[string]api.RunStatus{
		"run-terminal": api.RunSucceeded,
		"run-active":   api.RunRunning,
		"run-pooled":   api.RunSucceeded, // terminal, but pooled in-use -> must be kept
	}

	getRun := func(ctx context.Context, runID string) (api.Run, error) {
		status, ok := runStatus[runID]
		if !ok {
			return api.Run{}, errors.New("not found")
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
