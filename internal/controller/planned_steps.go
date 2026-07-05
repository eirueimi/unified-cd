package controller

import (
	"strings"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// plannedSteps derives the full list of steps a run's spec will execute, in
// order, each with Status "Pending". It reuses buildStages so stage indexing
// matches what the agent receives. Matrix/foreach steps are a single entry
// (they expand into variants only at runtime).
func plannedSteps(spec dsl.Spec) []api.StepReport {
	stepIdx := 0
	secrets := map[string]struct{}{} // unused here; buildStages requires it
	var out []api.StepReport
	add := func(stages []api.ClaimStage, section string) {
		for _, st := range stages {
			for _, cs := range api.StageSteps(st) {
				out = append(out, api.StepReport{
					Index:      cs.Index,
					StageIndex: cs.StageIndex,
					Name:       cs.Name,
					Kind:       stepKind(cs),
					Section:    section,
					Matrix:     cs.Matrix != nil,
					Status:     "Pending",
				})
			}
		}
	}
	add(buildStages(spec.Steps, &stepIdx, secrets), "main")
	add(buildStages(spec.Finally, &stepIdx, secrets), "finally")
	return out
}

// stepKind classifies a ClaimStep by its primary action for display.
func stepKind(cs api.ClaimStep) string {
	switch {
	case cs.Cache != nil:
		return "cache"
	case cs.Call != nil:
		if strings.HasPrefix(cs.Call.Job, "git://") {
			return "uses"
		}
		return "call"
	case cs.UploadArtifact != nil:
		return "uploadArtifact"
	case cs.DownloadArtifact != nil:
		return "downloadArtifact"
	case cs.Approval != nil:
		return "approval"
	default:
		return "run"
	}
}
