package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/unified-cd/unified-cd/internal/api"
	"github.com/unified-cd/unified-cd/internal/controller"
	"github.com/unified-cd/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeSPAFixture writes a minimal SPA shell for WebDir serving tests.
// Since `make test` does not assume a web/dist build, it verifies only the
// static file serving behaviour (200/redirect/Content-Type) using its own fixture
// rather than depending on an actual built UI.
func writeSPAFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	html := `<!DOCTYPE html><html lang="ja"><head><title>unified-cd</title></head>` +
		`<body><div id="app"></div><script type="module" src="/ui/assets/index.js"></script></body></html>`
	require.NoError(t, os.WriteFile(dir+"/index.html", []byte(html), 0o644))
	return dir
}

func TestPhase7_UIServed(t *testing.T) {
	pg := store.NewTestPostgres(t)
	srv := controller.NewServer(controller.Config{Token: "t", AgentToken: "t", WebDir: writeSPAFixture(t)}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, "t"))
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()
	resp, err := http.Get(httpSrv.URL + "/ui/")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")
}

func TestPhase7_RootRedirectsToUI(t *testing.T) {
	pg := store.NewTestPostgres(t)
	srv := controller.NewServer(controller.Config{Token: "t", AgentToken: "t", WebDir: writeSPAFixture(t)}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, "t"))
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(httpSrv.URL + "/")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.True(t, resp.StatusCode >= 300 && resp.StatusCode < 400)
	assert.Contains(t, resp.Header.Get("Location"), "/ui/")
}

// TestPhase7_UIServesSPAShell verifies that /ui/ returns the SPA entry HTML (title and mount div).
// The frontend is built with Svelte, so the index.html itself does not contain the framework name
// (it appears only in the bundled JS); a string check for "vue" would therefore always have been wrong.
func TestPhase7_UIServesSPAShell(t *testing.T) {
	pg := store.NewTestPostgres(t)
	srv := controller.NewServer(controller.Config{Token: "t", AgentToken: "t", WebDir: writeSPAFixture(t)}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, "t"))
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()
	resp, err := http.Get(httpSrv.URL + "/ui/")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	bodyStr := strings.ToLower(string(body))
	assert.Contains(t, bodyStr, `id="app"`, "SPA should have a mount point")
	assert.Contains(t, string(body), "unified-cd", "SPA should have app title")
}

func TestPhase7_AgentsAPI(t *testing.T) {
	pg := store.NewTestPostgres(t)
	srv := controller.NewServer(controller.Config{Token: "t", AgentToken: "t"}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, "t"))
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()
	require.NoError(t, pg.UpsertAgent(t.Context(), "a1", "host1", "linux", "dev", []string{"kind:linux"}, nil))
	req, _ := http.NewRequest(http.MethodGet, httpSrv.URL+"/api/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer t")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var agents []api.AgentInfo
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&agents))
	assert.Len(t, agents, 1)
}

func TestPhase7_RunsByJobAPI(t *testing.T) {
	pg := store.NewTestPostgres(t)
	srv := controller.NewServer(controller.Config{Token: "t", AgentToken: "t"}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, "t"))
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()
	_, _ = pg.UpsertJob(t.Context(), "myjob", "unified-cd/v1", []byte(`{"steps":[{"name":"s","run":"echo x"}]}`))
	_, _ = pg.CreateRun(t.Context(), "myjob", nil, []byte(`{}`), nil, "api")
	_, _ = pg.CreateRun(t.Context(), "myjob", nil, []byte(`{}`), nil, "webhook:gh")
	req, _ := http.NewRequest(http.MethodGet, httpSrv.URL+"/api/v1/runs?jobName=myjob", nil)
	req.Header.Set("Authorization", "Bearer t")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var runs []api.Run
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&runs))
	assert.Len(t, runs, 2)
}

func TestPhase7_CancelRun(t *testing.T) {
	pg := store.NewTestPostgres(t)
	srv := controller.NewServer(controller.Config{Token: "t", AgentToken: "t"}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, "t"))
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, "api")
	_, _ = pg.TransitionPendingToQueued(t.Context(), 10)
	_, _ = pg.ClaimNextRun(t.Context(), "agent-1", nil)
	_ = pg.MarkRunRunning(t.Context(), run.ID)
	req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/runs/"+run.ID+"/cancel", nil)
	req.Header.Set("Authorization", "Bearer t")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	got, _ := pg.GetRun(t.Context(), run.ID)
	assert.Equal(t, api.RunCancelled, got.Status)
}

// TestPhase7_SSE_RejectsTokenQuery verifies that the SSE endpoint does not accept
// authentication via the ?token= query parameter. The frontend (RunDetail.svelte) uses
// fetch + Authorization header and does not use the browser's native EventSource
// (which cannot set custom headers and would require query-parameter auth).
// Putting a token in the URL risks exposure in server logs, Referer headers, and
// browser history, so this behaviour is intentionally left unimplemented.
// This test exists as a regression guard to prevent accidental future implementation.
func TestPhase7_SSE_RejectsTokenQuery(t *testing.T) {
	pg := store.NewTestPostgres(t)
	srv := controller.NewServer(controller.Config{Token: "t", AgentToken: "t"}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, "t"))
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, "api")
	_, _ = pg.TransitionPendingToQueued(t.Context(), 10)
	_, _ = pg.ClaimNextRun(t.Context(), "ag", nil)
	_ = pg.MarkRunRunning(t.Context(), run.ID)
	_ = pg.MarkRunFinished(t.Context(), run.ID, api.RunSucceeded)
	client := &http.Client{Timeout: 5 * time.Second}
	req, _ := http.NewRequest(http.MethodGet, httpSrv.URL+"/api/v1/runs/"+run.ID+"/events?token=t", nil)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}
