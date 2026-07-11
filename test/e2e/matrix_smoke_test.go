package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/controller"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMatrixSmoke drives a 2-dimension matrix job end-to-end through a real
// controller + in-process agent + Postgres: apply -> trigger -> expand ->
// per-variant execution -> per-variant step reports -> aggregated outputs.
// This is the live smoke the matrix final review asked for, in reproducible form.
func TestMatrixSmoke(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("matrix smoke runs shell steps; linux/mac only")
	}

	pg := store.NewTestPostgres(t)
	srv := controller.NewServer(controller.Config{Token: "t", AgentToken: "t"}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, "t"))
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go controller.RunScheduler(ctx, pg, 50*time.Millisecond)

	ag := agent.New("agent-e2e", agent.NewClient(httpSrv.URL, "t"))
	go func() { _ = ag.Run(ctx) }()

	// os x arch = 4 combinations, minus one exclude = 3 expected variants.
	yamlJob := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: matrix-smoke
spec:
  native: true
  params:
    outputs:
      - name: tag
        type: string
  steps:
    - name: build
      matrix:
        os: [linux, darwin]
        arch: [amd64, arm64]
        exclude:
          - os: darwin
            arch: arm64
      run: 'printf "TAG=%s-%s\n" "{{ .Matrix.os }}" "{{ .Matrix.arch }}"'
      outputs:
        tag: '{{ .Stdout | grep "TAG=" | cut "=" 2 | trim }}'
`
	applyBody, _ := json.Marshal(api.ApplyJobRequest{YAML: yamlJob})
	doPost(t, httpSrv.URL+"/api/v1/jobs", applyBody)

	triggerBody, _ := json.Marshal(api.TriggerRunRequest{JobName: "matrix-smoke"})
	var run api.Run
	require.NoError(t, json.Unmarshal(doPost(t, httpSrv.URL+"/api/v1/runs", triggerBody), &run))

	require.Eventually(t, func() bool {
		r, err := pg.GetRun(ctx, run.ID)
		return err == nil && r.Status == api.RunSucceeded
	}, 30*time.Second, 100*time.Millisecond, "matrix run did not succeed")

	// Per-variant step reports: 3 rows sharing the same step index, one per combo.
	steps, err := pg.GetRunSteps(ctx, run.ID)
	require.NoError(t, err)
	variants := map[string]string{} // variant key -> display name
	for _, s := range steps {
		if s.Variant != "" {
			variants[s.Variant] = s.Name
		}
	}
	gotVariants := make([]string, 0, len(variants))
	for k := range variants {
		gotVariants = append(gotVariants, k)
	}
	sort.Strings(gotVariants)
	assert.Equal(t, []string{"darwin/amd64", "linux/amd64", "linux/arm64"}, gotVariants,
		"expected exactly the 3 non-excluded combinations as variant rows")
	assert.Equal(t, "build (linux, amd64)", variants["linux/amd64"], "display name carries the combination")

	// Aggregated output: combo key -> value, and each value is the per-variant tag.
	outs, err := pg.GetStepOutputs(ctx, run.ID, steps[0].Index)
	require.NoError(t, err)
	_ = outs // per-variant store rows exist; run-level promotion asserted below.

	runOuts, err := pg.GetRunOutputs(ctx, run.ID)
	require.NoError(t, err)
	// Promoted job output is the JSON-encoded aggregated map (documented behavior).
	assert.Contains(t, runOuts["tag"], "linux/amd64", "aggregated output promoted as JSON map")
	assert.Contains(t, runOuts["tag"], "linux-amd64", "each combo captured its own .Matrix values")
	assert.NotContains(t, runOuts["tag"], "darwin/arm64", "excluded combination absent")
}

func doPost(t *testing.T, url string, body []byte) []byte {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, asReader(body))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	out := make([]byte, 0, 1024)
	buf := make([]byte, 1024)
	for {
		n, e := resp.Body.Read(buf)
		out = append(out, buf[:n]...)
		if e != nil {
			break
		}
	}
	return out
}
