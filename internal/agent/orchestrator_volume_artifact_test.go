package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/artifact"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// An isolated run whose upload-artifact path lies inside a volume mount used to
// be failed (the host mountpoint is empty). It must now archive the volume's
// own contents, which is what the same job does on Kubernetes.
func TestExecuteRun_UploadsAnArtifactFromInsideAVolume(t *testing.T) {
	const agentID = "iso-vol-art-agent"
	h := newIsolatedHarness()
	inner := newIsolatedServer(t, agentID, h)

	var mu sync.Mutex
	var uploaded []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/artifacts/") {
			b, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			mu.Lock()
			uploaded = b
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	m, _ := newTestVolumes(t, exclusive("go-cache"))
	require.NoError(t, os.MkdirAll(filepath.Join(m.PathOf("go-cache"), "obj"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(m.PathOf("go-cache"), "obj", "out.txt"), []byte("built in the volume"), 0o600))

	f := &podFakeRT{}
	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok"), PauseImage: "pause:img", RunnerImage: "runner:img", Volumes: m}
	a.runtimeOnce.Do(func() {})
	a.resolvedRuntime = f

	a.executeRun(context.Background(), api.ClaimResponse{
		RunID: "run-iso-vol-art", JobName: "j", PodTemplate: mysqlTemplate(),
		Volumes: []dsl.VolumeMount{vmount("go-cache", "gc", ".ucd-cache/go")},
		Stages: []api.ClaimStage{{Step: &api.ClaimStep{
			Index: 0, Name: "up",
			UploadArtifact: &api.UploadArtifactStep{Name: "out", Path: ".ucd-cache/go/obj"},
		}}},
	}, t.TempDir())

	h.mu.Lock()
	require.Equal(t, "Succeeded", h.finishStatus, "the artifact path is reachable, so the run is green")
	h.mu.Unlock()

	mu.Lock()
	body := uploaded
	mu.Unlock()
	require.NotEmpty(t, body, "the upload endpoint must have been hit")
	dest := t.TempDir()
	require.NoError(t, artifact.ExtractTarZstd(strings.NewReader(string(body)), dest))
	got, err := os.ReadFile(filepath.Join(dest, "out.txt"))
	require.NoError(t, err, "the archive holds the volume's file, not an empty mountpoint")
	require.Equal(t, "built in the volume", string(got))
}
