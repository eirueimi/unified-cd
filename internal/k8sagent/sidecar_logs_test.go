package k8sagent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStreamPodContainerLogs_CopiesStream(t *testing.T) {
	client := k8sfake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "ns"},
	})
	var out bytes.Buffer
	err := streamPodContainerLogs(context.Background(), client, "ns", "p1", "mysql", metav1.Now(), &out)
	require.NoError(t, err)
	assert.NotEmpty(t, out.String()) // fake returns a canned body
}

// statusRecordingServer is a minimal httptest.Server that records every
// SidecarStatusRequest body POSTed to it, in arrival order.
type statusRecordingServer struct {
	srv *httptest.Server

	mu       sync.Mutex
	statuses []api.SidecarStatusRequest
}

func newStatusRecordingServer() *statusRecordingServer {
	rec := &statusRecordingServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sidecars") {
			var s api.SidecarStatusRequest
			if err := json.NewDecoder(r.Body).Decode(&s); err == nil {
				rec.mu.Lock()
				rec.statuses = append(rec.statuses, s)
				rec.mu.Unlock()
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	rec.srv = httptest.NewServer(mux)
	return rec
}

func (r *statusRecordingServer) statusReports() []api.SidecarStatusRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]api.SidecarStatusRequest, len(r.statuses))
	copy(out, r.statuses)
	return out
}

// TestK8sSidecarPump_ReportsRunningThenExited verifies the k8s pump reports
// "running" before streaming and "exited" (with the terminated container's
// exit code, read off the pod's ContainerStatuses) once the stream ends.
func TestK8sSidecarPump_ReportsRunningThenExited(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "ns"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:  "mysql",
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 9}},
				},
			},
		},
	}
	k8sClient := k8sfake.NewSimpleClientset(pod)

	rec := newStatusRecordingServer()
	defer rec.srv.Close()

	pump := &k8sSidecarPump{
		client:   k8sClient,
		logs:     agentlib.NewClient(rec.srv.URL, "tok"),
		ns:       "ns",
		pod:      "p1",
		agentID:  "agent-1",
		runID:    "run-1",
		sidecars: []string{"mysql"},
		since:    metav1.Now(),
	}
	pump.Start(context.Background())
	pump.Stop()

	statuses := rec.statusReports()
	require.Len(t, statuses, 2)

	assert.Equal(t, "run-1", statuses[0].RunID)
	assert.Equal(t, "mysql", statuses[0].Name)
	assert.Equal(t, dsl.SidecarLogIndex(0), statuses[0].Index)
	assert.Equal(t, "running", statuses[0].Phase)
	assert.Nil(t, statuses[0].ExitCode)

	assert.Equal(t, "exited", statuses[1].Phase)
	require.NotNil(t, statuses[1].ExitCode)
	assert.Equal(t, 9, *statuses[1].ExitCode)
}
