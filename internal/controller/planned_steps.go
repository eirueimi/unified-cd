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
	add(buildStages(spec.Steps, &stepIdx, secrets, spec.Shell), "main")
	add(buildStages(spec.Finally, &stepIdx, secrets, spec.Shell), "finally")
	return out
}

// mergedRunSteps overlays reported step statuses onto the planned step list.
// For each planned step index: if the agent has reported it (possibly as
// multiple matrix variants), use the reported rows (with kind/section attached
// from the plan); otherwise emit the planned "Pending" entry. Reported rows
// whose index is not in the plan (shouldn't happen) are appended verbatim so
// real data is never dropped.
func mergedRunSteps(reported []api.StepReport, spec dsl.Spec) []api.StepReport {
	planned := plannedSteps(spec)
	byIndex := map[int][]api.StepReport{}
	for _, r := range reported {
		byIndex[r.Index] = append(byIndex[r.Index], r)
	}
	plannedIdx := map[int]bool{}
	var out []api.StepReport
	for _, p := range planned {
		plannedIdx[p.Index] = true
		if rs, ok := byIndex[p.Index]; ok {
			for _, r := range rs {
				r.Kind, r.Section, r.Matrix = p.Kind, p.Section, p.Matrix
				out = append(out, r)
			}
			continue
		}
		out = append(out, p)
	}
	for _, r := range reported {
		if !plannedIdx[r.Index] {
			out = append(out, r)
		}
	}
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
