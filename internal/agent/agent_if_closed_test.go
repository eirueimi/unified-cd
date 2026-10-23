package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An if: that cannot be evaluated at run time FAILS CLOSED: the gated step's
// work never executes, the step is reported Failed (not Skipped: a silent skip
// would hide a broken gate), the reason lands in the run's own log, and the
// normal implicit-success rules then skip later steps. The two ways to reach
// this path past apply-time validation are covered: an evaluation error
// (steps.X.outputs.Y where X never set Y) and an expression that does not
// compile (a uses: template's if: is only resolved at trigger time).

func ranMarker(t *testing.T, dir, name string) (path string, cmd string) {
	t.Helper()
	path = filepath.ToSlash(filepath.Join(dir, name))
	return path, "echo ran > " + shellQuote(path)
}

func markerExists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestExecuteRun_IfUnevaluable_FailsClosed(t *testing.T) {
	cases := map[string]string{
		"compile error":             `params.env ==`,
		"steps reference never set": `steps.build.outputs.result == "ok"`,
		"non-bool result":           `"yes"`,
	}
	for name, expr := range cases {
		t.Run(name, func(t *testing.T) {
			h, a := newIfVarsHarness(t, "run-if-closed")
			dir := t.TempDir()
			gated, gatedCmd := ranMarker(t, dir, "gated")
			later, laterCmd := ranMarker(t, dir, "later")

			claim := api.ClaimResponse{
				Native: true, RunID: "run-if-closed", JobName: "test",
				Stages: []api.ClaimStage{
					{Step: &api.ClaimStep{Name: "build", Index: 0, Run: "echo built"}},
					{Step: &api.ClaimStep{Name: "deploy", Index: 1, If: expr, Run: gatedCmd}},
					{Step: &api.ClaimStep{Name: "later", Index: 2, Run: laterCmd}},
				},
			}
			a.executeRun(context.Background(), claim, t.TempDir())

			assert.False(t, markerExists(gated), "the gated step's command must never execute")
			assert.Equal(t, []string{"Failed"}, h.reports(1), "reported Failed, not Skipped and never Running")
			assert.Equal(t, []string{"Skipped"}, h.reports(2), "implicit success skips later steps")
			assert.False(t, markerExists(later))
			log := h.systemLog()
			require.NotEmpty(t, log, "the failure must be explained in the run's own log")
			assert.Contains(t, log, `step "deploy"`)
			assert.Contains(t, log, "did NOT run")
			assert.NotContains(t, log, "RAN")
			assert.Equal(t, "Failed", <-h.finish, "the run must fail")
		})
	}
}

// continueOnError on the gated step is honoured exactly as for any other
// failure of that step: it is reported Failed (and its work still did not run)
// but the run is not failed and later steps proceed.
func TestExecuteRun_IfUnevaluable_ContinueOnError(t *testing.T) {
	h, a := newIfVarsHarness(t, "run-if-coe")
	dir := t.TempDir()
	gated, gatedCmd := ranMarker(t, dir, "gated")
	later, laterCmd := ranMarker(t, dir, "later")

	claim := api.ClaimResponse{
		Native: true, RunID: "run-if-coe", JobName: "test",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Name: "deploy", Index: 0, If: `steps.nope.outputs.x == "1"`, ContinueOnError: true, Run: gatedCmd}},
			{Step: &api.ClaimStep{Name: "later", Index: 1, Run: laterCmd}},
		},
	}
	a.executeRun(context.Background(), claim, t.TempDir())

	assert.False(t, markerExists(gated))
	assert.Equal(t, []string{"Failed"}, h.reports(0))
	assert.True(t, markerExists(later), "continueOnError lets later steps run")
	assert.Contains(t, h.reports(1), "Succeeded")
	assert.NotEmpty(t, h.systemLog())
	assert.Equal(t, "Succeeded", <-h.finish)
}

// A finally step whose if: cannot be evaluated does not run its work and is
// Failed (so the run fails), but the other finally steps still proceed.
func TestExecuteRun_IfUnevaluable_FinallyStep(t *testing.T) {
	h, a := newIfVarsHarness(t, "run-if-fin")
	dir := t.TempDir()
	bad, badCmd := ranMarker(t, dir, "bad")
	good, goodCmd := ranMarker(t, dir, "good")

	claim := api.ClaimResponse{
		Native: true, RunID: "run-if-fin", JobName: "test",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Name: "main", Index: 0, Run: "echo main"}},
		},
		Finally: []api.ClaimStage{
			{Step: &api.ClaimStep{Name: "bad-cleanup", Index: 1, If: `steps.main.outputs.nope == "1"`, Run: badCmd}},
			{Step: &api.ClaimStep{Name: "good-cleanup", Index: 2, Run: goodCmd}},
		},
	}
	a.executeRun(context.Background(), claim, t.TempDir())

	assert.False(t, markerExists(bad), "the finally step with a broken if: must not run its work")
	assert.Equal(t, []string{"Failed"}, h.reports(1))
	assert.True(t, markerExists(good), "other finally steps must still run")
	assert.Contains(t, h.reports(2), "Succeeded")
	assert.Contains(t, h.systemLog(), `step "bad-cleanup"`)
	assert.Equal(t, "Failed", <-h.finish)
}
