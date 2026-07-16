package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/require"
)

// TestAgent_MinFreeDisk_SkipsClaimWhileBelowThreshold drives the real
// runLoop (via Agent.Run) with an injected freeBytesFn. While reported free
// space is below MinFreeDisk, the loop must never call Claim. Once
// freeBytesFn reports free space at/above MinFreeDisk, claiming resumes.
func TestAgent_MinFreeDisk_SkipsClaimWhileBelowThreshold(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", registerHandler)
	var claimCount int32
	mux.HandleFunc("POST /api/v1/agents/a-diskfree/claim", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&claimCount, 1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.ClaimResponse{}) //nolint:errcheck
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	claimCtx, cancelClaim := context.WithCancel(context.Background())
	defer cancelClaim()

	const minFreeDisk = uint64(1) << 30 // 1GiB
	var (
		freeMu  sync.Mutex
		freeVal = uint64(100) << 20 // 100MiB, below minFreeDisk
	)
	a := &Agent{
		ID:            "a-diskfree",
		Client:        NewClient(srv.URL, "tok"),
		MaxConcurrent: 1,
		WorkspaceDir:  t.TempDir(),
		MinFreeDisk:   minFreeDisk,
		freeBytesFn: func(string) (uint64, error) {
			freeMu.Lock()
			defer freeMu.Unlock()
			return freeVal, nil
		},
	}

	done := make(chan error, 1)
	go func() { done <- a.Run(claimCtx) }()

	// While free space stays below the minimum, Claim must never be called.
	time.Sleep(300 * time.Millisecond)
	require.EqualValues(t, 0, atomic.LoadInt32(&claimCount), "Claim called while free disk space below MinFreeDisk")

	// Raise reported free space above the minimum; claiming should resume.
	freeMu.Lock()
	freeVal = uint64(2) << 30 // 2GiB
	freeMu.Unlock()

	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&claimCount) > 0
	}, 5*time.Second, 50*time.Millisecond, "Claim was not called after free disk space rose above MinFreeDisk")

	cancelClaim()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after cancel")
	}
}
