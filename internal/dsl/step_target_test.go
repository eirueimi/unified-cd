package dsl

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	sidecarMsg = `container "unified-artifact" is reserved for the agent's artifact sidecar`
	shimMsg    = `ucd-sh installer init container`
)

func TestIsForbiddenStepContainer(t *testing.T) {
	for _, n := range []string{"unified-artifact", "ucd-shim", " Unified-Artifact ", "UCD-SHIM"} {
		assert.True(t, IsForbiddenStepContainer(n), n)
	}
	for _, n := range []string{"job", "tools", ""} {
		assert.False(t, IsForbiddenStepContainer(n), n)
	}
}

// stepTargetJobs builds kind: Job documents that each place step container
// `ctr` in a different position.
func stepTargetJobs(ctr string) map[string]string {
	hdr := "apiVersion: unified-cd/v1\nkind: Job\nmetadata: { name: j }\nspec:\n"
	return map[string]string{
		"plain step":        hdr + "  steps:\n    - { name: upload, container: " + ctr + ", run: env }\n",
		"parallel step":     hdr + "  steps:\n    - parallel:\n        - { name: a, run: env }\n        - { name: b, container: " + ctr + ", run: env }\n",
		"finally step":      hdr + "  steps:\n    - { name: a, run: env }\n  finally:\n    - { name: f, container: " + ctr + ", run: env }\n",
		"uses entry":        hdr + "  steps:\n    - { name: t, container: " + ctr + ", uses: { job: \"git://github.com/org/repo/t.yaml@v1\" } }\n",
		"named podTemplate": hdr + "  podTemplate: { name: golang }\n  steps:\n    - { name: upload, container: " + ctr + ", run: env }\n",
		"native job":        hdr + "  native: true\n  steps:\n    - { name: upload, container: " + ctr + ", run: env }\n",
	}
}

func TestParse_RejectsForbiddenStepContainers(t *testing.T) {
	for ctr, want := range map[string]string{"unified-artifact": sidecarMsg, "ucd-shim": shimMsg} {
		for name, y := range stepTargetJobs(ctr) {
			t.Run(ctr+"/"+name, func(t *testing.T) {
				_, err := Parse(strings.NewReader(y))
				require.Error(t, err)
				if name != "native job" { // native: either error is acceptable, it just must fail
					assert.Contains(t, err.Error(), want)
				}
			})
		}
	}
}

// A plain job gets the spec section 5.8 error form with the step path.
func TestParse_ForbiddenStepContainerErrorForm(t *testing.T) {
	_, err := Parse(strings.NewReader(stepTargetJobs("unified-artifact")["plain step"]))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.steps[0] (upload): ")
	assert.Contains(t, err.Error(), sidecarMsg)

	_, err = Parse(strings.NewReader(stepTargetJobs("unified-artifact")["parallel step"]))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.steps[0].parallel[1] (b): ")
}

// Case and whitespace variants never get through: they are rejected either by
// the reserved-name check or by DNS-1123 / reference validation.
func TestParse_ForbiddenStepContainerVariantsRejected(t *testing.T) {
	for _, ctr := range []string{`"Unified-Artifact"`, `" unified-artifact "`, `"UCD-SHIM"`, `" ucd-shim "`} {
		for name, y := range stepTargetJobs(ctr) {
			t.Run(ctr+"/"+name, func(t *testing.T) {
				_, err := Parse(strings.NewReader(y))
				require.Error(t, err, "variant %s must not parse", ctr)
			})
		}
	}
	// The reserved-name check itself catches the plain-step variant (it does not
	// rely on the later DNS-1123 / reference validation).
	_, err := Parse(strings.NewReader(stepTargetJobs(`" Unified-Artifact "`)["plain step"]))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is reserved for the agent's artifact sidecar")
}

func TestParseJobTemplate_RejectsForbiddenStepContainer(t *testing.T) {
	for ctr, want := range map[string]string{"unified-artifact": sidecarMsg, "ucd-shim": shimMsg} {
		y := "apiVersion: unified-cd/v1\nkind: JobTemplate\nmetadata: { name: t }\nspec:\n  steps:\n    - { name: s, container: " + ctr + ", run: env }\n"
		_, err := ParseJobTemplate([]byte(y))
		require.Error(t, err, ctr)
		assert.Contains(t, err.Error(), want)
	}
}

func TestParse_JobContainerWithoutPodTemplateStillParses(t *testing.T) {
	y := "apiVersion: unified-cd/v1\nkind: Job\nmetadata: { name: j }\nspec:\n  steps:\n    - { name: s, container: job, run: env }\n"
	_, err := Parse(strings.NewReader(y))
	require.NoError(t, err)
}

func TestValidateStepContainerTargets(t *testing.T) {
	require.NoError(t, ValidateStepContainerTargets(Spec{Steps: []StepEntry{{Name: "a", Container: "job"}, {Name: "b"}}}))
	err := ValidateStepContainerTargets(Spec{Finally: []StepEntry{{Name: "f", Container: "ucd-shim"}}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.finally[0] (f)")
}
